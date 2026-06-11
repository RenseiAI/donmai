package codeintel

import (
	"strings"
	"testing"
)

// ── ReadDiffObservations ──────────────────────────────────────────────────────

func TestReadDiffObservations_ZonePatterns(t *testing.T) {
	diff := PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   42,
		Title:      "feat: add auth middleware",
		Body:       "",
		Files: []PrFileDiff{
			{Path: "src/auth/middleware.go", Patch: "+package auth", Added: true},
			{Path: "src/auth/jwt.go", Patch: "+package auth", Added: true},
			{Path: "src/api/routes.go", Patch: "+package api", Added: false},
		},
	}

	obs := ReadDiffObservations(diff, "project")

	// Should find at least one "Auth layer" pattern and one "API layer" pattern.
	found := make(map[string]bool)
	for _, o := range obs {
		if o.Kind != "pattern" {
			continue
		}
		title, _ := o.Payload["title"].(string)
		found[title] = true
	}

	if !found["Auth layer pattern"] {
		t.Errorf("expected 'Auth layer pattern' in observations; got titles: %v", keys(found))
	}
	if !found["API layer pattern"] {
		t.Errorf("expected 'API layer pattern' in observations; got titles: %v", keys(found))
	}
}

func TestReadDiffObservations_ConventionSignals(t *testing.T) {
	diff := PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   7,
		Title:      "refactor: add result type",
		Body:       "",
		Files: []PrFileDiff{
			{
				Path:  "src/core/result.ts",
				Patch: "+export type MyResult = Result<string, Error>;\n+const x = Result<number, string>;",
			},
		},
	}

	obs := ReadDiffObservations(diff, "project")

	found := false
	for _, o := range obs {
		if o.Kind == "convention" {
			if title, _ := o.Payload["title"].(string); title == "Result<T, E> error handling" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 'Result<T, E> error handling' convention observation")
	}
}

func TestReadDiffObservations_DecisionInference(t *testing.T) {
	diff := PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   5,
		Title:      "chore: chose drizzle over prisma for edge runtime",
		Body:       "We chose drizzle over prisma because of edge runtime support.",
		Files:      []PrFileDiff{},
	}

	obs := ReadDiffObservations(diff, "project")

	found := false
	for _, o := range obs {
		if o.Kind == "decision" {
			if title, _ := o.Payload["title"].(string); strings.Contains(title, "drizzle") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected a decision observation containing 'drizzle'")
	}
}

func TestReadDiffObservations_AcceptanceCriteria(t *testing.T) {
	diff := PrDiff{
		Repository:         "github.com/org/repo",
		PrNumber:           3,
		Title:              "feat: AC test",
		Body:               "",
		Files:              []PrFileDiff{},
		AcceptanceCriteria: []string{"must return 200 OK on success", "short"},
	}

	obs := ReadDiffObservations(diff, "project")

	// "must return 200..." → normative → "convention"; "short" → skipped (< 10 chars)
	found := false
	for _, o := range obs {
		if o.Kind == "convention" {
			if desc, _ := o.Payload["description"].(string); strings.Contains(desc, "must return 200") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected a convention observation from the normative AC item")
	}
}

func TestReadDiffObservations_EmptyDiff(t *testing.T) {
	diff := PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   1,
		Title:      "chore: update readme",
		Body:       "",
		Files:      []PrFileDiff{},
	}

	obs := ReadDiffObservations(diff, "project")
	if len(obs) != 0 {
		t.Errorf("expected no observations for empty diff; got %d", len(obs))
	}
}

func TestReadDiffObservations_DefaultScope(t *testing.T) {
	diff := PrDiff{
		Repository: "github.com/org/repo",
		PrNumber:   1,
		Title:      "feat: add auth handler",
		Body:       "",
		Files: []PrFileDiff{
			{Path: "src/auth/handler.go", Patch: "+package auth"},
		},
	}

	// Empty scope string should default to "project"
	obs := ReadDiffObservations(diff, "")
	for _, o := range obs {
		if o.Scope != "project" {
			t.Errorf("expected scope 'project', got %q", o.Scope)
		}
	}
}

// ── EvaluateGate ──────────────────────────────────────────────────────────────

func TestEvaluateGate_None(t *testing.T) {
	highConf := []DiffObservation{{Kind: "pattern", Confidence: 0.9}}
	if EvaluateGate(highConf, "none") {
		t.Error("gate 'none' should never block")
	}
}

func TestEvaluateGate_ZeroDeviations(t *testing.T) {
	empty := []DiffObservation{}
	if EvaluateGate(empty, "zero-deviations") {
		t.Error("zero observations should not gate under zero-deviations")
	}

	one := []DiffObservation{{Kind: "pattern", Confidence: 0.1}}
	if !EvaluateGate(one, "zero-deviations") {
		t.Error("one observation should gate under zero-deviations")
	}
}

func TestEvaluateGate_NoSeverityHigh(t *testing.T) {
	lowConf := []DiffObservation{{Kind: "pattern", Confidence: 0.40}}
	if EvaluateGate(lowConf, "no-severity-high") {
		t.Error("low-confidence observation should not gate under no-severity-high")
	}

	highConf := []DiffObservation{{Kind: "convention", Confidence: 0.70}}
	if !EvaluateGate(highConf, "no-severity-high") {
		t.Error("high-confidence observation should gate under no-severity-high")
	}
}

func TestEvaluateGate_EmptyPolicy(t *testing.T) {
	// Empty policy defaults to no-severity-high.
	highConf := []DiffObservation{{Kind: "pattern", Confidence: 0.70}}
	if !EvaluateGate(highConf, "") {
		t.Error("high-confidence observation should gate under default (empty) policy")
	}
}

func TestEvaluateGate_MaxN(t *testing.T) {
	obs := []DiffObservation{
		{Kind: "pattern", Confidence: 0.3},
		{Kind: "convention", Confidence: 0.4},
		{Kind: "decision", Confidence: 0.5},
	}

	if EvaluateGate(obs, "max:3") {
		t.Error("3 observations should not gate under max:3 (> 3 required)")
	}
	if !EvaluateGate(obs, "max:2") {
		t.Error("3 observations should gate under max:2")
	}
	if !EvaluateGate(obs, "max:0") {
		t.Error("any observations should gate under max:0")
	}
}

func TestEvaluateGate_UnknownPolicy(t *testing.T) {
	obs := []DiffObservation{{Kind: "pattern", Confidence: 0.9}}
	if EvaluateGate(obs, "unknown-policy") {
		t.Error("unknown gate policy should not block")
	}
}

// ── BuildNativeDriftReport ────────────────────────────────────────────────────

func TestBuildNativeDriftReport_Clean(t *testing.T) {
	diff := PrDiff{Repository: "github.com/org/repo", PrNumber: 1, Title: "chore: readme"}
	report := BuildNativeDriftReport(diff, []DiffObservation{}, "no-severity-high")

	if report.Gated {
		t.Error("empty observations should not be gated")
	}
	if report.Mode != "native-diff-only" {
		t.Errorf("expected mode 'native-diff-only', got %q", report.Mode)
	}
	if report.HasCriticalDrift {
		t.Error("HasCriticalDrift should always be false in native mode")
	}
	if len(report.Deviations) != 0 {
		t.Error("Deviations should be empty in native mode")
	}
	if report.Change.Repository != "github.com/org/repo" {
		t.Errorf("unexpected change repository: %q", report.Change.Repository)
	}
}

func TestBuildNativeDriftReport_Gated(t *testing.T) {
	obs := []DiffObservation{{Kind: "convention", Confidence: 0.70}}
	diff := PrDiff{Repository: "github.com/org/repo", PrNumber: 5, Title: "feat: add convention"}
	report := BuildNativeDriftReport(diff, obs, "no-severity-high")

	if !report.Gated {
		t.Error("high-confidence observation should gate under no-severity-high")
	}
	if !strings.Contains(report.Summary, "BLOCKED") {
		t.Errorf("summary should mention BLOCKED; got: %q", report.Summary)
	}
}

func TestBuildNativeDriftReport_SummaryFormat(t *testing.T) {
	obs := []DiffObservation{
		{Kind: "pattern", Confidence: 0.40},
		{Kind: "convention", Confidence: 0.55},
		{Kind: "decision", Confidence: 0.60},
	}
	diff := PrDiff{Repository: "github.com/org/repo", PrNumber: 9}
	report := BuildNativeDriftReport(diff, obs, "none")

	if !strings.Contains(report.Summary, "3 diff signal(s)") {
		t.Errorf("summary should mention 3 signals; got: %q", report.Summary)
	}
	if !strings.Contains(strings.ToLower(report.Summary), "native diff-only mode") || !strings.Contains(report.Summary, "DONMAI_ARCH_BIN") {
		t.Errorf("summary should mention native mode and DONMAI_ARCH_BIN; got: %q", report.Summary)
	}
}

func TestBuildNativeDriftReport_AssessedAtNonEmpty(t *testing.T) {
	diff := PrDiff{Repository: "github.com/org/repo", PrNumber: 1}
	report := BuildNativeDriftReport(diff, nil, "none")
	if report.AssessedAt == "" {
		t.Error("AssessedAt should not be empty")
	}
}

// ── acTitle helper ────────────────────────────────────────────────────────────

func TestAcTitle_Short(t *testing.T) {
	got := acTitle("- must not exceed limits")
	if strings.HasPrefix(got, "-") || strings.HasPrefix(got, " ") {
		t.Errorf("acTitle should strip leading markers; got %q", got)
	}
}

func TestAcTitle_Long(t *testing.T) {
	long := strings.Repeat("x", 100)
	got := acTitle(long)
	if len(got) > 80 {
		t.Errorf("acTitle should truncate to 80 chars; got len %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated title should end with '...'; got %q", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
