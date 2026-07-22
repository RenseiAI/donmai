//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var errSessionProcessExited = errors.New("session process already exited")

// sessionTerminationGrace is the bounded cooperative window given to a worker
// process group after SIGTERM. It lets a worker flush its final state while
// still guaranteeing that a TERM-ignoring tree cannot hold a session slot
// indefinitely.
const sessionTerminationGrace = 250 * time.Millisecond

// processGroupPostWaitGrace bounds the defensive group-disappearance check
// after cmd.Wait has reaped the direct child. A platform can report EPERM for
// an unobservable/zombie-only group forever; that must not retain a terminal
// SessionID or capacity forever.
const processGroupPostWaitGrace = 2 * time.Second

type processGroupWaitResult string

const (
	processGroupGone       processGroupWaitResult = "gone"
	processGroupTimedOut   processGroupWaitResult = "timeout"
	processGroupPermission processGroupWaitResult = "permission-denied"
	processGroupUnknown    processGroupWaitResult = "unknown"
)

func configureSessionProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func signalSessionProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return fmt.Errorf("session process is unavailable")
	}
	pid := cmd.Process.Pid
	if pid == os.Getpid() {
		return fmt.Errorf("refusing to signal daemon process")
	}
	// Setpgid above establishes pgid == child pid. The group leader can have
	// exited before a descendant, in which case Getpgid(pid) returns ESRCH even
	// though signaling -pid is still required. Validate a live leader when one
	// exists, but always address the known dedicated group id.
	pgid, err := syscall.Getpgid(pid)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("resolve process group: %w", err)
	}
	selfPGID, selfErr := syscall.Getpgid(os.Getpid())
	if (err == nil && pgid != pid) || (selfErr == nil && pid == selfPGID) {
		return fmt.Errorf("refusing unsafe process group %d for child %d", pgid, pid)
	}
	if err := syscall.Kill(-pid, signal); errors.Is(err, syscall.ESRCH) {
		return errSessionProcessExited
	} else if err != nil {
		return err
	}
	return nil
}

func terminateSessionProcessGroup(cmd *exec.Cmd) error {
	return signalSessionProcessGroup(cmd, syscall.SIGTERM)
}

func killSessionProcessGroup(cmd *exec.Cmd) error {
	return signalSessionProcessGroup(cmd, syscall.SIGKILL)
}

// waitSessionProcessGroup waits at most maxWait for the daemon-owned process
// group to disappear. cmd.Wait only proves the direct child is reaped; callers
// use this result to distinguish a real surviving tree from an unobservable
// EPERM/zombie-only group and can log a truthful classification instead of
// looping forever while retaining capacity.
func waitSessionProcessGroup(cmd *exec.Cmd, maxWait time.Duration) processGroupWaitResult {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return processGroupUnknown
	}
	pgid := cmd.Process.Pid
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	permissionDenied := false
	for {
		err := syscall.Kill(-pgid, 0)
		switch {
		case errors.Is(err, syscall.ESRCH):
			return processGroupGone
		case errors.Is(err, syscall.EPERM):
			permissionDenied = true
		case err != nil:
			return processGroupUnknown
		}
		select {
		case <-deadline.C:
			if permissionDenied {
				return processGroupPermission
			}
			return processGroupTimedOut
		case <-ticker.C:
		}
	}
}
