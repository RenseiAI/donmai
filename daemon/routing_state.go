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
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/kit"
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
	// snapshotStatus is the cached ruleset-snapshot status AT RECORD TIME,
	// nil when no snapshot source was configured when this decision was
	// recorded. Historical — unlike GetConfig's ambient
	// RulesetSnapshot field (always current), this is frozen at
	// RecordDecisionWithSnapshot time so `routing explain <session>` shows
	// what the decision was actually evaluated against.
	snapshotStatus *afclient.RulesetSnapshotStatus
}

// RoutingTraceStore is the in-process record of routing decisions. The
// scheduler (or, in this wave, the test harness) feeds it via
// RecordDecision; HTTP handlers read via GetConfig and Explain.
//
// The store is safe for concurrent use.
type RoutingTraceStore struct {
	mu        sync.RWMutex
	ringSize  int
	ring      []recordedDecision // chronological, oldest first; len ≤ ringSize
	bySession map[string]recordedDecision
	// demand is the active kit composition's pre-placement toolchain
	// demand (K1.4 — see kit_demand.go), set by SetDemand once the
	// session's kit set is known and BEFORE placement. Zero value
	// (kit.PlacementDemand{}) until SetDemand is called: GetConfig then
	// reports no capability filters, identical to every session before
	// this signal existed.
	demand kit.PlacementDemand
	// snapshotStatusFn, when set via SetSnapshotStatusFunc, reports the
	// daemon's CURRENT cached ruleset-snapshot staleness — read fresh on
	// every GetConfig call (never cached here) so Age reflects
	// "now", exactly like an HTTP Age response header. nil (the default)
	// means no snapshot source is configured; GetConfig then reports no
	// RulesetSnapshot field, identical to every daemon before this signal
	// existed.
	snapshotStatusFn func() (afclient.RulesetSnapshotStatus, bool)
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
	s.recordDecision(decision, trace, nil)
}

// RecordDecisionWithSnapshot is RecordDecision plus the cached
// ruleset-snapshot status in effect when this decision was made — nil when
// no snapshot source is configured. The claim-gate path
// (daemon.FailStaticClaimGateProvider, via evaluateNarrowOnlyClaim) calls
// this so `routing explain <sessionId>` shows the rev/age/degraded state a
// fail-static claim decision was actually evaluated against, not just the
// daemon's current ambient state (which GetConfig already surfaces).
func (s *RoutingTraceStore) RecordDecisionWithSnapshot(decision afclient.RoutingDecision, trace []afclient.RoutingTraceStep, status *afclient.RulesetSnapshotStatus) {
	s.recordDecision(decision, trace, status)
}

func (s *RoutingTraceStore) recordDecision(decision afclient.RoutingDecision, trace []afclient.RoutingTraceStep, status *afclient.RulesetSnapshotStatus) {
	// Defensive copy: callers may continue to mutate trace after recording.
	traceCopy := make([]afclient.RoutingTraceStep, len(trace))
	copy(traceCopy, trace)
	rec := recordedDecision{decision: decision, trace: traceCopy, snapshotStatus: status}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A session can be recorded through two independent call sites — the
	// pre-existing OSS routing-decision recorder (RecordDecision, no
	// snapshot status) and the claim-gate path (RecordDecisionWithSnapshot)
	// — and either may fire second for the same SessionID. Whichever fires
	// second must not silently ERASE a
	// snapshot status the other already recorded: a caller that has no
	// opinion about the snapshot (status == nil) inherits whatever this
	// session's most recent recorded status already was, rather than
	// clobbering it to nil.
	if status == nil && decision.SessionID != "" {
		if existing, ok := s.bySession[decision.SessionID]; ok && existing.snapshotStatus != nil {
			inherited := *existing.snapshotStatus
			rec.snapshotStatus = &inherited
		}
	}
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
	decision, trace, _, ok := s.ExplainWithSnapshot(sessionID)
	return decision, trace, ok
}

// ExplainWithSnapshot is Explain plus the ruleset-snapshot status recorded
// alongside the decision, if any — see RecordDecisionWithSnapshot.
func (s *RoutingTraceStore) ExplainWithSnapshot(sessionID string) (afclient.RoutingDecision, []afclient.RoutingTraceStep, *afclient.RulesetSnapshotStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.bySession[sessionID]
	if !ok {
		return afclient.RoutingDecision{}, nil, nil, false
	}
	// Defensive copy of trace so the caller cannot mutate the stored slice.
	traceCopy := make([]afclient.RoutingTraceStep, len(rec.trace))
	copy(traceCopy, rec.trace)
	var status *afclient.RulesetSnapshotStatus
	if rec.snapshotStatus != nil {
		copied := *rec.snapshotStatus
		status = &copied
	}
	return rec.decision, traceCopy, status, true
}

// SetSnapshotStatusFunc wires the daemon's ambient cached-ruleset-snapshot
// status source — typically a *rulesetsnapshot.Client's Current() adapted
// to the wire shape. GetConfig calls fn fresh on every read (never
// memoized here) so the reported Age always reflects "now". Passing nil
// (the default) omits RulesetSnapshot entirely, identical to every daemon
// before this signal existed.
func (s *RoutingTraceStore) SetSnapshotStatusFunc(fn func() (afclient.RulesetSnapshotStatus, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotStatusFn = fn
}

// Len returns the current number of recorded decisions in the ring buffer.
// Test-only helper.
func (s *RoutingTraceStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ring)
}

// SetDemand records the active kit composition's pre-placement toolchain
// demand (KitRegistry.DemandForRepo). The composition surface calls this
// once the session's kit set is known and BEFORE placement; GetConfig then
// reports it as capability filters (demandCapabilityFilters) and
// FilterCandidatesByDemand uses it as the local scheduler's stage-2
// viability hard filter (004 "Routing algorithm" step 1; ADR-2026-08-12
// D1.2).
func (s *RoutingTraceStore) SetDemand(demand kit.PlacementDemand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demand = demand
}

// Demand returns the currently active toolchain demand. The zero value
// (kit.PlacementDemand{}) means no composition has called SetDemand yet.
func (s *RoutingTraceStore) Demand() kit.PlacementDemand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.demand
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
	demand := s.demand
	snapshotFn := s.snapshotStatusFn
	s.mu.RUnlock()

	llmProviders := buildLLMProviderState(providerNames, recent)
	sandboxProviders := buildSandboxProviderState(recent)

	var snapshotStatus *afclient.RulesetSnapshotStatus
	if snapshotFn != nil {
		if status, ok := snapshotFn(); ok {
			snapshotStatus = &status
		}
	}

	return afclient.RoutingConfig{
		CapabilityFilters: demandCapabilityFilters(demand),
		Weights:           DefaultRoutingWeights,
		SandboxProviders:  sandboxProviders,
		LLMProviders:      llmProviders,
		RecentDecisions:   recent,
		CapturedAt:        capturedAt,
		RulesetSnapshot:   snapshotStatus,
	}
}

// demandCapabilityFilters projects the active kit toolchain demand into the
// wire CapabilityFilter list the /api/daemon/routing/config surface
// reports (004 "Routing algorithm" step 1: "os and arch match
// capabilities.os / capabilities.arch (both required)"). A demand that was
// never set (SetDemand not called — the zero value) or that doesn't narrow
// either axis reports no filters: additive, identical output to every
// session before this signal existed.
func demandCapabilityFilters(demand kit.PlacementDemand) []afclient.CapabilityFilter {
	filters := []afclient.CapabilityFilter{}
	if demand.NarrowsOS() {
		filters = append(filters, afclient.CapabilityFilter{
			Field: "os", Op: "in", Value: strings.Join(demand.OS, ","),
		})
	}
	if demand.NarrowsArch() {
		filters = append(filters, afclient.CapabilityFilter{
			Field: "arch", Op: "in", Value: strings.Join(demand.Arch, ","),
		})
	}
	return filters
}

// RoutingCandidate is a minimal, daemon-local view of one local-scheduler
// routing candidate's declared platform support — deliberately NOT the
// execution-cell / placement-ref wire types under active design elsewhere
// in this codebase. Once that work lands, RoutingCandidate's OS/Arch
// declaration should be unified with the cell contract's declared
// execution-host capabilities (the "execution-host capabilities (os, arch,
// declared capability flags)" tuple slot in ADR-2026-08-12 D1.2) rather
// than kept as a second, parallel shape — noted here for the coordinator.
type RoutingCandidate struct {
	// ProviderID names the candidate (e.g. a worker fleet member or pool).
	ProviderID string
	// OS is the candidate's declared supported OS set. Empty means the
	// candidate declares no OS restriction (matches the manifest-side
	// permissive-empty convention) and always survives an OS filter.
	OS []string
	// Arch is the analogous architecture set.
	Arch []string
}

// ExcludedCandidate names a RoutingCandidate the capability-filter phase
// removed and the reason, mirroring the ADR-2026-08-12 D6 decision-record
// shape ("per-candidate exclusion: stage + named rule").
type ExcludedCandidate struct {
	ProviderID string
	Reason     string
}

// FilterCandidatesByDemand is the local scheduler's capability-filter phase
// (004 "Routing algorithm" step 1 — the projection of ADR-2026-08-12's
// stage-2 viability filter onto this daemon's own candidate set). A
// candidate survives only if its declared OS and Arch intersect the
// demand's effective OS/Arch for the engaged lanes (kit.PlacementDemand.
// EffectiveOS / EffectiveArch) — a demand for os=macos filters out every
// candidate that declares only linux.
//
// Additive: a demand that was never derived (the zero value —
// demand.OS/Arch are nil) filters nothing; every candidate survives
// unchanged. A genuinely unsatisfiable demand (PlacementDemand.
// IsUnsatisfiable — no OS common to the composing kits) excludes every
// candidate, per ADR-2026-08-12 D1.2 "∅ is always loud": the caller must
// surface that as a typed, named exclusion, never a silent empty result.
func FilterCandidatesByDemand(candidates []RoutingCandidate, demand kit.PlacementDemand, engagedLanes ...string) (survivors []RoutingCandidate, excluded []ExcludedCandidate) {
	demandOS := demand.EffectiveOS(engagedLanes...)
	demandArch := demand.EffectiveArch(engagedLanes...)

	for _, c := range candidates {
		if reason, ok := candidateExclusionReason(c, demandOS, demandArch); !ok {
			excluded = append(excluded, ExcludedCandidate{ProviderID: c.ProviderID, Reason: reason})
			continue
		}
		survivors = append(survivors, c)
	}
	return survivors, excluded
}

// candidateExclusionReason reports whether c survives the demand (ok=true)
// or, when it does not, why (reason, ok=false).
func candidateExclusionReason(c RoutingCandidate, demandOS, demandArch []string) (reason string, ok bool) {
	if demandOS != nil && !platformIntersects(c.OS, demandOS) {
		return "stage2 viability: candidate os " + strings.Join(c.OS, ",") +
			" does not intersect demand os " + strings.Join(demandOS, ","), false
	}
	if demandArch != nil && !platformIntersects(c.Arch, demandArch) {
		return "stage2 viability: candidate arch " + strings.Join(c.Arch, ",") +
			" does not intersect demand arch " + strings.Join(demandArch, ","), false
	}
	return "", true
}

// platformIntersects reports whether candidateValues (a candidate's
// declared OS or Arch set) satisfies demandValues. An empty candidate set
// means "no restriction declared" and always survives — the same
// permissive-empty convention the kit manifest side uses. A non-empty
// demandValues that is itself empty (PlacementDemand.IsUnsatisfiable) never
// intersects anything, so every restricted candidate is excluded.
func platformIntersects(candidateValues, demandValues []string) bool {
	if len(candidateValues) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(demandValues))
	for _, v := range demandValues {
		set[v] = struct{}{}
	}
	for _, v := range candidateValues {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
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
