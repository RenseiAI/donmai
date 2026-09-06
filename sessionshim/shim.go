package sessionshim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// Options configure a Shim.
type Options struct {
	// Identity is the session's sole lifecycle identity. Required.
	Identity Identity

	// Registry is where the discovery record and terminal tombstone are
	// published. Required.
	Registry *Registry

	// Spec is the PTY spec for the harness this shim will own.
	Spec ptyhost.Spec

	// WorkareaPath is the workarea the harness runs against. It is recorded and
	// verified at adoption, so a shim cannot be adopted into a workarea other
	// than the one it is actually running in.
	WorkareaPath string
	// WorkareaRoot is the optional session-owned lifecycle root. Empty preserves
	// the released discovery-record bytes for legacy flat workareas.
	WorkareaRoot string

	// Orphan bounds the controller-loss rule. A zero policy uses
	// DefaultOrphanPolicy.
	Orphan OrphanPolicy

	// ProcessEpoch is the monotonic per-session value for this shim incarnation.
	ProcessEpoch uint64

	// ProtocolMin/ProtocolMax optionally narrow this shim's supported range.
	// Zero/zero uses the build range; immutable overlap fixtures use max 2.
	ProtocolMin uint32
	ProtocolMax uint32

	Logger *slog.Logger

	// Now lets tests drive deterministic timestamps.
	Now func() time.Time

	// onTerminalCourtesy is the unexported test seam described on Shim's field
	// of the same name. It is set HERE rather than on the returned Shim because
	// Start launches watchHarness before it returns: a harness that exits on
	// its own reads the field from that goroutine, so assigning it afterwards
	// is an unsynchronized cross-goroutine write with no happens-before edge —
	// a data race whether or not a given run happens to lose it.
	onTerminalCourtesy func()
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (o Options) now() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

// Shim is the durable owner of one interactive session.
//
// It owns the harness process group, the PTY master, the VT/snapshot state, the
// output sequence, the bounded replay ring, and the final exit observation.
// Whichever daemon happens to be running is only its CONTROLLER, attached over a
// socket and replaceable at any moment. That inversion is the entire point: a
// daemon upgrade closes a socket, not a terminal.
type Shim struct {
	id       Identity
	registry *Registry
	sess     *ptyhost.Session
	ln       *net.UnixListener
	logger   *slog.Logger
	now      func() time.Time
	orphan   OrphanPolicy

	shimID       string
	epoch        uint64
	self         ProcessIdentity
	harness      ProcessIdentity
	workarea     string
	workareaRoot string
	protocolMin  uint32
	protocolMax  uint32

	socketPath string
	socketDev  uint64
	socketIno  uint64

	// recordMu serializes every write to this session's registry entry against
	// every other one. s.mu alone is not enough: it guards the fields, while the
	// hazard is two DISK writes interleaving — a controller-loss republish and
	// the terminal withdrawal racing each other can otherwise resurrect a
	// liveness record for a harness that is provably gone.
	recordMu sync.Mutex
	// handshakeMu serializes Hello-time output barriers. Two same-UID controller
	// dials cannot each freeze a different LastSeq and then race Welcome commits.
	handshakeMu sync.Mutex
	// ackedSeq is protected by recordMu because advancing it and publishing its
	// sidecar are one durability transition.
	ackedSeq          uint64
	terminalPublished bool
	// terminalSeq is the host sequence the terminal proof froze — the Exit
	// frame's own sequence, the same value the tombstone carries as LastSeq. It
	// is the ceiling every post-terminal acknowledgement is measured against.
	terminalSeq uint64
	ackNotify   chan struct{}

	mu    sync.Mutex
	gen   shimwire.Generation
	phase shimwire.Phase
	// ctrl is the CURRENT controller connection. Exactly one may hold authority;
	// adopting a new one closes this.
	ctrl        *controllerConn
	orphanTimer *time.Timer
	tombstoned  bool

	closeOnce  sync.Once
	stopOnce   sync.Once
	done       chan struct{}
	acceptDone chan struct{}
	// onTerminalCourtesy runs at the boundary between the durable terminal
	// proof and the best-effort delivery that follows it. It is nil in every
	// production build; it exists so a test can observe WHICH side of that
	// boundary the tombstone is written on, which no timing assertion can do
	// reliably when a controller happens to drain quickly.
	//
	// It is set ONCE, from Options, before Start launches watchHarness — the
	// goroutine that reads it. Assigning it on a returned Shim would be an
	// unsynchronized cross-goroutine write.
	onTerminalCourtesy func()
}

// controllerConn is one attached controller.
type controllerConn struct {
	conn           *net.UnixConn
	w              *shimwire.Writer
	sub            agent.InteractiveSubscription
	selected       uint32
	snapshotLedger map[uint64]*snapshotLedgerEntry
	emissionMu     sync.Mutex
	emissionBySeq  map[uint64]*snapshotLedgerEntry
	pumpDone       chan struct{}
	barrierMu      sync.Mutex
	outputBarrier  *ptyhost.OutputBarrier
	barrierTimer   *time.Timer
	barrierState   uint8
	closeOnce      sync.Once
}

const (
	outputBarrierNone uint8 = iota
	outputBarrierPending
	outputBarrierConsumed
	outputBarrierFailed
)

const adoptionOutputBarrierTimeout = 30 * time.Second

const snapshotRetryLedgerLimit = 1024

type snapshotLedgerEntry struct {
	request   shimwire.SnapshotRequest
	result    shimwire.SnapshotResult
	delivered chan struct{}
	writeErr  error
}

func (c *controllerConn) close() {
	c.closeOnce.Do(func() {
		c.failOutputBarrier()
		if c.sub != nil {
			_ = c.sub.Close()
		}
		_ = c.conn.Close()
	})
}

func (c *controllerConn) installOutputBarrier(barrier *ptyhost.OutputBarrier) {
	if barrier == nil {
		return
	}
	c.barrierMu.Lock()
	c.outputBarrier = barrier
	c.barrierState = outputBarrierPending
	c.barrierTimer = time.AfterFunc(adoptionOutputBarrierTimeout, func() {
		c.barrierMu.Lock()
		if c.barrierState != outputBarrierPending || c.outputBarrier == nil {
			c.barrierMu.Unlock()
			return
		}
		pending := c.outputBarrier
		c.outputBarrier = nil
		c.barrierState = outputBarrierFailed
		c.barrierTimer = nil
		c.barrierMu.Unlock()
		pending.Release()
		_ = c.conn.Close()
	})
	c.barrierMu.Unlock()
}

func (c *controllerConn) failOutputBarrier() {
	c.barrierMu.Lock()
	if c.barrierTimer != nil {
		c.barrierTimer.Stop()
		c.barrierTimer = nil
	}
	pending := c.outputBarrier
	c.outputBarrier = nil
	if c.barrierState == outputBarrierPending {
		c.barrierState = outputBarrierFailed
	}
	c.barrierMu.Unlock()
	if pending != nil {
		pending.Release()
	}
}

func (c *controllerConn) releaseOutputBarrierAfterDurableAdvance() {
	c.barrierMu.Lock()
	if c.barrierTimer != nil {
		c.barrierTimer.Stop()
		c.barrierTimer = nil
	}
	pending := c.outputBarrier
	c.outputBarrier = nil
	if c.barrierState == outputBarrierPending {
		c.barrierState = outputBarrierConsumed
	}
	c.barrierMu.Unlock()
	if pending != nil {
		pending.Release()
	}
}

func (c *controllerConn) emitSnapshot(sess *ptyhost.Session) (attachwire.Frame, bool, error) {
	c.barrierMu.Lock()
	switch c.barrierState {
	case outputBarrierPending:
		barrier := c.outputBarrier
		c.outputBarrier = nil
		c.barrierState = outputBarrierConsumed
		if c.barrierTimer != nil {
			c.barrierTimer.Stop()
			c.barrierTimer = nil
		}
		c.barrierMu.Unlock()
		return barrier.EmitSnapshot()
	case outputBarrierFailed:
		c.barrierMu.Unlock()
		return attachwire.Frame{}, false, errors.New("sessionshim: adoption output barrier expired")
	default:
		c.barrierMu.Unlock()
		return sess.EmitSnapshot()
	}
}

// ErrShimUnsupported reports a platform on which shim adoption cannot be safely
// enabled. §D3: without a trustworthy peer-credential primitive, adoption stays
// off rather than running unauthenticated.
var ErrShimUnsupported = errors.New("sessionshim: session shim adoption is unsupported on this platform")

// Start spawns the harness under a PTY, begins listening on the session's local
// adoption socket, and publishes the discovery record.
//
// The ORDER here is deliberate and is the same order §D6 implies: the socket
// exists before the record names it, so a daemon that reads the record can
// always dial it. Publishing the record first would create a window in which
// discovery points at nothing, which classifies as socket_unreachable and
// quarantines a perfectly healthy session.
func Start(opts Options) (*Shim, error) {
	if !peerCredSupported() {
		return nil, ErrShimUnsupported
	}
	if err := opts.Identity.Validate(); err != nil {
		return nil, err
	}
	if opts.Registry == nil {
		return nil, errors.New("sessionshim: Start requires a Registry")
	}
	protocolMin, protocolMax := opts.ProtocolMin, opts.ProtocolMax
	if protocolMin == 0 && protocolMax == 0 {
		protocolMin, protocolMax = shimwire.ProtocolMin, shimwire.ProtocolMax
	}
	if protocolMin == 0 || protocolMax < protocolMin || protocolMax > shimwire.ProtocolMax {
		return nil, fmt.Errorf("sessionshim: invalid shim protocol range [%d,%d]", protocolMin, protocolMax)
	}
	orphan := opts.Orphan
	if orphan.Deadline == 0 {
		orphan = DefaultOrphanPolicy()
	}
	if err := orphan.Validate(); err != nil {
		return nil, err
	}
	self, err := Self()
	if err != nil {
		return nil, err
	}
	shimID, err := newShimID()
	if err != nil {
		return nil, err
	}

	socketPath := opts.Registry.SocketPath(opts.Identity)
	// A leftover socket from a dead shim would make Listen fail with EADDRINUSE.
	// Removing it is safe: the path is derived from the identity inside a 0700
	// directory this process owns, and a LIVE shim for the same identity is the
	// duplicate-identity case the classifier quarantines rather than something
	// this path should be racing.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("sessionshim: clear stale socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("sessionshim: listen on adoption socket: %w", err)
	}
	// The socket carries no secret, but its 0600 mode keeps the daemon user's
	// local trust boundary explicit rather than relying on the parent
	// directory's mode alone.
	if err := os.Chmod(socketPath, RecordFileMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("sessionshim: tighten adoption socket: %w", err)
	}
	dev, ino, err := statSocket(socketPath)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	sess, err := ptyhost.Spawn(opts.Spec)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("sessionshim: spawn harness: %w", err)
	}

	harnessPID := sess.PID()
	harnessStart, startErr := processStartTime(harnessPID)
	if startErr != nil {
		// The harness is running but we cannot pin its identity. Continuing would
		// leave a tombstone that cannot distinguish "reaped" from "pid reused",
		// so fail closed and tear the child down now.
		stopCtx, cancel := context.WithTimeout(context.Background(), orphan.TerminationGrace+2*time.Second)
		_ = sess.Stop(stopCtx)
		cancel()
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("sessionshim: pin harness process identity: %w", startErr)
	}

	s := &Shim{
		id:           opts.Identity,
		registry:     opts.Registry,
		sess:         sess,
		ln:           ln,
		logger:       opts.logger(),
		now:          opts.now(),
		orphan:       orphan,
		shimID:       shimID,
		epoch:        opts.ProcessEpoch,
		self:         self,
		harness:      ProcessIdentity{PID: harnessPID, StartedAt: harnessStart},
		workarea:     opts.WorkareaPath,
		workareaRoot: opts.WorkareaRoot,
		protocolMin:  protocolMin,
		protocolMax:  protocolMax,
		socketPath:   socketPath,
		socketDev:    dev,
		socketIno:    ino,
		phase:        shimwire.PhaseRunning,
		done:         make(chan struct{}),
		acceptDone:   make(chan struct{}),
		ackNotify:    make(chan struct{}),

		onTerminalCourtesy: opts.onTerminalCourtesy,
	}

	if err := s.publishRecord(); err != nil {
		_ = s.Close()
		return nil, err
	}

	// No controller is attached yet, so the orphan clock starts immediately. A
	// shim whose creating daemon dies before it ever adopts must still be bounded.
	s.armOrphan()

	go s.acceptLoop()
	go s.watchHarness()
	return s, nil
}

// Identity returns the shim's lifecycle identity.
func (s *Shim) Identity() Identity { return s.id }

// ShimID returns the shim's correlation id.
func (s *Shim) ShimID() string { return s.shimID }

// SocketPath returns the local adoption socket path.
func (s *Shim) SocketPath() string { return s.socketPath }

// HarnessIdentity returns the owned harness's process identity. This is the
// value a restart must leave UNCHANGED — it is the concrete meaning of "the
// session survived".
func (s *Shim) HarnessIdentity() ProcessIdentity { return s.harness }

// Generation returns the controller generation currently in force.
func (s *Shim) Generation() shimwire.Generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Phase returns the shim's current lifecycle phase.
func (s *Shim) Phase() shimwire.Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Session exposes the owned PTY session. It exists for the in-process
// composition path (the shim's own runner side) and for tests; a CONTROLLER
// never gets this — it gets a shimwire connection, which is what keeps the
// daemon from holding a second direct reference to PTY state (§D1).
func (s *Shim) Session() *ptyhost.Session { return s.sess }

// Done is closed once the shim has fully stopped: harness reaped, terminal
// observation persisted, listener closed.
func (s *Shim) Done() <-chan struct{} { return s.done }

// Close stops serving and releases the listener WITHOUT terminating the harness.
//
// The asymmetry is intentional. Close is the "this shim process is going away"
// path; killing the harness is the bounded-orphan path and belongs to the
// deadline, not to teardown. Conflating them would make an ordinary shutdown
// destructive in exactly the way this design exists to prevent.
func (s *Shim) Close() error {
	s.closeOnce.Do(func() {
		_ = s.ln.Close()
		s.mu.Lock()
		ctrl := s.ctrl
		s.ctrl = nil
		if s.orphanTimer != nil {
			s.orphanTimer.Stop()
			s.orphanTimer = nil
		}
		s.mu.Unlock()
		if ctrl != nil {
			ctrl.close()
		}
		// Join the accept loop. AcceptUnix returns as soon as the listener above
		// is closed, so this is a handshake rather than a wait — but making it
		// explicit means a test that closes a shim and inspects the registry
		// cannot observe a half-torn-down one.
		<-s.acceptDone
		_ = os.Remove(s.socketPath)
	})
	return nil
}

// Terminate runs the bounded teardown: SIGTERM→grace→SIGKILL on the harness
// process group, drain to EOF, persist the terminal observation, and replace the
// discovery record with a tombstone.
//
// This is what the orphan deadline fires, and what a generation-fenced Stop
// reaches. It is idempotent.
func (s *Shim) Terminate(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() { err = s.terminate(ctx) })
	return err
}

func (s *Shim) terminate(ctx context.Context) error {
	stopCtx, cancel := context.WithTimeout(ctx, s.orphan.TerminationGrace+2*time.Second)
	defer cancel()
	_ = s.sess.Stop(stopCtx)
	<-s.sess.Done()
	return s.finalizeTerminal()
}

// finalizeTerminal persists the immutable terminal observation exactly once.
func (s *Shim) finalizeTerminal() error {
	exit, _ := s.sess.Exit()
	_, lastSeq, _ := s.sess.Snapshot()

	// Proof, not assumption: ask the OS whether the recorded harness incarnation
	// is really gone. A tombstone that claims a reap it did not verify is worse
	// than no tombstone, because §D10 lets a proven tombstone release a claim.
	alive, aliveErr := s.harness.Alive()
	reaped := aliveErr == nil && !alive

	s.mu.Lock()
	if s.tombstoned {
		s.mu.Unlock()
		return nil
	}
	s.tombstoned = true
	s.phase = shimwire.PhaseExited
	ctrl := s.ctrl
	s.mu.Unlock()

	t := Tombstone{
		SchemaVersion:      RecordSchemaVersion,
		OrgID:              s.id.OrgID,
		SessionID:          s.id.SessionID,
		ShimID:             s.shimID,
		ProcessEpoch:       s.epoch,
		HarnessPID:         s.harness.PID,
		HarnessStartedAt:   s.harness.StartedAt,
		ExitCode:           exit.ExitCode,
		Signal:             exit.Signal,
		LastSeq:            uint64(lastSeq),
		GroupReaped:        reaped,
		ObservedAtUnixNano: s.now().UnixNano(),
	}
	// Under recordMu for the same reason publishRecordWithDeadline takes it: this
	// is the write that withdraws the liveness claim, and a republish landing
	// between its two halves would undo it. s.tombstoned is already set above, so
	// any republish that queues behind this lock is refused rather than reordered.
	s.recordMu.Lock()
	err := s.registry.PutTombstone(t)
	if err == nil {
		s.terminalPublished = true
		s.terminalSeq = uint64(lastSeq)
	}
	s.recordMu.Unlock()
	if err != nil {
		return fmt.Errorf("sessionshim: persist tombstone: %w", err)
	}

	if s.onTerminalCourtesy != nil {
		s.onTerminalCourtesy()
	}
	// PROOF FIRST, COURTESY SECOND. Everything below is best-effort delivery to
	// a controller that may already be gone; none of it changes a single field
	// of the observation above, and all of it can block for the full finalize
	// bound. Publishing the tombstone first is what makes the proof independent
	// of controller latency and of this process's own teardown: a host that
	// stops waiting mid-courtesy now leaves a lineage that is provably ended
	// rather than one that is merely unobservable (§D10).
	if ctrl != nil && ctrl.selected < shimwire.V3 {
		// The controller may already be gone, which is exactly the case the
		// tombstone exists for.
		_ = writeTyped(ctrl.w, shimwire.TypeExit, func() ([]byte, error) {
			return shimwire.EncodeExit(shimwire.ExitMsg{Seq: uint64(lastSeq), ExitCode: exit.ExitCode, Signal: exit.Signal})
		})
	}
	if ctrl != nil && ctrl.selected >= shimwire.V3 && ctrl.pumpDone != nil {
		// In v3 the one raw Exit HostFrame is the terminal observation. Session
		// Done closes the subscription after publishing Exit. Give the pump a
		// bounded flush opportunity, then close a stalled controller so the
		// courtesy cannot deadlock behind socket backpressure.
		flushBound := s.finalizeWaitBound()
		timer := time.NewTimer(flushBound)
		select {
		case <-ctrl.pumpDone:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			ctrl.close()
			s.logger.Warn("sessionshim: selected-v3 controller stalled before terminal frame flush",
				"session", s.id.String())
		}
		// Socket write completion is not the acknowledgement boundary: the pump
		// having handed the Exit frame to the kernel says nothing about the
		// controller having read it. Give the controller the same bounded
		// opportunity to acknowledge that exact sequence back.
		//
		// AFTER THE TOMBSTONE THIS IS DELIVERY CONFIRMATION, NOT PERSISTENCE.
		// PutTombstone already removed the live incarnation's durable-ack
		// sidecar, and the heartbeat path deliberately does not write one back
		// — an entry whose discovery record no longer exists is unreachable and
		// uncollectable. The resume cursor an adopting controller reads is the
		// tombstone's own fsync-backed LastSeq, which equals the frozen
		// terminalSeq this waits for. So what is being waited on is the
		// controller's receipt of the terminal frame; the durable half is
		// already on disk before the wait begins.
		s.waitForDurableAck(uint64(lastSeq), flushBound)
	}
	close(s.done)
	return nil
}

// FinalizeBoundFor is FinalizeBound for a policy, for a host that must size its
// own grace before a shim exists.
func FinalizeBoundFor(policy OrphanPolicy) time.Duration {
	return 2 * finalizeWaitBoundFor(policy.TerminationGrace)
}

func finalizeWaitBoundFor(grace time.Duration) time.Duration {
	if grace <= 0 || grace > maxFinalizeWaitBound {
		return maxFinalizeWaitBound
	}
	return grace
}

// finalizeWaitBound is one of the two equal courtesy windows finalizeTerminal
// spends after the tombstone is durable: the selected-v3 pump flush and the
// durable-ack wait.
func (s *Shim) finalizeWaitBound() time.Duration {
	return finalizeWaitBoundFor(s.orphan.TerminationGrace)
}

// FinalizeBound is the longest finalizeTerminal can run after the harness is
// reaped: the two equal courtesy windows above. A host that waits for
// Done() must derive its grace from THIS, never from a second number picked
// beside it — the two silently drifting apart is how a process exits in the
// same instant its own proof was about to be written.
func (s *Shim) FinalizeBound() time.Duration { return 2 * s.finalizeWaitBound() }

// maxFinalizeWaitBound caps each courtesy window regardless of the configured
// termination grace.
const maxFinalizeWaitBound = 5 * time.Second

// watchHarness turns an ordinary harness exit into the terminal observation.
func (s *Shim) watchHarness() {
	<-s.sess.Done()
	// Take stopOnce here too: a harness that exits on its own and a Terminate
	// racing it must produce ONE tombstone, not two.
	s.stopOnce.Do(func() {
		if err := s.finalizeTerminal(); err != nil {
			s.logger.Error("sessionshim: finalize terminal observation", "session", s.id.String(), "error", err)
		}
	})
	_ = s.Close()
}

// ---- orphan rule -----------------------------------------------------------

// armOrphan starts the bounded controller-loss deadline (§D8).
func (s *Shim) armOrphan() {
	deadline := s.now().Add(s.orphan.Deadline)
	s.mu.Lock()
	if s.phase == shimwire.PhaseExited {
		s.mu.Unlock()
		return
	}
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
	}
	s.phase = shimwire.PhaseOrphaned
	s.orphanTimer = time.AfterFunc(s.orphan.Deadline, s.onOrphanDeadline)
	s.mu.Unlock()
	if err := s.publishRecordWithDeadline(deadline); err != nil {
		s.logger.Warn("sessionshim: republish record on orphan", "session", s.id.String(), "error", err)
	}
}

// disarmOrphan cancels the deadline because a controller adopted in time.
func (s *Shim) disarmOrphan() {
	s.mu.Lock()
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}
	if s.phase == shimwire.PhaseOrphaned {
		s.phase = shimwire.PhaseRunning
	}
	s.mu.Unlock()
	if err := s.publishRecord(); err != nil {
		s.logger.Warn("sessionshim: republish record on adoption", "session", s.id.String(), "error", err)
	}
}

// onOrphanDeadline fires when no controller returned in time.
//
// It terminates and reaps, then leaves a tombstone. It does NOT — and cannot —
// authorize a claim release: that decision lives in ReleaseDecision and requires
// this tombstone as evidence, which is the asymmetry §D8 insists on.
func (s *Shim) onOrphanDeadline() {
	s.logger.Warn("sessionshim: orphan deadline reached; reaping harness process group",
		"session", s.id.String(), "shim", s.shimID, "deadline", s.orphan.Deadline)
	ctx, cancel := context.WithTimeout(context.Background(), s.orphan.TerminationGrace+5*time.Second)
	defer cancel()
	if err := s.Terminate(ctx); err != nil {
		s.logger.Error("sessionshim: orphan termination", "session", s.id.String(), "error", err)
	}
}

// ---- discovery record ------------------------------------------------------

func (s *Shim) publishRecord() error { return s.publishRecordWithDeadline(time.Time{}) }

func (s *Shim) publishRecordWithDeadline(deadline time.Time) error {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	s.mu.Lock()
	phase := s.phase
	tombstoned := s.tombstoned
	s.mu.Unlock()
	// Once the terminal observation has withdrawn this session's liveness claim,
	// nothing may put one back. The reachable path is ordinary: a Stop reaps the
	// harness while the controller connection is dropping, and that connection's
	// teardown arms the orphan clock, which republishes. Without this check the
	// record returns AFTER the tombstone removed it, and the session reads as
	// live and terminal at once.
	if tombstoned {
		return nil
	}
	var resumeKey *ResumeKey
	if previous, err := s.registry.Get(s.id); err == nil && previous.ShimID == s.shimID && previous.ProcessEpoch == s.epoch {
		resumeKey = previous.ResumeKey
	}
	rec := Record{
		SchemaVersion:     RecordSchemaVersion,
		OrgID:             s.id.OrgID,
		SessionID:         s.id.SessionID,
		ShimID:            s.shimID,
		ProcessEpoch:      s.epoch,
		PID:               s.self.PID,
		ProcessStartedAt:  s.self.StartedAt,
		SocketPath:        s.socketPath,
		SocketDevice:      s.socketDev,
		SocketInode:       s.socketIno,
		ProtocolMin:       s.protocolMin,
		ProtocolMax:       s.protocolMax,
		Phase:             phase,
		WorkareaPath:      s.workarea,
		WorkareaRoot:      s.workareaRoot,
		ResumeKey:         resumeKey,
		CreatedAtUnixNano: s.now().UnixNano(),
	}
	if !deadline.IsZero() {
		rec.OrphanDeadlineUnixNano = deadline.UnixNano()
	}
	return s.registry.Put(rec)
}

// ---- serving ---------------------------------------------------------------

func (s *Shim) acceptLoop() {
	defer close(s.acceptDone)
	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			return // listener closed
		}
		go s.serveController(conn)
	}
}

// serveController runs one controller connection through authentication,
// handshake, and (on success) the live controller session.
func (s *Shim) serveController(conn *net.UnixConn) {
	w := shimwire.NewWriter(conn)
	r := shimwire.NewReader(conn)

	uid, err := peerUID(conn)
	if err != nil || uid != os.Getuid() {
		detail := "peer credentials unavailable"
		if err == nil {
			detail = fmt.Sprintf("peer uid %d is not the owning uid %d", uid, os.Getuid())
		}
		_ = sendError(w, shimwire.CodeUnauthenticated, detail)
		_ = conn.Close()
		return
	}

	if err := s.handshake(conn, w, r); err != nil {
		// A keepalive is a served exchange, not a refused handshake: it
		// deliberately ends the connection without adopting, so logging it as a
		// refusal would fill an orphaned shim's log with its own liveness.
		if !errors.Is(err, errOrphanKeepaliveServed) {
			s.logger.Warn("sessionshim: controller handshake refused",
				"session", s.id.String(), "error", err)
		}
		_ = conn.Close()
	}
}

// handshake performs Hello → Welcome → Adopted and, on success, hands the
// connection to the live loops.
func (s *Shim) handshake(conn *net.UnixConn, w *shimwire.Writer, r *shimwire.Reader) error {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()

	var (
		outputBarrier *ptyhost.OutputBarrier
		frozenLast    attachwire.HostSeq
	)
	if s.protocolMax >= shimwire.V3 {
		outputBarrier, frozenLast = s.sess.BeginOutputBarrier()
		_ = conn.SetDeadline(time.Now().Add(adoptionOutputBarrierTimeout))
		defer func() {
			if outputBarrier != nil {
				outputBarrier.Release()
			}
		}()
	}
	hello, err := s.buildHello()
	if err != nil {
		_ = sendError(w, shimwire.CodeInternal, "shim state unavailable")
		return err
	}
	if outputBarrier != nil && hello.LastSeq != uint64(frozenLast) {
		_ = sendError(w, shimwire.CodeInternal, "adoption output boundary changed")
		return errors.New("sessionshim: adoption output boundary changed while building Hello")
	}
	if err := writeTyped(w, shimwire.TypeHello, func() ([]byte, error) { return shimwire.EncodeHello(hello) }); err != nil {
		return err
	}

	msg, err := r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read welcome: %w", err)
	}
	if msg.Type == shimwire.TypeHeartbeat {
		// A Heartbeat where a Welcome belongs is the §D8 orphan keepalive: the
		// daemon is telling this shim it is still observed while its own
		// re-adoption keeps failing. It proposes no generation and takes no
		// authority, so it is answered and the connection ends here.
		//
		// The output barrier goes FIRST, before the registry write and the
		// answer. Nothing about the frozen boundary is used on this path, and
		// holding it would stop the harness's output sequence for a record
		// write and a round trip — once per keepalive interval, twenty times
		// across a ten-minute window, on a harness that is still producing.
		// The connection deadline comes down with it: the adoption barrier's
		// bound is sized for a whole handshake, not for one frame each way.
		if outputBarrier != nil {
			outputBarrier.Release()
			outputBarrier = nil
		}
		_ = conn.SetDeadline(time.Now().Add(orphanKeepaliveAnswerTimeout))
		return s.serveOrphanKeepalive(w, msg)
	}
	if msg.Type != shimwire.TypeWelcome {
		_ = sendError(w, shimwire.CodeMalformed, "expected Welcome")
		return fmt.Errorf("sessionshim: %w: expected Welcome, got %s", shimwire.ErrMalformed, msg.Type)
	}
	welcome, err := shimwire.DecodeWelcome(msg.Body)
	if err != nil {
		_ = sendError(w, shimwire.CodeMalformed, "welcome did not decode")
		return err
	}
	if welcome.Protocol != shimwire.ProtocolName {
		_ = sendError(w, shimwire.CodeVersionMismatch, "protocol name mismatch")
		return fmt.Errorf("sessionshim: %w: welcome names protocol %q", shimwire.ErrVersionMismatch, welcome.Protocol)
	}
	if welcome.Selected < s.protocolMin || welcome.Selected > s.protocolMax {
		_ = sendError(w, shimwire.CodeVersionMismatch, "selected version outside this shim's range")
		return fmt.Errorf("sessionshim: %w: selected %d outside [%d,%d]",
			shimwire.ErrVersionMismatch, welcome.Selected, s.protocolMin, s.protocolMax)
	}
	if err := welcome.Extensions.CheckRequired(); err != nil {
		_ = sendError(w, shimwire.CodeExtensionRequired, "required extension unsupported")
		return err
	}
	_, proofBoundCarrier := welcome.Extensions.Values[shimwire.ExtCarrierEpoch]
	if welcome.Selected < shimwire.V3 || !proofBoundCarrier {
		if outputBarrier != nil {
			outputBarrier.Release()
			outputBarrier = nil
		}
	}

	// The SHIM is authoritative for the generation: the daemon proposes and this
	// is where the proposal is accepted or refused. Refusing a non-advancing
	// proposal is what makes "single controller" a property rather than a hope.
	// An EXITED session is still adoptable, deliberately. §D8 keeps the tombstone
	// until a daemon adopts it and durably reports the terminal outcome, so
	// refusing here would strand the one artifact that can close the lifecycle
	// loop. The retained Exit frame rides the ordinary replay path below.
	s.recordMu.Lock()
	s.mu.Lock()
	if welcome.ProposedGeneration <= s.gen {
		current := s.gen
		s.mu.Unlock()
		s.recordMu.Unlock()
		_ = sendError(w, shimwire.CodeStaleGeneration,
			fmt.Sprintf("proposed generation %d does not advance current %d", welcome.ProposedGeneration, current))
		return fmt.Errorf("sessionshim: %w: proposed %d, current %d",
			shimwire.ErrStaleGeneration, welcome.ProposedGeneration, current)
	}
	prev := s.ctrl
	s.gen = welcome.ProposedGeneration
	ctrl := &controllerConn{
		conn: conn, w: w, selected: welcome.Selected,
		snapshotLedger: make(map[uint64]*snapshotLedgerEntry),
		emissionBySeq:  make(map[uint64]*snapshotLedgerEntry),
		pumpDone:       make(chan struct{}),
	}
	if outputBarrier != nil {
		ctrl.installOutputBarrier(outputBarrier)
		outputBarrier = nil
	}
	s.ctrl = ctrl
	s.mu.Unlock()
	s.recordMu.Unlock()

	// §D4: the old controller's socket is closed the moment a new generation
	// commits. A file lock would not be enough — an old daemon can hold an open
	// fd after losing a lock — so the fd itself is taken away.
	if prev != nil {
		prev.close()
	}
	s.disarmOrphan()

	adopted, sub, gap, snap, rawSnapshot, err := s.resume(welcome.ResumeFrom, ctrl.selected)
	if err != nil {
		_ = sendError(w, shimwire.CodeInternal, "resume failed")
		return err
	}
	adopted.Extensions = welcome.Extensions
	ctrl.sub = sub

	if err := writeTyped(w, shimwire.TypeAdopted, func() ([]byte, error) { return shimwire.EncodeAdopted(adopted) }); err != nil {
		ctrl.close()
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	// Gap BEFORE Snapshot, always in that order: the daemon learns what it lost
	// before it is handed the state that replaces it. Reversing them would let a
	// carrier render the snapshot as if it were continuous.
	if gap != nil {
		if err := writeTyped(w, shimwire.TypeGap, func() ([]byte, error) { return shimwire.EncodeGap(*gap) }); err != nil {
			ctrl.close()
			return err
		}
	}
	if snap != nil {
		if err := s.writeSnapshotMsg(ctrl, *snap); err != nil {
			ctrl.close()
			return err
		}
	}
	if rawSnapshot != nil {
		if err := s.writeHostFrame(ctrl, 0, *rawSnapshot); err != nil {
			ctrl.close()
			return err
		}
	}

	go s.pumpOutput(ctrl)
	go s.readControl(ctrl, r)
	return nil
}

func (s *Shim) buildHello() (shimwire.Hello, error) {
	_, lastSeq, err := s.sess.Snapshot()
	if err != nil {
		return shimwire.Hello{}, fmt.Errorf("sessionshim: hello snapshot: %w", err)
	}
	s.mu.Lock()
	gen, phase := s.gen, s.phase
	s.mu.Unlock()
	return shimwire.Hello{
		Protocol:         shimwire.ProtocolName,
		Min:              s.protocolMin,
		Max:              s.protocolMax,
		OrgID:            s.id.OrgID,
		SessionID:        s.id.SessionID,
		ShimID:           s.shimID,
		ProcessEpoch:     s.epoch,
		PID:              s.self.PID,
		ProcessStartedAt: s.self.StartedAt,
		HarnessPID:       s.harness.PID,
		HarnessStartedAt: s.harness.StartedAt,
		WorkareaPath:     s.workarea,
		Phase:            phase,
		Generation:       gen,
		FirstSeq:         uint64(s.sess.FirstBufferedSeq()),
		LastSeq:          uint64(lastSeq),
	}, nil
}

// resume computes the replay disposition for a controller resuming at resumeFrom.
//
// The three outcomes are the §D5 contract in code:
//
//   - Ring hit  -> exact frames replay, then live. Contiguous.
//   - Ring miss -> an EXPLICIT Gap for the lost inclusive range, then a snapshot
//     whose AtSeq is after the gap, then live.
//   - Ahead of stream -> also an explicit gap, with its own reason. The shim
//     does NOT rewind its sequence to match a controller's disagreeing durable
//     state; the sequence is the shim's, and renumbering it would be the exact
//     fabricated continuity the ADR forbids.
func (s *Shim) resume(resumeFrom uint64, selected uint32) (shimwire.Adopted, agent.InteractiveSubscription, *shimwire.GapMsg, *shimwire.SnapshotMsg, *attachwire.Frame, error) {
	if resumeFrom == 0 {
		resumeFrom = uint64(attachwire.HostSeqStart)
	}
	afterSeq := attachwire.HostSeq(resumeFrom - 1)

	sub, err := s.sess.Subscribe(afterSeq)
	if err == nil {
		_, lastSeq, _ := s.sess.Snapshot()
		s.mu.Lock()
		phase := s.phase
		s.mu.Unlock()
		return shimwire.Adopted{
			Generation: s.Generation(),
			Contiguous: true,
			ReplayFrom: resumeFrom,
			ReplayTo:   uint64(lastSeq),
			Phase:      phase,
		}, sub, nil, nil, nil, nil
	}
	if !errors.Is(err, agent.ErrRingMiss) {
		return shimwire.Adopted{}, nil, nil, nil, nil, fmt.Errorf("sessionshim: subscribe: %w", err)
	}
	_, beforeRecoverySeq, _ := s.sess.Snapshot()
	if selected >= shimwire.V3 && resumeFrom > uint64(beforeRecoverySeq)+1 {
		// A durable cursor ahead of this shim's real sequence is a disagreement,
		// not a missing range. In v3 the recovery Snapshot itself carries the exact
		// host sequence, so pretending it followed the ahead cursor would create an
		// impossible Gap -> HostFrame ordering. Selected v1/v2 retain their released
		// semantic recovery behavior.
		return shimwire.Adopted{}, nil, nil, nil, nil,
			fmt.Errorf("sessionshim: selected-v3 resume %d is ahead of host sequence %d", resumeFrom, beforeRecoverySeq)
	}

	// Ring miss. EmitSnapshot allocates the next sequence atomically under the
	// session lock, so its AtSeq is exactly the last frame produced before it —
	// which makes the gap range exact rather than approximate.
	frame, inStream, err := s.sess.EmitSnapshot()
	if err != nil {
		return shimwire.Adopted{}, nil, nil, nil, nil, fmt.Errorf("sessionshim: emit snapshot: %w", err)
	}
	env, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil {
		return shimwire.Adopted{}, nil, nil, nil, nil, fmt.Errorf("sessionshim: decode snapshot envelope: %w", err)
	}

	reason := shimwire.GapRingEvicted
	_, lastSeq, _ := s.sess.Snapshot()
	if resumeFrom > uint64(lastSeq)+1 {
		reason = shimwire.GapAheadOfStream
	}
	gapTo := env.AtSeq
	if gapTo < resumeFrom {
		// Nothing was actually lost (the controller is at or past the head); a
		// zero-width gap would be a lie in the other direction, so report the
		// single-sequence range the snapshot covers.
		gapTo = resumeFrom
	}
	gap := &shimwire.GapMsg{FromSeq: resumeFrom, ToSeq: gapTo, Reason: reason}
	snap := &shimwire.SnapshotMsg{AtSeq: env.AtSeq, Screen: env.Snap}
	var rawSnapshot *attachwire.Frame
	if selected >= shimwire.V3 {
		copyFrame := frame
		copyFrame.Payload = append([]byte(nil), frame.Payload...)
		rawSnapshot = &copyFrame
		snap = nil
	}

	// Continue live AFTER the snapshot frame. When the snapshot rode the stream
	// (inStream), subscribing at its own seq skips redelivering it.
	from := attachwire.HostSeq(env.AtSeq)
	if inStream {
		from = attachwire.HostSeq(frame.Seq)
	}
	sub, err = s.sess.Subscribe(from)
	if err != nil && !errors.Is(err, agent.ErrRingMiss) {
		return shimwire.Adopted{}, nil, nil, nil, nil, fmt.Errorf("sessionshim: subscribe after gap: %w", err)
	}

	s.mu.Lock()
	phase := s.phase
	s.mu.Unlock()
	return shimwire.Adopted{
		Generation: s.Generation(),
		Contiguous: false,
		ReplayFrom: env.AtSeq + 1,
		ReplayTo:   uint64(lastSeq),
		Phase:      phase,
	}, sub, gap, snap, rawSnapshot, nil
}

// pumpOutput forwards host-produced frames to one controller.
//
// The sequence on the wire is the SHIM's, verbatim. No renumbering happens here
// or anywhere downstream: the shim is the sole allocator (§D5), so a daemon that
// restarts resumes into the same namespace rather than starting a new one.
func (s *Shim) pumpOutput(ctrl *controllerConn) {
	defer close(ctrl.pumpDone)
	if ctrl.sub == nil {
		return
	}
	for frame := range ctrl.sub.Frames() {
		if s.currentController() != ctrl {
			return // superseded by a newer generation
		}
		var err error
		if ctrl.selected >= shimwire.V3 {
			requestID := uint64(0)
			var entry *snapshotLedgerEntry
			if frame.Type == attachwire.TypeSnapshot {
				ctrl.emissionMu.Lock()
				entry = ctrl.emissionBySeq[frame.Seq]
				if entry != nil {
					delete(ctrl.emissionBySeq, frame.Seq)
					requestID = entry.request.RequestID
				}
				ctrl.emissionMu.Unlock()
			}
			if entry != nil {
				entry.writeErr = s.writeHostFrameSnapshotPair(ctrl, requestID, frame, entry.result)
				close(entry.delivered)
				err = entry.writeErr
			} else {
				err = s.writeHostFrame(ctrl, requestID, frame)
			}
			if err != nil {
				ctrl.close()
				return
			}
			continue
		}
		switch frame.Type {
		case attachwire.TypeOutput:
			out := attachwire.DecodeOutput(frame.Payload)
			err = ctrl.w.Write(shimwire.TypeOutput, shimwire.EncodeOutput(frame.Seq, frame.RelTime, out.Data))
		case attachwire.TypeExit:
			exit, decErr := attachwire.DecodeExit(frame.Payload)
			if decErr != nil {
				s.logger.Warn("sessionshim: decode exit frame", "session", s.id.String(), "error", decErr)
				continue
			}
			err = writeTyped(ctrl.w, shimwire.TypeExit, func() ([]byte, error) {
				return shimwire.EncodeExit(shimwire.ExitMsg{Seq: frame.Seq, ExitCode: exit.ExitCode, Signal: exit.Signal})
			})
		case attachwire.TypeSnapshot:
			if ctrl.selected >= shimwire.V2 {
				ctrl.emissionMu.Lock()
				entry := ctrl.emissionBySeq[frame.Seq]
				if entry != nil {
					delete(ctrl.emissionBySeq, frame.Seq)
				}
				ctrl.emissionMu.Unlock()
				if entry != nil {
					entry.writeErr = writeSnapshotResult(ctrl, entry.result)
					close(entry.delivered)
					if entry.writeErr != nil {
						return
					}
					continue
				}
			}
			env, decErr := attachwire.DecodeSnapshotEnvelope(frame.Payload)
			if decErr != nil {
				s.logger.Warn("sessionshim: decode snapshot frame", "session", s.id.String(), "error", decErr)
				continue
			}
			err = s.writeSnapshotMsg(ctrl, shimwire.SnapshotMsg{AtSeq: env.AtSeq, Screen: env.Snap})
		default:
			// Marker and applied-Resize echoes are host frames the controller does
			// not need to reconstruct terminal state; they are intentionally not
			// part of the closed v1 vocabulary.
			continue
		}
		if err != nil {
			ctrl.close()
			return // controller gone; readControl arms the orphan clock
		}
	}
	// Exit closes the host subscription but the selected-v2 controller remains a
	// valid direct-transmission path for the final sequence-zero snapshot. Keep
	// the socket until readControl observes the controller dropping it. An
	// unexpected subscription loss cannot grant authority or terminalize the
	// session; the generation-fenced control loop remains the safer owner.
}

func (s *Shim) writeHostFrame(ctrl *controllerConn, requestID uint64, frame attachwire.Frame) error {
	body, err := s.hostFrameBody(requestID, frame)
	if err != nil {
		return err
	}
	return ctrl.w.WriteVersion(ctrl.selected, shimwire.TypeHostFrame, body)
}

// hostFrameBody encodes one HostFrame body, bounding an oversized Snapshot to
// the wire ceiling rather than letting it end the connection carrying it.
func (s *Shim) hostFrameBody(requestID uint64, frame attachwire.Frame) ([]byte, error) {
	encode := func(f attachwire.Frame) ([]byte, error) {
		return shimwire.EncodeHostFrame(shimwire.HostFrame{RequestID: requestID, FrameBytes: f.Encode()})
	}
	body, err := encode(frame)
	if err == nil {
		return body, nil
	}
	if !errors.Is(err, shimwire.ErrMessageTooLarge) {
		return nil, err
	}
	bounded, result, boundErr := boundSnapshotFrame(frame, shimwire.MaxHostFrameBytes,
		func(f attachwire.Frame) (int, error) { return len(f.Encode()), nil })
	if boundErr != nil {
		return nil, boundErr
	}
	s.logSnapshotTrim("host frame", frame.Seq, result)
	return encode(bounded)
}

// logSnapshotTrim is the only place a bounded Snapshot is announced. The trim is
// deliberately not a wire field (see snapshotbound.go), so this log line is the
// audit trail for a shortened history.
//
// It keys on Rewritten, not on the dropped COUNT: a screen can be brought inside
// the ceiling by the re-encode alone with every line retained, and bytes that
// changed must not go unreported just because no line was dropped.
func (s *Shim) logSnapshotTrim(carrier string, seq uint64, bound snapshotBound) {
	if !bound.Rewritten {
		return
	}
	s.logger.Warn("sessionshim: snapshot history truncated to fit the local wire",
		"session", s.id.String(), "carrier", carrier, "seq", seq,
		"droppedScrollbackLines", bound.Dropped)
}

func (s *Shim) writeHostFrameSnapshotPair(
	ctrl *controllerConn,
	requestID uint64,
	frame attachwire.Frame,
	result shimwire.SnapshotResult,
) error {
	hostBody, err := s.hostFrameBody(requestID, frame)
	if err != nil {
		return err
	}
	resultBody, err := shimwire.EncodeSnapshotResult(result)
	if err != nil {
		return err
	}
	return ctrl.w.WriteVersionBatch(ctrl.selected,
		shimwire.Message{Type: shimwire.TypeHostFrame, Body: hostBody},
		shimwire.Message{Type: shimwire.TypeSnapshotResult, Body: resultBody},
	)
}

// readControl consumes controller-originated frames, enforcing the generation
// fence on every mutating one.
func (s *Shim) readControl(ctrl *controllerConn, r *shimwire.Reader) {
	defer func() {
		ctrl.close()
		// Losing THIS controller only arms the orphan clock if it is still the
		// current one. A connection superseded by a newer adoption must not
		// restart a deadline the new controller already cancelled.
		if s.currentController() == ctrl {
			s.mu.Lock()
			s.ctrl = nil
			exited := s.phase == shimwire.PhaseExited
			s.mu.Unlock()
			if !exited {
				s.armOrphan()
			}
		}
	}()

	for {
		msg, err := r.ReadVersion(ctrl.selected)
		if err != nil {
			return
		}
		if err := s.dispatch(ctrl, msg); err != nil {
			return
		}
	}
}

// dispatch handles one controller-originated frame.
//
// A non-nil return ends the connection, and only a TRANSPORT failure does that.
// Every protocol-level refusal answers with an Error and keeps the connection —
// see sendError.
func (s *Shim) dispatch(ctrl *controllerConn, msg shimwire.Message) error {
	switch msg.Type {
	case shimwire.TypeInput:
		gen, data, err := shimwire.DecodeInput(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "input did not decode")
		}
		if !s.authorized(gen) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "input rejected: stale controller generation")
		}
		if _, err := s.sess.WriteInput(data); err != nil {
			return sendError(ctrl.w, shimwire.CodeExited, "input write failed")
		}
	case shimwire.TypeAttributedInput:
		if ctrl.selected < shimwire.V4 {
			return sendError(ctrl.w, shimwire.CodeMalformed, "AttributedInput is not legal in selected v1/v2/v3")
		}
		gen, userID, data, err := shimwire.DecodeAttributedInput(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "attributed input did not decode")
		}
		if !s.authorized(gen) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "attributed input rejected: stale controller generation")
		}
		// The one call site that can identify SYSTEM-authority input at the
		// PTY write boundary: WriteAttributedInput applies last-hop
		// pacing/paste-guard (ptyhost/systeminput.go) only when userID is the
		// shared attachwire.SystemNudgeUserID sentinel — every other userID
		// (ordinary human input, relay-stamped but not SYSTEM) gets exactly
		// WriteInput's verbatim, never-delayed write.
		if _, err := s.sess.WriteAttributedInput(userID, data); err != nil {
			return sendError(ctrl.w, shimwire.CodeExited, "attributed input write failed")
		}
	case shimwire.TypeResize:
		rz, err := shimwire.DecodeResize(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "resize did not decode")
		}
		if !s.authorized(rz.Generation) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "resize rejected: stale controller generation")
		}
		if err := s.sess.Resize(rz.Cols, rz.Rows, rz.PxWidth, rz.PxHeight); err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "resize refused")
		}
	case shimwire.TypeStop:
		st, err := shimwire.DecodeStop(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "stop did not decode")
		}
		if !s.authorized(st.Generation) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "stop rejected: stale controller generation")
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.orphan.TerminationGrace+5*time.Second)
			defer cancel()
			if err := s.Terminate(ctx); err != nil {
				s.logger.Error("sessionshim: stop", "session", s.id.String(), "error", err)
			}
		}()
	case shimwire.TypeHeartbeat:
		heartbeat, err := shimwire.DecodeHeartbeat(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat did not decode")
		}
		return s.persistHeartbeatAck(ctrl, heartbeat)
	case shimwire.TypeError:
		return nil // display-only from the controller; nothing to act on
	case shimwire.TypeSnapshotRequest:
		if ctrl.selected < shimwire.V2 {
			return sendError(ctrl.w, shimwire.CodeMalformed, "SnapshotRequest is not legal in selected v1")
		}
		return s.dispatchSnapshotRequest(ctrl, msg.Body)
	default:
		return sendError(ctrl.w, shimwire.CodeMalformed, "message type is not controller-originated")
	}
	return nil
}

func (s *Shim) persistHeartbeatAck(ctrl *controllerConn, heartbeat shimwire.HeartbeatMsg) error {
	if ctrl.selected < shimwire.V3 {
		// Selected v1/v2 retain their released behavior byte-for-byte. In
		// particular, they neither inspect nor create the v3-only .ack sidecar.
		if heartbeat.Generation == 0 || !s.authorized(heartbeat.Generation) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "heartbeat rejected: stale controller generation")
		}
		return writeTyped(ctrl.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
			return shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: s.Generation(), Phase: s.Phase()})
		})
	}
	s.recordMu.Lock()
	terminal, terminalSeq := s.terminalPublished, s.terminalSeq
	// A published terminal proof does NOT close the acknowledgement rail — it
	// FREEZES it. finalizeTerminal writes the tombstone BEFORE its courtesy
	// waits, and the wait it then spends is for this very acknowledgement of
	// the Exit sequence, so refusing everything the instant the proof lands
	// makes the shim refuse the one receipt it is waiting for: every terminal
	// exit burned the whole flush bound and the controller's ack was rejected
	// as an error it then acted on. An acknowledgement AT OR BELOW the frozen
	// sequence is exactly the cursor an adopting controller resumes from and is
	// honoured normally. Only a claim BEYOND it is refused: no such sequence
	// exists — the harness is reaped and the shim will never allocate another —
	// so it can only be a fabricated or misdirected cursor.
	if terminal && heartbeat.AckedSeq > terminalSeq {
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeExited, "heartbeat rejected: terminal proof is published")
	}
	s.mu.Lock()
	if heartbeat.Generation == 0 || heartbeat.Generation != s.gen || s.ctrl != ctrl {
		s.mu.Unlock()
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeStaleGeneration, "heartbeat rejected: stale controller generation")
	}
	generation, phase := s.gen, s.phase
	currentAck := s.ackedSeq
	s.mu.Unlock()

	hostSeq := terminalSeq
	if !terminal {
		_, lastSeq, snapshotErr := s.sess.Snapshot()
		if snapshotErr != nil {
			s.recordMu.Unlock()
			return sendError(ctrl.w, shimwire.CodeInternal, "heartbeat could not sample host sequence")
		}
		hostSeq = uint64(lastSeq)
	}
	advanced := false
	switch {
	case heartbeat.AckedSeq < currentAck:
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat acknowledgement regressed")
	case heartbeat.AckedSeq > hostSeq:
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat acknowledgement is ahead of host sequence")
	case heartbeat.AckedSeq > currentAck:
		// The sidecar is the LIVE incarnation's crash-restart cursor, and
		// PutTombstone already removed it: writing one back now would leave an
		// entry in the registry whose discovery record no longer exists and
		// which nothing will ever collect. It would also be redundant — the
		// tombstone is itself fsync-backed and carries LastSeq, so after the
		// terminal proof the durable cursor IS the proof. Advance in memory so
		// the receipt below is still exact, and skip the write.
		if !terminal {
			ack := durableAckCursor{
				SchemaVersion: durableAckSchemaVersion,
				OrgID:         s.id.OrgID, SessionID: s.id.SessionID, ShimID: s.shimID, ProcessEpoch: s.epoch,
				ControllerGeneration: generation, AckedSeq: heartbeat.AckedSeq,
			}
			if err := s.registry.putDurableAck(ack); err != nil {
				s.recordMu.Unlock()
				return sendError(ctrl.w, shimwire.CodeInternal, "heartbeat acknowledgement was not persisted")
			}
		}
		s.ackedSeq = heartbeat.AckedSeq
		currentAck = heartbeat.AckedSeq
		advanced = true
		close(s.ackNotify)
		s.ackNotify = make(chan struct{})
	}
	s.recordMu.Unlock()

	// A strictly advancing, durably stored ACK is adopted recovery's existing-
	// wire release for the Hello output barrier. Release follows the durable
	// state update even if the receipt write is then lost: the activation is
	// already committed locally, while the daemon still cannot advance its own
	// cursor without receiving the synchronous receipt. Equal/no-op ACKs never
	// release; fresh candidates must first allocate their mandatory Snapshot,
	// while retained recovery reaches this edge only after carrier_active(H).
	if advanced {
		ctrl.releaseOutputBarrierAfterDurableAdvance()
	}
	// Selected v3 makes persistence synchronous: the daemon cannot advance its
	// own cursor until this exact fsync-backed receipt arrives.
	reply := shimwire.HeartbeatMsg{Generation: generation, Phase: phase, AckedSeq: currentAck}
	return writeTyped(ctrl.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
		return shimwire.EncodeHeartbeat(reply)
	})
}

func (s *Shim) waitForDurableAck(sequence uint64, bound time.Duration) {
	if sequence == 0 || bound <= 0 {
		return
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		s.recordMu.Lock()
		if s.ackedSeq >= sequence {
			s.recordMu.Unlock()
			return
		}
		notify := s.ackNotify
		s.recordMu.Unlock()
		select {
		case <-notify:
		case <-timer.C:
			return
		}
	}
}

func (s *Shim) dispatchSnapshotRequest(ctrl *controllerConn, body []byte) error {
	req, err := shimwire.DecodeSnapshotRequest(body)
	if err != nil {
		return sendError(ctrl.w, shimwire.CodeMalformed, "snapshot request did not decode")
	}
	if prior := ctrl.snapshotLedger[req.RequestID]; prior != nil {
		if prior.request != req {
			return writeSnapshotResult(ctrl, refusedSnapshotResult(req, shimwire.CodeDuplicateChanged))
		}
		return writeSnapshotResult(ctrl, prior.result)
	}
	if len(ctrl.snapshotLedger) >= snapshotRetryLedgerLimit {
		return writeSnapshotResult(ctrl, refusedSnapshotResult(req, shimwire.CodeRequestLedgerFull))
	}
	entry := &snapshotLedgerEntry{request: req, delivered: make(chan struct{})}
	ctrl.snapshotLedger[req.RequestID] = entry
	if !s.authorized(req.Generation) {
		entry.result = refusedSnapshotResult(req, shimwire.CodeStaleGeneration)
		return writeSnapshotResult(ctrl, entry.result)
	}

	switch req.Mode {
	case shimwire.SnapshotInspect:
		screen, atSeq, snapErr := s.sess.Snapshot()
		if snapErr != nil {
			entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
			return writeSnapshotResult(ctrl, entry.result)
		}
		encoded, encErr := screen.Encode()
		if encErr != nil {
			entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
			return writeSnapshotResult(ctrl, entry.result)
		}
		bounded, boundErr := s.boundSnapshotResultBytes(shimwire.SnapshotResult{
			RequestID: req.RequestID, Generation: req.Generation, Mode: req.Mode,
			AtSeq: uint64(atSeq), Bytes: encoded,
		})
		if boundErr != nil {
			entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
			return writeSnapshotResult(ctrl, entry.result)
		}
		entry.result = bounded
		return writeSnapshotResult(ctrl, entry.result)
	case shimwire.SnapshotEmit:
		// Hold emissionMu across publication and correlation registration. The PTY
		// host publishes while EmitSnapshot holds its own sequence lock; the pump
		// cannot observe and forward that frame before this request owns its seq.
		ctrl.emissionMu.Lock()
		frame, inStream, emitErr := ctrl.emitSnapshot(s.sess)
		if emitErr != nil {
			ctrl.emissionMu.Unlock()
			entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
			return writeSnapshotResult(ctrl, entry.result)
		}
		env, decErr := attachwire.DecodeSnapshotEnvelope(frame.Payload)
		if decErr != nil {
			ctrl.emissionMu.Unlock()
			entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
			return writeSnapshotResult(ctrl, entry.result)
		}
		entry.result = shimwire.SnapshotResult{
			RequestID: req.RequestID, Generation: req.Generation, Mode: req.Mode,
			AtSeq: env.AtSeq, InStream: inStream,
		}
		if ctrl.selected < shimwire.V3 || !inStream {
			entry.result.Bytes = frame.Encode()
			bounded, boundErr := s.boundSnapshotResultBytes(entry.result)
			if boundErr != nil {
				ctrl.emissionMu.Unlock()
				entry.result = refusedSnapshotResult(req, shimwire.CodeInternal)
				return writeSnapshotResult(ctrl, entry.result)
			}
			entry.result = bounded
		}
		if inStream {
			ctrl.emissionBySeq[frame.Seq] = entry
		}
		ctrl.emissionMu.Unlock()
		if !inStream {
			return writeSnapshotResult(ctrl, entry.result)
		}
		select {
		case <-entry.delivered:
			return entry.writeErr
		case <-s.done:
			return io.EOF
		}
	default:
		entry.result = refusedSnapshotResult(req, shimwire.CodeMalformed)
		return writeSnapshotResult(ctrl, entry.result)
	}
}

func refusedSnapshotResult(req shimwire.SnapshotRequest, code shimwire.ErrorCode) shimwire.SnapshotResult {
	return shimwire.SnapshotResult{RequestID: req.RequestID, Generation: req.Generation, Mode: req.Mode, Code: code}
}

func writeSnapshotResult(ctrl *controllerConn, result shimwire.SnapshotResult) error {
	body, err := shimwire.EncodeSnapshotResult(result)
	if err != nil {
		return err
	}
	return ctrl.w.WriteVersion(ctrl.selected, shimwire.TypeSnapshotResult, body)
}

// writeSnapshotMsg writes one pre-v3 Snapshot, bounding an oversized screen to
// the wire ceiling. The measurement runs through the real encoder because this
// body is JSON: the screen bytes are base64-inflated on the way out, so any
// arithmetic on the raw length would be measuring the wrong number.
func (s *Shim) writeSnapshotMsg(ctrl *controllerConn, snap shimwire.SnapshotMsg) error {
	encode := func(screen []byte) ([]byte, error) {
		return shimwire.EncodeSnapshot(shimwire.SnapshotMsg{AtSeq: snap.AtSeq, Screen: screen})
	}
	// The ordinary path encodes exactly once and decodes nothing: the body it
	// built is the body it writes.
	body, err := encode(snap.Screen)
	if err != nil {
		return err
	}
	if messageBytes(body) > shimwire.MaxMessageBytes {
		bounded, result, boundErr := boundSnapshotScreen(snap.Screen, shimwire.MaxMessageBytes,
			func(screen []byte) (int, error) {
				encoded, encErr := encode(screen)
				if encErr != nil {
					return 0, encErr
				}
				return messageBytes(encoded), nil
			})
		if boundErr != nil {
			return boundErr
		}
		s.logSnapshotTrim("snapshot message", snap.AtSeq, result)
		if body, err = encode(bounded); err != nil {
			return err
		}
	}
	return ctrl.w.Write(shimwire.TypeSnapshot, body)
}

// boundSnapshotResultBytes bounds the screen or encoded Snapshot frame a
// SnapshotResult carries verbatim, so a long history cannot make the answer to a
// snapshot request unsendable.
//
// It runs where the result is BUILT rather than where it is written, because the
// retry ledger returns the stored result byte-for-byte on an exact retry: a
// result that was bounded on the way out but stored unbounded would answer the
// same request id with two different payloads.
func (s *Shim) boundSnapshotResultBytes(result shimwire.SnapshotResult) (shimwire.SnapshotResult, error) {
	// The header is fixed-width and the payload is copied verbatim, so the framed
	// size is arithmetic — no probe encode anywhere on this path, and none at all
	// in the common case, which then hands writeSnapshotResult the only encode
	// the request pays for.
	sizeOf := func(payload []byte) (int, error) {
		return shimwire.SnapshotResultMessageBytes(len(payload)), nil
	}
	// The overwhelmingly common case answers here, before anything is decoded or
	// re-encoded: a result that already fits is returned with its bytes untouched
	// rather than paying a DecodeFrame plus Encode round trip on every snapshot
	// request.
	if shimwire.SnapshotResultMessageBytes(len(result.Bytes)) <= shimwire.MaxMessageBytes {
		return result, nil
	}
	if result.Mode == shimwire.SnapshotEmit {
		frame, decErr := attachwire.DecodeFrame(result.Bytes)
		if decErr != nil {
			return shimwire.SnapshotResult{}, fmt.Errorf("sessionshim: bound snapshot result: %w", decErr)
		}
		bounded, boundResult, boundErr := boundSnapshotFrame(frame, shimwire.MaxMessageBytes,
			func(f attachwire.Frame) (int, error) { return sizeOf(f.Encode()) })
		if boundErr != nil {
			return shimwire.SnapshotResult{}, boundErr
		}
		s.logSnapshotTrim("snapshot result frame", frame.Seq, boundResult)
		result.Bytes = bounded.Encode()
		return result, nil
	}
	bounded, boundResult, err := boundSnapshotScreen(result.Bytes, shimwire.MaxMessageBytes, sizeOf)
	if err != nil {
		return shimwire.SnapshotResult{}, err
	}
	s.logSnapshotTrim("snapshot result screen", result.AtSeq, boundResult)
	result.Bytes = bounded
	return result, nil
}

// messageBytes is the size the shimwire framer charges for one body: the type
// byte plus the body itself, which is exactly what Writer compares against
// shimwire.MaxMessageBytes.
func messageBytes(body []byte) int { return 1 + len(body) }

// authorized is the single generation fence.
//
// Every mutating frame passes through here — there is deliberately no second
// path. §D4 exists because an old daemon's packet can be DELIVERED after a newer
// daemon adopts; equality against the current generation is what rejects it even
// though the bytes arrived "in time".
func (s *Shim) authorized(gen shimwire.Generation) bool {
	if gen == 0 {
		return false // ErrGenerationRequired: a mutating frame must carry one
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return gen == s.gen
}

func (s *Shim) currentController() *controllerConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl
}

// ---- helpers ---------------------------------------------------------------

func writeTyped(w *shimwire.Writer, t shimwire.MessageType, enc func() ([]byte, error)) error {
	body, err := enc()
	if err != nil {
		return err
	}
	return w.Write(t, body)
}

// sendError reports a closed error code plus display-only detail to the peer.
//
// It returns only a TRANSPORT failure. Refusing a frame is deliberately not a
// reason to end the connection: a controller that gets disconnected the instant
// it is refused cannot read WHY it was refused, and would retry the same frame
// on a fresh connection forever. The one place a refusal is terminal is the
// handshake, where the caller returns its own error after calling this.
func sendError(w *shimwire.Writer, code shimwire.ErrorCode, detail string) error {
	body, err := shimwire.EncodeError(shimwire.ErrorMsg{Code: code, Detail: detail})
	if err != nil {
		return err
	}
	return w.Write(shimwire.TypeError, body)
}

func newShimID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sessionshim: generate shim id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// statSocket returns the socket's device and inode, the pair a discovery record
// binds to so a controller can prove it dialled the SAME socket the record
// described rather than a replacement at the same path.
func statSocket(path string) (dev, ino uint64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("sessionshim: stat socket: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, nil // platform without stat details: identity check degrades to path
	}
	return uint64(st.Dev), uint64(st.Ino), nil //nolint:gosec,unconvert // Dev/Ino widths are platform-dependent; both are non-negative identifiers
}
