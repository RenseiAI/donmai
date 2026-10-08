package ptyhost

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestSpawnClassifiesTransientRefusals pins the retry decision table behind
// Spawn: EPERM and EAGAIN are launcher-side pressure (fork/process-count and
// the setsid/controlling-terminal steps under churn), so they retry; a
// missing binary or a permission refusal is an answer about the command that
// cannot change, so it never retries. Reverting isTransientSpawnError to
// treat EPERM as permanent fails here.
func TestSpawnClassifiesTransientRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{err: syscall.EPERM, retry: true},
		{err: syscall.EAGAIN, retry: true},
		{err: syscall.ENOENT, retry: false},
		{err: syscall.EACCES, retry: false},
		{err: syscall.ENOEXEC, retry: false},
		{err: errors.New("boom"), retry: false},
	} {
		if got := isTransientSpawnError(tc.err); got != tc.retry {
			t.Errorf("isTransientSpawnError(%v) = %v, want %v", tc.err, got, tc.retry)
		}
		// The classifier must see through os/exec's wrapping: the kernel
		// refusal arrives as fork/exec <argv0>: <errno>.
		wrapped := &os.PathError{Op: "fork/exec", Path: "cmd", Err: tc.err}
		if got := isTransientSpawnError(wrapped); got != tc.retry {
			t.Errorf("isTransientSpawnError(fork/exec wrapping %v) = %v, want %v", tc.err, got, tc.retry)
		}
	}
}

// TestSpawnFailureNamesItsAttempts drives the production entry point at a
// command that can never start and asserts the failure says how many starts
// were offered. That count is the observable difference between a spawn that
// retries transient refusals and one that tries once: reverting Spawn to a
// single pty.StartWithSize call fails here because the attempt suffix is
// gone.
func TestSpawnFailureNamesItsAttempts(t *testing.T) {
	t.Parallel()

	_, err := Spawn(Spec{Command: []string{"/nonexistent-ptyhost-spawn-probe-binary"}})
	if err == nil {
		t.Fatal("Spawn of a missing binary succeeded")
	}
	if !strings.Contains(err.Error(), "ptyhost: pty start:") {
		t.Fatalf("Spawn error = %q, want the pty-start prefix", err)
	}
	if !strings.Contains(err.Error(), "attempt 1 of") {
		t.Fatalf("Spawn error = %q, want the attempt count of a spawn that does not retry permanent failures", err)
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("Spawn error = %q, want the underlying permanent errno preserved", err)
	}
}

// TestSpawnSucceedsFirstAttempt drives the production entry point at a real
// child and asserts the session comes up live: the retry path must add no
// delay, no extra process, and no behaviour change when the first start is
// accepted. The child is this test binary in a no-op role (no shell), so
// this fast-path guard carries no dependency on a shell being exec-able on
// the runner. Reverting Spawn to bypass startPTYWithRetry keeps this green
// — the retry itself is pinned by TestSpawnFailureNamesItsAttempts,
// TestSpawnClassifiesTransientRefusals and TestSpawnRetriesATransientRefusal
// — so this test guards the fast path the retry wraps, not the retry.
func TestSpawnSucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	s := mustSpawn(t, Spec{
		Command: []string{exe, "-test.run", "TestSpawnSucceedsFirstAttemptNoop"},
		Env:     []string{"PTYHOST_TEST_ROLE=noop"},
	})
	waitDone(t, s, 10*time.Second)
	if exit, ok := s.Exit(); !ok || exit.ExitCode != 0 {
		t.Fatalf("Exit = %+v ok=%v, want a clean exit from the no-op child", exit, ok)
	}
}

// TestSpawnRetriesATransientRefusal drives the retry path itself through
// the production retry core with a fault-injecting starter: attempt 1 fails
// with a transient EPERM refusal and attempt 2 starts successfully. The
// retry must build a FRESH *exec.Cmd per attempt — os/exec marks a Cmd
// started on the first Start call even when that call fails ("It is an
// error to call Start twice even if the first call did not create a
// process"), so a retry loop that reuses one Cmd would answer its own
// second attempt with "exec: already started" (not EPERM/EAGAIN, so the
// loop exits at once) and mask the transient refusal it was built to
// absorb. The attempt-2 process is reaped so no child outlives the test.
// Reverting the production change to reuse one Cmd across attempts fails
// here: the commands offered to the starter are pointer-identical and
// attempt 2 returns "exec: already started", so the retry surfaces the
// wrong error and no PTY comes up.
func TestSpawnRetriesATransientRefusal(t *testing.T) {
	t.Parallel()

	ws := &pty.Winsize{Rows: 24, Cols: 80}

	// Pin the failure mode the factory exists to avoid: a second Start on
	// the same Cmd never reaches the kernel — os/exec refuses it outright.
	dead := exec.Command("/nonexistent-ptyhost-spawn-probe-binary")
	if _, err := pty.StartWithSize(dead, ws); err == nil {
		t.Fatal("probe of a missing binary succeeded")
	}
	if _, err := pty.StartWithSize(dead, ws); err == nil {
		t.Fatal("second Start on the same Cmd succeeded")
	} else if !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Start on a reused Cmd = %q, want exec: already started", err)
	}

	// A starter that fails attempt 1 the way the observed CI refusal
	// fails — a transient EPERM fork/exec error BEFORE any process exists
	// (no marking, no fd) — then delegates to the real PTY start. On top
	// of that it records every command pointer it is offered: a retry
	// that reuses one Cmd offers the same pointer twice, which is exactly
	// the shape os/exec answers with "exec: already started".
	var calls atomic.Int64
	var first *exec.Cmd
	starter := func(cmd *exec.Cmd) (*os.File, error) {
		if calls.Add(1) == 1 {
			first = cmd
			if cmd.Process != nil {
				t.Fatalf("attempt-1 command already has a process; the refusal must precede the start")
			}
			return nil, &os.PathError{Op: "fork/exec", Path: cmd.Path, Err: syscall.EPERM}
		}
		if cmd == first {
			// The reused-Cmd shape: os/exec would answer this attempt
			// itself. Mirror that answer so the revert goes RED here
			// instead of succeeding by accident.
			return nil, errors.New("exec: already started")
		}
		return pty.StartWithSize(cmd, ws)
	}
	// The argument is a sentinel: the retry logs name the program only, so
	// it must never appear in them.
	const argvSentinel = "ptyhost-retry-argv-sentinel"
	makeCmd := func() *exec.Cmd {
		return exec.Command("true", argvSentinel)
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var started *exec.Cmd
	ptmx, err := startPTYWithStarter(makeCmd, logger, starter, func(cmd *exec.Cmd) {
		started = cmd
	})
	if err != nil {
		t.Fatalf("startPTYWithStarter: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	if got := calls.Load(); got != 2 {
		t.Fatalf("start attempts = %d, want 2 (one refused, one retried)", got)
	}
	if started == nil || started.Process == nil {
		t.Fatal("onStart did not report the started command; teardown would signal the wrong process")
	}
	if !strings.Contains(logs.String(), "refused transiently") {
		t.Fatalf("retry logs = %q, want the transient refusal reported", logs.String())
	}
	if strings.Contains(logs.String(), argvSentinel) {
		t.Fatalf("retry logs = %q, want no argv in them", logs.String())
	}
	// Reap the only real child this test started so it never outlives the
	// test's PTY close.
	_ = started.Wait()
}
