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
//
// configureProcessGroup sets Setpgid, so the group id is the leader's
// pid. Signal -pid directly instead of looking the group up: once the
// leader has been reaped, Getpgid fails while the helpers it forked
// still live in the group.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid && pid > 0 {
		if err := syscall.Kill(-pid, sig); err == nil {
			return
		}
	}
	_ = cmd.Process.Signal(sig)
}
