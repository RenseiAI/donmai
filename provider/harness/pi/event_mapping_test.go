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
