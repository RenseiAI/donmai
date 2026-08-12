package daemon

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/kit"
)

// makeDecision builds a RoutingDecision with deterministic timestamps so
// ring-buffer eviction tests can assert ordering.
func makeDecision(sessionID, sandbox, llm string, decidedAt time.Time) afclient.RoutingDecision {
	return afclient.RoutingDecision{
		SessionID:     sessionID,
		ChosenSandbox: sandbox,
		ChosenLLM:     llm,
		Score:         0.5,
		DecidedAt:     decidedAt,
	}
}

func makeTrace(steps int) []afclient.RoutingTraceStep {
	out := make([]afclient.RoutingTraceStep, steps)
	for i := 0; i < steps; i++ {
		out[i] = afclient.RoutingTraceStep{
			Step:      i + 1,
			Phase:     "score",
			Dimension: "sandbox",
			Remaining: []string{"local"},
		}
	}
	return out
}

func TestNewRoutingTraceStore_DefaultRingSize(t *testing.T) {
	t.Parallel()
	for _, ringSize := range []int{0, -1, -100} {
		s := NewRoutingTraceStore(ringSize)
		if s.ringSize != DefaultRoutingRingBufferSize {
			t.Errorf("NewRoutingTraceStore(%d).ringSize = %d, want %d",
				ringSize, s.ringSize, DefaultRoutingRingBufferSize)
		}
	}
}

func TestRoutingTraceStore_RecordAndExplain(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(10)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	d := makeDecision("sess-1", "local", "claude", now)
	trace := makeTrace(3)
	s.RecordDecision(d, trace)

	gotDecision, gotTrace, ok := s.Explain("sess-1")
	if !ok {
		t.Fatalf("Explain(sess-1) ok=false, want true")
	}
	if gotDecision.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", gotDecision.SessionID)
	}
	if gotDecision.ChosenSandbox != "local" {
		t.Errorf("ChosenSandbox = %q, want local", gotDecision.ChosenSandbox)
	}
	if len(gotTrace) != 3 {
		t.Errorf("len(trace) = %d, want 3", len(gotTrace))
	}

	// Defensive copy: mutating the returned trace must not affect store.
	gotTrace[0].Phase = "mutated"
	_, again, _ := s.Explain("sess-1")
	if again[0].Phase != "score" {
		t.Errorf("internal trace mutated: phase = %q, want score", again[0].Phase)
	}
}

func TestRoutingTraceStore_Explain_NotFound(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(10)
	if _, _, ok := s.Explain("missing"); ok {
		t.Errorf("Explain(missing) ok=true, want false on empty store")
	}
	s.RecordDecision(makeDecision("sess-1", "local", "claude", time.Now()), nil)
	if _, _, ok := s.Explain("sess-2"); ok {
		t.Errorf("Explain(sess-2) ok=true, want false")
	}
}

func TestRoutingTraceStore_RingEviction(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(3)
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		s.RecordDecision(
			makeDecision("sess-"+string(rune('a'+i)), "local", "claude", base.Add(time.Duration(i)*time.Minute)),
			nil,
		)
	}

	if got := s.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}

	// Two oldest sessions ("sess-a", "sess-b") evicted.
	for _, id := range []string{"sess-a", "sess-b"} {
		if _, _, ok := s.Explain(id); ok {
			t.Errorf("Explain(%s) ok=true, want false (evicted)", id)
		}
	}
	for _, id := range []string{"sess-c", "sess-d", "sess-e"} {
		if _, _, ok := s.Explain(id); !ok {
			t.Errorf("Explain(%s) ok=false, want true (still in ring)", id)
		}
	}
}

func TestRoutingTraceStore_RingEviction_PreservesNewerSessionEntry(t *testing.T) {
	t.Parallel()
	// When the same SessionID is recorded twice, the newer entry must
	// not be dropped just because the older one ages out of the ring.
	s := NewRoutingTraceStore(2)
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	s.RecordDecision(makeDecision("sess-x", "local", "claude", base), nil)                   // ring[0]
	s.RecordDecision(makeDecision("sess-y", "local", "claude", base.Add(time.Minute)), nil)  // ring[1]
	s.RecordDecision(makeDecision("sess-x", "local", "codex", base.Add(2*time.Minute)), nil) // evicts ring[0] (old sess-x)

	// sess-x should still be retrievable — the newer record overwrites
	// bySession before the older one is evicted from the ring.
	got, _, ok := s.Explain("sess-x")
	if !ok {
		t.Fatalf("Explain(sess-x) ok=false, want true")
	}
	if got.ChosenLLM != "codex" {
		t.Errorf("Explain(sess-x).ChosenLLM = %q, want codex (newer record)", got.ChosenLLM)
	}
}

func TestRoutingTraceStore_RecordWithEmptySessionID(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(5)
	s.RecordDecision(makeDecision("", "local", "claude", time.Now()), nil)
	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 (unkeyed entry still in ring)", got)
	}
	if _, _, ok := s.Explain(""); ok {
		t.Errorf("Explain(\"\") ok=true, want false")
	}
}

func TestRoutingTraceStore_GetConfig_Empty(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(10)
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	cfg := s.GetConfig(nil, now)

	if cfg.Weights != DefaultRoutingWeights {
		t.Errorf("Weights = %+v, want %+v", cfg.Weights, DefaultRoutingWeights)
	}
	if cfg.Weights.Cost != 0.7 || cfg.Weights.Latency != 0.3 {
		t.Errorf("default weights = {Cost:%v Latency:%v}, want {Cost:0.7 Latency:0.3}", cfg.Weights.Cost, cfg.Weights.Latency)
	}
	if !cfg.CapturedAt.Equal(now) {
		t.Errorf("CapturedAt = %v, want %v", cfg.CapturedAt, now)
	}
	if got := len(cfg.RecentDecisions); got != 0 {
		t.Errorf("len(RecentDecisions) = %d, want 0", got)
	}
	if len(cfg.LLMProviders) != 0 {
		t.Errorf("len(LLMProviders) = %d, want 0", len(cfg.LLMProviders))
	}
	// Sandbox always seeds with the local row.
	if got := len(cfg.SandboxProviders); got != 1 {
		t.Fatalf("len(SandboxProviders) = %d, want 1", got)
	}
	if cfg.SandboxProviders[0].ProviderID != "local" {
		t.Errorf("SandboxProviders[0].ProviderID = %q, want local", cfg.SandboxProviders[0].ProviderID)
	}
	if cfg.SandboxProviders[0].Alpha != 1.0 || cfg.SandboxProviders[0].Beta != 1.0 {
		t.Errorf("local sandbox priors = (alpha=%v, beta=%v), want (1, 1)",
			cfg.SandboxProviders[0].Alpha, cfg.SandboxProviders[0].Beta)
	}
}

func TestRoutingTraceStore_GetConfig_ProvidersAndCounts(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(10)
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	s.RecordDecision(makeDecision("sess-1", "local", "claude", base), nil)
	s.RecordDecision(makeDecision("sess-2", "local", "codex", base.Add(time.Minute)), nil)
	s.RecordDecision(makeDecision("sess-3", "vercel", "claude", base.Add(2*time.Minute)), nil)

	cfg := s.GetConfig([]string{"codex", "claude", "stub"}, base.Add(3*time.Minute))

	// LLM providers: sorted by ID, SelectionCount counts decisions.
	if len(cfg.LLMProviders) != 3 {
		t.Fatalf("len(LLMProviders) = %d, want 3", len(cfg.LLMProviders))
	}
	wantOrder := []string{"claude", "codex", "stub"}
	for i, p := range cfg.LLMProviders {
		if p.ProviderID != wantOrder[i] {
			t.Errorf("LLMProviders[%d].ProviderID = %q, want %q", i, p.ProviderID, wantOrder[i])
		}
	}
	wantCount := map[string]int{"claude": 2, "codex": 1, "stub": 0}
	for _, p := range cfg.LLMProviders {
		if got := p.SelectionCount; got != wantCount[p.ProviderID] {
			t.Errorf("LLMProviders[%s].SelectionCount = %d, want %d",
				p.ProviderID, got, wantCount[p.ProviderID])
		}
	}

	// Local sandbox SelectionCount must reflect "local"-chosen decisions only.
	if cfg.SandboxProviders[0].SelectionCount != 2 {
		t.Errorf("local SelectionCount = %d, want 2", cfg.SandboxProviders[0].SelectionCount)
	}

	// RecentDecisions in chronological order.
	if len(cfg.RecentDecisions) != 3 {
		t.Fatalf("len(RecentDecisions) = %d, want 3", len(cfg.RecentDecisions))
	}
	for i, want := range []string{"sess-1", "sess-2", "sess-3"} {
		if cfg.RecentDecisions[i].SessionID != want {
			t.Errorf("RecentDecisions[%d].SessionID = %q, want %q",
				i, cfg.RecentDecisions[i].SessionID, want)
		}
	}
}

func TestRoutingTraceStore_ConcurrentRecordExplain(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(100)
	const goroutines = 8
	const writes = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			base := time.Now()
			for i := 0; i < writes; i++ {
				id := fmt.Sprintf("sess-g%d-%d", g, i%10)
				s.RecordDecision(
					makeDecision(id, "local", "claude", base.Add(time.Duration(i)*time.Microsecond)),
					nil,
				)
				_ = s.Len()
				_, _, _ = s.Explain("sess-foo")
			}
		}(g)
	}
	wg.Wait()

	// Just assert no panic and bounded ring.
	if got := s.Len(); got > 100 {
		t.Errorf("Len() = %d, want ≤ 100", got)
	}
}

// swiftDemandWithIOSLane mirrors what KitRegistry.DemandForRepo derives for
// a repo whose only matching kit is the swift foundation kit (supports
// linux+macos) declaring the iOS-app-build lane (macos only) — the
// swift-kit pattern named in 005-kit-manifest-spec.md § "Platform
// compatibility".
func swiftDemandWithIOSLane() kit.PlacementDemand {
	return kit.DeriveDemand([]kit.ManifestView{
		{
			ID:          "default/swift",
			SupportedOS: []string{kit.OSLinux, kit.OSMacOS},
			Lanes: []kit.LaneView{
				{Name: "ios-app-build", OS: []string{kit.OSMacOS}},
			},
		},
	})
}

func TestRoutingTraceStore_SetDemand_ReflectedInCapabilityFilters(t *testing.T) {
	t.Parallel()
	s := NewRoutingTraceStore(10)

	// Before SetDemand: no filters (identical to every session before this
	// signal existed).
	cfg := s.GetConfig(nil, time.Now())
	if len(cfg.CapabilityFilters) != 0 {
		t.Fatalf("CapabilityFilters before SetDemand = %+v, want none", cfg.CapabilityFilters)
	}

	s.SetDemand(kit.PlacementDemand{OS: []string{kit.OSMacOS}, Arch: []string{kit.ArchARM64, kit.ArchX86_64}})
	cfg = s.GetConfig(nil, time.Now())
	if len(cfg.CapabilityFilters) != 1 {
		t.Fatalf("CapabilityFilters after narrowed SetDemand = %+v, want 1 (os only; arch unconstrained)", cfg.CapabilityFilters)
	}
	want := afclient.CapabilityFilter{Field: "os", Op: "in", Value: "macos"}
	if cfg.CapabilityFilters[0] != want {
		t.Errorf("CapabilityFilters[0] = %+v, want %+v", cfg.CapabilityFilters[0], want)
	}

	if got := s.Demand(); !reflect.DeepEqual(got.OS, []string{kit.OSMacOS}) {
		t.Errorf("Demand().OS = %v, want [macos]", got.OS)
	}
}

// TestFilterCandidatesByDemand_MacOSLockedLaneExcludesLinuxProvider is the
// acceptance test named in the issue: a composition including a
// macOS-locked lane (the swift-kit iOS-app-build pattern) yields a demand
// the local scheduler honors by filtering out a Linux-only provider.
func TestFilterCandidatesByDemand_MacOSLockedLaneExcludesLinuxProvider(t *testing.T) {
	t.Parallel()
	demand := swiftDemandWithIOSLane()

	candidates := []RoutingCandidate{
		{ProviderID: "mac-pool", OS: []string{kit.OSMacOS}},
		{ProviderID: "linux-pool", OS: []string{kit.OSLinux}},
	}

	survivors, excluded := FilterCandidatesByDemand(candidates, demand, "ios-app-build")

	if len(survivors) != 1 || survivors[0].ProviderID != "mac-pool" {
		t.Fatalf("survivors = %+v, want [mac-pool]", survivors)
	}
	if len(excluded) != 1 || excluded[0].ProviderID != "linux-pool" {
		t.Fatalf("excluded = %+v, want [linux-pool]", excluded)
	}
	if excluded[0].Reason == "" {
		t.Error("excluded[0].Reason is empty, want a named stage-2 exclusion rule")
	}
}

func TestFilterCandidatesByDemand(t *testing.T) {
	t.Parallel()

	macCandidate := RoutingCandidate{ProviderID: "mac-pool", OS: []string{kit.OSMacOS}}
	linuxCandidate := RoutingCandidate{ProviderID: "linux-pool", OS: []string{kit.OSLinux}}
	hostAgnosticCandidate := RoutingCandidate{ProviderID: "any-pool"} // declares no OS restriction

	tests := []struct {
		name          string
		candidates    []RoutingCandidate
		demand        kit.PlacementDemand
		engagedLanes  []string
		wantSurvivors []string
		wantExcluded  []string
	}{
		{
			name:          "zero-value demand (never derived) filters nothing",
			candidates:    []RoutingCandidate{macCandidate, linuxCandidate},
			demand:        kit.PlacementDemand{},
			wantSurvivors: []string{"mac-pool", "linux-pool"},
		},
		{
			name:          "unconstrained demand (full known universe) filters nothing",
			candidates:    []RoutingCandidate{macCandidate, linuxCandidate},
			demand:        kit.DeriveDemand(nil),
			wantSurvivors: []string{"mac-pool", "linux-pool"},
		},
		{
			name:          "macos-locked lane engaged excludes the linux-only candidate",
			candidates:    []RoutingCandidate{macCandidate, linuxCandidate},
			demand:        swiftDemandWithIOSLane(),
			engagedLanes:  []string{"ios-app-build"},
			wantSurvivors: []string{"mac-pool"},
			wantExcluded:  []string{"linux-pool"},
		},
		{
			name:          "macos-locked lane NOT engaged: composition-wide demand stays broad",
			candidates:    []RoutingCandidate{macCandidate, linuxCandidate},
			demand:        swiftDemandWithIOSLane(),
			wantSurvivors: []string{"mac-pool", "linux-pool"},
		},
		{
			name:          "host-agnostic candidate always survives an OS filter",
			candidates:    []RoutingCandidate{hostAgnosticCandidate, linuxCandidate},
			demand:        swiftDemandWithIOSLane(),
			engagedLanes:  []string{"ios-app-build"},
			wantSurvivors: []string{"any-pool"},
			wantExcluded:  []string{"linux-pool"},
		},
		{
			name:          "unsatisfiable demand excludes every restricted candidate",
			candidates:    []RoutingCandidate{macCandidate, linuxCandidate, hostAgnosticCandidate},
			demand:        kit.PlacementDemand{OS: []string{}},
			wantSurvivors: []string{"any-pool"},
			wantExcluded:  []string{"mac-pool", "linux-pool"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			survivors, excluded := FilterCandidatesByDemand(tt.candidates, tt.demand, tt.engagedLanes...)

			var gotSurvivors []string
			for _, c := range survivors {
				gotSurvivors = append(gotSurvivors, c.ProviderID)
			}
			if !reflect.DeepEqual(gotSurvivors, orNilIfEmpty(tt.wantSurvivors)) {
				t.Errorf("survivors = %v, want %v", gotSurvivors, tt.wantSurvivors)
			}

			var gotExcluded []string
			for _, c := range excluded {
				gotExcluded = append(gotExcluded, c.ProviderID)
			}
			if !reflect.DeepEqual(gotExcluded, orNilIfEmpty(tt.wantExcluded)) {
				t.Errorf("excluded = %v, want %v", gotExcluded, tt.wantExcluded)
			}
		})
	}
}

// orNilIfEmpty normalizes an empty-but-non-nil want slice to nil so it
// compares equal to the nil built up by a for-range append loop that never
// executes (survivors or excluded is empty).
func orNilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestDemandCapabilityFilters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		demand kit.PlacementDemand
		want   []afclient.CapabilityFilter
	}{
		{
			name:   "zero value: no filters",
			demand: kit.PlacementDemand{},
			want:   []afclient.CapabilityFilter{},
		},
		{
			name:   "unconstrained (full universe): no filters",
			demand: kit.DeriveDemand(nil),
			want:   []afclient.CapabilityFilter{},
		},
		{
			name:   "os narrowed only",
			demand: kit.PlacementDemand{OS: []string{kit.OSMacOS}, Arch: []string{kit.ArchARM64, kit.ArchX86_64}},
			want:   []afclient.CapabilityFilter{{Field: "os", Op: "in", Value: "macos"}},
		},
		{
			name:   "both narrowed",
			demand: kit.PlacementDemand{OS: []string{kit.OSMacOS}, Arch: []string{kit.ArchARM64}},
			want: []afclient.CapabilityFilter{
				{Field: "os", Op: "in", Value: "macos"},
				{Field: "arch", Op: "in", Value: "arm64"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := demandCapabilityFilters(tt.demand)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("demandCapabilityFilters(%+v) = %+v, want %+v", tt.demand, got, tt.want)
			}
		})
	}
}
