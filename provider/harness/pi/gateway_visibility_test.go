package pi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestMapEvent_TurnEndGatewayIdentity pins the gateway-visibility finding: a
// turn reports the harness's selected catalog entry (System/Model) and, when
// the lane supplies it, the native served-model identity (ResponseModel).
// The selected entry names the aggregator itself on an aggregator-routed
// session — never the upstream behind it — and no turn field carries that
// final upstream or any per-attempt routing.
func TestMapEvent_TurnEndGatewayIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		message           map[string]any
		wantSystem        string
		wantModel         string
		wantResponseModel string
	}{
		{
			name: "aggregator pin reports the aggregator, not the upstream behind it",
			message: map[string]any{
				"provider": "example-aggregator",
				"model":    "example-author/example-model",
				"usage":    map[string]any{"input": float64(10), "output": float64(5)},
			},
			wantSystem:        "example-aggregator",
			wantModel:         "example-author/example-model",
			wantResponseModel: "",
		},
		{
			name: "served-model identity rides responseModel when the lane supplies it",
			message: map[string]any{
				"provider":      "example-aggregator",
				"model":         "example-author/example-model",
				"responseModel": "served-model-id",
				"usage":         map[string]any{"input": float64(10), "output": float64(5)},
			},
			wantSystem:        "example-aggregator",
			wantModel:         "example-author/example-model",
			wantResponseModel: "served-model-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := &mapperState{}
			ev := rawEvent{Type: "turn_end", Fields: map[string]any{"message": tt.message}, Line: []byte("line")}
			gotEvents, terminal := mapEvent(ev, st)
			if terminal {
				t.Fatal("terminal = true; a turn end never ends the session")
			}
			if len(gotEvents) != 1 {
				t.Fatalf("events = %d, want 1", len(gotEvents))
			}
			call, ok := gotEvents[0].(agent.LlmCallEvent)
			if !ok {
				t.Fatalf("event %T, want LlmCallEvent", gotEvents[0])
			}
			if call.System != tt.wantSystem || call.Model != tt.wantModel || call.ResponseModel != tt.wantResponseModel {
				t.Errorf("identity = system %q model %q responseModel %q; want system %q model %q responseModel %q",
					call.System, call.Model, call.ResponseModel, tt.wantSystem, tt.wantModel, tt.wantResponseModel)
			}
			if call.ResponseModelProvider != "" || call.ModelSnapshotID != "" {
				t.Errorf("provider/version identity = %q/%q; want empty (the lane reports no provider for the served model)",
					call.ResponseModelProvider, call.ModelSnapshotID)
			}
			if !call.TurnCompleted {
				t.Error("TurnCompleted = false; a mapped turn is a completed model turn")
			}
		})
	}
}

// TestMapEvent_AutoRetryAttemptsReachRunner pins that retry attempt counts
// reach the runner: each auto-retry event maps to a SystemEvent of the same
// subtype whose Raw preserves the attempt payload.
func TestMapEvent_AutoRetryAttemptsReachRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		typ     string
		fields  map[string]any
		wantKey string
	}{
		{
			name:    "retry start preserves the attempt count",
			typ:     "auto_retry_start",
			fields:  map[string]any{"attempt": float64(1), "maxAttempts": float64(3), "delayMs": float64(2000), "errorMessage": "overloaded"},
			wantKey: "attempt",
		},
		{
			name:    "retry end preserves the terminal attempt",
			typ:     "auto_retry_end",
			fields:  map[string]any{"success": true, "attempt": float64(2)},
			wantKey: "attempt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			line, err := json.Marshal(tt.fields)
			if err != nil {
				t.Fatal(err)
			}
			st := &mapperState{}
			gotEvents, terminal := mapEvent(rawEvent{Type: tt.typ, Fields: tt.fields, Line: line}, st)
			if terminal {
				t.Fatalf("terminal = true; %s never ends the session", tt.typ)
			}
			if len(gotEvents) != 1 {
				t.Fatalf("events = %d, want 1", len(gotEvents))
			}
			sys, ok := gotEvents[0].(agent.SystemEvent)
			if !ok {
				t.Fatalf("event %T, want SystemEvent", gotEvents[0])
			}
			if sys.Subtype != tt.typ {
				t.Errorf("Subtype = %q, want %q", sys.Subtype, tt.typ)
			}
			raw, ok := sys.Raw.(string)
			if !ok || !strings.Contains(raw, `"`+tt.wantKey+`"`) {
				t.Errorf("Raw = %v; want it to preserve %q", sys.Raw, tt.wantKey)
			}
		})
	}
}
