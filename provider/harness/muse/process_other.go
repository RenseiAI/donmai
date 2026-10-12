//go:build !unix

package muse

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup is a no-op on non-unix platforms.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup falls back to signalling the leader process only.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
