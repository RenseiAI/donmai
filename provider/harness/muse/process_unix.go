//go:build unix

package muse

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup places the serve child in its own process
// group via Setpgid, so Stop can signal every descendant atomically.
// Borrowed from provider/harness/clijsonl (same shape, same reason:
// the host may fork helpers that inherit stdio).
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the entire process group whose
// leader is cmd.Process, falling back to the leader alone.
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
