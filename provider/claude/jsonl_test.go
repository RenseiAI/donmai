package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// readFixture loads a JSONL fixture from testdata/. The trailing
// newline written into the fixture is trimmed so callers see the
// raw line as if it had been split by bufio.Scanner.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return bytes.TrimRight(body, "\n")
}

func TestMapLine_SystemInit(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "system_init.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	init, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("event %T, want InitEvent", events[0])
	}
	if init.SessionID != "568bf4dd-3dc8-4750-b7d8-b3b24919c6d2" {
		t.Errorf("SessionID = %q, want UUID from fixture", init.SessionID)
	}
	if init.Raw == nil {
		t.Errorf("Raw should be non-nil")
	}
}

func TestMapLine_SystemOther(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "system_other.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "compaction" {
		t.Errorf("Subtype = %q, want %q", sys.Subtype, "compaction")
	}
	if sys.Message != "compacted history" {
		t.Errorf("Message = %q, want status field", sys.Message)
	}
}

func TestMapLine_AssistantText(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_text.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	at, ok := events[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event %T, want AssistantTextEvent", events[0])
	}
	if at.Text != "Hi! I'll help with that." {
		t.Errorf("Text = %q, want fixture text", at.Text)
	}
}

func TestMapLine_AssistantToolUse(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_tool_use.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	tu, ok := events[0].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("event %T, want ToolUseEvent", events[0])
	}
	if tu.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", tu.ToolName)
	}
	if tu.ToolUseID != "toolu_abc" {
		t.Errorf("ToolUseID = %q, want toolu_abc", tu.ToolUseID)
	}
	if got := tu.Input["command"]; got != "ls /tmp" {
		t.Errorf("Input.command = %v, want ls /tmp", got)
	}
}

func TestMapLine_AssistantMixed(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_mixed.jsonl"))
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (text + tool_use): %v", len(events), events)
	}
	if _, ok := events[0].(agent.AssistantTextEvent); !ok {
		t.Errorf("event[0] %T, want AssistantTextEvent", events[0])
	}
	if _, ok := events[1].(agent.ToolUseEvent); !ok {
		t.Errorf("event[1] %T, want ToolUseEvent", events[1])
	}
}

func TestMapLine_UserToolResult(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "user_tool_result.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	tr, ok := events[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T, want ToolResultEvent", events[0])
	}
	if tr.ToolUseID != "toolu_abc" {
		t.Errorf("ToolUseID = %q, want toolu_abc", tr.ToolUseID)
	}
	if tr.Content != "file1\nfile2" {
		t.Errorf("Content = %q, want file1\\nfile2", tr.Content)
	}
	if tr.IsError {
		t.Errorf("IsError should be false")
	}
}

func TestMapLine_ResultSuccess(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "result_success.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	r, ok := events[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event %T, want ResultEvent", events[0])
	}
	if !r.Success {
		t.Errorf("Success should be true")
	}
	if r.Message != "Done." {
		t.Errorf("Message = %q, want Done.", r.Message)
	}
	if r.Cost == nil {
		t.Fatal("Cost should be set")
	}
	if r.Cost.TotalCostUsd != 0.117 {
		t.Errorf("TotalCostUsd = %f, want 0.117", r.Cost.TotalCostUsd)
	}
	if r.Cost.InputTokens != 5 {
		t.Errorf("InputTokens = %d, want 5", r.Cost.InputTokens)
	}
	if r.Cost.OutputTokens != 8 {
		t.Errorf("OutputTokens = %d, want 8", r.Cost.OutputTokens)
	}
	if r.Cost.CachedInputTokens != 16526 {
		t.Errorf("CachedInputTokens = %d, want 16526", r.Cost.CachedInputTokens)
	}
	if r.Cost.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", r.Cost.NumTurns)
	}
}

func TestMapLine_ResultError(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "result_error.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	r, ok := events[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event %T, want ResultEvent", events[0])
	}
	if r.Success {
		t.Errorf("Success should be false")
	}
	if r.ErrorSubtype != "error_max_turns" {
		t.Errorf("ErrorSubtype = %q, want error_max_turns", r.ErrorSubtype)
	}
	if !reflect.DeepEqual(r.Errors, []string{"max turns exceeded"}) {
		t.Errorf("Errors = %v, want [max turns exceeded]", r.Errors)
	}
	if r.Cost == nil || r.Cost.TotalCostUsd != 1.234 {
		t.Errorf("Cost should still be populated on error result, got %+v", r.Cost)
	}
}

func TestMapLine_ToolProgress(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "tool_progress.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	tp, ok := events[0].(agent.ToolProgressEvent)
	if !ok {
		t.Fatalf("event %T, want ToolProgressEvent", events[0])
	}
	if tp.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", tp.ToolName)
	}
	if tp.ElapsedSeconds != 4.2 {
		t.Errorf("ElapsedSeconds = %f, want 4.2", tp.ElapsedSeconds)
	}
}

func TestMapLine_AuthStatus_Authenticated(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "auth_status.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "auth_status" {
		t.Errorf("Subtype = %q, want auth_status", sys.Subtype)
	}
	if sys.Message != "Authenticated" {
		t.Errorf("Message = %q, want Authenticated", sys.Message)
	}
}

func TestMapLine_AuthStatus_Error(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "auth_status_error.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	er, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if er.Message != "OAuth token revoked" {
		t.Errorf("Message = %q, want OAuth token revoked", er.Message)
	}
	if er.Code != "auth_status" {
		t.Errorf("Code = %q, want auth_status", er.Code)
	}
}

func TestMapLine_StreamEvent_Dropped(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "stream_event.jsonl"))
	if len(events) != 0 {
		t.Errorf("stream_event should produce 0 events, got %d", len(events))
	}
}

func TestMapLine_RateLimitEvent_System(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "rate_limit_event.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "rate_limit" {
		t.Errorf("Subtype = %q, want rate_limit", sys.Subtype)
	}
}

func TestMapLine_InvalidJSON_ErrorEvent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte("not json"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 ErrorEvent", len(events))
	}
	er, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if er.Code != "decode_envelope" {
		t.Errorf("Code = %q, want decode_envelope", er.Code)
	}
}

func TestMapLine_MissingType_ErrorEvent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte(`{"foo":"bar"}`))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	er, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if er.Code != "missing_type" {
		t.Errorf("Code = %q, want missing_type", er.Code)
	}
}

func TestMapLine_UnknownType_System(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte(`{"type":"completely_new_thing","foo":"bar"}`))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "unknown" {
		t.Errorf("Subtype = %q, want unknown", sys.Subtype)
	}
}

func TestMapLine_UserMessage_NoToolResults(t *testing.T) {
	t.Parallel()

	// User message without any tool_result blocks should still emit
	// a generic system event so the runner sees it (legacy parity).
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`)
	events := mapLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "user_message" {
		t.Errorf("Subtype = %q, want user_message", sys.Subtype)
	}
}

func TestMapLine_ToolResult_StringifiedContent(t *testing.T) {
	t.Parallel()

	// Some legacy SDK paths embed content as a JSON object rather
	// than a string; verify we stringify.
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":{"a":1},"is_error":false}]}}`)
	events := mapLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	tr, ok := events[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T, want ToolResultEvent", events[0])
	}
	if tr.Content != `{"a":1}` {
		t.Errorf("Content = %q, want stringified JSON", tr.Content)
	}
}

func TestDecodeInput_MalformedReturnsNil(t *testing.T) {
	t.Parallel()

	if got := decodeInput([]byte("not json")); got != nil {
		t.Errorf("decodeInput(malformed) = %v, want nil", got)
	}
	if got := decodeInput(nil); got != nil {
		t.Errorf("decodeInput(nil) = %v, want nil", got)
	}
}
