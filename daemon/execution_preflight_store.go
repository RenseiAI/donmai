package daemon

import (
	"bytes"
	"context"
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
type FileExecutionPreflightStore struct {
	dir      string
	syncRoot func(*os.Root) error
}

// ExecutionPreflightReplayStore can recover the exact immutable receipt after
// an acknowledgement loss. Stores without this extension remain valid for v1
// but cannot truthfully advertise v2 registration support.
type ExecutionPreflightReplayStore interface {
	ExecutionPreflightStore
	Load(sessionID string) (json.RawMessage, error)
	BeginExecutionPreflightStart(context.Context, executioncell.PreflightRegistrationRequest, executioncell.PreflightRegistrationResponse) error
	RetireExecutionPreflight(context.Context, string) error
}

// NewFileExecutionPreflightStore constructs an append-only store rooted at dir.
func NewFileExecutionPreflightStore(dir string) *FileExecutionPreflightStore {
	return &FileExecutionPreflightStore{dir: dir, syncRoot: syncExecutionPreflightRoot}
}

func syncExecutionPreflightRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func (s *FileExecutionPreflightStore) reaffirmRoot(root *os.Root) error {
	if s.syncRoot == nil {
		return syncExecutionPreflightRoot(root)
	}
	return s.syncRoot(root)
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
	return s.reaffirmRoot(root)
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
	// Persist may have published the hard link and then failed its directory
	// fsync. Reaffirm the directory before these bytes can authorize a retry;
	// merely observing the linked inode does not establish durable publication.
	if err := s.reaffirmRoot(root); err != nil {
		return nil, fmt.Errorf("reaffirm execution preflight receipt directory: %w", err)
	}
	return bytes.Clone(raw), nil
}

type fileExecutionPreflightStartRecord struct {
	ContractVersion string                       `json:"contractVersion"`
	RequestID       string                       `json:"requestId"`
	ReceiptSHA256   string                       `json:"receiptSha256"`
	RuntimeBinding  executioncell.RuntimeBinding `json:"runtimeBinding"`
}

type fileExecutionPreflightRetirementRecord struct {
	ContractVersion string `json:"contractVersion"`
	RequestID       string `json:"requestId"`
}

func executionPreflightStartName(sessionID string) string {
	return strings.TrimSuffix(executionPreflightReceiptName(sessionID), ".json") + ".started.json"
}

func executionPreflightRetirementName(sessionID string) string {
	return strings.TrimSuffix(executionPreflightReceiptName(sessionID), ".json") + ".retired.json"
}

// BeginExecutionPreflightStart consumes the retained receipt exactly once for
// every registrar implementation. The controller ACK is necessary, but this
// host-local durable marker is what prevents a stale/replayed ACK from starting
// the same admitted claim twice.
func (s *FileExecutionPreflightStore) BeginExecutionPreflightStart(ctx context.Context, request executioncell.PreflightRegistrationRequest, response executioncell.PreflightRegistrationResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executioncell.ValidateAuthorizedPreflightRegistration(request, response); err != nil {
		return err
	}
	retained, err := s.Load(request.RuntimeBinding.RequestID)
	if err != nil {
		return err
	}
	if digestPreflightBytes(retained) != request.ReceiptSHA256 {
		return errors.New("execution preflight start receipt does not match retained bytes")
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	requestID := request.RuntimeBinding.RequestID
	if retired, checkErr := localPreflightRecordExists(root, executionPreflightRetirementName(requestID)); checkErr != nil {
		return checkErr
	} else if retired {
		return errors.New("execution preflight start authority is retired")
	}
	if started, checkErr := localPreflightRecordExists(root, executionPreflightStartName(requestID)); checkErr != nil {
		return checkErr
	} else if started {
		return errors.New("execution preflight start authority was already consumed")
	}
	record := fileExecutionPreflightStartRecord{ContractVersion: localExecutionPreflightAuthorityVersion, RequestID: requestID, ReceiptSHA256: request.ReceiptSHA256, RuntimeBinding: request.RuntimeBinding}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return durableLocalPreflightRecord(root, executionPreflightStartName(requestID), raw)
}

// RetireExecutionPreflight appends a terminal or failed-start marker so the
// retained receipt can never grant another start.
func (s *FileExecutionPreflightStore) RetireExecutionPreflight(ctx context.Context, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.Load(requestID); err != nil {
		return fmt.Errorf("retire unknown execution preflight receipt: %w", err)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	name := executionPreflightRetirementName(requestID)
	record := fileExecutionPreflightRetirementRecord{ContractVersion: localExecutionPreflightAuthorityVersion, RequestID: requestID}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return errors.New("execution preflight receipt retirement changed")
		}
		return s.reaffirmRoot(root)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return durableLocalPreflightRecord(root, name, raw)
}
