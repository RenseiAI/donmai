//go:build unix

package pi

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup places the child in its own process group so
// signalProcessGroup can signal every descendant atomically — pi may fork
// helper processes that inherit stdout, keeping the pipe open after the leader
// exits.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the whole process group led by cmd.Process,
// falling back to the leader alone when the group cannot be discovered.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		_ = cmd.Process.Signal(sig)
		return
	}
	_ = syscall.Kill(-pgid, sig)
}
