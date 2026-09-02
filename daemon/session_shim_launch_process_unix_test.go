//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

// Provenance: shim-discovery-deadline-2026-09-02.

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// assertShimChildReaped proves pid left no defunct entry behind.
//
// A liveness probe is NOT this proof: a zombie is still in the process table, so
// kill(pid, 0) succeeds and even the OS-reported start time still reads back. The
// only observation that separates "exited and reaped" from "exited and defunct"
// is waitpid itself — ECHILD means this process has no such child left to reap,
// which is exactly what the measured incident did not have.
func assertShimChildReaped(t *testing.T, pid int) {
	t.Helper()
	var status syscall.WaitStatus
	// One retry window: the reap is synchronous on the abandon path, but a child
	// that exited on its own can take a moment to be reported.
	deadline := time.Now().Add(2 * time.Second)
	for {
		wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return // already reaped: no defunct entry exists
		case err == nil && wpid == pid:
			t.Fatalf("pid %d was left defunct: this test had to reap it (status %v)", pid, status)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pid %d is still an unreaped child of this process (last waitpid: %d, %v)", pid, wpid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestShimLaunchProcessAliveReapsAnExitedChild pins the reap-aware liveness
// probe against a REAL child process: an exited child reads as not alive AND is
// reaped by the probe that observed it, so no abandon path can report "gone"
// while leaving a defunct entry behind.
func TestShimLaunchProcessAliveReapsAnExitedChild(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		wantAlive bool
	}{
		{name: "a running child is alive", command: []string{"/bin/sh", "-c", "sleep 30"}, wantAlive: true},
		{name: "an exited child is not alive", command: []string{"/bin/sh", "-c", "exit 0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(tc.command[0], tc.command[1:]...) //nolint:gosec // G204: literal test command
			configureShimProcess(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			pid := cmd.Process.Pid
			started, err := sessionshim.ProcessIdentityFor(pid)
			if err != nil {
				t.Fatalf("ProcessIdentityFor: %v", err)
			}
			// Release, exactly as startShimProcess does: from here on this test
			// process is the child's parent but not its waiter, which is the
			// state the defunct entry was measured in.
			if err := cmd.Process.Release(); err != nil {
				t.Fatalf("Release: %v", err)
			}
			process := newShimLaunchProcess(started)
			t.Cleanup(func() { _ = process.StopAndReap() })

			if !tc.wantAlive {
				// Give the child time to actually exit before probing.
				deadline := time.Now().Add(5 * time.Second)
				for {
					alive, aliveErr := process.Alive()
					if aliveErr != nil {
						t.Fatalf("Alive: %v", aliveErr)
					}
					if !alive {
						break
					}
					if !time.Now().Before(deadline) {
						t.Fatal("a child that exited never read as not alive")
					}
					time.Sleep(10 * time.Millisecond)
				}
				assertShimChildReaped(t, pid)
				return
			}
			alive, aliveErr := process.Alive()
			if aliveErr != nil {
				t.Fatalf("Alive: %v", aliveErr)
			}
			if !alive {
				t.Fatal("a running child read as not alive")
			}
		})
	}
}

// TestShimLaunchProcessStopAndReapEndsTheGroup pins the stop half against a real
// process group: a launched worker with a child of its own is terminated and
// reaped, and a second stop of an already-gone process is not an error.
func TestShimLaunchProcessStopAndReapEndsTheGroup(t *testing.T) {
	// The outer shell starts a child that ignores nothing and outlives its
	// parent unless the whole GROUP is signalled — which is why the stop
	// addresses -pid rather than pid.
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & sleep 60") //nolint:gosec // G204: literal test command
	configureShimProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	started, err := sessionshim.ProcessIdentityFor(pid)
	if err != nil {
		t.Fatalf("ProcessIdentityFor: %v", err)
	}
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	process := newShimLaunchProcess(started)

	if err := process.StopAndReap(); err != nil {
		t.Fatalf("StopAndReap: %v", err)
	}
	// The IDENTITY probe, deliberately, not process.Alive(): the identity probe
	// does not reap, and a defunct child still reads as alive through it (the
	// process table entry survives until someone waits). It is therefore the only
	// assertion here that can tell "stopped and reaped" from "stopped and left
	// defunct" — process.Alive() would reap the zombie itself and report the
	// answer this test is trying to earn.
	if alive, aliveErr := started.Alive(); alive || aliveErr != nil {
		t.Fatalf("the stopped process is still alive or defunct (err: %v)", aliveErr)
	}
	assertShimChildReaped(t, pid)
	if groupErr := syscall.Kill(-pid, 0); !errors.Is(groupErr, syscall.ESRCH) {
		t.Fatalf("the process group survived the stop: kill(-%d, 0) = %v", pid, groupErr)
	}
	// Idempotent: a second stop of an already-reaped process is a no-op, not an
	// error, because every abandon path may run it after the process died on its
	// own.
	if err := process.StopAndReap(); err != nil {
		t.Fatalf("a second StopAndReap reported an error: %v", err)
	}
}
