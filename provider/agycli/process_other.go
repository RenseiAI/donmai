//go:build !unix

package agycli

import (
	"os/exec"
	"syscall"
)

// signalProcessGroup falls back to signalling the leader process only on
// non-unix platforms. macOS / Linux are the supported targets per AGENTS.md;
// this stub keeps cross-compile builds clean.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
