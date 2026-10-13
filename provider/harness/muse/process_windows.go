//go:build windows

package muse

import (
	"os"
	"os/exec"
)

// configureProcessGroup is a no-op on Windows: process groups are a
// Unix concept. Stop signals the leader directly.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup signals the leader process on Windows. It takes
// os.Signal because the Windows sigTERM/sigKILL shims return os.Signal.
func signalProcessGroup(cmd *exec.Cmd, sig os.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
