package servicepriority

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/internal/statepath"
)

// stateFile is the leaf name of the persisted choice under the brand state
// directory (~/.donmai/ by default).
const stateFile = "service-priority"

// StatePath returns the file holding the persisted mode.
func StatePath() string {
	return statepath.Resolve(stateFile, filepath.Join(os.TempDir(), ".donmai", stateFile))
}

// Load returns the persisted mode. found is false, with no error, when nothing
// has been persisted yet. A file that exists but does not hold a valid mode is
// an error: silently treating it as Default would quietly undo the operator's
// choice.
func Load() (mode Mode, found bool, err error) {
	path := StatePath()
	data, err := os.ReadFile(path) //nolint:gosec // fixed state-dir path
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("servicepriority: read %s: %w", path, err)
	}
	mode, err = Parse(string(data))
	if err != nil {
		return "", false, fmt.Errorf("servicepriority: %s: %w", path, err)
	}
	if mode == "" {
		return "", false, fmt.Errorf("servicepriority: %s is empty", path)
	}
	return mode, true, nil
}

// Save persists mode so later installs keep it. The zero Mode is rejected;
// callers persist only an explicit choice.
func Save(mode Mode) error {
	if mode == "" {
		return errors.New("servicepriority: refusing to persist an empty mode")
	}
	if err := Validate(mode); err != nil {
		return err
	}
	path := StatePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("servicepriority: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, stateFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("servicepriority: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(string(mode) + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("servicepriority: write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("servicepriority: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("servicepriority: replace %s: %w", path, err)
	}
	return nil
}
