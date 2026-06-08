package codeintel

// arch_types.go — the arch-intel domain vocabulary for the native Go store.
//
// Port of donmai-libraries/packages/architectural-intelligence/src/types.ts.
// JSON field names match the TS shapes so the persisted payloads and any
// JSON output are byte-compatible with the TS @donmai/architectural-intelligence
// package (downstream tools see identical JSON regardless of producer).
//
// Note on naming: arch_drift.go already declares a `Deviation` (the LLM drift
// VERDICT shape). The persisted deviation NODE here is `StoredDeviation` to
// avoid the collision; its JSON matches the TS Deviation interface.

import (
	"encoding/json"
	"time"
)

// CitationConfidence levels. The "authored intent > inferences" constraint is
// enforced by ranking citations in this order:
//
//	authored > inferred-high > inferred-medium > inferred-low
const (
	ConfidenceAuthored       = "authored"
	ConfidenceInferredHigh   = "inferred-high"
	ConfidenceInferredMedium = "inferred-medium"
	ConfidenceInferredLow    = "inferred-low"
)

// CitationConfidenceRank is the numeric rank for confidence levels (higher =
// more authoritative). Mirrors CITATION_CONFIDENCE_RANK in types.ts.
var CitationConfidenceRank = map[string]int{
	ConfidenceAuthored:       4,
	ConfidenceInferredHigh:   3,
	ConfidenceInferredMedium: 2,
	ConfidenceInferredLow:    1,
}

// ArchScope is the four-level scope model (project | org | tenant | global),
// with an optional repo refinement for full repo-scoped synthesis. Mirrors
// ArchScope in types.ts.
type ArchScope struct {
	Level     string `json:"level"`
	ProjectID string `json:"projectId,omitempty"`
	OrgID     string `json:"orgId,omitempty"`
	TenantID  string `json:"tenantId,omitempty"`
	Repo      string `json:"repo,omitempty"`
}

// AuthoredDoc identifies a human-authored source document. When present on an
// observation's source AND confidence >= 0.9, the materialized citation is
// 'authored'. Mirrors the authoredDoc shape in types.ts.
type AuthoredDoc struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // claude-md | adr | spec
}

// ArchChangeRef identifies a VCS change without coupling to a provider. Mirrors
// ChangeRef in types.ts. (Named ArchChangeRef to avoid colliding with the
// JSON-output ChangeRefJSON already declared in arch_native.go.)
type ArchChangeRef struct {
	Repository  string `json:"repository"`
	Kind        string `json:"kind"` // commit | pr | branch
	SHA         string `json:"sha,omitempty"`
	PrNumber    int    `json:"prNumber,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Description string `json:"description,omitempty"`
}

// ObservationSource describes where an observation came from. Mirrors the
// ArchObservation.source shape in types.ts.
type ObservationSource struct {
	SessionID   string         `json:"sessionId,omitempty"`
	ChangeRef   *ArchChangeRef `json:"changeRef,omitempty"`
	ExtractorID string         `json:"extractorId,omitempty"`
	AuthoredDoc *AuthoredDoc   `json:"authoredDoc,omitempty"`
}

// ArchObservation is the write input to the architectural intelligence graph.
// Payload is held as raw JSON (the materializer interprets it per Kind). Mirrors
// ArchObservation in types.ts.
type ArchObservation struct {
	Kind       string            `json:"kind"` // pattern | convention | decision | deviation
	Payload    json.RawMessage   `json:"payload"`
	Source     ObservationSource `json:"source"`
	Confidence float64           `json:"confidence"`
	Scope      ArchScope         `json:"scope"`
}

// CitationSource is the discriminated-union source of a citation. Only the
// fields relevant to Kind are populated. Mirrors the Citation.source union.
type CitationSource struct {
	Kind      string         `json:"kind"` // file | session | change | adr | external
	Path      string         `json:"path,omitempty"`
	LineStart int            `json:"lineStart,omitempty"`
	LineEnd   int            `json:"lineEnd,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	ChangeRef *ArchChangeRef `json:"changeRef,omitempty"`
	ADRID     string         `json:"adrId,omitempty"`
	URL       string         `json:"url,omitempty"`
	Title     string         `json:"title,omitempty"`
}

// Citation traces an architectural assertion back to its origin. Mirrors
// Citation in types.ts.
type Citation struct {
	ID         string         `json:"id"`
	Source     CitationSource `json:"source"`
	Confidence string         `json:"confidence"`
	RecordedAt time.Time      `json:"recordedAt"`
	Excerpt    string         `json:"excerpt,omitempty"`
}

// PatternLocation is one code region where a pattern is observed.
type PatternLocation struct {
	Path string `json:"path"`
	Role string `json:"role,omitempty"`
}

// ArchitecturalPattern is a recurring structural/behavioral pattern. Mirrors
// ArchitecturalPattern in types.ts.
type ArchitecturalPattern struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Locations   []PatternLocation `json:"locations"`
	Tags        []string          `json:"tags"`
	Citations   []Citation        `json:"citations"`
	Scope       ArchScope         `json:"scope"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// ConventionExample is a representative example of a convention.
type ConventionExample struct {
	Path    string `json:"path"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Convention is a consistent practice the codebase follows. Mirrors Convention.
type Convention struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Examples    []ConventionExample `json:"examples"`
	Authored    bool                `json:"authored"`
	Citations   []Citation          `json:"citations"`
	Scope       ArchScope           `json:"scope"`
	CreatedAt   time.Time           `json:"createdAt"`
	UpdatedAt   time.Time           `json:"updatedAt"`
}

// DecisionAlternative is an evaluated-and-rejected option.
type DecisionAlternative struct {
	Option          string `json:"option"`
	RejectionReason string `json:"rejectionReason,omitempty"`
}

// Decision is a resolved architectural trade-off. Mirrors Decision in types.ts.
type Decision struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Chosen       string                `json:"chosen"`
	Alternatives []DecisionAlternative `json:"alternatives"`
	Rationale    string                `json:"rationale"`
	Status       string                `json:"status"` // active | superseded | deprecated
	Supersedes   string                `json:"supersedes,omitempty"`
	Citations    []Citation            `json:"citations"`
	Scope        ArchScope             `json:"scope"`
	CreatedAt    time.Time             `json:"createdAt"`
	UpdatedAt    time.Time             `json:"updatedAt"`
}

// DeviatesFrom names the established norm a deviation diverges from. Exactly one
// of the id fields is populated per Kind.
type DeviatesFrom struct {
	Kind         string `json:"kind"` // pattern | convention | decision
	PatternID    string `json:"patternId,omitempty"`
	ConventionID string `json:"conventionId,omitempty"`
	DecisionID   string `json:"decisionId,omitempty"`
}

// StoredDeviation is the persisted deviation NODE. Its JSON matches the TS
// Deviation interface (the in-package `Deviation` type in arch_drift.go is the
// LLM verdict shape, which is different).
type StoredDeviation struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	DeviatesFrom DeviatesFrom   `json:"deviatesFrom"`
	IntroducedBy *ArchChangeRef `json:"introducedBy,omitempty"`
	Status       string         `json:"status"`   // pending | intentional | unintentional | resolved
	Severity     string         `json:"severity"` // high | medium | low
	Citations    []Citation     `json:"citations"`
	Scope        ArchScope      `json:"scope"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// ArchView is the retrieval result of Store.Query. Mirrors ArchView in types.ts
// (drift omitted — that is the LaneAdapter's concern, not the store's).
type ArchView struct {
	Patterns    []ArchitecturalPattern `json:"patterns"`
	Conventions []Convention           `json:"conventions"`
	Decisions   []Decision             `json:"decisions"`
	Citations   []Citation             `json:"citations"`
	Scope       ArchScope              `json:"scope"`
	RetrievedAt time.Time              `json:"retrievedAt"`
}

// ArchQuerySpec is the query input to Store.Query. Mirrors ArchQuerySpec.
type ArchQuerySpec struct {
	WorkType string    `json:"workType"`
	Paths    []string  `json:"paths,omitempty"`
	IssueID  string    `json:"issueId,omitempty"`
	Scope    ArchScope `json:"scope"`
	Repos    []string  `json:"repos,omitempty"`
}

// effectiveRepos unions spec.Scope.Repo (single-repo shorthand) with spec.Repos
// (explicit list), de-dupes, drops empties. Empty result → "whole project/org
// corpus" (backward-compatible), NOT "match zero repos". Mirrors effectiveRepos.
func effectiveRepos(spec ArchQuerySpec) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(r string) {
		if r == "" {
			return
		}
		if _, ok := seen[r]; ok {
			return
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	add(spec.Scope.Repo)
	for _, r := range spec.Repos {
		add(r)
	}
	return out
}

// observationConfidenceToLevel maps an observation's numeric confidence +
// authoredDoc flag to a CitationConfidence level. The authored-intent
// constraint: only authored-doc sources with confidence >= 0.9 receive
// 'authored'. Mirrors _observationConfidenceToLevel in sqlite-impl.ts.
func observationConfidenceToLevel(obs ArchObservation) string {
	if obs.Source.AuthoredDoc != nil && obs.Confidence >= 0.9 {
		return ConfidenceAuthored
	}
	if obs.Confidence >= 0.7 {
		return ConfidenceInferredHigh
	}
	if obs.Confidence >= 0.4 {
		return ConfidenceInferredMedium
	}
	return ConfidenceInferredLow
}
