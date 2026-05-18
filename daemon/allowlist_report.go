package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Phase 1c–1d helpers for reporting the daemon's project allowlist to the
// platform. The daemon yaml `projects[]` is authoritative; these helpers
// derive a stable wire shape + hash so the platform can mirror without the
// daemon shipping its full ProjectConfig (which carries clone/git fields
// the platform doesn't need).
//
// Reference: ../runs/2026-05-18-daemon-config-sync-DESIGN.md (Phase 1c, 1d).

// allowlistEntriesFromConfig projects []ProjectConfig down to the trimmed
// wire shape sent in RegisterRequest.DaemonProjects and HeartbeatPayload.
// Entries are sorted by id so the hash is stable regardless of yaml order.
//
// Returns nil (omitted on the wire) when the daemon has no projects
// configured. The platform treats nil as "unknown — host may serve any
// project the dispatcher routes its way" rather than "explicit empty".
func allowlistEntriesFromConfig(projects []ProjectConfig) []ProjectAllowlistEntry {
	if len(projects) == 0 {
		return nil
	}
	out := make([]ProjectAllowlistEntry, 0, len(projects))
	for _, p := range projects {
		// Defensive — yaml may decode entries missing id/repository if
		// the operator hand-edited the file. Skip rather than ship a
		// half-formed entry the platform would need to re-validate.
		if p.ID == "" || p.Repository == "" {
			continue
		}
		out = append(out, ProjectAllowlistEntry{
			ID:         p.ID,
			Repository: p.Repository,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// allowlistHash returns a stable SHA-256 hex digest of the allowlist
// entries (already sorted). Used on every heartbeat as a cheap change
// detector — the full entry list is included on the wire only when the
// hash differs from the platform's last-known value.
//
// Returns "" for nil/empty input. The platform interprets "" as
// "daemon did not report" rather than "explicit empty allowlist".
func allowlistHash(entries []ProjectAllowlistEntry) string {
	if len(entries) == 0 {
		return ""
	}
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.ID))
		h.Write([]byte{0}) // field separator — id vs repository must not collide
		h.Write([]byte(e.Repository))
		h.Write([]byte{0, 0}) // record separator
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeAllowlistKey returns a hash-friendly canonical form of an
// allowlist entry for tests that compare two slices. Currently a no-op
// passthrough; reserved for future repository-URL canonicalisation (e.g.
// dropping .git suffix, lowercasing host).
func normalizeAllowlistKey(e ProjectAllowlistEntry) string {
	return strings.ToLower(e.ID) + "@" + strings.TrimSuffix(e.Repository, ".git")
}
