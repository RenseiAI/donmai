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
	Revoked   bool      `json:"revoked"`
	Bytes     int64     `json:"bytes"`
}

// catalogDocument is the on-disk shape of the secret-free listing.
type catalogDocument struct {
	Version int           `json:"version"`
	Mirrors []MirrorEntry `json:"mirrors"`
}

// catalogVersion is the only catalog version this store reads or writes.
const catalogVersion = 1

// writeCatalog persists the secret-free listing atomically: a temp file
// beside the destination, owner-only, fsynced, then renamed over the old
// catalog with a directory fsync before success.
func writeCatalog(catalogPath string, entries []MirrorEntry) error {
	body, err := json.Marshal(catalogDocument{Version: catalogVersion, Mirrors: entries})
	if err != nil {
		return fmt.Errorf("repo keeper: encode catalog: %w", err)
	}
	dir := filepath.Dir(catalogPath)
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
// secret-free listing, preserving every other entry. revoked is sticky: a
// revoked entry stays revoked across later refreshes until MarkRevoked is
// explicitly cleared with revoked=false.
func (s *Store) refreshCatalogEntry(digest string, fetchedAt time.Time, numBytes int64) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	entries, err := readCatalog(s.catalogPath())
	if err != nil {
		return err
	}
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
	return writeCatalog(s.catalogPath(), entries)
}
