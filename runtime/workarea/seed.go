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
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	seedStoreDirName     = ".workarea-seeds"
	seedRecordFileName   = "seed.json"
	seedStoreLockName    = ".store.lock"
	seedRecordSchemaV1   = "donmai.workarea-seed.v1"
	seedClaimFileName    = ".claim.json"
	seedClaimSchemaV1    = "donmai.workarea-seed-claim.v1"
	seedRecoveryPrefix   = "recovery-"
	seedRecoverySchemaV1 = "donmai.workarea-seed-recovery.v1"
)

// SeedRepository is one secret-free repository held by a reusable cache seed.
type SeedRepository struct {
	Name         string   `json:"name"`
	Leaf         string   `json:"leaf"`
	RequestedRef string   `json:"requestedRef,omitempty"`
	SourceDigest string   `json:"sourceDigest"`
	SparsePaths  []string `json:"sparsePaths,omitempty"`
}

// SeedRecord is the durable identity and separate physical charge for a cache
// object. It contains no session, lease, cursor, acquisition, or source URL.
type SeedRecord struct {
	SchemaVersion string           `json:"schemaVersion"`
	SeedID        string           `json:"seedId"`
	Repositories  []SeedRepository `json:"repositories"`
	PhysicalBytes int64            `json:"physicalBytes"`
	CreatedAt     time.Time        `json:"createdAt"`
}

type seedClaimRecord struct {
	SchemaVersion string    `json:"schemaVersion"`
	ClaimID       string    `json:"claimId"`
	SeedID        string    `json:"seedId"`
	StageLeaf     string    `json:"stageLeaf"`
	PhysicalBytes int64     `json:"physicalBytes"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (r seedClaimRecord) validate() error {
	if r.SchemaVersion != seedClaimSchemaV1 || !strings.HasPrefix(r.ClaimID, "wseedclaim_") || r.CreatedAt.IsZero() || r.PhysicalBytes < 0 {
		return fmt.Errorf("runtime/workarea: invalid durable seed claim")
	}
	if err := ValidateRepositoryLeaf(r.SeedID); err != nil {
		return fmt.Errorf("runtime/workarea: invalid durable seed claim: %w", err)
	}
	prefix := ".seed-" + r.SeedID + "-"
	if filepath.Base(r.StageLeaf) != r.StageLeaf || !strings.HasPrefix(r.StageLeaf, prefix) || len(r.StageLeaf) == len(prefix) {
		return fmt.Errorf("runtime/workarea: invalid durable seed stage identity")
	}
	return nil
}

// SeedRecoveryRecord is durable accounting for one crash-left warming stage.
type SeedRecoveryRecord struct {
	SchemaVersion string    `json:"schemaVersion"`
	ClaimID       string    `json:"claimId"`
	SeedID        string    `json:"seedId"`
	PhysicalBytes int64     `json:"physicalBytes"`
	RecoveredAt   time.Time `json:"recoveredAt"`
}

// SeedStore owns reusable material outside every session root.
type SeedStore struct {
	parentRoot *os.Root
	root       *os.Root
	identity   os.FileInfo
	dir        string
	now        func() time.Time
	mu         sync.Mutex
}

// NewSeedStore creates the host-owned cache namespace.
func NewSeedStore(parent string, now func() time.Time) (*SeedStore, error) {
	store, _, err := openSeedStore(parent, now, true)
	return store, err
}

// OpenExistingSeedStore opens cache authority without activating it on a
// legacy-flat-only host.
func OpenExistingSeedStore(parent string, now func() time.Time) (*SeedStore, bool, error) {
	return openSeedStore(parent, now, false)
}

func openSeedStore(parent string, now func() time.Time, create bool) (*SeedStore, bool, error) {
	abs, err := filepath.Abs(parent)
	if err != nil {
		return nil, false, fmt.Errorf("runtime/workarea: resolve seed parent: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	parentRoot, err := os.OpenRoot(abs)
	if err != nil {
		return nil, false, err
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = parentRoot.Close()
		}
	}()
	dir := filepath.Join(abs, seedStoreDirName)
	if create {
		if err := parentRoot.MkdirAll(seedStoreDirName, 0o700); err != nil {
			return nil, false, fmt.Errorf("runtime/workarea: create seed store: %w", err)
		}
	}
	info, err := parentRoot.Lstat(seedStoreDirName)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, false, fmt.Errorf("runtime/workarea: seed store is not a private real directory")
	}
	root, err := parentRoot.OpenRoot(seedStoreDirName)
	if err != nil {
		return nil, false, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, false, fmt.Errorf("runtime/workarea: seed store identity changed while opening")
	}
	store := &SeedStore{parentRoot: parentRoot, root: root, identity: opened, dir: dir, now: now}
	if err := store.recoverClaim(); err != nil {
		_ = root.Close()
		return nil, false, err
	}
	closeParent = false
	return store, true, nil
}

func (s *SeedStore) assertStoreIdentity() error {
	current, err := s.parentRoot.Lstat(seedStoreDirName)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, s.identity) {
		return fmt.Errorf("runtime/workarea: seed store directory identity changed")
	}
	return nil
}

func (s *SeedStore) recoverClaim() error {
	lock, err := s.lockStore()
	if err != nil {
		return err
	}
	defer releaseFlock(lock)
	return s.recoverClaimLocked()
}

func (s *SeedStore) recoverClaimLocked() error {
	if _, err := s.root.Lstat(seedClaimFileName); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	body, err := s.root.ReadFile(seedClaimFileName)
	if err != nil {
		return err
	}
	var claim seedClaimRecord
	if err := decodeClosedJSON(body, &claim); err != nil {
		return fmt.Errorf("runtime/workarea: invalid durable seed claim")
	}
	if err := claim.validate(); err != nil {
		return err
	}
	return s.recoverClaimRecord(claim)
}

func (s *SeedStore) recoverClaimRecord(claim seedClaimRecord) error {
	if err := claim.validate(); err != nil {
		return err
	}
	physical := int64(0)
	if info, err := s.root.Lstat(claim.StageLeaf); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime/workarea: durable seed stage is not a real directory")
		}
		stageRoot, err := s.root.OpenRoot(claim.StageLeaf)
		if err != nil {
			return err
		}
		opened, err := stageRoot.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = stageRoot.Close()
			return fmt.Errorf("runtime/workarea: durable seed stage identity changed while opening")
		}
		physical, err = PhysicalUsageRoot(stageRoot)
		_ = stageRoot.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	recovery := SeedRecoveryRecord{
		SchemaVersion: seedRecoverySchemaV1, ClaimID: claim.ClaimID, SeedID: claim.SeedID,
		PhysicalBytes: physical, RecoveredAt: s.now().UTC(),
	}
	if err := s.writeStoreJSON(seedRecoveryPrefix+claim.ClaimID+".json", recovery); err != nil {
		return err
	}
	if err := s.root.RemoveAll(claim.StageLeaf); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.root.Remove(seedClaimFileName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncRoot(s.root)
}

// Recoveries returns stable durable seed-staging cleanup accounting.
func (s *SeedStore) Recoveries() ([]SeedRecoveryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lockStore()
	if err != nil {
		return nil, err
	}
	defer releaseFlock(lock)
	directory, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil {
		return nil, err
	}
	var result []SeedRecoveryRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), seedRecoveryPrefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := s.root.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		var record SeedRecoveryRecord
		if err := decodeClosedJSON(body, &record); err != nil || record.SchemaVersion != seedRecoverySchemaV1 {
			return nil, fmt.Errorf("runtime/workarea: invalid seed recovery record")
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ClaimID < result[j].ClaimID })
	return result, nil
}

func (s *SeedStore) writeStoreJSON(name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := rootedTempName(".seed-store-")
	if err != nil {
		return err
	}
	file, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(temp) }()
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
	if err := s.root.Rename(temp, name); err != nil {
		return err
	}
	return syncRoot(s.root)
}

// Ensure materialises one seed exactly once. The callback receives a validated
// repository and a destination beneath a private atomic stage.
func (s *SeedStore) Ensure(
	ctx context.Context,
	seedID string,
	declaration NormalizedDeclaration,
	materialize func(context.Context, NormalizedRepository, string) error,
) (SeedRecord, map[string]string, error) {
	if err := ValidateRepositoryLeaf(seedID); err != nil {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: invalid seed identity: %w", err)
	}
	if materialize == nil {
		return SeedRecord{}, nil, errors.New("runtime/workarea: seed materializer is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lockStore()
	if err != nil {
		return SeedRecord{}, nil, err
	}
	defer releaseFlock(lock)
	if err := s.recoverClaimLocked(); err != nil {
		return SeedRecord{}, nil, err
	}
	if record, paths, err := s.load(seedID, declaration); err == nil {
		return record, paths, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return SeedRecord{}, nil, err
	}
	random, err := seedRandomSuffix()
	if err != nil {
		return SeedRecord{}, nil, err
	}
	stageLeaf := ".seed-" + seedID + "-" + random
	stage := filepath.Join(s.dir, stageLeaf)
	claimID, err := newGenerationID("wseedclaim_")
	if err != nil {
		return SeedRecord{}, nil, err
	}
	claim := seedClaimRecord{
		SchemaVersion: seedClaimSchemaV1, ClaimID: claimID, SeedID: seedID,
		StageLeaf: stageLeaf, CreatedAt: s.now().UTC(),
	}
	if err := s.writeStoreJSON(seedClaimFileName, claim); err != nil {
		return SeedRecord{}, nil, err
	}
	if err := s.root.Mkdir(stageLeaf, 0o700); err != nil {
		_ = s.recoverClaimRecord(claim)
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: create seed stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.recoverClaimRecord(claim)
		}
	}()
	repositories := append([]NormalizedRepository(nil), declaration.Repositories...)
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Name < repositories[j].Name })
	record := SeedRecord{
		SchemaVersion: seedRecordSchemaV1, SeedID: seedID,
		Repositories: make([]SeedRepository, 0, len(repositories)), CreatedAt: s.now().UTC(),
	}
	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return SeedRecord{}, nil, err
		}
		destination := filepath.Join(stage, repository.Leaf)
		if err := materialize(ctx, repository, destination); err != nil {
			return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: materialize seed repository %q: %w", repository.Name, err)
		}
		stageRoot, err := s.root.OpenRoot(stageLeaf)
		if err != nil {
			return SeedRecord{}, nil, err
		}
		claim.PhysicalBytes, err = PhysicalUsageRoot(stageRoot)
		_ = stageRoot.Close()
		if err != nil {
			return SeedRecord{}, nil, err
		}
		if err := s.writeStoreJSON(seedClaimFileName, claim); err != nil {
			return SeedRecord{}, nil, err
		}
		record.Repositories = append(record.Repositories, SeedRepository{
			Name: repository.Name, Leaf: repository.Leaf, RequestedRef: repository.Source.Ref,
			SourceDigest: mustRepositorySourceDigest(repository.Source.Repository), SparsePaths: append([]string(nil), repository.Source.Paths...),
		})
	}
	stableCharge := false
	for attempt := 0; attempt < 4; attempt++ {
		if err := writeSeedRecord(stage, record); err != nil {
			return SeedRecord{}, nil, err
		}
		physical, err := PhysicalUsage(RootPath(stage))
		if err != nil {
			return SeedRecord{}, nil, err
		}
		if physical == record.PhysicalBytes {
			stableCharge = true
			break
		}
		record.PhysicalBytes = physical
	}
	if !stableCharge {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed physical charge did not stabilize")
	}
	if err := RenameNoReplace(stage, filepath.Join(s.dir, seedID)); err != nil {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: publish cache seed: %w", err)
	}
	if err := syncArchiveLikeDir(s.dir); err != nil {
		return SeedRecord{}, nil, err
	}
	if err := s.root.Remove(seedClaimFileName); err != nil {
		return SeedRecord{}, nil, err
	}
	if err := syncRoot(s.root); err != nil {
		return SeedRecord{}, nil, err
	}
	committed = true
	paths := make(map[string]string, len(record.Repositories))
	for _, repository := range record.Repositories {
		paths[repository.Name] = filepath.Join(s.dir, seedID, repository.Leaf)
	}
	return record, paths, nil
}

// Record returns one seed's separate accounting identity.
func (s *SeedStore) Record(seedID string) (SeedRecord, error) {
	if err := ValidateRepositoryLeaf(seedID); err != nil {
		return SeedRecord{}, fmt.Errorf("runtime/workarea: invalid seed identity: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lockStore()
	if err != nil {
		return SeedRecord{}, err
	}
	defer releaseFlock(lock)
	seedRoot, err := s.root.OpenRoot(seedID)
	if err != nil {
		return SeedRecord{}, err
	}
	defer func() { _ = seedRoot.Close() }()
	data, err := seedRoot.ReadFile(seedRecordFileName)
	if err != nil {
		return SeedRecord{}, err
	}
	return decodeSeedRecord(data, seedID)
}

func (s *SeedStore) load(seedID string, declaration NormalizedDeclaration) (SeedRecord, map[string]string, error) {
	seedRoot, err := s.root.OpenRoot(seedID)
	if err != nil {
		return SeedRecord{}, nil, err
	}
	defer func() { _ = seedRoot.Close() }()
	data, err := seedRoot.ReadFile(seedRecordFileName)
	if err != nil {
		return SeedRecord{}, nil, err
	}
	record, err := decodeSeedRecord(data, seedID)
	if err != nil {
		return SeedRecord{}, nil, err
	}
	if len(record.Repositories) != len(declaration.Repositories) {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed declaration mismatch")
	}
	paths := make(map[string]string, len(record.Repositories))
	for _, repository := range record.Repositories {
		matched := false
		for _, declared := range declaration.Repositories {
			digest, _ := RepositorySourceDigest(declared.Source.Repository)
			if declared.Name == repository.Name && declared.Leaf == repository.Leaf && declared.Source.Ref == repository.RequestedRef && digest == repository.SourceDigest && slices.Equal(declared.Source.Paths, repository.SparsePaths) {
				matched = true
				break
			}
		}
		if !matched {
			return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed declaration mismatch")
		}
		info, err := seedRoot.Lstat(repository.Leaf)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed repository %q is unavailable", repository.Name)
		}
		paths[repository.Name] = filepath.Join(s.dir, seedID, repository.Leaf)
	}
	physical, err := PhysicalUsageRoot(seedRoot)
	if err != nil {
		return SeedRecord{}, nil, err
	}
	if physical != record.PhysicalBytes {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed physical charge changed")
	}
	return record, paths, nil
}

func decodeSeedRecord(data []byte, seedID string) (SeedRecord, error) {
	var record SeedRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return SeedRecord{}, fmt.Errorf("runtime/workarea: decode seed record: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return SeedRecord{}, err
	}
	if record.SchemaVersion != seedRecordSchemaV1 || record.SeedID != seedID || record.CreatedAt.IsZero() || len(record.Repositories) == 0 {
		return SeedRecord{}, fmt.Errorf("runtime/workarea: invalid cache seed record")
	}
	for _, repository := range record.Repositories {
		if !validRepositorySourceDigest(repository.SourceDigest) {
			return SeedRecord{}, fmt.Errorf("runtime/workarea: invalid cache seed source digest")
		}
	}
	return record, nil
}

func writeSeedRecord(root string, record SeedRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = rootHandle.Close() }()
	const temp = ".seed-record.tmp"
	file, err := rootHandle.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
	if err := rootHandle.Rename(temp, seedRecordFileName); err != nil {
		return err
	}
	return syncRoot(rootHandle)
}

func (s *SeedStore) lockStore() (*os.File, error) {
	if err := s.assertStoreIdentity(); err != nil {
		return nil, err
	}
	lock, err := openLockFile(s.root, seedStoreLockName)
	if err != nil {
		return nil, err
	}
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := s.assertStoreIdentity(); err != nil {
		releaseFlock(lock)
		return nil, err
	}
	return lock, nil
}

func seedRandomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func mustRepositorySourceDigest(repository string) string {
	digest, _ := RepositorySourceDigest(repository)
	return digest
}

func syncArchiveLikeDir(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
