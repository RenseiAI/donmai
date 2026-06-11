//go:build !windows

package process

import (
	"os"
	"syscall"
)

// runtimeBaseDir returns the per-user base directory PID files live
// under: $XDG_RUNTIME_DIR when set, else the OS temp dir.
func runtimeBaseDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg
	}
	return os.TempDir()
}

// probePIDAlive reports whether the process is alive via signal 0.
func probePIDAlive(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.Signal(0))
}
