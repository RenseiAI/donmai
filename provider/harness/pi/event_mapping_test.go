package pi

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
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
