package codeintel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seedBaseline contributes one pattern + one convention to the store so the full
// pipeline has a baseline to compare a change against.
func seedBaseline(t *testing.T, s *ArchStore) {
	t.Helper()
	if err := s.Contribute(ArchObservation{
		Kind: "pattern",
		Payload: mustPayload(t, map[string]any{
			"title":       "Centralized auth middleware",
			"description": "All routes delegate auth to lib/auth/middleware.ts.",
			"locations":   []map[string]string{{"path": "lib/auth/middleware.ts"}},
			"tags":        []string{"auth"},
		}),
		Confidence: 0.8,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "seed"},
	}); err != nil {
		t.Fatalf("seed pattern: %v", err)
	}
	if err := s.Contribute(ArchObservation{
		Kind: "convention",
		Payload: mustPayload(t, map[string]any{
			"title":       "Result<T,E> error handling",
			"description": "Code uses Result<T, E> instead of throwing.",
			"examples":    []map[string]string{{"path": "src/x.ts"}},
		}),
		Confidence: 0.7,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "seed"},
	}); err != nil {
		t.Fatalf("seed convention: %v", err)
	}
}

func sampleDiff() PrDiff {
	return PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   123,
		Title:      "Add inline auth to new route",
		Body:       "New endpoint authenticates inline.",
		Files: []PrFileDiff{
			{Path: "src/api/new-route.ts", Patch: "+function handler() { /* inline auth */ }", Added: true},
		},
	}
}

func TestAssessFull_EmptyBaselineIsCleanNative(t *testing.T) {
	s := openTestStore(t)
	// No baseline seeded. The adapter must NOT be called (no baseline to compare).
	adapter := stubAdapter{t: t, mustNotCall: true}

	rep, err := assessFull(context.Background(), s, adapter, assessFullInput{
		diff:  sampleDiff(),
		scope: ArchScope{Level: "project"},
	})
	if err != nil {
		t.Fatalf("assessFull: %v", err)
	}
	if rep.Mode != "native" {
		t.Errorf("mode = %q, want native", rep.Mode)
	}
	if len(rep.Deviations) != 0 || rep.Gated {
		t.Errorf("empty baseline must yield a clean report, got %+v", rep)
	}
	if !strings.Contains(rep.Summary, "No established architectural baseline") {
		t.Errorf("summary = %q", rep.Summary)
	}
}

func TestAssessFull_MaterializesDeviationsAndReinforced(t *testing.T) {
	s := openTestStore(t)
	seedBaseline(t, s)

	// Drive the REAL lane (agent.Complete → SpawnComplete) via the fake harness,
	// emitting a high-severity deviation that names the seeded convention.
	h := archFakeHarness{events: emitJSON(
		`{"deviations":[{"observation":"Result<T,E> error handling","severity":"critical","rationale":"new route throws instead of returning Result","citation":"src/api/new-route.ts"}]}`,
	)}
	adapter := LaneAdapter{Harness: h}

	rep, err := assessFull(context.Background(), s, adapter, assessFullInput{
		diff:       sampleDiff(),
		scope:      ArchScope{Level: "project"},
		gatePolicy: "no-severity-high",
	})
	if err != nil {
		t.Fatalf("assessFull: %v", err)
	}
	if rep.Mode != "native" {
		t.Errorf("mode = %q, want native", rep.Mode)
	}
	if len(rep.Deviations) != 1 {
		t.Fatalf("got %d deviations, want 1: %+v", len(rep.Deviations), rep.Deviations)
	}
	d := rep.Deviations[0]
	if d.Severity != "high" {
		t.Errorf("severity = %q, want high (critical→high)", d.Severity)
	}
	if d.DeviatesFrom.Kind != "convention" || d.DeviatesFrom.ConventionID == "" {
		t.Errorf("deviatesFrom not resolved to the seeded convention: %+v", d.DeviatesFrom)
	}
	if d.IntroducedBy == nil || d.IntroducedBy.PrNumber != 123 {
		t.Errorf("introducedBy not wired: %+v", d.IntroducedBy)
	}
	if !rep.HasCriticalDrift {
		t.Errorf("HasCriticalDrift = false, want true")
	}
	if !rep.Gated {
		t.Errorf("no-severity-high + high deviation must gate")
	}

	// The pattern was NOT flagged → reinforced. The convention WAS flagged →
	// not reinforced.
	var reinforcedPattern, reinforcedConvention bool
	for _, r := range rep.Reinforced {
		if r.Kind == "pattern" && r.PatternID != "" {
			reinforcedPattern = true
		}
		if r.Kind == "convention" && r.ConventionID != "" {
			reinforcedConvention = true
		}
	}
	if !reinforcedPattern {
		t.Errorf("unflagged pattern should be reinforced: %+v", rep.Reinforced)
	}
	if reinforcedConvention {
		t.Errorf("flagged convention must NOT be reinforced: %+v", rep.Reinforced)
	}

	// The deviation must be PERSISTED as a node in the store.
	devs, err := s.QueryDeviations("project")
	if err != nil {
		t.Fatalf("QueryDeviations: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("expected 1 persisted deviation, got %d", len(devs))
	}
	if devs[0].Severity != "high" {
		t.Errorf("persisted severity = %q, want high", devs[0].Severity)
	}
	if devs[0].IntroducedBy == nil || devs[0].IntroducedBy.PrNumber != 123 {
		t.Errorf("persisted introducedBy not wired: %+v", devs[0].IntroducedBy)
	}
}

func TestAssessFull_SoftMissYieldsCleanReport(t *testing.T) {
	s := openTestStore(t)
	seedBaseline(t, s)

	// Model emits prose, not schema-valid JSON → SchemaOK false → no deviations.
	h := archFakeHarness{events: emitJSON("I am not sure there are any deviations.")}
	rep, err := assessFull(context.Background(), s, LaneAdapter{Harness: h}, assessFullInput{
		diff:       sampleDiff(),
		scope:      ArchScope{Level: "project"},
		gatePolicy: "no-severity-high",
	})
	if err != nil {
		t.Fatalf("soft miss must not error: %v", err)
	}
	if len(rep.Deviations) != 0 || rep.Gated {
		t.Errorf("soft miss must yield a clean report, got %+v", rep)
	}
	if !strings.Contains(rep.Summary, "No architectural deviations detected") {
		t.Errorf("summary = %q", rep.Summary)
	}
	// Nothing persisted.
	if devs, _ := s.QueryDeviations("project"); len(devs) != 0 {
		t.Errorf("soft miss must persist no deviations, got %d", len(devs))
	}
}

func TestAssessFull_AdapterErrorPropagates(t *testing.T) {
	s := openTestStore(t)
	seedBaseline(t, s)

	_, err := assessFull(context.Background(), s, stubAdapter{err: errors.New("boom")}, assessFullInput{
		diff:  sampleDiff(),
		scope: ArchScope{Level: "project"},
	})
	if err == nil {
		t.Fatal("expected adapter error to propagate")
	}
}

func TestAssessFull_NilAdapterViaRunner(t *testing.T) {
	r := New(t.TempDir())
	if _, err := r.ArchAssessFull(context.Background(), nil, sampleDiff(), ArchScope{Level: "project"}, "", ""); err == nil {
		t.Fatal("expected error for nil adapter")
	}
}

func TestArchAssessFull_RunnerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/db.sqlite"
	// Seed a baseline into the same DB path the Runner will open.
	s, err := OpenArchStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedBaseline(t, s)
	_ = s.Close()

	h := archFakeHarness{events: emitJSON(
		`{"deviations":[{"observation":"Centralized auth middleware","severity":"warning","rationale":"new route does auth inline"}]}`,
	)}
	r := New(dir)
	rep, err := r.ArchAssessFull(context.Background(), LaneAdapter{Harness: h}, sampleDiff(),
		ArchScope{Level: "project"}, "zero-deviations", dbPath)
	if err != nil {
		t.Fatalf("ArchAssessFull: %v", err)
	}
	if len(rep.Deviations) != 1 || rep.Deviations[0].Severity != "medium" {
		t.Fatalf("warning→medium expected, got %+v", rep.Deviations)
	}
	if !rep.Gated {
		t.Errorf("zero-deviations policy + 1 deviation must gate")
	}
	if rep.HasCriticalDrift {
		t.Errorf("warning-only must not be critical drift")
	}
}

// TestArchAssessNative_FullPathSelectsNativeMode proves the Runner routes to the
// full lane-backed pipeline (mode "native") when an adapter is injected, and that
// the diff fetched via gh feeds the assessment.
func TestArchAssessNative_FullPathSelectsNativeMode(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })
	runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`{"title":"Inline auth route","body":"","files":[{"path":"src/api/x.ts","additions":3,"deletions":0}]}`), nil
	}
	runGhPRDiff = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("diff --git a/src/api/x.ts b/src/api/x.ts\n@@ -1 +1,2 @@\n+inline auth here\n"), nil
	}

	dir := t.TempDir()
	dbPath := dir + "/db.sqlite"
	s, err := OpenArchStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedBaseline(t, s)
	_ = s.Close()

	h := archFakeHarness{events: emitJSON(
		`{"deviations":[{"observation":"Centralized auth middleware","severity":"critical","rationale":"inline auth"}]}`,
	)}
	r := New(dir).WithArchAdapter(LaneAdapter{Harness: h})

	out, err := r.archAssessNative(ArchAssessOptions{
		PrURL:      "https://github.com/org/repo/pull/123",
		ScopeLevel: "project",
		GatePolicy: "no-severity-high",
		DB:         dbPath,
	})
	if err != nil {
		t.Fatalf("archAssessNative: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output not a map: %T", out)
	}
	if m["mode"] != "native" {
		t.Errorf("mode = %v, want native", m["mode"])
	}
	if m["gated"] != true {
		t.Errorf("expected gated=true, got %v", m["gated"])
	}
	devs, _ := m["deviations"].([]any)
	if len(devs) != 1 {
		t.Fatalf("expected 1 deviation in output, got %v", m["deviations"])
	}
}

// TestArchAssessNative_DiffOnlyWithoutAdapter proves that without an injected
// adapter the Runner stays on the diff-only path but still runs on the REAL
// fetched diff (mode "native-diff-only", observations present).
func TestArchAssessNative_DiffOnlyWithoutAdapter(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })
	runGhPRView = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`{"title":"t","body":"","files":[{"path":"src/auth/login.ts","additions":2,"deletions":0}]}`), nil
	}
	runGhPRDiff = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("diff --git a/src/auth/login.ts b/src/auth/login.ts\n@@ -1 +1,2 @@\n+const x = 1\n"), nil
	}

	r := New(t.TempDir()) // no adapter
	out, err := r.archAssessNative(ArchAssessOptions{
		PrURL:      "https://github.com/org/repo/pull/7",
		ScopeLevel: "project",
	})
	if err != nil {
		t.Fatalf("archAssessNative: %v", err)
	}
	m := out.(map[string]any)
	if m["mode"] != "native-diff-only" {
		t.Errorf("mode = %v, want native-diff-only", m["mode"])
	}
	// The fetched auth-zone file must have produced at least one observation.
	obs, _ := m["observations"].([]any)
	if len(obs) == 0 {
		t.Errorf("expected observations from the fetched diff, got none")
	}
}

func TestEvaluateGateNodes(t *testing.T) {
	high := []ReportDeviation{{Severity: "high"}}
	mixed := []ReportDeviation{{Severity: "low"}, {Severity: "medium"}}
	tests := []struct {
		name   string
		devs   []ReportDeviation
		policy string
		want   bool
	}{
		{"none never gates", high, "none", false},
		{"zero-deviations gates on any", mixed, "zero-deviations", true},
		{"zero-deviations passes empty", nil, "zero-deviations", false},
		{"no-severity-high gates on high", high, "no-severity-high", true},
		{"no-severity-high passes no-high", mixed, "no-severity-high", false},
		{"default == no-severity-high", high, "", true},
		{"max:1 gates on 2", mixed, "max:1", true},
		{"max:2 passes 2", mixed, "max:2", false},
		{"bad max passes", mixed, "max:x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluateGateNodes(tt.devs, tt.policy); got != tt.want {
				t.Errorf("evaluateGateNodes(%v, %q) = %v, want %v", tt.devs, tt.policy, got, tt.want)
			}
		})
	}
}

func TestLaneSeverityToNode(t *testing.T) {
	cases := map[string]string{
		SeverityCritical: "high",
		SeverityWarning:  "medium",
		SeverityInfo:     "low",
		"high":           "high",
		"medium":         "medium",
		"low":            "low",
		"weird":          "medium",
		"":               "medium",
	}
	for in, want := range cases {
		if got := laneSeverityToNode(in); got != want {
			t.Errorf("laneSeverityToNode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveDeviatesFrom_FallsBackToPattern(t *testing.T) {
	view := ArchView{} // no baseline nodes
	df := resolveDeviatesFrom(Deviation{Observation: "unknown"}, view)
	if df.Kind != "pattern" {
		t.Errorf("fallback kind = %q, want pattern", df.Kind)
	}
}

func TestBaselineSummary(t *testing.T) {
	view := ArchView{
		Patterns:    []ArchitecturalPattern{{ID: "p1", Title: "P", Description: "pd"}},
		Conventions: []Convention{{ID: "c1", Title: "C", Description: "cd"}},
		Decisions:   []Decision{{ID: "d1", Title: "D", Rationale: "dr", Chosen: "X"}},
	}
	got := baselineSummary(view)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if !strings.Contains(got[0], "[pattern:p1]") || !strings.Contains(got[1], "[convention:c1]") || !strings.Contains(got[2], "[decision:d1]") {
		t.Errorf("baseline summary tags wrong: %v", got)
	}
}

// stubAdapter is a deterministic ModelAdapter for tests that don't need the real
// lane: it returns a fixed assessment or an error, and can assert it is never
// called (mustNotCall).
type stubAdapter struct {
	t           *testing.T
	assessment  Assessment
	err         error
	mustNotCall bool
}

func (a stubAdapter) AssessChange(_ context.Context, _ AssessChangeRequest) (Assessment, error) {
	if a.mustNotCall {
		a.t.Fatal("adapter.AssessChange called but mustNotCall was set")
	}
	if a.err != nil {
		return Assessment{}, a.err
	}
	return a.assessment, nil
}
