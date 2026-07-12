package ptyhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/creack/pty"
)

// stopGrace is the deadline between SIGTERM and SIGKILL on Stop / kill
// (§12.2 "signal choice and escalation are host policy"). Matches the PTY-harness
// precedent.
const stopGrace = 5 * time.Second

// errExited is returned by mutating methods after the session has exited
// (§12.2: no host frames follow Exit).
var errExited = errors.New("ptyhost: session has exited")

// Session is the live interactive surface of one PTY-hosted process. It
// satisfies agent.InteractiveSession (see conformance.go) and is safe for
// concurrent use.
type Session struct {
	cmd     *exec.Cmd
	ptmx    *os.File
	replyW  *replyWriter
	spawnAt time.Time
	epoch   uint64
	logger  *slog.Logger

	vt  terminal
	rec *recorder

	// mu guards all sequence/ring/subscription/VT/recorder state below. The VT
	// is fed only from run() and snapshotted only under mu, so feeding and
	// snapshots are consistent (§12 single-feeder discipline).
	mu          sync.Mutex
	nextSeq     attachwire.HostSeq // next host output seq to allocate (starts at 1)
	lastRel     uint64             // last emitted rel_time (monotonic non-decreasing)
	ring        *ring
	subs        map[*subscription]struct{}
	exited      bool
	exitPayload attachwire.ExitPayload
	exitSeq     attachwire.HostSeq
	localDriver bool // a live local attach currently holds the single-driver pen

	closedFlag    atomic.Bool
	ptmxCloseOnce sync.Once
	finalizeOnce  sync.Once
	stopOnce      sync.Once
	shutdown      chan struct{}
	done          chan struct{}
}

// Spawn runs spec.Command under a pseudo-terminal and returns the live Session.
// The PTY winsize is applied before the child starts (§8); the process becomes a
// session/process-group leader for group teardown (§12.2).
func Spawn(spec Spec) (*Session, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("ptyhost: Spawn requires a non-empty Command")
	}
	cols, rows := spec.cols(), spec.rows()

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...) //nolint:gosec // caller-supplied argv is the session's own command
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = composeEnv(os.Environ(), spec.Env)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("ptyhost: pty start: %w", err)
	}
	spawnAt := time.Now()

	rec, err := newRecorder(spec.RecordPath, int(cols), int(rows), termEnv(cmd.Env), shellName())
	if err != nil {
		signalProcessGroup(cmd, syscall.SIGKILL)
		_ = ptmx.Close()
		return nil, fmt.Errorf("ptyhost: recorder: %w", err)
	}

	// Query replies go through a bounded async writer, never directly to
	// the master: the responders run inside the read loop under s.mu, and a
	// child that emits queries without reading stdin would otherwise wedge
	// the whole session on the kernel's bounded slave input queue (the T10
	// querywedge finding; reproducer in querywedge_test.go).
	replyW := newReplyWriter(ptmx, spec.logger())

	s := &Session{
		cmd:      cmd,
		ptmx:     ptmx,
		replyW:   replyW,
		spawnAt:  spawnAt,
		epoch:    spec.Epoch,
		logger:   spec.logger(),
		vt:       newVTHost(int(cols), int(rows), spec.scrollback(), replyW, spec.logger()),
		rec:      rec,
		nextSeq:  attachwire.HostSeqStart,
		ring:     newRing(spec.ringBytes()),
		subs:     make(map[*subscription]struct{}),
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// ---- read loop & teardown --------------------------------------------------

func (s *Session) run() {
	readerDone := make(chan struct{})
	go func() {
		select {
		case <-s.shutdown:
			s.closePTY() // unblock a blocked read on Stop
		case <-readerDone:
		}
	}()

	buf := make([]byte, readBufSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.onOutput(buf[:n])
		}
		if err != nil {
			break // EOF / EIO on slave close — normal PTY teardown
		}
	}
	close(readerDone)
	s.closePTY()
	s.finalize()
}

// onOutput feeds one read chunk to the VT (which may synthesize query answers to
// the master) then publishes it as Output frame(s), split to maxOutputFrame.
func (s *Session) onOutput(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return
	}
	s.vt.write(data)
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxOutputFrame {
			chunk = data[:maxOutputFrame]
		}
		f := s.allocFrameLocked(attachwire.TypeOutput, attachwire.EncodeOutput(chunk))
		s.publishLocked(f)
		s.rec.output(f.RelTime, chunk)
		data = data[len(chunk):]
	}
}

// finalize drains complete (child reaped), builds the Exit payload, emits the
// final Exit frame after every Output has been published (flush-before-Exit),
// then closes every subscription and Done (§12.2). Runs exactly once.
func (s *Session) finalize() {
	s.finalizeOnce.Do(func() {
		waitErr := s.cmd.Wait()
		exitPayload := exitPayloadFrom(waitErr)

		s.mu.Lock()
		s.exited = true
		s.exitPayload = exitPayload
		seq := s.nextSeq
		s.nextSeq++
		s.exitSeq = seq
		rel := s.relTimeLocked()
		f := attachwire.Frame{
			Type:    attachwire.TypeExit,
			Seq:     uint64(seq),
			RelTime: rel,
			Payload: exitPayload.Encode(),
		}
		s.ring.append(f)
		subs := make([]*subscription, 0, len(s.subs))
		for sub := range s.subs {
			sub.enqueue(f)
			subs = append(subs, sub)
		}
		s.mu.Unlock()

		// Close subscriptions only after the Exit frame has been delivered.
		for _, sub := range subs {
			sub.finish()
		}
		s.rec.close()
		close(s.done)
	})
}

// Stop terminates the session: SIGTERM to the process group, a grace window
// (default stopGrace, capped by ctx), then SIGKILL. The child's death drains the
// master to EOF and drives the normal flush→Exit flow (§12.2). Kill-by-request
// (the relay `kill` control message) maps to this same path. Idempotent.
func (s *Session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		signalProcessGroup(s.cmd, syscall.SIGTERM)
		grace := time.NewTimer(stopGrace)
		defer grace.Stop()
		select {
		case <-s.done:
			return
		case <-grace.C:
			signalProcessGroup(s.cmd, syscall.SIGKILL)
		case <-ctx.Done():
			signalProcessGroup(s.cmd, syscall.SIGKILL)
		}
		// Backstop: if a wedged read keeps the child from EOF-ing, unblock it.
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			s.triggerShutdown()
			<-s.done
		}
	})
	return nil
}

func (s *Session) triggerShutdown() {
	select {
	case <-s.shutdown:
	default:
		close(s.shutdown)
	}
}

func (s *Session) closePTY() {
	s.ptmxCloseOnce.Do(func() {
		s.closedFlag.Store(true)
		if s.replyW != nil {
			_ = s.replyW.Close()
		}
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
	})
}

// ---- host frame sequencing (all *Locked helpers require s.mu) --------------

func (s *Session) allocFrameLocked(t attachwire.EventType, payload []byte) attachwire.Frame {
	seq := s.nextSeq
	s.nextSeq++
	return attachwire.Frame{
		Type:    t,
		Seq:     uint64(seq),
		RelTime: s.relTimeLocked(),
		Payload: payload,
	}
}

func (s *Session) publishLocked(f attachwire.Frame) {
	s.ring.append(f)
	for sub := range s.subs {
		sub.enqueue(f)
	}
}

func (s *Session) relTimeLocked() uint64 {
	d := time.Since(s.spawnAt).Microseconds()
	rel := uint64(0)
	if d > 0 {
		rel = uint64(d)
	}
	if rel < s.lastRel {
		rel = s.lastRel
	}
	s.lastRel = rel
	return rel
}

// lastSeqLocked is the highest host seq emitted so far (0 before any frame;
// after Exit it is the Exit seq).
func (s *Session) lastSeqLocked() attachwire.HostSeq { return s.nextSeq - 1 }

// ---- agent.InteractiveSession ----------------------------------------------

// WriteInput writes already-encoded terminal input bytes verbatim to the PTY
// master (§5: input is never re-sanitized).
func (s *Session) WriteInput(p []byte) (int, error) {
	if s.closedFlag.Load() {
		return 0, errExited
	}
	n, err := s.ptmx.Write(p)
	if err != nil {
		return n, fmt.Errorf("ptyhost: write input: %w", err)
	}
	return n, nil
}

// Resize applies geometry verbatim to the PTY (TIOCSWINSZ, §8), resizes the VT,
// and emits the applied-Resize echo frame in the host sequence (§8). cols == 0
// || rows == 0 is a framing error.
func (s *Session) Resize(cols, rows, pxWidth, pxHeight uint32) error {
	payload, err := attachwire.ResizePayload{
		Cols:     uint64(cols),
		Rows:     uint64(rows),
		PxWidth:  uint64(pxWidth),
		PxHeight: uint64(pxHeight),
	}.Encode()
	if err != nil {
		return err // FramingError (§8: cols==0 || rows==0)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return errExited
	}
	if err := s.setWinsizeLocked(uint16(cols), uint16(rows), uint16(pxWidth), uint16(pxHeight)); err != nil {
		return err
	}
	s.vt.resize(int(cols), int(rows))

	f := s.allocFrameLocked(attachwire.TypeResize, payload)
	s.publishLocked(f)
	s.rec.resize(f.RelTime, uint64(cols), uint64(rows))
	return nil
}

// Snapshot serializes the current screen (§12.1) with the host output sequence
// it reflects (atSeq), emitting nothing. After Exit it keeps returning the final
// screen with atSeq == the Exit seq (§12.2).
func (s *Session) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scr := s.buildScreenLocked()
	return scr, s.lastSeqLocked(), nil
}

func (s *Session) buildScreenLocked() attachwire.Screen {
	raw := s.vt.raw()
	return buildScreen(raw, s.epoch, s.echoMode(), s.logger)
}

// echoMode reads the PTY's termios ECHO flag (§10, §12.1) via SyscallConn so it
// is race-safe against the concurrent teardown Close and does not disturb the
// read loop's poller-managed reads (os.File.Fd would force blocking mode and
// race with Close). A closed or unreadable master reports EchoUnknown, which
// biases predictive echo to SUPPRESSED.
func (s *Session) echoMode() uint8 {
	if s.closedFlag.Load() {
		return attachwire.EchoUnknown
	}
	rc, err := s.ptmx.SyscallConn()
	if err != nil {
		return attachwire.EchoUnknown
	}
	echo := attachwire.EchoUnknown
	if cerr := rc.Control(func(fd uintptr) { echo = echoModeOfFd(fd) }); cerr != nil {
		return attachwire.EchoUnknown
	}
	return echo
}

// setWinsizeLocked applies the TIOCSWINSZ ioctl via SyscallConn so the fd access
// is race-safe against teardown Close (unlike pty.Setsize, which calls
// os.File.Fd).
func (s *Session) setWinsizeLocked(cols, rows, pxW, pxH uint16) error {
	rc, err := s.ptmx.SyscallConn()
	if err != nil {
		return fmt.Errorf("ptyhost: setsize conn: %w", err)
	}
	var ioErr error
	if cerr := rc.Control(func(fd uintptr) { ioErr = applyWinsize(fd, cols, rows, pxW, pxH) }); cerr != nil {
		return fmt.Errorf("ptyhost: setsize control: %w", cerr)
	}
	if ioErr != nil {
		return fmt.Errorf("ptyhost: setsize: %w", ioErr)
	}
	return nil
}

// EmitSnapshot produces a Snapshot frame in answer to a snapshot_request (§12).
// Before Exit it allocates the next host seq, rides the ring and every
// subscription (inStream == true). After Exit it returns a seq==0 frame with
// atSeq == the Exit seq and inStream == false (§12.2 out-of-namespace).
func (s *Session) EmitSnapshot() (attachwire.Frame, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scr := s.buildScreenLocked()
	snapBytes, err := scr.Encode()
	if err != nil {
		return attachwire.Frame{}, false, fmt.Errorf("ptyhost: snapshot encode: %w", err)
	}

	if s.exited {
		env := attachwire.SnapshotEnvelope{
			AtSeq:      uint64(s.exitSeq),
			SnapFormat: attachwire.SnapFormatScreen,
			Snap:       snapBytes,
		}
		return attachwire.Frame{
			Type:    attachwire.TypeSnapshot,
			Seq:     attachwire.PostExitSnapshotSeq,
			RelTime: 0,
			Payload: env.Encode(),
		}, false, nil
	}

	seq := s.nextSeq
	s.nextSeq++
	env := attachwire.SnapshotEnvelope{
		AtSeq:      uint64(seq - 1), // the screen reflects every frame before this one
		SnapFormat: attachwire.SnapFormatScreen,
		Snap:       snapBytes,
	}
	f := attachwire.Frame{
		Type:    attachwire.TypeSnapshot,
		Seq:     uint64(seq),
		RelTime: s.relTimeLocked(),
		Payload: env.Encode(),
	}
	s.publishLocked(f)
	return f, true, nil
}

// EmitMarker appends a seq-bearing Marker frame (§3.1) to the stream and the
// recording. Returns an error after Exit (§12.2).
func (s *Session) EmitMarker(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return fmt.Errorf("ptyhost: EmitMarker: %w", errExited)
	}
	f := s.allocFrameLocked(attachwire.TypeMarker, attachwire.MarkerPayload{Label: label}.Encode())
	s.publishLocked(f)
	s.rec.marker(f.RelTime, label)
	return nil
}

// Subscribe returns a live feed of host-produced seq-bearing frames starting at
// fromSeq+1 (§13). A ring hit replays buffered frames then continues live; an
// evicted fromSeq returns agent.ErrRingMiss; fromSeq 0 is served from the oldest
// buffered frame.
func (s *Session) Subscribe(fromSeq attachwire.HostSeq) (agent.InteractiveSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	replay, hit := s.ring.replayFrom(fromSeq)
	if !hit {
		return nil, agent.ErrRingMiss
	}
	sub := newSubscription(s, replay)
	if s.exited {
		sub.finish() // no live frames will follow; close after the replay drains
	} else {
		s.subs[sub] = struct{}{}
	}
	return sub, nil
}

func (s *Session) removeSub(sub *subscription) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
}

// Done is closed after the child has exited AND the master has been drained to
// EOF with every pending Output emitted (§12.2).
func (s *Session) Done() <-chan struct{} { return s.done }

// Exit reports the terminal Exit payload. ok is false until Done is closed.
func (s *Session) Exit() (attachwire.ExitPayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitPayload, s.exited
}

// ---- helpers ---------------------------------------------------------------

// exitPayloadFrom maps a cmd.Wait error to the §12.2 Exit payload: normal exit
// carries the code; signal death carries the signal name and exitCode = 128 +
// signum.
func exitPayloadFrom(err error) attachwire.ExitPayload {
	if err == nil {
		return attachwire.NewNormalExit(0)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				sig := ws.Signal()
				return attachwire.NewSignalExit(signalName(sig), int(sig))
			}
			return attachwire.NewNormalExit(uint64(ws.ExitStatus()))
		}
		return attachwire.NewNormalExit(uint64(ee.ExitCode()))
	}
	return attachwire.NewNormalExit(1)
}

func termEnv(env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			return kv[len("TERM="):]
		}
	}
	return "xterm-256color"
}
