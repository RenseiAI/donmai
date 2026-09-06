package sessionshim

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// EventKind discriminates a ControllerEvent.
type EventKind string

// The controller event kinds.
const (
	// EventOutput carries shim-allocated sequence plus raw terminal bytes.
	EventOutput EventKind = "output"
	// EventGap declares an explicit, attributed loss of output. A consumer MUST
	// surface it rather than smoothing it over (§D5).
	EventGap EventKind = "gap"
	// EventSnapshot carries terminal state, typically right after a gap.
	EventSnapshot EventKind = "snapshot"
	// EventSnapshotFrame carries the exact interactive-attach frame bytes emitted
	// by a selected-v2 authoritative emit request. It is delivered only when the
	// shim reports in_stream=true.
	EventSnapshotFrame EventKind = "snapshot_frame"
	// EventHostFrame is the sole selected-v3 observation for one complete exact
	// sequence-bearing interactive-attach host frame.
	EventHostFrame EventKind = "host_frame"
	// EventExit is the immutable terminal observation.
	EventExit EventKind = "exit"
	// EventError is a closed code with display-only detail.
	EventError EventKind = "error"
)

// ControllerEvent is one thing that happened on an adopted session.
type ControllerEvent struct {
	Kind EventKind

	Seq     uint64
	RelTime uint64
	Data    []byte
	// RequestID is non-zero only for the selected-v3 live Snapshot emitted by
	// that connection-local request.
	RequestID uint64
	FrameType attachwire.EventType
	// FrameBytes is a complete encoded interactive-attach frame.
	FrameBytes []byte

	Gap      shimwire.GapMsg
	Snapshot shimwire.SnapshotMsg
	Exit     shimwire.ExitMsg
	Err      shimwire.ErrorMsg
}

// ControllerOptions configure a dial.
type ControllerOptions struct {
	// ControllerID identifies this controller process in diagnostics.
	ControllerID string
	// ProposedGeneration is the generation to propose. Zero means "derive it
	// from the shim's own authoritative current generation", which is the
	// ordinary path — see NextGeneration.
	ProposedGeneration shimwire.Generation
	// NextGeneration, when set, computes the proposal from the generation the
	// shim reports in Hello. It supersedes ProposedGeneration.
	//
	// The callback shape exists so the proposal is always derived from the
	// AUTHORITATIVE value rather than from a daemon's own bookkeeping: the shim
	// owns the generation, and a daemon that proposed from a stale local counter
	// would fence itself out (§D4).
	NextGeneration func(current shimwire.Generation) shimwire.Generation
	// PrepareAdoption runs after Hello has been authenticated and verified but
	// before Welcome proposes authority. It may durably reserve a carrier epoch
	// bound to the shim's authoritative current generation and return the exact
	// per-session generation/extensions to send. An error refuses adoption before
	// generation changes.
	PrepareAdoption func(evidence AdoptionPreparation) (PreparedAdoption, error)
	// ResumeFrom is the first sequence this controller still needs, i.e. its
	// durable last_forwarded_seq + 1. Zero means "from the start of the stream".
	ResumeFrom uint64
	// LocalResumeFrom is the normalized selected-v3 shim-side floor exposed to
	// PrepareAdoption. Zero normalizes to 1 for direct standalone Dial callers.
	LocalResumeFrom uint64
	// ResumeExternallyConfigured distinguishes an AdoptOptions.ResumeFrom
	// callback from the local sidecar floor carried in ResumeFrom.
	ResumeExternallyConfigured bool
	// DurableAckGeneration is the generation persisted beside LocalResumeFrom.
	// It must not be ahead of authenticated Hello.Generation.
	DurableAckGeneration shimwire.Generation
	// ExpectedWorkarea, when non-empty, is compared against the shim's
	// self-reported workarea. A mismatch refuses adoption.
	ExpectedWorkarea string
	// ExpectedWorkareaRoot cross-checks the optional discovery record root.
	ExpectedWorkareaRoot string
	// Extensions are optional negotiated extensions offered to the shim.
	Extensions shimwire.Extensions
	// ProtocolMin/ProtocolMax optionally narrow this controller's supported
	// range. Zero/zero preserves the released selected-v2 controller behavior.
	// A caller prepared to consume the complete raw selected-v3 event rail must
	// also set RequireFullHostFrames; that opts zero/zero into the build range.
	ProtocolMin uint32
	ProtocolMax uint32
	// RequireFullHostFrames declares that this controller consumes HostFrame as
	// the sole selected-v3 host-sequence authority. It enables max 3; it does not
	// reject a released max-2 shim, which must still be adopted conservatively.
	RequireFullHostFrames bool
	// EventBacklogBudget overrides EventBacklogBudget for this controller, in
	// payload bytes. Zero uses the default. A host running many sessions at once
	// may want a smaller per-session budget; it must never be set BELOW what the
	// shim's ring will hold, or the controller becomes the first thing to give
	// up again.
	EventBacklogBudget int
	// EventBacklogStallDeadline overrides eventBacklogStallDeadline for this
	// controller: how long the consumer may go without taking a whole budget's
	// worth of bytes before the controller fails closed. Zero uses the default.
	//
	// It is clamped up to eventBacklogStallFloor, which sits above
	// heartbeatReceiptWaitBound. A shorter deadline cannot be configured by any
	// route: a reader that fails closed while its consumer is still waiting on a
	// receipt only that reader can deliver severs the carrier through the very
	// knob meant to tune the back-pressure.
	EventBacklogStallDeadline time.Duration
	// DurableAckAmbiguityBound is the absolute time a stalled reader is held
	// open while a durable acknowledgement this controller sent is still
	// outstanding.
	//
	// A composing daemon MUST set this from its own resolved lineage-live
	// re-adoption window. ADR-2026-09-03 makes the two ONE configured value
	// ("Implementations MUST treat the two as one configured value, not two
	// values that happen to default identically today"), and the package
	// default satisfies that only for a composition that also runs the default
	// window: a deployment that configures a twenty-minute window and leaves
	// this zero gets a ten-minute bound, which is the silent drift the ADR's
	// Risks section names. Zero uses DurableAckAmbiguityBound.
	DurableAckAmbiguityBound time.Duration
	// EventBacklogDropBound is the absolute time a stalled reader is held open
	// after the stall deadline has been crossed, before the carrier is finally
	// dropped. Zero uses EventBacklogDropBound. It is clamped up to the
	// resolved stall deadline, below which it would delete the hold entirely.
	//
	// A composing daemon SHOULD set this from the same resolved lineage-live
	// re-adoption window it sets DurableAckAmbiguityBound from: both answer
	// "how long may a peer that is visibly still here fail to make progress",
	// and letting them drift apart is the silent misconfiguration
	// ADR-2026-09-03's Risks section names.
	EventBacklogDropBound time.Duration

	// DialTimeout bounds the connect + handshake. Zero uses 5s.
	DialTimeout time.Duration
	Logger      *slog.Logger
}

func (o ControllerOptions) protocolRange() (uint32, uint32, error) {
	if o.ProtocolMin == 0 && o.ProtocolMax == 0 {
		if o.RequireFullHostFrames {
			return shimwire.ProtocolMin, shimwire.ProtocolMax, nil
		}
		return shimwire.ProtocolMin, shimwire.V2, nil
	}
	if o.ProtocolMin == 0 || o.ProtocolMax < o.ProtocolMin || o.ProtocolMax > shimwire.ProtocolMax {
		return 0, 0, fmt.Errorf("sessionshim: invalid controller protocol range [%d,%d]", o.ProtocolMin, o.ProtocolMax)
	}
	if o.ProtocolMax >= shimwire.V3 && !o.RequireFullHostFrames {
		return 0, 0, errors.New("sessionshim: controller max 3 requires full HostFrame consumption")
	}
	if o.RequireFullHostFrames && o.ProtocolMax < shimwire.V3 {
		return 0, 0, errors.New("sessionshim: full HostFrame consumption requires controller max at least 3")
	}
	return o.ProtocolMin, o.ProtocolMax, nil
}

func (o ControllerOptions) dialTimeout() time.Duration {
	if o.DialTimeout > 0 {
		return o.DialTimeout
	}
	return 5 * time.Second
}

func (o ControllerOptions) eventBacklogBudget() int {
	if o.EventBacklogBudget > 0 {
		return o.EventBacklogBudget
	}
	return EventBacklogBudget
}

func (o ControllerOptions) eventBacklogStallDeadline() time.Duration {
	if o.EventBacklogStallDeadline > 0 {
		return o.EventBacklogStallDeadline
	}
	return eventBacklogStallDeadline
}

func (o ControllerOptions) durableAckAmbiguityBound() time.Duration {
	if o.DurableAckAmbiguityBound > 0 {
		return o.DurableAckAmbiguityBound
	}
	return DurableAckAmbiguityBound
}

func (o ControllerOptions) eventBacklogDropBound() time.Duration {
	if o.EventBacklogDropBound > 0 {
		return o.EventBacklogDropBound
	}
	return EventBacklogDropBound
}

// ResolvedEventBacklogDropBound reports the bound a controller built from these
// options would actually hold for, after the default and the floor. Exported
// for the same reason as ResolvedDurableAckAmbiguityBound: a composing daemon
// has to be able to assert its configured window REACHES the controller.
func (o ControllerOptions) ResolvedEventBacklogDropBound() time.Duration {
	return clampEventBacklogDropBound(o.eventBacklogDropBound(), clampEventBacklogStall(o.eventBacklogStallDeadline()))
}

// ResolvedDurableAckAmbiguityBound reports the bound a controller built from
// these options would actually hold for, after the default and the floor.
//
// It is exported so the composing daemon can assert that its configured
// re-adoption window REACHES the controller, rather than that the two defaults
// happen to agree — the drift ADR-2026-09-03's Risks section names.
func (o ControllerOptions) ResolvedDurableAckAmbiguityBound() time.Duration {
	return clampDurableAckAmbiguityBound(o.durableAckAmbiguityBound(), clampEventBacklogStall(o.eventBacklogStallDeadline()))
}

func (o ControllerOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Controller is the daemon side of an adopted session.
//
// It holds a socket and a generation — never an fd, an exec.Cmd, or a
// *ptyhost.Session. That is the §D1 ownership boundary made concrete: when this
// object is garbage, the session is unaffected.
type Controller struct {
	id                 Identity
	controllerID       string
	conn               *net.UnixConn
	w                  *shimwire.Writer
	r                  *shimwire.Reader
	gen                shimwire.Generation
	selected           uint32
	hello              shimwire.Hello
	workareaRoot       string
	helloAuthenticated bool
	adopted            shimwire.Adopted
	// resumeFrom is the exact durable cursor proposed in Welcome. Retaining it
	// lets a replacement daemon preserve last_forwarded_seq before any newly
	// replayed or live output advances its own bookkeeping.
	resumeFrom uint64
	events     chan ControllerEvent
	// backlog is selected-v3-only. It lets the socket reader reach a synchronous
	// Heartbeat persistence receipt even when the public event buffer is full,
	// while retaining an explicit fail-closed memory bound in payload bytes.
	backlog  *eventBacklog
	logger   *slog.Logger
	closeOne sync.Once
	done     chan struct{}
	// closing is closed by Close BEFORE the connection is dropped, so a read
	// loop parked on an event send has something to select on. Without it, a
	// caller that stops consuming events and then closes would leave the loop
	// blocked on a channel nobody will ever read — the connection error that
	// would otherwise unwind it is never observed, because the loop is not in
	// Read at that moment.
	closing chan struct{}
	exitMu  sync.RWMutex
	exit    *shimwire.ExitMsg

	// streamEnd is the fail-closed decision this controller made about its own
	// stream, or nil when the ending came from the peer or a caller. See
	// StreamEndCause.
	streamEndMu sync.Mutex
	streamEnd   error

	snapshotMu     sync.Mutex
	nextSnapshotID uint64
	snapshotCalls  map[uint64]*snapshotCall

	heartbeatCallMu sync.Mutex
	heartbeatMu     sync.Mutex
	heartbeatCall   *heartbeatCall
	// pendingReceipts records, oldest first, the heartbeats whose persistence
	// receipt has not arrived within the wait bound. The connection is NOT
	// dropped for one — see ErrHeartbeatReceiptPending — so a receipt that lands
	// late has to be recognised as the answer to a heartbeat this controller
	// really sent, rather than as the unsolicited frame that WOULD be a reason
	// to drop. Bounded by controllerPendingReceiptLimit: an unbounded ledger of
	// answers that never came is a leak, and the oldest one is the one least
	// likely to still be in flight.
	pendingReceipts []heartbeatCorrelation
}

// heartbeatCorrelation is the exact pair a persistence receipt must echo. The
// phase is deliberately absent: the request carries none and the receipt
// carries the shim's current one, so including it would never match.
type heartbeatCorrelation struct {
	generation shimwire.Generation
	ackedSeq   uint64
}

// controllerPendingReceiptLimit bounds the outstanding-receipt ledger.
const controllerPendingReceiptLimit = 8

type heartbeatCall struct {
	expected shimwire.HeartbeatMsg
	done     chan heartbeatResult
}

type heartbeatResult struct {
	err error
}

type snapshotCall struct {
	request         shimwire.SnapshotRequest
	result          *shimwire.SnapshotResult
	err             error
	sent            bool
	streamDelivered bool
	done            chan struct{}
}

type snapshotCompletion struct {
	call   *snapshotCall
	result shimwire.SnapshotResult
	err    error
}

const (
	controllerSnapshotRetryLedgerLimit = 1024
	publicEventBufferLimit             = 64
	// eventBacklogOverheadBytes is charged per queued event on top of its
	// payload, so a flood of empty frames is bounded by the same budget that
	// bounds a flood of large ones.
	eventBacklogOverheadBytes = 64
)

// EventBacklogBudget bounds, in payload bytes, how much undelivered stream the
// socket reader will hold for a consumer before it stops reading and applies
// back-pressure to the shim.
//
// It is expressed as a MULTIPLE of the shim's own output ring budget
// (ptyhost.DefaultRingBytes), and the direction of that relation is the point:
// the controller must never be TIGHTER than the ring. Both numbers answer the
// same question — how much host output may be in flight before this system
// admits it has lost some — and when the daemon-side bound was a frame count
// (192) it was orders of magnitude tighter than the shim's 8 MiB, so the
// controller collapsed long before the ring, which is the component actually
// DESIGNED to evict and declare a Gap (ADR-2026-08-17 §D5). A burst the shim
// absorbs by design must never be the thing that kills the connection carrying
// it.
//
// The multiple, rather than exact equality, is the measured lesson from a
// second incident. Seven seats were lost in one day on hosts whose durable
// consumer stalled for tens of seconds: at gate-output rates an ordinary build
// log fills 8 MiB in seconds, so the equality left no headroom at all between
// "briefly behind" and "at the bound". Raising the headroom does not change any
// decision this file makes — it changes how often a transient persistence stall
// has to reach for one. The memory ceiling it buys is per adopted session, and
// only while a consumer is behind; a host that wants a tighter one sets the
// override, which is bounded below by the shim ring it must not undercut.
//
// Reaching this budget is NOT a fail-closed decision. It used to be, and that
// cost healthy seats: a re-adopted lineage whose consumer was momentarily
// behind hit the bound on the resume Snapshot, the controller dropped the shim
// connection, nothing re-adopted the shim, and a live harness was quarantined
// and reaped. A single frame is never a verdict on a connection. Past the
// budget the reader now STALLS — which stalls the shim's output pump behind a
// socket nobody is draining, which is the same back-pressure every other layer
// of this stack already applies, and leaves the shim's ring to do the one job
// it exists for: evict and declare an explicit Gap (§D5).
//
// The reader still may not stall FOREVER: it is the only goroutine that can
// receive a durable heartbeat receipt, so a consumer that has genuinely stopped
// would park it behind a queue nobody will ever drain. That is what
// eventBacklogStallDeadline bounds, and only crossing THAT deadline still fails
// closed. "Stopped" there means measured in bytes taken, not in queue depth: a
// consumer keeping up at volume against a saturating producer never empties the
// queue and must never be mistaken for one that has stopped.
const EventBacklogBudget = eventBacklogBudgetRingMultiple * ptyhost.DefaultRingBytes

// eventBacklogBudgetRingMultiple is how many shim ring budgets of headroom the
// controller holds. One (exact equality) was the released value and left no
// room between a burst the ring absorbs by design and the bound; four is the
// smallest multiple that covers a full gate-output burst at the measured rates
// without changing any decision in this file.
const eventBacklogBudgetRingMultiple = 4

// eventBacklogStallDeadline bounds how long the consumer may fail to make
// PROGRESS — cumulatively, across every push — before the controller fails
// closed.
//
// It is not a per-call idle timer. A per-call timer measures one caller's
// patience, which a dribbling consumer resets for free: hand back one event
// every few seconds and every push returns before its own clock runs out, while
// the reader stays parked in push essentially forever, heartbeat receipts only
// trickle through, and the daemon's cursor acknowledger loops on
// ErrHeartbeatReceiptPending with nothing ever resolving it. So the clock is
// anchored on the BACKLOG, at the moment the consumer first falls behind.
//
// What resets it is defined in bytes, not in queue depth — see
// eventBacklogProgressBytes. Measuring emptiness instead would refuse a consumer
// that is keeping up perfectly well at volume: a saturating producer means the
// queue never once reaches zero, so a carrier turning over megabytes a second
// under a heavy build log would lose its carrier at the deadline. That is the
// exact failure class this whole mechanism exists to remove, and it would have
// been reintroduced by the definition of progress rather than by the drop.
//
// The deadline is deliberately LONGER than heartbeatReceiptWaitBound, and that
// ordering is load-bearing — see eventBacklogStallFloor, which enforces it
// against any override.
//
// This is a THROUGHPUT bound, not a latency one: the consumer must sustain at
// least budget/deadline to keep resetting the clock, about 273 KiB/s at the
// 32 MiB EventBacklogBudget / 120s defaults — the same required drain rate the
// released 8 MiB / 30s pair asked for, over four times the window. Shortening
// the deadline RAISES the required drain rate; it does not make the mechanism
// more lenient. Because the clock is anchored when the consumer first falls
// behind rather than re-armed per push, worst-case wall time from "first fell
// behind" to DEGRADED is about 2x this deadline (a consumer can fall behind,
// coast just above the required rate for nearly a full deadline without
// resetting the anchor, then stop).
//
// Crossing it is no longer the fail-closed decision. It is where the stall
// becomes VISIBLE — reported with bytes in flight and stall duration — while
// the reader keeps stalling; only EventBacklogDropBound still fails closed.
const eventBacklogStallDeadline = 120 * time.Second

// EventBacklogDropBound is how long a stalled reader is held open BEFORE it
// finally drops the carrier, once the stall deadline has already been crossed.
//
// # THE KILL SWITCH THIS REPLACES
//
// Measured on production hosts, seven interactive seats in one day: the durable
// consumer — a control plane persisting every host frame — stalled for tens of
// seconds while its database path was slow. Every one of the seats was
// OUTPUT-HEAVY (a build gate, a race-detector test run) so the in-flight budget
// filled in seconds, the stall deadline elapsed with no progress, and the
// controller dropped the shim connection over it. The connection was fine. The
// harness was fine. The persistence was slow, and the seat died anyway.
//
// "The consumer made no progress for 30s" was being read as "the consumer is
// gone". Over a durable path with a shared datastore behind it that inference
// is simply wrong: minutes of no progress is a slow write, not a dead peer, and
// ADR-2026-08-30 D2 puts an acknowledgement without its durable post-condition
// on the closed list of ALWAYS-AMBIGUOUS evidence — "preserve; recheck and
// retry; then degrade visibly", never a terminal verdict off the ambiguity
// alone.
//
// So the stall deadline now degrades visibly and keeps stalling, and this is
// the bound that finally fails closed. It is not infinite, because a controller
// process that has genuinely gone away without closing its socket would
// otherwise hold a shim's output pump forever, and the shim would never arm the
// orphan clock that is its own protection. Crossing it is still NOT loss: the
// carrier drops with the harness RETAINED (carrierLost=false), the shim keeps
// the process group, and re-adoption restores the stream from the shim's own
// sequence. That is the same disposition a daemon restart produces, and it is
// survivable; the thing that was not survivable was reaching it in 30 seconds.
//
// It takes the same ten minutes as DurableAckAmbiguityBound and for the same
// reason: it is the longest bound the corpus names for a peer that is visibly
// still here — the lineage-live re-adoption window of the 2026-09-03 amendment.
// A composition that configures a different window MUST configure this from it
// too, exactly as it must for the ambiguity bound.
const EventBacklogDropBound = 10 * time.Minute

// clampEventBacklogDropBound applies the resolved stall deadline as this
// bound's floor. Zero means "use the default".
//
// A drop bound BELOW the stall deadline does not tune the hold, it deletes it:
// the first crossing of the deadline would already be past the bound, and the
// reader would fail closed at exactly the moment this bound exists to carry it
// through — the released 30-second kill switch, reintroduced through the knob
// added to remove it.
func clampEventBacklogDropBound(bound, stall time.Duration) time.Duration {
	if bound <= 0 {
		bound = EventBacklogDropBound
	}
	return max(bound, stall)
}

// DurableAckAmbiguityBound is how long a stalled reader is held open while a
// durable acknowledgement THIS controller sent is still outstanding.
//
// # THE STRAND THIS UNDOES
//
// Measured on a consumer daemon: a healthy interactive session whose control
// plane was answering every request 200 but answering the DURABLE receipt
// slowly. The reader parked in push behind a consumer waiting on that durable
// side; parking the reader is what makes the receipt undeliverable, so the
// cursor acknowledger logged "the durable acknowledgement is still pending;
// keeping the shim connection and retrying" every 5s — for exactly one
// eventBacklogStallDeadline. At 30s the reader failed closed, the daemon read
// the ending as the shim's own, quarantined the lineage `socket_unreachable`
// with no re-adoption, and the control plane terminalized it 95s later. The
// socket was reachable throughout.
//
// The stall deadline exists to catch a consumer that has genuinely STOPPED. An
// outstanding durable acknowledgement is positive evidence of the opposite: the
// consumer is right there, blocked on the durable side, and ADR-2026-08-30 D2
// puts "a transport-level acknowledgement without the durable post-condition"
// on the closed list of evidence that is always AMBIGUOUS — "preserve; recheck
// and retry; then degrade visibly", never a terminal verdict. So while that
// acknowledgement is outstanding the stall clock is held rather than allowed to
// fail closed, and the memory bound is unaffected: the queue stays at one
// budget, which is the number the budget already promised.
//
// It is not unbounded, because "held forever" is its own way to strand a
// lineage. The corpus names no duration for this case (its two bounded-retry
// mechanisms — the shim's orphan deadline and the re-adoption window — are both
// keyed to the controller STREAM being lost, which is the opposite of this
// fact pattern), so this takes the longest bound the corpus does name for a
// daemon that is visibly still here: the ten-minute lineage-live re-adoption
// window of the 2026-09-03 amendment. Crossing it is still not loss — it
// degrades visibly, through ErrDurableAckAmbiguityBound and a quarantine reason
// that says what actually happened.
//
// It is EXPORTED so the composing daemon can pin it against the window it was
// derived from. A default that silently drifted away from that window would
// leave the derivation in this comment true only historically, and nothing in
// either package would notice.
const DurableAckAmbiguityBound = 10 * time.Minute

// durableAckAmbiguityFloor is the shortest ambiguity bound this package will
// honour, however short an embedder asks for — the exact counterpart of
// eventBacklogStallFloor, for the exact same reason.
//
// The bound exists to outlive the stall deadline it holds open. A bound BELOW
// that deadline does not merely tune the hold, it deletes it: the reader fails
// closed at whichever of the two comes first, which is the ambiguity bound, and
// a live shim is quarantined over a slow durable side again — reintroducing the
// measured incident through the very knob added to prevent it. So the resolved
// bound is raised to the larger of this floor and the controller's own resolved
// stall deadline, and an override below either is not honoured.
const durableAckAmbiguityFloor = eventBacklogStallDeadline

// clampDurableAckAmbiguityBound applies durableAckAmbiguityFloor and the
// resolved stall deadline to a configured bound. Zero means "use the default",
// which already satisfies both.
func clampDurableAckAmbiguityBound(bound, stall time.Duration) time.Duration {
	if bound <= 0 {
		bound = DurableAckAmbiguityBound
	}
	return max(bound, durableAckAmbiguityFloor, stall)
}

// eventBacklogStallFloor is the shortest stall deadline this package will honour,
// however short an embedder asks for.
//
// The one way a stalled reader can deadlock its own consumer is a consumer that
// drains events and calls Heartbeat on the same goroutine: it waits for a receipt
// only the reader can deliver while the reader waits for it to drain.
// heartbeatReceiptWaitBound breaks that inversion on its own — Heartbeat gives up
// after 5s with ErrHeartbeatReceiptPending, KEEPS the connection, and the consumer
// returns to draining. A deadline SHORTER than that bound never gives the
// inversion a chance to resolve: the reader fails closed while the consumer is
// still waiting on the receipt, and the carrier is severed by the very knob that
// was supposed to tune the back-pressure. So an override below this floor is
// raised to it rather than honoured. The slack above the bound is headroom for
// the consumer to actually resume draining once Heartbeat returns.
const eventBacklogStallFloor = heartbeatReceiptWaitBound + 2*time.Second

// clampEventBacklogStall applies eventBacklogStallFloor to a configured deadline.
// Zero means "use the default", which already satisfies the floor.
func clampEventBacklogStall(stall time.Duration) time.Duration {
	if stall <= 0 {
		return eventBacklogStallDeadline
	}
	return max(stall, eventBacklogStallFloor)
}

// ErrAdoptionRefused reports a handshake the shim or this daemon declined.
var ErrAdoptionRefused = errors.New("sessionshim: adoption refused")

type authenticatedHelloError struct {
	generation shimwire.Generation
	err        error
}

func (e *authenticatedHelloError) Error() string { return e.err.Error() }
func (e *authenticatedHelloError) Unwrap() error { return e.err }

func authenticatedHelloGeneration(err error) (shimwire.Generation, bool) {
	var evidence *authenticatedHelloError
	if !errors.As(err, &evidence) {
		return 0, false
	}
	return evidence.generation, true
}

// Dial connects to a shim described by rec and completes Hello → Welcome →
// Adopted.
//
// Verification happens BEFORE any authority is proposed, in this order: the
// socket must be the exact (device, inode) the record binds to; the peer's
// self-reported lifecycle identity, process identity, and workarea must match
// the record. Only then is a generation proposed. Proposing first and checking
// afterwards would mean a mismatched shim had already been handed control.
func Dial(ctx context.Context, rec Record, opts ControllerOptions) (*Controller, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if !peerCredSupported() {
		return nil, ErrShimUnsupported
	}
	if err := verifySocketIdentity(rec); err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, opts.dialTimeout())
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(dialCtx, "unix", rec.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: dial adoption socket: %w", err)
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return nil, errors.New("sessionshim: adoption socket is not a unix connection")
	}
	// The peer is authenticated in BOTH directions: the shim checks us, and we
	// check it. A one-sided check would let anything that can create a socket in
	// a readable directory impersonate a shim.
	uid, err := peerUID(conn)
	if err != nil || uid != os.Getuid() {
		_ = conn.Close()
		if err != nil {
			return nil, fmt.Errorf("sessionshim: verify shim peer: %w", err)
		}
		return nil, fmt.Errorf("%w: shim socket peer uid %d is not %d", ErrAdoptionRefused, uid, os.Getuid())
	}

	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	c := &Controller{
		id:            rec.Identity(),
		controllerID:  opts.ControllerID,
		conn:          conn,
		w:             shimwire.NewWriter(conn),
		r:             shimwire.NewReader(conn),
		resumeFrom:    opts.ResumeFrom,
		workareaRoot:  rec.WorkareaRoot,
		events:        make(chan ControllerEvent, publicEventBufferLimit),
		logger:        opts.logger(),
		done:          make(chan struct{}),
		closing:       make(chan struct{}),
		snapshotCalls: make(map[uint64]*snapshotCall),
	}
	if err := c.handshake(rec, opts); err != nil {
		_ = conn.Close()
		if c.helloAuthenticated {
			return nil, &authenticatedHelloError{generation: c.hello.Generation, err: err}
		}
		return nil, err
	}
	// Clear the handshake deadline: a live session is idle for long stretches by
	// design (a human is thinking), and inheriting a dial deadline would tear
	// down exactly the sessions this whole mechanism exists to preserve.
	_ = conn.SetDeadline(time.Time{})

	if c.selected >= shimwire.V3 {
		c.backlog = newEventBacklog(
			opts.eventBacklogBudget(), opts.eventBacklogStallDeadline(), c.closing,
			c.durableAckOutstanding, opts.durableAckAmbiguityBound(), opts.eventBacklogDropBound(),
		)
		c.backlog.report = c.reportBacklogFlow
		go c.dispatchEvents()
	}
	go c.readLoop()
	return c, nil
}

func (c *Controller) handshake(rec Record, opts ControllerOptions) error {
	msg, err := c.r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read hello: %w", err)
	}
	if msg.Type != shimwire.TypeError && msg.Type != shimwire.TypeHello {
		return fmt.Errorf("%w: expected Hello, got %s", ErrAdoptionRefused, msg.Type)
	}
	if msg.Type == shimwire.TypeError {
		e, decErr := shimwire.DecodeError(msg.Body)
		if decErr != nil {
			return decErr
		}
		return fmt.Errorf("%w: shim refused at hello: %s: %s", ErrAdoptionRefused, e.Code, e.Detail)
	}
	hello, err := shimwire.DecodeHello(msg.Body)
	if err != nil {
		return err
	}
	if err := verifyHello(hello, rec, opts.ExpectedWorkarea, opts.ExpectedWorkareaRoot); err != nil {
		return err
	}
	c.hello, c.helloAuthenticated = hello, true
	if err := hello.Extensions.CheckRequired(); err != nil {
		return err
	}
	localMin, localMax, err := opts.protocolRange()
	if err != nil {
		return err
	}
	selected, err := shimwire.Negotiate(hello.Min, hello.Max, localMin, localMax)
	if err != nil {
		return err
	}
	localResumeFrom := opts.LocalResumeFrom
	if localResumeFrom == 0 {
		localResumeFrom = 1
	}
	if selected >= shimwire.V3 {
		if opts.DurableAckGeneration > hello.Generation {
			return fmt.Errorf("%w: durable acknowledgement generation %d is ahead of Hello generation %d",
				ErrAdoptionPreparation, opts.DurableAckGeneration, hello.Generation)
		}
		if hello.LastSeq != ^uint64(0) && localResumeFrom > hello.LastSeq+1 {
			return fmt.Errorf("%w: local resume %d is ahead of Hello LastSeq %d",
				ErrAdoptionPreparation, localResumeFrom, hello.LastSeq)
		}
	}

	proposed := opts.ProposedGeneration
	if opts.NextGeneration != nil {
		proposed = opts.NextGeneration(hello.Generation)
	}
	extensions := opts.Extensions
	preparedResumeProvided := false
	if opts.PrepareAdoption != nil {
		lastForwarded := localResumeFrom - 1
		prepared, prepareErr := opts.PrepareAdoption(AdoptionPreparation{
			Identity:                    c.id,
			ControllerID:                opts.ControllerID,
			ShimID:                      hello.ShimID,
			ProcessEpoch:                hello.ProcessEpoch,
			CurrentControllerGeneration: hello.Generation,
			LocalResumeFrom:             localResumeFrom,
			LastHostSeq:                 hello.LastSeq,
			LastForwardedSeq:            lastForwarded,
			SelectedVersion:             selected,
		})
		if prepareErr != nil {
			return prepareErr
		}
		resolved, resolveErr := ResolvePreparedAdoption(prepared, PreparedAdoptionBounds{
			StaticGenerationConfigured: opts.ProposedGeneration != 0 || opts.NextGeneration != nil,
			StaticResumeConfigured: opts.ResumeExternallyConfigured ||
				(opts.LocalResumeFrom == 0 && opts.ResumeFrom != 0),
			LocalResumeFrom: localResumeFrom,
			HelloLastSeq:    hello.LastSeq,
		})
		if resolveErr != nil {
			return resolveErr
		}
		if resolved.ControllerGeneration != 0 {
			proposed = resolved.ControllerGeneration
		}
		extensions = resolved.Extensions
		if resolved.ResumeProvided {
			preparedResumeProvided = true
			opts.ResumeFrom = resolved.ResumeFrom
		}
	}
	if _, proofBoundCarrier := extensions.Values[shimwire.ExtCarrierEpoch]; selected >= shimwire.V3 && proofBoundCarrier && !preparedResumeProvided {
		return fmt.Errorf("%w: selected-v3 carrier preparation omitted proof-resolved ResumeFrom", ErrAdoptionPreparation)
	}
	if proposed == 0 {
		proposed = hello.Generation + 1
	}
	if proposed <= hello.Generation {
		return fmt.Errorf("%w: proposed generation %d does not advance the shim's %d",
			shimwire.ErrStaleGeneration, proposed, hello.Generation)
	}

	welcome := shimwire.Welcome{
		Protocol:           shimwire.ProtocolName,
		Selected:           selected,
		ControllerID:       opts.ControllerID,
		ProposedGeneration: proposed,
		ResumeFrom:         opts.ResumeFrom,
		Extensions:         extensions,
	}
	c.resumeFrom = opts.ResumeFrom
	if err := writeTyped(c.w, shimwire.TypeWelcome, func() ([]byte, error) { return shimwire.EncodeWelcome(welcome) }); err != nil {
		return err
	}

	reply, err := c.r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read adopted: %w", err)
	}
	if reply.Type == shimwire.TypeError {
		e, decErr := shimwire.DecodeError(reply.Body)
		if decErr != nil {
			return decErr
		}
		return fmt.Errorf("%w: %s: %s", ErrAdoptionRefused, e.Code, e.Detail)
	}
	if reply.Type != shimwire.TypeAdopted {
		return fmt.Errorf("%w: expected Adopted, got %s", ErrAdoptionRefused, reply.Type)
	}
	adopted, err := shimwire.DecodeAdopted(reply.Body)
	if err != nil {
		return err
	}
	// Trust the COMMITTED generation, not the proposed one. They agree today, but
	// the shim is authoritative and a controller that assumed its own proposal
	// would fence itself out the moment they ever diverged.
	if err := validateAdoptionCommit(adopted, proposed, extensions); err != nil {
		return err
	}
	c.adopted, c.gen, c.selected = adopted, adopted.Generation, selected
	return nil
}

func validateAdoptionCommit(adopted shimwire.Adopted, proposed shimwire.Generation, extensions shimwire.Extensions) error {
	if adopted.Generation != proposed {
		return fmt.Errorf("%w: shim committed generation %d, expected exactly %d",
			ErrAdoptionRefused, adopted.Generation, proposed)
	}
	if !extensions.ExactEqual(adopted.Extensions) {
		return fmt.Errorf("%w: shim extension acknowledgement differs from Welcome", ErrAdoptionRefused)
	}
	return nil
}

// verifyHello checks that the live peer is the shim the record described.
func verifyHello(h shimwire.Hello, rec Record, expectedWorkarea, expectedWorkareaRoot string) error {
	if h.Protocol != shimwire.ProtocolName {
		return fmt.Errorf("%w: hello names protocol %q", shimwire.ErrVersionMismatch, h.Protocol)
	}
	if h.OrgID != rec.OrgID || h.SessionID != rec.SessionID {
		return fmt.Errorf("%w: shim reports identity %s/%s, record says %s/%s",
			ErrAdoptionRefused, h.OrgID, h.SessionID, rec.OrgID, rec.SessionID)
	}
	if h.ShimID != rec.ShimID {
		return fmt.Errorf("%w: shim reports id %q, record says %q", ErrAdoptionRefused, h.ShimID, rec.ShimID)
	}
	if h.PID != rec.PID || h.ProcessStartedAt != rec.ProcessStartedAt {
		// The live peer is a DIFFERENT process than the record describes — the
		// PID-reuse case §D2 requires be caught rather than trusted.
		return fmt.Errorf("%w: shim process identity pid=%d start=%d does not match record pid=%d start=%d",
			ErrAdoptionRefused, h.PID, h.ProcessStartedAt, rec.PID, rec.ProcessStartedAt)
	}
	if !h.Phase.Known() {
		return fmt.Errorf("%w: shim reports uninterpretable phase %q", ErrAdoptionRefused, h.Phase)
	}
	if rec.WorkareaPath != "" && h.WorkareaPath != rec.WorkareaPath {
		return fmt.Errorf("%w: shim reports workarea %q, record says %q",
			ErrAdoptionRefused, h.WorkareaPath, rec.WorkareaPath)
	}
	if expectedWorkarea != "" && h.WorkareaPath != expectedWorkarea {
		return fmt.Errorf("%w: shim reports workarea %q, this daemon expects %q",
			ErrAdoptionRefused, h.WorkareaPath, expectedWorkarea)
	}
	if expectedWorkareaRoot != "" {
		nested := filepath.Clean(expectedWorkareaRoot) != filepath.Clean(expectedWorkarea)
		if nested && rec.WorkareaRoot == "" {
			return fmt.Errorf("%w: nested discovery record omitted required workarea root %q",
				ErrAdoptionRefused, expectedWorkareaRoot)
		}
		if rec.WorkareaRoot != "" && filepath.Clean(rec.WorkareaRoot) != filepath.Clean(expectedWorkareaRoot) {
			return fmt.Errorf("%w: discovery record workarea root %q, this daemon expects %q",
				ErrAdoptionRefused, rec.WorkareaRoot, expectedWorkareaRoot)
		}
		if !nested && rec.WorkareaRoot == "" && filepath.Clean(h.WorkareaPath) != filepath.Clean(expectedWorkareaRoot) {
			return fmt.Errorf("%w: legacy flat record does not identify the expected root", ErrAdoptionRefused)
		}
	}
	return nil
}

// verifySocketIdentity confirms the record's socket path still names the exact
// socket file the record was written against.
//
// A path can be unlinked and recreated by anything that can write the directory;
// the (device, inode) pair cannot be reproduced that way. Without this, adoption
// would authenticate a socket that merely occupies the right NAME.
func verifySocketIdentity(rec Record) error {
	info, err := os.Stat(rec.SocketPath)
	if err != nil {
		return fmt.Errorf("sessionshim: stat shim socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrRegistryUnsafe, rec.SocketPath)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	//nolint:gosec,unconvert // Dev/Ino widths are platform-dependent; both are non-negative identifiers
	dev, ino := uint64(st.Dev), uint64(st.Ino)
	if rec.SocketDevice != 0 || rec.SocketInode != 0 {
		if dev != rec.SocketDevice || ino != rec.SocketInode {
			return fmt.Errorf("%w: socket at %s is device %d inode %d, record binds device %d inode %d",
				ErrAdoptionRefused, rec.SocketPath, dev, ino, rec.SocketDevice, rec.SocketInode)
		}
	}
	return checkOwnedBySelf(info, rec.SocketPath)
}

// Identity returns the adopted session's lifecycle identity.
func (c *Controller) Identity() Identity { return c.id }

// ControllerID returns the exact diagnostic controller id sent in Welcome.
// It is a process/registration correlation only, never the durable host id.
func (c *Controller) ControllerID() string { return c.controllerID }

// Generation returns the committed controller generation.
func (c *Controller) Generation() shimwire.Generation { return c.gen }

// SelectedVersion returns the local-wire version committed by the handshake.
func (c *Controller) SelectedVersion() uint32 { return c.selected }

// SupportsAuthoritativeSnapshot reports whether fresh inspect/emit proxying is
// available. Selected v1 remains adoptable but intentionally returns false.
func (c *Controller) SupportsAuthoritativeSnapshot() bool { return c.selected >= shimwire.V2 }

// SupportsFullHostFrames reports whether the selected local wire supplies one
// exact complete attach-frame event for every host sequence.
func (c *Controller) SupportsFullHostFrames() bool { return c.selected >= shimwire.V3 }

// Hello returns the shim's opening self-report.
func (c *Controller) Hello() shimwire.Hello { return c.hello }

// WorkareaRoot returns the optional secret-free discovery-record root. Empty
// means the adopted record predates session-root-v1.
func (c *Controller) WorkareaRoot() string { return c.workareaRoot }

// Adoption returns the replay disposition the shim committed to.
func (c *Controller) Adoption() shimwire.Adopted { return c.adopted }

// ResumeFrom returns the first sequence this controller requested during
// adoption. When non-zero, ResumeFrom-1 is the composing carrier's exact
// durable last-forwarded sequence at the adoption boundary.
func (c *Controller) ResumeFrom() uint64 { return c.resumeFrom }

// HarnessSurvived reports whether the shim's harness is still live. It is the
// operative question after a restart: adoption succeeded AND the workload
// continues.
func (c *Controller) HarnessSurvived() bool {
	return c.hello.Phase == shimwire.PhaseRunning || c.hello.Phase == shimwire.PhaseOrphaned
}

// HarnessIdentity returns the owned harness's process identity as the shim
// reports it. Comparing this across a daemon restart is what distinguishes "the
// session survived" from "a new process took over the same session id".
func (c *Controller) HarnessIdentity() ProcessIdentity {
	return ProcessIdentity{PID: c.hello.HarnessPID, StartedAt: c.hello.HarnessStartedAt}
}

// Events is the stream of everything the shim reports. Closed when the
// connection ends.
func (c *Controller) Events() <-chan ControllerEvent { return c.events }

// Done is closed when the controller's read loop has finished.
func (c *Controller) Done() <-chan struct{} { return c.done }

// WriteInput sends attributed input bytes under this controller's generation.
func (c *Controller) WriteInput(data []byte) error {
	return c.w.Write(shimwire.TypeInput, shimwire.EncodeInput(c.gen, data))
}

// SupportsAttributedInput reports whether the selected local wire can carry a
// relay-stamped userId alongside input bytes (shimwire.TypeAttributedInput,
// v4+). Selected v1/v2/v3 controllers never negotiate it — WriteAttributedInput
// degrades to WriteInput's exact byte-identical unattributed send for them.
func (c *Controller) SupportsAttributedInput() bool { return c.selected >= shimwire.V4 }

// WriteAttributedInput sends input bytes under this controller's generation,
// additionally carrying userID — the relay-stamped sender identity (§5 of the
// wire protocol) — when the selected local wire supports it
// (SupportsAttributedInput, v4+). It exists so the shim's last-hop
// pacing/paste-guard for SYSTEM-authority input (ptyhost/systeminput.go,
// attachwire.SystemNudgeUserID) can be applied at the true PTY write boundary
// instead of only on the composition paths that talk to ptyhost directly.
//
// A shim negotiated below v4 cannot decode the attribution field — sending it
// anyway would either desync the wire or be silently misinterpreted as input
// bytes, neither acceptable — so this falls back to the exact byte-identical
// WriteInput send those shims have always received. The write still lands,
// verbatim; only the last-hop guarantee is unavailable there, exactly like an
// old selected-v2 shim never receiving HostFrame.
func (c *Controller) WriteAttributedInput(userID, data []byte) error {
	if !c.SupportsAttributedInput() {
		return c.WriteInput(data)
	}
	body, err := shimwire.EncodeAttributedInput(c.gen, userID, data)
	if err != nil {
		return err
	}
	return c.w.WriteVersion(c.selected, shimwire.TypeAttributedInput, body)
}

// Resize sends authoritative geometry under this controller's generation.
func (c *Controller) Resize(cols, rows, pxWidth, pxHeight uint32) error {
	return writeTyped(c.w, shimwire.TypeResize, func() ([]byte, error) {
		return shimwire.EncodeResize(shimwire.ResizeMsg{
			Generation: c.gen, Cols: cols, Rows: rows, PxWidth: pxWidth, PxHeight: pxHeight,
		})
	})
}

// Stop asks the shim to terminate and reap its harness, under this controller's
// generation.
func (c *Controller) Stop(reason shimwire.StopReason) error {
	return writeTyped(c.w, shimwire.TypeStop, func() ([]byte, error) {
		return shimwire.EncodeStop(shimwire.StopMsg{Generation: c.gen, Reason: reason})
	})
}

// Heartbeat sends liveness plus the highest sequence this controller has durably
// forwarded. That acknowledgement is what a LATER adoption resumes from, so a
// controller that never heartbeats will simply be replayed more after a restart —
// never less. Selected v3 returns only after the shim echoes an exact
// fsync-backed persistence receipt; selected v1/v2 retain their released
// write-only behavior.
func (c *Controller) Heartbeat(ackedSeq uint64) error {
	heartbeat := shimwire.HeartbeatMsg{Generation: c.gen, AckedSeq: ackedSeq}
	if c.selected < shimwire.V3 {
		return writeTyped(c.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
			return shimwire.EncodeHeartbeat(heartbeat)
		})
	}
	c.heartbeatCallMu.Lock()
	defer c.heartbeatCallMu.Unlock()
	body, err := shimwire.EncodeHeartbeat(heartbeat)
	if err != nil {
		return err
	}
	call := &heartbeatCall{expected: heartbeat, done: make(chan heartbeatResult, 1)}
	c.heartbeatMu.Lock()
	if c.heartbeatCall != nil {
		c.heartbeatMu.Unlock()
		return errors.New("sessionshim: selected-v3 heartbeat already pending")
	}
	c.heartbeatCall = call
	// A retry that re-sends the SAME correlation reclaims it: the outstanding
	// entry and this call are the same question, and leaving the entry in place
	// would let the answer be swallowed as a late receipt while the live call
	// waited out its bound for a reply that already arrived.
	c.consumePendingHeartbeatReceiptLocked(heartbeat)
	c.heartbeatMu.Unlock()
	if err := c.w.WriteVersion(c.selected, shimwire.TypeHeartbeat, body); err != nil {
		c.clearHeartbeatCall(call)
		return err
	}
	timer := time.NewTimer(heartbeatReceiptWaitBound)
	defer timer.Stop()
	select {
	case result := <-call.done:
		return result.err
	case <-timer.C:
		// A receipt that has not arrived within the bound is a statement about
		// how fast the durable side is answering, NOT about whether this
		// connection works. Dropping the connection here cost two healthy seats
		// on an installed host: the shims lost their controllers, nothing
		// re-adopted them, and they reaped their own harnesses on the orphan
		// deadline — over a persistence step that was merely slow. The cursor
		// still must not claim a sequence the shim did not store, so the caller
		// gets a distinguishable retryable failure and the stream stays up.
		c.clearHeartbeatCall(call)
		c.rememberPendingHeartbeatReceipt(heartbeat)
		c.log().Warn("sessionshim: durable heartbeat receipt is still pending; keeping the shim connection and retrying",
			"session", c.id.String(), "selected", c.selected,
			"ackedSeq", heartbeat.AckedSeq, "waited", heartbeatReceiptWaitBound)
		return fmt.Errorf("%w: acked sequence %d", ErrHeartbeatReceiptPending, heartbeat.AckedSeq)
	case <-c.done:
		c.clearHeartbeatCall(call)
		return io.EOF
	}
}

// rememberPendingHeartbeatReceipt records one outstanding correlation so a late
// receipt is consumed rather than treated as unsolicited.
func (c *Controller) rememberPendingHeartbeatReceipt(sent shimwire.HeartbeatMsg) {
	correlation := heartbeatCorrelation{generation: sent.Generation, ackedSeq: sent.AckedSeq}
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	for _, pending := range c.pendingReceipts {
		if pending == correlation {
			return
		}
	}
	c.pendingReceipts = append(c.pendingReceipts, correlation)
	if len(c.pendingReceipts) > controllerPendingReceiptLimit {
		c.pendingReceipts = c.pendingReceipts[len(c.pendingReceipts)-controllerPendingReceiptLimit:]
	}
}

// consumePendingHeartbeatReceiptLocked reports whether receipt answers a
// heartbeat whose receipt this controller already stopped waiting for, and
// forgets it when it does. c.heartbeatMu must be held.
func (c *Controller) consumePendingHeartbeatReceiptLocked(receipt shimwire.HeartbeatMsg) bool {
	correlation := heartbeatCorrelation{generation: receipt.Generation, ackedSeq: receipt.AckedSeq}
	for i, pending := range c.pendingReceipts {
		if pending != correlation {
			continue
		}
		c.pendingReceipts = append(c.pendingReceipts[:i], c.pendingReceipts[i+1:]...)
		return true
	}
	return false
}

// durableAckOutstanding reports whether a durable heartbeat this controller
// sent has not been answered: either a call is in flight right now, or one gave
// up on its wait bound and is being retried while its receipt may still land.
//
// It is the "the consumer is waiting, not gone" predicate the backlog stall
// consults — see durableAckAmbiguityBound.
func (c *Controller) durableAckOutstanding() bool {
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	return c.heartbeatCall != nil || len(c.pendingReceipts) > 0
}

func (c *Controller) clearHeartbeatCall(call *heartbeatCall) {
	c.heartbeatMu.Lock()
	if c.heartbeatCall == call {
		c.heartbeatCall = nil
	}
	c.heartbeatMu.Unlock()
}

// InspectSnapshot returns exact shim-owned encoded screen bytes and atSeq
// without allocating a host output sequence.
func (c *Controller) InspectSnapshot(ctx context.Context) (shimwire.SnapshotResult, error) {
	return c.SnapshotWithID(ctx, c.allocateSnapshotRequestID(), shimwire.SnapshotInspect)
}

// EmitSnapshot asks the shim-owned PTY host to emit exactly one Snapshot frame.
// Selected v2 returns the complete live frame in the result. Selected v3
// delivers it once through EventHostFrame and returns correlation-only empty
// result bytes for the in-stream disposition.
func (c *Controller) EmitSnapshot(ctx context.Context) (shimwire.SnapshotResult, error) {
	return c.SnapshotWithID(ctx, c.allocateSnapshotRequestID(), shimwire.SnapshotEmit)
}

// SnapshotWithID exposes exact retry semantics for a connection-local id. An
// exact retry shares/returns the first immutable result; changing mode under an
// existing id is refused before another wire request can be emitted.
func (c *Controller) SnapshotWithID(ctx context.Context, requestID uint64, mode shimwire.SnapshotMode) (shimwire.SnapshotResult, error) {
	if !c.SupportsAuthoritativeSnapshot() {
		return shimwire.SnapshotResult{}, fmt.Errorf("sessionshim: %w: selected local-wire v%d has no authoritative snapshot proxy", shimwire.ErrVersionMismatch, c.selected)
	}
	req := shimwire.SnapshotRequest{RequestID: requestID, Generation: c.gen, Mode: mode}
	body, err := shimwire.EncodeSnapshotRequest(req)
	if err != nil {
		return shimwire.SnapshotResult{}, err
	}
	c.snapshotMu.Lock()
	call := c.snapshotCalls[requestID]
	if call != nil && call.request != req {
		c.snapshotMu.Unlock()
		return shimwire.SnapshotResult{}, fmt.Errorf("sessionshim: %w: changed replay for request id %d", shimwire.ErrSnapshotMismatch, requestID)
	}
	if call == nil {
		if len(c.snapshotCalls) >= controllerSnapshotRetryLedgerLimit {
			c.snapshotMu.Unlock()
			return shimwire.SnapshotResult{}, fmt.Errorf("sessionshim: %w: controller retry ledger is full", shimwire.ErrSnapshotRefused)
		}
		call = &snapshotCall{request: req, done: make(chan struct{})}
		c.snapshotCalls[requestID] = call
	}
	if call.result != nil {
		result := cloneSnapshotResult(*call.result)
		err := call.err
		c.snapshotMu.Unlock()
		return result, err
	}
	if !call.sent {
		call.sent = true
		if err := c.w.WriteVersion(c.selected, shimwire.TypeSnapshotRequest, body); err != nil {
			call.err = err
			close(call.done)
		}
	}
	done := call.done
	c.snapshotMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		c.snapshotMu.Lock()
		defer c.snapshotMu.Unlock()
		if call.result == nil {
			return shimwire.SnapshotResult{}, call.err
		}
		return cloneSnapshotResult(*call.result), call.err
	case <-ctx.Done():
		return shimwire.SnapshotResult{}, fmt.Errorf("sessionshim: snapshot request %d: %w", requestID, ctx.Err())
	case <-c.done:
		return shimwire.SnapshotResult{}, io.EOF
	}
}

func (c *Controller) allocateSnapshotRequestID() uint64 {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	for {
		c.nextSnapshotID++
		if c.nextSnapshotID == 0 {
			c.nextSnapshotID++
		}
		if _, exists := c.snapshotCalls[c.nextSnapshotID]; !exists {
			return c.nextSnapshotID
		}
	}
}

func cloneSnapshotResult(in shimwire.SnapshotResult) shimwire.SnapshotResult {
	in.Bytes = append([]byte(nil), in.Bytes...)
	return in
}

// log is the nil-safe controller logger. A controller assembled without one
// still has to be able to say why it dropped a connection.
func (c *Controller) log() *slog.Logger {
	if c.logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return c.logger
}

// BacklogFlowState reports this controller's back-pressure state.
//
// A daemon publishes it so an operator can see "this carrier is degraded
// because nothing is draining it" while the session is still alive, instead of
// learning it from the drop. Pre-v3 controllers have no backlog and report the
// zero value.
func (c *Controller) BacklogFlowState() BacklogFlowState { return c.backlog.flowState() }

// reportBacklogFlow is the backlog's report hook: one operator-visible line per
// degrade tick and one per recovery, carrying the numbers the decision was made
// on. It runs on the reader and consumer goroutines, so it does nothing but log.
func (c *Controller) reportBacklogFlow(state BacklogFlowState) {
	if !state.Degraded {
		c.log().Info("sessionshim: durable consumer resumed draining; carrier back-pressure released",
			"session", c.id.String(), "queuedBytes", state.QueuedBytes, "budgetBytes", state.Budget)
		return
	}
	c.log().Warn("sessionshim: durable consumer is not draining; holding the carrier under back-pressure",
		"session", c.id.String(), "queuedBytes", state.QueuedBytes, "budgetBytes", state.Budget,
		"drainedBytes", state.DrainedBytes,
		"stalledFor", time.Since(state.StalledSince).Round(time.Second),
		"dropBound", state.DropBound)
}

// closeStream drops the connection after a fail-closed stream decision and says
// why it did.
//
// The reason is not decoration. A silent drop here is indistinguishable, from
// every later caller's side, from a peer that went away: input, resize, and the
// durable heartbeat all come back with "use of closed network connection" and
// nothing anywhere names the decision that caused it.
func (c *Controller) closeStream(reason string, cause error) {
	c.noteStreamEndCause(cause)
	c.log().Warn("sessionshim: controller dropped its shim connection",
		"session", c.id.String(), "reason", reason,
		"selected", c.selected, "error", cause)
	_ = c.Close()
}

// noteStreamEndCause records the FIRST fail-closed decision this controller
// made about its own stream.
func (c *Controller) noteStreamEndCause(cause error) {
	if cause == nil {
		return
	}
	c.streamEndMu.Lock()
	if c.streamEnd == nil {
		c.streamEnd = cause
	}
	c.streamEndMu.Unlock()
}

// StreamEndCause reports the fail-closed decision THIS controller made to drop
// its own shim connection, or nil when the stream ended any other way — the
// peer closed it, the socket went away, or a caller called Close.
//
// A consumer that cannot tell those apart has to guess, and the guess it made
// was the expensive one: it read its own back-pressure hanging up on a
// perfectly reachable socket as the socket being unreachable, and published
// that as the lineage's recovery-obligation reason. Nil here means "the ending
// was not mine"; non-nil names whose decision it was and why.
func (c *Controller) StreamEndCause() error {
	c.streamEndMu.Lock()
	defer c.streamEndMu.Unlock()
	return c.streamEnd
}

// Close drops the controller connection WITHOUT stopping the session. This is
// what a daemon shutdown does: the shim keeps the harness and starts its bounded
// orphan clock.
func (c *Controller) Close() error {
	c.closeOne.Do(func() {
		close(c.closing)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
	return nil
}

func (c *Controller) readLoop() {
	defer close(c.done)
	if c.selected >= shimwire.V3 {
		defer c.backlog.close()
	} else {
		defer close(c.events)
	}
	stream := hostFrameStreamState{expectedFirst: c.adopted.ReplayFrom}
	var pendingRequested *ControllerEvent
	for {
		msg, err := c.r.ReadVersion(c.selected)
		if err != nil {
			c.failSnapshotCalls(err)
			return
		}
		if pendingRequested != nil && msg.Type != shimwire.TypeSnapshotResult {
			c.failSnapshotCalls(shimwire.ErrSnapshotMismatch)
			c.closeStream("requested Snapshot was not followed by its result", shimwire.ErrSnapshotMismatch)
			return
		}
		if c.selected >= shimwire.V3 && msg.Type == shimwire.TypeHeartbeat {
			receipt, decodeErr := shimwire.DecodeHeartbeat(msg.Body)
			if decodeErr != nil {
				c.failHeartbeatCall(decodeErr)
				c.closeStream("heartbeat receipt did not decode", decodeErr)
				return
			}
			if receiptErr := c.acceptHeartbeatReceipt(receipt); receiptErr != nil {
				c.closeStream("heartbeat receipt was refused", receiptErr)
				return
			}
			continue
		}
		if c.selected >= shimwire.V3 && msg.Type == shimwire.TypeError {
			if refused, terminal := c.failHeartbeatFromError(msg.Body); refused {
				if !terminal {
					c.closeStream("shim refused the durable heartbeat", nil)
					return
				}
				// A TERMINAL refusal is not a reason to drop the stream — it is
				// the shim saying its tombstone is already on disk while it is
				// still flushing the one Exit HostFrame that ends the session
				// on THIS connection. Dropping here threw that observation
				// away: the consumer then saw a stream that ended without a
				// terminal frame, quarantined a lineage whose proof existed,
				// and left the harness held. Keep reading.
				continue
			}
		}
		if c.selected >= shimwire.V3 {
			switch msg.Type {
			case shimwire.TypeHostFrame:
				ev, decodeErr := decodeHostFrameEvent(msg.Body)
				if decodeErr != nil || pendingRequested != nil || stream.exited {
					c.failSnapshotCalls(shimwire.ErrDuplicateHostFrame)
					c.closeStream("HostFrame arrived out of contract", cmp.Or(decodeErr, error(shimwire.ErrDuplicateHostFrame)))
					return
				}
				if ev.RequestID != 0 {
					pendingRequested = &ev
					continue
				}
				if err := c.publishHostFrameEvent(ev, &stream); err != nil {
					c.failSnapshotCalls(err)
					c.closeStream("HostFrame could not be published", err)
					return
				}
				continue
			case shimwire.TypeOutput, shimwire.TypeSnapshot, shimwire.TypeExit:
				c.failSnapshotCalls(shimwire.ErrDuplicateHostFrame)
				c.closeStream("selected-v3 peer sent a pre-v3 frame type", shimwire.ErrDuplicateHostFrame)
				return
			}
		}
		if msg.Type == shimwire.TypeSnapshotResult {
			ev, emit, completion, resultErr := c.acceptSnapshotResult(msg.Body, pendingRequested)
			pendingRequested = nil
			if resultErr != nil {
				c.failSnapshotCalls(resultErr)
				c.closeStream("Snapshot result was refused", resultErr)
				return
			}
			if emit {
				if c.selected >= shimwire.V3 {
					if err := c.publishHostFrameEvent(ev, &stream); err != nil {
						c.completeSnapshotCall(completion, err)
						c.failSnapshotCalls(err)
						c.closeStream("emitted Snapshot could not be published", err)
						return
					}
				} else {
					if err := c.publishEvent(ev); err != nil {
						c.failSnapshotCalls(err)
						c.closeStream("emitted Snapshot could not be published", err)
						return
					}
				}
			}
			c.completeSnapshotCall(completion, nil)
			continue
		}
		ev, ok := decodeEvent(msg)
		if !ok {
			continue
		}
		if ev.Kind == EventGap {
			if c.selected >= shimwire.V3 {
				expectedFrom := c.resumeFrom
				if expectedFrom == 0 {
					expectedFrom = uint64(attachwire.HostSeqStart)
				}
				if stream.seen || stream.gap != nil || ev.Gap.FromSeq != expectedFrom ||
					ev.Gap.ToSeq == ^uint64(0) || ev.Gap.ToSeq+1 != c.adopted.ReplayFrom {
					c.failSnapshotCalls(shimwire.ErrDuplicateHostFrame)
					c.closeStream("declared Gap does not match the negotiated replay point", shimwire.ErrDuplicateHostFrame)
					return
				}
				gap := ev.Gap
				stream.gap = &gap
			}
			// A gap that is never logged is a gap nobody can audit. §D5 makes the
			// shim declare it explicitly; the least a controller can do is not be
			// the layer that swallows it.
			c.logger.Warn("sessionshim: replay gap declared by shim",
				"session", c.id.String(), "fromSeq", ev.Gap.FromSeq, "toSeq", ev.Gap.ToSeq, "reason", ev.Gap.Reason)
		}
		if ev.Kind == EventExit {
			if err := c.observeExit(ev.Exit); err != nil {
				c.failSnapshotCalls(err)
				c.closeStream("terminal observation was refused", err)
				return
			}
		}
		if err := c.publishEvent(ev); err != nil {
			c.failSnapshotCalls(err)
			c.closeStream("event could not be published", err)
			return
		}
	}
}

func (c *Controller) acceptHeartbeatReceipt(receipt shimwire.HeartbeatMsg) error {
	c.heartbeatMu.Lock()
	// A receipt for a heartbeat this controller gave up waiting for is LATE,
	// not unsolicited — and it is not the answer to whatever call is live now
	// either. Consuming it is what makes ErrHeartbeatReceiptPending safe: the
	// stream survives the stall, and a live retry keeps waiting for its own
	// answer. (A retry that re-sent the same correlation already reclaimed the
	// outstanding entry, so it resolves here as the live call, not as a late
	// one.)
	if c.consumePendingHeartbeatReceiptLocked(receipt) {
		c.prunePendingReceiptsLocked(receipt)
		c.heartbeatMu.Unlock()
		if !receipt.Phase.Known() {
			return errors.New("sessionshim: late selected-v3 heartbeat receipt carried an unknown phase")
		}
		return nil
	}
	call := c.heartbeatCall
	if call == nil {
		c.heartbeatMu.Unlock()
		return errors.New("sessionshim: unsolicited selected-v3 heartbeat receipt")
	}
	c.heartbeatCall = nil
	matched := receipt.Generation == call.expected.Generation &&
		receipt.AckedSeq == call.expected.AckedSeq && receipt.Phase.Known()
	if matched {
		c.prunePendingReceiptsLocked(receipt)
	}
	c.heartbeatMu.Unlock()
	if !matched {
		err := errors.New("sessionshim: selected-v3 heartbeat receipt changed generation, cursor, or phase")
		call.done <- heartbeatResult{err: err}
		return err
	}
	call.done <- heartbeatResult{}
	return nil
}

// prunePendingReceiptsLocked drops outstanding correlations that a confirmed
// receipt has made moot. c.heartbeatMu must be held.
//
// # WHY THIS IS NOT BOOKKEEPING
//
// The ledger is what durableAckOutstanding reads, and without this it is a
// STICKY LATCH. A retry never re-sends the correlation that stalled — the
// cursor has advanced, so it carries a new (generation, ackedSeq) — and the
// stalled entry is only ever removed by a receipt for that exact pair, which a
// durable side that has since moved on will never send. One slow receipt would
// therefore leave the ledger non-empty for the life of the controller, and
// every LATER stall on it, however unrelated, would hold the full ambiguity
// bound instead of failing closed at the stall deadline: the mechanism would
// quietly stop distinguishing the two cases it exists to distinguish.
//
// A confirmed receipt at (g, n) settles that: the cursor is monotonic and the
// generation only advances, so every outstanding correlation at an older
// generation, or at the same generation and a sequence no newer, has been
// overtaken and is not coming back.
func (c *Controller) prunePendingReceiptsLocked(receipt shimwire.HeartbeatMsg) {
	kept := c.pendingReceipts[:0]
	for _, pending := range c.pendingReceipts {
		overtaken := pending.generation < receipt.Generation ||
			(pending.generation == receipt.Generation && pending.ackedSeq <= receipt.AckedSeq)
		if overtaken {
			continue
		}
		kept = append(kept, pending)
	}
	c.pendingReceipts = kept
}

// failHeartbeatFromError completes a pending acknowledgement from a refusal
// frame. It reports whether the frame was consumed as one, and whether the
// refusal was the TERMINAL one — the two answers the read loop needs, because
// only one of them is a reason to drop the connection.
func (c *Controller) failHeartbeatFromError(body []byte) (refused, terminal bool) {
	c.heartbeatMu.Lock()
	call := c.heartbeatCall
	if call == nil {
		c.heartbeatMu.Unlock()
		return false, false
	}
	c.heartbeatCall = nil
	c.heartbeatMu.Unlock()
	message, err := shimwire.DecodeError(body)
	switch {
	case err != nil:
		call.done <- heartbeatResult{err: err}
	case message.Code == shimwire.CodeExited:
		// The shim is not failing to answer — it is answering that its terminal
		// proof is already published (shim.go's "heartbeat rejected: terminal
		// proof is published"). That is a FACT about the lifecycle, not a
		// transport failure, and a caller that only sees a formatted string
		// throws it away and quarantines a session whose tombstone is on disk.
		call.done <- heartbeatResult{err: fmt.Errorf("%w: %s", ErrShimExited, message.Code)}
		return true, true
	default:
		call.done <- heartbeatResult{err: fmt.Errorf("sessionshim: selected-v3 heartbeat refused: %s", message.Code)}
	}
	return true, false
}

func (c *Controller) failHeartbeatCall(err error) {
	c.heartbeatMu.Lock()
	call := c.heartbeatCall
	if call != nil {
		c.heartbeatCall = nil
	}
	c.heartbeatMu.Unlock()
	if call != nil {
		call.done <- heartbeatResult{err: err}
	}
}

type hostFrameStreamState struct {
	seen          bool
	last          uint64
	exited        bool
	gap           *shimwire.GapMsg
	expectedFirst uint64
}

func (c *Controller) publishHostFrameEvent(ev ControllerEvent, stream *hostFrameStreamState) error {
	if ev.Kind != EventHostFrame || ev.Seq == 0 || stream.exited {
		return shimwire.ErrDuplicateHostFrame
	}
	switch {
	case stream.gap != nil:
		if ev.FrameType != attachwire.TypeSnapshot || ev.Seq != stream.gap.ToSeq+1 {
			return fmt.Errorf("sessionshim: %w: Gap is not followed by its exact recovery Snapshot", shimwire.ErrSnapshotMismatch)
		}
		stream.gap = nil
	case !stream.seen && stream.expectedFirst != 0 && ev.Seq != stream.expectedFirst:
		return fmt.Errorf("sessionshim: %w: first HostFrame sequence %d, want %d",
			shimwire.ErrSnapshotMismatch, ev.Seq, stream.expectedFirst)
	case stream.seen && ev.Seq != stream.last+1:
		return fmt.Errorf("sessionshim: %w: HostFrame sequence %d follows %d", shimwire.ErrSnapshotMismatch, ev.Seq, stream.last)
	}
	stream.seen, stream.last = true, ev.Seq
	if ev.FrameType == attachwire.TypeExit {
		if err := c.observeExit(ev.Exit); err != nil {
			return err
		}
		stream.exited = true
	}
	return c.publishEvent(ev)
}

func (c *Controller) publishEvent(event ControllerEvent) error {
	if c.selected >= shimwire.V3 {
		return c.backlog.push(event)
	}
	select {
	case c.events <- event:
		return nil
	case <-c.closing:
		return io.EOF
	}
}

func (c *Controller) dispatchEvents() {
	defer close(c.events)
	for {
		event, ok := c.backlog.pop()
		if !ok {
			return
		}
		select {
		case c.events <- event:
		case <-c.closing:
			return
		}
	}
}

// ErrEventBacklogExceeded reports a consumer that made NO progress for the
// whole of EventBacklogDropBound while the backlog sat at its budget. It is a
// fail-closed decision, not a transport error: the connection is dropped, the
// shim KEEPS the harness (carrierLost=false) and re-adoption restores the
// stream from the shim's own sequence.
//
// Two earlier meanings have been retired from it, each after a measured
// incident. Merely reaching the budget does not produce it — a consumer that is
// behind gets back-pressure. And crossing the STALL DEADLINE no longer produces
// it either: that is where the stall is reported as degraded and the reader is
// held, which is the whole of EventBacklogDropBound's story. Only a consumer
// that has made no progress for the whole drop bound gets this.
var ErrEventBacklogExceeded = errors.New("sessionshim: event backlog exceeded the in-flight budget")

// ErrDurableAckAmbiguityBound reports the OTHER way a stalled reader gives up:
// the consumer was provably still there — a durable acknowledgement this
// controller sent was outstanding the whole time — and the durable side never
// answered for durableAckAmbiguityBound.
//
// It is a separate sentinel from ErrEventBacklogExceeded because the two say
// opposite things about the peer, and one caller downstream has to tell them
// apart: an exhausted ambiguity bound is a statement about the DURABLE side's
// latency with a reachable socket, and reporting it as a socket that could not
// be reached is the false terminal evidence this sentinel exists to prevent.
var ErrDurableAckAmbiguityBound = errors.New("sessionshim: durable acknowledgements stayed outstanding past the ambiguity bound")

// ErrShimExited reports that a shim refused a request because it has already
// published its terminal proof.
//
// It is a sentinel rather than a message because the refusal is ACTIONABLE: the
// tombstone exists by the time the shim answers this way, so the right response
// is to go and consume it, not to treat the exchange as a broken socket and
// leave the lineage in reconciliation quarantine until some later surface
// happens to look.
var ErrShimExited = errors.New("sessionshim: shim refused: terminal proof is already published")

// heartbeatReceiptWaitBound is how long Heartbeat waits for a selected-v3
// persistence receipt before reporting the receipt PENDING. It bounds one
// caller's wait; it is not a health verdict on the connection.
const heartbeatReceiptWaitBound = 5 * time.Second

// ErrHeartbeatReceiptPending reports that a selected-v3 durable heartbeat's
// persistence receipt had not arrived within heartbeatReceiptWaitBound.
//
// It is a sentinel rather than a message because the distinction is the whole
// fix: a receipt that is merely SLOW says nothing about whether this connection
// works, and dropping the shim's controller over one costs a live harness its
// supervision. The cursor still must not advance — the shim has not said it
// stored that sequence — so the caller keeps the connection, treats the
// acknowledgement as outstanding, and retries with backoff. A receipt that
// arrives after the wait is consumed as the answer it is, never as an
// unsolicited frame.
var ErrHeartbeatReceiptPending = errors.New("sessionshim: durable heartbeat persistence receipt is still pending")

// eventBacklog is the bounded hand-off between the socket reader and the
// consumer, accounted in payload bytes rather than frames.
//
// Frames are not uniform. A frame count cannot bound memory (one frame may be
// megabytes) and cannot express "as much as the shim itself will hold", which is
// the only bound with a principled source. Bytes do both.
type eventBacklog struct {
	mu     sync.Mutex
	queue  []ControllerEvent
	bytes  int
	closed bool
	// stalledSince is when the consumer last failed to keep up: set the first
	// time a push has to wait, cleared when the consumer makes progress. It is
	// the anchor of the CUMULATIVE stall deadline — see push.
	stalledSince time.Time
	// drainedSinceStall is how many payload bytes pop has handed over since
	// stalledSince was set. Reaching one budget's worth of them is what counts
	// as progress; see eventBacklogProgressBytes.
	drainedSinceStall int

	// ambiguousSince is when the stall clock was FIRST held open for an
	// outstanding durable acknowledgement. It anchors durableAckAmbiguityBound
	// absolutely: re-anchoring stalledSince must not also re-anchor the bound,
	// or a durable side that answers nothing at all would hold the reader
	// forever.
	ambiguousSince time.Time

	// degradedSince is when this stall FIRST crossed the stall deadline without
	// the durable side answering for it — the anchor of dropBound, and the
	// instant from which the stall is reported as degraded. It is cleared with
	// the stall it belongs to, in pop, so a later unrelated stall never
	// inherits a bound that has already been spent.
	degradedSince time.Time
	// reported is whether the current degradation has already been announced,
	// so recovery is announced exactly once against exactly one announcement.
	reported bool

	// budget, stall, dropBound, ambiguityBound, ambiguous, report and abort are
	// immutable after construction; push reads them without the lock.
	budget int
	stall  time.Duration
	// dropBound is the absolute time from degradedSince that the reader is held
	// before it fails closed. See EventBacklogDropBound.
	dropBound time.Duration
	// report, when set, observes every degrade/recover transition. It is called
	// off the lock, from whichever goroutine crossed the boundary, and must not
	// block: the reader is one of those goroutines.
	report func(BacklogFlowState)
	// ambiguous reports whether a durable acknowledgement this controller sent
	// is still outstanding — the one condition under which a stalled consumer
	// is provably present rather than gone. Nil means "never ambiguous", which
	// is the pre-v3 behaviour: those controllers send no durable heartbeat.
	ambiguous      func() bool
	ambiguityBound time.Duration
	// abort unblocks a stalled push when the controller is being closed. Without
	// it, Close would wait on a read loop parked behind a queue that only the
	// read loop's own unwinding will ever release.
	abort <-chan struct{}

	// arrived and drained are broadcast latches: each is CLOSED and replaced
	// whenever the queue grows or shrinks. A condition variable cannot be used
	// here because sync.Cond has no timed wait, and the stall deadline is the
	// whole point — a Cond would park the reader on a consumer that stopped and
	// never reach the fail-closed decision.
	arrived chan struct{}
	drained chan struct{}
}

func newEventBacklog(
	budget int,
	stall time.Duration,
	abort <-chan struct{},
	ambiguous func() bool,
	ambiguityBound time.Duration,
	dropBound time.Duration,
) *eventBacklog {
	if budget <= 0 {
		budget = EventBacklogBudget
	}
	// Clamped HERE rather than at the option seam, because this is the one
	// place every controller passes through: an embedder cannot reach a
	// deadline below eventBacklogStallFloor, a bound below
	// durableAckAmbiguityFloor, or a drop bound under the stall deadline it
	// carries, by any route.
	resolvedStall := clampEventBacklogStall(stall)
	return &eventBacklog{
		budget:         budget,
		stall:          resolvedStall,
		dropBound:      clampEventBacklogDropBound(dropBound, resolvedStall),
		abort:          abort,
		ambiguous:      ambiguous,
		ambiguityBound: clampDurableAckAmbiguityBound(ambiguityBound, resolvedStall),
		arrived:        make(chan struct{}),
		drained:        make(chan struct{}),
	}
}

// BacklogFlowState is the observable back-pressure state of one controller's
// event backlog: what a stalled carrier looks like from outside it.
//
// It exists because "degraded" has to be a published FACT, not an inference. A
// carrier whose consumer has stalled is indistinguishable, in every other field
// a daemon publishes, from one attached to an idle terminal — which is how a
// host serving nothing kept looking healthy right up to the moment it dropped
// seven sessions.
type BacklogFlowState struct {
	// Degraded reports that the consumer has been at the budget without a
	// budget's worth of progress for at least one whole stall deadline, and the
	// reader is being HELD rather than failed closed.
	Degraded bool
	// StalledSince is when this stall first crossed the deadline, or the zero
	// time when the carrier is not degraded.
	StalledSince time.Time
	// QueuedBytes is how much undelivered stream the backlog is holding.
	QueuedBytes int
	// DrainedBytes is how much the consumer has taken since the stall clock was
	// last anchored. Short of Budget for a whole deadline is what "no progress"
	// means here.
	DrainedBytes int
	// Budget is the in-flight budget this backlog was built with.
	Budget int
	// DropBound is when the hold gives up and the carrier is dropped, measured
	// from StalledSince.
	DropBound time.Duration
}

// flowStateLocked samples the state under b.mu.
func (b *eventBacklog) flowStateLocked() BacklogFlowState {
	return BacklogFlowState{
		Degraded:     !b.degradedSince.IsZero(),
		StalledSince: b.degradedSince,
		QueuedBytes:  b.bytes,
		DrainedBytes: b.drainedSinceStall,
		Budget:       b.budget,
		DropBound:    b.dropBound,
	}
}

// flowState samples the state for diagnostics. Safe on a nil backlog: a
// pre-v3 controller has none, and asking it is not an error.
func (b *eventBacklog) flowState() BacklogFlowState {
	if b == nil {
		return BacklogFlowState{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flowStateLocked()
}

// holdForFlowControl decides what an elapsed stall deadline with nothing
// outstanding may do, and returns the state to report with it.
//
// The answer used to be "fail closed", and that was the kill switch. It is now
// "degrade visibly and keep stalling", up to dropBound measured from the first
// crossing. Keeping the reader stalled is not passive: the stall propagates
// back through the socket to the shim's output pump, and from there to the
// shim's own PTY reader, which stops reading the master so the harness blocks
// in write(2). Nothing is lost, nothing is reordered, and the seat lives.
func (b *eventBacklog) holdForFlowControl(now time.Time) (BacklogFlowState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.degradedSince.IsZero() {
		// Anchored on when the consumer first fell BEHIND, not on the crossing,
		// so the bound means "ten minutes of a stalled carrier" rather than
		// "ten minutes plus however long the deadline happened to be".
		b.degradedSince = b.stalledSince
		if b.degradedSince.IsZero() {
			b.degradedSince = now
		}
	}
	state := b.flowStateLocked()
	if now.Sub(b.degradedSince) >= b.dropBound {
		return state, false
	}
	b.reported = true
	// Re-anchor the stall clock so the NEXT deadline produces the next report:
	// a stall that lasts minutes has to keep saying so, with current numbers,
	// rather than announce itself once and go quiet.
	b.stalledSince, b.drainedSinceStall = now, 0
	return state, true
}

// notify hands one transition to the report hook. It is called off the lock.
func (b *eventBacklog) notify(state BacklogFlowState) {
	if b.report != nil {
		b.report(state)
	}
}

// ambiguityHoldOutcome is what a stall deadline that has just elapsed is
// allowed to do about it.
type ambiguityHoldOutcome uint8

const (
	// ambiguityHoldNotAmbiguous: nothing is outstanding, so nothing contradicts
	// the ordinary verdict. The consumer stopped.
	ambiguityHoldNotAmbiguous ambiguityHoldOutcome = iota
	// ambiguityHoldGranted: a durable acknowledgement is outstanding and the
	// bound has room. Keep stalling.
	ambiguityHoldGranted
	// ambiguityHoldBoundReached: the hold ran the whole bound out. Degrade
	// visibly, under the sentinel that says so.
	ambiguityHoldBoundReached
)

// holdForDurableAckAmbiguity reports what a stall deadline that has just
// elapsed may do, re-anchoring the stall clock when the answer is "keep
// waiting", and returns how long the hold has run.
//
// The three answers are kept distinct because two of them look identical from
// the anchor alone. An anchor that is SET but no longer ambiguous — the durable
// side answered, the stall simply continued for its own reasons — must fall
// through to the ordinary backlog verdict, not report a bound it never reached;
// reading the anchor as the whole answer produced exactly that, an
// "ErrDurableAckAmbiguityBound after 1.2s" against a ten-minute bound. So the
// anchor is cleared the moment ambiguity ends, and only the bound-reached
// branch may name the sentinel.
//
// See DurableAckAmbiguityBound for the measured incident and the contract clause.
func (b *eventBacklog) holdForDurableAckAmbiguity(now time.Time) (ambiguityHoldOutcome, time.Duration) {
	ambiguous := b.ambiguous != nil && b.ambiguous()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !ambiguous {
		b.ambiguousSince = time.Time{}
		return ambiguityHoldNotAmbiguous, 0
	}
	if b.ambiguousSince.IsZero() {
		b.ambiguousSince = now
	}
	held := now.Sub(b.ambiguousSince)
	if held >= b.ambiguityBound {
		return ambiguityHoldBoundReached, held
	}
	b.stalledSince, b.drainedSinceStall = now, 0
	return ambiguityHoldGranted, held
}

// ambiguityAnchored reports whether a hold anchor is currently set.
// Diagnostics and tests only.
func (b *eventBacklog) ambiguityAnchored() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.ambiguousSince.IsZero()
}

// eventBacklogProgressBytes is how much the consumer must take before the stall
// clock is considered answered: one whole budget's worth.
//
// The unit is the point. Queue depth cannot express progress — a saturating
// producer keeps the queue non-empty no matter how fast the consumer runs, so an
// emptiness test refuses a carrier that is turning over megabytes a second and
// keeping up fine. Bytes separate the two cases cleanly: a consumer that hands
// back one small event every few seconds accumulates a rounding error against
// the budget and is correctly refused, while a consumer moving a budget's worth
// repeatedly is making exactly the progress this deadline was asking for, and
// resets the clock every time it does.
func (b *eventBacklog) progressBytes() int { return b.budget }

// releaseLatch closes the current latch and installs a fresh one, waking every
// waiter exactly once. b.mu must be held.
func releaseLatch(latch *chan struct{}) {
	close(*latch)
	*latch = make(chan struct{})
}

func eventBacklogCost(event ControllerEvent) int {
	return eventBacklogOverheadBytes + len(event.FrameBytes) + len(event.Data) + len(event.Snapshot.Screen)
}

// push queues one event, STALLING while the consumer is at the budget, and
// fails closed only when the consumer produced no progress for the whole stall
// deadline.
//
// Stalling here stalls the socket reader, which stalls the shim's output pump
// behind a socket nobody is draining. That is the intended chain: back-pressure
// reaches the producer, and the shim's ring — the one component designed to
// evict and declare an explicit Gap — absorbs the overflow. Dropping the
// connection instead put an oversized or ill-timed frame in a position to sever
// a healthy carrier.
func (b *eventBacklog) push(event ControllerEvent) error {
	cost := eventBacklogCost(event)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return io.EOF
		}
		// A single event larger than the whole budget is admitted once the
		// backlog has drained, exactly as the shim's ring retains one oversized
		// frame: refusing it would strand a session on one big redraw.
		if len(b.queue) == 0 || b.bytes+cost <= b.budget {
			b.queue = append(b.queue, event)
			b.bytes += cost
			releaseLatch(&b.arrived)
			b.mu.Unlock()
			return nil
		}
		// The deadline lives on the BACKLOG, not on this call. A per-push timer
		// measures one caller's patience, which a dribbling consumer resets for
		// free: hand back one event every few seconds and every push returns
		// before its own clock runs out. The reader is then parked in push
		// essentially forever, heartbeat receipts only trickle through, and the
		// fail-closed verdict this deadline exists to reach is never reached.
		// stalledSince is cleared in exactly one place — pop, once the consumer
		// has taken a budget's worth of BYTES (or emptied the queue outright) —
		// so what is bounded is the consumer making progress, not any single
		// hand-off, and not the queue happening to reach zero.
		if b.stalledSince.IsZero() {
			b.stalledSince, b.drainedSinceStall = time.Now(), 0
		}
		remaining := b.stall - time.Since(b.stalledSince)
		stalled, drained := b.stalledSince, b.drainedSinceStall
		room := b.drained
		b.mu.Unlock()
		if remaining <= 0 {
			switch outcome, held := b.holdForDurableAckAmbiguity(time.Now()); outcome {
			case ambiguityHoldGranted:
				continue
			case ambiguityHoldBoundReached:
				return fmt.Errorf("%w after %s: the consumer took %d bytes, short of the %d it owed",
					ErrDurableAckAmbiguityBound, held.Round(time.Millisecond), drained, b.progressBytes())
			case ambiguityHoldNotAmbiguous:
			}
			// Reported only while the hold is GRANTED. The crossing that gives
			// up is announced by the drop itself, and a "holding the carrier"
			// line immediately before "dropped the carrier" reads as two
			// contradictory decisions rather than one.
			if state, held := b.holdForFlowControl(time.Now()); held {
				b.notify(state)
				continue
			}
			return fmt.Errorf("%w of %d bytes: the consumer took %d bytes in %s, short of the %d it owed",
				ErrEventBacklogExceeded, b.budget, drained,
				time.Since(stalled).Round(time.Millisecond), b.progressBytes())
		}
		if timer == nil {
			timer = time.NewTimer(remaining)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(remaining)
		}
		select {
		case <-room:
		case <-b.abort:
			return io.EOF
		case <-timer.C:
			switch outcome, held := b.holdForDurableAckAmbiguity(time.Now()); outcome {
			case ambiguityHoldGranted:
				continue
			case ambiguityHoldBoundReached:
				return fmt.Errorf("%w after %s: the consumer made no budget's worth of progress",
					ErrDurableAckAmbiguityBound, held.Round(time.Millisecond))
			case ambiguityHoldNotAmbiguous:
			}
			if state, keep := b.holdForFlowControl(time.Now()); keep {
				b.notify(state)
				continue
			}
			return fmt.Errorf("%w of %d bytes: the consumer made no budget's worth of progress in %s",
				ErrEventBacklogExceeded, b.budget, b.dropBound)
		}
	}
}

// pop blocks until an event is available or the backlog is closed and drained.
func (b *eventBacklog) pop() (ControllerEvent, bool) {
	for {
		recovered := false
		b.mu.Lock()
		if len(b.queue) > 0 {
			event := b.queue[0]
			b.queue[0] = ControllerEvent{} // let the frame bytes go
			b.queue = b.queue[1:]
			b.bytes -= eventBacklogCost(event)
			if !b.stalledSince.IsZero() {
				// Progress is measured in bytes taken, plus the degenerate case
				// where the consumer has drained everything there was. Anything
				// less than a budget's worth still standing after a full deadline
				// is the dribble this bound exists to catch.
				b.drainedSinceStall += eventBacklogCost(event)
				if len(b.queue) == 0 || b.drainedSinceStall >= b.progressBytes() {
					// The ambiguity and degradation anchors clear with the stall
					// they belong to. A consumer that made a budget's worth of
					// progress is not the one either bound was ever measuring,
					// and carrying an anchor into an unrelated later stall would
					// spend a bound that had already been answered.
					b.stalledSince, b.drainedSinceStall = time.Time{}, 0
					b.ambiguousSince = time.Time{}
					b.degradedSince = time.Time{}
					if b.reported {
						b.reported = false
						recovered = true
					}
				}
			}
			releaseLatch(&b.drained)
			state := b.flowStateLocked()
			b.mu.Unlock()
			if recovered {
				b.notify(state)
			}
			return event, true
		}
		if b.closed {
			b.mu.Unlock()
			return ControllerEvent{}, false
		}
		next := b.arrived
		b.mu.Unlock()
		<-next
	}
}

func (b *eventBacklog) close() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		releaseLatch(&b.arrived)
		releaseLatch(&b.drained)
	}
	b.mu.Unlock()
}

// queuedBytes reports the current backlog depth. Diagnostics and tests only.
func (b *eventBacklog) queuedBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
}

func decodeHostFrameEvent(body []byte) (ControllerEvent, error) {
	hostFrame, err := shimwire.DecodeHostFrame(body)
	if err != nil {
		return ControllerEvent{}, err
	}
	frame, err := attachwire.DecodeFrame(hostFrame.FrameBytes)
	if err != nil {
		return ControllerEvent{}, err
	}
	event := ControllerEvent{
		Kind: EventHostFrame, RequestID: hostFrame.RequestID,
		FrameType: frame.Type, Seq: frame.Seq, RelTime: frame.RelTime,
		FrameBytes: append([]byte(nil), hostFrame.FrameBytes...),
	}
	switch frame.Type {
	case attachwire.TypeOutput:
		event.Data = append([]byte(nil), attachwire.DecodeOutput(frame.Payload).Data...)
	case attachwire.TypeSnapshot:
		envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
		if err != nil {
			return ControllerEvent{}, err
		}
		event.Snapshot = shimwire.SnapshotMsg{AtSeq: envelope.AtSeq, Screen: append([]byte(nil), envelope.Snap...)}
	case attachwire.TypeExit:
		exit, err := attachwire.DecodeExit(frame.Payload)
		if err != nil {
			return ControllerEvent{}, err
		}
		event.Exit = shimwire.ExitMsg{Seq: frame.Seq, ExitCode: exit.ExitCode, Signal: exit.Signal}
	}
	return event, nil
}

func (c *Controller) acceptSnapshotResult(
	body []byte,
	pending *ControllerEvent,
) (ControllerEvent, bool, *snapshotCompletion, error) {
	result, err := shimwire.DecodeSnapshotResult(body)
	if err != nil {
		return ControllerEvent{}, false, nil, err
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	call := c.snapshotCalls[result.RequestID]
	if call == nil || call.request.Generation != result.Generation || call.request.Mode != result.Mode {
		return ControllerEvent{}, false, nil, fmt.Errorf("sessionshim: %w: result id=%d generation=%d mode=%s", shimwire.ErrSnapshotMismatch, result.RequestID, result.Generation, result.Mode)
	}
	if call.result != nil {
		if pending != nil {
			return ControllerEvent{}, false, nil, shimwire.ErrDuplicateHostFrame
		}
		if !snapshotResultsEqual(*call.result, result) {
			return ControllerEvent{}, false, nil, fmt.Errorf("sessionshim: %w: changed result for request id %d", shimwire.ErrSnapshotMismatch, result.RequestID)
		}
		return ControllerEvent{}, false, nil, nil
	}
	if err := validateSnapshotResult(result, c.observedExit(), c.selected, pending); err != nil {
		return ControllerEvent{}, false, nil, err
	}
	stored := cloneSnapshotResult(result)
	var callErr error
	if result.Code != "" {
		callErr = fmt.Errorf("sessionshim: %w: %s", shimwire.ErrSnapshotRefused, result.Code)
	}
	if result.Mode == shimwire.SnapshotEmit && result.InStream && !call.streamDelivered {
		call.streamDelivered = true
		if c.selected >= shimwire.V3 {
			if pending == nil {
				return ControllerEvent{}, false, nil, shimwire.ErrSnapshotMismatch
			}
			return *pending, true, &snapshotCompletion{call: call, result: stored, err: callErr}, nil
		}
		call.result = &stored
		call.err = callErr
		close(call.done)
		return ControllerEvent{Kind: EventSnapshotFrame, Seq: result.AtSeq + 1, FrameBytes: append([]byte(nil), result.Bytes...)}, true, nil, nil
	}
	call.result = &stored
	call.err = callErr
	close(call.done)
	return ControllerEvent{}, false, nil, nil
}

func (c *Controller) completeSnapshotCall(completion *snapshotCompletion, publishErr error) {
	if completion == nil {
		return
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if completion.call.result != nil || completion.call.err != nil {
		return
	}
	if publishErr != nil {
		completion.call.err = publishErr
		close(completion.call.done)
		return
	}
	stored := cloneSnapshotResult(completion.result)
	completion.call.result = &stored
	completion.call.err = completion.err
	close(completion.call.done)
}

func validateSnapshotResult(result shimwire.SnapshotResult, observedExit *shimwire.ExitMsg, selected uint32, pending *ControllerEvent) error {
	if result.Code != "" {
		if pending != nil {
			return shimwire.ErrDuplicateHostFrame
		}
		return nil
	}
	if result.Mode == shimwire.SnapshotInspect {
		if pending != nil {
			return shimwire.ErrDuplicateHostFrame
		}
		if result.InStream || len(result.Bytes) == 0 {
			return fmt.Errorf("sessionshim: %w: invalid inspect disposition", shimwire.ErrSnapshotMismatch)
		}
		if _, err := attachwire.DecodeScreen(result.Bytes); err != nil {
			return fmt.Errorf("sessionshim: %w: inspect screen: %v", shimwire.ErrSnapshotMismatch, err)
		}
		return nil
	}
	if selected >= shimwire.V3 && result.InStream {
		if pending == nil || pending.Kind != EventHostFrame || pending.FrameType != attachwire.TypeSnapshot ||
			pending.RequestID != result.RequestID || pending.Seq != result.AtSeq+1 || len(result.Bytes) != 0 {
			return fmt.Errorf("sessionshim: %w: v3 live emit pair differs", shimwire.ErrSnapshotMismatch)
		}
		frame, err := attachwire.DecodeFrame(pending.FrameBytes)
		if err != nil {
			return fmt.Errorf("sessionshim: %w: v3 live Snapshot frame", shimwire.ErrSnapshotMismatch)
		}
		envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
		if err != nil || envelope.AtSeq != result.AtSeq {
			return fmt.Errorf("sessionshim: %w: v3 live Snapshot atSeq differs", shimwire.ErrSnapshotMismatch)
		}
		if envelope.SnapFormat != attachwire.SnapFormatScreen {
			return fmt.Errorf("sessionshim: %w: v3 live Snapshot format differs", shimwire.ErrSnapshotMismatch)
		}
		if _, err := attachwire.DecodeScreen(envelope.Snap); err != nil {
			return fmt.Errorf("sessionshim: %w: v3 live Snapshot screen", shimwire.ErrSnapshotMismatch)
		}
		if observedExit != nil {
			return fmt.Errorf("sessionshim: %w: live emit arrived after Exit", shimwire.ErrSnapshotMismatch)
		}
		return nil
	}
	if pending != nil {
		return shimwire.ErrDuplicateHostFrame
	}
	frame, err := attachwire.DecodeFrame(result.Bytes)
	if err != nil || frame.Type != attachwire.TypeSnapshot {
		return fmt.Errorf("sessionshim: %w: emit frame is not an encoded Snapshot", shimwire.ErrSnapshotMismatch)
	}
	env, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil || env.AtSeq != result.AtSeq {
		return fmt.Errorf("sessionshim: %w: emit atSeq differs from frame", shimwire.ErrSnapshotMismatch)
	}
	if env.SnapFormat != attachwire.SnapFormatScreen {
		return fmt.Errorf("sessionshim: %w: emit snapshot format %d is not screen", shimwire.ErrSnapshotMismatch, env.SnapFormat)
	}
	if _, err := attachwire.DecodeScreen(env.Snap); err != nil {
		return fmt.Errorf("sessionshim: %w: emit screen: %v", shimwire.ErrSnapshotMismatch, err)
	}
	if result.InStream {
		if observedExit != nil {
			return fmt.Errorf("sessionshim: %w: live emit arrived after Exit", shimwire.ErrSnapshotMismatch)
		}
		if frame.Seq == 0 || frame.Seq != result.AtSeq+1 {
			return fmt.Errorf("sessionshim: %w: live emit sequence disposition invalid", shimwire.ErrSnapshotMismatch)
		}
	} else {
		if observedExit == nil {
			return fmt.Errorf("sessionshim: %w: direct emit arrived before Exit", shimwire.ErrSnapshotMismatch)
		}
		if frame.Seq != attachwire.PostExitSnapshotSeq || frame.RelTime != 0 {
			return fmt.Errorf("sessionshim: %w: direct emit is not post-Exit sequence/rel-time zero", shimwire.ErrSnapshotMismatch)
		}
		if result.AtSeq != observedExit.Seq || env.AtSeq != observedExit.Seq {
			return fmt.Errorf("sessionshim: %w: direct emit atSeq does not equal observed Exit seq", shimwire.ErrSnapshotMismatch)
		}
	}
	return nil
}

func (c *Controller) observeExit(exit shimwire.ExitMsg) error {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	if c.exit == nil {
		observed := exit
		c.exit = &observed
		return nil
	}
	if *c.exit != exit {
		return fmt.Errorf("sessionshim: %w: changed immutable Exit observation", shimwire.ErrSnapshotMismatch)
	}
	return nil
}

func (c *Controller) observedExit() *shimwire.ExitMsg {
	c.exitMu.RLock()
	defer c.exitMu.RUnlock()
	if c.exit == nil {
		return nil
	}
	observed := *c.exit
	return &observed
}

func snapshotResultsEqual(a, b shimwire.SnapshotResult) bool {
	return a.RequestID == b.RequestID && a.Generation == b.Generation && a.Mode == b.Mode &&
		a.Code == b.Code && a.AtSeq == b.AtSeq && a.InStream == b.InStream && bytes.Equal(a.Bytes, b.Bytes)
}

func (c *Controller) failSnapshotCalls(err error) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	for _, call := range c.snapshotCalls {
		if call.result == nil && call.err == nil {
			call.err = err
			close(call.done)
		}
	}
}

func decodeEvent(msg shimwire.Message) (ControllerEvent, bool) {
	switch msg.Type {
	case shimwire.TypeOutput:
		seq, rel, data, err := shimwire.DecodeOutput(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		// DecodeOutput aliases the reader's per-message buffer; the event escapes
		// this goroutine, so the bytes are copied.
		out := make([]byte, len(data))
		copy(out, data)
		return ControllerEvent{Kind: EventOutput, Seq: seq, RelTime: rel, Data: out}, true
	case shimwire.TypeGap:
		g, err := shimwire.DecodeGap(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventGap, Gap: g}, true
	case shimwire.TypeSnapshot:
		s, err := shimwire.DecodeSnapshot(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventSnapshot, Snapshot: s}, true
	case shimwire.TypeExit:
		e, err := shimwire.DecodeExit(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventExit, Exit: e}, true
	case shimwire.TypeError:
		e, err := shimwire.DecodeError(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventError, Err: e}, true
	default:
		return ControllerEvent{}, false
	}
}
