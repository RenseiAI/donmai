package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Fixture transcripts for the interactive conversation tail, in the shape
// the harness really writes: assistant lines with text/tool_use content
// blocks and message.usage, user lines echoing tool results or carrying
// provider-side text, system/attachment/summary bookkeeping, and thinking
// blocks that must never leave the file.

func conversationLine(t *testing.T, v any) string {
	t.Helper()
	return jsonLine(t, v)
}

func TestMapConversationLine_AssistantTextAndToolUse(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-text-1", "timestamp": "2026-10-09T00:00:01.000Z",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "working on it"},
				map[string]any{
					"type": "tool_use", "id": "toolu-1", "name": "Bash",
					"input": map[string]any{"command": "echo hi"},
				},
			},
		},
	}))
	seen := make(map[string]struct{})
	events := mapConversationLine(line, seen, time.Time{})
	if len(events) != 2 {
		t.Fatalf("mapped %d events; want 2 (text + tool_use)", len(events))
	}
	text, ok := events[0].(agent.AssistantTextEvent)
	if !ok || text.Text != "working on it" {
		t.Fatalf("first event = %#v; want AssistantTextEvent{working on it}", events[0])
	}
	use, ok := events[1].(agent.ToolUseEvent)
	if !ok || use.ToolName != "Bash" || use.ToolUseID != "toolu-1" {
		t.Fatalf("second event = %#v; want ToolUseEvent{Bash toolu-1}", events[1])
	}
	if cmd, _ := use.Input["command"].(string); cmd != "echo hi" {
		t.Fatalf("tool input = %v; want the command", use.Input)
	}
}

func TestMapConversationLine_ThinkingNeverLeavesTheFile(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-think-1",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "hidden plan", "signature": "sig"},
				map[string]any{"type": "redacted_thinking", "data": "encrypted"},
			},
		},
	}))
	if events := mapConversationLine(line, make(map[string]struct{}), time.Time{}); len(events) != 0 {
		t.Fatalf("thinking mapped to %d events; want none", len(events))
	}
}

func TestMapConversationLine_ToolResult(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "user", "uuid": "u-result-1",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "toolu-1",
					"content": "hi\n", "is_error": false,
				},
			},
		},
	}))
	events := mapConversationLine(line, make(map[string]struct{}), time.Time{})
	if len(events) != 1 {
		t.Fatalf("mapped %d events; want 1", len(events))
	}
	result, ok := events[0].(agent.ToolResultEvent)
	if !ok || result.ToolUseID != "toolu-1" || result.Content != "hi\n" || result.IsError {
		t.Fatalf("event = %#v; want ToolResultEvent{toolu-1 hi}", events[0])
	}
}

func TestMapConversationLine_UserTurnIsAMarkerNeverAnInputEcho(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "user", "uuid": "u-user-1",
		"message": map[string]any{"role": "user", "content": "please continue"},
	}))
	events := mapConversationLine(line, make(map[string]struct{}), time.Time{})
	if len(events) != 1 {
		t.Fatalf("mapped %d events; want 1 marker", len(events))
	}
	marker, ok := events[0].(agent.SystemEvent)
	if !ok || marker.Subtype != "user_message" || marker.Message != "please continue" {
		t.Fatalf("event = %#v; want SystemEvent{user_message}", events[0])
	}
}

func TestMapConversationLine_StopHookFeedbackIsNotConversation(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "user", "uuid": "u-hook-1", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "Stop hook feedback:\nthe build is red"},
	}))
	if events := mapConversationLine(line, make(map[string]struct{}), time.Time{}); len(events) != 0 {
		t.Fatalf("stop-hook feedback mapped to %d events; want none (the notice channel acks on it)", len(events))
	}
}

func TestMapConversationLine_NonConversationRecordsMapToNothing(t *testing.T) {
	t.Parallel()
	lines := []string{
		conversationLine(t, map[string]any{"type": "system", "subtype": "stop_hook_summary"}),
		conversationLine(t, map[string]any{
			"type":       "attachment",
			"attachment": map[string]any{"type": "hook_success"},
		}),
		conversationLine(t, map[string]any{"type": "summary", "summary": "compacted"}),
		conversationLine(t, map[string]any{"type": "queue-operation", "operation": "enqueue"}),
		"{not json",
		conversationLine(t, map[string]any{
			"type":    "assistant",
			"message": map[string]any{"role": "assistant", "content": "bare"},
		}),
	}
	for i, l := range lines {
		if events := mapConversationLine([]byte(l), make(map[string]struct{}), time.Time{}); len(events) != 0 {
			t.Fatalf("line %d mapped to %d events; want none", i, len(events))
		}
	}
}

func TestMapConversationLine_DedupeAndResumeFilter(t *testing.T) {
	t.Parallel()
	line := []byte(conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-dupe-1", "timestamp": "2026-10-09T00:00:01.000Z",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "once"}},
		},
	}))
	seen := make(map[string]struct{})
	if got := mapConversationLine(line, seen, time.Time{}); len(got) != 1 {
		t.Fatalf("first sight mapped %d events; want 1", len(got))
	}
	if got := mapConversationLine(line, seen, time.Time{}); len(got) != 0 {
		t.Fatalf("second sight mapped %d events; want none (re-read must not double-emit)", len(got))
	}
	// A line stamped before the session began is history this session did
	// not run — a resumed conversation's earlier turns.
	since, _ := time.Parse(time.RFC3339Nano, "2026-10-09T00:00:02.000Z")
	if got := mapConversationLine(line, make(map[string]struct{}), since); len(got) != 0 {
		t.Fatalf("pre-session line mapped %d events; want none", len(got))
	}
}

// TestInteractiveConversationTailer_OrderedIncrementalDelivery pins the
// tail end to end: two transcripts (a conversation clear moved the session
// to a second file) plus a subagent file deliver every turn in order, and
// a re-read after a shrink emits nothing twice.
func TestInteractiveConversationTailer_OrderedIncrementalDelivery(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	first := writeTranscript(t, []string{
		conversationLine(t, map[string]any{
			"type": "assistant", "uuid": "u-1",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "first"}},
			},
		}),
		conversationLine(t, map[string]any{
			"type": "assistant", "uuid": "u-2",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": "toolu-9", "name": "Read",
					"input": map[string]any{"file_path": "x"},
				}},
			},
		}),
	})
	fireHook(t, c, hookStdin(first, false))
	tailer := newInteractiveConversationTailer(c, time.Time{})
	var kinds []string
	collect := func() []agent.Event {
		events, _ := tailer.sweepCapped()
		for _, ev := range events {
			switch e := ev.(type) {
			case agent.AssistantTextEvent:
				kinds = append(kinds, "text:"+e.Text)
			case agent.ToolUseEvent:
				kinds = append(kinds, "use:"+e.ToolName)
			case agent.ToolResultEvent:
				kinds = append(kinds, "result:"+e.ToolUseID)
			default:
				kinds = append(kinds, string(ev.Kind()))
			}
		}
		return events
	}
	// A conversation clear moves the session to a second transcript: it
	// needs its own file, since writeTranscript reuses one path per
	// call and a second call would overwrite the first. The production
	// usage tail polls the locator every second, so each transcript is
	// observed while it is latest: sweep between the fires to mirror
	// that incremental discovery.
	collect()
	secondPath := filepath.Join(t.TempDir(), "second.jsonl")
	var sb strings.Builder
	for _, l := range []string{
		conversationLine(t, map[string]any{
			"type": "user", "uuid": "u-3",
			"message": map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu-9",
					"content": "contents",
				}},
			},
		}),
	} {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(secondPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	fireHook(t, c, hookStdin(secondPath, false))
	collect()
	want := []string{"text:first", "use:Read", "result:toolu-9"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("delivered = %v; want %v in order", kinds, want)
	}
	// A re-read emits nothing twice.
	if again := collect(); len(again) != 0 {
		t.Fatalf("re-read delivered %d events; want none", len(again))
	}
}

// TestInteractiveConversationTailer_UnavailableSourceIsQuiet covers the
// session whose transcript never materialized: no locator, no events, no
// error — the PTY lifecycle still governs the session end.
func TestInteractiveConversationTailer_UnavailableSourceIsQuiet(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	tailer := newInteractiveConversationTailer(c, time.Time{})
	if events, more := tailer.sweepCapped(); len(events) != 0 || more {
		t.Fatalf("sweep with no transcript = %d events more=%v; want none", len(events), more)
	}
	var nilTailer *interactiveConversationTailer
	_ = nilTailer
	empty := newInteractiveConversationTailer(nil, time.Time{})
	if events, more := empty.sweepCapped(); len(events) != 0 || more {
		t.Fatalf("sweep with no hook = %d events more=%v; want none", len(events), more)
	}
}

// TestInteractiveConversationTailer_RotationDoesNotDuplicate pins the
// rotation rule: a shrunk file restarts from zero against the same dedupe
// set, so rewritten lines emit nothing twice while genuinely new lines
// still deliver.
func TestInteractiveConversationTailer_RotationDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{
		conversationLine(t, map[string]any{
			"type": "assistant", "uuid": "u-1",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "first"}},
			},
		}),
	})
	fireHook(t, c, hookStdin(path, false))
	tailer := newInteractiveConversationTailer(c, time.Time{})
	if events, _ := tailer.sweepCapped(); len(events) != 1 {
		t.Fatalf("first sweep = %d events; want 1", len(events))
	}
	// Rotation: the file is rewritten shorter with the same line plus one
	// new line.
	rewritten := conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-1",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "first"}},
		},
	}) + "\n" + conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-2",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "second"}},
		},
	}) + "\n"
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	events, _ := tailer.sweepCapped()
	if len(events) != 1 {
		t.Fatalf("post-rotation sweep = %d events; want 1 (only the new line)", len(events))
	}
	if text := events[0].(agent.AssistantTextEvent).Text; text != "second" {
		t.Fatalf("post-rotation event = %q; want second", text)
	}
}

// TestInteractiveConversationTailer_RotationDoesNotDuplicateIdLessLines
// pins the uuid-less rotation fix: a transcript line without a uuid
// keys its dedupe on the line's content hash, so truncation (the file
// losing its tail) plus a rewrite never re-emits the surviving id-less
// line. Reverting the claim to the uuid-only no-op re-emits it on the
// second sweep.
func TestInteractiveConversationTailer_RotationDoesNotDuplicateIdLessLines(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	first := conversationLine(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "unidentified first"}},
		},
	})
	// A second line pads the file so truncating it back to the first
	// line is a real shrink (rotation), resetting the tail offset.
	pad := conversationLine(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "padding"}},
		},
	})
	path := writeTranscript(t, []string{first, pad})
	fireHook(t, c, hookStdin(path, false))
	tailer := newInteractiveConversationTailer(c, time.Time{})
	if events, _ := tailer.sweepCapped(); len(events) != 2 {
		t.Fatalf("first sweep = %d events; want 2", len(events))
	}
	// Rotation: the file is truncated back to the surviving id-less
	// line. The offset resets, but the claim holds: nothing re-emits.
	if err := os.WriteFile(path, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if events, _ := tailer.sweepCapped(); len(events) != 0 {
		t.Fatalf("post-rotation sweep = %d events; want none (the surviving id-less line must not re-emit)", len(events))
	}
	// Two genuinely different id-less lines keep their own identity:
	// a new distinct line still delivers.
	appendTranscript(t, path, conversationLine(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "unidentified second"}},
		},
	}))
	if events, _ := tailer.sweepCapped(); len(events) != 1 {
		t.Fatalf("new-line sweep = %d events; want 1", len(events))
	}
}

// TestInteractiveConversationTailer_TornTailWaitsForTheWriter pins that a
// trailing line without its newline stays unread until the writer
// completes it.
func TestInteractiveConversationTailer_TornTailWaitsForTheWriter(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	full := conversationLine(t, map[string]any{
		"type": "assistant", "uuid": "u-1",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "whole"}},
		},
	})
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil { // no newline yet
		t.Fatal(err)
	}
	fireHook(t, c, hookStdin(path, false))
	tailer := newInteractiveConversationTailer(c, time.Time{})
	if events, _ := tailer.sweepCapped(); len(events) != 0 {
		t.Fatalf("torn tail delivered %d events; want none", len(events))
	}
	appendTranscript(t, path, "")
	if events, _ := tailer.sweepCapped(); len(events) != 1 {
		t.Fatalf("completed tail delivered %d events; want 1", len(events))
	}
}

// TestWrapInteractiveHandle_ForwardsConversationAndUsage drives the
// production spawn wrapper end to end: conversation tailed from the
// session's transcript arrives as assistant/tool events alongside the
// usage events, and the terminal ResultEvent still carries the deduped
// session totals. The fake binary stands in for the REPL (kept alive
// briefly so the tailers sweep while the session runs, as a real REPL
// does); the transcript is the harness's own fixture.
func TestWrapInteractiveHandle_ForwardsConversationAndUsage(t *testing.T) {
	p := newFakeInteractiveProvider(t, `echo "argv: $@"; sleep 5`)
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	wrapper, ok := h.(*interactiveHandle)
	if !ok {
		t.Fatalf("Spawn returned %T; want *interactiveHandle", h)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })

	path := writeTranscript(t, []string{
		conversationLine(t, map[string]any{
			"type": "assistant", "uuid": "u-conv-1",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "live turn"}},
			},
		}),
		usageLine(t, "msg-1", 100, 20, 1000, 50),
	})
	// The hook fires as the session runs: both tailers discover the
	// locator and the conversation + usage land on Events.
	fireHook(t, wrapper.notices, hookStdin(path, false))

	var texts []string
	var usage int
	var terminal *agent.ResultEvent
	for {
		select {
		case ev, ok := <-wrapper.Events():
			if !ok {
				goto drained
			}
			switch v := ev.(type) {
			case agent.AssistantTextEvent:
				texts = append(texts, v.Text)
			case agent.LlmCallEvent:
				usage++
			case agent.ResultEvent:
				cp := v
				terminal = &cp
			}
			if terminal != nil {
				goto drained
			}
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
drained:
	// The usage fixture line is itself a real assistant line with text,
	// so the conversation tail maps its text as well, in file order.
	if len(texts) != 2 || texts[0] != "live turn" || texts[1] != "work" {
		t.Fatalf("conversation texts = %v; want [live turn work]", texts)
	}
	if usage != 1 {
		t.Fatalf("usage events = %d; want 1", usage)
	}
	if terminal == nil {
		t.Fatal("never observed the terminal ResultEvent")
	}
	if terminal.Cost == nil || terminal.Cost.InputTokens != 100 {
		t.Fatalf("terminal cost = %+v; want the transcript totals", terminal.Cost)
	}
}
