//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

// Provenance: shim-discovery-deadline-2026-09-02.

import (
	"errors"
	"os/exec"
	"strings"
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

// TestShimLaunchProcessStopAndReapClassifiesARefusedSignal is the measured-shape
// guard for the ONE refusal a stop cannot stage from outside the kernel: the
// group's last member dies between the liveness probe and the signal, leaving a
// zombie-only group, which BSD/macOS answers EPERM rather than ESRCH.
//
// Treating that as a failed stop would make stopAbandonedShimLaunch log "may
// keep running un-adopted" about a worker that is already gone and reaped —
// precisely the false statement about the host this whole change exists to
// prevent, inverted. So the classification is by evidence (waitpid), not by
// errno: a refusal whose child turns out to be reaped is a SUCCESS, and a
// refusal whose child is still running is still a failure.
func TestShimLaunchProcessStopAndReapClassifiesARefusedSignal(t *testing.T) {
	tests := []struct {
		name string
		// killDuringSignal reproduces the race: the real group dies while the
		// kernel refuses our signal.
		killDuringSignal bool
		signalErr        error
		wantErr          bool
	}{
		{
			name:             "a refusal whose group is already gone is a successful stop",
			killDuringSignal: true,
			signalErr:        syscall.EPERM,
		},
		{
			name:      "a refusal whose group is still running is still a failure",
			signalErr: syscall.EPERM,
			wantErr:   true,
		},
		{
			name:             "ESRCH is a successful stop and still discharges the reap",
			killDuringSignal: true,
			signalErr:        syscall.ESRCH,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", "sleep 60") //nolint:gosec // G204: literal test command
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
			t.Cleanup(func() { _ = newShimLaunchProcess(started).StopAndReap() })

			process := osShimLaunchProcess{
				identity: started,
				signalGroupFn: func(syscall.Signal) error {
					if tc.killDuringSignal {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
					return tc.signalErr
				},
			}
			stopErr := process.StopAndReap()
			if tc.wantErr {
				if stopErr == nil {
					t.Fatal("a refused signal against a still-running group was reported as a successful stop")
				}
				return
			}
			if stopErr != nil {
				t.Fatalf("a refused signal against an already-gone group was reported as a failure: %v", stopErr)
			}
			assertShimChildReaped(t, pid)
		})
	}
}

// TestShimLaunchProcessStopAndReapRefusesAnUnpinnedUnprobeableLaunch pins the
// crash-matrix rule literally: the recorded leader's start identity is verified
// before any SIGTERM/SIGKILL. A launch whose start time was never pinned AND
// whose waitpid answer is unavailable has neither half of that verification, so
// signalling it would address a bare PID — the reuse-unsafe comparison
// sessionshim.ProcessIdentity exists to forbid. It must refuse rather than
// signal.
func TestShimLaunchProcessStopAndReapRefusesAnUnpinnedUnprobeableLaunch(t *testing.T) {
	signalled := false
	// PID 1 is refused by the probe's own guard (never a child of this daemon,
	// and never a legitimate stop target), so Alive() answers with an ERROR
	// rather than a disposition — which, paired with StartedAt == 0, is exactly
	// the unverifiable identity the crash-matrix rule forbids signalling.
	process := osShimLaunchProcess{
		identity:      sessionshim.ProcessIdentity{PID: 1},
		signalGroupFn: func(syscall.Signal) error { signalled = true; return nil },
	}
	err := process.StopAndReap()
	if err == nil {
		t.Fatal("an unpinned, unprobeable launch was stopped instead of refused")
	}
	if signalled {
		t.Fatal("an unpinned, unprobeable launch was signalled; the start identity must be verified first")
	}
	if !strings.Contains(err.Error(), "start identity was never pinned") {
		t.Fatalf("StopAndReap failed with %v, want the unpinned-identity refusal", err)
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
