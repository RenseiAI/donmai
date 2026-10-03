package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// TestInteractive_TranscriptEventsReachSink drives the real
// dispatchInteractive supervisor with a handle whose event channel carries
// transcript-mapped agent events, and asserts they reach the activity sink —
// in order — alongside the existing lifecycle markers.
func TestInteractive_TranscriptEventsReachSink(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	worktreePath := t.TempDir()
	session := liveRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	base.events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1", Input: map[string]any{"command": "echo hi"}}
	base.events <- agent.AssistantTextEvent{Text: "running it"}
	base.events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: "hi\n"}
	base.events <- agent.LlmCallEvent{InputTokens: 10, OutputTokens: 20, UsageSource: agent.LlmUsageProvider, TurnCompleted: true}
	close(base.events)
	handle := &testInteractiveHandle{Handle: base, session: session}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-sink", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := r.dispatchInteractive(
			context.Background(), handle, worktreePath, qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
		)
		done <- outcome{result: result, err: err}
	}()
	// Wait until all four transcript events reach the sink before ending
	// the session; closing done first would let the Done branch win the
	// supervisor select and skip the buffered events.
	deadline := time.After(5 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.events)
		sink.mu.Unlock()
		if n >= 5 { // started + 4 transcript events
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for transcript events to reach the sink (got %d)", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(session.done)
	got := <-done
	if got.err != nil || got.result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", got.result.Status, got.err)
	}

	var kinds []string
	for _, ev := range sink.events {
		kinds = append(kinds, string(ev.Kind()))
	}
	joined := strings.Join(kinds, ",")
	wantOrder := []string{
		string(agent.EventSystem),  // interactive-session-started
		string(agent.EventToolUse), // transcript tool call
		string(agent.EventAssistantText),
		string(agent.EventToolResult),
		string(agent.EventLlmCall),
		string(agent.EventSystem), // interactive-session-ended
	}
	if joined != strings.Join(wantOrder, ",") {
		t.Fatalf("sink event kinds = %v, want %v", kinds, wantOrder)
	}

	// Transcript events are also mirrored to events.jsonl for audit parity.
	body, err := os.ReadFile(filepath.Join(worktreePath, ".agent", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"toolName":"bash"`, `"text":"running it"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("events.jsonl missing %s:\n%s", want, body)
		}
	}
}

// TestInteractive_TranscriptActivityTruncatesLongText pins the per-event
// token budget: a huge assistant chunk / tool result is capped before it
// reaches the sink, so a chatty transcript cannot flood the buffer.
func TestInteractive_TranscriptActivityTruncatesLongText(t *testing.T) {
	r := minimalRunner(t)
	sink := &recordingSink{}
	dir := t.TempDir()
	ctx := context.Background()
	r.postInteractiveTranscriptActivity(ctx, dir, sink, agent.AssistantTextEvent{Text: strings.Repeat("x", 3000)})
	r.postInteractiveTranscriptActivity(ctx, dir, sink, agent.ToolResultEvent{ToolName: "bash", Content: strings.Repeat("y", 5000)})
	if len(sink.events) != 2 {
		t.Fatalf("sink got %d events, want 2", len(sink.events))
	}
	if got := sink.events[0].(agent.AssistantTextEvent).Text; len(got) > 2100 {
		t.Errorf("assistant text not capped: %d bytes", len(got))
	}
	if got := sink.events[1].(agent.ToolResultEvent).Content; len(got) > 4200 {
		t.Errorf("tool result not capped: %d bytes", len(got))
	}
}
