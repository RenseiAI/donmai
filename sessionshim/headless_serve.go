package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// headlessTerminalSeq is the only sequence a headless shim ever allocates.
// The headless profile has no host output stream; the terminal observation is
// its sole sequence-bearing message.
const headlessTerminalSeq = 1

// RunnerProcess is the headless profile's owned run: the runner and the
// harness process group(s) it drives, supervised without a PTY. It is the
// headless analogue of the interactive profile's ptyhost.Session: the thing
// the shim watches, stops, and proves gone before it writes the terminal
// observation.
//
// The runner may be hosted in the shim's own process (ADR-2026-10-07 D1,
// Option C: the worker process hosts the shim in process). Nothing in this
// contract may therefore lean on the runner's PID being a separate process:
// the PID is correlation only, Stop never signals the shim's own process
// group, and the reap proof comes from the harness groups, never from the
// runner PID.
type RunnerProcess interface {
	// PID is the runner's process id. Under Option C it is the shim's own
	// PID. It is recorded as correlation (the record's and Hello's harness
	// identity), never used as the reap proof.
	PID() int
	// Done closes when the run has ended: the runner's work is over and it
	// has waited its harness processes.
	Done() <-chan struct{}
	// Exit reports the runner's terminal exit. It is called after Done
	// closes; like ptyhost.Session.Exit, the second value reports whether
	// the observation is authoritative.
	Exit() (RunnerExit, bool)
	// Stop runs the bounded teardown: end the runner's work, signal every
	// harness process group it owns, wait for them, and return. It mirrors
	// ptyhost.Session.Stop's contract — a bounded SIGTERM→grace→SIGKILL on
	// the owned groups — so the orphan deadline and a generation-fenced Stop
	// share one teardown semantic. A runner hosted in the shim's own process
	// must never signal its own process group: the shim lives there.
	Stop(ctx context.Context) error
	// HarnessGroupsReaped is the reap proof the terminal observation carries
	// as groupReaped: true only when every harness process group the runner
	// started is proved to have no member left (ProcessGroupGone is the
	// primitive for that). It must not be answered from the runner's own PID,
	// which under Option C is the shim's process and is alive while the
	// tombstone is written. An error means unproved, which the tombstone
	// records as not reaped.
	HarnessGroupsReaped() (bool, error)
}

// RunnerExit is a runner's terminal exit: the process exit code and, when the
// process died on a signal, that signal's name.
type RunnerExit struct {
	ExitCode uint64
	Signal   string
}

// HeadlessOptions configure a headless StartHeadless call.
type HeadlessOptions struct {
	// Identity is the session's sole lifecycle identity. Required.
	Identity Identity

	// Registry is where the discovery record and terminal tombstone are
	// published. Required.
	Registry *Registry

	// Runner is the owned runner process. Required: the headless shim
	// supervises it without spawning anything of its own.
	Runner RunnerProcess

	// WorkareaPath is the workarea the runner runs against. It is recorded
	// and verified at adoption exactly as the interactive profile's is.
	WorkareaPath string
	// WorkareaRoot is the optional session-owned lifecycle root.
	WorkareaRoot string

	// OutboxKey and OutboxState name the session's terminal-status outbox
	// record and its delivery state at start. Empty means no record yet;
	// the tombstone carries the values current when the runner exits.
	OutboxKey   string
	OutboxState HeadlessOutboxState

	// Orphan bounds the controller-loss rule. A zero policy uses
	// DefaultOrphanPolicy.
	Orphan OrphanPolicy

	// ProcessEpoch is the monotonic per-session value for this shim incarnation.
	ProcessEpoch uint64

	// There is deliberately no protocol-range option: a headless shim
	// advertises exactly [shimwire.HeadlessMin, shimwire.HeadlessMax], the
	// contract's [6,6], and refuses every lower selection.

	Logger *slog.Logger

	// Now lets tests drive deterministic timestamps.
	Now func() time.Time
}

// StartHeadless starts the headless profile: it takes ownership of an
// already-running runner process, begins listening on the session's local
// adoption socket, and publishes the headless discovery record.
//
// The headless shim owns the runner's process group with no ring, no VT, and
// no flow control: the runner's events, activity and spans go to the control
// plane directly, never sequenced through the daemon. The runner's own exit,
// after the harness group is proved gone, is the terminal observation.
//
// It advertises exactly [shimwire.HeadlessMin, shimwire.HeadlessMax] (the
// contract's [6,6]) and refuses any lower selection, so a controller that
// predates the headless profile quarantines it as protocol_mismatch instead of
// adopting it as a terminal session.
//
// Like Start, the socket exists before the record names it, and the orphan
// clock starts immediately: a shim whose creating daemon dies before it ever
// adopts must still be bounded.
func StartHeadless(opts HeadlessOptions) (*Shim, error) {
	if opts.Runner == nil {
		return nil, errors.New("sessionshim: StartHeadless requires a Runner")
	}
	serveOpts := Options{
		Identity:     opts.Identity,
		Registry:     opts.Registry,
		Workload:     WorkloadHeadless,
		WorkareaPath: opts.WorkareaPath,
		WorkareaRoot: opts.WorkareaRoot,
		Orphan:       opts.Orphan,
		ProcessEpoch: opts.ProcessEpoch,
		Now:          opts.Now,
	}
	if opts.Logger != nil {
		serveOpts.Logger = opts.Logger
	}
	core, opened, err := serve(serveOpts)
	if err != nil {
		return nil, err
	}
	ln, socketPath := opened.ln, opened.path

	pid := opts.Runner.PID()
	if pid <= 0 {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("sessionshim: headless runner pid %d is not positive", pid)
	}
	runnerStart, startErr := processStartTime(pid)
	if startErr != nil {
		// The runner is supervised but its identity cannot be pinned, and
		// the record, Hello and tombstone carry that identity as their
		// correlation. Fail closed without stopping the runner: unlike the
		// interactive profile's freshly spawned harness, this process was
		// already running before the shim took ownership.
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("sessionshim: pin headless runner process identity: %w", startErr)
	}

	s := core.finishHeadless(
		opts.Runner,
		ln,
		ProcessIdentity{PID: pid, StartedAt: runnerStart},
		headlessOutbox{key: opts.OutboxKey, state: opts.OutboxState},
	)

	if err := s.publishRecord(); err != nil {
		s.abortUnserved(ln, socketPath)
		return nil, err
	}

	s.armOrphan()

	go s.acceptLoop()
	go s.watchRunner()
	return s, nil
}

// headlessOutbox is the outbox record the tombstone names at terminal time.
type headlessOutbox struct {
	key   string
	state HeadlessOutboxState
}

// finishHeadless attaches the owned runner process, its pinned identity, and
// the outbox pointer to a core built by serve. The listener travels with it
// because Close owns the listener lifetime.
func (s *Shim) finishHeadless(runner RunnerProcess, ln *net.UnixListener, harness ProcessIdentity, outbox headlessOutbox) *Shim {
	s.runner = runner
	s.ln = ln
	s.harness = harness
	s.headlessOutbox = outbox
	return s
}

// WorkloadOf reports the profile this shim serves.
func (s *Shim) WorkloadOf() Workload {
	if s.workload == "" {
		return WorkloadInteractive
	}
	return s.workload
}

// watchRunner turns an ordinary runner exit into the terminal observation.
// It is the headless analogue of watchHarness: the runner's own exit,
// after the harness group is proved gone, becomes the tombstone.
func (s *Shim) watchRunner() {
	<-s.runner.Done()
	// Take stopOnce here too: a runner that exits on its own and a Terminate
	// racing it must produce ONE tombstone, not two.
	s.stopOnce.Do(func() {
		if err := s.finalizeHeadlessTerminal(HeadlessExitCompleted); err != nil {
			s.logger.Error("sessionshim: finalize headless terminal observation", "session", s.id.String(), "error", err)
		}
	})
	if s.currentController() == nil {
		_ = s.Close()
		return
	}
	s.beginFinalScreenWindow()
}

// terminateHeadlessOrphan runs the orphan-deadline teardown of the owned
// runner process group and persists the headless terminal observation with
// the orphaned cause. It is what Terminate reaches on a headless shim.
func (s *Shim) terminateHeadlessOrphan(ctx context.Context) error {
	if err := s.terminateRunner(ctx); err != nil {
		return err
	}
	<-s.runner.Done()
	return s.finalizeHeadlessTerminal(HeadlessExitOrphaned)
}

// terminateHeadlessStop runs the generation-fenced Stop teardown of the owned
// runner process group and persists the headless terminal observation with
// the completed cause. A fenced Stop ends the run the way the runner's own
// exit would: the outbox record the tombstone names carries the runner's
// terminal state, so the cause stays completed rather than recording which
// side asked first.
func (s *Shim) terminateHeadlessStop(ctx context.Context) error {
	if err := s.terminateRunner(ctx); err != nil {
		return err
	}
	<-s.runner.Done()
	return s.finalizeHeadlessTerminal(HeadlessExitCompleted)
}

// terminateRunner runs the bounded teardown of the owned runner process group.
func (s *Shim) terminateRunner(ctx context.Context) error {
	stopCtx, cancel := context.WithTimeout(ctx, s.orphan.TerminationGrace+2*time.Second)
	defer cancel()
	_ = s.runner.Stop(stopCtx)
	select {
	case <-s.runner.Done():
	case <-stopCtx.Done():
	}
	return nil
}

// finalizeHeadlessTerminal persists the immutable headless terminal
// observation exactly once, with the given cause.
func (s *Shim) finalizeHeadlessTerminal(cause HeadlessExitCause) error {
	exit, _ := s.runner.Exit()

	// Proof, not assumption: the runner proves its harness process groups
	// gone. A tombstone that claims a reap it did not verify is worse than no
	// tombstone, because the release rule lets a proven tombstone release a
	// claim. The runner PID is deliberately not the proof: a runner hosted in
	// the shim's own process (Option C) is alive right here, so a PID proof
	// would never pass, and an out-of-process runner's exit says nothing about
	// a harness group it left behind.
	groupsReaped, reapErr := s.runner.HarnessGroupsReaped()
	reaped := reapErr == nil && groupsReaped

	s.mu.Lock()
	if s.tombstoned {
		s.mu.Unlock()
		return nil
	}
	s.tombstoned = true
	s.phase = shimwire.PhaseExited
	ctrl := s.ctrl
	s.mu.Unlock()

	s.recordMu.Lock()
	outbox := s.headlessOutbox
	s.recordMu.Unlock()
	// No normalization: an empty outbox state names no outbox record, which
	// is the ordinary case until the persist-before-send half lands. The
	// tombstone still carries the runner's exit, cause, and reap proof.

	t := Tombstone{
		SchemaVersion:      HeadlessRecordSchemaVersion,
		Workload:           WorkloadHeadless,
		OrgID:              s.id.OrgID,
		SessionID:          s.id.SessionID,
		ShimID:             s.shimID,
		ProcessEpoch:       s.epoch,
		HarnessPID:         s.harness.PID,
		HarnessStartedAt:   s.harness.StartedAt,
		ExitCode:           exit.ExitCode,
		Signal:             exit.Signal,
		LastSeq:            headlessTerminalSeq,
		GroupReaped:        reaped,
		Cause:              cause,
		OutboxKey:          outbox.key,
		OutboxState:        outbox.state,
		ObservedAtUnixNano: s.now().UnixNano(),
	}
	// Under recordMu for the same reason the interactive terminal write takes
	// it: this is the write that withdraws the liveness claim, and a republish
	// landing between its two halves would undo it. s.tombstoned is already set
	// above, so any republish that queues behind this lock is refused rather
	// than reordered.
	s.recordMu.Lock()
	t.ResumeKey = s.resumeKey
	err := s.registry.PutTombstone(t)
	if err == nil {
		s.terminalPublished = true
		s.terminalSeq = headlessTerminalSeq
	}
	s.recordMu.Unlock()
	if err != nil {
		return fmt.Errorf("sessionshim: persist headless tombstone: %w", err)
	}

	s.terminalDoneOnce.Do(func() { close(s.terminalDone) })
	s.closeDoneWhenStopped()
	if s.onTerminalCourtesy != nil {
		s.onTerminalCourtesy()
	}
	// PROOF FIRST, COURTESY SECOND, exactly as the interactive terminal path:
	// everything below is best-effort delivery to a controller that may
	// already be gone.
	if ctrl != nil {
		// The courtesy wait is the same bound the interactive profile spends:
		// the controller acknowledges the terminal observation with a
		// generation-fenced Heartbeat(1) after durably recording it, and a
		// missing acknowledgement leaves the tombstone for the next adoption.
		s.waitForDurableAck(headlessTerminalSeq, s.finalizeWaitBound())
	}
	return nil
}

// serveHeadlessController runs one adopted headless controller connection:
// the generation fence on Stop, Heartbeat and CredentialUpdate, refusal of
// every PTY-shaped type, and the terminal courtesy. Heartbeat doubles as the
// terminal acknowledgement: ackedSeq 1 after the tombstone is durable is the
// receipt.
//
// Every frame is read through readControllerFrame, which reads a headless
// connection with shimwire's profile reader: a PTY-shaped type is answered
// Error{code:"malformed"} at the read and never reaches dispatchHeadless.
func (s *Shim) serveHeadlessController(ctrl *controllerConn, r *shimwire.Reader) {
	defer s.loseController(ctrl)
	for {
		frame, err := s.readControllerFrame(ctrl, r)
		if err != nil {
			return
		}
		if frame.Handled {
			continue
		}
		if end := s.dispatchHeadless(ctrl, frame.Message); end {
			return
		}
	}
}

// dispatchHeadless handles one controller-originated frame on a headless
// connection. A true return ends the connection, and only a TRANSPORT failure
// does that; every protocol-level refusal answers with an Error and keeps it.
func (s *Shim) dispatchHeadless(ctrl *controllerConn, msg shimwire.Message) (end bool) {
	switch msg.Type {
	case shimwire.TypeStop:
		st, err := shimwire.DecodeStop(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "stop did not decode") != nil
		}
		if !s.authorized(st.Generation) {
			return sendError(ctrl.w, shimwire.CodeStaleGeneration, "stop rejected: stale controller generation") != nil
		}
		go func() {
			// A fenced Stop ends the run the way the runner's own exit
			// would, with the completed cause: the outbox record the
			// tombstone names carries the runner's terminal state.
			ctx, cancel := context.WithTimeout(context.Background(), s.orphan.TerminationGrace+5*time.Second)
			defer cancel()
			if err := s.terminateHeadlessStop(ctx); err != nil {
				s.logger.Error("sessionshim: headless stop", "session", s.id.String(), "error", err)
			}
		}()
		return false
	case shimwire.TypeHeartbeat:
		heartbeat, err := shimwire.DecodeHeartbeat(msg.Body)
		if err != nil {
			return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat did not decode") != nil
		}
		return s.persistHeadlessHeartbeatAck(ctrl, heartbeat) != nil
	case shimwire.TypeCredentialUpdate:
		// Legal on both profiles at selected v6: the same correlation, ledger
		// and generation fence the interactive dispatch applies.
		return s.dispatchCredentialUpdate(ctrl, msg.Body) != nil
	case shimwire.TypeError:
		return false // display-only from the controller; nothing to act on
	default:
		// Every PTY-shaped type was already refused at the profile read, so
		// what reaches here is a shim-originated type a controller must never
		// send (Hello, Welcome, Adopted, CredentialResult, HeadlessExit). It is
		// refused without action; the connection stays up so the controller
		// can read WHY rather than retrying the same frame forever.
		return sendError(ctrl.w, shimwire.CodeMalformed, "message type is not controller-originated") != nil
	}
}

// persistHeadlessHeartbeatAck answers a headless Heartbeat and records the
// terminal acknowledgement. Before the tombstone it is liveness with a
// generation fence; after it, ackedSeq 1 is the receipt the terminal courtesy
// waits for, and any claim beyond 1 is refused — no such sequence exists on a
// profile with no output stream.
func (s *Shim) persistHeadlessHeartbeatAck(ctrl *controllerConn, heartbeat shimwire.HeartbeatMsg) error {
	if heartbeat.Generation == 0 || !s.authorized(heartbeat.Generation) {
		return sendError(ctrl.w, shimwire.CodeStaleGeneration, "heartbeat rejected: stale controller generation")
	}
	s.recordMu.Lock()
	terminal, terminalSeq := s.terminalPublished, s.terminalSeq
	if terminal && heartbeat.AckedSeq > terminalSeq {
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeExited, "heartbeat rejected: terminal proof is published")
	}
	if heartbeat.AckedSeq > headlessTerminalSeq {
		s.recordMu.Unlock()
		return sendError(ctrl.w, shimwire.CodeMalformed, "heartbeat acknowledgement is ahead of headless terminal sequence")
	}
	s.mu.Lock()
	generation, phase := s.gen, s.phase
	s.mu.Unlock()
	if heartbeat.AckedSeq == headlessTerminalSeq && terminal {
		s.ackedSeq = headlessTerminalSeq
		close(s.ackNotify)
		s.ackNotify = make(chan struct{})
	}
	s.recordMu.Unlock()
	reply := shimwire.HeartbeatMsg{Generation: generation, Phase: phase, AckedSeq: heartbeat.AckedSeq}
	return writeTyped(ctrl.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
		return shimwire.EncodeHeartbeat(reply)
	})
}
