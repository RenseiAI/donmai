package gemini

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestMapResponse_TextOnly_Final(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}`)
	turn := mapResponse(in, &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeFinal {
		t.Fatalf("outcome: want outcomeFinal, got %v", turn.outcome)
	}
	if len(turn.events) != 2 {
		t.Fatalf("events: want LLM call + text, got %d", len(turn.events))
	}
	llm, ok := turn.events[0].(agent.LlmCallEvent)
	if !ok || llm.InputTokens != 10 || llm.OutputTokens != 4 || llm.UsageSource != agent.LlmUsageProvider {
		t.Fatalf("events[0]: want provider LlmCallEvent(10/4), got %#v", turn.events[0])
	}
	if ev, ok := turn.events[1].(agent.AssistantTextEvent); !ok || ev.Text != "hello" {
		t.Fatalf("events[1]: want AssistantTextEvent(hello), got %#v", turn.events[1])
	}
	res, ok := turn.result.(agent.ResultEvent)
	if !ok {
		t.Fatalf("result: want ResultEvent, got %T", turn.result)
	}
	if !res.Success {
		t.Error("Result.Success: want true for STOP")
	}
	if res.Cost == nil {
		t.Fatal("Result.Cost: want non-nil")
	}
	if res.Cost.InputTokens != 10 || res.Cost.OutputTokens != 4 {
		t.Errorf("Cost tokens: want in=10 out=4, got in=%d out=%d", res.Cost.InputTokens, res.Cost.OutputTokens)
	}
}

func TestMapResponse_FunctionCall_Continue(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"Bash","args":{"command":"ls"}}}]}}]}`)
	turn := mapResponse(in, &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeContinue {
		t.Fatalf("outcome: want outcomeContinue, got %v", turn.outcome)
	}
	if len(turn.funcCalls) != 1 {
		t.Fatalf("funcCalls: want 1, got %d", len(turn.funcCalls))
	}
	if turn.funcCalls[0].ID != "call-1" || turn.funcCalls[0].Name != "Bash" {
		t.Errorf("funcCall: want id=call-1 name=Bash, got %#v", turn.funcCalls[0])
	}
	llm, ok := turn.events[0].(agent.LlmCallEvent)
	if !ok || llm.UsageSource != agent.LlmUsageProvider || llm.Synthetic {
		t.Fatalf("events[0]: want provider LlmCallEvent, got %#v", turn.events[0])
	}
	tu, ok := turn.events[1].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("events[1]: want ToolUseEvent, got %T", turn.events[1])
	}
	if tu.ToolName != "Bash" || tu.ToolUseID != "call-1" {
		t.Errorf("ToolUse: want Bash/call-1, got %s/%s", tu.ToolName, tu.ToolUseID)
	}
	if tu.Input["command"] != "ls" {
		t.Errorf("ToolUse.Input[command]: want ls, got %v", tu.Input["command"])
	}
	// The model turn must be recorded so the next turn carries the call.
	if len(turn.modelParts) != 1 || turn.modelParts[0].FunctionCall == nil {
		t.Fatalf("modelParts: want 1 functionCall part, got %#v", turn.modelParts)
	}
}

func TestMapResponse_FinishReasonSafety_Failure(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"finishReason":"SAFETY"}]}`)
	turn := mapResponse(in, &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeFinal {
		t.Fatalf("outcome: want outcomeFinal, got %v", turn.outcome)
	}
	res, ok := turn.result.(agent.ResultEvent)
	if !ok {
		t.Fatalf("result: want ResultEvent, got %T", turn.result)
	}
	if res.Success {
		t.Error("Result.Success: want false for SAFETY")
	}
	if !strings.Contains(res.ErrorSubtype, "SAFETY") {
		t.Errorf("ErrorSubtype: want SAFETY mention, got %q", res.ErrorSubtype)
	}
	if llm, ok := turn.events[0].(agent.LlmCallEvent); !ok || llm.FinishReason != "SAFETY" {
		t.Fatalf("want SAFETY LlmCallEvent, got %#v", turn.events)
	}
}

func TestMapResponse_PromptBlocked_Error(t *testing.T) {
	t.Parallel()
	in := []byte(`{"promptFeedback":{"blockReason":"OTHER"}}`)
	turn := mapResponse(in, &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeError {
		t.Fatalf("outcome: want outcomeError, got %v", turn.outcome)
	}
	errEv, ok := turn.result.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("result: want ErrorEvent, got %T", turn.result)
	}
	if errEv.Code != "prompt_blocked" {
		t.Errorf("Code: want prompt_blocked, got %q", errEv.Code)
	}
	if llm, ok := turn.events[0].(agent.LlmCallEvent); !ok || llm.FinishReason != "prompt_blocked_OTHER" {
		t.Fatalf("want explicit blocked LlmCallEvent, got %#v", turn.events)
	}
}

func TestMapResponse_MalformedJSON_Error(t *testing.T) {
	t.Parallel()
	turn := mapResponse([]byte(`{not-json`), &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeError {
		t.Fatalf("outcome: want outcomeError, got %v", turn.outcome)
	}
	if _, ok := turn.result.(agent.ErrorEvent); !ok {
		t.Fatalf("result: want ErrorEvent on bad JSON, got %T", turn.result)
	}
	if llm, ok := turn.events[0].(agent.LlmCallEvent); !ok || llm.FinishReason != "decode_error" {
		t.Fatalf("want decode-error LlmCallEvent, got %#v", turn.events)
	}
}

// TestMapResponse_CostAccumulatesAcrossTurns verifies the running totals
// fold across multiple turns (function-call round-trip then final).
func TestMapResponse_CostAccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()
	state := &turnState{model: "gemini-3.5-flash"}

	turn1 := mapResponse([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"Read"}}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`), state)
	if turn1.outcome != outcomeContinue {
		t.Fatalf("turn1 outcome: want outcomeContinue, got %v", turn1.outcome)
	}

	turn2 := mapResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":30}}`), state)
	res, ok := turn2.result.(agent.ResultEvent)
	if !ok {
		t.Fatalf("turn2 result: want ResultEvent, got %T", turn2.result)
	}
	if res.Cost.InputTokens != 250 {
		t.Errorf("Cost.InputTokens: want 250 (100+150), got %d", res.Cost.InputTokens)
	}
	if res.Cost.OutputTokens != 50 {
		t.Errorf("Cost.OutputTokens: want 50 (20+30), got %d", res.Cost.OutputTokens)
	}
	if res.Cost.NumTurns != 2 {
		t.Errorf("Cost.NumTurns: want 2, got %d", res.Cost.NumTurns)
	}
	// 250 in @ 1.50/M + 50 out @ 9.00/M = 0.000375 + 0.00045 = 0.000825.
	want := (250.0/1_000_000)*1.50 + (50.0/1_000_000)*9.00
	if diff := res.Cost.TotalCostUsd - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("Cost.TotalCostUsd: want %g, got %g", want, res.Cost.TotalCostUsd)
	}
}

// TestMapResponse_MaxTokens_IsNonSuccess verifies that a MAX_TOKENS
// finish reason is classified as a truncation (Success=false), NOT a
// success. A truncated response misleads the runner's acceptance gate if
// Success is true — the caller cannot distinguish a clean finish from one
// where the model ran out of budget mid-response.
func TestMapResponse_MaxTokens_IsNonSuccess(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"content":{"parts":[{"text":"partial output..."}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100}}`)
	turn := mapResponse(in, &turnState{model: "gemini-3.5-flash"})
	if turn.outcome != outcomeFinal {
		t.Fatalf("outcome: want outcomeFinal, got %v", turn.outcome)
	}
	res, ok := turn.result.(agent.ResultEvent)
	if !ok {
		t.Fatalf("result: want ResultEvent, got %T", turn.result)
	}
	if res.Success {
		t.Error("Result.Success: want false for MAX_TOKENS (truncation, not clean finish)")
	}
	if res.ErrorSubtype != "finish_MAX_TOKENS" {
		t.Errorf("ErrorSubtype: want finish_MAX_TOKENS, got %q", res.ErrorSubtype)
	}
	if len(res.Errors) == 0 || res.Errors[0] != "MAX_TOKENS" {
		t.Errorf("Errors: want [MAX_TOKENS], got %v", res.Errors)
	}
	// Text events must still be emitted even though the result is non-success.
	if len(turn.events) != 2 {
		t.Fatalf("events: want LLM call + partial text, got %d", len(turn.events))
	}
	if _, ok := turn.events[0].(agent.LlmCallEvent); !ok {
		t.Fatalf("events[0]: want LlmCallEvent, got %T", turn.events[0])
	}
	if _, ok := turn.events[1].(agent.AssistantTextEvent); !ok {
		t.Fatalf("events[1]: want AssistantTextEvent, got %T", turn.events[1])
	}
}
