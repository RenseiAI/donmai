package muse

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestMapItem_RevisionAware drives the production mapper entry point
// (mapNotification) across the revision lifecycle: started records,
// stale updated drops, delta appends without bumping revision,
// completed at a higher revision renders the terminal object.
func TestMapItem_RevisionAware(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: DefaultModelID}
	raw := func(v any) (json.RawMessage, any) {
		buf, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(buf, &m)
		return buf, m
	}

	// item/started rev 1 opens; renders no assistant text yet.
	started := map[string]any{"sessionId": "s", "viewCursor": "v:1", "item": map[string]any{
		"itemId": "i1", "kind": "agentMessage", "revision": 1, "status": "inProgress", "text": "",
	}}
	p, r := raw(started)
	if evs := mapNotification("item/started", p, state, r); len(evs) != 0 {
		t.Fatalf("open agentMessage must not emit, got %v", evs)
	}

	// Seed a tracked rev-1 toolCall open first: the stale rev-1
	// re-emission below must drop against it. The stale payload
	// WOULD emit a ToolUse if applied, so the test fails when the
	// revision guard is reverted.
	seed := map[string]any{"sessionId": "s", "viewCursor": "v:1.5", "item": map[string]any{
		"itemId": "stale-tool", "kind": "toolCall", "revision": 1, "status": "inProgress",
		"tool": "write", "args": `{"path":"stale.txt"}`,
	}}
	p, r = raw(seed)
	if evs := mapNotification("item/started", p, state, r); len(evs) != 1 {
		t.Fatalf("seed open must emit ToolUse, got %v", evs)
	}
	stale := map[string]any{"sessionId": "s", "viewCursor": "v:2", "item": map[string]any{
		"itemId": "stale-tool", "kind": "toolCall", "revision": 1, "status": "inProgress",
		"tool": "write", "args": `{"path":"stale.txt"}`,
	}}
	p, r = raw(stale)
	if evs := mapNotification("item/updated", p, state, r); len(evs) != 0 {
		t.Fatalf("stale revision must drop, got %v", evs)
	}

	// item/delta appends.
	delta := map[string]any{"sessionId": "s", "viewCursor": "v:3", "itemId": "i1", "field": "text", "delta": "streamed "}
	p, r = raw(delta)
	if evs := mapNotification("item/delta", p, state, r); len(evs) != 0 {
		t.Fatalf("delta must not emit, got %v", evs)
	}

	// item/completed at rev 2 is authoritative and renders.
	completed := map[string]any{"sessionId": "s", "viewCursor": "v:4", "item": map[string]any{
		"itemId": "i1", "kind": "agentMessage", "revision": 2, "status": "completed", "text": "final text",
	}}
	p, r = raw(completed)
	evs := mapNotification("item/completed", p, state, r)
	if len(evs) != 1 {
		t.Fatalf("completed must emit one event, got %v", evs)
	}
	txt, ok := evs[0].(agent.AssistantTextEvent)
	if !ok || txt.Text != "final text" {
		t.Fatalf("completed text: %#v", evs[0])
	}

	// A LOWER completed revision after the terminal never re-renders.
	late := map[string]any{"sessionId": "s", "viewCursor": "v:5", "item": map[string]any{
		"itemId": "i1", "kind": "agentMessage", "revision": 1, "status": "completed", "text": "late stale",
	}}
	p, r = raw(late)
	if evs := mapNotification("item/completed", p, state, r); len(evs) != 0 {
		t.Fatalf("late stale completed must drop, got %v", evs)
	}
}

// TestMapItem_ToolCallEditCategory drives the production mapper for
// file-edit toolCalls: write|edit|patch surface under the filesystem
// category as ToolUse + ToolResult pairs.
func TestMapItem_ToolCallEditCategory(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"write", "edit", "patch"} {
		state := &mapperState{model: DefaultModelID}
		open := map[string]any{"sessionId": "s", "viewCursor": "v:1", "item": map[string]any{
			"itemId": "t-" + tool, "kind": "toolCall", "revision": 1, "status": "inProgress",
			"tool": tool, "args": `{"path":"f.txt"}`,
		}}
		buf, _ := json.Marshal(open)
		var m map[string]any
		_ = json.Unmarshal(buf, &m)
		evs := mapNotification("item/started", buf, state, m)
		if len(evs) != 1 {
			t.Fatalf("%s: open must emit ToolUse, got %v", tool, evs)
		}
		use, ok := evs[0].(agent.ToolUseEvent)
		if !ok {
			t.Fatalf("%s: open event is %T", tool, evs[0])
		}
		if use.ToolName != tool || use.ToolCategory != "filesystem" {
			t.Fatalf("%s: ToolUse=%+v", tool, use)
		}
		done := map[string]any{"sessionId": "s", "viewCursor": "v:2", "item": map[string]any{
			"itemId": "t-" + tool, "kind": "toolCall", "revision": 2, "status": "completed",
			"tool": tool, "args": `{"path":"f.txt"}`,
		}}
		buf, _ = json.Marshal(done)
		_ = json.Unmarshal(buf, &m)
		evs = mapNotification("item/completed", buf, state, m)
		if len(evs) != 1 {
			t.Fatalf("%s: completed must emit ToolResult, got %v", tool, evs)
		}
		res, ok := evs[0].(agent.ToolResultEvent)
		if !ok || res.IsError {
			t.Fatalf("%s: ToolResult=%#v", tool, evs[0])
		}
	}
}

// TestMapItem_SkippedKinds drives the production mapper for the two
// skipped kinds: userMessage (caller-owned input) and reminderChild
// (no turn content) never reach the stream.
func TestMapItem_SkippedKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"userMessage", "reminderChild"} {
		state := &mapperState{model: DefaultModelID}
		for _, method := range []string{"item/started", "item/completed"} {
			body := map[string]any{"sessionId": "s", "viewCursor": "v:1", "item": map[string]any{
				"itemId": "x", "kind": kind, "revision": 1, "status": "completed", "text": "should not surface",
			}}
			buf, _ := json.Marshal(body)
			var m map[string]any
			_ = json.Unmarshal(buf, &m)
			if evs := mapNotification(method, buf, state, m); len(evs) != 0 {
				t.Fatalf("%s/%s must emit nothing, got %v", kind, method, evs)
			}
		}
	}
}

// TestMapTurnCompleted_Terminals drives the production terminal mapper
// for all three terminals: completed → success, cancelled →
// interrupted, failed → failed with the host error.
func TestMapTurnCompleted_Terminals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		terminal   string
		errObj     map[string]any
		wantOK     bool
		wantSub    string
		wantErrMsg string
	}{
		{name: "completed", terminal: "completed", wantOK: true},
		{name: "cancelled", terminal: "cancelled", wantOK: false, wantSub: "interrupted"},
		{name: "failed", terminal: "failed", errObj: map[string]any{"kind": "modelFailed", "message": "boom", "retryable": true}, wantOK: false, wantSub: "modelFailed", wantErrMsg: "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := &mapperState{model: "muse-wonder-1.0", turnCount: 1}
			body := map[string]any{"sessionId": "s", "viewCursor": "v:1", "turnId": "t-" + tc.name, "terminal": tc.terminal}
			if tc.errObj != nil {
				body["error"] = tc.errObj
			}
			buf, _ := json.Marshal(body)
			var m map[string]any
			_ = json.Unmarshal(buf, &m)
			evs := mapNotification("turn/completed", buf, state, m)
			if len(evs) != 2 {
				t.Fatalf("terminal must emit LlmCall+Result, got %d", len(evs))
			}
			if _, ok := evs[0].(agent.LlmCallEvent); !ok {
				t.Fatalf("first event is %T", evs[0])
			}
			res, ok := evs[1].(agent.ResultEvent)
			if !ok {
				t.Fatalf("second event is %T", evs[1])
			}
			if res.Success != tc.wantOK {
				t.Fatalf("Success=%v", res.Success)
			}
			if !tc.wantOK {
				if res.ErrorSubtype != tc.wantSub {
					t.Fatalf("ErrorSubtype=%q want %q", res.ErrorSubtype, tc.wantSub)
				}
				if tc.wantErrMsg != "" && (len(res.Errors) == 0 || !strings.Contains(res.Errors[0], tc.wantErrMsg)) {
					t.Fatalf("Errors=%v", res.Errors)
				}
			}
			if res.ObservedTurns == nil || *res.ObservedTurns != 1 {
				t.Fatalf("ObservedTurns=%+v", res.ObservedTurns)
			}
		})
	}
}

// TestMapTurnCompleted_UsageOverride drives the usage rule:
// turn/completed.usage overrides the folded per-step usage on the
// terminal cost.
func TestMapTurnCompleted_UsageOverride(t *testing.T) {
	t.Parallel()
	state := &mapperState{model: "m"}
	step := map[string]any{
		"sessionId": "s", "viewCursor": "v:1", "turnId": "t", "modelId": "muse-wonder-1.0",
		"usage":        map[string]any{"inputTokens": 100, "outputTokens": 20, "cachedTokens": 0, "reasoningTokens": 0},
		"promptTokens": 100, "totalTokens": 120,
		"cumulative": map[string]any{
			"promptTokens": 100, "outputTokens": 20, "totalTokens": 120,
			"cost": map[string]any{"usd": 0.001, "partial": false},
		},
	}
	buf, _ := json.Marshal(step)
	var m map[string]any
	_ = json.Unmarshal(buf, &m)
	mapNotification("session/tokenUsage", buf, state, m)

	done := map[string]any{
		"sessionId": "s", "viewCursor": "v:2", "turnId": "t", "terminal": "completed",
		"usage": map[string]any{"inputTokens": 500, "outputTokens": 60, "cachedTokens": 0, "reasoningTokens": 0},
	}
	buf, _ = json.Marshal(done)
	_ = json.Unmarshal(buf, &m)
	evs := mapNotification("turn/completed", buf, state, m)
	res := evs[1].(agent.ResultEvent)
	if res.Cost.InputTokens != 600 || res.Cost.OutputTokens != 80 {
		t.Fatalf("usage override did not add the turn aggregate: %+v", res.Cost)
	}
	if res.Cost.TotalCostUsd != 0.001 {
		t.Fatalf("cost must stay the host cumulative: %+v", res.Cost)
	}
}
