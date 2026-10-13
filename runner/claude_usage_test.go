package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
)

type claudeUsageProvider struct {
	agent.HarnessProvider
	claude *claude.Provider
	stream string
}

func (p *claudeUsageProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return p.claude.Spawn(ctx, agent.Spec{Cwd: spec.Cwd, Prompt: p.stream})
}

func (p *claudeUsageProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func TestRun_ClaudeTerminalUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native CLI subprocess test is unix-only")
	}
	dir := t.TempDir()
	source, err := os.ReadFile("../provider/harness/clijsonl/testdata/usagecli/main.go")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.WriteFile("fixture.go", source, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "claude-fixture")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", "claude-fixture", "fixture.go")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stream-json fixture: %v\n%s", err, output)
	}
	const calls = `{"type":"system","subtype":"init","session_id":"usage-session"}
{"type":"assistant","message":{"model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}
{"type":"assistant","message":{"model":"claude-opus-4-1","content":[],"usage":{"input_tokens":15,"output_tokens":4,"cache_read_input_tokens":200,"cache_creation_input_tokens":30}}}`
	wantCalls := &agent.CostData{InputTokens: 25, OutputTokens: 7, CachedInputTokens: 300, CacheWriteTokens: 50, NumTurns: 2}
	cases := []struct {
		name   string
		stream string
		want   *agent.CostData
		model  string
	}{
		{name: "EOF without result", stream: calls, want: wantCalls, model: "claude-opus-4-1"},
		{name: "empty error result retains calls", stream: calls + `
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"failed","usage":{}}`, want: wantCalls, model: "claude-opus-4-1"},
		{name: "empty tokens retain terminal metadata", stream: calls + `
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":3,"total_cost_usd":0.1,"usage":{}}`, want: &agent.CostData{InputTokens: 25, OutputTokens: 7, CachedInputTokens: 300, CacheWriteTokens: 50, TotalCostUsd: 0.1, NumTurns: 3}, model: "claude-opus-4-1"},
		{name: "error result usage is authoritative", stream: calls + `
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"failed","num_turns":2,"total_cost_usd":0.25,"usage":{"input_tokens":40,"output_tokens":12,"cache_read_input_tokens":500,"cache_creation_input_tokens":60}}`, want: &agent.CostData{InputTokens: 40, OutputTokens: 12, CachedInputTokens: 500, CacheWriteTokens: 60, TotalCostUsd: 0.25, NumTurns: 2}, model: "claude-opus-4-1"},
		{name: "identity without per-call usage", stream: `{"type":"assistant","message":{"model":"claude-sonnet-4-5","content":[]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input_tokens":9,"output_tokens":2}}`, want: &agent.CostData{InputTokens: 9, OutputTokens: 2}, model: "claude-sonnet-4-5"},
		{name: "unreported usage stays nil", stream: `{"type":"assistant","message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[]}}
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"usage":{}}`, model: "claude-sonnet-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := claude.New(claude.Options{Binary: binary})
			if err != nil {
				t.Fatal(err)
			}
			platform := newRecordingPlatformServer(t)
			res, _ := runScriptedSession(t, scriptedSession{
				workType: "qa", skipSteering: true, platform: platform,
				// The JSONL handle keeps its channel open for injection after EOF.
				// Bound the runner's existing no-progress classification here.
				idleTimeout: 5 * time.Second,
				provider: func(base agent.HarnessProvider) agent.Provider {
					return &claudeUsageProvider{HarnessProvider: base, claude: provider, stream: tc.stream}
				},
			})
			if res.Status != "failed" {
				t.Errorf("Status = %q; want failed", res.Status)
			}
			if tc.want == nil {
				if res.Cost != nil {
					t.Errorf("Cost = %+v; want nil for unreported usage", res.Cost)
				}
			} else if res.Cost == nil || *res.Cost != *tc.want {
				t.Errorf("Cost = %+v; want %+v", res.Cost, tc.want)
			}
			status := platform.terminalStatus(t)
			if got := string(status["model"]); got != `"`+tc.model+`"` {
				t.Errorf("posted model = %s; want %q", got, tc.model)
			}
			want := agent.CostData{}
			if tc.want != nil {
				want = *tc.want
			}
			for key, expected := range map[string]int64{"inputTokens": want.InputTokens, "outputTokens": want.OutputTokens, "cacheReadTokens": want.CachedInputTokens, "cacheWriteTokens": want.CacheWriteTokens} {
				var got int64
				if raw, present := status[key]; present {
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Fatal(err)
					}
				} else if expected != 0 {
					t.Errorf("posted %s missing; want %d", key, expected)
				}
				if got != expected {
					t.Errorf("posted %s = %d; want %d", key, got, expected)
				}
			}
		})
	}
}

func TestRun_ObservedModelOnTerminalPaths(t *testing.T) {
	call := agent.LlmCallEvent{ResponseModel: "claude-sonnet-4-5", InputTokens: 25, OutputTokens: 7, ReasoningTokens: 2, UsageSource: agent.LlmUsageProvider}
	identity := agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, ObservedModel: &agent.ObservedModelIdentity{Model: "claude-opus-4-1"}}
	cases := []struct {
		name   string
		turn   verdictScriptTurn
		budget *prompt.StageBudget
		model  string
	}{
		{name: "completed", turn: verdictScriptTurn{events: []agent.Event{call, identity}, text: "WORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"}, model: "claude-opus-4-1"},
		{name: "empty terminal with reasoning", turn: verdictScriptTurn{events: []agent.Event{call}, cost: &agent.CostData{}, text: "WORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"}, model: "claude-sonnet-4-5"},
		{name: "crash", turn: verdictScriptTurn{events: []agent.Event{call}, crash: true}, model: "claude-sonnet-4-5"},
		{name: "budget cap", turn: verdictScriptTurn{events: []agent.Event{call}}, budget: &prompt.StageBudget{MaxTokens: 20}, model: "claude-sonnet-4-5"},
		{name: "blocked", turn: verdictScriptTurn{events: []agent.Event{call, identity}, text: "WORK_RESULT: blocked\nAGENT_BLOCKED: missing input"}, model: "claude-opus-4-1"},
		{name: "request and synthetic models ignored", turn: verdictScriptTurn{events: []agent.Event{call, agent.LlmCallEvent{Model: "requested-model"}, agent.LlmCallEvent{ResponseModel: "synthetic-model", Synthetic: true}, agent.SystemEvent{Subtype: agent.SystemSubtypeModelIdentity, ObservedModel: &agent.ObservedModelIdentity{}}}, text: "WORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"}, model: "claude-sonnet-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			platform := newRecordingPlatformServer(t)
			res, _ := runScriptedSession(t, scriptedSession{workType: "qa", skipSteering: true, platform: platform, budget: tc.budget, turns: []verdictScriptTurn{tc.turn}})
			if res.Model != tc.model {
				t.Errorf("Model = %q; want %q", res.Model, tc.model)
			}
			if res.Cost == nil || res.Cost.InputTokens != 25 || res.Cost.OutputTokens != 7 || res.Cost.ReasoningTokens != 2 {
				t.Errorf("Cost = %+v; want 25 input and 7 output tokens including 2 reasoning tokens", res.Cost)
			}
			if got := string(platform.terminalStatus(t)["model"]); got != `"`+tc.model+`"` {
				t.Errorf("posted model = %s; want %q", got, tc.model)
			}
		})
	}
}

func TestRun_ObservedModelAcrossFollowUps(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", platform: platform,
		turns: []verdictScriptTurn{
			{events: []agent.Event{agent.LlmCallEvent{ResponseModel: "claude-sonnet-4-5", InputTokens: 10, UsageSource: agent.LlmUsageProvider}}, text: "Still working."},
			{events: []agent.Event{agent.LlmCallEvent{ResponseModel: "claude-opus-4-1", InputTokens: 15, UsageSource: agent.LlmUsageProvider}}, text: "WORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"},
		},
	})
	if res.Status != "completed" || res.Cost == nil || res.Cost.InputTokens != 25 {
		t.Errorf("follow-up result: status=%q cost=%+v; want completed with 25 input tokens", res.Status, res.Cost)
	}
	if got := string(platform.terminalStatus(t)["model"]); got != `"claude-opus-4-1"` {
		t.Errorf("posted model = %s; want the follow-up response model", got)
	}
}
