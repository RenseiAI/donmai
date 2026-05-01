//go:build !unix

package claude

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup is a no-op on non-unix platforms. macOS / Linux
// are the supported targets per AGENTS.md; this stub keeps cross-compile
// builds clean.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup falls back to signalling the leader process only.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
