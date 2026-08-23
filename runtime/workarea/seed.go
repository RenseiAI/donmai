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
	seedStoreDirName   = ".workarea-seeds"
	seedRecordFileName = "seed.json"
	seedStoreLockName  = ".store.lock"
	seedRecordSchemaV1 = "donmai.workarea-seed.v1"
)

// SeedRepository is one secret-free repository held by a reusable cache seed.
type SeedRepository struct {
	Name         string `json:"name"`
	Leaf         string `json:"leaf"`
	RequestedRef string `json:"requestedRef,omitempty"`
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

// SeedStore owns reusable material outside every session root.
type SeedStore struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
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
	dir := filepath.Join(abs, seedStoreDirName)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, fmt.Errorf("runtime/workarea: create seed store: %w", err)
		}
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, false, fmt.Errorf("runtime/workarea: seed store is not a private real directory")
	}
	return &SeedStore{dir: dir, now: now}, true, nil
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
	if err := os.Mkdir(stage, 0o700); err != nil {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: create seed stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
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
		record.Repositories = append(record.Repositories, SeedRepository{
			Name: repository.Name, Leaf: repository.Leaf, RequestedRef: repository.Source.Ref,
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
	if err := os.Rename(stage, filepath.Join(s.dir, seedID)); err != nil {
		return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: publish cache seed: %w", err)
	}
	if err := syncArchiveLikeDir(s.dir); err != nil {
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
	storeRoot, err := os.OpenRoot(s.dir)
	if err != nil {
		return SeedRecord{}, err
	}
	defer func() { _ = storeRoot.Close() }()
	seedRoot, err := storeRoot.OpenRoot(seedID)
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
	root := filepath.Join(s.dir, seedID)
	storeRoot, err := os.OpenRoot(s.dir)
	if err != nil {
		return SeedRecord{}, nil, err
	}
	defer func() { _ = storeRoot.Close() }()
	seedRoot, err := storeRoot.OpenRoot(seedID)
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
			if declared.Name == repository.Name && declared.Leaf == repository.Leaf && declared.Source.Ref == repository.RequestedRef {
				matched = true
				break
			}
		}
		if !matched {
			return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed declaration mismatch")
		}
		path := filepath.Join(root, repository.Leaf)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return SeedRecord{}, nil, fmt.Errorf("runtime/workarea: cache seed repository %q is unavailable", repository.Name)
		}
		paths[repository.Name] = path
	}
	physical, err := PhysicalUsage(RootPath(root))
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
	lock, err := os.OpenFile(filepath.Join(s.dir, seedStoreLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
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
