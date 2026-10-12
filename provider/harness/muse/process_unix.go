//go:build unix

package muse

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup places the child process in its own process
// group via Setpgid. Borrowed from clijsonl: signalling the group
// reaps helper descendants (shell wrappers, MCP servers' children)
// that inherit stdio, keeping the pipe open even after the leader
// exits.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the entire process group whose
// leader is cmd.Process. Falls back to signalling the leader alone
// when the process group could not be discovered.
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
	// Negative pid signals the process group on Unix.
	_ = syscall.Kill(-pgid, sig)
}
