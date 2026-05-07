// Package afclient routing_types.go — wire types for the daemon's
// /api/daemon/routing/* operator surface. The contract is locked in
// rensei-architecture/ADR-2026-05-07-daemon-http-control-api.md (D4) and
// surfaces the cross-provider scheduler decisions described in
// 004-sandbox-capability-matrix.md and the REN-205 reframe.
package afclient

import "time"

// RoutingConfig is the current routing configuration returned by
// GET /api/daemon/routing/config. It surfaces Thompson-Sampling state
// across both dimensions (LLM × sandbox) per
// 004-sandbox-capability-matrix.md and the REN-205 reframe.
type RoutingConfig struct {
	// CapabilityFilters are the active hard-filter constraints the
	// scheduler enforces before scoring (e.g. region, OS, arch, GPU
	// requirements).
	CapabilityFilters []CapabilityFilter `json:"capabilityFilters"`

	// Weights holds the tenant-level scoring bias between cost and
	// latency. Values sum to 1.0; default is {Cost: 0.7, Latency: 0.3}
	// per 004.
	Weights RoutingWeights `json:"weights"`

	// SandboxProviders lists the known sandbox provider IDs and their
	// Thompson-Sampling state (alpha, beta, recent scores).
	SandboxProviders []SandboxProviderState `json:"sandboxProviders"`

	// LLMProviders lists the known LLM provider IDs and their
	// Thompson-Sampling state.
	LLMProviders []LLMProviderState `json:"llmProviders"`

	// RecentDecisions is the last N routing decisions (capped at 50 for
	// the show view). Full history is available via the explain endpoint
	// keyed by session id.
	RecentDecisions []RoutingDecision `json:"recentDecisions"`

	// CapturedAt is when this snapshot was taken.
	CapturedAt time.Time `json:"capturedAt"`
}

// CapabilityFilter is a single hard constraint applied before scoring.
type CapabilityFilter struct {
	// Field is the capability field name (e.g. "os", "arch", "region").
	Field string `json:"field"`
	// Op is the operator: "eq", "in", "lte", "gte".
	Op string `json:"op"`
	// Value is the filter value (string, number, or array serialised to
	// string).
	Value string `json:"value"`
}

// RoutingWeights is the tenant-level scoring bias between cost and
// latency.
type RoutingWeights struct {
	Cost    float64 `json:"cost"`
	Latency float64 `json:"latency"`
}

// SandboxProviderState is the Thompson-Sampling state for one sandbox
// provider.
type SandboxProviderState struct {
	ProviderID      string   `json:"providerId"`
	Alpha           float64  `json:"alpha"`
	Beta            float64  `json:"beta"`
	RecentScore     *float64 `json:"recentScore,omitempty"`
	RecentCostCents *float64 `json:"recentCostCents,omitempty"`
	RecentLatencyMs *float64 `json:"recentLatencyMs,omitempty"`
	SelectionCount  int      `json:"selectionCount"`
}

// LLMProviderState is the Thompson-Sampling state for one LLM provider.
type LLMProviderState struct {
	ProviderID     string   `json:"providerId"`
	Model          string   `json:"model,omitempty"`
	Alpha          float64  `json:"alpha"`
	Beta           float64  `json:"beta"`
	RecentScore    *float64 `json:"recentScore,omitempty"`
	SelectionCount int      `json:"selectionCount"`
}

// RoutingDecision is a single scheduler dispatch decision, emitted as a
// Layer 6 hook event (kind: "routing-decision") per
// 002-provider-base-contract.md.
type RoutingDecision struct {
	SessionID          string              `json:"sessionId"`
	ChosenSandbox      string              `json:"chosenSandbox"`
	ChosenLLM          string              `json:"chosenLLM"`
	RejectedCandidates []RejectedCandidate `json:"rejectedCandidates,omitempty"`
	Score              float64             `json:"score"`
	EstimatedCostCents *float64            `json:"estimatedCostCents,omitempty"`
	EstimatedLatencyMs *float64            `json:"estimatedLatencyMs,omitempty"`
	DecidedAt          time.Time           `json:"decidedAt"`
}

// RejectedCandidate is a provider that was considered but not chosen.
type RejectedCandidate struct {
	ProviderID string `json:"providerId"`
	// Dimension is the routing dimension: "sandbox" or "llm".
	Dimension string `json:"dimension"`
	// Reason categories from 004: "capability-filter", "tenant-policy",
	// "capacity-filter", "score-loss".
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// RoutingConfigResponse wraps RoutingConfig in the
// /api/daemon/routing/config response.
type RoutingConfigResponse struct {
	Config RoutingConfig `json:"config"`
}

// RoutingExplainResponse is the full decision trace for a specific
// session, returned by GET /api/daemon/routing/explain/<sessionId>.
type RoutingExplainResponse struct {
	SessionID string             `json:"sessionId"`
	Decision  RoutingDecision    `json:"decision"`
	Trace     []RoutingTraceStep `json:"trace"`
}

// RoutingTraceStep is a single step in the scheduler's decision trace.
type RoutingTraceStep struct {
	Step int `json:"step"`
	// Phase is the scheduler phase:
	// "capability-filter", "tenant-policy", "capacity-filter", "score".
	Phase string `json:"phase"`
	// Dimension is the routing dimension: "sandbox" or "llm".
	Dimension  string               `json:"dimension"`
	Remaining  []string             `json:"remaining"`
	Eliminated []EliminatedProvider `json:"eliminated,omitempty"`
	Note       string               `json:"note,omitempty"`
}

// EliminatedProvider captures a provider that was eliminated at a trace
// step.
type EliminatedProvider struct {
	ProviderID string `json:"providerId"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}
