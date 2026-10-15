//go:build !unix && !windows

package muse

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup is a no-op on platforms without process
// groups. Stop signals the leader directly.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup signals the leader process.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
