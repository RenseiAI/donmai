package localqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
)

type sessionState struct {
	record       AdmissionRecord
	attempt      *AttemptRef
	launch       *OwnedProcessCorrelation
	observations []BoundedTypedObservation
	terminal     *DurableReceipt
}

// Store owns one append-only local authority and its kernel writer lock.
// All projections below are rebuilt from checked journal transactions.
type Store struct {
	mu           sync.Mutex
	root         string
	journalDir   string
	rootHandle   *os.Root
	journalRoot  *os.Root
	lockFile     *os.File
	binding      ConfiguredHostBinding
	authority    Authority
	revision     Revision
	digest       string
	sessions     map[string]*sessionState
	sources      map[string]string
	publications map[string]PublicationOutbox
	closed       bool
	poisoned     bool
	fileSync     func(*os.File) error
	dirSync      func(*os.Root) error
	// fault is package-private so disk-failure tests can interrupt precise
	// transaction stages without weakening the production durability path.
	fault func(stage string) error
}

func validateConfiguredBinding(binding ConfiguredHostBinding) error {
	if !validLocalID(binding.ScopeID) || !validLocalID(binding.HostID) || !validLocalID(binding.WorkerID) {
		return fmt.Errorf("%w: local scope, host, and worker IDs are required", ErrBindingMismatch)
	}
	return nil
}

func cloneSelectors(input *executioncell.ExecutionSelectorRegistry) *executioncell.ExecutionSelectorRegistry {
	if input == nil {
		return nil
	}
	clone := *input
	if input.HarnessVersions != nil {
		clone.HarnessVersions = make(map[string][]string, len(input.HarnessVersions))
		for name, versions := range input.HarnessVersions {
			clone.HarnessVersions[name] = slices.Clone(versions)
		}
	}
	clone.Models = slices.Clone(input.Models)
	clone.Endpoints = slices.Clone(input.Endpoints)
	clone.AuthBindings = slices.Clone(input.AuthBindings)
	clone.Placements = slices.Clone(input.Placements)
	clone.Capabilities = slices.Clone(input.Capabilities)
	return &clone
}

func secureDirectory(path string) error {
	parent := filepath.Dir(path)
	if err := ensureParentDirectories(parent); err != nil {
		return err
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("localqueue: open directory parent: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()
	name := filepath.Base(path)
	if _, err := parentRoot.Lstat(name); errors.Is(err, os.ErrNotExist) {
		if err := parentRoot.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("localqueue: create directory %q: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("localqueue: inspect directory %q: %w", path, err)
	}
	info, err := parentRoot.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("localqueue: created directory %q is not private", path)
	}
	// Repeat this even for a pre-existing directory. A previous process may
	// have crashed after mkdir but before the parent linkage became durable.
	if err := syncDirectory(parentRoot); err != nil {
		return fmt.Errorf("localqueue: sync directory parent linkage: %w", err)
	}
	return nil
}

// ensureParentDirectories creates only absent ancestors and fsyncs each new
// parent link. Existing ancestors may be shared system directories (for
// example the macOS temp root); the queue leaf itself must still be private.
func ensureParentDirectories(path string) error {
	opened, err := os.OpenRoot(path)
	if err == nil {
		defer func() { _ = opened.Close() }()
		info, err := opened.Stat(".")
		if err != nil || !info.IsDir() {
			return fmt.Errorf("localqueue: parent %q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localqueue: inspect parent %q: %w", path, err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("localqueue: no existing filesystem root for %q", path)
	}
	if err := ensureParentDirectories(parent); err != nil {
		return err
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("localqueue: open parent %q: %w", parent, err)
	}
	defer func() { _ = parentRoot.Close() }()
	name := filepath.Base(path)
	if err := parentRoot.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("localqueue: create parent %q: %w", path, err)
	}
	createdInfo, err := parentRoot.Lstat(name)
	if err != nil || !createdInfo.IsDir() || createdInfo.Mode()&os.ModeSymlink != 0 || createdInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("localqueue: newly created parent %q is not private", path)
	}
	if err := syncDirectory(parentRoot); err != nil {
		return fmt.Errorf("localqueue: sync parent linkage for %q: %w", path, err)
	}
	return nil
}

func openLockFile(root *os.Root) (*os.File, error) {
	const name = "writer.lock"
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("localqueue: writer lock is not a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("localqueue: inspect writer lock: %w", err)
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("localqueue: open writer lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("localqueue: opened writer lock is not a private regular file")
	}
	if err := lockWriter(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Open acquires a real kernel single-writer lock and verifies the entire
// journal before admitting any work. An intentional copy of the same bound
// authority on a different host cannot be detected cryptographically.
func Open(root string, binding ConfiguredHostBinding) (*Store, error) {
	if err := validateConfiguredBinding(binding); err != nil {
		return nil, err
	}
	binding.Selectors = cloneSelectors(binding.Selectors)
	if !filepath.IsAbs(root) || strings.Contains(root, "://") {
		return nil, fmt.Errorf("localqueue: root must be an absolute local path")
	}
	root = filepath.Clean(root)
	if err := secureDirectory(root); err != nil {
		return nil, err
	}
	journalDir := filepath.Join(root, "transactions")
	if err := secureDirectory(journalDir); err != nil {
		return nil, err
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("localqueue: open private root: %w", err)
	}
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		_ = rootHandle.Close()
		return nil, fmt.Errorf("localqueue: open private transaction root: %w", err)
	}
	lockFile, err := openLockFile(rootHandle)
	if err != nil {
		_ = journalRoot.Close()
		_ = rootHandle.Close()
		return nil, err
	}
	store := &Store{
		root: root, journalDir: journalDir, rootHandle: rootHandle, journalRoot: journalRoot,
		lockFile: lockFile, binding: binding,
		sessions: make(map[string]*sessionState), sources: make(map[string]string),
		publications: make(map[string]PublicationOutbox),
		fileSync:     func(file *os.File) error { return file.Sync() }, dirSync: syncDirectory,
	}
	// A prior writer may have linked a transaction and lost the directory
	// fsync acknowledgement. Durably re-sync every visible link before replay
	// can expose pending dispatch as recovered authority.
	if err := syncDirectory(journalRoot); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("%w: recovery directory sync: %v", ErrAmbiguousCommit, err)
	}
	if err := store.recoverJournal(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if store.revision == 0 {
		authority := Authority{
			ScopeID: binding.ScopeID, HostID: binding.HostID, WorkerID: binding.WorkerID,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		store.mu.Lock()
		err = store.commitLocked(context.Background(), 0, kindAuthority, authority)
		store.mu.Unlock()
		if err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	if store.authority.ScopeID != binding.ScopeID || store.authority.HostID != binding.HostID || store.authority.WorkerID != binding.WorkerID {
		_ = store.Close()
		return nil, ErrBindingMismatch
	}
	return store, nil
}

// Close releases only this Store's writer lock. It never removes the durable
// journal or another daemon's files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var unlockErr, closeErr, journalErr, rootErr error
	if s.lockFile != nil {
		unlockErr = unlockWriter(s.lockFile)
		closeErr = s.lockFile.Close()
		s.lockFile = nil
	}
	if s.journalRoot != nil {
		journalErr = s.journalRoot.Close()
		s.journalRoot = nil
	}
	if s.rootHandle != nil {
		rootErr = s.rootHandle.Close()
		s.rootHandle = nil
	}
	return errors.Join(unlockErr, closeErr, journalErr, rootErr)
}

// LoadAuthority returns the persisted authority, not a fresh caller claim.
func (s *Store) LoadAuthority() (Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned || s.revision == 0 {
		return Authority{}, ErrCorruptStore
	}
	return s.authority, nil
}

// CurrentRevision returns the last fully fsynced transaction revision.
func (s *Store) CurrentRevision() Revision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func sessionKey(scopeID, sessionID string) string { return scopeID + "\x00" + sessionID }

func sourceKey(source GitHubSource) string {
	return fmt.Sprintf("%d:%d", source.RepositoryID, source.IssueNumber)
}

func cloneValue[T any](value T) T {
	raw, _ := json.Marshal(value)
	var clone T
	_ = json.Unmarshal(raw, &clone)
	return clone
}

// UpdateSelectors replaces only trusted, live admission catalog input. It does
// not rewrite historical evidence, identity or journal revision. Dispatch must
// independently revalidate pending evidence against current process policy.
func (s *Store) UpdateSelectors(ctx context.Context, selectors *executioncell.ExecutionSelectorRegistry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if selectors == nil || selectors.HarnessVersions == nil || selectors.Models == nil || selectors.Endpoints == nil || selectors.AuthBindings == nil || selectors.Placements == nil {
		return fmt.Errorf("%w: complete selector registry is required", ErrInvalidEnvelope)
	}
	next := cloneSelectors(selectors)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return ErrCorruptStore
	}
	s.binding.Selectors = next
	return nil
}

// Session returns one local session projection under its exact scope.
func (s *Store) Session(ctx context.Context, scopeID, sessionID string) (SessionProjection, error) {
	if err := ctx.Err(); err != nil {
		return SessionProjection{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return SessionProjection{}, ErrCorruptStore
	}
	state := s.sessions[sessionKey(scopeID, sessionID)]
	if state == nil {
		return SessionProjection{}, ErrUnknownSession
	}
	return s.projectionLocked(state), nil
}

// SessionForSource resolves the original admitted session without deriving a
// session ID from tracker identity. Source aliases must still match the stored
// correlation; a renamed/mismatched source is not implicitly rebound.
func (s *Store) SessionForSource(ctx context.Context, scopeID string, source GitHubSource) (SessionProjection, error) {
	if err := ctx.Err(); err != nil {
		return SessionProjection{}, err
	}
	if err := validateSource(source); err != nil {
		return SessionProjection{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return SessionProjection{}, ErrCorruptStore
	}
	if scopeID != s.authority.ScopeID {
		return SessionProjection{}, ErrUnknownSession
	}
	key, exists := s.sources[sourceKey(source)]
	if !exists {
		return SessionProjection{}, ErrUnknownSession
	}
	state := s.sessions[key]
	if state == nil {
		return SessionProjection{}, ErrCorruptStore
	}
	original := state.record.Envelope.Source
	if !strings.EqualFold(original.OwnerRepo, source.OwnerRepo) || !strings.EqualFold(original.IssueURL, source.IssueURL) {
		return SessionProjection{}, ErrBindingMismatch
	}
	return s.projectionLocked(state), nil
}

func (s *Store) projectionLocked(state *sessionState) SessionProjection {
	sessionID := state.record.Envelope.Session.SessionID
	publications := make([]PublicationOutbox, 0)
	for _, item := range s.publications {
		if item.SessionID == sessionID {
			publications = append(publications, item)
		}
	}
	sort.Slice(publications, func(i, j int) bool { return publications[i].Key < publications[j].Key })
	projection := SessionProjection{
		Admission:    cloneAdmissionRecord(state.record),
		Observations: slices.Clone(state.observations), Publications: slices.Clone(publications),
	}
	if state.attempt != nil {
		attempt := *state.attempt
		projection.Attempt = &attempt
	}
	if state.launch != nil {
		launch := *state.launch
		projection.Launch = &launch
	}
	if state.terminal != nil {
		terminal := *state.terminal
		terminal.Body = bytes.Clone(terminal.Body)
		projection.Terminal = &terminal
	}
	return projection
}

// SessionsNeedingRecovery discovers privately fenced in-flight sessions after
// restart. It never requeues or grants a replacement attempt; callers must
// reconcile observed process/terminal evidence under each exact AttemptRef.
func (s *Store) SessionsNeedingRecovery(ctx context.Context) ([]SessionProjection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return nil, ErrCorruptStore
	}
	result := make([]SessionProjection, 0)
	for _, state := range s.sessions {
		switch state.record.DispatchState {
		case DispatchClaimed, DispatchUnknown, DispatchLaunched:
			result = append(result, s.projectionLocked(state))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Admission.Revision < result[j].Admission.Revision
	})
	return result, nil
}

// PendingDispatch returns only durable, not-yet-claimed admissions.
func (s *Store) PendingDispatch(ctx context.Context) ([]AdmissionRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return nil, ErrCorruptStore
	}
	records := make([]AdmissionRecord, 0)
	for _, state := range s.sessions {
		if state.record.DispatchState == DispatchPending {
			records = append(records, cloneAdmissionRecord(state.record))
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Revision < records[j].Revision })
	return records, nil
}

// PendingPublication returns durable, unpublished terminal operations.
func (s *Store) PendingPublication(ctx context.Context) ([]PublicationOutbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return nil, ErrCorruptStore
	}
	items := make([]PublicationOutbox, 0)
	for _, item := range s.publications {
		if item.State == DispatchPending {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return items, nil
}
