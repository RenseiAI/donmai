package sessionshim

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// PutTombstone durably publishes a terminal tombstone AND removes the live
// discovery record for the same identity, in that order.
//
// The order is the safe one: publishing the proof of death before withdrawing
// the liveness claim means a daemon scanning concurrently can see both, or the
// record alone — never neither. A crash between the two leaves a record and a
// tombstone for the same identity, which the classifier reads as "terminal",
// not as an ambiguity.
func (r *Registry) PutTombstone(t Tombstone) error {
	data, err := t.encode()
	if err != nil {
		return err
	}
	if err := r.publish(t.Identity().TombstoneName(), data); err != nil {
		return err
	}
	return r.Remove(t.Identity())
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

// RemoveTombstone deletes a tombstone. This is the audited-disposal path: it is
// called only after a daemon has DURABLY reported the terminal outcome, because
// the tombstone is the only remaining proof that the harness group was reaped.
func (r *Registry) RemoveTombstone(id Identity) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(id.TombstoneName()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove tombstone: %w", err)
	}
	return nil
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
	data, err := r.readEntry(id.TombstoneName())
	if err != nil {
		return Tombstone{}, err
	}
	return decodeTombstone(data)
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
	if err := r.checkDirMode(); err != nil {
		return nil, err
	}
	names, err := r.entryNames(tombstoneSuffix)
	if err != nil {
		return nil, err
	}
	out := make([]Tombstone, 0, len(names))
	for _, name := range names {
		data, err := r.readEntry(name)
		if err != nil {
			continue
		}
		t, err := decodeTombstone(data)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
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
