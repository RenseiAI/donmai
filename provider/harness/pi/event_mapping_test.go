package pi

import (
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

	providerError := func(detail string) agent.Event {
		return agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: detail, Raw: "line"}
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
			wantEvents: []agent.Event{providerError("503 Service Unavailable")},
		},
		{
			name:     "error after partial text keeps the text first",
			buffered: "Let me check",
			message:  map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "504 Gateway Timeout"},
			wantEvents: []agent.Event{
				agent.AssistantTextEvent{Text: "Let me check", Raw: "line"},
				providerError("504 Gateway Timeout"),
			},
		},
		{
			name:       "error without a message still names a provider error",
			message:    map[string]any{"role": "assistant", "stopReason": "error"},
			wantEvents: []agent.Event{providerError("model provider error")},
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
				if gotEvents[i] != tt.wantEvents[i] {
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
