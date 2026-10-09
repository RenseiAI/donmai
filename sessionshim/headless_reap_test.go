//go:build darwin || linux

package sessionshim

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// inProcessRunner is the ADR-2026-10-07 D1 Option C shape: the runner is
// hosted in the shim's own process (PID is this test process), and the
// harness it drives runs in its own process group, the way a headless driver
// starts a harness with Setpgid. Its reap proof is the harness group being
// gone; its Stop signals only that group, never its own.
type inProcessRunner struct {
	harness *exec.Cmd
	pgid    int
	done    chan struct{}

	mu       sync.Mutex
	exit     RunnerExit
	exitOK   bool
	signaled []int
	stopOnce sync.Once
}

func newInProcessRunner(t *testing.T, argv ...string) *inProcessRunner {
	t.Helper()
	//nolint:gosec // G204: fixed test-only argv, no shell input from outside the test
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start harness: %v", err)
	}
	r := &inProcessRunner{harness: cmd, pgid: cmd.Process.Pid, done: make(chan struct{})}
	t.Cleanup(func() {
		// Whatever the test left behind in the harness group goes; the
		// runner's own (this test's) group is never signaled.
		_ = syscall.Kill(-r.pgid, syscall.SIGKILL)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	})
	// The runner loop: the run ends once the runner has waited its harness.
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			r.exit, r.exitOK = RunnerExit{}, true
		case errors.As(err, &exitErr):
			status, _ := exitErr.Sys().(syscall.WaitStatus)
			if status.Signaled() {
				r.exit = RunnerExit{ExitCode: 128 + uint64(status.Signal()), Signal: status.Signal().String()} //nolint:gosec // G115: signal numbers are small and positive
			} else {
				r.exit = RunnerExit{ExitCode: uint64(exitErr.ExitCode())} //nolint:gosec // G115: exit codes are small and non-negative
			}
			r.exitOK = true
		}
		r.mu.Unlock()
		close(r.done)
	}()
	return r
}

func (r *inProcessRunner) PID() int { return os.Getpid() }

func (r *inProcessRunner) Done() <-chan struct{} { return r.done }

func (r *inProcessRunner) Exit() (RunnerExit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exit, r.exitOK
}

func (r *inProcessRunner) Stop(ctx context.Context) error {
	if r.pgid == syscall.Getpgrp() {
		return errors.New("refusing to signal the shim's own process group")
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.signaled = append(r.signaled, r.pgid)
		r.mu.Unlock()
		_ = syscall.Kill(-r.pgid, syscall.SIGKILL)
	})
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *inProcessRunner) HarnessGroupsReaped() (bool, error) { return ProcessGroupGone(r.pgid) }

func (r *inProcessRunner) signaledGroups() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.signaled...)
}

func startInProcessHeadless(t *testing.T, runner *inProcessRunner) (*Shim, *Registry, Identity) {
	t.Helper()
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-in-process", SessionID: "sess-in-process"}
	shim, err := StartHeadless(HeadlessOptions{
		Identity: id, Registry: registry, Runner: runner,
		WorkareaPath: dir + "/workarea", Orphan: settledOrphanPolicy(), ProcessEpoch: 1,
	})
	if err != nil {
		t.Fatalf("StartHeadless with an in-process runner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	return shim, registry, id
}

// TestHeadlessInProcessRunnerProvesItsHarnessGroupReaped is the Option C reap
// proof: the runner's PID is the shim's own (always alive while the tombstone
// is written), so a PID-based proof could never pass. The tombstone's
// groupReaped comes from the harness group being proved gone instead, both
// when the run ends on its own and when the shim tears it down, and the
// teardown signals only the harness group.
func TestHeadlessInProcessRunnerProvesItsHarnessGroupReaped(t *testing.T) {
	t.Parallel()

	t.Run("runner completes", func(t *testing.T) {
		t.Parallel()
		runner := newInProcessRunner(t, "/bin/sh", "-c", "sleep 0.3; exit 3")
		_, registry, id := startInProcessHeadless(t, runner)
		tomb := waitForHeadlessTombstone(t, registry, id)
		if tomb.HarnessPID != os.Getpid() {
			t.Fatalf("tombstone harness pid = %d, want the shim's own pid %d (in-process runner)", tomb.HarnessPID, os.Getpid())
		}
		if tomb.Cause != HeadlessExitCompleted || tomb.ExitCode != 3 {
			t.Fatalf("tombstone = cause %q exit %d, want completed exit 3", tomb.Cause, tomb.ExitCode)
		}
		if !tomb.GroupReaped {
			t.Fatal("tombstone does not prove the harness group reaped for an in-process runner")
		}
	})

	t.Run("shim tears the run down", func(t *testing.T) {
		t.Parallel()
		runner := newInProcessRunner(t, "/bin/sleep", "60")
		shim, registry, id := startInProcessHeadless(t, runner)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shim.Terminate(ctx); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		tomb := waitForHeadlessTombstone(t, registry, id)
		if tomb.Cause != HeadlessExitOrphaned {
			t.Fatalf("tombstone cause = %q, want orphaned", tomb.Cause)
		}
		if !tomb.GroupReaped {
			t.Fatal("tombstone does not prove the harness group reaped after the shim's teardown")
		}
		signaled := runner.signaledGroups()
		if len(signaled) != 1 || signaled[0] != runner.pgid || signaled[0] == syscall.Getpgrp() {
			t.Fatalf("teardown signaled groups %v, want only the harness group %d (never the shim's own %d)",
				signaled, runner.pgid, syscall.Getpgrp())
		}
	})
}

// TestHeadlessReapProofIsNeverAssumed pins the other direction: a run whose
// runner ended while a harness group member still lives (here a background
// child the harness leader left behind) is written with groupReaped false.
// The release rule lets a proven tombstone release a claim, so a reap that was
// not proved must never be claimed.
func TestHeadlessReapProofIsNeverAssumed(t *testing.T) {
	t.Parallel()

	runner := newInProcessRunner(t, "/bin/sh", "-c", "sleep 60 & exit 0")
	_, registry, id := startInProcessHeadless(t, runner)
	tomb := waitForHeadlessTombstone(t, registry, id)
	if tomb.Cause != HeadlessExitCompleted {
		t.Fatalf("tombstone cause = %q, want completed", tomb.Cause)
	}
	if tomb.GroupReaped {
		t.Fatal("tombstone claims the harness group reaped while a member of it is still running")
	}
	if gone, err := ProcessGroupGone(runner.pgid); err != nil || gone {
		t.Fatalf("ProcessGroupGone(harness) = (%v,%v), want the left-behind member still present", gone, err)
	}
}

// TestProcessGroupGone pins the group-gone primitive: a populated group is not
// gone, the same group after its members are killed and waited is gone, and
// the probes that would name the caller's own group or every process are
// refused rather than answered.
func TestProcessGroupGone(t *testing.T) {
	t.Parallel()

	for _, pgid := range []int{-5, 0, 1, syscall.Getpgrp()} {
		if gone, err := ProcessGroupGone(pgid); err == nil || gone {
			t.Fatalf("ProcessGroupGone(%d) = (%v,%v), want a refusal", pgid, gone, err)
		}
	}

	//nolint:gosec // G204: fixed test-only argv
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if gone, err := ProcessGroupGone(pgid); err != nil || gone {
		t.Fatalf("ProcessGroupGone(live group) = (%v,%v), want (false,nil)", gone, err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if gone, err := ProcessGroupGone(pgid); err != nil || !gone {
		t.Fatalf("ProcessGroupGone(killed and waited group) = (%v,%v), want (true,nil)", gone, err)
	}
}
