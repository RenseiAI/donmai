//go:build !windows

package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PIDFile manages a PID file for tracking a running process.
type PIDFile struct {
	path string
}

// NewPIDFile returns a PIDFile for the given process name.
// It prefers $XDG_RUNTIME_DIR/agentfactory/<name>.pid and falls back to
// os.TempDir()/agentfactory/<name>.pid.
func NewPIDFile(name string) (*PIDFile, error) {
	var dir string
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dir = filepath.Join(xdg, "agentfactory")
	} else {
		dir = filepath.Join(os.TempDir(), "agentfactory")
	}
	return &PIDFile{path: filepath.Join(dir, name+".pid")}, nil
}

// Path returns the absolute path to the PID file.
func (p *PIDFile) Path() string {
	return p.path
}

// Write creates the parent directory (mode 0o700) and writes pid to the file
// with mode 0o600, overwriting any existing content.
func (p *PIDFile) Write(pid int) error {
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("process: create pid dir: %w", err)
	}
	data := []byte(strconv.Itoa(pid) + "\n")
	if err := os.WriteFile(p.path, data, 0o600); err != nil { //nolint:gosec // path is constructed programmatically
		return fmt.Errorf("process: write pid file: %w", err)
	}
	return nil
}

// Read reads and validates the PID from the file.
// Returns ErrNotRunning if the file does not exist.
// Returns ErrStalePID if the recorded process is no longer alive.
func (p *PIDFile) Read() (int, error) {
	data, err := os.ReadFile(p.path) //nolint:gosec // path is constructed programmatically
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotRunning
		}
		return 0, fmt.Errorf("process: read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("process: invalid pid in %s: %w", p.path, err)
	}

	// Probe liveness via signal 0.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, ErrStalePID
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, ErrStalePID
	}

	return pid, nil
}

// Remove deletes the PID file. It is idempotent — no error is returned if the
// file does not exist.
func (p *PIDFile) Remove() error {
	if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("process: remove pid file: %w", err)
	}
	return nil
}
