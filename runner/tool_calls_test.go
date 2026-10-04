package runner

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestRun_ToolCallsSumAcrossContinuation pins that the terminal envelope
// sums tool calls across the initial turn and a continuation turn.
func TestRun_ToolCallsSumAcrossContinuation(t *testing.T) {
	// The first turn stops early (no verdict, no pull request), so the
	// runner continues it; the continuation delivers the verdict. A qa
	// turn needs no pull request, so the run completes after one
	// continuation turn.
	res := runScripted(t, WorkTypeQAStr, false, "",
		verdictScriptTurn{toolCalls: 2, text: "Working."},
		verdictScriptTurn{toolCalls: 3, text: "Done.\nWORK_RESULT: passed"},
	)
	if res.ToolCalls != 5 {
		t.Fatalf("ToolCalls = %d; want 5 (2 initial + 3 continuation)", res.ToolCalls)
	}
}

// TestRun_ToolCallsZeroWhenNoToolCall pins that a run with no tool calls
// reports 0 on the envelope (not absent): the zero value serializes on
// the wire because agent.Result.ToolCalls has no omitempty.
func TestRun_ToolCallsZeroWhenNoToolCall(t *testing.T) {
	res := runScripted(t, WorkTypeQAStr, true, "",
		verdictScriptTurn{text: "Done.\nWORK_RESULT: passed"},
	)
	if res.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d; want 0", res.ToolCalls)
	}
	var wire map[string]any
	raw, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("marshal Result: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	v, present := wire["toolCalls"]
	if !present {
		t.Fatalf("toolCalls absent from wire JSON; want explicit 0")
	}
	if v != float64(0) {
		t.Fatalf("toolCalls = %v; want 0", v)
	}
}

// TestApplyTo_ToolCallsAccumulate pins the accumulation contract: each
// distinct turn observation contributes its own stream count exactly once.
func TestApplyTo_ToolCallsAccumulate(t *testing.T) {
	t.Parallel()
	res := &Result{}
	streamObservation{toolCalls: 2}.applyTo(res, agent.ProviderStub)
	streamObservation{toolCalls: 3}.applyTo(res, agent.ProviderStub)
	if res.ToolCalls != 5 {
		t.Fatalf("ToolCalls = %d; want 5", res.ToolCalls)
	}
}
