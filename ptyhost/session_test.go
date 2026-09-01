package ptyhost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// ---- shared test helpers ---------------------------------------------------

func mustSpawn(t *testing.T, spec Spec) *Session {
	t.Helper()
	s, err := Spawn(spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

func waitDone(t *testing.T, s *Session, d time.Duration) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(d):
		t.Fatalf("session did not finish within %v", d)
	}
}

// nextFrameOfType drains a subscription until a frame of the given type arrives
// or the deadline elapses.
func nextFrameOfType(t *testing.T, sub agent.InteractiveSubscription, typ attachwire.EventType, d time.Duration) attachwire.Frame {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatalf("subscription closed before a %s frame arrived", typ)
			}
			if f.Type == typ {
				return f
			}
		case <-deadline:
			t.Fatalf("no %s frame within %v", typ, d)
		}
	}
}

// collectOutput reads Output-frame payloads from a channel for up to d and
// returns them concatenated.
func collectOutput(frames <-chan attachwire.Frame, d time.Duration) []byte {
	var out []byte
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			if f.Type == attachwire.TypeOutput {
				out = append(out, attachwire.DecodeOutput(f.Payload).Data...)
			}
		case <-deadline:
			return out
		}
	}
}

// ---- tests -----------------------------------------------------------------

// TestCPRConformance is the spec §12 / Appendix A conformance fixture: a real
// child under the PTY emits CSI 6n (and CSI c) and blocks reading the reply; the
// host VT must answer a correct CPR on the master. This runs as a REAL PTY spawn.
func TestCPRConformance(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	s := mustSpawn(t, Spec{
		Command: []string{exe},
		Env:     []string{"PTYHOST_TEST_ROLE=cpr"},
	})
	waitDone(t, s, 15*time.Second)
	exit, ok := s.Exit()
	if !ok {
		t.Fatal("Exit not reported")
	}
	if exit.ExitCode != 0 {
		t.Fatalf("CPR child exited code=%d signal=%q (non-zero = the host VT did not answer CPR/DA correctly on the master)", exit.ExitCode, exit.Signal)
	}
}

// TestExitOrdering: every Output is delivered before Exit, Exit is the final
// (max-seq) frame, and the exit code is captured (§12.2).
func TestExitOrdering(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", "printf hello; exit 3"}})
	waitDone(t, s, 10*time.Second)

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	var frames []attachwire.Frame
	for f := range sub.Frames() {
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		t.Fatal("no frames")
	}

	sawHello := false
	exitIdx := -1
	var maxSeq uint64
	for i, f := range frames {
		if f.Seq > maxSeq {
			maxSeq = f.Seq
		}
		switch f.Type {
		case attachwire.TypeOutput:
			if exitIdx >= 0 {
				t.Error("Output frame delivered after Exit")
			}
			if bytes.Contains(attachwire.DecodeOutput(f.Payload).Data, []byte("hello")) {
				sawHello = true
			}
		case attachwire.TypeExit:
			exitIdx = i
		}
	}
	if !sawHello {
		t.Error("hello output never seen")
	}
	if exitIdx != len(frames)-1 {
		t.Errorf("Exit at index %d, want last (%d)", exitIdx, len(frames)-1)
	}
	if frames[exitIdx].Seq != maxSeq {
		t.Errorf("Exit seq %d is not the max seq %d", frames[exitIdx].Seq, maxSeq)
	}

	exit, ok := s.Exit()
	if !ok || exit.ExitCode != 3 || exit.BySignal() {
		t.Errorf("Exit = %+v ok=%v, want code 3 normal exit", exit, ok)
	}
}

// TestSignalDeath: a process killed by SIGKILL reports exitCode 137 and signal
// name "SIGKILL" (§12.2 128+signum convention).
func TestSignalDeath(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}})
	// Give the child a moment to start, then SIGKILL its process group directly.
	time.Sleep(200 * time.Millisecond)
	signalProcessGroup(s.cmd, syscall.SIGKILL)
	waitDone(t, s, 10*time.Second)

	exit, ok := s.Exit()
	if !ok {
		t.Fatal("Exit not reported")
	}
	if !exit.BySignal() || exit.Signal != "SIGKILL" || exit.ExitCode != 137 {
		t.Errorf("Exit = %+v, want signal SIGKILL / code 137", exit)
	}
}

// TestStopTerminates: Stop drives the child to a signal Exit and closes Done.
func TestStopTerminates(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force immediate SIGKILL escalation
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitDone(t, s, 10*time.Second)
	if exit, ok := s.Exit(); !ok || !exit.BySignal() {
		t.Errorf("Exit = %+v ok=%v, want a signal exit after Stop", exit, ok)
	}
}

// TestPostExitBehavior: after Exit, EmitSnapshot returns seq=0 / atSeq=exitSeq /
// inStream=false and EmitMarker errors (§12.2).
func TestPostExitBehavior(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", "printf hi"}})
	waitDone(t, s, 10*time.Second)

	s.mu.Lock()
	exitSeq := s.exitSeq
	s.mu.Unlock()

	f, inStream, err := s.EmitSnapshot()
	if err != nil {
		t.Fatalf("EmitSnapshot post-Exit: %v", err)
	}
	if inStream {
		t.Error("post-Exit EmitSnapshot inStream should be false")
	}
	if f.Seq != attachwire.PostExitSnapshotSeq {
		t.Errorf("post-Exit snapshot header seq = %d, want 0", f.Seq)
	}
	if f.RelTime != 0 {
		t.Errorf("post-Exit snapshot rel_time = %d, want 0 (out-of-namespace)", f.RelTime)
	}
	env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
	if err != nil {
		t.Fatalf("decode snapshot envelope: %v", err)
	}
	if env.AtSeq != uint64(exitSeq) {
		t.Errorf("post-Exit snapshot atSeq = %d, want exit seq %d", env.AtSeq, exitSeq)
	}
	if _, err := attachwire.DecodeScreen(env.Snap); err != nil {
		t.Errorf("post-Exit snapshot is not escape-safe: %v", err)
	}

	if err := s.EmitMarker("late"); !errors.Is(err, errExited) {
		t.Errorf("EmitMarker post-Exit err = %v, want errExited", err)
	}
}

// TestWriteInputReachesChild: WriteInput is written verbatim to the PTY and the
// child's response appears in the output stream.
func TestWriteInputReachesChild(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", "read x; printf 'ECHO=%s' \"$x\""}})
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := s.WriteInput([]byte("ping\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	out := collectOutput(sub.Frames(), 5*time.Second)
	if !bytes.Contains(out, []byte("ECHO=ping")) {
		t.Errorf("output %q does not contain ECHO=ping", out)
	}
}

// TestWriteAttributedInputSystemPacingAndHumanBypass proves the real
// Session wiring end to end: a human (non-system) userID never triggers
// systemInputPacingGap even for a bare CR right after another write — "do
// not delay human input" — while the shared SYSTEM sentinel
// (attachwire.SystemNudgeUserID) does, landing its CR at least
// systemInputPacingGap after the write before it. The isolated
// segment-by-segment behavior is exhaustively covered against a fake writer
// in systeminput_test.go; this pins the one real *os.File write path that
// wiring reaches.
func TestWriteAttributedInputSystemPacingAndHumanBypass(t *testing.T) {
	orig := systemInputPacingGap
	systemInputPacingGap = 30 * time.Millisecond
	t.Cleanup(func() { systemInputPacingGap = orig })

	s := mustSpawn(t, Spec{Command: []string{"cat"}})

	if _, err := s.WriteAttributedInput([]byte("user_01hz3k9xyz"), []byte("x")); err != nil {
		t.Fatalf("WriteAttributedInput(human text): %v", err)
	}
	start := time.Now()
	if _, err := s.WriteAttributedInput([]byte("user_01hz3k9xyz"), []byte("\r")); err != nil {
		t.Fatalf("WriteAttributedInput(human CR): %v", err)
	}
	if since := time.Since(start); since >= systemInputPacingGap {
		t.Errorf("human CR waited %v, want < %v (human input must never be delayed)", since, systemInputPacingGap)
	}

	if _, err := s.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("y")); err != nil {
		t.Fatalf("WriteAttributedInput(system text): %v", err)
	}
	start = time.Now()
	if _, err := s.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("\r")); err != nil {
		t.Fatalf("WriteAttributedInput(system CR): %v", err)
	}
	if since := time.Since(start); since < systemInputPacingGap {
		t.Errorf("system-attributed CR waited only %v, want >= %v", since, systemInputPacingGap)
	}
}

// TestResizeEchoAndFraming: an applied resize is echoed as a seq-bearing Resize
// frame (§8) carrying the exact geometry, and a zero-dimension resize is a
// framing error.
func TestResizeEchoAndFraming(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}})
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := s.Resize(100, 40, 8, 16); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	f := nextFrameOfType(t, sub, attachwire.TypeResize, 5*time.Second)
	if f.Seq == 0 {
		t.Error("applied-resize echo must be seq-bearing (§8), got header seq 0")
	}
	rp, err := attachwire.DecodeResize(f.Payload)
	if err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if rp.Cols != 100 || rp.Rows != 40 {
		t.Errorf("resize echo = %dx%d, want 100x40", rp.Cols, rp.Rows)
	}

	if err := s.Resize(0, 40, 0, 0); !attachwire.IsFramingErr(err) {
		t.Errorf("Resize(0,40) err = %v, want a framing error", err)
	}
	if err := s.Resize(100, 0, 0, 0); !attachwire.IsFramingErr(err) {
		t.Errorf("Resize(100,0) err = %v, want a framing error", err)
	}
}

// TestEmitMarkerAndSnapshotLive: a Marker and a live EmitSnapshot both ride the
// subscription in-sequence (§3.1, §12).
func TestEmitMarkerAndSnapshotLive(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}})
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := s.EmitMarker(agent.MarkerApprovalPending); err != nil {
		t.Fatalf("EmitMarker: %v", err)
	}
	mf := nextFrameOfType(t, sub, attachwire.TypeMarker, 5*time.Second)
	mp, err := attachwire.DecodeMarker(mf.Payload)
	if err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if mp.Label != agent.MarkerApprovalPending {
		t.Errorf("marker label = %q, want %q", mp.Label, agent.MarkerApprovalPending)
	}

	frame, inStream, err := s.EmitSnapshot()
	if err != nil {
		t.Fatalf("EmitSnapshot: %v", err)
	}
	if !inStream {
		t.Error("live EmitSnapshot inStream should be true")
	}
	if frame.Seq == 0 {
		t.Error("live snapshot must be seq-bearing")
	}
	sf := nextFrameOfType(t, sub, attachwire.TypeSnapshot, 5*time.Second)
	if sf.Seq != frame.Seq {
		t.Errorf("snapshot on subscription seq %d != emitted seq %d", sf.Seq, frame.Seq)
	}
}
