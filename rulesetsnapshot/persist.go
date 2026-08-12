package rulesetsnapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/internal/statepath"
)

// defaultPersistSuffix / defaultPersistTmpFallback follow the same
// statepath.Resolve(suffix, tmpFallback) convention every other on-disk
// daemon state file in this repo uses (see
// daemon.NewFileExecutionPreflightStore's "adaptation-receipts" call).
const (
	defaultPersistSuffix      = "ruleset-snapshot.json"
	defaultPersistTmpFallback = "/tmp/.donmai/ruleset-snapshot.json"
)

// persistPath resolves the on-disk path for the last-known-good snapshot:
// Config.StatePath when set, else the brand state directory.
func (c *Client) persistPath() string {
	if c.cfg.StatePath != "" {
		return c.cfg.StatePath
	}
	return statepath.Resolve(defaultPersistSuffix, defaultPersistTmpFallback)
}

// persist durably writes raw (the exact verified wire response bytes) to
// disk via a temp-file-then-rename so a crash mid-write can never leave a
// half-written, unparseable file in place of a good one. Best-effort: a
// persist failure is recorded but does not fail Refresh — the in-memory
// verified snapshot is already usable regardless of whether the durable
// copy lands.
func (c *Client) persist(raw []byte) error {
	path := c.persistPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("rulesetsnapshot: create state dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ruleset-snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("rulesetsnapshot: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: after a successful Rename below this is a no-op
	// (the file no longer exists at tmpName), so the error is deliberately
	// ignored rather than laundered into the caller's result.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rulesetsnapshot: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rulesetsnapshot: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rulesetsnapshot: rename into place: %w", err)
	}
	return nil
}

// loadPersisted best-effort loads and RE-VERIFIES the on-disk last-known-good
// snapshot at construction time, so Current() can serve immediately after a
// daemon restart — before any network fetch succeeds — without ever trusting
// disk content that was not itself signature-and-hash verified (defense in
// depth against a tampered on-disk cache while the daemon was stopped).
// Errors are recorded (LastError) but never returned: a missing or corrupt
// persisted file is not a construction failure, it is simply "no cache yet".
func (c *Client) loadPersisted() {
	path := c.persistPath()
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator/embedder-configured, not request-derived
	if err != nil {
		return
	}
	snap, err := c.parseAndVerify(context.Background(), raw)
	if err != nil {
		c.mu.Lock()
		c.lastErr = fmt.Errorf("rulesetsnapshot: persisted snapshot failed verification: %w", err)
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.current = &snap
	c.mu.Unlock()
}
