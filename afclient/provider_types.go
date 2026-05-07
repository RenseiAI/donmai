// Package afclient provider_types.go — wire types for the daemon's
// /api/daemon/providers* operator surface. The contract is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md (D4) and
// matches the eight Provider Family vocabulary from
// 002-provider-base-contract.md.
package afclient

// ProviderFamily is one of the eight plugin families defined in
// 002-provider-base-contract.md. Values match the canonical string
// identifiers from the architecture corpus.
type ProviderFamily string

// Family identifiers for the eight Provider Family contracts in
// 002-provider-base-contract.md.
const (
	FamilySandbox       ProviderFamily = "sandbox"
	FamilyWorkarea      ProviderFamily = "workarea"
	FamilyAgentRuntime  ProviderFamily = "agent-runtime"
	FamilyVCS           ProviderFamily = "vcs"
	FamilyIssueTracker  ProviderFamily = "issue-tracker"
	FamilyDeployment    ProviderFamily = "deployment"
	FamilyAgentRegistry ProviderFamily = "agent-registry"
	FamilyKit           ProviderFamily = "kit"
)

// AllProviderFamilies lists the eight families in display order.
var AllProviderFamilies = []ProviderFamily{
	FamilySandbox,
	FamilyWorkarea,
	FamilyAgentRuntime,
	FamilyVCS,
	FamilyIssueTracker,
	FamilyDeployment,
	FamilyAgentRegistry,
	FamilyKit,
}

// ProviderScope is the activation scope level from
// 002-provider-base-contract.md.
type ProviderScope string

// Provider activation scope levels.
const (
	ScopeProject ProviderScope = "project"
	ScopeOrg     ProviderScope = "org"
	ScopeTenant  ProviderScope = "tenant"
	ScopeGlobal  ProviderScope = "global"
)

// ProviderTrustState encodes the three trust states visible in the
// operator surface.
type ProviderTrustState string

// Provider trust states surfaced in the operator view.
const (
	TrustSignedVerified   ProviderTrustState = "signed-verified"
	TrustSignedUnverified ProviderTrustState = "signed-unverified"
	TrustUnsigned         ProviderTrustState = "unsigned"
)

// ProviderSource is where the provider originated.
type ProviderSource string

// Provider sources — where the registered provider originated.
const (
	SourceBundled  ProviderSource = "bundled"
	SourceRegistry ProviderSource = "registry"
	SourceLocal    ProviderSource = "local"
)

// ProviderStatus is the current activation status.
type ProviderStatus string

// Provider activation statuses.
const (
	StatusReady     ProviderStatus = "ready"
	StatusDegraded  ProviderStatus = "degraded"
	StatusUnhealthy ProviderStatus = "unhealthy"
	StatusInactive  ProviderStatus = "inactive"
)

// Provider is the API representation of a registered provider, as returned
// by GET /api/daemon/providers and GET /api/daemon/providers/<id>. Maps to
// the base contract in 002-provider-base-contract.md.
type Provider struct {
	// Identity
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Version string         `json:"version"`
	Family  ProviderFamily `json:"family"`

	// Runtime state
	Scope  ProviderScope  `json:"scope"`
	Status ProviderStatus `json:"status"`
	Source ProviderSource `json:"source"`

	// Trust
	Trust      ProviderTrustState `json:"trust"`
	SignerID   string             `json:"signerId,omitempty"`
	SignedAt   string             `json:"signedAt,omitempty"`
	ManifestOK bool               `json:"manifestOk"`

	// Capabilities is the family-typed capability struct serialised as a
	// free-form JSON object. The typed shape varies per family; consumers
	// inspect Family to deserialise correctly. map[string]any lets the
	// CLI display and pass through the JSON without hardcoding every
	// family's struct.
	Capabilities map[string]any `json:"capabilities"`
}

// ListProvidersResponse matches GET /api/daemon/providers.
//
// Per ADR-2026-05-07-daemon-http-control-api.md § D4, the Wave-9 ship of
// the daemon's provider registry exposes only the AgentRuntime family
// (claude/codex/ollama/opencode/gemini/amp/stub). The remaining seven
// families return as empty until their per-family registries land in a
// future wave. Consumers MUST honour PartialCoverage when rendering — the
// "other families coming" caveat is sourced from the flag, not from
// sniffing for emptiness.
type ListProvidersResponse struct {
	// Providers is the populated set across all covered families.
	Providers []Provider `json:"providers"`

	// PartialCoverage is true when CoveredFamilies is a strict subset of
	// AllProviderFamilies.
	PartialCoverage bool `json:"partialCoverage"`

	// CoveredFamilies enumerates the families this daemon currently
	// surfaces. In Wave 9 this is exactly ["agent-runtime"].
	CoveredFamilies []ProviderFamily `json:"coveredFamilies"`
}

// ProviderEnvelope wraps a single provider returned by
// GET /api/daemon/providers/<id>.
type ProviderEnvelope struct {
	Provider Provider `json:"provider"`
}
