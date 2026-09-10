package daemon

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
)

// FileExecutionPreflightStore is an append-only, fsync-backed receipt store.
// One immutable initial receipt is allowed per session id.
type FileExecutionPreflightStore struct{ dir string }

// ExecutionPreflightReplayStore can recover the exact immutable receipt after
// an acknowledgement loss. Stores without this extension remain valid for v1
// but cannot truthfully advertise v2 registration support.
type ExecutionPreflightReplayStore interface {
	ExecutionPreflightStore
	Load(sessionID string) (json.RawMessage, error)
}

// NewFileExecutionPreflightStore constructs an append-only store rooted at dir.
func NewFileExecutionPreflightStore(dir string) *FileExecutionPreflightStore {
	return &FileExecutionPreflightStore{dir: dir}
}

// Persist fsyncs the one immutable initial receipt for sessionID.
func (s *FileExecutionPreflightStore) Persist(sessionID string, receipt json.RawMessage) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("execution preflight receipt directory is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("invalid execution preflight session id")
	}
	decoded, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return fmt.Errorf("validate execution preflight receipt: %w", err)
	}
	if decoded.RequestID != sessionID {
		return errors.New("execution preflight receipt request does not match session id")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create execution preflight receipt directory: %w", err)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("open execution preflight receipt root: %w", err)
	}
	defer func() { _ = root.Close() }()

	finalName := executionPreflightReceiptName(sessionID)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate execution preflight receipt nonce: %w", err)
	}
	pendingName := fmt.Sprintf(".%x.pending", nonce)
	file, err := root.OpenFile(pendingName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create execution preflight pending receipt: %w", err)
	}
	defer func() { _ = root.Remove(pendingName) }()
	if _, err := file.Write(receipt); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// A hard link publishes the fully fsynced inode atomically and, unlike a
	// rename, cannot replace an existing receipt or follow a destination
	// symlink. Both names are constrained by os.Root.
	if err := root.Link(pendingName, finalName); err != nil {
		return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
	}
	dir, err := root.Open(".")
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
	return nil
}

func executionPreflightReceiptName(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("%x.json", digest)
}

// Load returns the exact fsynced receipt bytes. It never recompiles or
// reserializes the receipt.
func (s *FileExecutionPreflightStore) Load(sessionID string) (json.RawMessage, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil, errors.New("execution preflight receipt directory is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("invalid execution preflight session id")
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("open execution preflight receipt root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(executionPreflightReceiptName(sessionID))
	if err != nil {
		return nil, fmt.Errorf("open execution preflight receipt: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, executioncell.MaxPreflightRegistrationReceiptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read execution preflight receipt: %w", err)
	}
	if len(raw) == 0 || len(raw) > executioncell.MaxPreflightRegistrationReceiptBytes {
		return nil, errors.New("execution preflight receipt size is invalid")
	}
	decoded, err := executioncell.DecodeHostAdaptationReceipt(raw)
	if err != nil {
		return nil, fmt.Errorf("validate stored execution preflight receipt: %w", err)
	}
	if decoded.RequestID != sessionID {
		return nil, errors.New("stored execution preflight receipt request does not match session id")
	}
	return bytes.Clone(raw), nil
}
