// Package localruntimeauth owns private, local-only credentials for one
// queue-bound daemon authority. It does not decide attempt lifecycle state.
package localruntimeauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/internal/localqueue"
)

var (
	// ErrBindingMismatch means the protected identity or attempt disagrees
	// with the configured queue/host binding.
	ErrBindingMismatch = errors.New("localruntimeauth: authority binding mismatch")
	// ErrCorruptState means a private file, mode, or record is missing or invalid.
	ErrCorruptState = errors.New("localruntimeauth: protected state is missing or corrupt")
	// ErrMissingCredential means a referenced attempt key was not found.
	ErrMissingCredential = errors.New("localruntimeauth: credential is missing")
	// ErrUnauthorized is the uniform outward credential refusal.
	ErrUnauthorized = errors.New("localruntimeauth: unauthorized")
	// ErrReadOnly means an OpenExisting handle attempted a writer operation.
	ErrReadOnly = errors.New("localruntimeauth: store is read-only")
	// ErrClosed means the protected store handle has been closed.
	ErrClosed = errors.New("localruntimeauth: store is closed")
	// ErrInvalidInput means a local path, handle, origin, or ID is invalid.
	ErrInvalidInput = errors.New("localruntimeauth: invalid local input")
	// ErrAmbiguousCommit requires a fresh open and exact readback after a
	// published file whose directory sync was not confirmed.
	ErrAmbiguousCommit = errors.New("localruntimeauth: protected write durability is ambiguous")
	// ErrUnsupportedPlatform means the OS has no supported kernel writer lock.
	ErrUnsupportedPlatform = errors.New("localruntimeauth: protected writer lock unsupported on this platform")
)

const (
	formatVersion       = 1
	bootstrapFileName   = "bootstrap.json"
	initializedFileName = "initialized.json"
	writerLockFileName  = "writer.lock"
	maxProtectedBytes   = 4096
	operatorTokenPrefix = "locop_"
	attemptTokenPrefix  = "locat_"
)

var localIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

type bootstrapRecord struct {
	Version         int                  `json:"version"`
	QueueRootSHA256 string               `json:"queueRootSha256"`
	Authority       localqueue.Authority `json:"authority"`
	OperatorSecret  string               `json:"operatorSecret"`
}

// The durable marker is published before a secret is minted. Losing the
// bootstrap record after first use cannot look like an empty auth root.
type initializedRecord struct {
	Version         int    `json:"version"`
	QueueRootSHA256 string `json:"queueRootSha256"`
}

// Store is private credential storage, not a queue writer lease or a claim.
// Bootstrap callers own the queue binding; OpenExisting cannot write.
type Store struct {
	mu              sync.Mutex
	root            *os.Root
	lockFile        *os.File
	rootPath        string
	queueRootSHA256 string
	identity        localqueue.Authority
	operator        Credential
	readOnly        bool
	closed          bool
	poisoned        bool
	fileSync        func(*os.File) error
	dirSync         func(*os.Root) error
	// fault is a private durability seam for deterministic interrupted-write
	// controls. It never changes the production path unless a package test sets it.
	fault func(stage string) error
}

func (*Store) String() string { return "localruntimeauth.Store{[REDACTED]}" }

// GoString never exposes private credential material in %#v diagnostics.
func (*Store) GoString() string { return "localruntimeauth.Store{[REDACTED]}" }

// Format prevents alternate fmt verbs from traversing private fields.
func (*Store) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "localruntimeauth.Store{[REDACTED]}")
}

func checkedPaths(root, queueRoot string) (string, string, error) {
	for _, value := range []string{root, queueRoot} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.Contains(value, "://") {
			return "", "", ErrInvalidInput
		}
	}
	sum := sha256.Sum256([]byte(queueRoot))
	return root, hex.EncodeToString(sum[:]), nil
}

func newStore(root *os.Root, rootPath, queueDigest string, readOnly bool) *Store {
	return &Store{
		root: root, rootPath: rootPath, queueRootSHA256: queueDigest, readOnly: readOnly,
		fileSync: func(file *os.File) error { return file.Sync() }, dirSync: syncDirectory,
	}
}

// Bootstrap creates a genuinely fresh bound identity or reopens the exact
// existing one. A partial or corrupt prior state never mints replacement keys.
func Bootstrap(root, queueRoot string) (_ *Store, err error) {
	root, digest, err := checkedPaths(root, queueRoot)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, err
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open private auth root: %w", err)
	}
	s := newStore(handle, root, digest, false)
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	var createdLock bool
	s.lockFile, createdLock, err = openWriterLock(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	// Reaffirm only after acquiring the writer lock; a concurrent initializer
	// could otherwise publish after this sync but before the record readback.
	if err := s.dirSync(s.root); err != nil {
		return nil, fmt.Errorf("sync private auth root: %w", err)
	}

	record, err := s.readBootstrap()
	if err == nil {
		if err := s.adoptBootstrap(record); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !createdLock {
		return nil, fmt.Errorf("%w: existing auth root lacks identity; preserve it for explicit recovery", ErrCorruptState)
	}
	if err := requireFreshRoot(s.root, queueRoot); err != nil {
		return nil, err
	}
	marker, err := encodeRecord(initializedRecord{Version: formatVersion, QueueRootSHA256: digest})
	if err != nil {
		return nil, err
	}
	marked, err := s.writeImmutable(context.Background(), s.root, initializedFileName, marker)
	if err != nil {
		return nil, err
	}
	if !marked {
		return nil, ErrCorruptState
	}
	record, err = newBootstrapRecord(digest)
	if err != nil {
		return nil, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return nil, err
	}
	created, err := s.writeImmutable(context.Background(), s.root, bootstrapFileName, raw)
	if err != nil {
		return nil, err
	}
	if !created {
		// Defensive if another conforming writer published despite a distinct
		// lock description: load its identity and secret, never our generated one.
		record, err = s.readBootstrap()
		if err != nil {
			return nil, err
		}
	}
	if err := s.adoptBootstrap(record); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenExisting reads an already initialized root. It never creates a file,
// directory, identity, operator secret, endpoint, or attempt credential.
func OpenExisting(root, queueRoot string) (_ *Store, err error) {
	root, digest, err := checkedPaths(root, queueRoot)
	if err != nil {
		return nil, err
	}
	if err := inspectPrivateDirectory(root); err != nil {
		return nil, err
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open existing auth root: %w", err)
	}
	s := newStore(handle, root, digest, true)
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	s.lockFile, err = openReaderLock(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	// Read-only discovery reaffirms a visible prior publication under a
	// shared lock, never before a concurrent writer can publish new bytes.
	if err := s.dirSync(s.root); err != nil {
		return nil, fmt.Errorf("sync existing auth root: %w", err)
	}
	record, err := s.readBootstrap()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			prior, inspectErr := authRootHasEntries(s.root)
			if inspectErr != nil {
				return nil, inspectErr
			}
			if prior {
				return nil, fmt.Errorf("%w: existing auth root lacks identity; preserve it for explicit recovery", ErrCorruptState)
			}
		}
		return nil, err
	}
	if err := s.adoptBootstrap(record); err != nil {
		return nil, err
	}
	return s, nil
}

func newBootstrapRecord(queueDigest string) (bootstrapRecord, error) {
	scope, err := randomID("scope")
	if err != nil {
		return bootstrapRecord{}, err
	}
	host, err := randomID("host")
	if err != nil {
		return bootstrapRecord{}, err
	}
	worker, err := randomID("worker")
	if err != nil {
		return bootstrapRecord{}, err
	}
	secret, err := randomToken(operatorTokenPrefix)
	if err != nil {
		return bootstrapRecord{}, err
	}
	return bootstrapRecord{
		Version: formatVersion, QueueRootSHA256: queueDigest,
		Authority: localqueue.Authority{
			ScopeID: scope, HostID: host, WorkerID: worker,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		OperatorSecret: secret,
	}, nil
}

func randomID(role string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate local identity: %w", err)
	}
	return role + "-" + hex.EncodeToString(bytes[:]), nil
}

func randomToken(prefix string) (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate local credential: %w", err)
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func validToken(token, prefix string) bool {
	if !strings.HasPrefix(token, prefix) || len(token) != len(prefix)+64 {
		return false
	}
	suffix := strings.TrimPrefix(token, prefix)
	_, err := hex.DecodeString(suffix)
	return err == nil && suffix == strings.ToLower(suffix)
}

func validAuthority(authority localqueue.Authority) bool {
	if !localIDPattern.MatchString(authority.ScopeID) || !localIDPattern.MatchString(authority.HostID) ||
		!localIDPattern.MatchString(authority.WorkerID) {
		return false
	}
	when, err := time.Parse(time.RFC3339Nano, authority.CreatedAt)
	return err == nil && !when.IsZero()
}

func (s *Store) readBootstrap() (bootstrapRecord, error) {
	var record bootstrapRecord
	if err := readRecord(s.root, bootstrapFileName, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, markerErr := s.root.Lstat(initializedFileName); markerErr == nil || !errors.Is(markerErr, os.ErrNotExist) {
				return bootstrapRecord{}, ErrCorruptState
			}
		}
		return record, err
	}
	if record.Version != formatVersion || !validAuthority(record.Authority) ||
		!validToken(record.OperatorSecret, operatorTokenPrefix) || !validDigest(record.QueueRootSHA256) {
		return bootstrapRecord{}, ErrCorruptState
	}
	var marker initializedRecord
	if err := readRecord(s.root, initializedFileName, &marker); err != nil ||
		marker.Version != formatVersion || !validDigest(marker.QueueRootSHA256) ||
		marker.QueueRootSHA256 != record.QueueRootSHA256 {
		return bootstrapRecord{}, ErrCorruptState
	}
	return record, nil
}

func (s *Store) adoptBootstrap(record bootstrapRecord) error {
	if record.QueueRootSHA256 != s.queueRootSHA256 {
		return ErrBindingMismatch
	}
	s.identity = record.Authority
	s.operator = newCredential(record.OperatorSecret)
	return nil
}

// Identity is the stable local authority. Only its three IDs should be
// compared with a queue Authority; CreatedAt timestamps are independent.
func (s *Store) Identity() localqueue.Authority {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity
}

func (s *Store) checkOpenLocked(write bool) error {
	if s.closed {
		return ErrClosed
	}
	if s.poisoned {
		return ErrAmbiguousCommit
	}
	if write && s.readOnly {
		return ErrReadOnly
	}
	return nil
}

func (s *Store) ensureBootstrapLocked() error {
	record, err := s.readBootstrap()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrCorruptState
		}
		return err
	}
	if record.QueueRootSHA256 != s.queueRootSHA256 ||
		record.Authority.ScopeID != s.identity.ScopeID ||
		record.Authority.HostID != s.identity.HostID ||
		record.Authority.WorkerID != s.identity.WorkerID ||
		!equalSecret(record.OperatorSecret, s.operator.BearerToken()) {
		return ErrBindingMismatch
	}
	return nil
}

// Close releases only this package's file handles. It does not erase any
// credential, since an exact terminal retry may still need its original key.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	if s.lockFile != nil {
		closeErr = errors.Join(closeErr, s.lockFile.Close())
	}
	if s.root != nil {
		closeErr = errors.Join(closeErr, s.root.Close())
	}
	return closeErr
}
