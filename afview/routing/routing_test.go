package routing_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/routing"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func ptrFloat(v float64) *float64 { return &v }

func sampleConfig() *afclient.RoutingConfig {
	score1 := 0.210
	score2 := 0.820
	cost1 := 1.50
	lat1 := 320.0
	return &afclient.RoutingConfig{
		CapturedAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		Weights:    afclient.RoutingWeights{Cost: 0.7, Latency: 0.3},
		CapabilityFilters: []afclient.CapabilityFilter{
			{Field: "os", Op: "in", Value: "linux,macos"},
			{Field: "arch", Op: "eq", Value: "arm64"},
		},
		SandboxProviders: []afclient.SandboxProviderState{
			{
				ProviderID: "local", Alpha: 12, Beta: 3,
				RecentScore: &score1, RecentCostCents: &cost1, RecentLatencyMs: &lat1,
				SelectionCount: 15,
			},
			{ProviderID: "e2b", Alpha: 5, Beta: 8, RecentScore: &score2, SelectionCount: 13},
		},
		LLMProviders: []afclient.LLMProviderState{
			{ProviderID: "claude", Model: "claude-opus-4-7", Alpha: 20, Beta: 2, RecentScore: &score1, SelectionCount: 22},
			{ProviderID: "codex", Model: "gpt-5", Alpha: 8, Beta: 6, RecentScore: &score2, SelectionCount: 14},
		},
		RecentDecisions: []afclient.RoutingDecision{
			{
				SessionID: "sess-abc123", ChosenSandbox: "local", ChosenLLM: "claude",
				Score: 0.210, EstimatedCostCents: &cost1,
				DecidedAt: time.Date(2026, 5, 7, 11, 59, 0, 0, time.UTC),
			},
			{
				SessionID: "sess-xyz987", ChosenSandbox: "e2b", ChosenLLM: "codex",
				Score:     0.820,
				DecidedAt: time.Date(2026, 5, 7, 11, 58, 0, 0, time.UTC),
				RejectedCandidates: []afclient.RejectedCandidate{
					{ProviderID: "local", Dimension: "sandbox", Reason: "capacity-filter", Detail: "at 95% capacity"},
				},
			},
		},
	}
}

// ─── RenderShow ───────────────────────────────────────────────────────────────

func TestRenderShow_ContainsSections(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := sampleConfig()
	if err := routing.RenderShow(&buf, cfg, true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	output := buf.String()

	for _, want := range []string{
		"Routing Configuration",
		"Scoring Weights",
		"Capability Filters",
		"Sandbox Providers",
		"LLM Providers",
		"2D Routing Heatmap",
		"Recent Decisions",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected section %q in output:\n%s", want, output)
		}
	}
}

func TestRenderShow_Weights(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := sampleConfig()
	cfg.Weights = afclient.RoutingWeights{Cost: 0.7, Latency: 0.3}
	if err := routing.RenderShow(&buf, cfg, true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "70%") {
		t.Errorf("expected cost weight 70%% in output:\n%s", out)
	}
	if !strings.Contains(out, "30%") {
		t.Errorf("expected latency weight 30%% in output:\n%s", out)
	}
}

func TestRenderShow_CapabilityFilters(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := routing.RenderShow(&buf, sampleConfig(), true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "os") || !strings.Contains(out, "arm64") {
		t.Errorf("missing capability filters in output:\n%s", out)
	}
}

func TestRenderShow_Tables(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := routing.RenderShow(&buf, sampleConfig(), true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"local", "e2b", "claude", "codex", "claude-opus-4-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestRenderShow_RecentDecisions(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := routing.RenderShow(&buf, sampleConfig(), true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sess-abc123") {
		t.Errorf("expected session id in output:\n%s", out)
	}
	if !strings.Contains(out, "0.210") {
		t.Errorf("expected score 0.210 in output:\n%s", out)
	}
}

func TestRenderShow_EmptyConfig(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := &afclient.RoutingConfig{
		CapturedAt: time.Now().UTC(),
		Weights:    afclient.RoutingWeights{Cost: 0.7, Latency: 0.3},
	}
	if err := routing.RenderShow(&buf, cfg, true); err != nil {
		t.Fatalf("RenderShow with empty config: %v", err)
	}
	if !strings.Contains(buf.String(), "Routing Configuration") {
		t.Errorf("expected header even with empty config:\n%s", buf.String())
	}
}

func TestRenderShow_ANSIColorPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := routing.RenderShow(&buf, sampleConfig(), false); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	// ANSI escape sequences begin with \033[. Don't pin exact codes;
	// just confirm we see them somewhere in the output.
	if !strings.Contains(buf.String(), "\033[") {
		t.Errorf("expected ANSI escapes when noColor=false:\n%s", buf.String())
	}
}

// ─── RenderExplain ────────────────────────────────────────────────────────────

func TestRenderExplain_Basic(t *testing.T) {
	t.Parallel()
	resp := &afclient.RoutingExplainResponse{
		SessionID: "sess-explain-001",
		Decision: afclient.RoutingDecision{
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.180,
			RejectedCandidates: []afclient.RejectedCandidate{
				{ProviderID: "e2b", Dimension: "sandbox", Reason: "capability-filter", Detail: "requires linux, got macos"},
				{ProviderID: "codex", Dimension: "llm", Reason: "score-loss"},
			},
		},
		Trace: []afclient.RoutingTraceStep{
			{
				Step:      1,
				Phase:     "capability-filter",
				Dimension: "sandbox",
				Remaining: []string{"local", "docker"},
				Eliminated: []afclient.EliminatedProvider{
					{ProviderID: "e2b", Reason: "os mismatch: requires linux, host is macos"},
				},
				Note: "Filtered 1 provider on os capability.",
			},
			{
				Step:      2,
				Phase:     "score",
				Dimension: "sandbox",
				Remaining: []string{"local"},
				Eliminated: []afclient.EliminatedProvider{
					{ProviderID: "docker", Reason: "score-loss", Detail: "score 0.220 vs winner 0.180"},
				},
				Note: "local selected with lowest composite score.",
			},
		},
	}

	var buf bytes.Buffer
	if err := routing.RenderExplain(&buf, resp, true); err != nil {
		t.Fatalf("RenderExplain: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Routing Decision Trace",
		"sess-explain-001",
		"Chosen",
		"local",
		"claude",
		"0.180",
		"Rejected Candidates",
		"e2b",
		"capability-filter",
		"Decision Trace",
		"Step 1",
		"Step 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in explain output:\n%s", want, out)
		}
	}
}

func TestRenderExplain_NoTrace(t *testing.T) {
	t.Parallel()
	resp := &afclient.RoutingExplainResponse{
		SessionID: "sess-no-trace",
		Decision: afclient.RoutingDecision{
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.100,
		},
	}
	var buf bytes.Buffer
	if err := routing.RenderExplain(&buf, resp, true); err != nil {
		t.Fatalf("RenderExplain: %v", err)
	}
	if !strings.Contains(buf.String(), "no trace steps available") {
		t.Errorf("expected no-trace notice:\n%s", buf.String())
	}
}

func TestRenderExplain_EstimatedCostAndLatency(t *testing.T) {
	t.Parallel()
	cost := 3.75
	lat := 180.0
	resp := &afclient.RoutingExplainResponse{
		SessionID: "sess-cost-lat",
		Decision: afclient.RoutingDecision{
			ChosenSandbox:      "e2b",
			ChosenLLM:          "claude",
			Score:              0.220,
			EstimatedCostCents: &cost,
			EstimatedLatencyMs: &lat,
		},
	}
	var buf bytes.Buffer
	if err := routing.RenderExplain(&buf, resp, true); err != nil {
		t.Fatalf("RenderExplain: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "3.75") || !strings.Contains(out, "180") {
		t.Errorf("expected cost/latency in output:\n%s", out)
	}
}

// ─── Plain rendering (smoke pin point) ───────────────────────────────────────

func TestPlainShow_Snapshot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := routing.PlainShow(&buf, sampleConfig()); err != nil {
		t.Fatalf("PlainShow: %v", err)
	}
	got := buf.String()

	// Pin the header lines and the canonical key=value form for one
	// row of each section. The plain-text format is what rensei-smokes
	// pins against.
	wantSubs := []string{
		"captured: 2026-05-07T12:00:00Z\n",
		"weights: cost=0.70 latency=0.30\n",
		"capability-filters:\n",
		"  - field=os op=in value=linux,macos\n",
		"sandbox-providers:\n",
		"  - id=local alpha=12.00 beta=3.00 selections=15\n",
		"llm-providers:\n",
		"  - id=claude alpha=20.00 beta=2.00 selections=22\n",
		"recent-decisions:\n",
		"  - session=sess-abc123 sandbox=local llm=claude score=0.210 decided=2026-05-07T11:59:00Z\n",
	}
	for _, w := range wantSubs {
		if !strings.Contains(got, w) {
			t.Errorf("missing substring %q in PlainShow output:\n%s", w, got)
		}
	}
	// No ANSI escapes should leak into plain output.
	if strings.Contains(got, "\033[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%s", got)
	}
}

func TestPlainExplain_NumberedTrace(t *testing.T) {
	t.Parallel()
	resp := &afclient.RoutingExplainResponse{
		SessionID: "sess-explain-001",
		Decision: afclient.RoutingDecision{
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.180,
			RejectedCandidates: []afclient.RejectedCandidate{
				{ProviderID: "e2b", Dimension: "sandbox", Reason: "capability-filter", Detail: "linux only"},
			},
		},
		Trace: []afclient.RoutingTraceStep{
			{
				Step:      1,
				Phase:     "capability-filter",
				Dimension: "sandbox",
				Remaining: []string{"local", "docker"},
				Eliminated: []afclient.EliminatedProvider{
					{ProviderID: "e2b", Reason: "os mismatch"},
				},
				Note: "filtered 1 on os",
			},
			{
				Step:      2,
				Phase:     "score",
				Dimension: "sandbox",
				Remaining: []string{"local"},
				Eliminated: []afclient.EliminatedProvider{
					{ProviderID: "docker", Reason: "score-loss", Detail: "0.22 vs 0.18"},
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := routing.PlainExplain(&buf, resp); err != nil {
		t.Fatalf("PlainExplain: %v", err)
	}
	got := buf.String()

	for _, w := range []string{
		"session: sess-explain-001\n",
		"chosen-sandbox: local\n",
		"chosen-llm: claude\n",
		"score: 0.180\n",
		"rejected:\n",
		"  - id=e2b dim=sandbox reason=capability-filter detail=linux only\n",
		"trace:\n",
		"  step=1 phase=capability-filter dim=sandbox remaining=[local,docker] note=\"filtered 1 on os\"\n",
		"    eliminated=e2b reason=os mismatch detail=-\n",
		"  step=2 phase=score dim=sandbox remaining=[local]\n",
		"    eliminated=docker reason=score-loss detail=0.22 vs 0.18\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing substring %q in PlainExplain output:\n%s", w, got)
		}
	}
	if strings.Contains(got, "\033[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%s", got)
	}
}

func TestPlainExplain_EmptyTrace(t *testing.T) {
	t.Parallel()
	resp := &afclient.RoutingExplainResponse{
		SessionID: "sess-empty",
		Decision: afclient.RoutingDecision{
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.5,
		},
	}
	var buf bytes.Buffer
	if err := routing.PlainExplain(&buf, resp); err != nil {
		t.Fatalf("PlainExplain: %v", err)
	}
	if !strings.Contains(buf.String(), "trace: (empty)") {
		t.Errorf("expected empty-trace marker:\n%s", buf.String())
	}
}

// ─── Utility helpers ──────────────────────────────────────────────────────────

func TestShortID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"short", "short"},
		{"exactly-16-char!", "exactly-16-char!"},
		{"this-is-a-very-long-provider-id", "this-is-a-very-…"},
	} {
		if got := routing.ShortID(tc.in); got != tc.want {
			t.Errorf("ShortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"short", "short"},
		{"sess-abc123", "sess-abc123"},
		{"sess-very-long-session-id-here", "sess-very-lo"},
	} {
		if got := routing.ShortSession(tc.in); got != tc.want {
			t.Errorf("ShortSession(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMeanFloat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []float64
		want float64
	}{
		{[]float64{1.0, 2.0, 3.0}, 2.0},
		{[]float64{0.5, 0.5}, 0.5},
		{nil, 0.0},
		{[]float64{}, 0.0},
	} {
		if got := routing.MeanFloat(tc.in); got != tc.want {
			t.Errorf("MeanFloat(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScoreColorThresholds(t *testing.T) {
	t.Parallel()
	if routing.ScoreColorThresholds.Green != 0.33 {
		t.Errorf("Green threshold = %v, want 0.33", routing.ScoreColorThresholds.Green)
	}
	if routing.ScoreColorThresholds.Yellow != 0.67 {
		t.Errorf("Yellow threshold = %v, want 0.67", routing.ScoreColorThresholds.Yellow)
	}
}

// silence unused-helper warning when ptrFloat falls out of use
var _ = ptrFloat
