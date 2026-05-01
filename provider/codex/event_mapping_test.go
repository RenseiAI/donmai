package codex

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestMapNotification_ThreadStarted(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	params := mustJSON(t, map[string]any{"thread": map[string]any{"id": "tid-1"}})
	got := mapNotification("thread/started", params, state, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	init, ok := got[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("expected InitEvent, got %T", got[0])
	}
	if init.SessionID != "tid-1" {
		t.Fatalf("expected sessionID=tid-1, got %q", init.SessionID)
	}
	if state.sessionID != "tid-1" {
		t.Fatalf("expected state.sessionID=tid-1, got %q", state.sessionID)
	}
}

func TestMapNotification_TurnStarted(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	got := mapNotification("turn/started", mustJSON(t, map[string]any{"turn": map[string]any{"id": "t1"}}), state, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if _, ok := got[0].(agent.SystemEvent); !ok {
		t.Fatalf("expected SystemEvent, got %T", got[0])
	}
	if state.turnCount != 1 {
		t.Fatalf("expected turnCount=1, got %d", state.turnCount)
	}
}

func TestMapNotification_TurnCompleted_Success(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: DefaultCodexModel}
	params := mustJSON(t, map[string]any{
		"turn": map[string]any{
			"id":     "t1",
			"status": "completed",
			"usage": map[string]any{
				"input_tokens":        500_000,
				"output_tokens":       100_000,
				"cached_input_tokens": 100_000,
			},
		},
	})
	got := mapNotification("turn/completed", params, state, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	res, ok := got[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("expected ResultEvent, got %T", got[0])
	}
	if !res.Success {
		t.Fatalf("expected success=true")
	}
	if res.Cost == nil {
		t.Fatalf("expected cost data")
	}
	// 400000 fresh*$2 + 100000 cached*$0.5 + 100000 output*$8 = $0.8 + $0.05 + $0.8 = $1.65
	wantCost := 1.65
	if abs(res.Cost.TotalCostUsd-wantCost) > 0.0001 {
		t.Fatalf("expected total cost ~%.2f, got %.4f", wantCost, res.Cost.TotalCostUsd)
	}
}

func TestMapNotification_TurnCompleted_CamelCaseUsage(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: DefaultCodexModel}
	params := mustJSON(t, map[string]any{
		"turn": map[string]any{
			"status": "completed",
			"usage":  map[string]any{"inputTokens": 1000, "outputTokens": 500, "cachedInputTokens": 200},
		},
	})
	got := mapNotification("turn/completed", params, state, nil)
	res := got[0].(agent.ResultEvent)
	if res.Cost.InputTokens != 1000 || res.Cost.OutputTokens != 500 {
		t.Fatalf("unexpected token totals: %+v", res.Cost)
	}
}

func TestMapNotification_TurnCompleted_Failed(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: DefaultCodexModel}
	params := mustJSON(t, map[string]any{
		"turn": map[string]any{
			"status": "failed",
			"error":  map[string]any{"message": "boom", "codexErrorInfo": "rate_limited"},
		},
	})
	got := mapNotification("turn/completed", params, state, nil)
	res := got[0].(agent.ResultEvent)
	if res.Success {
		t.Fatalf("expected failure")
	}
	if res.ErrorSubtype != "rate_limited" {
		t.Fatalf("expected ErrorSubtype=rate_limited, got %q", res.ErrorSubtype)
	}
	if len(res.Errors) == 0 || res.Errors[0] != "boom" {
		t.Fatalf("expected errors=[boom], got %v", res.Errors)
	}
}

func TestMapNotification_TurnCompleted_Interrupted(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: DefaultCodexModel}
	params := mustJSON(t, map[string]any{"turn": map[string]any{"status": "interrupted"}})
	got := mapNotification("turn/completed", params, state, nil)
	res := got[0].(agent.ResultEvent)
	if res.ErrorSubtype != "interrupted" {
		t.Fatalf("expected interrupted, got %q", res.ErrorSubtype)
	}
}

func TestMapNotification_AssistantMessageDelta(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	got := mapNotification("item/agentMessage/delta", mustJSON(t, map[string]any{"delta": "hello"}), state, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	at, ok := got[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("expected AssistantTextEvent, got %T", got[0])
	}
	if at.Text != "hello" {
		t.Fatalf("expected hello, got %q", at.Text)
	}
}

func TestMapNotification_ReasoningDelta(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	got := mapNotification("item/reasoning/textDelta", mustJSON(t, map[string]any{"text": "thinking..."}), state, nil)
	se := got[0].(agent.SystemEvent)
	if se.Subtype != "reasoning" || se.Message != "thinking..." {
		t.Fatalf("unexpected reasoning event: %+v", se)
	}
}

func TestMapItem_CommandExecutionStartedAndCompleted(t *testing.T) {
	t.Parallel()
	startedParams := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":      "shell-1",
			"type":    "commandExecution",
			"command": "git status",
		},
	})
	got := mapNotification("item/started", startedParams, &mapperState{}, nil)
	tu, ok := got[0].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("expected ToolUseEvent, got %T", got[0])
	}
	if tu.ToolName != "shell" {
		t.Fatalf("expected toolName=shell, got %q", tu.ToolName)
	}
	if tu.Input["command"] != "git status" {
		t.Fatalf("expected command=git status, got %v", tu.Input)
	}

	exitCode := 0
	completedParams := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":       "shell-1",
			"type":     "commandExecution",
			"text":     "clean\n",
			"status":   "completed",
			"exitCode": exitCode,
		},
	})
	got = mapNotification("item/completed", completedParams, &mapperState{}, nil)
	tr, ok := got[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("expected ToolResultEvent, got %T", got[0])
	}
	if tr.IsError {
		t.Fatalf("expected non-error result")
	}
	if tr.Content != "clean\n" {
		t.Fatalf("expected content=clean, got %q", tr.Content)
	}
}

func TestMapItem_CommandExecutionFailed(t *testing.T) {
	t.Parallel()
	exit := 1
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":       "x",
			"type":     "commandExecution",
			"status":   "failed",
			"exitCode": exit,
		},
	})
	got := mapNotification("item/completed", params, &mapperState{}, nil)
	tr := got[0].(agent.ToolResultEvent)
	if !tr.IsError {
		t.Fatalf("expected IsError=true on failure")
	}
}

func TestMapItem_MCPToolCall_NormalizedName(t *testing.T) {
	t.Parallel()
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":        "mcp-1",
			"type":      "mcpToolCall",
			"server":    "af-linear",
			"tool":      "af_linear_get_issue",
			"arguments": map[string]any{"id": "REN-1"},
		},
	})
	got := mapNotification("item/started", params, &mapperState{}, nil)
	tu := got[0].(agent.ToolUseEvent)
	if tu.ToolName != "mcp__af-linear__af_linear_get_issue" {
		t.Fatalf("unexpected tool name: %q", tu.ToolName)
	}
	if tu.ToolCategory != "linear" {
		t.Fatalf("expected category=linear, got %q", tu.ToolCategory)
	}
}

func TestMapNotification_Unknown(t *testing.T) {
	t.Parallel()
	got := mapNotification("totally/new", mustJSON(t, map[string]any{}), &mapperState{}, nil)
	se, ok := got[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("expected SystemEvent for unknown method, got %T", got[0])
	}
	if se.Subtype != "unknown" {
		t.Fatalf("expected subtype=unknown, got %q", se.Subtype)
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	in := "\x1b[32mhello\x1b[0m world"
	if got := stripANSI(in); got != "hello world" {
		t.Fatalf("expected %q, got %q", "hello world", got)
	}
}

func TestCalculateCostUSD_DefaultPricing(t *testing.T) {
	t.Parallel()
	// 1M fresh input ($2.00) + 500k cached ($0.25) + 200k output ($1.6) = $3.85
	got := calculateCostUSD(1_500_000, 500_000, 200_000, "gpt-5-codex")
	want := 3.85
	if abs(got-want) > 0.0001 {
		t.Fatalf("expected %.4f, got %.4f", want, got)
	}
}

func TestCalculateCostUSD_UnknownModelFallsBack(t *testing.T) {
	t.Parallel()
	got := calculateCostUSD(1_000_000, 0, 0, "unknown-model")
	want := 2.0
	if abs(got-want) > 0.0001 {
		t.Fatalf("expected fallback to default model pricing, got %.4f", got)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return buf
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
