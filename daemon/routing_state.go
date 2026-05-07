// Package daemon routing_state.go — in-process routing trace store and
// configuration projector for the /api/daemon/routing/* surface (Wave 9 / A4).
//
// The OSS daemon does not yet ship a real cross-provider scheduler in
// production. The store therefore defines the shape the eventual scheduler
// will record decisions through, and the read paths used by the HTTP
// handlers in handle_routing.go.
//
// See ADR-2026-05-07-daemon-http-control-api.md §D4 for the wire contract,
// 004-sandbox-capability-matrix.md for the cross-provider scheduler model,
// and the forward reference at
// /api/daemon/routing/explain/<sessionID> in the same doc.
package daemon

import (
	"sort"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// DefaultRoutingRingBufferSize is the maximum number of recent routing
// decisions retained for the GetConfig view. The explain endpoint key is
// per-session and bounded by the same ring — a session whose decision has
// fallen out of the ring returns 404.
const DefaultRoutingRingBufferSize = 50

// DefaultRoutingWeights are the cost/latency scoring weights described in
// 004-sandbox-capability-matrix.md §"Open questions" — 70/30 cost/latency
// is the documented default. The store returns these on every GetConfig
// call until a tenant config layer overrides them in a future wave.
var DefaultRoutingWeights = afclient.RoutingWeights{Cost: 0.7, Latency: 0.3}

// recordedDecision pairs a RoutingDecision with the trace produced by the
// scheduler. The pair lives in the ring buffer; the trace is what the
// explain endpoint surfaces.
type recordedDecision struct {
	decision afclient.RoutingDecision
	trace    []afclient.RoutingTraceStep
}

// RoutingTraceStore is the in-process record of routing decisions. The
// scheduler (or, in this wave, the test harness) feeds it via
// RecordDecision; HTTP handlers read via GetConfig and Explain.
//
// The store is safe for concurrent use.
type RoutingTraceStore struct {
	mu        sync.RWMutex
	ringSize  int
	ring      []recordedDecision  // chronological, oldest first; len ≤ ringSize
	bySession map[string]recordedDecision
}

// NewRoutingTraceStore constructs a store with the given ring-buffer size.
// ringSize ≤ 0 falls back to DefaultRoutingRingBufferSize.
func NewRoutingTraceStore(ringSize int) *RoutingTraceStore {
	if ringSize <= 0 {
		ringSize = DefaultRoutingRingBufferSize
	}
	return &RoutingTraceStore{
		ringSize:  ringSize,
		ring:      make([]recordedDecision, 0, ringSize),
		bySession: make(map[string]recordedDecision),
	}
}

// RecordDecision appends decision + trace to the store. If the store is
// already at ring capacity, the oldest entry is evicted from both the
// ring and the per-session lookup. Recording with an empty SessionID is
// allowed (the ring still tracks it) but the explain lookup is keyed by
// SessionID, so an unkeyed entry is invisible to Explain.
func (s *RoutingTraceStore) RecordDecision(decision afclient.RoutingDecision, trace []afclient.RoutingTraceStep) {
	// Defensive copy: callers may continue to mutate trace after recording.
	traceCopy := make([]afclient.RoutingTraceStep, len(trace))
	copy(traceCopy, trace)
	rec := recordedDecision{decision: decision, trace: traceCopy}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ring) >= s.ringSize {
		evicted := s.ring[0]
		s.ring = s.ring[1:]
		// Only forget the session lookup when the evicted record is the
		// one bySession currently points at — RecordDecision overwrites
		// per-session entries on each new decision and we don't want to
		// accidentally drop a fresher record.
		if cur, ok := s.bySession[evicted.decision.SessionID]; ok && cur.decision.DecidedAt.Equal(evicted.decision.DecidedAt) {
			delete(s.bySession, evicted.decision.SessionID)
		}
	}
	s.ring = append(s.ring, rec)
	if decision.SessionID != "" {
		s.bySession[decision.SessionID] = rec
	}
}

// recentDecisions returns a copy of the ring contents in chronological
// order (oldest first). Caller must not mutate.
func (s *RoutingTraceStore) recentDecisions() []afclient.RoutingDecision {
	out := make([]afclient.RoutingDecision, 0, len(s.ring))
	for _, r := range s.ring {
		out = append(out, r.decision)
	}
	return out
}

// Explain returns the recorded decision and trace for sessionID. Returns
// false when the session has no recorded decision (or the decision has
// been evicted from the ring).
func (s *RoutingTraceStore) Explain(sessionID string) (afclient.RoutingDecision, []afclient.RoutingTraceStep, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.bySession[sessionID]
	if !ok {
		return afclient.RoutingDecision{}, nil, false
	}
	// Defensive copy of trace so the caller cannot mutate the stored slice.
	traceCopy := make([]afclient.RoutingTraceStep, len(rec.trace))
	copy(traceCopy, rec.trace)
	return rec.decision, traceCopy, true
}

// Len returns the current number of recorded decisions in the ring buffer.
// Test-only helper.
func (s *RoutingTraceStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ring)
}

// GetConfig builds the wire-shape RoutingConfig for the
// /api/daemon/routing/config endpoint. It composes the static portions
// (weights, capability filters, sandbox/LLM provider state) with the
// rolling RecentDecisions tail.
//
// The provider-state surfaces are seeded from the runner.Registry's
// Names() (passed in via providerNames) — this represents AgentRuntime
// providers. The sandbox state lists only "local" because that's the
// only OSS-shipped sandbox in this wave. Both lists default to
// Thompson-Sampling priors (alpha=1, beta=1) when no decisions have
// been recorded.
//
// capturedAt sets the snapshot timestamp; pass time.Now().UTC() in
// production.
func (s *RoutingTraceStore) GetConfig(providerNames []string, capturedAt time.Time) afclient.RoutingConfig {
	s.mu.RLock()
	recent := s.recentDecisions()
	s.mu.RUnlock()

	llmProviders := buildLLMProviderState(providerNames, recent)
	sandboxProviders := buildSandboxProviderState(recent)

	return afclient.RoutingConfig{
		CapabilityFilters: defaultCapabilityFilters(),
		Weights:           DefaultRoutingWeights,
		SandboxProviders:  sandboxProviders,
		LLMProviders:      llmProviders,
		RecentDecisions:   recent,
		CapturedAt:        capturedAt,
	}
}

// defaultCapabilityFilters returns the static set of hard filters the
// scheduler enforces in this wave. The values come from the local host's
// runtime profile (only `local` ships in OSS), so they're a reasonable
// always-on baseline until per-tenant configuration ships. Listed in a
// deterministic order so consumers see stable output.
func defaultCapabilityFilters() []afclient.CapabilityFilter {
	return []afclient.CapabilityFilter{}
}

// buildLLMProviderState projects the runner.Registry's AgentRuntime names
// into the wire-shape LLMProviderState slice. When recent decisions
// reference a name, its SelectionCount reflects that. Names are sorted
// for determinism.
func buildLLMProviderState(names []string, recent []afclient.RoutingDecision) []afclient.LLMProviderState {
	if len(names) == 0 {
		return []afclient.LLMProviderState{}
	}
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	counts := make(map[string]int, len(sorted))
	for _, d := range recent {
		if d.ChosenLLM != "" {
			counts[d.ChosenLLM]++
		}
	}

	out := make([]afclient.LLMProviderState, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, afclient.LLMProviderState{
			ProviderID:     n,
			Alpha:          1.0,
			Beta:           1.0,
			SelectionCount: counts[n],
		})
	}
	return out
}

// buildSandboxProviderState seeds the local sandbox row with Thompson
// priors. Other sandbox providers (Vercel/E2B/Modal/Daytona/Docker/K8s)
// don't ship in OSS this wave; surfaces stay empty per ADR D4. The local
// row's SelectionCount reflects observed decisions when the chosen
// sandbox matches "local".
func buildSandboxProviderState(recent []afclient.RoutingDecision) []afclient.SandboxProviderState {
	const localID = "local"
	count := 0
	for _, d := range recent {
		if d.ChosenSandbox == localID {
			count++
		}
	}
	return []afclient.SandboxProviderState{
		{
			ProviderID:     localID,
			Alpha:          1.0,
			Beta:           1.0,
			SelectionCount: count,
		},
	}
}
