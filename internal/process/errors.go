package process

import "errors"

// Sentinel errors returned by this package.
var (
	// ErrAlreadyRunning is returned when a process is already running.
	ErrAlreadyRunning = errors.New("process: already running")

	// ErrStalePID is returned when the PID file exists but the process is dead.
	ErrStalePID = errors.New("process: stale PID (process not running)")

	// ErrNotRunning is returned when no PID file is found.
	ErrNotRunning = errors.New("process: not running")

	// ErrUnsupported is returned on platforms where a feature is not supported.
	ErrUnsupported = errors.New("process: unsupported on this platform")
)
