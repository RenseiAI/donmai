//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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
	pgid, err := syscall.Getpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		return errSessionProcessExited
	}
	if err != nil {
		return fmt.Errorf("resolve process group: %w", err)
	}
	// Setpgid above establishes pgid == child pid. Requiring that invariant,
	// and rejecting our own group, prevents accidental sibling/self kills if a
	// future spawn path stops creating a dedicated group.
	selfPGID, selfErr := syscall.Getpgid(os.Getpid())
	if pgid != pid || (selfErr == nil && pgid == selfPGID) {
		return fmt.Errorf("refusing unsafe process group %d for child %d", pgid, pid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
		return errSessionProcessExited
	} else if err != nil {
		return err
	}
	return nil
}
