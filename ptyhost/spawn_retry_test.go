package ptyhost

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
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
// accepted. Reverting Spawn to bypass startPTYWithRetry keeps this green —
// the retry itself is pinned by TestSpawnFailureNamesItsAttempts and
// TestSpawnClassifiesTransientRefusals — so this test guards the fast path
// the retry wraps, not the retry.
func TestSpawnSucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", "exit 0"}})
	waitDone(t, s, 10*time.Second)
}
