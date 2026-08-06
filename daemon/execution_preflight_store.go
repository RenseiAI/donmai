package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileExecutionPreflightStore is an append-only, fsync-backed receipt store.
// One immutable initial receipt is allowed per session id.
type FileExecutionPreflightStore struct{ dir string }

// NewFileExecutionPreflightStore constructs an append-only store rooted at dir.
func NewFileExecutionPreflightStore(dir string) *FileExecutionPreflightStore {
	return &FileExecutionPreflightStore{dir: dir}
}

// Persist fsyncs the one immutable initial receipt for sessionID.
func (s *FileExecutionPreflightStore) Persist(sessionID string, receipt json.RawMessage) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("execution preflight receipt directory is required")
	}
	if strings.TrimSpace(sessionID) == "" || filepath.Base(sessionID) != sessionID {
		return errors.New("invalid execution preflight session id")
	}
	if !json.Valid(receipt) {
		return errors.New("execution preflight receipt is not valid JSON")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create execution preflight receipt directory: %w", err)
	}
	path := filepath.Join(s.dir, sessionID+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // sessionID is a validated basename under the configured store root
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(receipt); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	written = true
	return nil
}
