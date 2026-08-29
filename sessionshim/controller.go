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

	snapshotMu     sync.Mutex
	nextSnapshotID uint64
	snapshotCalls  map[uint64]*snapshotCall

	heartbeatCallMu sync.Mutex
	heartbeatMu     sync.Mutex
	heartbeatCall   *heartbeatCall
}

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

// EventBacklogBudget bounds, in payload bytes, how far behind the socket reader
// a consumer may fall before the controller fails closed.
//
// It is deliberately EQUAL to the shim's own output ring budget
// (ptyhost.DefaultRingBytes), and that equality is the point. Both numbers
// answer the same question — how much host output may be in flight before this
// system admits it has lost some — and they must answer it in the same currency
// at the same magnitude. When the daemon-side bound was a frame count (192) it
// was orders of magnitude tighter than the shim's 8 MiB, so the controller
// collapsed long before the ring, which is the component actually DESIGNED to
// evict and declare a Gap (ADR-2026-08-17 §D5). A burst the shim absorbs by
// design must never be the thing that kills the connection carrying it.
//
// The reader still may not block on a consumer: it is the only goroutine that
// can receive a durable heartbeat receipt, so parking it behind a full backlog
// would deadlock a consumer waiting on that receipt. Past this budget the
// controller therefore still fails closed — the shim keeps the harness and the
// session is released to quarantine. What changed is WHERE that line sits.
const EventBacklogBudget = ptyhost.DefaultRingBytes

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
		c.backlog = newEventBacklog(opts.eventBacklogBudget())
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
		if prepared.ControllerGeneration != 0 {
			if opts.ProposedGeneration != 0 || opts.NextGeneration != nil {
				return fmt.Errorf("%w: prepared and static controller generations are both configured", ErrAdoptionPreparation)
			}
			proposed = prepared.ControllerGeneration
		}
		extensions = prepared.Extensions
		if prepared.ResumeFrom != nil {
			preparedResumeProvided = true
			staticResumeConfigured := opts.ResumeExternallyConfigured ||
				(opts.LocalResumeFrom == 0 && opts.ResumeFrom != 0)
			if staticResumeConfigured {
				return fmt.Errorf("%w: static and proof-resolved resume cursors are both configured", ErrAdoptionPreparation)
			}
			resolved := *prepared.ResumeFrom
			if resolved < localResumeFrom {
				return fmt.Errorf("%w: prepared resume %d regresses local floor %d",
					ErrAdoptionPreparation, resolved, localResumeFrom)
			}
			if hello.LastSeq == ^uint64(0) || resolved > hello.LastSeq+1 {
				return fmt.Errorf("%w: prepared resume %d is ahead of Hello LastSeq %d",
					ErrAdoptionPreparation, resolved, hello.LastSeq)
			}
			opts.ResumeFrom = resolved
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
	c.heartbeatMu.Unlock()
	if err := c.w.WriteVersion(c.selected, shimwire.TypeHeartbeat, body); err != nil {
		c.clearHeartbeatCall(call)
		return err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-call.done:
		return result.err
	case <-timer.C:
		c.clearHeartbeatCall(call)
		c.closeStream("durable heartbeat receipt timed out", nil)
		return errors.New("sessionshim: selected-v3 heartbeat persistence receipt timed out")
	case <-c.done:
		c.clearHeartbeatCall(call)
		return io.EOF
	}
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

// closeStream drops the connection after a fail-closed stream decision and says
// why it did.
//
// The reason is not decoration. A silent drop here is indistinguishable, from
// every later caller's side, from a peer that went away: input, resize, and the
// durable heartbeat all come back with "use of closed network connection" and
// nothing anywhere names the decision that caused it.
func (c *Controller) closeStream(reason string, cause error) {
	c.log().Warn("sessionshim: controller dropped its shim connection",
		"session", c.id.String(), "reason", reason,
		"selected", c.selected, "error", cause)
	_ = c.Close()
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
	call := c.heartbeatCall
	if call == nil {
		c.heartbeatMu.Unlock()
		return errors.New("sessionshim: unsolicited selected-v3 heartbeat receipt")
	}
	c.heartbeatCall = nil
	c.heartbeatMu.Unlock()
	if receipt.Generation != call.expected.Generation || receipt.AckedSeq != call.expected.AckedSeq || !receipt.Phase.Known() {
		err := errors.New("sessionshim: selected-v3 heartbeat receipt changed generation, cursor, or phase")
		call.done <- heartbeatResult{err: err}
		return err
	}
	call.done <- heartbeatResult{}
	return nil
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

// ErrEventBacklogExceeded reports a consumer that fell further behind than the
// shim's own in-flight budget. It is a fail-closed decision, not a transport
// error: the connection is dropped, the shim keeps the harness.
var ErrEventBacklogExceeded = errors.New("sessionshim: event backlog exceeded the in-flight budget")

// ErrShimExited reports that a shim refused a request because it has already
// published its terminal proof.
//
// It is a sentinel rather than a message because the refusal is ACTIONABLE: the
// tombstone exists by the time the shim answers this way, so the right response
// is to go and consume it, not to treat the exchange as a broken socket and
// leave the lineage in reconciliation quarantine until some later surface
// happens to look.
var ErrShimExited = errors.New("sessionshim: shim refused: terminal proof is already published")

// eventBacklog is the bounded hand-off between the socket reader and the
// consumer, accounted in payload bytes rather than frames.
//
// Frames are not uniform. A frame count cannot bound memory (one frame may be
// megabytes) and cannot express "as much as the shim itself will hold", which is
// the only bound with a principled source. Bytes do both.
type eventBacklog struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []ControllerEvent
	bytes  int
	budget int
	closed bool
}

func newEventBacklog(budget int) *eventBacklog {
	if budget <= 0 {
		budget = EventBacklogBudget
	}
	b := &eventBacklog{budget: budget}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func eventBacklogCost(event ControllerEvent) int {
	return eventBacklogOverheadBytes + len(event.FrameBytes) + len(event.Data) + len(event.Snapshot.Screen)
}

// push queues one event, or fails closed when the consumer has fallen further
// behind than the budget allows. It never blocks: the socket reader is the only
// goroutine that can deliver a durable heartbeat receipt, and parking it here
// would deadlock a consumer waiting on one.
func (b *eventBacklog) push(event ControllerEvent) error {
	cost := eventBacklogCost(event)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return io.EOF
	}
	// A single event larger than the whole budget is still accepted when the
	// backlog is otherwise empty, exactly as the shim's ring retains one
	// oversized frame: refusing it would strand a session on one big redraw.
	if len(b.queue) > 0 && b.bytes+cost > b.budget {
		return fmt.Errorf("%w of %d bytes", ErrEventBacklogExceeded, b.budget)
	}
	b.queue = append(b.queue, event)
	b.bytes += cost
	b.cond.Signal()
	return nil
}

// pop blocks until an event is available or the backlog is closed and drained.
func (b *eventBacklog) pop() (ControllerEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.queue) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.queue) == 0 {
		return ControllerEvent{}, false
	}
	event := b.queue[0]
	b.queue = b.queue[1:]
	b.bytes -= eventBacklogCost(event)
	return event, true
}

func (b *eventBacklog) close() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
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
