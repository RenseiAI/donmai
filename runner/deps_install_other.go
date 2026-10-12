//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package runner

import (
	"os"
	"os/exec"
	"os/user"
	"time"
)

// homeDir returns the daemon user's home for the install env's HOME.
func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir
	}
	return ""
}

// asExitError unwraps runErr to an *exec.ExitError when the command ran
// and exited non-zero.
func asExitError(runErr error) *exec.ExitError {
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		return exitErr
	}
	return nil
}

// setNewProcessGroup has no process-group support on this platform; the
// install runs as a plain child and the context deadline still bounds
// the leader.
func setNewProcessGroup(_ *exec.Cmd) {}

// killDepsInstallGroup is a no-op where process groups are unsupported:
// exec.CommandContext already signals the leader on expiry.
func killDepsInstallGroup(_ *exec.Cmd, _ time.Duration) {}
