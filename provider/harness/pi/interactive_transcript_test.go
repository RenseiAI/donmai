package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func writeTranscriptLine(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func transcriptMessage(id string, msg any) map[string]any {
	return map[string]any{"type": "message", "id": id, "parentId": "p", "timestamp": "t", "message": msg}
}

// TestInteractiveTranscriptTailSweep_MapsTurnsToolsAndResultsInOrder feeds a
// fixture transcript through the tailer and asserts the emitted agent events,
// in order: assistant text, one ToolUseEvent per tool call, one
// ToolResultEvent per result, and one per-turn usage event.
func TestInteractiveTranscriptTailSweep_MapsTurnsToolsAndResultsInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	writeTranscriptLine(t, path, map[string]any{"type": "session", "id": "s"})
	writeTranscriptLine(t, path, transcriptMessage("m-user", map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "text", "text": "do it"}},
	}))
	writeTranscriptLine(t, path, transcriptMessage("m-asst", map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "running it"},
			map[string]any{"type": "toolCall", "id": "call-1", "name": "bash", "arguments": map[string]any{"command": "echo hi"}},
			map[string]any{"type": "toolCall", "id": "call-2", "name": "read", "arguments": map[string]any{"path": "f"}},
		},
		"provider": "anthropic", "model": "m",
		"usage": map[string]any{"input": 10, "output": 20, "cost": map[string]any{"total": 0.01}},
	}))
	writeTranscriptLine(t, path, transcriptMessage("m-res1", map[string]any{
		"role": "toolResult", "toolCallId": "call-1", "toolName": "bash",
		"content": []any{map[string]any{"type": "text", "text": "hi\n"}}, "isError": false,
	}))

	tailer := newInteractiveTranscriptTailer(dir)
	events := tailer.sweep()

	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind()))
	}
	want := []string{
		string(agent.EventAssistantText), // "running it"
		string(agent.EventToolUse),       // bash call-1
		string(agent.EventToolUse),       // read call-2
		string(agent.EventLlmCall),       // turn usage
		string(agent.EventToolResult),    // bash result
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	text := events[0].(agent.AssistantTextEvent)
	if text.Text != "running it" {
		t.Errorf("assistant text = %q", text.Text)
	}
	use := events[1].(agent.ToolUseEvent)
	if use.ToolName != "bash" || use.ToolUseID != "call-1" {
		t.Errorf("tool use = %+v", use)
	}
	llm := events[3].(agent.LlmCallEvent)
	if llm.InputTokens != 10 || llm.OutputTokens != 20 || !llm.TurnCompleted {
		t.Errorf("llm call = %+v", llm)
	}
	if llm.ObservedCostUsd == nil || *llm.ObservedCostUsd != 0.01 {
		t.Errorf("llm cost = %+v", llm.ObservedCostUsd)
	}
	res := events[4].(agent.ToolResultEvent)
	if res.ToolName != "bash" || res.Content != "hi\n" || res.IsError {
		t.Errorf("tool result = %+v", res)
	}

	// A second sweep emits nothing — offsets advanced past every line.
	if again := tailer.sweep(); len(again) != 0 {
		t.Fatalf("second sweep emitted %d events, want 0", len(again))
	}

	// A torn trailing write (no newline) is not consumed until completed.
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"m-torn",`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if again := tailer.sweep(); len(again) != 0 {
		t.Fatalf("torn-line sweep emitted %d events, want 0", len(again))
	}
}

// TestInteractiveTranscriptTail_IgnoresNonMessageLines pins the negative
// space: session metadata, unknown types, and malformed lines map to nothing.
func TestInteractiveTranscriptTail_IgnoresNonMessageLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	writeTranscriptLine(t, path, map[string]any{"type": "session", "id": "s"})
	writeTranscriptLine(t, path, map[string]any{"type": "model_change", "id": "x"})
	writeTranscriptLine(t, path, transcriptMessage("m-user", map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}},
	}))
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n")
	_ = f.Close()

	tailer := newInteractiveTranscriptTailer(dir)
	if events := tailer.sweep(); len(events) != 0 {
		t.Fatalf("sweep emitted %d events, want 0", len(events))
	}
}

// TestInteractiveTranscriptTail_SkipsConfigAndInjectedDirs pins that the
// state-dir walk never descends into the per-session config home or the
// injected-extensions dir, which hold no transcripts.
func TestInteractiveTranscriptTail_SkipsConfigAndInjectedDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, sub := range []string{agentHomeDir, injectedExtensionsDir} {
		p := filepath.Join(dir, sub, "sess.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTranscriptLine(t, p, transcriptMessage("m", map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
		}))
	}
	tailer := newInteractiveTranscriptTailer(dir)
	if events := tailer.sweep(); len(events) != 0 {
		t.Fatalf("sweep emitted %d events from excluded dirs, want 0", len(events))
	}
}

// TestInteractiveTranscriptTail_CapsEventsPerSweep pins the rate limit: a
// transcript burst maps many events but one sweep emits at most the cap, and
// the tail never replays the dropped remainder.
func TestInteractiveTranscriptTail_CapsEventsPerSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	for i := 0; i < interactiveTranscriptMaxEventsPerSweep+10; i++ {
		writeTranscriptLine(t, path, transcriptMessage(jsonID(i), map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
		}))
	}
	tailer := newInteractiveTranscriptTailer(dir)
	first := tailer.sweep()
	if len(first) != interactiveTranscriptMaxEventsPerSweep {
		t.Fatalf("first sweep emitted %d events, want cap %d", len(first), interactiveTranscriptMaxEventsPerSweep)
	}
	if rest := tailer.sweep(); len(rest) != 0 {
		t.Fatalf("second sweep emitted %d events, want 0 (offsets advanced past the burst)", len(rest))
	}
}

func jsonID(i int) string {
	return "m-" + strings.Repeat("a", 4) + string(rune('0'+i%10)) + string(rune('a'+i/10%26))
}
