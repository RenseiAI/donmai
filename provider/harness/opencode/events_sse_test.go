package opencode

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// evt builds a serverEvent frame with the given type and property map.
func evt(id, typ string, props map[string]any) serverEvent {
	raw, _ := json.Marshal(props)
	return serverEvent{ID: id, Type: typ, Properties: raw}
}

// mapAll runs a mapper over a sequence of frames and returns the flattened
// agent.Event slice.
func mapAll(m *sseMapper, frames ...serverEvent) []agent.Event {
	var out []agent.Event
	for _, f := range frames {
		out = append(out, m.Map(f)...)
	}
	return out
}

func TestSSEMapper_HappyPath_Conforms(t *testing.T) {
	t.Parallel()
	const sid = "ses_abc"
	m := newSSEMapper(sid)
	events := mapAll(
		m,
		evt("e1", evSessionCreated, map[string]any{"sessionID": sid}),
		evt("e2", evTextEnded, map[string]any{"sessionID": sid, "text": "Hello there."}),
		evt("e3", evToolCalled, map[string]any{"sessionID": sid, "callID": "c1", "tool": "read", "input": map[string]any{"path": "x"}}),
		evt("e4", evToolSuccess, map[string]any{"sessionID": sid, "callID": "c1", "tool": "read", "content": "file body"}),
		evt("e5", evStepEnded, map[string]any{"sessionID": sid, "finish": "stop", "cost": 0.01, "tokens": map[string]any{"input": 10, "output": 5}}),
	)

	// Exactly one Init, ≥1 AssistantText, exactly one terminal, terminal last.
	var init, text, result int
	for _, ev := range events {
		switch ev.(type) {
		case agent.InitEvent:
			init++
		case agent.AssistantTextEvent:
			text++
		case agent.ResultEvent:
			result++
		}
	}
	if init != 1 {
		t.Errorf("InitEvent count = %d, want 1", init)
	}
	if text < 1 {
		t.Errorf("AssistantTextEvent count = %d, want >=1", text)
	}
	if result != 1 {
		t.Errorf("ResultEvent count = %d, want 1", result)
	}
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal contract violated: %v", err)
	}
	// Post-terminal frames must produce nothing (latch).
	if extra := m.Map(evt("e6", evTextEnded, map[string]any{"sessionID": sid, "text": "late"})); len(extra) != 0 {
		t.Errorf("post-terminal frame produced %d events, want 0", len(extra))
	}
}

func TestSSEMapper_SessionFilter(t *testing.T) {
	t.Parallel()
	m := newSSEMapper("ses_mine")
	// A frame for another session is dropped entirely.
	out := m.Map(evt("x1", evTextEnded, map[string]any{"sessionID": "ses_other", "text": "not mine"}))
	if len(out) != 0 {
		t.Errorf("cross-session frame produced %d events, want 0", len(out))
	}
	if m.initSent {
		t.Error("init should not fire for a foreign session")
	}
}

func TestSSEMapper_Dedup(t *testing.T) {
	t.Parallel()
	const sid = "ses_d"
	m := newSSEMapper(sid)
	f := evt("dup", evTextEnded, map[string]any{"sessionID": sid, "text": "once"})
	first := m.Map(f)
	second := m.Map(f) // replay of the same frame id
	if len(first) == 0 {
		t.Fatal("first delivery produced no events")
	}
	// second delivery is a pure duplicate (same frame id) → nothing.
	for _, ev := range second {
		if _, ok := ev.(agent.AssistantTextEvent); ok {
			t.Errorf("dedup failed: replayed frame re-emitted AssistantText")
		}
	}
}

func TestSSEMapper_ToolFailed(t *testing.T) {
	t.Parallel()
	const sid = "ses_tf"
	m := newSSEMapper(sid)
	out := mapAll(
		m,
		evt("e1", evSessionCreated, map[string]any{"sessionID": sid}),
		evt("e2", evToolFailed, map[string]any{"sessionID": sid, "callID": "c9", "tool": "bash", "error": "boom"}),
	)
	var found bool
	for _, ev := range out {
		if tr, ok := ev.(agent.ToolResultEvent); ok {
			if !tr.IsError || tr.Content != "boom" {
				t.Errorf("tool.failed → %+v, want IsError w/ content 'boom'", tr)
			}
			found = true
		}
	}
	if !found {
		t.Error("tool.failed did not map to a ToolResultEvent")
	}
}

func TestSSEMapper_SessionError_Terminal(t *testing.T) {
	t.Parallel()
	const sid = "ses_err"
	m := newSSEMapper(sid)
	out := mapAll(
		m,
		evt("e1", evSessionCreated, map[string]any{"sessionID": sid}),
		evt("e2", evSessionError, map[string]any{"sessionID": sid, "error": map[string]any{"message": "model unavailable"}}),
	)
	if err := conformance.CheckTerminalContract(out); err != nil {
		t.Errorf("terminal contract: %v", err)
	}
	last := out[len(out)-1]
	res, ok := last.(agent.ResultEvent)
	if !ok || res.Success {
		t.Fatalf("last event = %#v, want failed ResultEvent", last)
	}
	if len(res.Errors) == 0 || res.Errors[0] != "model unavailable" {
		t.Errorf("ResultEvent.Errors = %v, want [model unavailable]", res.Errors)
	}
}

func TestSSEMapper_StepEnded_ToolCallsNotTerminal(t *testing.T) {
	t.Parallel()
	const sid = "ses_s"
	m := newSSEMapper(sid)
	out := mapAll(
		m,
		evt("e1", evSessionCreated, map[string]any{"sessionID": sid}),
		evt("e2", evStepEnded, map[string]any{"sessionID": sid, "finish": "tool-calls"}),
	)
	for _, ev := range out {
		if _, ok := ev.(agent.ResultEvent); ok {
			t.Error("step.ended finish=tool-calls must NOT be terminal")
		}
	}
	if m.terminal {
		t.Error("mapper latched terminal on a tool-calls step")
	}
}

func TestExtractFinishReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{`"stop"`, "stop"},
		{`{"reason":"tool-calls"}`, "tool-calls"},
		{`{"type":"stop"}`, "stop"},
		{`{"finishReason":"length"}`, "length"},
		{``, ""},
	}
	for _, tc := range cases {
		if got := extractFinishReason(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("extractFinishReason(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsStopReason(t *testing.T) {
	t.Parallel()
	stop := []string{"stop", "end_turn", "", "length", "content-filter"}
	notStop := []string{"tool-calls", "tool_calls", "continue"}
	for _, r := range stop {
		if !isStopReason(r) {
			t.Errorf("isStopReason(%q) = false, want true", r)
		}
	}
	for _, r := range notStop {
		if isStopReason(r) {
			t.Errorf("isStopReason(%q) = true, want false", r)
		}
	}
}
