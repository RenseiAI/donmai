package workarea

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// AcquisitionRecordSchemaV1 identifies the durable generation authority.
	AcquisitionRecordSchemaV1    = "donmai.workarea-acquisition.v1"
	acquisitionStoreDirName      = ".workarea-acquisitions"
	acquisitionRecordFileName    = "acquisition.json"
	acquisitionStagingRootName   = "root"
	acquisitionReleasedRootName  = "released-root"
	acquisitionRestoreRootName   = "restore-root"
	acquisitionOwnerLockName     = "owner.lock"
	acquisitionStoreLockName     = ".store.lock"
	acquisitionStoreIdentityName = "store.json"
	acquisitionStoreAnchorName   = ".workarea-acquisitions.identity.json"
	acquisitionStoreSchemaV1     = "donmai.workarea-acquisition-store.v1"
	acquisitionTransactionsName  = "transactions"
)

// ErrAcquisitionRootOccupied means a different filesystem object already owns
// the requested final leaf. It never authorizes deletion of that object.
var ErrAcquisitionRootOccupied = errors.New("runtime/workarea: acquisition root occupied")

// ErrAcquisitionNotFound means the durable journal has no live matching root.
var ErrAcquisitionNotFound = errors.New("runtime/workarea: acquisition not found")

var errAcquisitionStoreDormant = errors.New("runtime/workarea: acquisition store is dormant")

// AcquisitionState is the durable generation lifecycle.
type AcquisitionState string

const (
	// AcquisitionClaiming means the durable identity exists before root creation.
	AcquisitionClaiming AcquisitionState = "claiming"
	// AcquisitionProvisioning means the proven staging root is being populated.
	AcquisitionProvisioning AcquisitionState = "provisioning"
	// AcquisitionReady means the declared root was atomically published.
	AcquisitionReady AcquisitionState = "ready"
	// AcquisitionRestoring means an immutable archive is being re-entered under
	// the same acquisition identity.
	AcquisitionRestoring AcquisitionState = "restoring"
	// AcquisitionQuarantined means ownership could not be proved and no cleanup is authorized.
	AcquisitionQuarantined AcquisitionState = "quarantined"
	// AcquisitionAborted means a proved staging generation was removed.
	AcquisitionAborted AcquisitionState = "aborted"
	// AcquisitionReleased means the exact published root was disposed.
	AcquisitionReleased AcquisitionState = "released"
)

// FileIdentity is a durable local filesystem object identity.
type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Empty reports whether no published root identity is recorded.
func (i FileIdentity) Empty() bool { return i.Device == 0 && i.Inode == 0 }

// IdentityOf returns the durable local identity of one filesystem object.
func IdentityOf(info os.FileInfo) (FileIdentity, error) { return fileIdentity(info) }

// Participant is one shared-mode reference to an owner root.
type Participant struct {
	SessionID          string `json:"sessionId"`
	ParentWorkareaID   string `json:"parentWorkareaId"`
	SelectedRepository string `json:"selectedRepository"`
}

// AcquisitionRecord is the durable authority for one session generation.
type AcquisitionRecord struct {
	SchemaVersion       string           `json:"schemaVersion"`
	StoreID             string           `json:"storeId"`
	AcquisitionID       string           `json:"acquisitionId"`
	WorkareaID          string           `json:"workareaId"`
	AccountingID        string           `json:"accountingId"`
	ObservationCursorID string           `json:"observationCursorId"`
	SessionID           string           `json:"sessionId"`
	FinalRoot           string           `json:"finalRoot"`
	SelectedLeaf        string           `json:"selectedLeaf"`
	CacheSeedID         string           `json:"cacheSeedId,omitempty"`
	State               AcquisitionState `json:"state"`
	RootIdentity        FileIdentity     `json:"rootIdentity"`
	OwnerReleased       bool             `json:"ownerReleased,omitempty"`
	Participants        []Participant    `json:"participants,omitempty"`
	CreatedAt           time.Time        `json:"createdAt"`
	UpdatedAt           time.Time        `json:"updatedAt"`
	LastError           string           `json:"lastError,omitempty"`
	RestoreArchiveID    string           `json:"restoreArchiveId,omitempty"`
	RestoreDigest       string           `json:"restoreDigest,omitempty"`
	ArchiveID           string           `json:"archiveId,omitempty"`
	ArchiveDigest       string           `json:"archiveDigest,omitempty"`
}

// Validate checks the closed acquisition record independently of its store.
func (r AcquisitionRecord) Validate() error {
	if r.SchemaVersion != AcquisitionRecordSchemaV1 || !strings.HasPrefix(r.StoreID, "wstore_") || !strings.HasPrefix(r.AcquisitionID, "wac_") || !strings.HasPrefix(r.AccountingID, "waa_") || !strings.HasPrefix(r.ObservationCursorID, "woc_") || r.WorkareaID == "" || r.SessionID == "" || !filepath.IsAbs(r.FinalRoot) || r.SelectedLeaf == "" {
		return fmt.Errorf("runtime/workarea: invalid acquisition record header")
	}
	if err := ValidateRepositoryLeaf(r.SelectedLeaf); err != nil {
		return err
	}
	switch r.State {
	case AcquisitionClaiming, AcquisitionProvisioning, AcquisitionReady, AcquisitionRestoring, AcquisitionQuarantined, AcquisitionAborted, AcquisitionReleased:
	default:
		return fmt.Errorf("runtime/workarea: invalid acquisition state %q", r.State)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return fmt.Errorf("runtime/workarea: acquisition timestamps are required")
	}
	if r.State == AcquisitionRestoring {
		if r.RestoreArchiveID == "" || r.RestoreDigest == "" {
			return fmt.Errorf("runtime/workarea: restoring acquisition requires archive identity and digest")
		}
	} else if r.RestoreArchiveID != "" || r.RestoreDigest != "" {
		return fmt.Errorf("runtime/workarea: restore identity is present outside restoring state")
	}
	if (r.ArchiveID == "") != (r.ArchiveDigest == "") {
		return fmt.Errorf("runtime/workarea: archive identity and digest must be paired")
	}
	seen := make(map[string]struct{}, len(r.Participants))
	for _, participant := range r.Participants {
		if participant.SessionID == "" || participant.ParentWorkareaID != r.WorkareaID || participant.SelectedRepository == "" {
			return fmt.Errorf("runtime/workarea: invalid shared participant")
		}
		if _, duplicate := seen[participant.SessionID]; duplicate {
			return fmt.Errorf("runtime/workarea: duplicate shared participant %q", participant.SessionID)
		}
		seen[participant.SessionID] = struct{}{}
	}
	return nil
}

// Acquisition is one in-progress proved staging generation.
type Acquisition struct {
	Record      AcquisitionRecord
	StagingRoot RootPath
}

// RestoreAcquisition is one crash-recoverable archive re-entry stage.
type RestoreAcquisition struct {
	Record      AcquisitionRecord
	StagingRoot RootPath
}

// AcquisitionStore owns durable generation records beneath one worktree root.
type AcquisitionStore struct {
	parent     string
	dir        string
	parentRoot *os.Root
	root       *os.Root
	identity   os.FileInfo
	storeID    string
	now        func() time.Time
	mu         sync.Mutex
	byWorkarea map[string]string
	bySession  map[string]string
	owners     map[string]*os.File
}

type acquisitionStoreIdentity struct {
	SchemaVersion string       `json:"schemaVersion"`
	StoreID       string       `json:"storeId"`
	Identity      FileIdentity `json:"identity"`
}

// NewAcquisitionStore opens and reconciles the generation authority.
func NewAcquisitionStore(parent string, now func() time.Time) (*AcquisitionStore, error) {
	store, _, err := openAcquisitionStore(parent, now, true)
	return store, err
}

// OpenExistingAcquisitionStore opens recovery authority only when negotiated
// state already exists. It never creates metadata for a legacy-flat-only host.
func OpenExistingAcquisitionStore(parent string, now func() time.Time) (*AcquisitionStore, bool, error) {
	return openAcquisitionStore(parent, now, false)
}

func openAcquisitionStore(parent string, now func() time.Time, create bool) (*AcquisitionStore, bool, error) {
	abs, err := filepath.Abs(parent)
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: resolve acquisition parent: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	parentRoot, err := os.OpenRoot(abs)
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: open acquisition parent: %w", err)
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = parentRoot.Close()
		}
	}()
	dir := filepath.Join(abs, acquisitionStoreDirName)
	if create {
		if err := parentRoot.MkdirAll(acquisitionStoreDirName, 0o700); err != nil {
			return nil, false, fmt.Errorf("runtime/workarea: create acquisition store: %w", err)
		}
	}
	info, err := parentRoot.Lstat(acquisitionStoreDirName)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, false, fmt.Errorf("runtime/workarea: acquisition store is not a private real directory")
	}
	root, err := parentRoot.OpenRoot(acquisitionStoreDirName)
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: open acquisition store root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: acquisition store identity changed while opening")
	}
	store := &AcquisitionStore{
		parent: abs, dir: dir, parentRoot: parentRoot, root: root, identity: openedInfo, now: now,
		byWorkarea: make(map[string]string), bySession: make(map[string]string), owners: make(map[string]*os.File),
	}
	bootstrapLock, err := openLockFile(root, acquisitionStoreLockName)
	if err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: open acquisition identity lock: %w", err)
	}
	if err := flockExclusive(bootstrapLock, false); err != nil {
		_ = bootstrapLock.Close()
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: lock acquisition identity: %w", err)
	}
	storeID, err := store.loadOrCreateStoreIdentity(create)
	releaseFlock(bootstrapLock)
	if errors.Is(err, errAcquisitionStoreDormant) {
		_ = root.Close()
		return nil, false, nil
	}
	if err != nil {
		_ = root.Close()
		return nil, false, err
	}
	store.storeID = storeID
	if err := root.MkdirAll(acquisitionTransactionsName, 0o700); err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: create acquisition transaction locks: %w", err)
	}
	transactionInfo, err := root.Lstat(acquisitionTransactionsName)
	if err != nil || !transactionInfo.IsDir() || transactionInfo.Mode()&os.ModeSymlink != 0 || transactionInfo.Mode().Perm() != 0o700 {
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: acquisition transaction lock directory is unsafe")
	}
	if err := store.Recover(context.Background()); err != nil {
		_ = root.Close()
		return nil, false, err
	}
	closeParent = false
	return store, true, nil
}

func (s *AcquisitionStore) loadOrCreateStoreIdentity(create bool) (string, error) {
	physical, err := fileIdentity(s.identity)
	if err != nil {
		return "", err
	}
	storeRecord, storeExists, err := readAcquisitionStoreIdentity(s.root, acquisitionStoreIdentityName)
	if err != nil {
		return "", err
	}
	anchorRecord, anchorExists, err := readAcquisitionStoreIdentity(s.parentRoot, acquisitionStoreAnchorName)
	if err != nil {
		return "", err
	}
	if anchorExists && anchorRecord.Identity != physical {
		return "", fmt.Errorf("runtime/workarea: acquisition store replacement differs from durable parent anchor")
	}
	if storeExists && storeRecord.Identity != physical {
		return "", fmt.Errorf("runtime/workarea: acquisition store identity mismatch")
	}
	switch {
	case storeExists && anchorExists:
		if storeRecord != anchorRecord {
			return "", fmt.Errorf("runtime/workarea: acquisition store and parent anchor disagree")
		}
		return storeRecord.StoreID, nil
	case storeExists:
		if err := writeAcquisitionStoreAnchor(s.parentRoot, storeRecord); err != nil {
			return "", err
		}
		return storeRecord.StoreID, nil
	case anchorExists:
		if err := s.writeStoreIdentity(anchorRecord); err != nil {
			return "", err
		}
		return anchorRecord.StoreID, nil
	default:
		entries, readErr := s.readDirRoot()
		if readErr != nil {
			return "", readErr
		}
		if !create {
			dormant := true
			for _, entry := range entries {
				if entry.Name() != acquisitionStoreLockName {
					dormant = false
					break
				}
			}
			if dormant {
				return "", errAcquisitionStoreDormant
			}
			return "", fmt.Errorf("runtime/workarea: existing acquisition store has no pinned identity")
		}
		storeID, idErr := newGenerationID("wstore_")
		if idErr != nil {
			return "", idErr
		}
		record := acquisitionStoreIdentity{SchemaVersion: acquisitionStoreSchemaV1, StoreID: storeID, Identity: physical}
		if writeErr := s.writeStoreIdentity(record); writeErr != nil {
			return "", writeErr
		}
		if writeErr := writeAcquisitionStoreAnchor(s.parentRoot, record); writeErr != nil {
			return "", writeErr
		}
		return storeID, nil
	}
}

func readAcquisitionStoreIdentity(root *os.Root, name string) (acquisitionStoreIdentity, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return acquisitionStoreIdentity{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return acquisitionStoreIdentity{}, false, fmt.Errorf("runtime/workarea: acquisition store identity file is unsafe")
	}
	body, err := root.ReadFile(name)
	if err != nil {
		return acquisitionStoreIdentity{}, false, err
	}
	var record acquisitionStoreIdentity
	if err := decodeClosedJSON(body, &record); err != nil || record.SchemaVersion != acquisitionStoreSchemaV1 || !strings.HasPrefix(record.StoreID, "wstore_") {
		return acquisitionStoreIdentity{}, false, fmt.Errorf("runtime/workarea: invalid acquisition store identity")
	}
	return record, true, nil
}

func writeAcquisitionStoreAnchor(parent *os.Root, record acquisitionStoreIdentity) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := parent.OpenFile(acquisitionStoreAnchorName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
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
	return syncRoot(parent)
}

func (s *AcquisitionStore) writeStoreIdentity(record acquisitionStoreIdentity) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tempName, err := rootedTempName(".store-")
	if err != nil {
		return err
	}
	file, err := s.root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(tempName) }()
	if _, err := file.Write(body); err != nil {
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
	if err := s.root.Rename(tempName, acquisitionStoreIdentityName); err != nil {
		return err
	}
	return syncRoot(s.root)
}

func (s *AcquisitionStore) assertStoreIdentity() error {
	current, err := s.parentRoot.Lstat(acquisitionStoreDirName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: inspect pinned acquisition store: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, s.identity) {
		return fmt.Errorf("runtime/workarea: acquisition store directory identity changed")
	}
	anchor, exists, err := readAcquisitionStoreIdentity(s.parentRoot, acquisitionStoreAnchorName)
	if err != nil || !exists || anchor.StoreID != s.storeID {
		return fmt.Errorf("runtime/workarea: acquisition store parent anchor changed")
	}
	return nil
}

func (s *AcquisitionStore) readDirRoot() ([]os.DirEntry, error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	if closeErr := directory.Close(); readErr == nil {
		readErr = closeErr
	}
	return entries, readErr
}

// WithRootTransaction serializes every lifecycle transition for one workarea
// across processes. Callers compose lease and acquisition operations inside the
// callback so shared join, lease commit, and teardown have one order.
func (s *AcquisitionStore) WithRootTransaction(ctx context.Context, workareaID string, fn func() error) error {
	if workareaID == "" || fn == nil {
		return fmt.Errorf("runtime/workarea: root transaction requires workarea identity and callback")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.assertStoreIdentity(); err != nil {
		return err
	}
	transactions, err := s.root.OpenRoot(acquisitionTransactionsName)
	if err != nil {
		return err
	}
	defer func() { _ = transactions.Close() }()
	lock, err := openLockFile(transactions, fileKey(workareaID)+".lock")
	if err != nil {
		return err
	}
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		_ = lock.Close()
		return err
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return err
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
	defer releaseFlock(lock)
	if err := s.assertStoreIdentity(); err != nil {
		return err
	}
	return fn()
}

// Begin persists identity before creating the staging root.
func (s *AcquisitionStore) Begin(sessionID, workareaID string, finalRoot RootPath, selectedLeaf, cacheSeedID string) (*Acquisition, error) {
	if sessionID == "" || workareaID == "" {
		return nil, fmt.Errorf("runtime/workarea: session and workarea identities are required")
	}
	if err := ValidateRepositoryLeaf(selectedLeaf); err != nil {
		return nil, err
	}
	if cacheSeedID != "" {
		if err := ValidateRepositoryLeaf(cacheSeedID); err != nil {
			return nil, fmt.Errorf("runtime/workarea: invalid cache seed identity: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return nil, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return nil, err
	}
	if !directChild(s.parent, finalRoot.String()) {
		return nil, fmt.Errorf("runtime/workarea: final root is not a direct child of the worktree root")
	}
	if _, exists := s.byWorkarea[workareaID]; exists {
		return nil, fmt.Errorf("%w: duplicate workarea id %q", ErrAcquisitionRootOccupied, workareaID)
	}
	if existing := s.bySession[sessionID]; existing != "" {
		return nil, fmt.Errorf("%w: session %q already belongs to acquisition %q", ErrAcquisitionRootOccupied, sessionID, existing)
	}
	acquisitionID, err := newAcquisitionID()
	if err != nil {
		return nil, err
	}
	accountingID, err := newGenerationID("waa_")
	if err != nil {
		return nil, err
	}
	observationCursorID, err := newGenerationID("woc_")
	if err != nil {
		return nil, err
	}
	claimLeaf := ".claim-" + acquisitionID
	if err := s.root.Mkdir(claimLeaf, 0o700); err != nil {
		return nil, fmt.Errorf("runtime/workarea: claim acquisition identity: %w", err)
	}
	claimRoot, err := s.root.OpenRoot(claimLeaf)
	if err != nil {
		_ = s.root.RemoveAll(claimLeaf)
		return nil, fmt.Errorf("runtime/workarea: open acquisition claim: %w", err)
	}
	ownerLock, err := openLockFile(claimRoot, acquisitionOwnerLockName)
	if err != nil {
		_ = claimRoot.Close()
		_ = s.root.RemoveAll(claimLeaf)
		return nil, fmt.Errorf("runtime/workarea: open acquisition owner lock: %w", err)
	}
	if err := flockExclusive(ownerLock, false); err != nil {
		_ = ownerLock.Close()
		_ = claimRoot.Close()
		_ = s.root.RemoveAll(claimLeaf)
		return nil, fmt.Errorf("runtime/workarea: acquire provisioning ownership: %w", err)
	}
	claimPublished := false
	defer func() {
		if !claimPublished {
			releaseFlock(ownerLock)
			_ = claimRoot.Close()
			_ = s.root.RemoveAll(claimLeaf)
		}
	}()
	now := s.now().UTC()
	record := AcquisitionRecord{
		SchemaVersion: AcquisitionRecordSchemaV1, StoreID: s.storeID, AcquisitionID: acquisitionID,
		WorkareaID: workareaID, AccountingID: accountingID, ObservationCursorID: observationCursorID,
		SessionID: sessionID, FinalRoot: finalRoot.String(),
		SelectedLeaf: selectedLeaf, CacheSeedID: cacheSeedID, State: AcquisitionClaiming,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := writeAcquisitionRecordRoot(claimRoot, record); err != nil {
		return nil, err
	}
	if err := syncRoot(claimRoot); err != nil {
		return nil, err
	}
	if err := s.root.Rename(claimLeaf, acquisitionID); err != nil {
		return nil, fmt.Errorf("runtime/workarea: publish acquisition identity: %w", err)
	}
	if err := syncRoot(s.root); err != nil {
		return nil, err
	}
	claimPublished = true
	s.byWorkarea[workareaID] = acquisitionID
	s.bySession[sessionID] = acquisitionID
	s.owners[acquisitionID] = ownerLock
	if err := claimRoot.Mkdir(acquisitionStagingRootName, 0o750); err != nil {
		s.releaseOwnerLocked(acquisitionID)
		_ = claimRoot.Close()
		return nil, fmt.Errorf("runtime/workarea: create proved staging root: %w", err)
	}
	if err := syncRoot(claimRoot); err != nil {
		s.releaseOwnerLocked(acquisitionID)
		_ = claimRoot.Close()
		return nil, err
	}
	_ = claimRoot.Close()
	record.State = AcquisitionProvisioning
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		s.releaseOwnerLocked(acquisitionID)
		return nil, err
	}
	return &Acquisition{Record: record, StagingRoot: RootPath(filepath.Join(s.dir, acquisitionID, acquisitionStagingRootName))}, nil
}

// Commit atomically publishes a fully declared staging root.
func (s *AcquisitionStore) Commit(acquisitionID string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[acquisitionID] == nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: acquisition is not owned by this provisioner")
	}
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	staging := RootPath(filepath.Join(s.dir, acquisitionID, acquisitionStagingRootName))
	declaration, err := ReadDeclaration(staging)
	if err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: commit requires durable declaration: %w", err)
	}
	if declaration.AcquisitionID != acquisitionID || declaration.WorkareaID != record.WorkareaID || declaration.SessionID != record.SessionID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: declaration does not bind acquisition identity")
	}
	if err := ValidateDeclaredRoot(staging, declaration); err != nil {
		return AcquisitionRecord{}, err
	}
	parentRoot := s.parentRoot
	finalLeaf := filepath.Base(record.FinalRoot)
	if _, err := parentRoot.Lstat(finalLeaf); err == nil {
		return AcquisitionRecord{}, fmt.Errorf("%w: %s", ErrAcquisitionRootOccupied, record.FinalRoot)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: inspect final root: %w", err)
	}
	stagingRelative := filepath.Join(acquisitionStoreDirName, acquisitionID, acquisitionStagingRootName)
	stagingIdentity, err := parentRoot.Lstat(stagingRelative)
	if err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: inspect staging root identity: %w", err)
	}
	if !stagingIdentity.IsDir() || stagingIdentity.Mode()&os.ModeSymlink != 0 {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: staging root is not a real directory")
	}
	identity, err := fileIdentity(stagingIdentity)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	record.RootIdentity = identity
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return AcquisitionRecord{}, err
	}
	if err := renameNoReplace(filepath.Join(s.parent, stagingRelative), record.FinalRoot); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return AcquisitionRecord{}, fmt.Errorf("%w: %s", ErrAcquisitionRootOccupied, record.FinalRoot)
		}
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: publish session root: %w", err)
	}
	if err := syncRoot(parentRoot); err != nil {
		return AcquisitionRecord{}, err
	}
	publishedIdentity, err := parentRoot.Lstat(finalLeaf)
	if err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: inspect published root identity: %w", err)
	}
	publishedFileIdentity, err := fileIdentity(publishedIdentity)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if publishedIdentity.Mode()&os.ModeSymlink != 0 || !os.SameFile(stagingIdentity, publishedIdentity) || publishedFileIdentity != identity {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: published root identity changed during claim")
	}
	record.State = AcquisitionReady
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return AcquisitionRecord{}, err
	}
	s.releaseOwnerLocked(acquisitionID)
	return record, nil
}

// Abort removes only a staging root proved by its durable acquisition record.
func (s *AcquisitionStore) Abort(acquisitionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return err
	}
	if record.State == AcquisitionReady || record.State == AcquisitionQuarantined || record.State == AcquisitionReleased {
		s.releaseOwnerLocked(acquisitionID)
		return fmt.Errorf("runtime/workarea: acquisition %s does not authorize staging cleanup", acquisitionID)
	}
	if _, err := s.parentRoot.Lstat(filepath.Base(record.FinalRoot)); err == nil {
		if reconcileErr := s.reconcileRecord(&record); reconcileErr != nil {
			s.releaseOwnerLocked(acquisitionID)
			return reconcileErr
		}
		s.releaseOwnerLocked(acquisitionID)
		return fmt.Errorf("runtime/workarea: acquisition %s published a final root; staging cleanup refused", acquisitionID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: inspect final root before abort: %w", err)
	}
	root, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition for abort: %w", err)
	}
	if err := root.RemoveAll(acquisitionStagingRootName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = root.Close()
		return fmt.Errorf("runtime/workarea: remove proved staging root: %w", err)
	}
	_ = root.Close()
	record.State = AcquisitionAborted
	record.UpdatedAt = s.now().UTC()
	err = s.writeRecord(record)
	s.releaseOwnerLocked(acquisitionID)
	return err
}

// Abandon simulates or completes a process loss at a test fault boundary. It
// releases only the live provisioning or restore lock; durable recovery remains
// the sole authority allowed to classify and clean the proved staging root.
func (s *AcquisitionStore) Abandon(acquisitionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[acquisitionID] == nil {
		return fmt.Errorf("runtime/workarea: acquisition is not owned by this provisioner")
	}
	s.releaseOwnerLocked(acquisitionID)
	return nil
}

// Recover reconciles every durable acquisition before admission.
func (s *AcquisitionStore) Recover(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	entries, err := s.readDirRoot()
	if err != nil {
		return fmt.Errorf("runtime/workarea: scan acquisitions: %w", err)
	}
	s.byWorkarea = make(map[string]string)
	s.bySession = make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".claim-wac_") {
			if err := s.recoverUnpublishedClaim(entry.Name()); err != nil {
				return err
			}
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "wac_") {
			continue
		}
		record, err := s.readRecord(entry.Name())
		if err != nil {
			return err
		}
		if record.State == AcquisitionAborted {
			if err := s.reconcileRecord(&record); err != nil {
				return err
			}
			continue
		}
		if record.State == AcquisitionReleased {
			if err := s.reconcileRecord(&record); err != nil {
				return err
			}
		}
		if other, duplicate := s.byWorkarea[record.WorkareaID]; duplicate && other != record.AcquisitionID {
			return fmt.Errorf("runtime/workarea: duplicate durable workarea id %q", record.WorkareaID)
		}
		s.byWorkarea[record.WorkareaID] = record.AcquisitionID
		if other := s.bySession[record.SessionID]; other != "" && other != record.AcquisitionID {
			return fmt.Errorf("runtime/workarea: duplicate durable session id %q", record.SessionID)
		}
		s.bySession[record.SessionID] = record.AcquisitionID
		for _, participant := range record.Participants {
			if other := s.bySession[participant.SessionID]; other != "" && other != record.AcquisitionID {
				return fmt.Errorf("runtime/workarea: duplicate durable participant id %q", participant.SessionID)
			}
			s.bySession[participant.SessionID] = record.AcquisitionID
		}
		if record.State == AcquisitionClaiming || record.State == AcquisitionProvisioning || record.State == AcquisitionRestoring {
			ownerLock, acquired, err := s.tryOwnerLock(record.AcquisitionID)
			if err != nil {
				return err
			}
			if !acquired {
				continue
			}
			if err := s.reconcileRecord(&record); err != nil {
				releaseFlock(ownerLock)
				return err
			}
			releaseFlock(ownerLock)
			continue
		}
		if err := s.reconcileRecord(&record); err != nil {
			return err
		}
	}
	return nil
}

func (s *AcquisitionStore) recoverUnpublishedClaim(claimLeaf string) error {
	root, err := s.root.OpenRoot(claimLeaf)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open unpublished acquisition claim: %w", err)
	}
	lock, err := openLockFile(root, acquisitionOwnerLockName)
	if err != nil {
		_ = root.Close()
		return err
	}
	if err := flockExclusive(lock, true); err != nil {
		_ = lock.Close()
		_ = root.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		return err
	}
	releaseFlock(lock)
	_ = root.Close()
	if err := s.root.RemoveAll(claimLeaf); err != nil {
		return fmt.Errorf("runtime/workarea: remove abandoned unpublished acquisition claim: %w", err)
	}
	return syncRoot(s.root)
}

func (s *AcquisitionStore) reconcileRecord(record *AcquisitionRecord) error {
	staging := filepath.Join(record.AcquisitionID, acquisitionStagingRootName)
	privateReleased := filepath.Join(record.AcquisitionID, acquisitionReleasedRootName)
	_, stagingErr := s.root.Lstat(staging)
	_, finalErr := s.parentRoot.Lstat(filepath.Base(record.FinalRoot))
	if _, privateErr := s.root.Lstat(privateReleased); privateErr == nil {
		return s.finishPrivateDisposal(record)
	} else if !errors.Is(privateErr, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: inspect private disposal root: %w", privateErr)
	}
	if record.State == AcquisitionRestoring {
		restoreRelative := filepath.Join(record.AcquisitionID, acquisitionRestoreRootName)
		_, restoreErr := s.root.Lstat(restoreRelative)
		if finalErr == nil {
			if record.RootIdentity.Empty() {
				record.State = AcquisitionQuarantined
				record.LastError = "restored final root exists without a pre-publish identity"
			} else if err := s.verifyFinal(*record); err != nil {
				record.State = AcquisitionQuarantined
				record.LastError = err.Error()
			} else {
				record.State = AcquisitionReady
				record.OwnerReleased = false
				record.Participants = nil
				record.LastError = ""
			}
			record.RestoreArchiveID = ""
			record.RestoreDigest = ""
			record.UpdatedAt = s.now().UTC()
			return s.writeRecord(*record)
		}
		if !errors.Is(finalErr, fs.ErrNotExist) {
			return fmt.Errorf("runtime/workarea: inspect restoring final root: %w", finalErr)
		}
		if restoreErr == nil {
			root, err := s.root.OpenRoot(record.AcquisitionID)
			if err != nil {
				return err
			}
			if err := root.RemoveAll(acquisitionRestoreRootName); err != nil {
				_ = root.Close()
				return err
			}
			_ = root.Close()
		} else if !errors.Is(restoreErr, fs.ErrNotExist) {
			return fmt.Errorf("runtime/workarea: inspect restore stage: %w", restoreErr)
		}
		record.State = AcquisitionReleased
		record.RootIdentity = FileIdentity{}
		record.RestoreArchiveID = ""
		record.RestoreDigest = ""
		record.UpdatedAt = s.now().UTC()
		return s.writeRecord(*record)
	}
	if record.State == AcquisitionReleased || record.State == AcquisitionAborted {
		if finalErr == nil {
			record.State = AcquisitionQuarantined
			record.LastError = "a final root exists for a terminal acquisition state"
			record.UpdatedAt = s.now().UTC()
			return s.writeRecord(*record)
		}
		if !errors.Is(finalErr, fs.ErrNotExist) {
			return fmt.Errorf("runtime/workarea: inspect terminal acquisition root: %w", finalErr)
		}
		return nil
	}
	if record.State == AcquisitionQuarantined {
		return nil
	}
	if record.State == AcquisitionReady {
		if err := s.verifyFinal(*record); err != nil {
			record.State = AcquisitionQuarantined
			record.LastError = err.Error()
			record.UpdatedAt = s.now().UTC()
			return s.writeRecord(*record)
		}
		return nil
	}
	if finalErr == nil {
		if record.RootIdentity.Empty() {
			record.State = AcquisitionQuarantined
			record.LastError = "final root exists without a pre-publish filesystem identity"
			record.UpdatedAt = s.now().UTC()
			return s.writeRecord(*record)
		}
		if err := s.verifyFinal(*record); err != nil {
			record.State = AcquisitionQuarantined
			record.LastError = err.Error()
		} else {
			record.State = AcquisitionReady
			record.LastError = ""
		}
		record.UpdatedAt = s.now().UTC()
		return s.writeRecord(*record)
	}
	if !errors.Is(finalErr, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: inspect recovery root: %w", finalErr)
	}
	if stagingErr == nil {
		root, err := s.root.OpenRoot(record.AcquisitionID)
		if err != nil {
			return err
		}
		if err := root.RemoveAll(acquisitionStagingRootName); err != nil {
			_ = root.Close()
			return fmt.Errorf("runtime/workarea: recover proved staging root: %w", err)
		}
		_ = root.Close()
	} else if !errors.Is(stagingErr, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: inspect staging root: %w", stagingErr)
	}
	record.State = AcquisitionAborted
	record.UpdatedAt = s.now().UTC()
	return s.writeRecord(*record)
}

func (s *AcquisitionStore) finishPrivateDisposal(record *AcquisitionRecord) error {
	acquisitionRoot, err := s.root.OpenRoot(record.AcquisitionID)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition for disposal recovery: %w", err)
	}
	defer func() { _ = acquisitionRoot.Close() }()
	privateRoot, err := acquisitionRoot.OpenRoot(acquisitionReleasedRootName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open private disposal root: %w", err)
	}
	identityInfo, err := privateRoot.Stat(".")
	if err != nil {
		_ = privateRoot.Close()
		return fmt.Errorf("runtime/workarea: stat private disposal root: %w", err)
	}
	identity, err := fileIdentity(identityInfo)
	if err != nil {
		_ = privateRoot.Close()
		return err
	}
	declaration, declarationErr := readDeclarationFromRoot(privateRoot)
	_ = privateRoot.Close()
	if declarationErr != nil || record.RootIdentity.Empty() || identity != record.RootIdentity || declaration.AcquisitionID != record.AcquisitionID || declaration.WorkareaID != record.WorkareaID || declaration.SessionID != record.SessionID {
		record.State = AcquisitionQuarantined
		if declarationErr != nil {
			record.LastError = declarationErr.Error()
		} else {
			record.LastError = "private disposal root ownership mismatch"
		}
		record.UpdatedAt = s.now().UTC()
		return s.writeRecord(*record)
	}
	if err := acquisitionRoot.RemoveAll(acquisitionReleasedRootName); err != nil {
		return fmt.Errorf("runtime/workarea: finish proved root disposal: %w", err)
	}
	if err := syncRoot(acquisitionRoot); err != nil {
		return err
	}
	record.State = AcquisitionReleased
	record.LastError = ""
	record.UpdatedAt = s.now().UTC()
	return s.writeRecord(*record)
}

// ReadyRecords returns verified published generations in stable order.
func (s *AcquisitionStore) ReadyRecords() ([]AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return nil, err
	}
	defer releaseStore()
	entries, err := s.readDirRoot()
	if err != nil {
		return nil, err
	}
	var records []AcquisitionRecord
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "wac_") {
			continue
		}
		record, err := s.readRecord(entry.Name())
		if err != nil {
			return nil, err
		}
		if record.State == AcquisitionReady {
			if err := s.verifyFinal(record); err != nil {
				return nil, err
			}
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].AcquisitionID < records[j].AcquisitionID })
	return records, nil
}

// RecordForWorkareaID resolves a durable workarea identity without walking the
// session-root filesystem. The index is rebuilt from the acquisition journal
// before admission on every manager restart.
func (s *AcquisitionStore) RecordForWorkareaID(workareaID string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return AcquisitionRecord{}, err
	}
	return s.findByWorkareaID(workareaID)
}

// RecordForAcquisitionID returns one exact durable generation in any state.
func (s *AcquisitionStore) RecordForAcquisitionID(acquisitionID string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	return s.readRecord(acquisitionID)
}

// RecordForSessionID resolves either an owner or shared participant through the
// durable acquisition journal. It never searches session roots.
func (s *AcquisitionStore) RecordForSessionID(sessionID string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return AcquisitionRecord{}, err
	}
	acquisitionID := s.bySession[sessionID]
	if acquisitionID == "" {
		return AcquisitionRecord{}, fmt.Errorf("%w: session %q", ErrAcquisitionNotFound, sessionID)
	}
	return s.readRecord(acquisitionID)
}

// AuthorizeRoot proves that one acquisition owns the exact published root.
func (s *AcquisitionStore) AuthorizeRoot(acquisitionID string, root RootPath) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if record.State != AcquisitionReady || filepath.Clean(record.FinalRoot) != filepath.Clean(root.String()) {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: acquisition does not authorize root")
	}
	if err := s.verifyFinal(record); err != nil {
		return AcquisitionRecord{}, err
	}
	return record, nil
}

// JoinShared durably adds a participant to an owner generation.
func (s *AcquisitionStore) JoinShared(parentWorkareaID, sessionID, selectedRepository string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return AcquisitionRecord{}, err
	}
	record, err := s.findByWorkareaID(parentWorkareaID)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if record.State != AcquisitionReady || record.OwnerReleased {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: parent generation is not joinable")
	}
	if sessionID == record.SessionID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: root owner cannot join as a shared participant")
	}
	if existing := s.bySession[sessionID]; existing != "" && existing != record.AcquisitionID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: session %q already belongs to acquisition %q", sessionID, existing)
	}
	declaration, err := s.verifiedFinalDeclaration(record)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	selectedDeclared := false
	for _, repository := range declaration.Repositories {
		selectedDeclared = selectedDeclared || repository.Name == selectedRepository
	}
	if !selectedDeclared {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: shared participant selection is not declared")
	}
	for _, participant := range record.Participants {
		if participant.SessionID == sessionID {
			if participant.SelectedRepository == selectedRepository {
				return record, nil
			}
			return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: participant selection conflict")
		}
	}
	record.Participants = append(record.Participants, Participant{
		SessionID: sessionID, ParentWorkareaID: parentWorkareaID, SelectedRepository: selectedRepository,
	})
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return AcquisitionRecord{}, err
	}
	s.bySession[sessionID] = record.AcquisitionID
	return record, nil
}

// LeaveShared removes a participant and reports whether owner release can run.
func (s *AcquisitionStore) LeaveShared(workareaID, sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return false, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return false, err
	}
	record, err := s.findByWorkareaID(workareaID)
	if err != nil {
		return false, err
	}
	participants := record.Participants[:0]
	for _, participant := range record.Participants {
		if participant.SessionID != sessionID {
			participants = append(participants, participant)
		}
	}
	record.Participants = participants
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return false, err
	}
	delete(s.bySession, sessionID)
	return record.OwnerReleased && len(record.Participants) == 0, nil
}

// RequestOwnerRelease holds the root while shared participants remain.
func (s *AcquisitionStore) RequestOwnerRelease(workareaID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return false, err
	}
	defer releaseStore()
	if err := s.refreshIndexLocked(); err != nil {
		return false, err
	}
	record, err := s.findByWorkareaID(workareaID)
	if err != nil {
		return false, err
	}
	record.OwnerReleased = true
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return false, err
	}
	return len(record.Participants) == 0, nil
}

// BindArchive records the immutable archive digest while the exact source root
// is still ready and identity-verifiable.
func (s *AcquisitionStore) BindArchive(acquisitionID, workareaID, sessionID, archiveID, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return err
	}
	if record.State != AcquisitionReady || record.WorkareaID != workareaID || record.SessionID != sessionID || archiveID == "" || digest == "" {
		return fmt.Errorf("runtime/workarea: archive binding does not match a ready acquisition")
	}
	if err := s.verifyFinal(record); err != nil {
		return err
	}
	if record.ArchiveID != "" && (record.ArchiveID != archiveID || record.ArchiveDigest != digest) {
		return fmt.Errorf("runtime/workarea: acquisition already binds another archive")
	}
	record.ArchiveID = archiveID
	record.ArchiveDigest = digest
	record.UpdatedAt = s.now().UTC()
	return s.writeRecord(record)
}

// MarkReleased records completion after exact root disposal.
func (s *AcquisitionStore) MarkReleased(acquisitionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return err
	}
	record.State = AcquisitionReleased
	record.UpdatedAt = s.now().UTC()
	return s.writeRecord(record)
}

// BeginRestore durably binds an immutable archive before creating its private
// restore stage. The authoritative final path comes only from the acquisition.
func (s *AcquisitionStore) BeginRestore(acquisitionID, workareaID, sessionID, archiveID, archiveDigest string) (RestoreAcquisition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return RestoreAcquisition{}, err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return RestoreAcquisition{}, err
	}
	if record.State != AcquisitionReleased || record.WorkareaID != workareaID || record.SessionID != sessionID || archiveID == "" || archiveDigest == "" || record.ArchiveID != archiveID || record.ArchiveDigest != archiveDigest {
		return RestoreAcquisition{}, fmt.Errorf("runtime/workarea: archive restore does not match a released acquisition")
	}
	if _, err := s.parentRoot.Lstat(filepath.Base(record.FinalRoot)); err == nil {
		return RestoreAcquisition{}, fmt.Errorf("%w: %s", ErrAcquisitionRootOccupied, record.FinalRoot)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return RestoreAcquisition{}, err
	}
	root, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return RestoreAcquisition{}, err
	}
	defer func() { _ = root.Close() }()
	if s.owners[acquisitionID] != nil {
		return RestoreAcquisition{}, fmt.Errorf("runtime/workarea: acquisition restore is already owned")
	}
	ownerLock, err := openLockFile(root, acquisitionOwnerLockName)
	if err != nil {
		return RestoreAcquisition{}, err
	}
	if err := flockExclusive(ownerLock, false); err != nil {
		_ = ownerLock.Close()
		return RestoreAcquisition{}, err
	}
	owned := false
	defer func() {
		if !owned {
			releaseFlock(ownerLock)
		}
	}()
	s.owners[acquisitionID] = ownerLock
	if err := root.RemoveAll(acquisitionRestoreRootName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		delete(s.owners, acquisitionID)
		return RestoreAcquisition{}, err
	}
	record.State = AcquisitionRestoring
	record.RootIdentity = FileIdentity{}
	record.RestoreArchiveID = archiveID
	record.RestoreDigest = archiveDigest
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		delete(s.owners, acquisitionID)
		return RestoreAcquisition{}, err
	}
	if err := root.Mkdir(acquisitionRestoreRootName, 0o750); err != nil {
		delete(s.owners, acquisitionID)
		return RestoreAcquisition{}, err
	}
	if err := syncRoot(root); err != nil {
		delete(s.owners, acquisitionID)
		return RestoreAcquisition{}, err
	}
	owned = true
	return RestoreAcquisition{
		Record:      record,
		StagingRoot: RootPath(filepath.Join(s.dir, acquisitionID, acquisitionRestoreRootName)),
	}, nil
}

// CommitRestore publishes the fully copied declared root without replacement.
func (s *AcquisitionStore) CommitRestore(acquisitionID string) (AcquisitionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[acquisitionID] == nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: restore is not owned by this process")
	}
	releaseStore, err := s.lockStore()
	if err != nil {
		return AcquisitionRecord{}, err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if record.State != AcquisitionRestoring {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: acquisition is not restoring")
	}
	stagingRelative := filepath.Join(acquisitionID, acquisitionRestoreRootName)
	stagingInfo, err := s.root.Lstat(stagingRelative)
	if err != nil || !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: restore stage is not a real directory")
	}
	stagingRoot := RootPath(filepath.Join(s.dir, stagingRelative))
	declaration, err := ReadDeclaration(stagingRoot)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if declaration.AcquisitionID != acquisitionID || declaration.WorkareaID != record.WorkareaID || declaration.SessionID != record.SessionID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: restored declaration identity mismatch")
	}
	if err := ValidateDeclaredRoot(stagingRoot, declaration); err != nil {
		return AcquisitionRecord{}, err
	}
	identity, err := fileIdentity(stagingInfo)
	if err != nil {
		return AcquisitionRecord{}, err
	}
	record.RootIdentity = identity
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return AcquisitionRecord{}, err
	}
	if err := renameNoReplace(stagingRoot.String(), record.FinalRoot); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return AcquisitionRecord{}, fmt.Errorf("%w: %s", ErrAcquisitionRootOccupied, record.FinalRoot)
		}
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: publish restored root: %w", err)
	}
	if err := syncRoot(s.parentRoot); err != nil {
		return AcquisitionRecord{}, err
	}
	publishedInfo, err := s.parentRoot.Lstat(filepath.Base(record.FinalRoot))
	if err != nil || !os.SameFile(stagingInfo, publishedInfo) {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: restored root identity changed during publish")
	}
	record.State = AcquisitionReady
	record.OwnerReleased = false
	record.Participants = nil
	record.LastError = ""
	record.RestoreArchiveID = ""
	record.RestoreDigest = ""
	record.UpdatedAt = s.now().UTC()
	if err := s.writeRecord(record); err != nil {
		return AcquisitionRecord{}, err
	}
	s.releaseOwnerLocked(acquisitionID)
	return record, nil
}

// AbortRestore removes only the proved private stage when no final root exists.
func (s *AcquisitionStore) AbortRestore(acquisitionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[acquisitionID] == nil {
		return fmt.Errorf("runtime/workarea: restore is not owned by this process")
	}
	defer s.releaseOwnerLocked(acquisitionID)
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return err
	}
	if record.State != AcquisitionRestoring {
		return fmt.Errorf("runtime/workarea: acquisition is not restoring")
	}
	if _, err := s.parentRoot.Lstat(filepath.Base(record.FinalRoot)); err == nil {
		return s.reconcileRecord(&record)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	root, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return err
	}
	if err := root.RemoveAll(acquisitionRestoreRootName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = root.Close()
		return err
	}
	_ = root.Close()
	record.State = AcquisitionReleased
	record.RootIdentity = FileIdentity{}
	record.RestoreArchiveID = ""
	record.RestoreDigest = ""
	record.UpdatedAt = s.now().UTC()
	return s.writeRecord(record)
}

// RemovePublishedRoot disposes only the exact root identity and declaration
// owned by acquisitionID. The root is first atomically moved into the private
// acquisition directory; a mismatched object is quarantined there and is never
// deleted.
func (s *AcquisitionStore) RemovePublishedRoot(acquisitionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseStore, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseStore()
	record, err := s.readRecord(acquisitionID)
	if err != nil {
		return err
	}
	if record.State != AcquisitionReady || record.RootIdentity.Empty() {
		return fmt.Errorf("runtime/workarea: acquisition does not authorize root disposal")
	}
	if !directChild(s.parent, record.FinalRoot) {
		return fmt.Errorf("runtime/workarea: published root escaped acquisition parent")
	}
	parentRoot := s.parentRoot
	finalLeaf := filepath.Base(record.FinalRoot)
	pathIdentity, err := parentRoot.Lstat(finalLeaf)
	if err != nil {
		return fmt.Errorf("runtime/workarea: inspect published root for disposal: %w", err)
	}
	identity, err := fileIdentity(pathIdentity)
	if err != nil {
		return err
	}
	if !pathIdentity.IsDir() || pathIdentity.Mode()&os.ModeSymlink != 0 || identity != record.RootIdentity {
		return fmt.Errorf("runtime/workarea: published root identity changed before disposal")
	}
	privateRelative := filepath.Join(acquisitionStoreDirName, acquisitionID, acquisitionReleasedRootName)
	if _, err := parentRoot.Lstat(privateRelative); err == nil {
		return fmt.Errorf("runtime/workarea: private disposal root already exists")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: inspect private disposal root: %w", err)
	}
	if err := renameNoReplace(record.FinalRoot, filepath.Join(s.parent, privateRelative)); err != nil {
		return fmt.Errorf("runtime/workarea: quarantine published root for disposal: %w", err)
	}
	if err := syncRoot(parentRoot); err != nil {
		return err
	}
	privateRoot, err := parentRoot.OpenRoot(privateRelative)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open quarantined root: %w", err)
	}
	defer func() { _ = privateRoot.Close() }()
	openedIdentity, err := privateRoot.Stat(".")
	if err != nil {
		return fmt.Errorf("runtime/workarea: stat quarantined root: %w", err)
	}
	openedFileIdentity, err := fileIdentity(openedIdentity)
	if err != nil {
		return err
	}
	if !os.SameFile(pathIdentity, openedIdentity) || openedFileIdentity != record.RootIdentity {
		record.State = AcquisitionQuarantined
		record.LastError = "root identity changed during disposal quarantine"
		record.UpdatedAt = s.now().UTC()
		if writeErr := s.writeRecord(record); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("runtime/workarea: quarantined root identity mismatch; deletion refused")
	}
	declaration, err := readDeclarationFromRoot(privateRoot)
	if err != nil || declaration.AcquisitionID != acquisitionID || declaration.WorkareaID != record.WorkareaID || declaration.SessionID != record.SessionID {
		record.State = AcquisitionQuarantined
		if err != nil {
			record.LastError = err.Error()
		} else {
			record.LastError = "root declaration changed during disposal quarantine"
		}
		record.UpdatedAt = s.now().UTC()
		if writeErr := s.writeRecord(record); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("runtime/workarea: quarantined root ownership mismatch; deletion refused")
	}
	acquisitionRoot, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition for root disposal: %w", err)
	}
	if err := acquisitionRoot.RemoveAll(acquisitionReleasedRootName); err != nil {
		_ = acquisitionRoot.Close()
		return fmt.Errorf("runtime/workarea: remove proved published root: %w", err)
	}
	if err := syncRoot(acquisitionRoot); err != nil {
		_ = acquisitionRoot.Close()
		return err
	}
	_ = acquisitionRoot.Close()
	record.State = AcquisitionReleased
	record.UpdatedAt = s.now().UTC()
	record.LastError = ""
	return s.writeRecord(record)
}

func (s *AcquisitionStore) verifyFinal(record AcquisitionRecord) error {
	_, err := s.verifiedFinalDeclaration(record)
	return err
}

func (s *AcquisitionStore) verifiedFinalDeclaration(record AcquisitionRecord) (DeclarationRecord, error) {
	if record.RootIdentity.Empty() {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: published root has no durable identity")
	}
	root, err := OpenRootExact(RootPath(record.FinalRoot), record.RootIdentity)
	if err != nil {
		return DeclarationRecord{}, err
	}
	defer func() { _ = root.Close() }()
	declaration, err := ReadDeclarationRoot(root)
	if err != nil {
		return DeclarationRecord{}, err
	}
	if err := ValidateDeclaredRootHandle(root, declaration); err != nil {
		return DeclarationRecord{}, err
	}
	if declaration.AcquisitionID != record.AcquisitionID || declaration.WorkareaID != record.WorkareaID || declaration.SessionID != record.SessionID {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: published declaration ownership mismatch")
	}
	selectedLeaf := ""
	declaredNames := make(map[string]struct{}, len(declaration.Repositories))
	for _, repository := range declaration.Repositories {
		declaredNames[repository.Name] = struct{}{}
		if repository.Name == declaration.SelectedRepository {
			selectedLeaf = repository.Leaf
		}
	}
	if selectedLeaf != record.SelectedLeaf {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: published selected leaf mismatch")
	}
	for _, participant := range record.Participants {
		if _, declared := declaredNames[participant.SelectedRepository]; !declared {
			return DeclarationRecord{}, fmt.Errorf("runtime/workarea: shared participant selection is not declared")
		}
	}
	return declaration, nil
}

func (s *AcquisitionStore) findByWorkareaID(workareaID string) (AcquisitionRecord, error) {
	acquisitionID := s.byWorkarea[workareaID]
	if acquisitionID == "" {
		return AcquisitionRecord{}, fmt.Errorf("%w: workarea %q", ErrAcquisitionNotFound, workareaID)
	}
	return s.readRecord(acquisitionID)
}

func (s *AcquisitionStore) refreshIndexLocked() error {
	entries, err := s.readDirRoot()
	if err != nil {
		return fmt.Errorf("runtime/workarea: refresh acquisition index: %w", err)
	}
	index := make(map[string]string)
	sessions := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "wac_") {
			continue
		}
		record, err := s.readRecord(entry.Name())
		if err != nil {
			return err
		}
		if record.State == AcquisitionAborted {
			continue
		}
		if other, duplicate := index[record.WorkareaID]; duplicate && other != record.AcquisitionID {
			return fmt.Errorf("runtime/workarea: duplicate durable workarea id %q", record.WorkareaID)
		}
		index[record.WorkareaID] = record.AcquisitionID
		for _, sessionID := range append([]string{record.SessionID}, participantSessionIDs(record.Participants)...) {
			if other, duplicate := sessions[sessionID]; duplicate && other != record.AcquisitionID {
				return fmt.Errorf("runtime/workarea: duplicate durable session id %q", sessionID)
			}
			sessions[sessionID] = record.AcquisitionID
		}
	}
	s.byWorkarea = index
	s.bySession = sessions
	return nil
}

func participantSessionIDs(participants []Participant) []string {
	result := make([]string, 0, len(participants))
	for _, participant := range participants {
		result = append(result, participant.SessionID)
	}
	return result
}

func (s *AcquisitionStore) lockStore() (func(), error) {
	if err := s.assertStoreIdentity(); err != nil {
		return nil, err
	}
	lock, err := openLockFile(s.root, acquisitionStoreLockName)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: open acquisition store lock: %w", err)
	}
	if err := flockExclusive(lock, false); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("runtime/workarea: lock acquisition store: %w", err)
	}
	if err := s.assertStoreIdentity(); err != nil {
		releaseFlock(lock)
		return nil, err
	}
	return func() { releaseFlock(lock) }, nil
}

func (s *AcquisitionStore) tryOwnerLock(acquisitionID string) (*os.File, bool, error) {
	root, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: open acquisition for recovery lock: %w", err)
	}
	lock, err := openLockFile(root, acquisitionOwnerLockName)
	_ = root.Close()
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: open acquisition recovery lock: %w", err)
	}
	if err := flockExclusive(lock, true); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, false, nil
		}
		_ = lock.Close()
		return nil, false, fmt.Errorf("runtime/workarea: lock acquisition for recovery: %w", err)
	}
	return lock, true, nil
}

func (s *AcquisitionStore) releaseOwnerLocked(acquisitionID string) {
	lock := s.owners[acquisitionID]
	if lock == nil {
		return
	}
	delete(s.owners, acquisitionID)
	releaseFlock(lock)
}

func flockExclusive(file *os.File, nonblocking bool) error {
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	fd, err := intFileDescriptor(file.Fd())
	if err != nil {
		return err
	}
	return syscall.Flock(fd, operation)
}

func releaseFlock(file *os.File) {
	if file == nil {
		return
	}
	if fd, err := intFileDescriptor(file.Fd()); err == nil {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
	}
	_ = file.Close()
}

func (s *AcquisitionStore) readRecord(acquisitionID string) (AcquisitionRecord, error) {
	root, err := s.root.OpenRoot(acquisitionID)
	if err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: open acquisition record: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(acquisitionRecordFileName)
	if err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: read acquisition record: %w", err)
	}
	var record AcquisitionRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: decode acquisition record: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return AcquisitionRecord{}, err
	}
	if err := record.Validate(); err != nil {
		return AcquisitionRecord{}, err
	}
	if record.AcquisitionID != acquisitionID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: acquisition directory identity mismatch")
	}
	if record.StoreID != s.storeID {
		return AcquisitionRecord{}, fmt.Errorf("runtime/workarea: acquisition record belongs to another store")
	}
	return record, nil
}

func (s *AcquisitionStore) writeRecord(record AcquisitionRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	root, err := s.root.OpenRoot(record.AcquisitionID)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open acquisition for write: %w", err)
	}
	defer func() { _ = root.Close() }()
	return writeAcquisitionRecordRoot(root, record)
}

func writeAcquisitionRecordRoot(root *os.Root, record AcquisitionRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tempName, err := rootedTempName(".acquisition-")
	if err != nil {
		return err
	}
	temp, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tempName) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := root.Rename(tempName, acquisitionRecordFileName); err != nil {
		return err
	}
	return syncRoot(root)
}

func syncRoot(root *os.Root) error {
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

func rootedTempName(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]) + ".tmp", nil
}

func newAcquisitionID() (string, error) {
	return newGenerationID("wac_")
}

func newGenerationID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("runtime/workarea: generate durable generation id: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func directChild(parent, child string) bool {
	return filepath.Clean(filepath.Dir(child)) == filepath.Clean(parent) && filepath.Base(child) != "." && filepath.Base(child) != ".."
}
