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

func configureSessionProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killSessionProcessGroup(cmd *exec.Cmd) error {
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
	if err := syscall.Kill(-pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
		return errSessionProcessExited
	} else if err != nil {
		return err
	}
	return nil
}

// waitSessionProcessGroup waits until the daemon-owned process group no
// longer exists. The direct child may already be reaped while a descendant is
// still alive, so cmd.Wait alone is not a terminal ownership boundary.
func waitSessionProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return
	}
	pgid := cmd.Process.Pid
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
