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

// FileExecutionPreflightStore is an fsync-backed receipt store with
// append-only semantics once a session has spawned. One initial receipt is
// published per session id together with an atomic attempt-pending marker;
// while the session never spawned (pending marker present, no spawn, start,
// or retirement marker) a redelivered attempt may atomically replace it, so a
// transient pre-spawn failure does not wedge every redelivery on the stale
// bytes. A receipt without the pending marker is legacy: it may have been
// written by a previous lifecycle that never recorded spawn evidence, so it
// stays immutable. After the spawn is committed the receipt is immutable and
// a differing republish is still refused.
type FileExecutionPreflightStore struct {
	dir      string
	syncRoot func(*os.Root) error
	// preOpenHook runs between Lstat and Open inside storedReceiptEquals.
	// It is nil in production and lets tests swap the receipt name to a
	// symlink (or a different inode) to pin the SameFile TOCTOU fence.
	preOpenHook func()
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

// ExecutionPreflightSpawnTracker is an optional ExecutionPreflightStore
// extension that records the moment a session's spawn commits. The receipt
// store uses the marker to tell a never-spawned attempt (safe to replace on
// redelivery) from a spawned one (immutable). Stores without this extension
// keep the legacy refuse-every-differing-republish behaviour.
type ExecutionPreflightSpawnTracker interface {
	ExecutionPreflightStore
	MarkExecutionPreflightSpawned(sessionID string) error
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

// executionPreflightReceiptConflictError reports a refused differing
// republish: the session already owns a receipt (or lifecycle marker) that a
// new attempt must not disturb. It unwraps to the os.ErrExist-bearing link
// error so callers keep treating it as an existence conflict, while letting
// the NACK path tell a spawned-session conflict (session defect, redelivery
// is futile) from a host-environment failure (safe to redeliver).
type executionPreflightReceiptConflictError struct {
	cause error
}

// executionPreflightReceiptLegacyConflictError reports a refused differing
// republish for a markerless legacy receipt: the store cannot prove the
// session never spawned, so it keeps the bytes immutable. It unwraps to the
// os.ErrExist-bearing link error like the spawned conflict, but the NACK
// path projects it as a host defect (safe to redeliver elsewhere) rather
// than a terminal session defect.
type executionPreflightReceiptLegacyConflictError struct {
	cause error
}

func (e *executionPreflightReceiptConflictError) Error() string {
	if e == nil || e.cause == nil {
		return "publish immutable execution preflight receipt: file exists"
	}
	return "publish immutable execution preflight receipt: " + e.cause.Error()
}

func (e *executionPreflightReceiptConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *executionPreflightReceiptLegacyConflictError) Error() string {
	if e == nil || e.cause == nil {
		return "publish immutable execution preflight receipt: file exists"
	}
	return "publish immutable execution preflight receipt: " + e.cause.Error()
}

func (e *executionPreflightReceiptLegacyConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Persist fsyncs the one immutable initial receipt for sessionID.
//
// Persist is idempotent for identical bytes: when the session already has a
// receipt whose content is byte-identical to receipt, it reaffirms the
// directory and returns nil, so a redelivered claim that compiles the same
// receipt can be accepted again. A different receipt for the same session
// whose spawn was already committed (or whose start authority was consumed)
// is still refused, and that error wraps os.ErrExist. A different receipt
// for a session that carries the attempt-pending marker and never spawned
// is atomically published over the stale one, so a transient pre-spawn
// failure does not wedge every redelivery. A different receipt for a
// markerless legacy receipt is refused without touching the stored bytes.
func (s *FileExecutionPreflightStore) Persist(sessionID string, receipt json.RawMessage) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("execution preflight receipt directory is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("invalid execution preflight session id")
	}
	if len(receipt) == 0 || len(receipt) > executioncell.MaxPreflightRegistrationReceiptBytes {
		return errors.New("execution preflight receipt size is invalid")
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
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
		}
		if storedReceiptEquals(root, finalName, receipt, s.preOpenHook) {
			return s.reaffirmRoot(root)
		}
		if replaceErr := s.replaceUnspawnedReceipt(root, sessionID, finalName, pendingName, err); replaceErr != nil {
			return replaceErr
		}
		return s.reaffirmRoot(root)
	}
	if err := s.ensureExecutionPreflightAttemptPending(root, sessionID); err != nil {
		return err
	}
	return s.reaffirmRoot(root)
}

// replaceUnspawnedReceipt atomically publishes the fully fsynced pending
// file over an existing differing receipt for a session that carries the
// attempt-pending marker and never spawned. The rename replaces the
// directory entry and never follows a destination symlink, so a name swapped
// to a link between the checks is replaced, never traversed: nothing outside
// the root is ever written through. A pre-created symlink or non-regular
// incumbent is still refused without touching it, and any spawn, start, or
// retirement marker keeps the receipt immutable. A receipt without the
// attempt-pending marker is legacy and stays immutable: the store cannot
// prove it never spawned.
func (s *FileExecutionPreflightStore) replaceUnspawnedReceipt(root *os.Root, sessionID, finalName, pendingName string, linkErr error) error {
	conflict := func() error {
		return &executionPreflightReceiptConflictError{cause: linkErr}
	}
	legacyConflict := func() error {
		return &executionPreflightReceiptLegacyConflictError{cause: linkErr}
	}
	pendingInfo, err := root.Lstat(executionPreflightAttemptPendingName(sessionID))
	if err != nil || !pendingInfo.Mode().IsRegular() {
		return legacyConflict()
	}
	for _, marker := range []string{
		executionPreflightSpawnName(sessionID),
		executionPreflightStartName(sessionID),
		executionPreflightRetirementName(sessionID),
	} {
		exists, err := localPreflightRecordExists(root, marker)
		if err != nil {
			return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
		}
		if exists {
			return conflict()
		}
	}
	info, err := root.Lstat(finalName)
	if err != nil || !info.Mode().IsRegular() {
		return conflict()
	}
	if s.preOpenHook != nil {
		s.preOpenHook()
	}
	if err := root.Rename(pendingName, finalName); err != nil {
		return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
	}
	return nil
}

// storedReceiptEquals reports whether name is a regular file inside root whose
// content is exactly want. A symlink, a non-regular file, a size mismatch, or
// any read error reports false, so the caller keeps refusing. The open
// descriptor is re-statted and required to be the same regular file the Lstat
// observed, closing the Lstat-to-Open swap where the name is replaced with a
// symlink (or a different inode) between the two calls. The preOpen hook runs
// between Lstat and Open; it is nil in production and exists only so tests
// can deterministically drive that swap window.
func storedReceiptEquals(root *os.Root, name string, want []byte, preOpen func()) bool {
	if len(want) == 0 || len(want) > executioncell.MaxPreflightRegistrationReceiptBytes {
		return false
	}
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(want)) {
		return false
	}
	if preOpen != nil {
		preOpen()
	}
	file, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	finfo, err := file.Stat()
	if err != nil || !finfo.Mode().IsRegular() || finfo.Size() != int64(len(want)) || !os.SameFile(info, finfo) {
		return false
	}
	got, err := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
	if err != nil {
		return false
	}
	return bytes.Equal(got, want)
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

// executionPreflightSpawnName is the post-commit spawn marker. It is written
// exactly once, after the spawner accepts the session, and gates receipt
// replacement: a receipt with this marker belongs to a live (or lived)
// worker and stays immutable. A receipt without the attempt-pending marker
// is legacy and stays immutable even without this marker; only a receipt
// carrying the pending marker and none of the terminal markers may be
// replaced by a redelivered attempt.
func executionPreflightSpawnName(sessionID string) string {
	return strings.TrimSuffix(executionPreflightReceiptName(sessionID), ".json") + ".spawned.json"
}

// executionPreflightAttemptPendingName is the affirmative replaceability
// marker for the new lifecycle. It is published atomically inside the store
// root alongside the first receipt and never follows symlinks. Only a
// receipt carrying this regular-file marker, with no spawn, start, or
// retirement marker, may be replaced. A receipt without it is legacy and
// stays immutable.
func executionPreflightAttemptPendingName(sessionID string) string {
	return strings.TrimSuffix(executionPreflightReceiptName(sessionID), ".json") + ".attempt-pending.json"
}

type fileExecutionPreflightAttemptPendingRecord struct {
	ContractVersion string `json:"contractVersion"`
	RequestID       string `json:"requestId"`
}

const fileExecutionPreflightAttemptPendingVersion = "execution-preflight-attempt-pending/v1"

// ensureExecutionPreflightAttemptPending publishes the attempt-pending marker
// for sessionID when absent. The marker is created through a temp file plus
// hard link inside the store root, so creation is atomic and never follows a
// destination symlink. An existing regular-file marker is idempotent; a
// symlink or non-regular incumbent fails closed without being touched.
func (s *FileExecutionPreflightStore) ensureExecutionPreflightAttemptPending(root *os.Root, sessionID string) error {
	name := executionPreflightAttemptPendingName(sessionID)
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return &executionPreflightReceiptLegacyConflictError{cause: os.ErrExist}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
	}
	raw, err := json.Marshal(fileExecutionPreflightAttemptPendingRecord{ContractVersion: fileExecutionPreflightAttemptPendingVersion, RequestID: sessionID})
	if err != nil {
		return err
	}
	if err := durableLocalPreflightRecord(root, name, raw); err != nil {
		if errors.Is(err, os.ErrExist) {
			if info, statErr := root.Lstat(name); statErr == nil && info.Mode().IsRegular() {
				return nil
			}
			return &executionPreflightReceiptLegacyConflictError{cause: err}
		}
		return fmt.Errorf("publish immutable execution preflight receipt: %w", err)
	}
	return nil
}

type fileExecutionPreflightSpawnRecord struct {
	ContractVersion string `json:"contractVersion"`
	RequestID       string `json:"requestId"`
}

const fileExecutionPreflightSpawnVersion = "execution-preflight-spawn/v1"

// MarkExecutionPreflightSpawned records that the session's spawn committed.
// The marker is append-only and idempotent: remarking the same session
// reaffirms the directory and returns nil. It never alters the receipt.
func (s *FileExecutionPreflightStore) MarkExecutionPreflightSpawned(sessionID string) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("execution preflight receipt directory is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("invalid execution preflight session id")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create execution preflight receipt directory: %w", err)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("open execution preflight receipt root: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := executionPreflightSpawnName(sessionID)
	raw, err := json.Marshal(fileExecutionPreflightSpawnRecord{ContractVersion: fileExecutionPreflightSpawnVersion, RequestID: sessionID})
	if err != nil {
		return err
	}
	if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return errors.New("execution preflight spawn marker changed")
		}
		return s.reaffirmRoot(root)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := durableLocalPreflightRecord(root, name, raw); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil && bytes.Equal(existing, raw) {
				return s.reaffirmRoot(root)
			}
			return errors.New("execution preflight spawn marker changed")
		}
		return err
	}
	return s.reaffirmRoot(root)
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
