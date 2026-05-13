package afclient

// ── AgentCard types (H — workType lane) ──────────────────────────────────────
// Mirrors the canonical TypeScript interface in
// rensei-architecture/11-runtime-binding-strategy.md §1 and the schema
// decisions codified in ADR-2026-05-12-agentcard-schema-and-scope.md (D1–D8,
// D21). JSON tags match the wire keys emitted by the platform's
// /api/agents endpoint.

// AgentCardScope is the four-scope enum (D3, D5).
// Visibility cascades: system → org → project → workflow.
// workflow-scope cards are inline-only (no DB row until "Promote to project").
type AgentCardScope = string

const (
	// AgentCardScopeSystem is visible to all orgs, projects, and workflows.
	AgentCardScopeSystem AgentCardScope = "system"
	// AgentCardScopeOrg is visible to all projects and workflows within the org.
	AgentCardScopeOrg AgentCardScope = "org"
	// AgentCardScopeProject is visible to all workflows within the project.
	AgentCardScopeProject AgentCardScope = "project"
	// AgentCardScopeWorkflow is inline in a workflow definition; no DB row.
	AgentCardScopeWorkflow AgentCardScope = "workflow"
)

// AgentWorkType is the classification of work an agent performs (D21, D22).
// System-seeded values; per-org extension via org_work_types table.
type AgentWorkType = string

const (
	WorkTypeResearch       AgentWorkType = "research"
	WorkTypeBacklogWriting AgentWorkType = "backlog-writing"
	WorkTypeDevelopment    AgentWorkType = "development"
	WorkTypeCoordination   AgentWorkType = "coordination"
	WorkTypeQA             AgentWorkType = "qa"
	WorkTypeAcceptance     AgentWorkType = "acceptance"
	WorkTypeOther          AgentWorkType = "other"
)

// RuntimeKind is the §2 enum of eight runtime path types.
type RuntimeKind = string

const (
	RuntimeKindNative            RuntimeKind = "native"
	RuntimeKindNPM               RuntimeKind = "npm"
	RuntimeKindPythonPip         RuntimeKind = "python-pip"
	RuntimeKindHTTP              RuntimeKind = "http"
	RuntimeKindMCPServer         RuntimeKind = "mcp-server"
	RuntimeKindA2AProtocol       RuntimeKind = "a2a-protocol"
	RuntimeKindVendorHosted      RuntimeKind = "vendor-hosted"
	RuntimeKindLangchainRunnable RuntimeKind = "langchain-runnable"
)

// RuntimePath is a single ordered preference entry in AgentCard.Runtimes.
// The Config field is kind-discriminated — see 11-runtime-binding-strategy.md §1
// for the per-kind shape.
type RuntimePath struct {
	// Kind is the runtime type (RuntimeKind* constants).
	Kind string `json:"kind"`
	// Config holds kind-specific parameters as a raw JSON object.
	// Discriminated by Kind:
	//   native:              { providerId, modelProfileId? }
	//   npm:                 { package, versionSpec?, entrypoint? }
	//   python-pip:          { package, versionSpec?, entrypoint? }
	//   http:                { method, url, bodyTemplate? }
	//   mcp-server:          { transport, command?, args?, url? }
	//   a2a-protocol:        { endpoint, skillsAdvertised }
	//   vendor-hosted:       { vendor, vendorAgentId, sdk }
	//   langchain-runnable:  { hubRef, sdk, commitSha? }
	Config map[string]any `json:"config"`
	// Preference is an optional tie-breaker; lower = preferred.
	// Zero value means unspecified; natural array order is used.
	Preference *int `json:"preference,omitempty"`
}

// AuthRequirement declares a single auth obligation that crosses all runtime
// paths. Multiple entries are AND-combined (all must be satisfied).
type AuthRequirement struct {
	// Kind is one of: api-key | oauth | service-account | host-session | none.
	Kind string `json:"kind"`
	// EnvVar is the environment variable name for api-key auth (e.g. "ELEVENLABS_API_KEY").
	EnvVar *string `json:"envVar,omitempty"`
	// OAuthProvider names the OAuth provider for oauth auth (e.g. "linear", "notion", "github").
	OAuthProvider *string `json:"oauthProvider,omitempty"`
	// VaultRef is resolved at dispatch time from the credential store.
	VaultRef *string `json:"vaultRef,omitempty"`
}

// SubstrateRequirement declares a substrate need beyond the runtime path itself.
// Examples: network egress to specific hosts, host binaries, GPU, persistent storage.
type SubstrateRequirement struct {
	// Kind is one of: network-egress | host-binary | workarea | gpu | persistent-storage | long-running.
	Kind string `json:"kind"`
	// Config holds kind-specific parameters as a raw JSON object.
	// Discriminated by Kind:
	//   network-egress:      { hosts: string[] }
	//   host-binary:         { name, minVersion? }
	//   workarea:            { mode: 'fresh'|'persistent', sizeMB? }
	//   long-running:        { maxDurationSec, suspendable }
	Config map[string]any `json:"config"`
}

// TrustProvenanceInfo records the import provenance for a card.
type TrustProvenanceInfo struct {
	// SourceURL is the canonical upstream source (e.g. a GitHub tree URL).
	SourceURL *string `json:"sourceUrl,omitempty"`
	// SourceCommit is the git commit sha at import time.
	SourceCommit *string `json:"sourceCommit,omitempty"`
	// ImportedBy is the operator or system principal that mounted the ARP.
	ImportedBy string `json:"importedBy"`
	// ImportedAt is the RFC3339 timestamp of import.
	ImportedAt string `json:"importedAt"`
}

// TrustClaims carries the security posture of an AgentCard.
// Consumed at dispatch time by Cedar policy gates.
type TrustClaims struct {
	// Tier is the trust classification: system | partner | community | untrusted.
	Tier string `json:"tier"`
	// Signature is an optional detached signature over the card body.
	Signature *string `json:"signature,omitempty"`
	// SigningKeyID is the mount-asserted key identifier used to verify Signature.
	SigningKeyID *string `json:"signingKeyId,omitempty"`
	// Provenance records the import chain for auditing.
	Provenance TrustProvenanceInfo `json:"provenance"`
	// EvaluatedQualityScore is a Tessl-style 0-1 composite quality metric.
	EvaluatedQualityScore *float64 `json:"evaluatedQualityScore,omitempty"`
}

// PartialRef is a reference to a named Partial in the partials catalog.
// Partials are prose-overlay fragments that the runtime assembles into the
// agent's system prompt before dispatch.
type PartialRef struct {
	// ID is the platform-assigned partial identifier (e.g. "par_…").
	ID string `json:"id"`
	// MetadataID is the user-facing stable name (e.g. "code-review-guidelines").
	MetadataID string `json:"metadataId"`
	// Scope is the scope at which this partial was resolved.
	Scope string `json:"scope"`
	// Order is the insertion position within the assembled prompt; lower = earlier.
	Order *int `json:"order,omitempty"`
}

// AgentCardToolSurface declares the tool allow/disallow lists.
type AgentCardToolSurface struct {
	Allow    []string `json:"allow"`
	Disallow []string `json:"disallow"`
}

// AgentCard is the canonical materialized form of an agent in the
// agent_cards table, as defined by 11-runtime-binding-strategy.md §1
// and ADR-2026-05-12-agentcard-schema-and-scope.md.
//
// The struct mirrors the TypeScript interface exactly so JSON round-trips are
// lossless. All TS optional fields are represented as pointer types or
// omitempty slices to preserve the original zero-vs-absent distinction.
//
// Field count: 24 exported fields (identity × 7, provenance × 4,
// composition × 3, ontology axes × 4, workType × 1, tool surface × 1,
// lifecycle × 4).
type AgentCard struct {
	// ── Identity (existing + A1 additions) ──────────────────────────────────

	// ID is the platform-generated identifier (e.g. "ag_…").
	ID string `json:"id"`
	// MetadataID is the user-facing stable identifier (e.g. "backlog-writer").
	MetadataID string `json:"metadataId"`
	// Name is the human-readable display name.
	Name string `json:"name"`
	// Description is a short prose description of what the agent does.
	Description string `json:"description"`
	// Version is the monotonic integer version per MetadataID (D2).
	Version int `json:"version"`
	// Scope is one of: system | org | project | workflow (D3, D5).
	Scope string `json:"scope"`
	// ScopeOwnerID is null for scope=system; otherwise the owning org/project ID.
	ScopeOwnerID *string `json:"scopeOwnerId"`

	// ── Provenance (A2 columns — ARP materialization tracking) ──────────────

	// SourceProviderID identifies the ARP that materialized this card.
	// Values: 'db:internal' | 'kit:<kitId>' | 'a2a:<peer>' | 'tessl:<owner/name>'
	//         | 'github:<repo>' | 'openai-assistant:<orgId>' | …
	SourceProviderID string `json:"sourceProviderId"`
	// SourceLocator is the ARP-specific pointer to the upstream source
	// (e.g. "<owner/name>@<version>", "asst_xxx"). Null for db:internal cards.
	SourceLocator *string `json:"sourceLocator,omitempty"`
	// LastReconciledAt is the RFC3339 timestamp of the most recent ARP sync.
	// Null for author-time-only cards (reconcileTTLSec = null).
	LastReconciledAt *string `json:"lastReconciledAt,omitempty"`
	// ReconcileTTLSec is the reconciler refresh interval in seconds.
	// Null means the card is author-time-only and never auto-refreshed.
	ReconcileTTLSec *int `json:"reconcileTtlSec,omitempty"`

	// ── Composition ─────────────────────────────────────────────────────────

	// Partials is the ordered list of partial references assembled at dispatch.
	Partials []PartialRef `json:"partials,omitempty"`
	// Capabilities is the flat-typed boolean capability struct (REN-1513 pattern).
	Capabilities map[string]bool `json:"capabilities,omitempty"`

	// ── THE ONTOLOGY — five runtime/auth/substrate axes ─────────────────────

	// Runtimes is the ordered list of runtime paths (≥1 entry required).
	// The resolver picks the first satisfiable path.
	Runtimes []RuntimePath `json:"runtimes"`
	// Auth is the cross-cutting auth requirements applying to ALL runtime paths.
	Auth []AuthRequirement `json:"auth"`
	// Requires declares substrate requirements beyond the runtime path itself.
	Requires []SubstrateRequirement `json:"requires"`
	// Trust carries security posture consumed by Cedar policy gates at dispatch.
	Trust TrustClaims `json:"trust"`

	// ── WorkType (D21) ──────────────────────────────────────────────────────

	// WorkType is the SDLC-pathway classification for this agent.
	// Required at dispatch; ARP imports classify heuristically; operators
	// override at /admin/agents/<id>.
	// System-seeded values: research | backlog-writing | development |
	// coordination | qa | acceptance | other.
	// Per-org extension via org_work_types table (D22).
	WorkType string `json:"workType"`

	// ── Tool surface (preserved from existing AgentDefinition) ───────────────

	// Tools declares the allow/disallow tool permission lists.
	Tools AgentCardToolSurface `json:"tools"`

	// ── Lifecycle ────────────────────────────────────────────────────────────

	// ModelProfileID references the model_profiles table entry for this card (D23).
	ModelProfileID *string `json:"modelProfileId,omitempty"`
	// PublishedAt is the RFC3339 timestamp when this version was published.
	// Null means draft.
	PublishedAt *string `json:"publishedAt,omitempty"`
	// DeprecatedAt is the RFC3339 timestamp when this card was deprecated.
	// Null means active.
	DeprecatedAt *string `json:"deprecatedAt,omitempty"`
	// Tags is the free-form tag list for operator filtering.
	Tags []string `json:"tags,omitempty"`
}

// AgentScopeQuery carries the scope parameters for /api/agents list calls.
// At most one of OrgID and ProjectID should be set; both empty returns
// system-scope cards visible to all callers.
type AgentScopeQuery struct {
	// Scope restricts results to a specific scope tier.
	// Empty returns the natural union visible to the caller's (org, project).
	Scope string `json:"scope,omitempty"`
	// OrgID restricts results to cards owned by this org (scope=org rows).
	OrgID string `json:"orgId,omitempty"`
	// ProjectID restricts results to cards owned by this project (scope=project rows).
	ProjectID string `json:"projectId,omitempty"`
	// WorkType filters results to a specific workType classification.
	WorkType string `json:"workType,omitempty"`
}

// AgentListResponse matches GET /api/agents.
type AgentListResponse struct {
	Agents    []AgentCard `json:"agents"`
	Count     int         `json:"count"`
	Timestamp string      `json:"timestamp"`
}
