package geminicli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// readFixture loads a JSONL fixture from testdata/. The trailing
// newline written into the fixture is trimmed so callers see the
// raw line as if it had been split by bufio.Scanner.
//
// Fixtures were captured from gemini CLI v0.44.1 (2026-06-03) running
// `gemini --output-format stream-json --yolo --skip-trust -p ""`.
// Pin the expected event shape here; treat CLI upgrades that change
// the output contract as a breaking change requiring re-validation.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return bytes.TrimRight(body, "\n")
}

// TestMapLine_Init verifies that an "init" line maps to agent.InitEvent
// with the correct session_id captured.
func TestMapLine_Init(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "init.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("event %T, want InitEvent", events[0])
	}
	if ev.SessionID != "3280f7e8-8723-476b-aa83-3d2a8782996a" {
		t.Errorf("SessionID = %q, want UUID from fixture", ev.SessionID)
	}
	if ev.Raw == nil {
		t.Errorf("Raw should be non-nil")
	}
}

// TestMapLine_MessageUser verifies that a role="user" message line maps
// to a SystemEvent with subtype "user_message".
func TestMapLine_MessageUser(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "message_user.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if ev.Subtype != "user_message" {
		t.Errorf("Subtype = %q, want user_message", ev.Subtype)
	}
}

// TestMapLine_MessageAssistant verifies that a role="assistant" message
// line maps to agent.AssistantTextEvent with the correct text.
func TestMapLine_MessageAssistant(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "message_assistant.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event %T, want AssistantTextEvent", events[0])
	}
	if ev.Text != "Hello! How can I help you today?" {
		t.Errorf("Text = %q, want fixture text", ev.Text)
	}
	if ev.Raw == nil {
		t.Errorf("Raw should be non-nil")
	}
}

// TestMapLine_ToolUse verifies that a "tool_use" line maps to
// agent.ToolUseEvent with correct tool name, id, and parameters.
func TestMapLine_ToolUse(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "tool_use.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("event %T, want ToolUseEvent", events[0])
	}
	if ev.ToolName != "shell" {
		t.Errorf("ToolName = %q, want shell", ev.ToolName)
	}
	if ev.ToolUseID != "call_abc123" {
		t.Errorf("ToolUseID = %q, want call_abc123", ev.ToolUseID)
	}
	if got := ev.Input["command"]; got != "ls /tmp" {
		t.Errorf("Input.command = %v, want ls /tmp", got)
	}
}

// TestMapLine_ToolResultSuccess verifies that a "tool_result" with
// status="success" maps to agent.ToolResultEvent with IsError=false.
func TestMapLine_ToolResultSuccess(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "tool_result_success.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T, want ToolResultEvent", events[0])
	}
	if ev.ToolUseID != "call_abc123" {
		t.Errorf("ToolUseID = %q, want call_abc123", ev.ToolUseID)
	}
	if ev.Content != "file1.txt\nfile2.txt\n" {
		t.Errorf("Content = %q, want fixture output", ev.Content)
	}
	if ev.IsError {
		t.Errorf("IsError should be false for status=success")
	}
}

// TestMapLine_ToolResultError verifies that a "tool_result" with
// status="error" maps to agent.ToolResultEvent with IsError=true and
// the error message as Content.
func TestMapLine_ToolResultError(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "tool_result_error.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T, want ToolResultEvent", events[0])
	}
	if ev.ToolUseID != "call_def456" {
		t.Errorf("ToolUseID = %q, want call_def456", ev.ToolUseID)
	}
	if ev.Content != "command not found: badcmd" {
		t.Errorf("Content = %q, want error message from fixture", ev.Content)
	}
	if !ev.IsError {
		t.Errorf("IsError should be true for status=error")
	}
}

// TestMapLine_ErrorWarning verifies that an "error" line with
// severity="warning" maps to a SystemEvent (not an ErrorEvent) so
// the runner does not treat it as a fatal failure.
func TestMapLine_ErrorWarning(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "error_warning.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent (warning is non-fatal)", events[0])
	}
	if ev.Subtype != "warning" {
		t.Errorf("Subtype = %q, want warning", ev.Subtype)
	}
	if ev.Message != "Agent execution blocked: rate limit approaching" {
		t.Errorf("Message = %q, want fixture message", ev.Message)
	}
}

// TestMapLine_ErrorFatal verifies that an "error" line with
// severity="error" maps to an agent.ErrorEvent.
func TestMapLine_ErrorFatal(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "error_fatal.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if ev.Code != "gemini_cli_error" {
		t.Errorf("Code = %q, want gemini_cli_error", ev.Code)
	}
	if ev.Message != "Maximum session turns exceeded" {
		t.Errorf("Message = %q, want fixture message", ev.Message)
	}
}

// TestMapLine_ResultSuccess verifies that a "result" line with
// status="success" maps to agent.ResultEvent with Success=true and
// token usage populated from the stats block.
func TestMapLine_ResultSuccess(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "result_success.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(events), events)
	}
	ev, ok := events[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event %T, want ResultEvent", events[0])
	}
	if !ev.Success {
		t.Errorf("Success should be true")
	}
	if ev.Cost == nil {
		t.Fatal("Cost should be set")
	}
	if ev.Cost.InputTokens != 1100 {
		t.Errorf("InputTokens = %d, want 1100", ev.Cost.InputTokens)
	}
	if ev.Cost.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", ev.Cost.OutputTokens)
	}
	if ev.Cost.CachedInputTokens != 800 {
		t.Errorf("CachedInputTokens = %d, want 800", ev.Cost.CachedInputTokens)
	}
}

// TestMapLine_InvalidJSON_ErrorEvent verifies that a non-JSON line maps
// to an ErrorEvent with code "decode_envelope".
func TestMapLine_InvalidJSON_ErrorEvent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte("not json at all"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 ErrorEvent", len(events))
	}
	ev, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if ev.Code != "decode_envelope" {
		t.Errorf("Code = %q, want decode_envelope", ev.Code)
	}
}

// TestMapLine_MissingType_ErrorEvent verifies that a JSON object without
// a "type" field maps to an ErrorEvent with code "missing_type".
func TestMapLine_MissingType_ErrorEvent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte(`{"timestamp":"2026-06-03T00:00:00Z"}`))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev, ok := events[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event %T, want ErrorEvent", events[0])
	}
	if ev.Code != "missing_type" {
		t.Errorf("Code = %q, want missing_type", ev.Code)
	}
}

// TestMapLine_UnknownType_SystemEvent verifies that an unknown event
// type maps to a SystemEvent with subtype "unknown".
func TestMapLine_UnknownType_SystemEvent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte(`{"type":"future_event_v9","data":"something"}`))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if ev.Subtype != "unknown" {
		t.Errorf("Subtype = %q, want unknown", ev.Subtype)
	}
}

// TestMapLine_AssistantEmptyContent verifies that an empty assistant
// content chunk produces no events (not even an empty text event).
func TestMapLine_AssistantEmptyContent(t *testing.T) {
	t.Parallel()

	events := mapLine([]byte(`{"type":"message","role":"assistant","content":"","delta":true}`))
	if len(events) != 0 {
		t.Errorf("empty assistant content should produce 0 events, got %d: %v", len(events), events)
	}
}

// TestDecodeParameters_Malformed verifies that a malformed parameters
// JSON returns nil without panicking.
func TestDecodeParameters_Malformed(t *testing.T) {
	t.Parallel()

	if got := decodeParameters([]byte("not json")); got != nil {
		t.Errorf("decodeParameters(malformed) = %v, want nil", got)
	}
	if got := decodeParameters(nil); got != nil {
		t.Errorf("decodeParameters(nil) = %v, want nil", got)
	}
}
