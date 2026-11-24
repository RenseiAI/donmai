package pi

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestToolProgressCarriesItemIDAndElapsedFromStart(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := &mapperState{initEmitted: true, now: func() time.Time { return clock }}
	call := map[string]any{"toolName": "bash", "toolCallId": "call_1", "args": map[string]any{"command": "make test"}}
	mapEvent(rawEvent{Type: "tool_execution_start", Fields: call, Line: []byte("l")}, st)
	clock = clock.Add(12 * time.Second)
	events, _ := mapEvent(rawEvent{Type: "tool_execution_update", Fields: map[string]any{"toolName": "bash", "toolCallId": "call_1", "partialResult": "..."}, Line: []byte("l")}, st)
	if len(events) != 1 {
		t.Fatalf("update mapped %d events, want 1", len(events))
	}
	p, ok := events[0].(agent.ToolProgressEvent)
	if !ok || p.ItemID != "call_1" || p.ElapsedSeconds != 12 {
		t.Fatalf("progress = %#v; want call_1 at 12s", events[0])
	}
	mapEvent(rawEvent{Type: "tool_execution_end", Fields: map[string]any{"toolName": "bash", "toolCallId": "call_1"}, Line: []byte("l")}, st)
	if _, still := st.toolStarted["call_1"]; still {
		t.Fatal("finished call still tracked")
	}
}

func TestMessageEndEmitsReasoningBeforeText(t *testing.T) {
	t.Parallel()
	st := &mapperState{initEmitted: true}
	st.textBuf.WriteString("Done.")
	msg := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "Fixture thinking."},
		map[string]any{"type": "text", "text": "Done."},
	}}
	events, _ := mapEvent(rawEvent{Type: "message_end", Fields: map[string]any{"message": msg}, Line: []byte("l")}, st)
	var kinds []agent.EventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind())
	}
	if len(events) < 2 || kinds[0] != agent.EventReasoning || events[0].(agent.ReasoningEvent).Text != "Fixture thinking." || kinds[1] != agent.EventAssistantText {
		t.Fatalf("events = %v; want reasoning then assistant text", kinds)
	}

	debug := &mapperState{initEmitted: true, debug: true}
	events, _ = mapEvent(rawEvent{Type: "message_end", Fields: map[string]any{"message": msg}, Line: []byte("l")}, debug)
	for _, ev := range events {
		if ev.Kind() == agent.EventReasoning {
			t.Fatal("debug mode repeats thinking already in the text buffer")
		}
	}
}
