package muse

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestMapNotification_SessionStartedEmitsInit(t *testing.T) {
	state := newMapperState("muse-wonder-1.0")
	events := mapNotification("session/started", json.RawMessage(`{"session":{"sessionId":"sess-1","status":"idle"}}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	init, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("event is %T, want InitEvent", events[0])
	}
	if init.SessionID != "sess-1" {
		t.Errorf("SessionID=%q, want sess-1", init.SessionID)
	}
}

func TestMapNotification_UnknownMethodSurfacesUnknown(t *testing.T) {
	state := newMapperState("m")
	events := mapNotification("something/new", json.RawMessage(`{}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok || sys.Subtype != "unknown" {
		t.Errorf("event=%+v, want unknown SystemEvent", events[0])
	}
}

func TestMapTurnCompleted_CompletedFailedCancelled(t *testing.T) {
	cases := []struct {
		name       string
		params     string
		wantOK     bool
		wantErrSub string
		wantErr    string
		terminal   bool
	}{
		{
			name:     "completed",
			params:   `{"sessionId":"s","terminal":"completed","turnId":"t1","viewCursor":"c"}`,
			wantOK:   true,
			terminal: true,
		},
		{
			name:       "cancelled",
			params:     `{"sessionId":"s","terminal":"cancelled","turnId":"t2","viewCursor":"c","reason":"stopped"}`,
			wantErrSub: "interrupted",
			wantErr:    "stopped",
			terminal:   true,
		},
		{
			name:       "failed with error",
			params:     `{"sessionId":"s","terminal":"failed","turnId":"t3","viewCursor":"c","error":{"kind":"overloaded","message":"busy","retryable":true}}`,
			wantErrSub: "overloaded",
			wantErr:    "busy",
			terminal:   true,
		},
		{
			name:     "open terminal stays non-terminal",
			params:   `{"sessionId":"s","terminal":"paused","turnId":"t4","viewCursor":"c"}`,
			terminal: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newMapperState("muse-wonder-1.0")
			events := mapNotification("turn/completed", json.RawMessage(tc.params), state, nil)
			var res *agent.ResultEvent
			for _, ev := range events {
				if r, ok := ev.(agent.ResultEvent); ok {
					r := r
					res = &r
				}
			}
			if !tc.terminal {
				if res != nil {
					t.Fatalf("open terminal produced a ResultEvent: %+v", res)
				}
				return
			}
			if res == nil {
				t.Fatal("no ResultEvent")
			}
			if res.Success != tc.wantOK {
				t.Errorf("Success=%v, want %v", res.Success, tc.wantOK)
			}
			if !tc.wantOK {
				if res.ErrorSubtype != tc.wantErrSub {
					t.Errorf("ErrorSubtype=%q, want %q", res.ErrorSubtype, tc.wantErrSub)
				}
				if len(res.Errors) != 1 || res.Errors[0] != tc.wantErr {
					t.Errorf("Errors=%v, want [%s]", res.Errors, tc.wantErr)
				}
			}
		})
	}
}

func TestMapTurnCompleted_DeduplicatesTurnID(t *testing.T) {
	state := newMapperState("m")
	params := json.RawMessage(`{"sessionId":"s","terminal":"completed","turnId":"t1","viewCursor":"c"}`)
	first := mapNotification("turn/completed", params, state, nil)
	if len(first) == 0 {
		t.Fatal("first delivery produced nothing")
	}
	if second := mapNotification("turn/completed", params, state, nil); len(second) != 0 {
		t.Errorf("duplicate turn/completed produced %v", kindsOf(second))
	}
}

func TestMapItem_RevisionRuleDropsStale(t *testing.T) {
	state := newMapperState("m")
	started := func(rev int) json.RawMessage {
		return json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i1","kind":"agentMessage","revision":` + itoa(rev) + `,"status":"inProgress","text":"hi"}}`)
	}
	// item/started at rev 1 carries no event for agentMessage (only
	// completed emits), but it records the revision.
	mapNotification("item/started", started(1), state, nil)
	// A stale item/updated at rev 1 after rev 2 applied must drop.
	mapNotification("item/updated", started(2), state, nil)
	stale := mapNotification("item/updated", started(1), state, nil)
	if len(stale) != 0 {
		t.Errorf("stale revision produced %v", kindsOf(stale))
	}
	// Completion always applies, even at a stale revision.
	done := mapNotification("item/completed", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i1","kind":"agentMessage","revision":1,"status":"completed","text":"hi"}}`), state, nil)
	if len(done) != 1 {
		t.Fatalf("authoritative completion produced %v", kindsOf(done))
	}
	if text, ok := done[0].(agent.AssistantTextEvent); !ok || text.Text != "hi" {
		t.Errorf("completion event=%+v", done[0])
	}
}

func TestMapItem_SkipsUserMessageAndReminder(t *testing.T) {
	state := newMapperState("m")
	for _, kind := range []string{"userMessage", "reminderChild"} {
		events := mapNotification("item/completed", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i","kind":"`+kind+`","revision":1,"status":"completed","text":"prompt"}}`), state, nil)
		if len(events) != 0 {
			t.Errorf("kind %s produced %v", kind, kindsOf(events))
		}
	}
}

func TestMapItem_ToolCallStartedCompleted(t *testing.T) {
	state := newMapperState("m")
	started := mapNotification("item/started", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"inProgress","tool":"edit","args":"{\"path\":\"a\"}"}}`), state, nil)
	if len(started) != 1 {
		t.Fatalf("started produced %v", kindsOf(started))
	}
	use, ok := started[0].(agent.ToolUseEvent)
	if !ok || use.ToolName != "edit" || use.ToolCategory != "filesystem" {
		t.Errorf("ToolUse=%+v", started[0])
	}
	done := mapNotification("item/completed", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"t1","kind":"toolCall","revision":2,"status":"completed","tool":"edit","visibleOutput":"ok"}}`), state, nil)
	if len(done) != 1 {
		t.Fatalf("completed produced %v", kindsOf(done))
	}
	res, ok := done[0].(agent.ToolResultEvent)
	if !ok || res.Content != "ok" || res.IsError {
		t.Errorf("ToolResult=%+v", done[0])
	}
}

func TestMapItem_UnknownKindRendersGenerically(t *testing.T) {
	state := newMapperState("m")
	events := mapNotification("item/completed", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i","kind":"fancyNew","revision":1,"status":"completed","fallbackText":"shiny"}}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok || sys.Subtype != "unknown_item" {
		t.Errorf("event=%+v, want unknown_item", events[0])
	}
}

func TestMapItem_DeltaNeverEmits(t *testing.T) {
	state := newMapperState("m")
	events := mapNotification("item/delta", json.RawMessage(`{"sessionId":"s","viewCursor":"c","itemId":"i1","field":"text","delta":"hel"}`), state, nil)
	if len(events) != 0 {
		t.Errorf("delta produced %v", kindsOf(events))
	}
}

func TestMapTokenUsage_AccumulatesCost(t *testing.T) {
	state := newMapperState("muse-wonder-1.0")
	events := mapNotification("session/tokenUsage", json.RawMessage(`{
		"sessionId":"s","turnId":"t","viewCursor":"c","modelId":"muse-wonder-full-1.0",
		"usage":{"cachedTokens":5,"inputTokens":50,"outputTokens":20,"reasoningTokens":2},
		"promptTokens":50,"totalTokens":70,
		"cumulative":{"outputTokens":20,"promptTokens":50,"totalTokens":70,"cost":{"partial":false,"usd":0.5}}
	}`), state, nil)
	if len(events) != 0 {
		t.Errorf("tokenUsage produced %v (must fold silently)", kindsOf(events))
	}
	if !state.hasCost || state.totalCostUSD != 0.5 {
		t.Errorf("cost not folded: %+v", state)
	}
	if state.model != "muse-wonder-full-1.0" {
		t.Errorf("model=%q, want the usage-reported model", state.model)
	}
	// turn/completed with its own usage block overrides the folded counters.
	res := mapNotification("turn/completed", json.RawMessage(`{"sessionId":"s","terminal":"completed","turnId":"t","viewCursor":"c","usage":{"cachedTokens":1,"inputTokens":11,"outputTokens":7,"reasoningTokens":0}}`), state, nil)
	var result *agent.ResultEvent
	for _, ev := range res {
		if r, ok := ev.(agent.ResultEvent); ok {
			r := r
			result = &r
		}
	}
	if result == nil || result.ObservedCostUsd == nil || *result.ObservedCostUsd != 0.5 {
		t.Errorf("result=%+v, want observed cost 0.5", result)
	}
	if result.Cost.InputTokens != 11 || result.Cost.OutputTokens != 7 {
		t.Errorf("cost=%+v, want the turn usage override (11 in, 7 out)", result.Cost)
	}
}

func TestMapNotification_ViewGapIsFatal(t *testing.T) {
	state := newMapperState("m")
	events := mapNotification("view/gap", json.RawMessage(`{"sessionId":"s","after":"c1","next":"c9"}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	errEv, ok := events[0].(agent.ErrorEvent)
	if !ok || errEv.Code != "view_gap" {
		t.Errorf("event=%+v, want view_gap ErrorEvent", events[0])
	}
	if state.gap == nil {
		t.Error("mapper did not record the gap")
	}
	// While the gap is open, non-terminal item events drop.
	held := mapNotification("item/started", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i","kind":"agentMessage","revision":1,"status":"inProgress","text":"x"}}`), state, nil)
	if len(held) != 0 {
		t.Errorf("gap-open item event produced %v", kindsOf(held))
	}
}

func itoa(i int) string {
	if i < 0 || i > 9 {
		panic("itoa: single digit only")
	}
	return string([]byte{byte('0' + i)})
}

func TestMapTurnCompleted_UnpricedHostReportsNoCost(t *testing.T) {
	state := newMapperState("muse-wonder-1.0")
	// No session/tokenUsage with a cost block ever arrived (unpriced
	// subscription host) and turn/completed carries no usage: the
	// result honestly reports no cost instead of a fabricated zero.
	events := mapNotification("turn/completed", json.RawMessage(`{"sessionId":"s","terminal":"completed","turnId":"t","viewCursor":"c"}`), state, nil)
	var result *agent.ResultEvent
	for _, ev := range events {
		if r, ok := ev.(agent.ResultEvent); ok {
			r := r
			result = &r
		}
	}
	if result == nil {
		t.Fatal("no ResultEvent")
	}
	if result.ObservedCostUsd != nil {
		t.Errorf("ObservedCostUsd=%v, want nil (host never priced a leg)", *result.ObservedCostUsd)
	}
	if result.Cost.TotalCostUsd != 0 {
		t.Errorf("TotalCostUsd=%v, want 0", result.Cost.TotalCostUsd)
	}
}
