package codeintel

import (
	"encoding/json"
	"testing"
)

func sampleCase() Case {
	return Case{
		ID: "codeintel-find-symbol-donmai-001",
		Input: CaseInput{
			TaskType: TaskFindSymbol,
			Repo:     "RenseiAI/donmai",
			Ref:      "13a3fdd404331a8b7348bbf6474e85fe68c073f1",
			Prompt:   "Where is newAgentRunCmd defined?",
		},
		ExpectedOutput: json.RawMessage(`{"file":"afcli/agent_run.go","lineRange":[70,100]}`),
		Tags:           []string{"codeintel-eval", "find-symbol", "v1"},
	}
}

func TestBuildEnvelope_ShapeAndCrossLinks(t *testing.T) {
	c := sampleCase()
	tr := Transcript{
		Arm:         ArmWith,
		FinalAnswer: "afcli/agent_run.go:80",
		ToolCalls: []ToolCall{
			{Name: "mcp__af-code-intelligence__af_code_search_symbols", Arguments: json.RawMessage(`{"query":"newAgentRunCmd"}`), ResultText: "afcli/agent_run.go:79"},
		},
		TurnCount:   2,
		TokenCounts: TokenCounts{Input: 1200, Output: 300},
		SnapshotRef: &SnapshotRef{Provider: "local", SnapshotID: "wa-1", Retain: RetainEvalPermanent},
	}
	grades := []GradeResult{{GraderID: "structural/codeintel-task-v1", Score: 1, Pass: true}}
	meta := ReportMeta{CaseID: c.ID, Arm: ArmWith, Family: string(c.Family()), Repo: c.Input.Repo, Trial: 1}

	env, err := BuildEnvelope("run-1", "trace-1", "disp-1", "org-1", "proj-1", "ds-1", c, tr, grades, meta)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	// Cross-links must knit run<->trace together.
	if env.Run.TraceRef != "trace-1" {
		t.Errorf("run.traceRef = %q, want trace-1", env.Run.TraceRef)
	}
	if env.Trace.EvalRunID != "run-1" {
		t.Errorf("trace.evalRunId = %q, want run-1", env.Trace.EvalRunID)
	}
	if env.Run.DatasetCaseID != c.ID {
		t.Errorf("run.datasetCaseId = %q, want %q", env.Run.DatasetCaseID, c.ID)
	}
	if env.Run.InputHash == "" || env.Run.OutputHash == "" {
		t.Error("input/output hashes must be populated")
	}
	// Benchmark snapshots must be retained.
	if env.Trace.Retain != RetainEvalPermanent {
		t.Errorf("trace.retain = %q, want %q", env.Trace.Retain, RetainEvalPermanent)
	}
	// The signal set the graders/metrics need must survive into the trace.
	if env.Trace.TurnCount != 2 || env.Trace.TokenCounts.Total() != 1500 || len(env.Trace.ToolCalls) != 1 {
		t.Errorf("trace signal set wrong: turns=%d tokens=%d tools=%d",
			env.Trace.TurnCount, env.Trace.TokenCounts.Total(), len(env.Trace.ToolCalls))
	}

	// The whole envelope must marshal (the bridge POSTs it verbatim).
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("envelope not round-trippable: %v", err)
	}
	if _, ok := round["run"]; !ok {
		t.Error("envelope missing run")
	}
	if _, ok := round["trace"]; !ok {
		t.Error("envelope missing trace")
	}
}

func TestHashPayload_DeterministicAndSensitive(t *testing.T) {
	a, err := hashPayload(map[string]any{"x": 1, "y": "two"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hashPayload(map[string]any{"x": 1, "y": "two"})
	if a != b {
		t.Errorf("hash not deterministic: %s != %s", a, b)
	}
	c, _ := hashPayload(map[string]any{"x": 2, "y": "two"})
	if a == c {
		t.Error("hash must change when payload changes")
	}
}

// TestTokenCounts_JSONTagsMatchPlatform locks the wire tags to the platform
// tokenCounts shape ({input, output, cache_read}).
func TestTokenCounts_JSONTagsMatchPlatform(t *testing.T) {
	b, _ := json.Marshal(TokenCounts{Input: 1, Output: 2, CacheRead: 3})
	got := string(b)
	want := `{"input":1,"output":2,"cache_read":3}`
	if got != want {
		t.Errorf("tokenCounts JSON = %s, want %s", got, want)
	}
}

// TestTokenCounts_TotalIncludesCacheRead guards the tokens-to-solution numerator
// against the WITH-arm hiding its context/MCP-schema overhead in cached-input
// accounting: cache-read tokens are real cost and MUST count toward Total(),
// or the <=+10% token gate can be satisfied by an arm that is actually more
// expensive.
func TestTokenCounts_TotalIncludesCacheRead(t *testing.T) {
	// A WITH-arm-shaped trace: modest fresh input+output, but a large cached
	// prompt (the MCP tool schemas + orienting context) landing as cache reads.
	tc := TokenCounts{Input: 200, Output: 45, CacheRead: 5000}
	if got := tc.Total(); got != 5245 {
		t.Errorf("Total() = %d, want 5245 (input+output+cacheRead) — cache reads must not be free", got)
	}
	// Cache-read-only cost must still register (an arm whose spend is entirely
	// cached input is not a zero-cost arm).
	if got := (TokenCounts{CacheRead: 1000}).Total(); got != 1000 {
		t.Errorf("Total() = %d, want 1000 for a cache-read-only trace", got)
	}
}
