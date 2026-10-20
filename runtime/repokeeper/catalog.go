package repokeeper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// catalogFileName is the secret-free listing beside the mirrors directory.
// It names each mirror's digest, last fetch, revoked flag, and bytes —
// never a remote URL, scope credential, or any value a credential could be
// recovered from.
const catalogFileName = "catalog.json"

// catalogFilePerms locks the catalog to owner-only access. Its entries are
// secret-free, but the mode keeps the keeper root's posture uniform.
const catalogFilePerms = 0o600

// MirrorEntry is one secret-free catalog row: the mirror's directory
// digest, its last successful fetch time, whether its scope has been
// revoked, and its on-disk bytes. There is deliberately no remote URL,
// scope credential, or path field: the digest alone addresses the mirror.
type MirrorEntry struct {
	Digest    string    `json:"digest"`
	LastFetch time.Time `json:"lastFetch"`
	// LastUse is the daemon layer's least-recently-used marker: the last
	// time the mirror was served or fetched. The store never sets it —
	// Ensure and refreshCatalogEntry preserve whatever value is there —
	// so the eviction order stays the daemon's policy, on the daemon's
	// clock, while the bytes the order budgets against stay the store's.
	LastUse time.Time `json:"lastUse"`
	Revoked bool      `json:"revoked"`
	Bytes   int64     `json:"bytes"`
}

// catalogDocument is the on-disk shape of the secret-free listing.
type catalogDocument struct {
	Version int           `json:"version"`
	Mirrors []MirrorEntry `json:"mirrors"`
}

// catalogVersion is the only catalog version this store reads or writes.
const catalogVersion = 1

// writeCatalog persists the secret-free listing atomically: the parent
// is created owner-only first (a fresh state home has no keeper root
// yet), then a temp file beside the destination, owner-only, fsynced,
// renamed over the old catalog with a directory fsync before success.
func writeCatalog(catalogPath string, entries []MirrorEntry) error {
	body, err := json.Marshal(catalogDocument{Version: catalogVersion, Mirrors: entries})
	if err != nil {
		return fmt.Errorf("repo keeper: encode catalog: %w", err)
	}
	dir := filepath.Dir(catalogPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("repo keeper: create catalog parent dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return fmt.Errorf("repo keeper: create catalog temp file: %w", err)
	}
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(catalogFilePerms); err != nil {
		return fmt.Errorf("repo keeper: mode catalog temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("repo keeper: write catalog: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("repo keeper: fsync catalog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("repo keeper: close catalog temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), catalogPath); err != nil {
		return fmt.Errorf("repo keeper: publish catalog: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	committed = true
	return nil
}

// readCatalog loads the secret-free listing, returning no entries when the
// catalog does not exist yet.
func readCatalog(catalogPath string) ([]MirrorEntry, error) {
	data, err := os.ReadFile(catalogPath) //nolint:gosec // G304: the store's own catalog path under its state dir.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("repo keeper: read catalog: %w", err)
	}
	var doc catalogDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("repo keeper: decode catalog: %w", err)
	}
	if doc.Version != catalogVersion {
		return nil, fmt.Errorf("repo keeper: unsupported catalog version %d", doc.Version)
	}
	return doc.Mirrors, nil
}

// refreshCatalogEntry records one mirror's fetch time and bytes in the
// secret-free listing, preserving every other entry and the row's LastUse
// marker. revoked is sticky: a revoked entry stays revoked across later
// refreshes until MarkRevoked is explicitly cleared with revoked=false.
func (s *Store) refreshCatalogEntry(digest string, fetchedAt time.Time, numBytes int64) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return s.updateCatalogLocked(func(entries []MirrorEntry) ([]MirrorEntry, error) {
		found := false
		for i := range entries {
			if entries[i].Digest != digest {
				continue
			}
			entries[i].LastFetch = fetchedAt
			entries[i].Bytes = numBytes
			found = true
		}
		if !found {
			entries = append(entries, MirrorEntry{Digest: digest, LastFetch: fetchedAt, Bytes: numBytes})
		}
		return entries, nil
	})
}

// RecordFetch notes a completed mirror fetch: last-fetch advances, bytes
// and revoked state are replaced, and the row's LastUse marker advances to
// the fetch time so a freshly fetched mirror is not the next eviction
// victim. A digest with no catalog row gains one. The daemon layer calls
// this after it observes a fetch; the store never fetches on its own.
func (s *Store) RecordFetch(digest string, at time.Time, numBytes int64, revoked bool) error {
	if at.IsZero() {
		at = time.Now()
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return s.updateCatalogLocked(func(entries []MirrorEntry) ([]MirrorEntry, error) {
		found := false
		for i := range entries {
			if entries[i].Digest != digest {
				continue
			}
			entries[i].LastFetch = at
			entries[i].LastUse = at
			entries[i].Bytes = numBytes
			entries[i].Revoked = revoked
			found = true
		}
		if !found {
			entries = append(entries, MirrorEntry{Digest: digest, LastFetch: at, LastUse: at, Bytes: numBytes, Revoked: revoked})
		}
		return entries, nil
	})
}

// TouchUse advances one mirror's LastUse marker to at (now when zero)
// without changing its fetch time, bytes, or revoked flag. The daemon
// layer calls this when it serves an existing mirror so the
// least-recently-used order tracks serving, not just fetching.
func (s *Store) TouchUse(digest string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return s.updateCatalogLocked(func(entries []MirrorEntry) ([]MirrorEntry, error) {
		for i := range entries {
			if entries[i].Digest != digest {
				continue
			}
			entries[i].LastUse = at
			return entries, nil
		}
		return append(entries, MirrorEntry{Digest: digest, LastUse: at}), nil
	})
}

// Remove deletes one mirror's directory and drops its catalog row. The
// per-mirror lock is held only for the rename into a trash sibling; the
// recursive delete runs outside the lock so one large mirror cannot stall
// every caller for another. The rename runs under the exclusive
// cross-process lock, so it waits for outstanding leases to release
// rather than deleting a mirror another process is using. A missing
// directory still drops the row, so Remove repairs a half-evicted state
// instead of failing it.
func (s *Store) Remove(digest string) error {
	_, err := s.remove(digest, true)
	return err
}

// TryRemove deletes like Remove but never waits: when the mirror's
// in-process mutex or its cross-process lock is held — a lease is
// outstanding in this or another process sharing the state dir — it
// reports removed=false and leaves everything in place. Budget eviction
// uses it so a leased mirror is skipped as a victim instead of stalling
// the keeper lock until the lease releases. A missing directory still
// drops the row and reports removed=true.
func (s *Store) TryRemove(digest string) (bool, error) {
	return s.remove(digest, false)
}

// remove implements Remove (wait=true) and TryRemove (wait=false).
func (s *Store) remove(digest string, wait bool) (bool, error) {
	if !validMirrorDigest(digest) {
		return false, fmt.Errorf("repo keeper: refusing to remove a path outside the mirrors directory")
	}
	dir := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)
	var unlock func()
	if wait {
		unlock = s.lockMirror(digest)
	} else {
		var ok bool
		unlock, ok = s.tryLockMirror(digest)
		if !ok {
			return false, nil
		}
	}
	trash, busy, err := func() (string, bool, error) {
		defer unlock()
		excl, err := s.openMirrorLock(digest)
		if err != nil {
			return "", false, err
		}
		if wait {
			// Maintenance exclusion against lease holders: no ctx here,
			// like the catalog publish — the section is short and a
			// dead holder releases its lock with its file description.
			if fd, ferr := intFileDescriptor(excl.Fd()); ferr != nil {
				_ = excl.Close()
				return "", false, ferr
			} else if ferr = syscallFlockExclusive(fd); ferr != nil {
				_ = excl.Close()
				return "", false, ferr
			}
		} else {
			fd, ferr := intFileDescriptor(excl.Fd())
			if ferr != nil {
				_ = excl.Close()
				return "", false, ferr
			}
			acquired, ferr := tryLockExclusiveNB(fd)
			if ferr != nil {
				_ = excl.Close()
				return "", false, ferr
			}
			if !acquired {
				_ = excl.Close()
				return "", true, nil
			}
		}
		defer func() { _ = unlockAndClose(excl) }()
		if _, err := s.root.Lstat(mirrorsRelPath(digest)); err != nil {
			if os.IsNotExist(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("repo keeper: stat mirror for removal: %w", err)
		}
		parent := filepath.Dir(dir)
		staging, err := os.MkdirTemp(parent, ".evict-*.tmp")
		if err != nil {
			return "", false, fmt.Errorf("repo keeper: create eviction staging dir: %w", err)
		}
		// MkdirTemp created the staging path; remove it so the rename
		// below has a free target that cannot collide with a clone's.
		if err := os.Remove(staging); err != nil {
			return "", false, fmt.Errorf("repo keeper: free eviction staging path: %w", err)
		}
		if err := os.Rename(dir, staging); err != nil {
			return "", false, fmt.Errorf("repo keeper: stage mirror for removal: %w", err)
		}
		if err := fsyncDir(parent); err != nil {
			return "", false, err
		}
		return staging, false, nil
	}()
	if err != nil {
		return false, err
	}
	if busy {
		return false, nil
	}
	if trash != "" {
		if err := os.RemoveAll(trash); err != nil {
			return false, fmt.Errorf("repo keeper: remove staged mirror: %w", err)
		}
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	// Write even when nothing changed: a half-evicted state (directory
	// gone, row present) converges to no row either way.
	if err := s.updateCatalogLocked(func(entries []MirrorEntry) ([]MirrorEntry, error) {
		kept := entries[:0]
		for _, entry := range entries {
			if entry.Digest != digest {
				kept = append(kept, entry)
			}
		}
		return kept, nil
	}); err != nil {
		return false, err
	}
	return true, nil
}

// Reconcile drops catalog rows whose mirror directory is gone and adopts
// directories the catalog does not name — but only when the store's own
// verification passes for the caller's (canonical remote, scope). A
// directory that fails verification for every candidate is left on disk
// but reported as unverified: the caller decides whether to remove it,
// and the keeper never serves it. The returned verified set is the
// keeper's serving set; the unverified set is never served.
//
// Candidates name the remotes the caller expects to find: one canonical
// remote maps to at most one directory per scope, so the scan probes each
// (remote, scope) pair's digest directly instead of walking the tree.
func (s *Store) Reconcile(candidates []ReconcileCandidate) (verified []MirrorEntry, unverified []string, err error) {
	type verdict struct {
		digest   string
		verified bool
	}
	byDigest := make(map[string]verdict)
	for _, candidate := range candidates {
		canonical, remoteDigest, err := CanonicalRemote(candidate.Remote)
		if err != nil {
			continue
		}
		digest, err := ScopeDigest(remoteDigest, candidate.Scope)
		if err != nil {
			continue
		}
		dir := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := s.verifyMirror(dir, digest, canonical, candidate.Scope); err != nil {
			byDigest[digest] = verdict{digest: digest}
			continue
		}
		byDigest[digest] = verdict{digest: digest, verified: true}
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	// The catalog read-modify-write below holds the cross-process flock
	// across both halves, so a concurrent Store cannot interleave its own
	// publish between this read and write and lose an update.
	lock, unlock, err := s.lockCatalog()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unlock(lock) }()
	entries, err := readCatalog(s.catalogPath())
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if v, ok := byDigest[entry.Digest]; ok && v.verified {
			verified = append(verified, entry)
			continue
		}
		// A catalog row survives only when its directory is still a real
		// directory on disk: anything else is a stale row for a mirror
		// that is gone (or never was), and is dropped.
		dir := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, entry.Digest)
		if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			verified = append(verified, entry)
		}
	}
	// Adopt verified candidate directories the catalog does not name.
	known := make(map[string]bool, len(verified))
	for _, entry := range verified {
		known[entry.Digest] = true
	}
	for digest, v := range byDigest {
		if !v.verified {
			unverified = append(unverified, digest)
			continue
		}
		if known[digest] {
			continue
		}
		numBytes, _ := dirBytes(filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest))
		entry := MirrorEntry{Digest: digest, LastFetch: dirModTime(filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)), Bytes: numBytes}
		verified = append(verified, entry)
		entries = append(entries, entry)
	}
	if err := writeCatalog(s.catalogPath(), entries); err != nil {
		return nil, nil, err
	}
	return verified, unverified, nil
}

// ReconcileCandidate names one (remote, scope) pair the caller expects the
// store to hold. Remote is the clone source; scope is the opaque
// credential-scope name (empty for the host-wide default).
type ReconcileCandidate struct {
	Remote string
	Scope  string
}
