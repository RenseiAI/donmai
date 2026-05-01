//go:build !windows

package codex

import (
	"os"
	"syscall"
)

// syscallSIGTERM returns the SIGTERM signal value for the current
// platform. On unix, that's syscall.SIGTERM. On windows the Provider
// falls back to os.Interrupt because windows lacks SIGTERM.
//
// macOS-only Phase F per HANDOFF (Windows is REN-1346 deferred), so
// the unix branch is the load-bearing path.
func syscallSIGTERM() os.Signal { return syscall.SIGTERM }
