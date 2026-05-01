package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestEvent_AllVariantsRoundTrip exercises every Event variant through
// MarshalEvent → UnmarshalEvent and asserts equality.
func TestEvent_AllVariantsRoundTrip(t *testing.T) {
	t.Parallel()
	cost := &CostData{InputTokens: 10, OutputTokens: 5, TotalCostUsd: 0.001, NumTurns: 1}
	cases := []struct {
		name string
		in   Event
		kind EventKind
	}{
		{"init", InitEvent{SessionID: "sess-1", Raw: map[string]any{"foo": "bar"}}, EventInit},
		{"system", SystemEvent{Subtype: "compaction", Message: "compacted", Raw: nil}, EventSystem},
		{"assistant_text", AssistantTextEvent{Text: "hello world"}, EventAssistantText},
		{
			"tool_use",
			ToolUseEvent{
				ToolName:     "Bash",
				ToolUseID:    "tu-1",
				Input:        map[string]any{"cmd": "git status"},
				ToolCategory: "shell",
			},
			EventToolUse,
		},
		{
			"tool_result",
			ToolResultEvent{
				ToolName:  "Bash",
				ToolUseID: "tu-1",
				Content:   "clean",
				IsError:   false,
			},
			EventToolResult,
		},
		{
			"tool_progress",
			ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 12.5},
			EventToolProgress,
		},
		{
			"result",
			ResultEvent{
				Success: true,
				Message: "done",
				Cost:    cost,
			},
			EventResult,
		},
		{
			"error",
			ErrorEvent{Message: "boom", Code: "spawn_failed"},
			EventError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.in.Kind() != tc.kind {
				t.Fatalf("Kind() = %q, want %q", tc.in.Kind(), tc.kind)
			}
			bytes, err := MarshalEvent(tc.in)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			wire := string(bytes)
			wantKind := `"kind":"` + string(tc.kind) + `"`
			if !strings.Contains(wire, wantKind) {
				t.Fatalf("wire missing kind discriminator %s\nwire=%s", wantKind, wire)
			}

			out, err := UnmarshalEvent(bytes)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v", err)
			}
			if out.Kind() != tc.kind {
				t.Fatalf("decoded Kind() = %q, want %q", out.Kind(), tc.kind)
			}

			// Compare normalized via JSON since Raw is `any` and may
			// be re-decoded as map[string]any.
			gotJSON, _ := json.Marshal(out)
			wantJSON, _ := json.Marshal(tc.in)
			if !reflect.DeepEqual(normalize(t, gotJSON), normalize(t, wantJSON)) {
				t.Fatalf("variant mismatch:\nwant=%s\ngot =%s", wantJSON, gotJSON)
			}
		})
	}
}

// normalize unmarshals into a generic map for type-tolerant comparison.
func normalize(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return m
}

// TestUnmarshalEvent_ErrorPaths verifies error reporting on bad input.
func TestUnmarshalEvent_ErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		wantSubst string
	}{
		{"invalid-json", `not json`, "decode event kind"},
		{"missing-kind", `{"sessionId":"x"}`, "missing kind discriminator"},
		{"unknown-kind", `{"kind":"alien"}`, `unknown event kind "alien"`},
		{"variant-decode-bad-text", `{"kind":"assistant_text","text":12}`, "decode AssistantTextEvent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalEvent([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSubst)
			}
			if !strings.Contains(err.Error(), tt.wantSubst) {
				t.Fatalf("error %q missing %q", err.Error(), tt.wantSubst)
			}
		})
	}
}

// TestMarshalEvent_NilEvent guards against accidental nil panic.
func TestMarshalEvent_NilEvent(t *testing.T) {
	t.Parallel()
	if _, err := MarshalEvent(nil); err == nil {
		t.Fatal("MarshalEvent(nil) returned nil error")
	}
}

// TestEvent_KindWireValues guards the wire-format kind strings.
func TestEvent_KindWireValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ev   Event
		want string
	}{
		{InitEvent{}, "init"},
		{SystemEvent{}, "system"},
		{AssistantTextEvent{}, "assistant_text"},
		{ToolUseEvent{}, "tool_use"},
		{ToolResultEvent{}, "tool_result"},
		{ToolProgressEvent{}, "tool_progress"},
		{ResultEvent{}, "result"},
		{ErrorEvent{}, "error"},
	}
	for _, tc := range cases {
		if got := string(tc.ev.Kind()); got != tc.want {
			t.Errorf("%T.Kind() = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

// TestEvent_PolymorphicDispatch ensures every kind dispatches to the
// correct concrete type after MarshalEvent → UnmarshalEvent.
func TestEvent_PolymorphicDispatch(t *testing.T) {
	t.Parallel()
	in := []Event{
		InitEvent{SessionID: "s1"},
		SystemEvent{Subtype: "x"},
		AssistantTextEvent{Text: "y"},
		ToolUseEvent{ToolName: "Bash", Input: map[string]any{}},
		ToolResultEvent{Content: "ok"},
		ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 1.0},
		ResultEvent{Success: true},
		ErrorEvent{Message: "boom"},
	}
	wantTypes := []string{
		"agent.InitEvent",
		"agent.SystemEvent",
		"agent.AssistantTextEvent",
		"agent.ToolUseEvent",
		"agent.ToolResultEvent",
		"agent.ToolProgressEvent",
		"agent.ResultEvent",
		"agent.ErrorEvent",
	}
	for i, ev := range in {
		bytes, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("[%d] MarshalEvent: %v", i, err)
		}
		out, err := UnmarshalEvent(bytes)
		if err != nil {
			t.Fatalf("[%d] UnmarshalEvent: %v", i, err)
		}
		gotType := reflect.TypeOf(out).String()
		if gotType != wantTypes[i] {
			t.Errorf("[%d] type = %q, want %q", i, gotType, wantTypes[i])
		}
	}
}
