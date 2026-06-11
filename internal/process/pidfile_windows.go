//go:build windows

package process

import "os"

// runtimeBaseDir returns the base directory PID files live under.
// Windows has no XDG runtime dir; the OS temp dir is used.
func runtimeBaseDir() string {
	return os.TempDir()
}

// probePIDAlive is a no-op on Windows — stale detection is not
// performed and the recorded PID is returned as-is.
func probePIDAlive(int) error {
	return nil
}
