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

// AllowlistEntriesFromConfig projects []ProjectConfig down to the trimmed
// wire shape sent in RegisterRequest.DaemonProjects and HeartbeatPayload.
// Entries are sorted by id so the hash is stable regardless of yaml order.
//
// Exported so embedders can drive multi-identity poll loops (e.g. a
// downstream embedder that registers separate per-org worker identities
// against the same shared spawner).
//
// Returns nil (omitted on the wire) when the daemon has no projects
// configured. The platform treats nil as "unknown — host may serve any
// project the dispatcher routes its way" rather than "explicit empty".
func AllowlistEntriesFromConfig(projects []ProjectConfig) []ProjectAllowlistEntry {
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

// ProjectAdmissionReport is the daemon's COMPLETE project-admission state as
// reported to an orchestrator.
//
// The repository-derived Entries alone are not that state, and reporting only
// them was a real defect: a project admitted with no repository resource
// produced identical entries and an identical hash, so the orchestrator never
// learned about it and kept routing work the daemon would then refuse. Because
// the enabled set was otherwise only sent at registration — which happens once,
// at process start — the operator-visible symptom was "enable the project, then
// restart the daemon before the platform believes you". Carrying Mode and
// EnabledProjectIDs in the hashed report removes the restart.
type ProjectAdmissionReport struct {
	// Mode is ProjectAdmissionModeEnumerated or ProjectAdmissionModeAllRouted.
	Mode string
	// EnabledProjectIDs is the authoritative admission set under the enumerated
	// mode. Meaningless (and typically empty) under all-routed.
	EnabledProjectIDs []string
	// Entries is the repository projection retained for the orchestrator's
	// repository mirror.
	Entries []ProjectAllowlistEntry
}

// admissionHash returns a stable SHA-256 hex digest of the whole admission
// report. Used on every heartbeat as a cheap change detector — the full report
// is put on the wire only when the hash differs from the last transmitted one.
//
// Returns "" only for a report that says nothing at all (enumerated mode, no
// enabled projects, no entries), preserving the established "" = "daemon did
// not report" signal. Any non-default mode or any enabled project yields a
// digest, which is the whole point: those are exactly the states the old
// entries-only hash rendered invisible.
func admissionHash(report ProjectAdmissionReport) string {
	mode := normalizeProjectAdmissionMode(report.Mode)
	ids := normalizeProjectIDs(report.EnabledProjectIDs)
	if mode == ProjectAdmissionModeEnumerated && len(ids) == 0 && len(report.Entries) == 0 {
		return ""
	}
	h := sha256.New()
	// Domain-separate each section so a mode string can never collide with a
	// project id, nor an id with a repository URL.
	h.Write([]byte("mode\x00"))
	h.Write([]byte(mode))
	h.Write([]byte{0, 0})
	h.Write([]byte("enabled\x00"))
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	h.Write([]byte{0, 0})
	h.Write([]byte("entries\x00"))
	for _, e := range report.Entries {
		h.Write([]byte(e.ID))
		h.Write([]byte{0}) // field separator — id vs repository must not collide
		h.Write([]byte(e.Repository))
		h.Write([]byte{0, 0}) // record separator
	}
	return hex.EncodeToString(h.Sum(nil))
}

// allowlistHash is the entries-only digest retained for callers that report
// nothing but the repository projection.
//
// Returns "" for nil/empty input. The platform interprets "" as
// "daemon did not report" rather than "explicit empty allowlist".
func allowlistHash(entries []ProjectAllowlistEntry) string {
	if len(entries) == 0 {
		return ""
	}
	return admissionHash(ProjectAdmissionReport{Entries: entries})
}

// normalizeAllowlistKey returns a hash-friendly canonical form of an
// allowlist entry for tests that compare two slices. Currently a no-op
// passthrough; reserved for future repository-URL canonicalisation (e.g.
// dropping .git suffix, lowercasing host).
func normalizeAllowlistKey(e ProjectAllowlistEntry) string {
	return strings.ToLower(e.ID) + "@" + strings.TrimSuffix(e.Repository, ".git")
}
