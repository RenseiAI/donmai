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

	// Orphan bounds the controller-loss rule. A zero policy uses
	// DefaultOrphanPolicy.
	Orphan OrphanPolicy

	// ProcessEpoch is the monotonic per-session value for this shim incarnation.
	ProcessEpoch uint64

	Logger *slog.Logger

	// Now lets tests drive deterministic timestamps.
	Now func() time.Time
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

	shimID   string
	epoch    uint64
	self     ProcessIdentity
	harness  ProcessIdentity
	workarea string

	socketPath string
	socketDev  uint64
	socketIno  uint64

	// recordMu serializes every write to this session's registry entry against
	// every other one. s.mu alone is not enough: it guards the fields, while the
	// hazard is two DISK writes interleaving — a controller-loss republish and
	// the terminal withdrawal racing each other can otherwise resurrect a
	// liveness record for a harness that is provably gone.
	recordMu sync.Mutex

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
}

// controllerConn is one attached controller.
type controllerConn struct {
	conn      *net.UnixConn
	w         *shimwire.Writer
	sub       agent.InteractiveSubscription
	closeOnce sync.Once
}

func (c *controllerConn) close() {
	c.closeOnce.Do(func() {
		if c.sub != nil {
			_ = c.sub.Close()
		}
		_ = c.conn.Close()
	})
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
		id:         opts.Identity,
		registry:   opts.Registry,
		sess:       sess,
		ln:         ln,
		logger:     opts.logger(),
		now:        opts.now(),
		orphan:     orphan,
		shimID:     shimID,
		epoch:      opts.ProcessEpoch,
		self:       self,
		harness:    ProcessIdentity{PID: harnessPID, StartedAt: harnessStart},
		workarea:   opts.WorkareaPath,
		socketPath: socketPath,
		socketDev:  dev,
		socketIno:  ino,
		phase:      shimwire.PhaseRunning,
		done:       make(chan struct{}),
		acceptDone: make(chan struct{}),
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

	if ctrl != nil {
		// Best-effort: the controller may already be gone, which is exactly the
		// case the tombstone exists for.
		_ = writeTyped(ctrl.w, shimwire.TypeExit, func() ([]byte, error) {
			return shimwire.EncodeExit(shimwire.ExitMsg{Seq: uint64(lastSeq), ExitCode: exit.ExitCode, Signal: exit.Signal})
		})
	}

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
	s.recordMu.Unlock()
	if err != nil {
		return fmt.Errorf("sessionshim: persist tombstone: %w", err)
	}
	close(s.done)
	return nil
}

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
		ProtocolMin:       shimwire.ProtocolMin,
		ProtocolMax:       shimwire.ProtocolMax,
		Phase:             phase,
		WorkareaPath:      s.workarea,
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
		s.logger.Warn("sessionshim: controller handshake refused",
			"session", s.id.String(), "error", err)
		_ = conn.Close()
	}
}

// handshake performs Hello → Welcome → Adopted and, on success, hands the
// connection to the live loops.
func (s *Shim) handshake(conn *net.UnixConn, w *shimwire.Writer, r *shimwire.Reader) error {
	hello, err := s.buildHello()
	if err != nil {
		_ = sendError(w, shimwire.CodeInternal, "shim state unavailable")
		return err
	}
	if err := writeTyped(w, shimwire.TypeHello, func() ([]byte, error) { return shimwire.EncodeHello(hello) }); err != nil {
		return err
	}

	msg, err := r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read welcome: %w", err)
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
	if welcome.Selected < shimwire.ProtocolMin || welcome.Selected > shimwire.ProtocolMax {
		_ = sendError(w, shimwire.CodeVersionMismatch, "selected version outside this shim's range")
		return fmt.Errorf("sessionshim: %w: selected %d outside [%d,%d]",
			shimwire.ErrVersionMismatch, welcome.Selected, shimwire.ProtocolMin, shimwire.ProtocolMax)
	}
	if err := welcome.Extensions.CheckRequired(); err != nil {
		_ = sendError(w, shimwire.CodeExtensionRequired, "required extension unsupported")
		return err
	}

	// The SHIM is authoritative for the generation: the daemon proposes and this
	// is where the proposal is accepted or refused. Refusing a non-advancing
	// proposal is what makes "single controller" a property rather than a hope.
	// An EXITED session is still adoptable, deliberately. §D8 keeps the tombstone
	// until a daemon adopts it and durably reports the terminal outcome, so
	// refusing here would strand the one artifact that can close the lifecycle
	// loop. The retained Exit frame rides the ordinary replay path below.
	s.mu.Lock()
	if welcome.ProposedGeneration <= s.gen {
		current := s.gen
		s.mu.Unlock()
		_ = sendError(w, shimwire.CodeStaleGeneration,
			fmt.Sprintf("proposed generation %d does not advance current %d", welcome.ProposedGeneration, current))
		return fmt.Errorf("sessionshim: %w: proposed %d, current %d",
			shimwire.ErrStaleGeneration, welcome.ProposedGeneration, current)
	}
	prev := s.ctrl
	s.gen = welcome.ProposedGeneration
	ctrl := &controllerConn{conn: conn, w: w}
	s.ctrl = ctrl
	s.mu.Unlock()

	// §D4: the old controller's socket is closed the moment a new generation
	// commits. A file lock would not be enough — an old daemon can hold an open
	// fd after losing a lock — so the fd itself is taken away.
	if prev != nil {
		prev.close()
	}
	s.disarmOrphan()

	adopted, sub, gap, snap, err := s.resume(welcome.ResumeFrom)
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
		if err := writeTyped(w, shimwire.TypeSnapshot, func() ([]byte, error) { return shimwire.EncodeSnapshot(*snap) }); err != nil {
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
		Min:              shimwire.ProtocolMin,
		Max:              shimwire.ProtocolMax,
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
func (s *Shim) resume(resumeFrom uint64) (shimwire.Adopted, agent.InteractiveSubscription, *shimwire.GapMsg, *shimwire.SnapshotMsg, error) {
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
		}, sub, nil, nil, nil
	}
	if !errors.Is(err, agent.ErrRingMiss) {
		return shimwire.Adopted{}, nil, nil, nil, fmt.Errorf("sessionshim: subscribe: %w", err)
	}

	// Ring miss. EmitSnapshot allocates the next sequence atomically under the
	// session lock, so its AtSeq is exactly the last frame produced before it —
	// which makes the gap range exact rather than approximate.
	frame, inStream, err := s.sess.EmitSnapshot()
	if err != nil {
		return shimwire.Adopted{}, nil, nil, nil, fmt.Errorf("sessionshim: emit snapshot: %w", err)
	}
	env, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil {
		return shimwire.Adopted{}, nil, nil, nil, fmt.Errorf("sessionshim: decode snapshot envelope: %w", err)
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

	// Continue live AFTER the snapshot frame. When the snapshot rode the stream
	// (inStream), subscribing at its own seq skips redelivering it.
	from := attachwire.HostSeq(env.AtSeq)
	if inStream {
		from = attachwire.HostSeq(frame.Seq)
	}
	sub, err = s.sess.Subscribe(from)
	if err != nil && !errors.Is(err, agent.ErrRingMiss) {
		return shimwire.Adopted{}, nil, nil, nil, fmt.Errorf("sessionshim: subscribe after gap: %w", err)
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
	}, sub, gap, snap, nil
}

// pumpOutput forwards host-produced frames to one controller.
//
// The sequence on the wire is the SHIM's, verbatim. No renumbering happens here
// or anywhere downstream: the shim is the sole allocator (§D5), so a daemon that
// restarts resumes into the same namespace rather than starting a new one.
func (s *Shim) pumpOutput(ctrl *controllerConn) {
	if ctrl.sub == nil {
		return
	}
	defer ctrl.close()
	for frame := range ctrl.sub.Frames() {
		if s.currentController() != ctrl {
			return // superseded by a newer generation
		}
		var err error
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
			env, decErr := attachwire.DecodeSnapshotEnvelope(frame.Payload)
			if decErr != nil {
				s.logger.Warn("sessionshim: decode snapshot frame", "session", s.id.String(), "error", decErr)
				continue
			}
			err = writeTyped(ctrl.w, shimwire.TypeSnapshot, func() ([]byte, error) {
				return shimwire.EncodeSnapshot(shimwire.SnapshotMsg{AtSeq: env.AtSeq, Screen: env.Snap})
			})
		default:
			// Marker and applied-Resize echoes are host frames the controller does
			// not need to reconstruct terminal state; they are intentionally not
			// part of the closed v1 vocabulary.
			continue
		}
		if err != nil {
			return // controller gone; readControl arms the orphan clock
		}
	}
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
		msg, err := r.Read()
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
		// Heartbeat is read-only liveness plus an acknowledgement; it carries no
		// authority and therefore needs no fence.
		if _, err := shimwire.DecodeHeartbeat(msg.Body); err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat did not decode")
		}
		return writeTyped(ctrl.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
			return shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: s.Generation(), Phase: s.Phase()})
		})
	case shimwire.TypeError:
		return nil // display-only from the controller; nothing to act on
	default:
		return sendError(ctrl.w, shimwire.CodeMalformed, "message type is not controller-originated")
	}
	return nil
}

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
