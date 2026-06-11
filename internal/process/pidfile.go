package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// runDirName is the directory (under the per-OS runtime/temp base, see
// runtimeBaseDir) where PID files live.
const runDirName = "donmai"

// legacyRunDirName is the pre-rename PID directory. Read falls back to
// it for one release window so an upgrade does not orphan a process
// recorded by an older binary; Write and Remove clear the legacy file so
// it cannot shadow the canonical one afterwards.
const legacyRunDirName = "agentfactory"

// PIDFile manages a PID file for tracking a running process.
type PIDFile struct {
	path       string
	legacyPath string
}

// NewPIDFile returns a PIDFile for the given process name. The file
// lives at <runtimeBaseDir>/donmai/<name>.pid — $XDG_RUNTIME_DIR when
// set (Unix), else the OS temp dir.
func NewPIDFile(name string) (*PIDFile, error) {
	base := runtimeBaseDir()
	return &PIDFile{
		path:       filepath.Join(base, runDirName, name+".pid"),
		legacyPath: filepath.Join(base, legacyRunDirName, name+".pid"),
	}, nil
}

// Path returns the absolute path to the PID file.
func (p *PIDFile) Path() string {
	return p.path
}

// Write creates the parent directory (mode 0o700) and writes pid to the file
// with mode 0o600, overwriting any existing content. The legacy-directory
// record is best-effort removed: the writer owns the name now, so a stale
// pre-rename file must not resurface through Read's fallback later.
func (p *PIDFile) Write(pid int) error {
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("process: create pid dir: %w", err)
	}
	data := []byte(strconv.Itoa(pid) + "\n")
	if err := os.WriteFile(p.path, data, 0o600); err != nil { //nolint:gosec // path is constructed programmatically
		return fmt.Errorf("process: write pid file: %w", err)
	}
	_ = os.Remove(p.legacyPath)
	return nil
}

// Read reads and validates the PID from the file, falling back to the
// legacy (pre-rename) directory when the canonical file does not exist —
// the migration window for processes started by an older binary.
// Returns ErrNotRunning if neither file exists.
// Returns ErrStalePID if the recorded process is no longer alive
// (platforms without liveness probing return the recorded PID as-is).
func (p *PIDFile) Read() (int, error) {
	pid, err := readPIDPath(p.path)
	if errors.Is(err, ErrNotRunning) {
		return readPIDPath(p.legacyPath)
	}
	return pid, err
}

// readPIDPath reads, parses, and liveness-probes a single PID file.
func readPIDPath(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed programmatically
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotRunning
		}
		return 0, fmt.Errorf("process: read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("process: invalid pid in %s: %w", path, err)
	}

	if err := probePIDAlive(pid); err != nil {
		return 0, ErrStalePID
	}

	return pid, nil
}

// Remove deletes the PID file (canonical and legacy locations). It is
// idempotent — no error is returned if the files do not exist.
func (p *PIDFile) Remove() error {
	for _, path := range []string{p.path, p.legacyPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("process: remove pid file: %w", err)
		}
	}
	return nil
}
