//go:build windows

package muse

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup is a no-op on Windows: process groups are a
// Unix concept. Stop signals the leader directly.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup signals the leader process on Windows.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
