package sessionshim

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Directory and file modes required by §D6. These are checked on READ as well
// as set on write: a record whose mode has been widened is quarantine evidence,
// not something to silently repair.
const (
	// RegistryDirMode is the required mode of the registry directory.
	RegistryDirMode fs.FileMode = 0o700
	// RecordFileMode is the required mode of a discovery record or tombstone.
	RecordFileMode fs.FileMode = 0o600
)

// ErrRegistryUnsafe reports a registry directory or entry whose ownership or
// mode violates the §D6 bounds.
var ErrRegistryUnsafe = errors.New("sessionshim: unsafe registry permissions")

// ErrTombstoneAmbiguous reports a legacy identity-only operation that matched
// more than one shim/process incarnation. Singular callers fail closed rather
// than deleting or returning an arbitrary proof.
var ErrTombstoneAmbiguous = errors.New("sessionshim: multiple terminal incarnations match session identity")

// Registry is the on-disk discovery surface: one bounded, secret-free record per
// live shim, plus a terminal tombstone per shim that reaped its own harness.
//
// Every write goes through the same durable publish sequence — temp file,
// fsync, rename, parent-directory fsync — because a torn record read by a
// starting daemon is indistinguishable from a corrupted one, and the daemon's
// only safe response to corruption is quarantine. Making publication atomic
// removes that failure mode at the source rather than handling it downstream.
//
// The directory is opened through os.Root so every name is confined to it: a
// symlink planted in the registry cannot redirect a write outside.
type Registry struct {
	dir string
}

// NewRegistry returns a Registry rooted at dir, creating it 0700 if absent.
//
// dir comes from the caller's injected state-directory seam. No brand-specific
// path is compiled in here (§ implementation notes) — that is what keeps the
// package embeddable without assuming a particular install layout.
func NewRegistry(dir string) (*Registry, error) {
	if dir == "" {
		return nil, errors.New("sessionshim: registry directory is required")
	}
	// The registry directory IS this package's configuration seam: an embedder
	// chooses where session state lives, and there is no narrower root to confine
	// it to. Cleaning normalises the operator's path; everything INSIDE the
	// directory is then confined by os.Root on every access, which is where
	// untrusted names (record filenames) actually appear.
	dir = filepath.Clean(dir)
	//nolint:gosec // G703: dir is the embedder-supplied state-directory seam, not untrusted input; entry names below are confined by os.Root
	if err := os.MkdirAll(dir, RegistryDirMode); err != nil {
		return nil, fmt.Errorf("sessionshim: create registry dir: %w", err)
	}
	// MkdirAll leaves an EXISTING directory's mode alone, so tighten explicitly.
	// A registry that is group- or world-readable would put the socket paths of
	// every live session in reach of another local user.
	//nolint:gosec // G703: same operator-supplied seam as the MkdirAll above
	if err := os.Chmod(dir, RegistryDirMode); err != nil {
		return nil, fmt.Errorf("sessionshim: tighten registry dir: %w", err)
	}
	return &Registry{dir: dir}, nil
}

// Dir returns the registry directory path.
func (r *Registry) Dir() string { return r.dir }

// SocketPath returns the socket path this identity's shim must listen on.
// Deriving it (rather than letting a shim choose) means a record cannot point a
// controller at an arbitrary socket elsewhere on the filesystem.
func (r *Registry) SocketPath(id Identity) string {
	return filepath.Join(r.dir, id.SocketName())
}

// Put durably publishes a discovery record, replacing any previous record for
// the same identity.
func (r *Registry) Put(rec Record) error {
	data, err := rec.encode()
	if err != nil {
		return err
	}
	return r.publish(rec.Identity().RecordName(), data)
}

// recordWriteMu serializes every read-modify-write of a live discovery record.
//
// Two such writers run concurrently inside one process. The Codex harness
// records its resume key through its OWN Registry handle (PutResumeKey) while
// the shim that supervises it republishes the record on each phase transition
// (PutRetainingResumeKey) — and the naming window those two overlap in is tens
// of seconds wide. Both do Get → mutate → Put, so without a shared lock the
// interleaving Get/Get/Put/Put drops whichever field the loser carried, which
// is precisely the resume key this package was taught to keep. The two handles
// are distinct values over the same directory, so the serialization point has
// to be package-level rather than per-Registry. It costs nothing: a record is
// published only on phase transitions, never per frame, and one mutex cannot
// grow unbounded the way a per-identity lock table would.
var recordWriteMu sync.Mutex

// PutResumeKey records the first durable Codex rollout for a live shim. The
// record remains the source of truth through adoption and quarantine.
//
// The first key wins: a session has exactly one native conversation, and a
// later writer that disagreed would be describing a different one.
func (r *Registry) PutResumeKey(id Identity, key ResumeKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	recordWriteMu.Lock()
	defer recordWriteMu.Unlock()
	rec, err := r.Get(id)
	if err != nil {
		return fmt.Errorf("sessionshim: get record for resume key: %w", err)
	}
	if rec.ResumeKey != nil {
		return nil
	}
	rec.ResumeKey = &key
	return r.Put(rec)
}

// PutRetainingResumeKey publishes rec while carrying forward the resume key the
// SAME incarnation already recorded, and returns the key the published record
// carries so a caller can hold it in memory.
//
// This is the republishing half of the pair above: the shim rebuilds its record
// from its own in-memory state on every phase transition, and that state has
// never held the harness's resume key. Reading the previous record inside the
// same critical section as the write is what makes a republish incapable of
// erasing a key recorded microseconds earlier.
//
// A previous record from a DIFFERENT shim or epoch is ignored: its key
// describes another incarnation's conversation.
func (r *Registry) PutRetainingResumeKey(rec Record) (*ResumeKey, error) {
	recordWriteMu.Lock()
	defer recordWriteMu.Unlock()
	if rec.ResumeKey == nil {
		if previous, err := r.Get(rec.Identity()); err == nil &&
			previous.ShimID == rec.ShimID && previous.ProcessEpoch == rec.ProcessEpoch {
			rec.ResumeKey = previous.ResumeKey
		}
	}
	if err := r.Put(rec); err != nil {
		return nil, err
	}
	return rec.ResumeKey, nil
}

// PutTombstone durably publishes a per-incarnation terminal tombstone AND
// removes only the matching live discovery record, in that order.
//
// The order is the safe one: publishing the proof of death before withdrawing
// the liveness claim means a daemon scanning concurrently can see both, or the
// record alone — never neither. A crash between the two leaves an exact record
// and tombstone correlation, while a different live incarnation under the same
// lifecycle identity remains visible rather than being erased.
func (r *Registry) PutTombstone(t Tombstone) error {
	// The terminal record replaces the live record. Preserve the resume key
	// before withdrawing that record, so an exited session remains resumable —
	// under the same lock the live writers hold, because this read-then-remove
	// is the third read-modify-write over the same file and a key published
	// between its two halves would be withdrawn without ever being carried.
	recordWriteMu.Lock()
	defer recordWriteMu.Unlock()
	if t.ResumeKey == nil {
		if rec, err := r.Get(t.Identity()); err == nil && rec.ShimID == t.ShimID && rec.ProcessEpoch == t.ProcessEpoch {
			t.ResumeKey = rec.ResumeKey
		}
	}
	data, err := t.encode()
	if err != nil {
		return err
	}
	if err := r.publish(tombstoneIncarnationName(t), data); err != nil {
		return err
	}
	// Keep the legacy identity-only alias while it is unambiguous so older v1
	// readers can still consume a tombstone written by this release. Once a
	// second incarnation exists, never overwrite the first alias: an old reader
	// may then reconcile one proof and conservatively leave the other held, but it
	// can never receive the wrong proof under an identity.
	legacySafe, err := r.legacyTombstoneAliasSafe(t)
	if err != nil {
		return err
	}
	if legacySafe {
		if err := r.publish(t.Identity().TombstoneName(), data); err != nil {
			return err
		}
	}
	if err := r.RemoveIncarnation(t.Identity(), t.ShimID, t.ProcessEpoch); err != nil {
		return err
	}
	return r.removeDurableAck(t.Identity(), t.ShimID, t.ProcessEpoch)
}

func tombstoneIncarnationName(t Tombstone) string {
	correlation := t.Identity().Key() + "\x1f" + t.ShimID + "\x1f" + strconv.FormatUint(t.ProcessEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + tombstoneSuffix
}

// Remove deletes the live discovery record for an identity. A missing record is
// not an error — removal is idempotent because both the shim's own teardown and
// a daemon janitor may reach it.
func (r *Registry) Remove(id Identity) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(id.RecordName()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove record: %w", err)
	}
	return nil
}

// RemoveIncarnation removes only discovery records whose decoded shim/process
// correlation matches the expected incarnation. It is the terminal path's safe
// alternative to legacy identity-only Remove when duplicate records coexist.
func (r *Registry) RemoveIncarnation(id Identity, shimID string, processEpoch uint64) error {
	entries, err := r.Scan()
	if err != nil {
		return err
	}
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, entry := range entries {
		if entry.Err != nil || entry.Record.Identity() != id || entry.Record.ShimID != shimID || entry.Record.ProcessEpoch != processEpoch {
			continue
		}
		if err := root.Remove(entry.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sessionshim: remove exact discovery record: %w", err)
		}
	}
	return nil
}

// HasIncarnation reports whether an exact live discovery record remains.
func (r *Registry) HasIncarnation(id Identity, shimID string, processEpoch uint64) (bool, error) {
	entries, err := r.Scan()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Err == nil && entry.Record.Identity() == id && entry.Record.ShimID == shimID && entry.Record.ProcessEpoch == processEpoch {
			return true, nil
		}
	}
	return false, nil
}

// RemoveTombstone deletes a tombstone. This is the audited-disposal path: it is
// called only after a daemon has DURABLY reported the terminal outcome, because
// the tombstone is the only remaining proof that the harness group was reaped.
func (r *Registry) RemoveTombstone(id Identity) error {
	entries, err := r.tombstoneEntries()
	if err != nil {
		return err
	}
	var matched []tombstoneFile
	unique := make(map[terminalIncarnation]struct{})
	for _, entry := range entries {
		if entry.tombstone.Identity() != id {
			continue
		}
		matched = append(matched, entry)
		unique[terminalIncarnationForTombstone(entry.tombstone)] = struct{}{}
	}
	if len(unique) > 1 {
		return fmt.Errorf("%w: %s", ErrTombstoneAmbiguous, id)
	}
	return r.removeTombstoneFiles(matched)
}

// RemoveTombstoneIncarnation deletes every legacy/new filename that contains
// the exact terminal correlation, leaving sibling incarnations untouched.
func (r *Registry) RemoveTombstoneIncarnation(t Tombstone) error {
	entries, err := r.tombstoneEntries()
	if err != nil {
		return err
	}
	want := terminalIncarnationForTombstone(t)
	matched := make([]tombstoneFile, 0, 2)
	for _, entry := range entries {
		if terminalIncarnationForTombstone(entry.tombstone) == want {
			matched = append(matched, entry)
		}
	}
	return r.removeTombstoneFiles(matched)
}

// Get reads one discovery record by identity.
func (r *Registry) Get(id Identity) (Record, error) {
	data, err := r.readEntry(id.RecordName())
	if err != nil {
		return Record{}, err
	}
	return decodeRecord(data)
}

// GetTombstone reads one tombstone by identity.
func (r *Registry) GetTombstone(id Identity) (Tombstone, error) {
	entries, err := r.tombstoneEntries()
	if err != nil {
		return Tombstone{}, err
	}
	unique := make(map[terminalIncarnation]Tombstone)
	for _, entry := range entries {
		if entry.tombstone.Identity() == id {
			unique[terminalIncarnationForTombstone(entry.tombstone)] = entry.tombstone
		}
	}
	if len(unique) == 0 {
		return Tombstone{}, fmt.Errorf("sessionshim: tombstone for %s: %w", id, fs.ErrNotExist)
	}
	if len(unique) > 1 {
		return Tombstone{}, fmt.Errorf("%w: %s", ErrTombstoneAmbiguous, id)
	}
	for _, tombstone := range unique {
		return tombstone, nil
	}
	return Tombstone{}, fmt.Errorf("sessionshim: tombstone for %s: %w", id, fs.ErrNotExist)
}

// GetTombstoneIncarnation returns one exact shim/process terminal proof.
func (r *Registry) GetTombstoneIncarnation(id Identity, shimID string, processEpoch uint64) (Tombstone, error) {
	want := terminalIncarnation{identity: id, shimID: shimID, processEpoch: processEpoch}
	entries, err := r.tombstoneEntries()
	if err != nil {
		return Tombstone{}, err
	}
	for _, entry := range entries {
		if terminalIncarnationForTombstone(entry.tombstone) == want {
			return entry.tombstone, nil
		}
	}
	return Tombstone{}, fmt.Errorf("sessionshim: exact tombstone for %s/%s/%d: %w", id, shimID, processEpoch, fs.ErrNotExist)
}

// ScanEntry is one raw registry entry as found on disk. Err is non-nil when the
// entry could not be read or decoded.
//
// A scan reports failures as ENTRIES rather than aborting, because a single
// corrupt file must not blind a starting daemon to every other live session —
// and because an unreadable entry is itself something the daemon has to account
// for, not something it may skip.
type ScanEntry struct {
	Name   string
	Record Record
	Err    error
}

// Scan reads every discovery record in the registry, sorted by filename for
// deterministic classification order.
func (r *Registry) Scan() ([]ScanEntry, error) {
	if err := r.checkDirMode(); err != nil {
		return nil, err
	}
	names, err := r.entryNames(recordSuffix, tombstoneSuffix)
	if err != nil {
		return nil, err
	}
	out := make([]ScanEntry, 0, len(names))
	for _, name := range names {
		e := ScanEntry{Name: name}
		data, err := r.readEntry(name)
		if err != nil {
			e.Err = err
			out = append(out, e)
			continue
		}
		rec, err := decodeRecord(data)
		e.Record, e.Err = rec, err
		out = append(out, e)
	}
	return out, nil
}

// ScanTombstones reads every tombstone in the registry, sorted by filename.
func (r *Registry) ScanTombstones() ([]Tombstone, error) {
	entries, err := r.tombstoneEntries()
	if err != nil {
		return nil, err
	}
	unique := make(map[terminalIncarnation]Tombstone, len(entries))
	for _, entry := range entries {
		unique[terminalIncarnationForTombstone(entry.tombstone)] = entry.tombstone
	}
	out := make([]Tombstone, 0, len(unique))
	for _, tombstone := range unique {
		out = append(out, tombstone)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := terminalIncarnationForTombstone(out[i]), terminalIncarnationForTombstone(out[j])
		if a.identity.Key() != b.identity.Key() {
			return a.identity.Key() < b.identity.Key()
		}
		if a.shimID != b.shimID {
			return a.shimID < b.shimID
		}
		return a.processEpoch < b.processEpoch
	})
	return out, nil
}

type tombstoneFile struct {
	name      string
	tombstone Tombstone
}

func (r *Registry) tombstoneEntries() ([]tombstoneFile, error) {
	if err := r.checkDirMode(); err != nil {
		return nil, err
	}
	names, err := r.entryNames(tombstoneSuffix)
	if err != nil {
		return nil, err
	}
	out := make([]tombstoneFile, 0, len(names))
	for _, name := range names {
		data, readErr := r.readEntry(name)
		if readErr != nil {
			continue
		}
		tombstone, decodeErr := decodeTombstone(data)
		if decodeErr != nil {
			continue
		}
		out = append(out, tombstoneFile{name: name, tombstone: tombstone})
	}
	return out, nil
}

// legacyTombstoneAliasSafe reports whether the identity-only alias may carry
// this incarnation's proof.
//
// The alias says "this IDENTITY's harness group was reaped", so it may only be
// written while the identity has no OTHER live incarnation, and it is never
// overwritten once some incarnation owns it. §D7's duplicate-identity case
// makes both real: a sibling lineage tombstoning beside a running session would
// otherwise leave a v1 reader — which can only read the alias — believing the
// live session's group was reaped, and the real lineage's later tombstone could
// never replace an alias that is deliberately never overwritten.
//
// Both questions are answered from the two files that can possibly hold the
// answer, addressed by name. This runs on every PutTombstone, under the shim's
// recordMu, on the terminal path: scanning and decoding the whole registry
// there made a shim's own finalization cost grow with every other session on
// the host, and made one unreadable file belonging to a stranger suppress the
// alias for everyone.
func (r *Registry) legacyTombstoneAliasSafe(t Tombstone) (bool, error) {
	if err := r.checkDirMode(); err != nil {
		return false, err
	}
	want := terminalIncarnationForTombstone(t)

	// LIVE siblings only, and only this identity's. Put writes one discovery
	// record per identity under Identity().RecordName(), so TODAY an identity
	// has at most one record on disk and a later incarnation OVERWRITES the
	// earlier one — this reads that single file because that is the whole store
	// there is, not because the format guarantees it stays that way. Nothing
	// here enforces it; a store that ever kept a record per incarnation would
	// need this to read them all. What it does rule out is the case the
	// whole-registry scan got wrong: a tombstone belonging to a DEAD sibling is
	// not a liveness claim, and treating it as one refused the alias for a
	// lineage whose identity had nothing running at all.
	record, err := r.Get(t.Identity())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No liveness claim for this identity at all.
	case err != nil:
		// This identity's OWN record is unreadable, so a sibling cannot be
		// ruled out. Errors on any other entry are not this question's
		// business and are never reached.
		return false, nil
	case record.Identity() != t.Identity():
		// A record whose contents disagree with the name it was read under.
		return false, nil
	case record.ShimID != t.ShimID || record.ProcessEpoch != t.ProcessEpoch:
		return false, nil
	}

	// The alias is a single file. If some incarnation already owns it, it keeps
	// it; if this one owns it, rewriting is idempotent.
	raw, err := r.readEntry(t.Identity().TombstoneName())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, nil
	}
	existing, err := decodeTombstone(raw)
	if err != nil {
		return false, nil
	}
	return terminalIncarnationForTombstone(existing) == want, nil
}

func (r *Registry) removeTombstoneFiles(files []tombstoneFile) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, file := range files {
		if err := root.Remove(file.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sessionshim: remove exact tombstone: %w", err)
		}
	}
	return nil
}

// ---- internals -------------------------------------------------------------

func (r *Registry) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: open registry root: %w", err)
	}
	return root, nil
}

// entryNames lists entries with the given suffix. exclude removes suffixes that
// would otherwise also match (".tombstone.json" ends with ".json").
func (r *Registry) entryNames(suffix string, exclude ...string) ([]string, error) {
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: read registry dir: %w", err)
	}
	var names []string
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}
		skip := false
		for _, ex := range exclude {
			if strings.HasSuffix(name, ex) {
				skip = true
				break
			}
		}
		if !skip {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// readEntry reads one registry file, enforcing the §D6 ownership, mode, type,
// and size bounds BEFORE returning bytes to a decoder.
func (r *Registry) readEntry(name string) ([]byte, error) {
	root, err := r.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: open registry entry: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sessionshim: stat registry entry: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRegistryUnsafe, name)
	}
	if perm := info.Mode().Perm(); perm&^RecordFileMode != 0 {
		return nil, fmt.Errorf("%w: %s has mode %#o, want at most %#o", ErrRegistryUnsafe, name, perm, RecordFileMode)
	}
	if err := checkOwnedBySelf(info, name); err != nil {
		return nil, err
	}
	if info.Size() > MaxRecordBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, max %d", ErrRecordInvalid, name, info.Size(), MaxRecordBytes)
	}
	// LimitReader guards the size bound against a file that grows between Stat
	// and Read.
	data, err := io.ReadAll(io.LimitReader(f, MaxRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sessionshim: read registry entry: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrRecordInvalid, name, MaxRecordBytes)
	}
	return data, nil
}

func (r *Registry) checkDirMode() error {
	info, err := os.Stat(r.dir)
	if err != nil {
		return fmt.Errorf("sessionshim: stat registry dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: registry path is not a directory", ErrRegistryUnsafe)
	}
	if perm := info.Mode().Perm(); perm&^RegistryDirMode != 0 {
		return fmt.Errorf("%w: registry dir has mode %#o, want at most %#o", ErrRegistryUnsafe, perm, RegistryDirMode)
	}
	return checkOwnedBySelf(info, r.dir)
}

// checkOwnedBySelf verifies the entry belongs to the running user. Same-UID
// processes are inside the daemon user's existing local trust boundary (§D3);
// a DIFFERENT uid is not, and is refused rather than repaired.
func checkOwnedBySelf(info fs.FileInfo, name string) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // platform without stat details: the mode check above still applies
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrRegistryUnsafe, name, st.Uid, os.Getuid())
	}
	return nil
}

// publish writes data to name durably and atomically: a uniquely-named temp
// file, fsync, rename over the target, then a parent-directory fsync so the
// rename itself survives a crash. Every step is confined to the registry root.
func (r *Registry) publish(name string, data []byte) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("sessionshim: publish nonce: %w", err)
	}
	tmpName := fmt.Sprintf(".%x.tmp", nonce)
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, RecordFileMode)
	if err != nil {
		return fmt.Errorf("sessionshim: create temp record: %w", err)
	}
	// Removing the temp name unconditionally is safe after a successful rename
	// (the name no longer exists) and is what cleans up every failure path.
	defer func() { _ = root.Remove(tmpName) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("sessionshim: write temp record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sessionshim: sync temp record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sessionshim: close temp record: %w", err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("sessionshim: publish record: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("sessionshim: open registry dir for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sessionshim: sync registry dir: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("sessionshim: close registry dir: %w", err)
	}
	return nil
}
