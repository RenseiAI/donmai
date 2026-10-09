package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/activity"
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
	if len(got) != 2 {
		t.Fatalf("expected LlmCallEvent + ResultEvent, got %d", len(got))
	}
	llm, ok := got[0].(agent.LlmCallEvent)
	if !ok {
		t.Fatalf("expected LlmCallEvent first, got %T", got[0])
	}
	if llm.UsageSource != agent.LlmUsageProvider || llm.Synthetic {
		t.Fatalf("unexpected usage provenance: %+v", llm)
	}
	if !llm.TurnCompleted || llm.ObservedCostUsd != nil {
		t.Fatalf("Codex completion must report turn, not locally estimated price: %+v", llm)
	}
	if llm.StartTimeUnixNano == "" || llm.EndTimeUnixNano == "" {
		t.Fatalf("per-call timing missing: %+v", llm)
	}
	// Reported input (500000) includes the 100000 cached reads; the harness
	// reports the fresh remainder as InputTokens.
	if llm.InputTokens != 400_000 || llm.OutputTokens != 100_000 || llm.CachedInputTokens != 100_000 {
		t.Fatalf("unexpected per-call usage: %+v", llm)
	}
	res, ok := got[1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("expected ResultEvent second, got %T", got[1])
	}
	if !res.Success {
		t.Fatalf("expected success=true")
	}
	if res.Cost == nil {
		t.Fatalf("expected cost data")
	}
	if res.ObservedCostUsd != nil || res.ObservedTurns == nil || *res.ObservedTurns != 1 {
		t.Fatalf("native turn count and estimated price availability confused: %+v", res)
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
	llm := got[0].(agent.LlmCallEvent)
	if llm.InputTokens != 800 || llm.OutputTokens != 500 || llm.CachedInputTokens != 200 {
		t.Fatalf("unexpected per-call tokens: %+v", llm)
	}
	res := got[1].(agent.ResultEvent)
	if res.Cost.InputTokens != 800 || res.Cost.OutputTokens != 500 {
		t.Fatalf("unexpected token totals: %+v", res.Cost)
	}
}

// Reported input_tokens include cached reads; the harness reports the
// fresh remainder (reported minus cached, floored at zero) as InputTokens
// on both the per-call event and the accumulated result cost, while cached
// and output ride unchanged. Removing the subtraction turns this RED.
func TestMapNotification_TurnCompleted_FreshInputExcludesCached(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		turns []map[string]any
		// per-call fresh input expectations, one per turn.
		wantCallInput []int64
		wantInput     int64
		wantCached    int64
		wantOutput    int64
		wantCost      float64
	}{
		{
			name: "cached over two turns accumulates fresh",
			turns: []map[string]any{
				{"input_tokens": 500_000, "output_tokens": 100_000, "cached_input_tokens": 100_000},
				{"input_tokens": 300_000, "output_tokens": 50_000, "cached_input_tokens": 200_000},
			},
			wantCallInput: []int64{400_000, 100_000},
			wantInput:     500_000,
			wantCached:    300_000,
			wantOutput:    150_000,
			// 500000 fresh*$2 + 300000 cached*$0.5 + 150000 output*$8
			// = $1.0 + $0.15 + $1.2 = $2.35
			wantCost: 2.35,
		},
		{
			name: "no cached tokens passes input through",
			turns: []map[string]any{
				{"input_tokens": 1000, "output_tokens": 500, "cached_input_tokens": 0},
			},
			wantCallInput: []int64{1000},
			wantInput:     1000,
			wantCached:    0,
			wantOutput:    500,
			wantCost:      0.006,
		},
		{
			name: "cached above input floors fresh at zero",
			turns: []map[string]any{
				{"input_tokens": 100, "output_tokens": 50, "cached_input_tokens": 500},
			},
			wantCallInput: []int64{0},
			wantInput:     0,
			wantCached:    500,
			wantOutput:    50,
			// 0 fresh + 500 cached*$0.5 + 50 output*$8 = $0.00025 + $0.0004
			wantCost: 0.00065,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &mapperState{model: DefaultCodexModel}
			var res agent.ResultEvent
			for i, usage := range tt.turns {
				params := mustJSON(t, map[string]any{
					"turn": map[string]any{
						"id":     strings.Repeat("t", i+1),
						"status": "completed",
						"usage":  usage,
					},
				})
				got := mapNotification("turn/completed", params, state, nil)
				if len(got) != 2 {
					t.Fatalf("turn %d: expected LlmCallEvent + ResultEvent, got %d", i, len(got))
				}
				llm, ok := got[0].(agent.LlmCallEvent)
				if !ok {
					t.Fatalf("turn %d: expected LlmCallEvent first, got %T", i, got[0])
				}
				if llm.InputTokens != tt.wantCallInput[i] {
					t.Fatalf("turn %d: per-call InputTokens = %d, want %d (usage %+v)", i, llm.InputTokens, tt.wantCallInput[i], usage)
				}
				r, ok := got[1].(agent.ResultEvent)
				if !ok {
					t.Fatalf("turn %d: expected ResultEvent second, got %T", i, got[1])
				}
				res = r
			}
			if res.Cost == nil {
				t.Fatal("expected cost data")
			}
			if res.Cost.InputTokens != tt.wantInput {
				t.Fatalf("accumulated InputTokens = %d, want %d", res.Cost.InputTokens, tt.wantInput)
			}
			if res.Cost.CachedInputTokens != tt.wantCached {
				t.Fatalf("accumulated CachedInputTokens = %d, want %d", res.Cost.CachedInputTokens, tt.wantCached)
			}
			if res.Cost.OutputTokens != tt.wantOutput {
				t.Fatalf("accumulated OutputTokens = %d, want %d", res.Cost.OutputTokens, tt.wantOutput)
			}
			if abs(res.Cost.TotalCostUsd-tt.wantCost) > 0.0001 {
				t.Fatalf("total cost = %.5f, want %.5f", res.Cost.TotalCostUsd, tt.wantCost)
			}
		})
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
	llm := got[0].(agent.LlmCallEvent)
	if llm.FinishReason != "failed" {
		t.Fatalf("finish reason = %q, want failed", llm.FinishReason)
	}
	res := got[1].(agent.ResultEvent)
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
	res := got[1].(agent.ResultEvent)
	if res.ErrorSubtype != "interrupted" {
		t.Fatalf("expected interrupted, got %q", res.ErrorSubtype)
	}
}

// Partial-message deltas are dropped (mirroring the Claude provider's
// stream_event drop) so each streamed token does not become its own
// "thought" activity. The full text arrives on item/completed.
func TestMapNotification_AssistantMessageDelta_Dropped(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	got := mapNotification("item/agentMessage/delta", mustJSON(t, map[string]any{"delta": "hello"}), state, nil)
	if len(got) != 0 {
		t.Fatalf("expected delta to be dropped, got %d event(s): %+v", len(got), got)
	}
}

func TestMapNotification_ReasoningDelta_Dropped(t *testing.T) {
	t.Parallel()
	state := &mapperState{}
	for _, method := range []string{"item/reasoning/textDelta", "item/reasoning/summaryTextDelta"} {
		got := mapNotification(method, mustJSON(t, map[string]any{"text": "thinking..."}), state, nil)
		if len(got) != 0 {
			t.Fatalf("%s: expected delta to be dropped, got %d event(s): %+v", method, len(got), got)
		}
	}
}

// A reasoning item/started must emit nothing: only item/completed carries
// the final summary, and the activity poster now forwards reasoning
// SystemEvents as thoughts — a started-time emission would double-post (or
// post a partial summary) for every reasoning item.
func TestMapItem_ReasoningStarted_Dropped(t *testing.T) {
	t.Parallel()
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":      "r-1",
			"type":    "reasoning",
			"summary": "partial summary at start",
		},
	})
	if got := mapNotification("item/started", params, &mapperState{}, nil); len(got) != 0 {
		t.Fatalf("expected no events for reasoning item/started, got %d: %#v", len(got), got)
	}
}

// Chain guard: a COMPLETED reasoning item must map to
// SystemEvent{Subtype: "reasoning"}, and the shared activity poster must
// forward exactly that event as a type=thought payload. Companion to
// TestMapNotification_ReasoningDelta_Dropped above, which pins the
// complete-message discipline (per-token deltas stay dropped); together they
// guarantee codex emits one thought per completed reasoning item, no spam.
func TestMapItem_ReasoningCompleted_PostsThoughtActivity(t *testing.T) {
	t.Parallel()
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":      "r-1",
			"type":    "reasoning",
			"summary": "inspecting the failing test first",
		},
	})
	got := mapNotification("item/completed", params, &mapperState{}, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	se, ok := got[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("expected SystemEvent, got %T", got[0])
	}
	if se.Subtype != "reasoning" {
		t.Fatalf("expected subtype=reasoning, got %q", se.Subtype)
	}
	if se.Message != "inspecting the failing test first" {
		t.Fatalf("unexpected message: %q", se.Message)
	}

	// Second hop: push the mapped event through a real activity poster and
	// assert the wire payload is a thought carrying the reasoning text.
	var captured atomic.Pointer[string]
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/activity") {
			body, _ := io.ReadAll(r.Body)
			s := string(body)
			captured.Store(&s)
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	poster, err := activity.New(activity.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := poster.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = poster.Stop() })

	poster.Send(context.Background(), se)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("expected reasoning SystemEvent to be posted as an activity")
	}
	body := captured.Load()
	if body == nil {
		t.Fatal("no body captured")
	}
	var wire struct {
		Activity struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"activity"`
	}
	if err := json.Unmarshal([]byte(*body), &wire); err != nil {
		t.Fatalf("decode body: %v\n%s", err, *body)
	}
	if wire.Activity.Type != "thought" {
		t.Fatalf("type = %q; want thought", wire.Activity.Type)
	}
	if wire.Activity.Content != "inspecting the failing test first" {
		t.Fatalf("content = %q; want reasoning summary", wire.Activity.Content)
	}
}

// The complete assistant message (item/completed) still emits a single
// AssistantTextEvent carrying the full text.
func TestMapNotification_AgentMessageCompleted_EmitsFullText(t *testing.T) {
	t.Parallel()
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":   "msg-1",
			"type": "agentMessage",
			"text": "the full message",
		},
	})
	got := mapNotification("item/completed", params, &mapperState{}, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	at, ok := got[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("expected AssistantTextEvent, got %T", got[0])
	}
	if at.Text != "the full message" {
		t.Fatalf("expected full text, got %q", at.Text)
	}
}

func TestMapNotification_Codex0147LifecycleNoiseDropped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		params map[string]any
	}{
		{
			method: "mcpServer/startupStatus/updated",
			params: map[string]any{"name": "af-code", "status": "starting", "threadId": "thread-1"},
		},
		{
			method: "mcpServer/startupStatus/updated",
			params: map[string]any{"name": "af-code", "status": "ready", "threadId": "thread-1"},
		},
		{
			method: "thread/settings/updated",
			params: map[string]any{"threadId": "thread-1", "threadSettings": map[string]any{}},
		},
	}
	for _, tt := range tests {
		if got := mapNotification(tt.method, mustJSON(t, tt.params), &mapperState{}, nil); len(got) != 0 {
			t.Fatalf("%s: expected lifecycle sync to be consumed, got %#v", tt.method, got)
		}
	}
}

func TestMapNotification_MCPStartupFailureRemainsVisibleWithoutRawErrorText(t *testing.T) {
	t.Parallel()
	const rawError = "transport rejected secret-bearing details"
	params := mustJSON(t, map[string]any{
		"name":          "af-code",
		"status":        "failed",
		"failureReason": "reauthenticationRequired",
		"error":         rawError,
		"threadId":      "thread-1",
	})
	got := mapNotification("mcpServer/startupStatus/updated", params, &mapperState{}, nil)
	if len(got) != 1 {
		t.Fatalf("expected one visible MCP startup failure, got %#v", got)
	}
	event, ok := got[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("expected SystemEvent, got %T", got[0])
	}
	if event.Subtype != "mcp_server_startup_failed" {
		t.Fatalf("subtype = %q, want mcp_server_startup_failed", event.Subtype)
	}
	if !strings.Contains(event.Message, "af-code") || !strings.Contains(event.Message, "reauthenticationRequired") {
		t.Fatalf("failure message omitted bounded server/reason detail: %q", event.Message)
	}
	if strings.Contains(event.Message, rawError) {
		t.Fatalf("failure message leaked raw MCP error text: %q", event.Message)
	}
}

func TestMapItem_UserMessageEchoDropped(t *testing.T) {
	t.Parallel()
	params := mustJSON(t, map[string]any{
		"item": map[string]any{
			"id":      "user-1",
			"type":    "userMessage",
			"content": []map[string]any{{"type": "text", "text": "already-known input"}},
		},
	})
	for _, method := range []string{"item/started", "item/completed"} {
		if got := mapNotification(method, params, &mapperState{}, nil); len(got) != 0 {
			t.Fatalf("%s: expected user-message echo to be dropped, got %#v", method, got)
		}
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
			"arguments": map[string]any{"id": "ENG-1"},
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
	got := calculateCostUSD(1_000_000, 500_000, 200_000, "gpt-5-codex")
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

// The cost helper takes fresh input and floors it at zero: a negative
// fresh-input value must price identically to zero, never as a negative
// dollar contribution. Deleting the clamp turns this RED.
func TestCalculateCostUSD_NegativeFreshInputFloorsAtZero(t *testing.T) {
	t.Parallel()
	floored := calculateCostUSD(0, 500, 50, DefaultCodexModel)
	got := calculateCostUSD(-100, 500, 50, DefaultCodexModel)
	if abs(got-floored) > 0.0001 {
		t.Fatalf("negative fresh input priced at %.6f, want floored %.6f", got, floored)
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
