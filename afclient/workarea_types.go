// Package afclient workarea_types.go — wire types for the daemon's
// /api/daemon/workareas* operator surface. The contract is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md (D4 + D4a)
// and follows the pool-member state machine + archive semantics in
// 003-workarea-provider.md.
package afclient

import "time"

// WorkareaPoolStatus is the state of a workarea pool member, from the
// pool-member state machine in 003-workarea-provider.md.
type WorkareaPoolStatus string

// Pool-member states from the workarea state machine in
// 003-workarea-provider.md.
const (
	WorkareaStatusWarming   WorkareaPoolStatus = "warming"
	WorkareaStatusReady     WorkareaPoolStatus = "ready"
	WorkareaStatusAcquired  WorkareaPoolStatus = "acquired"
	WorkareaStatusReleasing WorkareaPoolStatus = "releasing"
	WorkareaStatusInvalid   WorkareaPoolStatus = "invalid"
	WorkareaStatusRetired   WorkareaPoolStatus = "retired"
	WorkareaStatusArchived  WorkareaPoolStatus = "archived"
)

// WorkareaKind disambiguates active pool members from on-disk archives in
// the GET /api/daemon/workareas response.
type WorkareaKind string

// Workarea kinds discriminate between live pool members and on-disk archives.
const (
	WorkareaKindActive   WorkareaKind = "active"
	WorkareaKindArchived WorkareaKind = "archived"
)

// WorkareaSummary is a single workarea entry returned by
// GET /api/daemon/workareas (list endpoint). The same shape covers both
// active pool members and on-disk archives; consumers use Kind to
// disambiguate.
type WorkareaSummary struct {
	ID         string             `json:"id"`
	Kind       WorkareaKind       `json:"kind"`
	ProviderID string             `json:"providerId"`
	SessionID  string             `json:"sessionId,omitempty"`
	ProjectID  string             `json:"projectId,omitempty"`
	Status     WorkareaPoolStatus `json:"status"`
	Ref        string             `json:"ref,omitempty"`
	Repository string             `json:"repository,omitempty"`

	// Archive-only fields. Populated when Kind == WorkareaKindArchived.
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	SizeBytes      int64      `json:"sizeBytes,omitempty"`
	SourceProvider string     `json:"sourceProvider,omitempty"`
	Disposition    string     `json:"disposition,omitempty"`

	// Active-only fields. Populated when Kind == WorkareaKindActive.
	AcquiredAt *time.Time `json:"acquiredAt,omitempty"`
	ReleasedAt *time.Time `json:"releasedAt,omitempty"`
	AgeSeconds int64      `json:"ageSeconds,omitempty"`
}

// Workarea is the full workarea record returned by
// GET /api/daemon/workareas/<id> (inspect endpoint). Fields match the
// Workarea interface in 003-workarea-provider.md.
type Workarea struct {
	ID                 string             `json:"id"`
	Kind               WorkareaKind       `json:"kind"`
	ProviderID         string             `json:"providerId"`
	SessionID          string             `json:"sessionId,omitempty"`
	ProjectID          string             `json:"projectId,omitempty"`
	Status             WorkareaPoolStatus `json:"status"`
	Path               string             `json:"path,omitempty"`
	Ref                string             `json:"ref,omitempty"`
	Repository         string             `json:"repository,omitempty"`
	CleanStateChecksum string             `json:"cleanStateChecksum,omitempty"`
	Toolchain          map[string]string  `json:"toolchain,omitempty"`
	Mode               string             `json:"mode,omitempty"`        // "exclusive" | "shared"
	AcquirePath        string             `json:"acquirePath,omitempty"` // "pool-warm" | "pool-fresh" | "cold"
	AcquiredAt         *time.Time         `json:"acquiredAt,omitempty"`
	ReleasedAt         *time.Time         `json:"releasedAt,omitempty"`
	ArchiveLocation    string             `json:"archiveLocation,omitempty"`
	OwnerSession       string             `json:"ownerSession,omitempty"`

	// Manifest is the captured per-archive manifest for archived
	// workareas (kind = "archived"). Free-form JSON keyed by archive
	// version; consumers display rather than enforce shape.
	Manifest map[string]any `json:"manifest,omitempty"`
}

// ListWorkareasResponse matches GET /api/daemon/workareas. Per ADR D4a,
// the daemon returns active pool members and on-disk archives in two
// arrays.
type ListWorkareasResponse struct {
	Active   []WorkareaSummary `json:"active"`
	Archived []WorkareaSummary `json:"archived"`
}

// WorkareaEnvelope wraps a single Workarea returned by
// GET /api/daemon/workareas/<id>.
type WorkareaEnvelope struct {
	Workarea Workarea `json:"workarea"`
}

// WorkareaRestoreRequest is the body for
// POST /api/daemon/workareas/<archiveID>/restore. Per ADR D4a, restore
// materialises the named archive into a fresh active pool member.
type WorkareaRestoreRequest struct {
	// Reason is an optional human-readable justification recorded in the
	// daemon's audit log alongside the restore.
	Reason string `json:"reason,omitempty"`
	// IntoSessionId optionally pins the restored workarea to a specific
	// session id. If the id is already in use the daemon returns 409.
	IntoSessionID string `json:"intoSessionId,omitempty"`
}

// WorkareaRestoreResult is the response from
// POST /api/daemon/workareas/<archiveID>/restore. The new id is distinct
// from the archive id (archives are immutable per ADR D4a).
type WorkareaRestoreResult struct {
	Workarea Workarea `json:"workarea"`
}

// WorkareaDiffStatus is the per-entry change classification.
type WorkareaDiffStatus string

// Per-entry diff change classifications.
const (
	WorkareaDiffStatusAdded    WorkareaDiffStatus = "added"
	WorkareaDiffStatusRemoved  WorkareaDiffStatus = "removed"
	WorkareaDiffStatusModified WorkareaDiffStatus = "modified"
)

// WorkareaDiffEntry is a single per-path entry in a workarea diff. Hashes
// are SHA-256 over file contents; missing for directories. Symlinks
// compared by target string.
type WorkareaDiffEntry struct {
	Path   string             `json:"path"`
	Status WorkareaDiffStatus `json:"status"`

	SizeA int64  `json:"sizeA,omitempty"`
	SizeB int64  `json:"sizeB,omitempty"`
	ModeA string `json:"modeA,omitempty"`
	ModeB string `json:"modeB,omitempty"`
	HashA string `json:"hashA,omitempty"`
	HashB string `json:"hashB,omitempty"`
}

// WorkareaDiffSummary is the aggregate-counts envelope. In a single-JSON
// response (entries below the streaming threshold) it sits alongside
// Entries; in NDJSON streaming it ships as the final line.
type WorkareaDiffSummary struct {
	WorkareaA string `json:"workareaA"`
	WorkareaB string `json:"workareaB"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Modified  int    `json:"modified"`
	Total     int    `json:"total"`
}

// WorkareaDiffEnvelope is the single-JSON response shape from
// GET /api/daemon/workareas/<idA>/diff/<idB> when entries ≤ the daemon's
// configured streaming threshold (default 1000, see daemon.yaml key
// workarea.diffStreamingThreshold). Consumers MUST handle both this
// shape and the NDJSON streaming variant via Content-Type discrimination
// per ADR D4a.
type WorkareaDiffEnvelope struct {
	Diff WorkareaDiffResult `json:"diff"`
}

// WorkareaDiffResult is the diff payload itself.
type WorkareaDiffResult struct {
	Summary WorkareaDiffSummary `json:"summary"`
	Entries []WorkareaDiffEntry `json:"entries"`
}
