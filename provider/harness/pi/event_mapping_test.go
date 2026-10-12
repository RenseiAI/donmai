package pi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

// TestMapEvent_ResponseCommand is table-driven over the "response" event
// type's `command` discriminant. get_state and get_entries carry payload the
// caller needs; every other command response is a plain ack. Before this
// test's fix, get_entries fell through the same `return nil, false` path as
// an ack and its reply vanished with no observable event — a bare Resume (no
// follow-up prompt/steer) would then produce exactly one InitEvent and go
// silent forever, giving the caller no signal the cursor-replay round trip
// even completed. See event_mapping.go's `case "response":` and pi.go's
// Resume doc comment.
func TestMapEvent_ResponseCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		st         *mapperState
		fields     map[string]any
		wantEvents []agent.Event
		wantTerm   bool
		wantSessID string
		wantInit   bool // st.initEmitted after the call
	}{
		{
			name: "get_state first response emits InitEvent and resolves session id",
			st:   &mapperState{},
			fields: map[string]any{
				"command": "get_state",
				"data":    map[string]any{"sessionId": "ses_123"},
			},
			wantEvents: []agent.Event{agent.InitEvent{SessionID: "ses_123", Raw: "line"}},
			wantSessID: "ses_123",
			wantInit:   true,
		},
		{
			name: "get_state response after init only updates session id, no second InitEvent",
			st:   &mapperState{initEmitted: true},
			fields: map[string]any{
				"command": "get_state",
				"data":    map[string]any{"sessionId": "ses_456"},
			},
			wantEvents: nil,
			wantSessID: "ses_456",
			wantInit:   true,
		},
		{
			name: "get_entries response is routed as a SystemEvent, not dropped",
			st:   &mapperState{initEmitted: true, sessionID: "ses_789"},
			fields: map[string]any{
				"command": "get_entries",
				"data":    map[string]any{"entries": []any{"e1", "e2"}},
			},
			wantEvents: []agent.Event{agent.SystemEvent{Subtype: "get_entries", Raw: "line"}},
			wantSessID: "ses_789", // unchanged — get_entries carries no session id
			wantInit:   true,
		},
		{
			name: "unrecognized command response is a silent ack (unchanged behavior)",
			st:   &mapperState{initEmitted: true, sessionID: "ses_789"},
			fields: map[string]any{
				"command": "set_thinking_level",
			},
			wantEvents: nil,
			wantSessID: "ses_789",
			wantInit:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := rawEvent{Type: "response", Fields: tt.fields, Line: []byte("line")}
			gotEvents, gotTerm := mapEvent(ev, tt.st)

			if len(gotEvents) != len(tt.wantEvents) {
				t.Fatalf("events = %#v, want %#v", gotEvents, tt.wantEvents)
			}
			for i := range gotEvents {
				if gotEvents[i] != tt.wantEvents[i] {
					t.Errorf("events[%d] = %#v, want %#v", i, gotEvents[i], tt.wantEvents[i])
				}
			}
			if gotTerm != tt.wantTerm {
				t.Errorf("terminal = %v, want %v", gotTerm, tt.wantTerm)
			}
			if tt.st.sessionID != tt.wantSessID {
				t.Errorf("st.sessionID = %q, want %q", tt.st.sessionID, tt.wantSessID)
			}
			if tt.st.initEmitted != tt.wantInit {
				t.Errorf("st.initEmitted = %v, want %v", tt.st.initEmitted, tt.wantInit)
			}
		})
	}
}

// TestMapEvent_MessageEndProviderError pins that an assistant message which
// ended on a provider error (stopReason "error") surfaces the provider-error
// observation after any buffered text, and that every other stop reason does
// not.
func TestMapEvent_MessageEndProviderError(t *testing.T) {
	t.Parallel()

	providerError := func(detail string, msg map[string]any) agent.Event {
		return agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: detail, Upstream: agent.ParseUpstreamError(msg), Raw: "line"}
	}
	tests := []struct {
		name       string
		buffered   string
		message    map[string]any
		wantEvents []agent.Event
	}{
		{
			name:       "error with the provider's message and no text",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "503 Service Unavailable"},
			wantEvents: []agent.Event{providerError("503 Service Unavailable", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "503 Service Unavailable"})},
		},
		{
			name:     "error after partial text keeps the text first",
			buffered: "Let me check",
			message:  map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "504 Gateway Timeout"},
			wantEvents: []agent.Event{
				agent.AssistantTextEvent{Text: "Let me check", Raw: "line"},
				providerError("504 Gateway Timeout", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "504 Gateway Timeout"}),
			},
		},
		{
			name:       "error without a message still names a provider error",
			message:    map[string]any{"role": "assistant", "stopReason": "error"},
			wantEvents: []agent.Event{providerError("model provider error", map[string]any{"role": "assistant", "stopReason": "error"})},
		},
		{
			name:       "non-retryable gateway failure carries the marker",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "400 invalid parameters", "error": map[string]any{"isRetryable": false}},
			wantEvents: []agent.Event{providerError("400 invalid parameters"+agent.ProviderErrorNotRetryableSuffix, map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "400 invalid parameters", "error": map[string]any{"isRetryable": false}})},
		},
		{
			name:       "diagnostic status marks a 400 as non-retryable",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "request failed", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(400)}}}},
			wantEvents: []agent.Event{providerError("request failed"+agent.ProviderErrorNotRetryableSuffix, map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "request failed", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(400)}}}})},
		},
		{
			name:       "a 503 stays retryable",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "503 Service Unavailable"},
			wantEvents: []agent.Event{providerError("503 Service Unavailable", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "503 Service Unavailable"})},
		},
		{
			name:       "a 408 diagnostic status stays retryable",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "request timed out", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(408)}}}},
			wantEvents: []agent.Event{providerError("request timed out", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "request timed out", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(408)}}}})},
		},
		{
			name:       "a 409 status stays retryable",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "conflict", "statusCode": float64(409)},
			wantEvents: []agent.Event{providerError("conflict", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "conflict", "statusCode": float64(409)})},
		},
		{
			name:       "a context overflow with a 400 status is not marked",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "prompt is too long: 213456 tokens > 200000 maximum", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(400)}}}},
			wantEvents: []agent.Event{providerError("prompt is too long: 213456 tokens > 200000 maximum", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "prompt is too long: 213456 tokens > 200000 maximum", "diagnostics": []any{map[string]any{"details": map[string]any{"status": float64(400)}}}})},
		},
		{
			name:       "a context overflow flagged not retryable is not marked",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "400 context_length_exceeded", "error": map[string]any{"isRetryable": false}},
			wantEvents: []agent.Event{providerError("400 context_length_exceeded", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "400 context_length_exceeded", "error": map[string]any{"isRetryable": false}})},
		},
		{
			name:       "a network error with a port and no structured status is not marked",
			message:    map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "connect ECONNREFUSED 10.0.0.12:443"},
			wantEvents: []agent.Event{providerError("connect ECONNREFUSED 10.0.0.12:443", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "connect ECONNREFUSED 10.0.0.12:443"})},
		},
		{
			name:       "a clean stop is only text",
			buffered:   "Done.",
			message:    map[string]any{"role": "assistant", "stopReason": "stop"},
			wantEvents: []agent.Event{agent.AssistantTextEvent{Text: "Done.", Raw: "line"}},
		},
		{
			name:    "an aborted message is not a provider error",
			message: map[string]any{"role": "assistant", "stopReason": "aborted"},
		},
		{
			name:    "a non-assistant message is ignored",
			message: map[string]any{"role": "toolResult", "stopReason": "error"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := &mapperState{initEmitted: true}
			st.textBuf.WriteString(tt.buffered)
			ev := rawEvent{Type: "message_end", Fields: map[string]any{"message": tt.message}, Line: []byte("line")}
			gotEvents, gotTerm := mapEvent(ev, st)
			if gotTerm {
				t.Errorf("terminal = true; a message end never ends the turn")
			}
			if len(gotEvents) != len(tt.wantEvents) {
				t.Fatalf("events = %#v, want %#v", gotEvents, tt.wantEvents)
			}
			for i := range gotEvents {
				if !reflect.DeepEqual(gotEvents[i], tt.wantEvents[i]) {
					t.Errorf("events[%d] = %#v, want %#v", i, gotEvents[i], tt.wantEvents[i])
				}
			}
		})
	}
}

func TestObservedPiTurnCostAndMissingTerminalCost(t *testing.T) {
	st := &mapperState{}
	turn := func(cost map[string]any) agent.LlmCallEvent {
		t.Helper()
		events, terminal := mapEvent(rawEvent{
			Type: "turn_end",
			Fields: map[string]any{"message": map[string]any{
				"provider": "fixture-surface", "model": "fixture-model",
				"usage": map[string]any{"input": float64(3), "output": float64(2), "cost": cost},
			}},
		}, st)
		if terminal || len(events) != 1 {
			t.Fatalf("turn mapping: terminal=%v events=%d", terminal, len(events))
		}
		call, ok := events[0].(agent.LlmCallEvent)
		if !ok || !call.TurnCompleted {
			t.Fatalf("native completed turn missing: %#v", events[0])
		}
		return call
	}
	zero := turn(map[string]any{"total": float64(0)})
	if zero.ObservedCostUsd == nil || *zero.ObservedCostUsd != 0 {
		t.Fatalf("reported zero lost: %+v", zero)
	}
	missing := turn(map[string]any{})
	if missing.ObservedCostUsd != nil {
		t.Fatal("missing Pi price became observed zero")
	}
	events, terminal := mapEvent(rawEvent{Type: "agent_settled", Fields: map[string]any{}}, st)
	if !terminal || len(events) != 1 {
		t.Fatalf("settled mapping: terminal=%v events=%d", terminal, len(events))
	}
	result := events[0].(agent.ResultEvent)
	if result.ObservedTurns == nil || *result.ObservedTurns != 2 || result.ObservedCostUsd != nil {
		t.Fatalf("partial observed costs became authoritative total: %+v", result)
	}
	turn(map[string]any{"total": float64(1)})
	if *result.ObservedTurns != 2 {
		t.Fatal("emitted cumulative turn observation mutated on continuation")
	}
}

func TestMapEvent_MessageEndProviderError_Upstream(t *testing.T) {
	t.Parallel()

	st := &mapperState{initEmitted: true}
	ev := rawEvent{Type: "message_end", Fields: map[string]any{"message": map[string]any{
		"role": "assistant", "stopReason": "error", "errorMessage": "Rate limited: quota exhausted",
		"statusCode": float64(429), "error": map[string]any{"code": "usage_limit"},
		"diagnostics": []any{map[string]any{"details": map[string]any{"resetsAt": float64(1777673400)}}},
	}}, Line: []byte("line")}
	got, term := mapEvent(ev, st)
	if term {
		t.Fatal("terminal = true; a message end never ends the turn")
	}
	if len(got) != 1 {
		t.Fatalf("events = %#v, want 1 provider-error event", got)
	}
	sys, ok := got[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", got[0])
	}
	if sys.Subtype != agent.SystemSubtypeProviderError {
		t.Errorf("Subtype = %q, want provider_error", sys.Subtype)
	}
	if sys.Upstream == nil {
		t.Fatal("Upstream is nil, want the endpoint's structured error")
	}
	if sys.Upstream.HTTPStatus != 429 {
		t.Errorf("Upstream.HTTPStatus = %d, want 429", sys.Upstream.HTTPStatus)
	}
	if sys.Upstream.ProviderCode != "usage_limit" {
		t.Errorf("Upstream.ProviderCode = %q, want usage_limit", sys.Upstream.ProviderCode)
	}
	if sys.Upstream.ProviderMessage != "Rate limited: quota exhausted" {
		t.Errorf("Upstream.ProviderMessage = %q", sys.Upstream.ProviderMessage)
	}
	if sys.Upstream.ResetAt != "1777673400" {
		t.Errorf("Upstream.ResetAt = %q, want 1777673400", sys.Upstream.ResetAt)
	}
}

// TestMapEvent_TurnEndCacheBuckets pins that pi's cache buckets survive the
// mapper. pi reports usage as {input, output, cacheRead, cacheWrite} with
// input already excluding both cache classes; before this mapping only input
// and output were read, so every cache read and write a pi session made was
// dropped before it reached the runner, the budget meter or the status post.
// Each turn's LlmCallEvent must carry its own buckets, and the terminal
// ResultEvent's Cost must carry their whole-session sums.
func TestMapEvent_TurnEndCacheBuckets(t *testing.T) {
	t.Parallel()

	type usage = map[string]any
	tests := []struct {
		name         string
		suppressCost bool
		turns        []usage
		wantCalls    []agent.LlmCallEvent
		wantCost     agent.CostData
	}{
		{
			name: "cache reads accumulate across turns",
			turns: []usage{
				{"input": float64(1200), "output": float64(80), "cacheRead": float64(48000), "cacheWrite": float64(0), "cost": usage{"total": 0.01}},
				{"input": float64(1200), "output": float64(80), "cacheRead": float64(48000), "cacheWrite": float64(0), "cost": usage{"total": 0.02}},
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 1200, OutputTokens: 80, CachedInputTokens: 48000},
				{InputTokens: 1200, OutputTokens: 80, CachedInputTokens: 48000},
			},
			wantCost: agent.CostData{InputTokens: 2400, OutputTokens: 160, CachedInputTokens: 96000, TotalCostUsd: 0.03, NumTurns: 2},
		},
		{
			name: "a cache write then a read of the written prefix",
			turns: []usage{
				{"input": float64(500), "output": float64(40), "cacheRead": float64(0), "cacheWrite": float64(3000)},
				{"input": float64(200), "output": float64(20), "cacheRead": float64(3000), "cacheWrite": float64(0)},
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 500, OutputTokens: 40, CacheWriteTokens: 3000},
				{InputTokens: 200, OutputTokens: 20, CachedInputTokens: 3000},
			},
			wantCost: agent.CostData{InputTokens: 700, OutputTokens: 60, CachedInputTokens: 3000, CacheWriteTokens: 3000, NumTurns: 2},
		},
		{
			name: "token-suffixed spellings are read too",
			turns: []usage{
				{"inputTokens": float64(10), "outputTokens": float64(5), "cacheReadTokens": float64(90), "cacheWriteTokens": float64(7)},
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 90, CacheWriteTokens: 7},
			},
			wantCost: agent.CostData{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 90, CacheWriteTokens: 7, NumTurns: 1},
		},
		{
			name:         "an unpriced injected lane keeps cache usage without a dollar total",
			suppressCost: true,
			turns: []usage{
				{"input": float64(100), "output": float64(50), "cacheRead": float64(4000), "cacheWrite": float64(0), "cost": usage{"total": float64(0)}},
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 4000},
			},
			wantCost: agent.CostData{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 4000, NumTurns: 1},
		},
		{
			name: "a turn without cache buckets reports none",
			turns: []usage{
				{"input": float64(3), "output": float64(2)},
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 3, OutputTokens: 2},
			},
			wantCost: agent.CostData{InputTokens: 3, OutputTokens: 2, NumTurns: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := &mapperState{suppressCost: tt.suppressCost}
			for i, u := range tt.turns {
				events, terminal := mapEvent(rawEvent{
					Type:   "turn_end",
					Fields: map[string]any{"message": map[string]any{"role": "assistant", "usage": u}},
				}, st)
				if terminal || len(events) != 1 {
					t.Fatalf("turn %d: terminal=%v events=%d; want one non-terminal event", i, terminal, len(events))
				}
				call, ok := events[0].(agent.LlmCallEvent)
				if !ok {
					t.Fatalf("turn %d: event %T; want LlmCallEvent", i, events[0])
				}
				want := tt.wantCalls[i]
				if call.InputTokens != want.InputTokens || call.OutputTokens != want.OutputTokens ||
					call.CachedInputTokens != want.CachedInputTokens || call.CacheWriteTokens != want.CacheWriteTokens {
					t.Errorf("turn %d: tokens in=%d out=%d cacheRead=%d cacheWrite=%d; want in=%d out=%d cacheRead=%d cacheWrite=%d",
						i, call.InputTokens, call.OutputTokens, call.CachedInputTokens, call.CacheWriteTokens,
						want.InputTokens, want.OutputTokens, want.CachedInputTokens, want.CacheWriteTokens)
				}
			}
			events, terminal := mapEvent(rawEvent{Type: "agent_settled", Fields: map[string]any{}}, st)
			if !terminal || len(events) != 1 {
				t.Fatalf("settle: terminal=%v events=%d; want one terminal event", terminal, len(events))
			}
			res, ok := events[0].(agent.ResultEvent)
			if !ok || res.Cost == nil {
				t.Fatalf("settle: %#v; want a ResultEvent carrying Cost", events[0])
			}
			got := *res.Cost
			// The dollar sum is float arithmetic; compare it with a tolerance
			// and the token classes exactly.
			if diff := got.TotalCostUsd - tt.wantCost.TotalCostUsd; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("terminal TotalCostUsd = %v; want %v", got.TotalCostUsd, tt.wantCost.TotalCostUsd)
			}
			got.TotalCostUsd = tt.wantCost.TotalCostUsd
			if got != tt.wantCost {
				t.Errorf("terminal Cost = %+v; want %+v", got, tt.wantCost)
			}
		})
	}
}

// TestMapEvent_ToolResultNestedUsage pins the ToolResultMessage.usage shape:
// a tool_execution_end whose result carries nested LLM usage (a subagent
// run, a compaction or branch summary the tool performed) surfaces that
// spend as its own per-call LlmCallEvent behind the ToolResultEvent, and
// folds it into the terminal Cost. pi's own session totals
// (get_session_stats) count this usage alongside the turn's assistant
// usage; before this mapping the result's usage was never read, so every
// cache token a nested run spent — reads and writes alike — was dropped
// before the runner, the budget meter and the status post.
func TestMapEvent_ToolResultNestedUsage(t *testing.T) {
	t.Parallel()

	toolEnd := func(result map[string]any) rawEvent {
		return rawEvent{Type: "tool_execution_end", Fields: map[string]any{
			"toolName": "task", "toolCallId": "call-1", "result": result,
		}}
	}
	turnEnd := func(usage map[string]any) rawEvent {
		return rawEvent{Type: "turn_end", Fields: map[string]any{
			"message": map[string]any{"role": "assistant", "usage": usage},
		}}
	}
	type usage = map[string]any
	tests := []struct {
		name string
		// events fed through one mapperState, in order
		feeds []rawEvent
		// wantCalls are the LlmCallEvents in emitted order (turn usage
		// first, then one per nested result usage)
		wantCalls []agent.LlmCallEvent
		wantCost  agent.CostData
	}{
		{
			name: "nested cache reads and writes ride their own buckets",
			feeds: []rawEvent{
				turnEnd(usage{"input": float64(100), "output": float64(10), "cacheRead": float64(1000), "cacheWrite": float64(50), "cost": usage{"total": 0.01}}),
				toolEnd(map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "done"}},
					"usage":   usage{"input": float64(20), "output": float64(5), "cacheRead": float64(9000), "cacheWrite": float64(400), "cost": usage{"total": 0.005}},
				}),
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 1000, CacheWriteTokens: 50},
				{InputTokens: 20, OutputTokens: 5, CachedInputTokens: 9000, CacheWriteTokens: 400},
			},
			wantCost: agent.CostData{InputTokens: 120, OutputTokens: 15, CachedInputTokens: 10000, CacheWriteTokens: 450, TotalCostUsd: 0.015, NumTurns: 2},
		},
		{
			name: "a result without nested usage emits no call",
			feeds: []rawEvent{
				turnEnd(usage{"input": float64(3), "output": float64(2)}),
				toolEnd(map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}),
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 3, OutputTokens: 2},
			},
			wantCost: agent.CostData{InputTokens: 3, OutputTokens: 2, NumTurns: 1},
		},
		{
			name: "an all-zero nested usage emits no call",
			feeds: []rawEvent{
				turnEnd(usage{"input": float64(3), "output": float64(2)}),
				toolEnd(map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "ok"}},
					"usage":   usage{"input": float64(0), "output": float64(0), "cacheRead": float64(0), "cacheWrite": float64(0)},
				}),
			},
			wantCalls: []agent.LlmCallEvent{
				{InputTokens: 3, OutputTokens: 2},
			},
			wantCost: agent.CostData{InputTokens: 3, OutputTokens: 2, NumTurns: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := &mapperState{}
			var calls []agent.LlmCallEvent
			for i, feed := range tt.feeds {
				events, terminal := mapEvent(feed, st)
				if terminal {
					t.Fatalf("feed %d: terminal=true; neither turn_end nor tool_execution_end ends the turn", i)
				}
				for _, ev := range events {
					if call, ok := ev.(agent.LlmCallEvent); ok {
						calls = append(calls, call)
					}
				}
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("calls = %+v; want %+v", calls, tt.wantCalls)
			}
			for i := range calls {
				want := tt.wantCalls[i]
				got := calls[i]
				if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens ||
					got.CachedInputTokens != want.CachedInputTokens || got.CacheWriteTokens != want.CacheWriteTokens {
					t.Errorf("call %d = %+v; want %+v", i, got, want)
				}
			}
			events, terminal := mapEvent(rawEvent{Type: "agent_settled", Fields: map[string]any{}}, st)
			if !terminal || len(events) != 1 {
				t.Fatalf("settle: terminal=%v events=%d; want one terminal event", terminal, len(events))
			}
			res, ok := events[0].(agent.ResultEvent)
			if !ok || res.Cost == nil {
				t.Fatalf("settle: %#v; want a ResultEvent carrying Cost", events[0])
			}
			got := *res.Cost
			if diff := got.TotalCostUsd - tt.wantCost.TotalCostUsd; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("terminal TotalCostUsd = %v; want %v", got.TotalCostUsd, tt.wantCost.TotalCostUsd)
			}
			got.TotalCostUsd = tt.wantCost.TotalCostUsd
			if got != tt.wantCost {
				t.Errorf("terminal Cost = %+v; want %+v", got, tt.wantCost)
			}
		})
	}
}

// TestMapEvent_TurnEndToolResultsUsage pins the turn_end.toolResults shape:
// the turn_end event carries the turn's tool results alongside the assistant
// message, and any result carrying nested LLM usage (ToolResultMessage.usage
// — a subagent run the tool performed) surfaces as its own per-call event
// and folds into the terminal Cost. Same spend class as
// TestMapEvent_ToolResultNestedUsage, observed on the turn envelope rather
// than the live tool_execution_end line.
func TestMapEvent_TurnEndToolResultsUsage(t *testing.T) {
	t.Parallel()

	st := &mapperState{}
	events, terminal := mapEvent(rawEvent{Type: "turn_end", Fields: map[string]any{
		"message": map[string]any{"role": "assistant", "usage": map[string]any{
			"input": float64(100), "output": float64(10),
			"cacheRead": float64(1000), "cacheWrite": float64(50),
			"cost": map[string]any{"total": 0.01},
		}},
		"toolResults": []any{
			map[string]any{
				"role": "toolResult", "toolCallId": "call-1", "toolName": "task",
				"usage": map[string]any{
					"input": float64(20), "output": float64(5),
					"cacheRead": float64(9000), "cacheWrite": float64(400),
					"cost": map[string]any{"total": 0.005},
				},
			},
			map[string]any{
				"role": "toolResult", "toolCallId": "call-2", "toolName": "bash",
				"content": []any{map[string]any{"type": "text", "text": "ok"}},
				// No usage: an ordinary result contributes no spend.
			},
		},
	}}, st)
	if terminal {
		t.Fatal("terminal=true; turn_end never ends the turn")
	}
	if len(events) != 2 {
		t.Fatalf("events = %d; want the turn call plus one nested-usage call", len(events))
	}
	turn, ok := events[0].(agent.LlmCallEvent)
	if !ok || turn.InputTokens != 100 || turn.CachedInputTokens != 1000 || turn.CacheWriteTokens != 50 {
		t.Fatalf("turn call = %+v; want the assistant usage", events[0])
	}
	nested, ok := events[1].(agent.LlmCallEvent)
	if !ok || nested.InputTokens != 20 || nested.CachedInputTokens != 9000 || nested.CacheWriteTokens != 400 {
		t.Fatalf("nested call = %+v; want the tool result usage", events[1])
	}
	if nested.TurnCompleted {
		t.Error("nested call TurnCompleted=true; only the turn's own call completes it")
	}

	settled, terminal := mapEvent(rawEvent{Type: "agent_settled", Fields: map[string]any{}}, st)
	if !terminal || len(settled) != 1 {
		t.Fatalf("settle: terminal=%v events=%d; want one terminal event", terminal, len(settled))
	}
	res, ok := settled[0].(agent.ResultEvent)
	if !ok || res.Cost == nil {
		t.Fatalf("settle: %#v; want a ResultEvent carrying Cost", settled[0])
	}
	want := agent.CostData{InputTokens: 120, OutputTokens: 15, CachedInputTokens: 10000, CacheWriteTokens: 450, TotalCostUsd: 0.015, NumTurns: 2}
	got := *res.Cost
	if diff := got.TotalCostUsd - want.TotalCostUsd; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("terminal TotalCostUsd = %v; want %v", got.TotalCostUsd, want.TotalCostUsd)
	}
	got.TotalCostUsd = want.TotalCostUsd
	if got != want {
		t.Fatalf("terminal Cost = %+v; want %+v", got, want)
	}
}

// TestMapEvent_CompactionEndEmitsContextUsageNotice pins the pi leg of the
// typed context notice: a native compaction_end event (real RPC shape, per
// the RPC reference) emits the observability SystemEvent AND the
// ContextUsageEvent the usage series appends. The pre-compaction token
// count rides as input-side consumption; the post-compaction estimate is a
// heuristic, never a window measurement.
func TestMapEvent_CompactionEndEmitsContextUsageNotice(t *testing.T) {
	t.Parallel()
	st := &mapperState{}
	ev := rawEvent{Type: "compaction_end", Fields: map[string]any{
		"reason": "threshold",
		"result": map[string]any{
			"summary":              "Summary of conversation...",
			"firstKeptEntryId":     "abc123",
			"tokensBefore":         float64(150000),
			"estimatedTokensAfter": float64(32000),
			"usage": map[string]any{
				"input":  float64(32000),
				"output": float64(1200),
			},
		},
	}, Line: []byte("line")}
	got, terminal := mapEvent(ev, st)
	if terminal {
		t.Fatal("compaction_end is not terminal")
	}
	if len(got) != 2 {
		t.Fatalf("compaction_end emitted %d events, want 2 (system + context usage)", len(got))
	}
	system, ok := got[0].(agent.SystemEvent)
	if !ok || system.Subtype != "compaction_end" {
		t.Fatalf("first event = %#v, want the compaction_end SystemEvent", got[0])
	}
	notice, ok := got[1].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("second event = %T, want ContextUsageEvent", got[1])
	}
	if notice.NoticeKind != agent.ContextUsageCompaction {
		t.Errorf("kind = %q, want compaction", notice.NoticeKind)
	}
	if notice.Harness != string(agent.HarnessPi) {
		t.Errorf("harness = %q, want pi", notice.Harness)
	}
	if notice.Source != agent.ContextUsageSourceRPCUsage {
		t.Errorf("source = %q, want rpc-usage", notice.Source)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 150000 {
		t.Errorf("inputTokens = %v, want 150000 (tokensBefore)", notice.InputTokens)
	}
	if notice.OutputTokens == nil || *notice.OutputTokens != 1200 {
		t.Errorf("outputTokens = %v, want 1200", notice.OutputTokens)
	}
	if notice.ContextWindowSize != nil {
		t.Errorf("contextWindowSize = %v; the post-compaction estimate is a heuristic, never a window", notice.ContextWindowSize)
	}
}

// TestMapEvent_AbortedCompactionEndStillMarks pins that a failed or aborted
// compaction (null result) still emits the compaction marker with no
// measurements: the marker itself is the signal.
func TestMapEvent_AbortedCompactionEndStillMarks(t *testing.T) {
	t.Parallel()
	st := &mapperState{}
	ev := rawEvent{Type: "compaction_end", Fields: map[string]any{
		"reason": "overflow", "aborted": true,
	}, Line: []byte("line")}
	got, _ := mapEvent(ev, st)
	if len(got) != 2 {
		t.Fatalf("aborted compaction_end emitted %d events, want 2", len(got))
	}
	notice, ok := got[1].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("second event = %T, want ContextUsageEvent", got[1])
	}
	if notice.NoticeKind != agent.ContextUsageCompaction {
		t.Errorf("kind = %q, want compaction", notice.NoticeKind)
	}
	if notice.HasMeasurements() {
		t.Errorf("aborted compaction fabricated measurements: %+v", notice)
	}
}

// TestMapEvent_CompactionEndToSeries pins the whole pi headless leg: a
// native compaction_end event in the real RPC shape maps to a typed
// notice that normalizes to the usage.recorded series record. It drives
// the production map→normalize chain, not a helper.
func TestMapEvent_CompactionEndToSeries(t *testing.T) {
	t.Parallel()
	st := &mapperState{}
	ev := rawEvent{Type: "compaction_end", Fields: map[string]any{
		"reason": "threshold",
		"result": map[string]any{
			"tokensBefore":         float64(150000),
			"estimatedTokensAfter": float64(32000),
		},
	}, Line: []byte("line")}
	got, _ := mapEvent(ev, st)
	if len(got) != 2 {
		t.Fatalf("compaction_end emitted %d events, want 2", len(got))
	}
	notice, ok := got[1].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("second event = %T, want ContextUsageEvent", got[1])
	}
	record, emitted, err := executionevent.NormalizeEvent("session_pi", 5, time.Now(), notice)
	if err != nil {
		t.Fatalf("NormalizeEvent: %v", err)
	}
	if !emitted {
		t.Fatal("notice did not emit a series record")
	}
	raw, _ := json.Marshal(record)
	var wire struct {
		EventType string         `json:"eventType"`
		Payload   map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode series record: %v", err)
	}
	if wire.EventType != "usage.recorded" {
		t.Fatalf("eventType = %q, want usage.recorded (in %s)", wire.EventType, raw)
	}
	if wire.Payload["kind"] != "compaction" {
		t.Errorf("kind = %v, want compaction", wire.Payload["kind"])
	}
	if wire.Payload["harness"] != "pi" || wire.Payload["source"] != "rpc-usage" {
		t.Errorf("payload identity = %v; want pi/rpc-usage", wire.Payload)
	}
}
