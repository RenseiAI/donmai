package muse

import (
	"encoding/json"
	"strings"
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

func TestMapNotification_ApprovalModeChanged(t *testing.T) {
	// Seen live after session/start applies the requested mode: it
	// must surface as a named system event, never as unknown and
	// never terminal.
	state := newMapperState("m")
	events := mapNotification("session/approvalModeChanged", json.RawMessage(`{"sessionId":"s","viewCursor":"c"}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok || sys.Subtype != "approval_mode_changed" {
		t.Errorf("event=%+v, want approval_mode_changed SystemEvent", events[0])
	}
}

func TestMapNotification_UsageChangedIsRecordOnly(t *testing.T) {
	// usage/changed is the subscription-meter companion to
	// session/tokenUsage: recorded on the stream, never terminal,
	// carrying no counters of its own (cost still comes from the
	// cumulative.cost.usd folded out of session/tokenUsage).
	state := newMapperState("m")
	events := mapNotification("usage/changed", json.RawMessage(`{"sessionId":"s","viewCursor":"c"}`), state, nil)
	if len(events) != 1 {
		t.Fatalf("events=%v", kindsOf(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok || sys.Subtype != "subscription_usage_changed" {
		t.Errorf("event=%+v, want subscription_usage_changed SystemEvent", events[0])
	}
	if state.hasCost {
		t.Error("usage/changed must not fabricate cost; only session/tokenUsage folds cost")
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
	// item/started at rev 1 emits the single progress event for the item
	// and records the revision.
	if got := mapNotification("item/started", started(1), state, nil); len(got) != 1 {
		t.Fatalf("started produced %v, want one progress event", kindsOf(got))
	}
	// A stale item/updated at rev 1 after rev 2 applied must drop.
	mapNotification("item/updated", started(2), state, nil)
	stale := mapNotification("item/updated", started(1), state, nil)
	if len(stale) != 0 {
		t.Errorf("stale revision produced %v", kindsOf(stale))
	}
	// Completion always applies, even at a stale revision and when it
	// replaces already streamed text.
	done := mapNotification("item/completed", json.RawMessage(`{"sessionId":"s","viewCursor":"c","item":{"itemId":"i1","kind":"agentMessage","revision":1,"status":"completed","text":"bye"}}`), state, nil)
	if len(done) != 1 {
		t.Fatalf("authoritative completion produced %v", kindsOf(done))
	}
	if text, ok := done[0].(agent.AssistantTextEvent); !ok || text.Text != "bye" {
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
	if len(events) != 1 {
		t.Fatalf("tokenUsage produced %v, want one usage event", kindsOf(events))
	}
	call, ok := events[0].(agent.LlmCallEvent)
	// The step input nets the cached slice: 50 reported minus 5 cached.
	if !ok || call.InputTokens != 45 || call.OutputTokens != 20 || call.CachedInputTokens != 5 || call.ReasoningTokens != 2 || call.ObservedCostUsd == nil || *call.ObservedCostUsd != 0.5 || call.UsageSource != agent.LlmUsageProvider || call.TurnCompleted {
		t.Fatalf("step usage=%+v", events[0])
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
	if result.Cost.InputTokens != 10 || result.Cost.OutputTokens != 7 {
		t.Errorf("cost=%+v, want the turn usage override netted (11 in minus 1 cached, 7 out)", result.Cost)
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

func TestMapTokenUsage_CostDeltas(t *testing.T) {
	floatPtr := func(v float64) *float64 { return &v }
	state := newMapperState("m")
	for _, tc := range []struct {
		name string
		cost string
		want *float64
	}{
		{"unpriced", ``, nil},
		{"first price", `,"cost":{"usd":0.5}`, floatPtr(0.5)},
		{"repeated", `,"cost":{"usd":0.5}`, floatPtr(0)},
		{"lower", `,"cost":{"usd":0.25}`, floatPtr(0)},
		{"after lower", `,"cost":{"usd":0.375}`, floatPtr(0.125)},
		{"missing price", ``, nil},
		{"after missing", `,"cost":{"usd":0.5}`, floatPtr(0.125)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := mapNotification("session/tokenUsage", json.RawMessage(`{"usage":{"inputTokens":10,"outputTokens":2},"cumulative":{"promptTokens":10,"outputTokens":2`+tc.cost+`}}`), state, nil)
			if len(events) != 1 {
				t.Fatalf("events=%v, want one usage event", kindsOf(events))
			}
			call := events[0].(agent.LlmCallEvent)
			if tc.want == nil {
				if call.ObservedCostUsd != nil {
					t.Fatalf("unpriced step has cost %v", *call.ObservedCostUsd)
				}
			} else if call.ObservedCostUsd == nil || *call.ObservedCostUsd != *tc.want {
				t.Fatalf("cost=%v, want %v", call.ObservedCostUsd, *tc.want)
			}
		})
	}
}

func TestMapTokenUsage_CumulativeFallback(t *testing.T) {
	state := newMapperState("m")
	for i := 1; i <= 2; i++ {
		params, err := json.Marshal(map[string]any{"cumulative": map[string]any{
			"promptTokens": 50 * i, "outputTokens": 20 * i, "cacheReadTokens": 5 * i, "cacheWriteTokens": 3 * i,
		}})
		if err != nil {
			t.Fatal(err)
		}
		events := mapNotification("session/tokenUsage", params, state, nil)
		if len(events) != 1 {
			t.Fatalf("events=%v", kindsOf(events))
		}
		call := events[0].(agent.LlmCallEvent)
		// The cumulative prompt delta (50) nets the read delta (5).
		if call.InputTokens != 45 || call.OutputTokens != 20 || call.CachedInputTokens != 5 || call.CacheWriteTokens != 3 || call.ObservedCostUsd != nil {
			t.Fatalf("fallback=%+v", call)
		}
	}
	// Explicit zero step usage is authoritative even if totals moved.
	events := mapNotification("session/tokenUsage", json.RawMessage(`{"usage":{},"cumulative":{"promptTokens":150,"outputTokens":60}}`), state, nil)
	call := events[0].(agent.LlmCallEvent)
	if call.InputTokens != 0 || call.OutputTokens != 0 {
		t.Fatalf("explicit zero=%+v", call)
	}
	if events := mapNotification("session/tokenUsage", json.RawMessage(`{`), state, nil); len(events) != 0 {
		t.Fatalf("malformed usage=%v", events)
	}
}

// TestMapTokenUsage_NetsCacheReads pins the token accounting contract for
// the cumulative session/tokenUsage path: the provider's prompt/input
// counts fold cached reads back in (OpenAI-style), so the mapper reports
// InputTokens net of the read slice, which rides CachedInputTokens
// instead (agent.CostData). Cache writes keep their own bucket and are
// never removed from input here: only the proven-included read slice is
// netted, floored at zero.
func TestMapTokenUsage_NetsCacheReads(t *testing.T) {
	tests := []struct {
		name       string
		params     string
		wantIn     int64
		wantOut    int64
		wantCached int64
		wantWrite  int64
	}{
		{
			name:       "step usage with cacheReadTokens",
			params:     `{"cumulative":{"promptTokens":100,"outputTokens":20,"cacheReadTokens":30},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":30}}`,
			wantIn:     70,
			wantOut:    20,
			wantCached: 30,
		},
		{
			name:       "step usage with legacy cachedTokens",
			params:     `{"cumulative":{"promptTokens":100,"outputTokens":20},"usage":{"inputTokens":100,"outputTokens":20,"cachedTokens":25}}`,
			wantIn:     75,
			wantOut:    20,
			wantCached: 25,
		},
		{
			name:       "cumulative fallback nets reads",
			params:     `{"cumulative":{"promptTokens":100,"outputTokens":20,"cacheReadTokens":40}}`,
			wantIn:     60,
			wantOut:    20,
			wantCached: 40,
		},
		{
			name:       "cache writes keep their own bucket",
			params:     `{"cumulative":{"promptTokens":100,"outputTokens":20,"cacheReadTokens":30,"cacheWriteTokens":6},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":30,"cacheWriteTokens":6}}`,
			wantIn:     70,
			wantOut:    20,
			wantCached: 30,
			wantWrite:  6,
		},
		{
			name:       "reads beyond prompt floor at zero",
			params:     `{"cumulative":{"promptTokens":10,"outputTokens":2,"cacheReadTokens":25},"usage":{"inputTokens":10,"outputTokens":2,"cacheReadTokens":25}}`,
			wantIn:     0,
			wantOut:    2,
			wantCached: 25,
		},
		{
			name:    "no cache fields pass through",
			params:  `{"cumulative":{"promptTokens":12,"outputTokens":3},"usage":{"inputTokens":12,"outputTokens":3}}`,
			wantIn:  12,
			wantOut: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newMapperState("m")
			events := mapNotification("session/tokenUsage", json.RawMessage(tc.params), state, nil)
			if len(events) != 1 {
				t.Fatalf("tokenUsage produced %v, want one usage event", kindsOf(events))
			}
			call, ok := events[0].(agent.LlmCallEvent)
			if !ok {
				t.Fatalf("event is %T, want LlmCallEvent", events[0])
			}
			if call.InputTokens != tc.wantIn || call.OutputTokens != tc.wantOut || call.CachedInputTokens != tc.wantCached || call.CacheWriteTokens != tc.wantWrite {
				t.Errorf("usage=%+v, want in=%d out=%d cached=%d write=%d", call, tc.wantIn, tc.wantOut, tc.wantCached, tc.wantWrite)
			}
		})
	}
}

// TestMapTokenUsage_NetsReadsAcrossSteps pins the delta math over two
// cumulative reports: each step reports only its own fresh input.
func TestMapTokenUsage_NetsReadsAcrossSteps(t *testing.T) {
	state := newMapperState("m")
	first := mapNotification("session/tokenUsage", json.RawMessage(`{"cumulative":{"promptTokens":100,"outputTokens":20,"cacheReadTokens":30}}`), state, nil)
	second := mapNotification("session/tokenUsage", json.RawMessage(`{"cumulative":{"promptTokens":250,"outputTokens":60,"cacheReadTokens":90}}`), state, nil)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("steps produced %v and %v", kindsOf(first), kindsOf(second))
	}
	got := second[0].(agent.LlmCallEvent)
	if got.InputTokens != 90 || got.OutputTokens != 40 || got.CachedInputTokens != 60 {
		t.Errorf("second step=%+v, want in=90 out=40 cached=60", got)
	}
}

// TestMapTurnCompleted_NetsCacheReads pins the same contract for the
// turn/completed override path: a turn usage block carrying cached reads
// reports InputTokens net of them, and a turn with no cache-read field
// passes its input through untouched.
func TestMapTurnCompleted_NetsCacheReads(t *testing.T) {
	tests := []struct {
		name       string
		usage      string
		wantIn     int64
		wantCached int64
		wantWrite  int64
	}{
		{
			name:       "usage with cacheReadTokens",
			usage:      `,"usage":{"inputTokens":200,"outputTokens":40,"cacheReadTokens":150}`,
			wantIn:     50,
			wantCached: 150,
		},
		{
			name:       "usage with legacy cachedTokens",
			usage:      `,"usage":{"inputTokens":200,"outputTokens":40,"cachedTokens":60}`,
			wantIn:     140,
			wantCached: 60,
		},
		{
			name:   "usage with no cache-read field passes through",
			usage:  `,"usage":{"inputTokens":200,"outputTokens":40}`,
			wantIn: 200,
		},
		{
			name:       "cache writes keep their own bucket",
			usage:      `,"usage":{"inputTokens":200,"outputTokens":40,"cacheReadTokens":50,"cacheWriteTokens":20}`,
			wantIn:     150,
			wantCached: 50,
			wantWrite:  20,
		},
		{
			name:       "reads beyond input floor at zero",
			usage:      `,"usage":{"inputTokens":10,"outputTokens":2,"cacheReadTokens":99}`,
			wantIn:     0,
			wantCached: 99,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newMapperState("m")
			events := mapNotification("turn/completed", json.RawMessage(`{"sessionId":"s","terminal":"completed","turnId":"t1","viewCursor":"c"`+tc.usage+`}`), state, nil)
			var llm *agent.LlmCallEvent
			var result *agent.ResultEvent
			for _, ev := range events {
				switch e := ev.(type) {
				case agent.LlmCallEvent:
					llm = &e
				case agent.ResultEvent:
					result = &e
				}
			}
			if llm == nil || result == nil || result.Cost == nil {
				t.Fatalf("events=%v, want llm call plus result", kindsOf(events))
			}
			if llm.InputTokens != tc.wantIn || llm.CachedInputTokens != tc.wantCached || llm.CacheWriteTokens != tc.wantWrite {
				t.Errorf("llm=%+v, want in=%d cached=%d write=%d", llm, tc.wantIn, tc.wantCached, tc.wantWrite)
			}
			if result.Cost.InputTokens != tc.wantIn || result.Cost.CachedInputTokens != tc.wantCached || result.Cost.CacheWriteTokens != tc.wantWrite {
				t.Errorf("cost=%+v, want in=%d cached=%d write=%d", result.Cost, tc.wantIn, tc.wantCached, tc.wantWrite)
			}
		})
	}
}

// TestMapTurnCompleted_FallsBackToNettedTotals pins the no-usage path:
// without a turn usage block the result carries the session totals with
// the cached slice removed from input.
func TestMapTurnCompleted_FallsBackToNettedTotals(t *testing.T) {
	state := newMapperState("m")
	seed := mapNotification("session/tokenUsage", json.RawMessage(`{"cumulative":{"promptTokens":500,"outputTokens":100,"cacheReadTokens":400}}`), state, nil)
	if len(seed) != 1 {
		t.Fatalf("seed produced %v", kindsOf(seed))
	}
	events := mapNotification("turn/completed", json.RawMessage(`{"sessionId":"s","terminal":"completed","turnId":"t1","viewCursor":"c"}`), state, nil)
	var result *agent.ResultEvent
	for _, ev := range events {
		if r, ok := ev.(agent.ResultEvent); ok {
			r := r
			result = &r
		}
	}
	if result == nil || result.Cost == nil {
		t.Fatalf("events=%v, want a result", kindsOf(events))
	}
	if result.Cost.InputTokens != 100 || result.Cost.CachedInputTokens != 400 || result.Cost.OutputTokens != 100 {
		t.Errorf("cost=%+v, want in=100 out=100 cached=400", result.Cost)
	}
}

// TestFirstTurnShape_AllowlistsOnly pins the safe-shape contract: only
// allowlisted names, validated tokens, counts and presence flags
// survive. Raw prompt, paths, URLs, headers, environment and unknown
// keys never enter the payload.
func TestFirstTurnShape_AllowlistsOnly(t *testing.T) {
	const promptSentinel = "PROMPT-SENTINEL-evt-9f8a7b6c5d4e"
	sessionParams := map[string]any{
		"sessionId":     "sid",
		"commandId":     "cmd",
		"workspaceRoot": "/sentinel/path/9f8a7b6c5d4e",
		"modelId":       DefaultModel,
		"providerId":    ProviderID,
		"approvalMode":  string(ApprovalModePromptUnmatched),
		"config": map[string]any{"mcpServers": map[string]SessionMCPServerConfig{
			"sentinel-name": {Transport: "streamableHttp", URL: "https://sentinel.invalid/mcp", Headers: map[string]string{"Authorization": "sentinel-header"}},
		}},
		"unknownFutureKey": "sentinel-unknown",
	}
	turnParams := map[string]any{
		"sessionId": "sid",
		"commandId": "cmd",
		"input": []map[string]any{
			{"type": "text", "text": promptSentinel},
			{"type": "image", "base64Data": "sentinel-b64"},
			{"type": "teleport", "text": "sentinel-unknown-type"},
		},
		"ifBusy":          string(IfBusyQueue),
		"displayText":     promptSentinel,
		"workspaceRoots":  []string{"/sentinel/root"},
		"reasoningEffort": string(ReasoningEffortLow),
		"mysteryKey":      "sentinel-mystery",
	}
	shape := buildFirstTurnShape("session/start", sessionParams, turnParams)
	payload := encodeFirstTurnDiagnostic(shape, nil)
	for _, sentinel := range []string{promptSentinel, "sentinel-path", "sentinel.invalid", "sentinel-header", "sentinel-name", "sentinel-unknown", "sentinel-mystery", "sentinel-b64", "/sentinel/root"} {
		if strings.Contains(payload, sentinel) {
			t.Errorf("shape payload leaked %q; payload=%q", sentinel, payload)
		}
	}
	if shape.ModelID != DefaultModel {
		t.Errorf("ModelID=%q, want validated default", shape.ModelID)
	}
	if shape.ApprovalMode != string(ApprovalModePromptUnmatched) {
		t.Errorf("ApprovalMode=%q", shape.ApprovalMode)
	}
	if shape.ReasoningEffort != string(ReasoningEffortLow) {
		t.Errorf("ReasoningEffort=%q", shape.ReasoningEffort)
	}
	if shape.InputPartCount != 3 {
		t.Errorf("InputPartCount=%d, want 3", shape.InputPartCount)
	}
	wantTypes := []string{"text", "image", "unknown"}
	if len(shape.InputPartTypes) != len(wantTypes) {
		t.Fatalf("InputPartTypes=%v, want %v", shape.InputPartTypes, wantTypes)
	}
	for i, w := range wantTypes {
		if shape.InputPartTypes[i] != w {
			t.Errorf("InputPartTypes[%d]=%q, want %q", i, shape.InputPartTypes[i], w)
		}
	}
	if shape.MCPServerCount != 1 {
		t.Errorf("MCPServerCount=%d, want 1 (count, never names)", shape.MCPServerCount)
	}
	if shape.WorkspaceRootsCount != 1 {
		t.Errorf("WorkspaceRootsCount=%d, want 1", shape.WorkspaceRootsCount)
	}
	if !shape.HasDisplayText || !shape.HasWorkspaceRoot || !shape.HasConfig {
		t.Errorf("presence flags lost: %+v", shape)
	}
	for _, k := range shape.SessionKeys {
		if !allowedSessionStartKeys[k] {
			t.Errorf("session key %q not allowlisted", k)
		}
	}
	for _, k := range shape.TurnKeys {
		if !allowedTurnStartKeys[k] {
			t.Errorf("turn key %q not allowlisted", k)
		}
	}
}

// TestFirstTurnAsyncError_SanitizesFreeText pins the error side: known
// kinds travel, unknown kinds and every message/reason stay out.
func TestFirstTurnAsyncError_SanitizesFreeText(t *testing.T) {
	const sentinel = "HOST-SENTINEL-evt-9f8a7b6c5d4e"
	// Known kind survives with its flags; the sentinel message does not.
	known := &TurnError{Kind: "overloaded", Message: sentinel, Retryable: true, Quota: json.RawMessage(`{"usd":1}`)}
	classified := firstTurnAsyncError(TurnTerminalFailed, known, nil)
	payload := encodeFirstTurnDiagnostic(firstTurnRequestShape{SessionMethod: "session/start"}, classified)
	if classified.ErrorKind != "overloaded" {
		t.Errorf("ErrorKind=%q, want overloaded", classified.ErrorKind)
	}
	if strings.Contains(payload, sentinel) {
		t.Errorf("diagnostic copied host message; payload=%q", payload)
	}
	// Unknown kind collapses to unknown; the sentinel kind does not travel.
	unknown := &TurnError{Kind: sentinel, Message: sentinel, Retryable: false}
	classified = firstTurnAsyncError(TurnTerminalFailed, unknown, nil)
	payload = encodeFirstTurnDiagnostic(firstTurnRequestShape{SessionMethod: "session/start"}, classified)
	if classified.ErrorKind != "unknown" {
		t.Errorf("ErrorKind=%q, want unknown for unlisted kind", classified.ErrorKind)
	}
	if strings.Contains(payload, sentinel) {
		t.Errorf("diagnostic copied unknown kind/message; payload=%q", payload)
	}
	// Reason text never travels, only its presence.
	reason := sentinel
	classified = firstTurnAsyncError(TurnTerminalCancelled, nil, &reason)
	payload = encodeFirstTurnDiagnostic(firstTurnRequestShape{SessionMethod: "session/start"}, classified)
	if !classified.ErrorHasReason {
		t.Error("ErrorHasReason=false, want presence flag")
	}
	if strings.Contains(payload, sentinel) {
		t.Errorf("diagnostic copied reason text; payload=%q", payload)
	}
}

// TestEncodeFirstTurnDiagnostic_CapsAtTwoKiB pins the 2 KiB ceiling.
// The oversize half proves the truncation fires: it builds a payload
// whose uncapped JSON exceeds the cap, so deleting the truncation
// makes the length assertion fail.
func TestEncodeFirstTurnDiagnostic_CapsAtTwoKiB(t *testing.T) {
	shape := firstTurnRequestShape{
		SessionMethod: "session/start",
		SessionKeys:   []string{"approvalMode", "commandId", "config", "modelId", "providerId", "sessionId", "workspaceRoot"},
		TurnKeys:      []string{"commandId", "displayText", "ifBusy", "input", "sessionId", "workspaceRoots"},
	}
	payload := encodeFirstTurnDiagnostic(shape, &firstTurnDiagnosticData{ErrorKind: "overloaded", ErrorMethod: "turn/start"})
	if len(payload) > firstTurnDiagnosticMaxBytes {
		t.Errorf("payload length=%d, want <= %d", len(payload), firstTurnDiagnosticMaxBytes)
	}

	// Oversize input proves the cap truncates: without the
	// buf[:max] slice the payload stays over the ceiling.
	largeKeys := make([]string, 400)
	for i := range largeKeys {
		largeKeys[i] = "overflow-key-" + strings.Repeat("x", 32) + itoa(i%10) + strings.Repeat("y", 8)
	}
	largeTypes := make([]string, 400)
	for i := range largeTypes {
		largeTypes[i] = strings.Repeat("z", 32) + itoa(i%10)
	}
	oversize := firstTurnRequestShape{
		SessionMethod:   "session/start",
		SessionKeys:     largeKeys,
		TurnKeys:        largeKeys,
		InputPartTypes:  largeTypes,
		InputPartCount:  400,
		MCPServerCount:  9999,
		ModelID:         DefaultModel,
		ApprovalMode:    string(ApprovalModePromptUnmatched),
		ReasoningEffort: string(ReasoningEffortLow),
	}
	raw, err := json.Marshal(firstTurnDiagnosticData{Shape: oversize, ErrorKind: "overloaded", ErrorMethod: "turn/start"})
	if err != nil {
		t.Fatalf("marshal oversize: %v", err)
	}
	if len(raw) <= firstTurnDiagnosticMaxBytes {
		t.Fatalf("oversize fixture is not oversize: raw=%d, want > %d", len(raw), firstTurnDiagnosticMaxBytes)
	}
	capped := encodeFirstTurnDiagnostic(oversize, &firstTurnDiagnosticData{ErrorKind: "overloaded", ErrorMethod: "turn/start"})
	if len(capped) > firstTurnDiagnosticMaxBytes {
		t.Errorf("oversize payload length=%d, want <= %d", len(capped), firstTurnDiagnosticMaxBytes)
	}
	if len(capped) != firstTurnDiagnosticMaxBytes {
		t.Errorf("oversize payload length=%d, want exactly %d (truncated)", len(capped), firstTurnDiagnosticMaxBytes)
	}
}
