package codex

// Fixture rollout records for the interactive conversation tail, in the
// shape real rollouts take: response_item halves (the model's own records)
// and event_msg item_completed halves (the client's view, under different
// ids), token_count per-turn usage, task_complete turn markers, and the
// reasoning/session_meta/lifecycle shapes that must never map.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func conversationRolloutLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

func writeConversationRollout(t *testing.T, home, threadID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+threadID+".jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assistantMessageLine(id, text string) map[string]any {
	return map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message", "id": id, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		},
	}
}

func TestMapConversationLine_AssistantTextDeliversOnceAcrossHalves(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	const text = "reading the runs document now"
	first := tailer.mapLine([]byte(conversationRolloutLine(t, assistantMessageLine("msg-1", text))))
	if len(first) != 1 {
		t.Fatalf("response_item half mapped %d events; want 1", len(first))
	}
	if got := first[0].(agent.AssistantTextEvent).Text; got != text {
		t.Fatalf("text = %q; want %q", got, text)
	}
	// The event_msg completion of the SAME record under a different id
	// maps to nothing: one record, one event, whichever half arrives
	// first.
	second := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "AgentMessage", "id": "01a10d7d-other-id",
				"content": []any{map[string]any{"type": "Text", "text": text}},
			},
		},
	})))
	if len(second) != 0 {
		t.Fatalf("completion half mapped %d events; want none (already delivered)", len(second))
	}
	// And in the other order: the completion first, then the
	// response_item.
	other := newInteractiveConversationTailer("", nil)
	other.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "AgentMessage", "id": "01a10d7d-other-id",
				"content": []any{map[string]any{"type": "Text", "text": text}},
			},
		},
	})))
	if again := other.mapLine([]byte(conversationRolloutLine(t, assistantMessageLine("msg-1", text)))); len(again) != 0 {
		t.Fatalf("response_item after completion mapped %d events; want none", len(again))
	}
}

func TestMapConversationLine_ShellCallPairsAcrossHalves(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	// The exec start stages the call and emits nothing.
	start := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "custom_tool_call", "id": "ctc-1", "status": "running",
			"call_id": "call-abc", "name": "exec",
			"input": `text(await tools.exec_command({cmd:"git status",` + "\"max_output_tokens\":2500}));\n",
		},
	})))
	if len(start) != 0 {
		t.Fatalf("exec start mapped %d events; want none (staged)", len(start))
	}
	// The completion pairs by command and delivers both halves.
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "CommandExecution", "id": "exec-1",
				"command":           []any{"/bin/zsh", "-lc", "git status"},
				"status":            "completed",
				"exit_code":         0,
				"aggregated_output": "nothing to commit\n",
			},
		},
	})))
	if len(done) != 2 {
		t.Fatalf("completion mapped %d events; want 2 (use + result)", len(done))
	}
	use, ok := done[0].(agent.ToolUseEvent)
	if !ok || use.ToolName != "shell" || use.ToolUseID != "call-abc" {
		t.Fatalf("first event = %#v; want ToolUseEvent{shell call-abc}", done[0])
	}
	if cmd, _ := use.Input["command"].(string); cmd != "git status" {
		t.Fatalf("tool input = %v; want the command", use.Input)
	}
	result, ok := done[1].(agent.ToolResultEvent)
	if !ok || result.ToolUseID != "call-abc" || result.Content != "nothing to commit\n" || result.IsError {
		t.Fatalf("second event = %#v; want ToolResultEvent{call-abc}", done[1])
	}
	// A re-read of the completion emits nothing twice.
	if again := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "CommandExecution", "id": "exec-1",
				"command":           []any{"/bin/zsh", "-lc", "git status"},
				"status":            "completed",
				"exit_code":         0,
				"aggregated_output": "nothing to commit\n",
			},
		},
	}))); len(again) != 0 {
		t.Fatalf("re-read completion mapped %d events; want none", len(again))
	}
}

func TestMapConversationLine_IdenticalCommandsStageIndependently(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	mkStart := func(call string) string {
		return conversationRolloutLine(t, map[string]any{
			"type": "response_item",
			"payload": map[string]any{
				"type": "custom_tool_call", "id": "ctc-" + call, "status": "running",
				"call_id": call, "name": "exec",
				"input": `text(await tools.exec_command({cmd:"pwd",` + "\"max_output_tokens\":100}));\n",
			},
		})
	}
	mkDone := func(execID string) string {
		return conversationRolloutLine(t, map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
				"item": map[string]any{
					"type": "CommandExecution", "id": execID,
					"command":           []any{"/bin/zsh", "-lc", "pwd"},
					"status":            "completed",
					"exit_code":         0,
					"aggregated_output": "/tmp\n",
				},
			},
		})
	}
	if got := tailer.mapLine([]byte(mkStart("call-AAA"))); len(got) != 0 {
		t.Fatalf("first start mapped %d events; want none (staged)", len(got))
	}
	if got := tailer.mapLine([]byte(mkStart("call-BBB"))); len(got) != 0 {
		t.Fatalf("second identical start mapped %d events; want none (staged independently)", len(got))
	}
	first := tailer.mapLine([]byte(mkDone("exec-1")))
	if len(first) != 2 {
		t.Fatalf("first completion mapped %d events; want 2", len(first))
	}
	if use, ok := first[0].(agent.ToolUseEvent); !ok || use.ToolUseID != "call-AAA" {
		t.Fatalf("first use = %#v; want ToolUseID call-AAA", first[0])
	}
	second := tailer.mapLine([]byte(mkDone("exec-2")))
	if len(second) != 2 {
		t.Fatalf("second completion mapped %d events; want 2", len(second))
	}
	if use, ok := second[0].(agent.ToolUseEvent); !ok || use.ToolUseID != "call-BBB" {
		t.Fatalf("second use = %#v; want ToolUseID call-BBB", second[0])
	}
	if res, ok := second[1].(agent.ToolResultEvent); !ok || res.ToolUseID != "call-BBB" {
		t.Fatalf("second result = %#v; want ToolUseID call-BBB", second[1])
	}
}

func TestMapConversationLine_CompletionWithoutStartStillDelivers(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "CommandExecution", "id": "exec-late",
				"command":           []any{"/bin/zsh", "-lc", "pwd"},
				"status":            "failed",
				"exit_code":         1,
				"aggregated_output": "nope\n",
			},
		},
	})))
	if len(done) != 2 {
		t.Fatalf("orphan completion mapped %d events; want 2 (reconstructed use + result)", len(done))
	}
	if use := done[0].(agent.ToolUseEvent); use.ToolUseID != "exec-late" {
		t.Fatalf("reconstructed use id = %q; want the completion id", use.ToolUseID)
	}
	if result := done[1].(agent.ToolResultEvent); !result.IsError {
		t.Fatalf("failed completion mapped IsError=false; want true")
	}
}

func TestMapConversationLine_McpToolCallMirrorsHeadless(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "McpToolCall", "id": "exec-mcp-1",
				"server": "asana", "tool": "list_tasks",
				"arguments": map[string]any{"limit": float64(5)},
				"status":    "completed",
				"result": map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "[{}]"}},
				},
			},
		},
	})))
	if len(done) != 2 {
		t.Fatalf("mcp completion mapped %d events; want 2", len(done))
	}
	if use := done[0].(agent.ToolUseEvent); use.ToolName != "mcp__asana__list_tasks" {
		t.Fatalf("mcp tool name = %q; want the normalized headless name", use.ToolName)
	}
}

func TestMapConversationLine_FileChangeIsContentNotACall(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type":   "FileChange",
				"id":     "exec-fc-1",
				"status": "completed",
				"changes": map[string]any{
					"/tmp/a.txt": map[string]any{"type": "modify"},
				},
			},
		},
	})))
	if len(done) != 1 {
		t.Fatalf("file change mapped %d events; want 1 result", len(done))
	}
	if result := done[0].(agent.ToolResultEvent); result.ToolName != "file_change" {
		t.Fatalf("event = %#v; want ToolResultEvent{file_change}", done[0])
	}
}

func TestMapConversationLine_UserTurnIsAMarkerNeverAnInputEcho(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": "thread-x", "turn_id": "turn-1",
			"item": map[string]any{
				"type": "UserMessage", "id": "01a10d7d-u1",
				"content": []any{map[string]any{"type": "text", "text": "keep going"}},
			},
		},
	})))
	if len(done) != 1 {
		t.Fatalf("user message mapped %d events; want 1 marker", len(done))
	}
	marker, ok := done[0].(agent.SystemEvent)
	if !ok || marker.Subtype != "user_message" || marker.Message != "keep going" {
		t.Fatalf("event = %#v; want SystemEvent{user_message}", done[0])
	}
}

func TestMapConversationLine_HarnessFramingAndReasoningNeverMap(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	lines := []string{
		conversationRolloutLine(t, map[string]any{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message", "id": "msg-dev", "role": "developer",
				"content": []any{map[string]any{"type": "input_text", "text": "<environment_context>\ncwd: /tmp"}},
			},
		}),
		conversationRolloutLine(t, map[string]any{
			"type":    "response_item",
			"payload": map[string]any{"type": "reasoning", "id": "rs-1", "encrypted_content": "gAAAAA..."},
		}),
		conversationRolloutLine(t, map[string]any{
			"type":    "response_item",
			"payload": map[string]any{"type": "custom_tool_call_output", "id": "ctco-1", "call_id": "call-x", "output": []any{}},
		}),
		conversationRolloutLine(t, map[string]any{
			"type":    "event_msg",
			"payload": map[string]any{"type": "item_started", "thread_id": "t", "item": map[string]any{"type": "Reasoning", "id": "rs-2"}},
		}),
		conversationRolloutLine(t, map[string]any{
			"type":    "event_msg",
			"payload": map[string]any{"type": "session_meta", "thread_id": "t"},
		}),
		"{not json",
	}
	for i, l := range lines {
		if events := tailer.mapLine([]byte(l)); len(events) != 0 {
			t.Fatalf("line %d mapped %d events; want none", i, len(events))
		}
	}
}

func TestMapConversationLine_ClockPacingNeverMaps(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	start := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "function_call", "id": "fc-sleep", "name": "sleep",
			"namespace": "clock", "arguments": `{"ms":1000}`, "call_id": "call-sleep",
		},
	})))
	if len(start) != 0 {
		t.Fatalf("clock sleep staged %d events; want none", len(start))
	}
}

func TestMapConversationLine_TurnCompletionIsAMarker(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "task_complete", "turn_id": "turn-9",
			"last_agent_message": "draft is ready",
		},
	})))
	if len(done) != 1 {
		t.Fatalf("task_complete mapped %d events; want 1 marker", len(done))
	}
	marker, ok := done[0].(agent.SystemEvent)
	if !ok || marker.Subtype != "turn_completed" || marker.Message != "draft is ready" {
		t.Fatalf("event = %#v; want SystemEvent{turn_completed}", done[0])
	}
	if again := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "task_complete", "turn_id": "turn-9",
			"last_agent_message": "draft is ready",
		},
	}))); len(again) != 0 {
		t.Fatalf("duplicate task_complete mapped %d events; want none", len(again))
	}
}

func TestMapConversationLine_TokenCountMapsLastCallUsage(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":      "token_count",
			"thread_id": "thread-x",
			"info": map[string]any{
				"total_token_usage": map[string]any{
					"input_tokens": 1000, "cached_input_tokens": 800,
					"cache_write_input_tokens": 10, "output_tokens": 50,
					"reasoning_output_tokens": 5, "total_tokens": 1050,
				},
				"last_token_usage": map[string]any{
					"input_tokens": 1000, "cached_input_tokens": 800,
					"cache_write_input_tokens": 10, "output_tokens": 50,
					"reasoning_output_tokens": 5, "total_tokens": 1050,
				},
			},
		},
	})))
	if len(done) != 1 {
		t.Fatalf("token_count mapped %d events; want 1", len(done))
	}
	usage, ok := done[0].(agent.LlmCallEvent)
	if !ok || !usage.TurnCompleted || usage.UsageSource != agent.LlmUsageProvider {
		t.Fatalf("event = %#v; want the provider-completed usage shape", done[0])
	}
	// Fresh input excludes both cache buckets, exactly as the exit read
	// owns it: 1000 - 800 - 10 = 190.
	if usage.InputTokens != 190 || usage.CachedInputTokens != 800 ||
		usage.CacheWriteTokens != 10 || usage.OutputTokens != 50 {
		t.Fatalf("usage = %+v; want fresh=190 cached=800 written=10 output=50", usage)
	}
}

// TestInteractiveConversationTailer_OrderedIncrementalDelivery pins the
// tail end to end over a real-shaped rollout: assistant text, a paired
// shell call, a user turn, and turn completion deliver in file order, and
// a re-read (shrink + rewrite) emits nothing twice.
func TestInteractiveConversationTailer_OrderedIncrementalDelivery(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const thread = "thread-conversation-order"
	writeConversationRollout(t, home, thread, []string{
		conversationRolloutLine(t, assistantMessageLine("msg-1", "starting now")),
		conversationRolloutLine(t, map[string]any{
			"type": "response_item",
			"payload": map[string]any{
				"type": "custom_tool_call", "id": "ctc-1", "status": "running",
				"call_id": "call-1", "name": "exec",
				"input": `text(await tools.exec_command({cmd:"pwd",` + "\"max_output_tokens\":100}));\n",
			},
		}),
	})
	resolve := func() string { return thread }
	tailer := newInteractiveConversationTailer(home, resolve)
	first, more := tailer.sweepCapped()
	if more {
		t.Fatalf("first sweep reports more with two lines pending; want a clean sweep")
	}
	if len(first) != 1 {
		t.Fatalf("first sweep = %d events; want 1 (text; the exec start stages)", len(first))
	}
	if text := first[0].(agent.AssistantTextEvent).Text; text != "starting now" {
		t.Fatalf("first event = %q; want the assistant text", text)
	}
	// Append the completion, a user turn, and turn completion.
	path := findThreadRollout(home, thread)
	appendConversationRollout(t, path, []string{
		conversationRolloutLine(t, map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type": "item_completed", "thread_id": thread, "turn_id": "turn-1",
				"item": map[string]any{
					"type": "CommandExecution", "id": "exec-1",
					"command":           []any{"/bin/zsh", "-lc", "pwd"},
					"status":            "completed",
					"exit_code":         0,
					"aggregated_output": "/tmp\n",
				},
			},
		}),
		conversationRolloutLine(t, map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type": "item_completed", "thread_id": thread, "turn_id": "turn-1",
				"item": map[string]any{
					"type": "UserMessage", "id": "u-1",
					"content": []any{map[string]any{"type": "text", "text": "nice"}},
				},
			},
		}),
		conversationRolloutLine(t, map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type": "task_complete", "turn_id": "turn-1",
				"last_agent_message": "done",
			},
		}),
	})
	second, _ := tailer.sweepCapped()
	var kinds []string
	for _, ev := range second {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			kinds = append(kinds, "use:"+e.ToolName)
		case agent.ToolResultEvent:
			kinds = append(kinds, "result:"+e.ToolUseID)
		case agent.SystemEvent:
			kinds = append(kinds, "system:"+e.Subtype)
		default:
			kinds = append(kinds, string(ev.Kind()))
		}
	}
	want := []string{"use:shell", "result:call-1", "system:user_message", "system:turn_completed"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("second sweep = %v; want %v in order", kinds, want)
	}
	// A shrink + rewrite (rotation) emits nothing twice.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil { //nolint:gosec // G703: path is findThreadRollout's own return under the test's temp home, never wire input.
		t.Fatal(err)
	}
	if again, _ := tailer.sweepCapped(); len(again) != 0 {
		t.Fatalf("post-rotation sweep = %d events; want none", len(again))
	}
}

// TestInteractiveConversationTailer_UnknownThreadStaysQuiet covers the
// fresh-named path before naming completes (no identity yet) and the
// session whose rollout never materialized: no scan, no events, no error.
func TestInteractiveConversationTailer_UnknownThreadStaysQuiet(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// A decoy rollout from an unrelated thread sits in the same home:
	// the session binding must never pick it up, even though it holds
	// a mappable assistant line.
	writeConversationRollout(t, home, "thread-someone-else", []string{
		conversationRolloutLine(t, assistantMessageLine("msg-decoy", "someone else's turn")),
	})
	quiet := newInteractiveConversationTailer(home, nil)
	if events, more := quiet.sweepCapped(); len(events) != 0 || more {
		t.Fatalf("nil-resolver sweep = %d events more=%v; want none", len(events), more)
	}
	empty := func() string { return "" }
	unknown := newInteractiveConversationTailer(home, empty)
	if events, more := unknown.sweepCapped(); len(events) != 0 || more {
		t.Fatalf("empty-identity sweep = %d events more=%v; want none", len(events), more)
	}
	absent := newInteractiveConversationTailer(home, func() string { return "thread-never-ran" })
	if events, more := absent.sweepCapped(); len(events) != 0 || more {
		t.Fatalf("absent-file sweep = %d events more=%v; want none", len(events), more)
	}
	// The decoy is still there and still unread: resolving to the wrong
	// identity would emit it, so assert the decoy thread itself DOES map
	// (proving the fixture is live) while the unknown-identity sweeps
	// above stayed quiet.
	decoy := newInteractiveConversationTailer(home, func() string { return "thread-someone-else" })
	if events, _ := decoy.sweepCapped(); len(events) == 0 {
		t.Fatal("decoy sweep = 0 events; want the fixture to map (else the quiet test proves nothing)")
	}
}

// TestMapConversationLine_TokenCountDeliversIdenticalCountsOnDifferentTurns
// pins the turn-identity fix: two turns with identical counters deliver
// two usage events, because the usage key is the envelope turn id, not
// the counters. Reverting the key to counters-only maps the second turn
// to nothing.
func TestMapConversationLine_TokenCountDeliversIdenticalCountsOnDifferentTurns(t *testing.T) {
	t.Parallel()
	tailer := newInteractiveConversationTailer("", nil)
	mkUsage := func(turn string) string {
		return conversationRolloutLine(t, map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type":      "token_count",
				"thread_id": "thread-x",
				"turn_id":   turn,
				"info": map[string]any{
					"total_token_usage": map[string]any{
						"input_tokens": 100, "cached_input_tokens": 10,
						"cache_write_input_tokens": 5, "output_tokens": 20,
						"reasoning_output_tokens": 0, "total_tokens": 120,
					},
					"last_token_usage": map[string]any{
						"input_tokens": 100, "cached_input_tokens": 10,
						"cache_write_input_tokens": 5, "output_tokens": 20,
						"reasoning_output_tokens": 0, "total_tokens": 120,
					},
				},
			},
		})
	}
	first := tailer.mapLine([]byte(mkUsage("turn-1")))
	if len(first) != 1 {
		t.Fatalf("first identical-count turn mapped %d events; want 1", len(first))
	}
	second := tailer.mapLine([]byte(mkUsage("turn-2")))
	if len(second) != 1 {
		t.Fatalf("second identical-count turn mapped %d events; want 1 (different turn, different record)", len(second))
	}
	// A re-read of the same line still collapses: the turn id plus the
	// counters repeat exactly.
	if again := tailer.mapLine([]byte(mkUsage("turn-1"))); len(again) != 0 {
		t.Fatalf("re-read of turn-1 mapped %d events; want none", len(again))
	}
}

// TestInteractiveConversationTailer_OrphanedStartsStayBounded pins the
// pending-eviction fix: staged exec starts that never complete age out
// after the sweep bound instead of growing for the session's life. A
// completion arriving after its start aged out still delivers through
// the completion-reconstruction path. It drives the production sweep
// path: each orphan is written to the session's real rollout and swept.
func TestInteractiveConversationTailer_OrphanedStartsStayBounded(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-orphan-bound"
	tailer := newInteractiveConversationTailer(home, func() string { return thread })
	// Create the rollout once, then append one orphan per sweep: every
	// sweep stages exactly one new orphan while the bound reaps the
	// oldest, mirroring a chatty session whose completions lag.
	path := writeConversationRollout(t, home, thread, nil)
	total := interactiveConversationMaxPendingAgeSweeps + 10
	for i := range total {
		call := "orphan-" + itoa(i)
		appendConversationRollout(t, path, []string{
			conversationRolloutLine(t, map[string]any{
				"type": "response_item",
				"payload": map[string]any{
					"type": "custom_tool_call", "id": "ctc-" + call, "status": "running",
					"call_id": call, "name": "exec",
					"input": `text(await tools.exec_command({cmd:"orphan-` + call + `",` + "\"max_output_tokens\":100}));\\n",
				},
			}),
		})
		tailer.sweep()
	}
	if len(tailer.pending) > interactiveConversationMaxPendingAgeSweeps {
		t.Fatalf("pending holds %d orphaned starts after %d sweeps; want at most %d", len(tailer.pending), total, interactiveConversationMaxPendingAgeSweeps)
	}
	for command, queue := range tailer.pendingExecByCommand {
		for _, key := range queue {
			if _, ok := tailer.pending[key]; !ok {
				t.Fatalf("queue %q holds evicted key %q; want the queues pruned with the entries", command, key)
			}
		}
	}
	// A completion arriving after its start aged out still delivers
	// through the completion-reconstruction path.
	done := tailer.mapLine([]byte(conversationRolloutLine(t, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type": "item_completed", "thread_id": thread, "turn_id": "turn-late",
			"item": map[string]any{
				"type": "CommandExecution", "id": "exec-late",
				"command":           []any{"/bin/zsh", "-lc", "orphan-orphan-0"},
				"status":            "completed",
				"exit_code":         0,
				"aggregated_output": "late\\n",
			},
		},
	})))
	if len(done) != 2 {
		t.Fatalf("late completion mapped %d events; want 2 (reconstructed use + result)", len(done))
	}
}

// TestInteractiveConversationTailer_SubagentFilesDeliver pins that a
// session that delegates streams its subagent's conversation: the
// subagent thread's rollout (named by session_meta) is tailed alongside
// the parent's.
func TestInteractiveConversationTailer_OverlongLineDoesNotWedge(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-overlong"
	big := strings.Repeat("x", 9*1024*1024)
	path := writeConversationRollout(t, home, thread, []string{
		conversationRolloutLine(t, assistantMessageLine("msg-before", "before the flood")),
		`{"type":"response_item","payload":{"type":"message","id":"msg-big","role":"assistant","content":[{"type":"output_text","text":"` + big + `"}]}}`,
		conversationRolloutLine(t, assistantMessageLine("msg-after", "after the flood")),
	})
	tailer := newInteractiveConversationTailer(home, func() string { return thread })
	var events []agentEvent
	for range 16 {
		batch, more := tailer.sweepCapped()
		events = append(events, batch...)
		if !more {
			break
		}
	}
	var texts []string
	for _, ev := range events {
		if at, ok := ev.(agent.AssistantTextEvent); ok {
			texts = append(texts, at.Text)
		}
	}
	for _, want := range []string{"before the flood", "after the flood"} {
		found := false
		for _, got := range texts {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("texts = %v (path %s); want %q delivered despite the overlong line", texts, path, want)
		}
	}
}

func TestInteractiveConversationTailer_SubagentFilesDeliver(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const parent, sub = "thread-parent-conv", "thread-sub-conv"
	writeConversationRollout(t, home, parent, []string{
		`{"type":"session_meta","payload":{"id":"` + parent + `"}}`,
		conversationRolloutLine(t, assistantMessageLine("msg-p", "parent turn")),
	})
	writeConversationRollout(t, home, sub, []string{
		subagentMetaLine(sub, parent),
		conversationRolloutLine(t, assistantMessageLine("msg-s", "subagent turn")),
	})
	tailer := newInteractiveConversationTailer(home, func() string { return parent })
	events, _ := tailer.sweepCapped()
	var texts []string
	for _, ev := range events {
		if text, ok := ev.(agent.AssistantTextEvent); ok {
			texts = append(texts, text.Text)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("delivered texts = %v; want both parent and subagent turns", texts)
	}
}

func appendConversationRollout(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test-owned fixture
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("append rollout: %v", err)
		}
	}
}

// TestWrapInteractiveConversationHandle_ForwardsRolloutConversation drives
// the production SpawnInteractive entry point end to end: the fake PTY
// client creates the fixture thread (as the real CLI would), a real-shaped
// rollout is planted in the spawned home before the child exits, and the
// conversation tailed from it arrives on Events as assistant/tool events
// ahead of the terminal ResultEvent.
func TestWrapInteractiveConversationHandle_ForwardsRolloutConversation(t *testing.T) {
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	// The fake app-server always creates the same thread id; plant a
	// real-shaped rollout for exactly that thread in the spawned home.
	plant := func(home string) {
		dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := conversationRolloutLine(t, assistantMessageLine("msg-live", "live codex turn")) + "\n" +
			conversationRolloutLine(t, map[string]any{
				"type": "response_item",
				"payload": map[string]any{
					"type": "custom_tool_call", "id": "ctc-live", "status": "running",
					"call_id": "call-live", "name": "exec",
					"input": `text(await tools.exec_command({cmd:"pwd",` + "\"max_output_tokens\":100}));\n",
				},
			}) + "\n" +
			conversationRolloutLine(t, map[string]any{
				"type": "event_msg",
				"payload": map[string]any{
					"type": "item_completed", "thread_id": fakeNamedAppServerThreadID, "turn_id": "turn-live",
					"item": map[string]any{
						"type": "CommandExecution", "id": "exec-live",
						"command":           []any{"/bin/zsh", "-lc", "pwd"},
						"status":            "completed",
						"exit_code":         0,
						"aggregated_output": "/tmp\n",
					},
				},
			}) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+fakeNamedAppServerThreadID+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := spawnInteractiveForUsageTest(t, self, boundaryRoot, workdir, mcpServers, nil, plant)
	wrapper, ok := h.(*interactiveConversationHandle)
	if !ok {
		t.Fatalf("SpawnInteractive returned %T; want *interactiveConversationHandle", h)
	}
	var texts, tools []string
	var terminal *agent.ResultEvent
	timeout := time.After(20 * time.Second)
drain:
	for {
		select {
		case ev, ok := <-wrapper.Events():
			if !ok {
				break drain
			}
			switch v := ev.(type) {
			case agent.AssistantTextEvent:
				texts = append(texts, v.Text)
			case agent.ToolUseEvent:
				tools = append(tools, v.ToolName)
			case agent.ResultEvent:
				cp := v
				terminal = &cp
			}
			if terminal != nil {
				break drain
			}
		case <-timeout:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
	if len(texts) != 1 || texts[0] != "live codex turn" {
		t.Fatalf("conversation texts = %v; want [live codex turn]", texts)
	}
	if len(tools) != 1 || tools[0] != "shell" {
		t.Fatalf("tool uses = %v; want [shell]", tools)
	}
	if terminal == nil || !terminal.Success {
		t.Fatalf("terminal = %+v; want a successful ResultEvent", terminal)
	}
}
