package rulesetsnapshot

import "time"

// ---------------------------------------------------------------------------
// Section 1: policy bundle.
// ---------------------------------------------------------------------------

// Policy is one active policy, carrying enough scope metadata for an
// in-process evaluator to reproduce a workspace ∪ project(scopeId) filter
// without a round trip. The policy language itself (whatever engine the
// publisher uses) is opaque here — this package never evaluates a policy
// body, only carries it as part of the signed, hashed bundle.
type Policy struct {
	ID      string  `json:"id"`
	Body    string  `json:"cedarPolicy"`
	Scope   string  `json:"scope"`
	ScopeID *string `json:"scopeId"`
}

// PolicyBundleSection is section 1 of the snapshot.
type PolicyBundleSection struct {
	WorkspaceID string   `json:"workspaceId"`
	Policies    []Policy `json:"policies"`
}

// ---------------------------------------------------------------------------
// Section 2: capacity profiles + grants — the "profile candidates" input.
// ---------------------------------------------------------------------------

// ReservationPosture is a capacity profile's reservation-wait configuration.
type ReservationPosture struct {
	Mode      string `json:"mode"`
	TimeoutMs *int64 `json:"timeoutMs"`
}

// CapacityProfile is one named, ordered policy over a list of pools
// (ADR-2026-08-12 D1.3). PoolIDs is the fallback chain in authored order.
type CapacityProfile struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	OrderingPolicy        string             `json:"orderingPolicy"`
	PreferenceVector      map[string]float64 `json:"preferenceVector"`
	ReservationPosture    ReservationPosture `json:"reservationPosture"`
	BurstPosture          string             `json:"burstPosture"`
	DisconnectLostAfterMs int64              `json:"disconnectLostAfterMs"`
	DisconnectReplace     bool               `json:"disconnectReplace"`
	DisconnectReconcile   string             `json:"disconnectReconcile"`
	IsOrgDefault          bool               `json:"isOrgDefault"`
	Revision              int                `json:"revision"`
	PoolIDs               []string           `json:"poolIds"`
}

// CapacityProfileGrant grants a profile to a project.
type CapacityProfileGrant struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	ProfileID string `json:"profileId"`
	IsPrimary bool   `json:"isPrimary"`
}

// CapacityProfilesSection is section 2 of the snapshot.
type CapacityProfilesSection struct {
	Profiles []CapacityProfile      `json:"profiles"`
	Grants   []CapacityProfileGrant `json:"grants"`
}

// ---------------------------------------------------------------------------
// Section 3: pool / execution-host inventory.
// ---------------------------------------------------------------------------

// Pool is one single-provider source of execution contexts
// (ADR-2026-08-07-execution-context-pool-and-placement-vocabulary.md D6.1).
type Pool struct {
	ID               string `json:"id"`
	ProviderID       string `json:"providerId"`
	DisplayName      string `json:"displayName"`
	ServesPersistent bool   `json:"servesPersistent"`
	ServesOnDemand   bool   `json:"servesOnDemand"`
	// Status is one of "active" | "paused" | "draining" | "disabled". Only
	// "active" is eligible for a fail-static claim decision — see
	// isEligiblePoolStatus in claim_eval.go.
	Status         string  `json:"status"`
	CostWeight     float64 `json:"costWeight"`
	Priority       int     `json:"priority"`
	SubstrateClass string  `json:"substrateClass"`
	// AllowedProjectIDs is nil for "unrestricted" (any project may route
	// here) or an explicit allowlist — snapshotted verbatim from whatever
	// the publisher's grant store exposes.
	AllowedProjectIDs []string `json:"allowedProjectIds"`
}

// Host is one execution host within a pool.
type Host struct {
	ID              string `json:"id"`
	ExecutionPoolID string `json:"executionPoolId"`
	// Status is one of "ready" | "draining" | "unhealthy" | "offline". Only
	// "ready" is treated as healthy for a fail-static claim decision.
	Status          string  `json:"status"`
	OS              *string `json:"os"`
	Arch            *string `json:"arch"`
	MaxSessions     int     `json:"maxSessions"`
	ActiveSessions  int     `json:"activeSessions"`
	LastHeartbeatMs *int64  `json:"lastHeartbeatMs"`
}

// PoolHostInventorySection is section 3 of the snapshot.
type PoolHostInventorySection struct {
	Pools []Pool `json:"pools"`
	Hosts []Host `json:"hosts"`
}

// ---------------------------------------------------------------------------
// Section 4: execution-cell matrix.
// ---------------------------------------------------------------------------

// Provider is one company × harness × auth-mode registry entry.
type Provider struct {
	ID                 string   `json:"id"`
	DisplayName        string   `json:"displayName"`
	ConfigNamespace    string   `json:"configNamespace"`
	SupportedAuthModes []string `json:"supportedAuthModes"`
	Category           string   `json:"category"`
	// HarnessByAuthMode maps an auth-mode string to the harness id that
	// serves it. The OSS fail-static evaluator only asks "is this harness id
	// present anywhere in this map for any provider" (claim_eval.go) — the
	// auth-mode *key* vocabulary is publisher-owned and not cross-walked
	// against this repo's own AuthMechanism enum, which names a different
	// axis (see BuildClaimLocalReality's doc comment).
	HarnessByAuthMode map[string]string `json:"harnessByAuthMode"`
}

// ModelProfile is one authored model profile.
type ModelProfile struct {
	ID             string   `json:"id"`
	Scope          string   `json:"scope"`
	OrgID          *string  `json:"orgId"`
	ProjectID      *string  `json:"projectId"`
	Name           string   `json:"name"`
	Slug           string   `json:"slug"`
	Provider       string   `json:"provider"`
	ModelCatalogID *string  `json:"modelCatalogId"`
	Effort         *string  `json:"effort"`
	AuthModes      []string `json:"authModes"`
	Audience       string   `json:"audience"`
}

// ExecutionCellMatrixSection is section 4 of the snapshot — the "cell
// matrix" input.
type ExecutionCellMatrixSection struct {
	Providers     []Provider     `json:"providers"`
	ModelProfiles []ModelProfile `json:"modelProfiles"`
}

// ---------------------------------------------------------------------------
// Section 5: posterior summary.
// ---------------------------------------------------------------------------

// Posterior is one provider × work-type Thompson-sampling summary. Carried
// for parity with the publisher's contract; the OSS fail-static claim
// evaluator does not read it (ranking is a hosted extension point per
// ADR-2026-08-12 D3) — a future local ordering policy could.
type Posterior struct {
	Provider          string  `json:"provider"`
	WorkType          string  `json:"workType"`
	Alpha             float64 `json:"alpha"`
	Beta              float64 `json:"beta"`
	AvgReward         float64 `json:"avgReward"`
	TotalObservations int     `json:"totalObservations"`
	LastUpdated       int64   `json:"lastUpdated"`
}

// PosteriorSummarySection is section 5 of the snapshot.
type PosteriorSummarySection struct {
	Posteriors []Posterior `json:"posteriors"`
}

// ---------------------------------------------------------------------------
// The bundle.
// ---------------------------------------------------------------------------

// Sections is the typed, best-effort projection of the five signed sections,
// decoded permissively (unknown fields ignored — this is a wire contract
// from an external publisher, not this package's own versioned schema).
type Sections struct {
	PolicyBundle        PolicyBundleSection        `json:"policyBundle"`
	CapacityProfiles    CapacityProfilesSection    `json:"capacityProfiles"`
	PoolHostInventory   PoolHostInventorySection   `json:"poolHostInventory"`
	ExecutionCellMatrix ExecutionCellMatrixSection `json:"executionCellMatrix"`
	PosteriorSummary    PosteriorSummarySection    `json:"posteriorSummary"`
}

// Snapshot is one verified-in-full ruleset snapshot held in memory. A value
// of this type has ALWAYS passed content-hash and Ed25519-signature
// verification — see Client.Refresh and parseAndVerify. There is
// deliberately no exported constructor: the only way to obtain a Snapshot
// is through a Client that has verified it.
type Snapshot struct {
	OrgID        string
	Revision     int
	RulesetRev   string
	ContentHash  string
	SigningKeyID string
	CompiledAt   time.Time
	Sections     Sections
}

// Status is the point-in-time staleness signal for a cached Snapshot
// (Consul `Age` / Envoy TTL precedent, 05-sota-research.md §A5). Age is
// always recomputed relative to "now" at read time — never frozen at fetch
// time — so two callers a minute apart see different Age values for the
// same cached Snapshot, exactly like an HTTP `Age` response header would.
type Status struct {
	Rev        string
	Age        time.Duration
	Degraded   bool
	CompiledAt time.Time
}
