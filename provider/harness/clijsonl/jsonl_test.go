package clijsonl

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
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
	events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
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

func TestMapLine_AssistantPerCallUsage(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"assistant","message":{"model":"claude-opus-4","stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":80},"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}]}}`)
	events := mapLine(line)
	if len(events) != 2 {
		t.Fatalf("got %d events, want LlmCallEvent + ToolUseEvent", len(events))
	}
	llm, ok := events[0].(agent.LlmCallEvent)
	if !ok {
		t.Fatalf("event[0] %T, want LlmCallEvent", events[0])
	}
	if llm.Model != "claude-opus-4" || llm.ResponseModel != "claude-opus-4" || llm.InputTokens != 120 || llm.OutputTokens != 30 || llm.CachedInputTokens != 80 {
		t.Fatalf("unexpected usage event: %+v", llm)
	}
	if llm.UsageSource != agent.LlmUsageProvider || llm.Synthetic {
		t.Fatalf("unexpected usage provenance: %+v", llm)
	}
	encoded, err := agent.MarshalEvent(llm)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if bytes.Contains(encoded, []byte("README.md")) || bytes.Contains(encoded, []byte(`"raw"`)) {
		t.Fatalf("LLM metadata event leaked provider content: %s", encoded)
	}
	if _, ok := events[1].(agent.ToolUseEvent); !ok {
		t.Fatalf("event[1] %T, want ToolUseEvent", events[1])
	}
}

func TestMapLine_AssistantToolUse(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_tool_use.jsonl"))
	events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
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
	events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
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

// Claude extended-thinking blocks must surface as
// SystemEvent{Subtype: "reasoning"} — the activity poster whitelists
// exactly that subtype and forwards it as a "thought" activity (parity
// with codex completed reasoning items). They must never become
// AssistantTextEvent: the runner scans assistant text for verdict
// markers.
func TestMapLine_AssistantThinking(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_thinking.jsonl"))
	events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (reasoning + text): %v", len(events), events)
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event[0] %T, want SystemEvent", events[0])
	}
	if sys.Subtype != "reasoning" {
		t.Errorf("Subtype = %q, want %q", sys.Subtype, "reasoning")
	}
	if sys.Message != "The user wants the file listed; ls is sufficient." {
		t.Errorf("Message = %q, want fixture thinking text", sys.Message)
	}
	if sys.Raw == nil {
		t.Errorf("Raw should be non-nil")
	}
	at, ok := events[1].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event[1] %T, want AssistantTextEvent", events[1])
	}
	if at.Text != "Listing the directory." {
		t.Errorf("Text = %q, want fixture text", at.Text)
	}
}

// redacted_thinking carries encrypted, non-renderable reasoning — it is
// passed over without emitting an event (and without an error).
func TestMapLine_AssistantRedactedThinking(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "assistant_redacted_thinking.jsonl"))
	events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (text only): %v", len(events), events)
	}
	at, ok := events[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event[0] %T, want AssistantTextEvent", events[0])
	}
	if at.Text != "Done." {
		t.Errorf("Text = %q, want fixture text", at.Text)
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

func TestMapLine_StreamEvent_APIError_ProviderError(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "api_error_event.jsonl"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	sys, ok := events[0].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event %T, want SystemEvent", events[0])
	}
	if sys.Subtype != agent.SystemSubtypeProviderError {
		t.Errorf("Subtype = %q, want %q", sys.Subtype, agent.SystemSubtypeProviderError)
	}
	if sys.Message != "Overloaded" {
		t.Errorf("Message = %q, want Overloaded", sys.Message)
	}
	if sys.Upstream == nil {
		t.Fatal("Upstream is nil, want the endpoint's structured error")
	}
	if sys.Upstream.HTTPStatus != 529 {
		t.Errorf("Upstream.HTTPStatus = %d, want 529", sys.Upstream.HTTPStatus)
	}
	if sys.Upstream.ProviderCode != "overloaded_error" {
		t.Errorf("Upstream.ProviderCode = %q, want overloaded_error", sys.Upstream.ProviderCode)
	}
	// A non-error frame still maps to nothing.
	if events := mapLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta"}}`)); len(events) != 0 {
		t.Errorf("non-error stream_event produced %d events, want 0", len(events))
	}
}

func TestMapLine_ResultUpstreamError(t *testing.T) {
	t.Parallel()

	events := mapLine(readFixture(t, "result_upstream_error.jsonl"))
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
	if r.Upstream == nil {
		t.Fatal("Upstream is nil, want the endpoint's structured error")
	}
	if r.Upstream.HTTPStatus != 429 {
		t.Errorf("Upstream.HTTPStatus = %d, want 429", r.Upstream.HTTPStatus)
	}
	if r.Upstream.ProviderCode != "usage_limit" {
		t.Errorf("Upstream.ProviderCode = %q, want usage_limit", r.Upstream.ProviderCode)
	}
	if r.Upstream.ProviderMessage != "Rate limited: quota exhausted" {
		t.Errorf("Upstream.ProviderMessage = %q", r.Upstream.ProviderMessage)
	}
	if r.Upstream.ResetAt != "1777673400" {
		t.Errorf("Upstream.ResetAt = %q, want 1777673400", r.Upstream.ResetAt)
	}
	// A success result carries no upstream error.
	for _, ev := range mapLine(readFixture(t, "result_success.jsonl")) {
		if r, ok := ev.(agent.ResultEvent); ok && r.Upstream != nil {
			t.Errorf("success ResultEvent carries Upstream = %+v, want nil", r.Upstream)
		}
	}
	// An error result without structured fields carries none either.
	for _, ev := range mapLine(readFixture(t, "result_error.jsonl")) {
		if r, ok := ev.(agent.ResultEvent); ok && r.Upstream != nil {
			t.Errorf("plain error ResultEvent carries Upstream = %+v, want nil", r.Upstream)
		}
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

func TestMapLine_RateLimitEvent_UsageWindow(t *testing.T) {
	t.Parallel()

	// A streamed event naming a window with a utilization fraction
	// maps onto the shared quota-window type the daemon publishes.
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.85,"resetsAt":1784000000}}`)
	events := mapLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	usage, ok := events[0].(agent.UsageEvent)
	if !ok {
		t.Fatalf("event %T, want UsageEvent", events[0])
	}
	if usage.Usage == nil || len(usage.Usage.Windows) != 1 {
		t.Fatalf("usage = %+v, want one window", usage.Usage)
	}
	w := usage.Usage.Windows[0]
	if w.ID != "seven_day" || w.Kind != agent.UsageWindowWeekly {
		t.Errorf("window = %+v, want seven_day weekly", w)
	}
	if w.UsedPercent != 85 {
		t.Errorf("UsedPercent = %v, want 85", w.UsedPercent)
	}
	if w.ResetsAt == "" {
		t.Error("window carries no reset time")
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

func TestResponseModelFromNativeMessageWithoutUsage(t *testing.T) {
	t.Parallel()
	events := mapLine([]byte(`{"type":"assistant","message":{"model":"native-response-id","content":[]}}`))
	if len(events) != 1 {
		t.Fatalf("response identity was dropped without usage: %+v", events)
	}
	observation, ok := events[0].(agent.SystemEvent)
	if !ok || observation.Subtype != agent.SystemSubtypeModelIdentity || observation.ObservedModel == nil || observation.ObservedModel.Model != "native-response-id" || observation.ObservedModel.Provider != "" || observation.ObservedModel.Version != "" {
		t.Fatalf("response identity = %+v; provider/version must remain unknown", events[0])
	}
}

// The native model observation precedes the existing content projections.
// Validate it strictly before asserting the original content sequence below.
func requireResponseModelPrefix(t *testing.T, events []agent.Event, model string) []agent.Event {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("native response model observation missing")
	}
	observation, ok := events[0].(agent.SystemEvent)
	if !ok || observation.Subtype != agent.SystemSubtypeModelIdentity || observation.ObservedModel == nil || observation.ObservedModel.Model != model || observation.ObservedModel.Provider != "" || observation.ObservedModel.Version != "" {
		t.Fatalf("native response model prefix = %+v, want model %q and unknown provider/version", events[0], model)
	}
	return events[1:]
}

func TestNativeModelObservationPreservesTerminalAggregateUsage(t *testing.T) {
	t.Parallel()
	var spans []agent.Span
	processor, err := spanruntime.NewProcessor(spanruntime.ProcessorConfig{SessionID: "local-session", OrgID: "local", WorkspaceID: "local", System: "configured", Model: "requested-alias", Sender: spanruntime.SendFunc(func(s agent.Span) bool { spans = append(spans, s); return true })})
	if err != nil {
		t.Fatal(err)
	}
	var observation *agent.ObservedModelIdentity
	for _, ev := range mapLine([]byte(`{"type":"assistant","message":{"model":"native-response-id","content":[{"type":"text","text":"done"}]}}`)) {
		for _, processed := range processor.Process(ev) {
			if status, ok := processed.(agent.SystemEvent); ok && status.ObservedModel != nil {
				observation = status.ObservedModel
			}
			if call, ok := processed.(agent.LlmCallEvent); ok && call.ResponseModel != "" {
				observation = &agent.ObservedModelIdentity{Model: call.ResponseModel, Provider: call.ResponseModelProvider, Version: call.ModelSnapshotID}
			}
		}
	}
	preTerminalSpans := len(spans)
	if observation == nil || observation.Model != "native-response-id" {
		t.Fatal("native response model observation was lost")
	}
	var aggregate *agent.LlmCallEvent
	var terminal *agent.ResultEvent
	for _, ev := range mapLine([]byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":1.25,"usage":{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":80}}`)) {
		for _, processed := range processor.Process(ev) {
			if call, ok := processed.(agent.LlmCallEvent); ok {
				aggregate = &call
			}
			if res, ok := processed.(agent.ResultEvent); ok {
				terminal = &res
			}
		}
	}
	if aggregate == nil || !aggregate.Synthetic || aggregate.UsageSource != agent.LlmUsageAggregate || aggregate.InputTokens != 120 || aggregate.OutputTokens != 30 || aggregate.CachedInputTokens != 80 {
		t.Fatalf("terminal aggregate usage was suppressed/changed: %+v", aggregate)
	}
	if preTerminalSpans != 0 {
		t.Fatalf("metadata-only response emitted %d usage spans before terminal", preTerminalSpans)
	}
	if terminal == nil || terminal.Cost == nil || terminal.Cost.TotalCostUsd != 1.25 || terminal.Cost.NumTurns != 3 {
		t.Fatalf("terminal cost/turns changed: %+v", terminal)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want one terminal aggregate LLM span", len(spans))
	}
	llm, ok := spans[0].(agent.LlmCallSpan)
	if !ok || llm.GenAI.UsageInputTokens != 120 || llm.GenAI.UsageOutputTokens != 30 || llm.GenAI.UsageCacheReadInputTokens != 80 {
		t.Fatalf("terminal aggregate span changed: %+v", spans[0])
	}
}

func TestObservedClaudeResultAvailability(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line      string
		wantCost  bool
		wantTurns bool
	}{
		{"reported zero", `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0,"num_turns":0}`, true, true},
		{"missing values", `{"type":"result","subtype":"success","is_error":false}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := mapLine([]byte(tc.line))
			if len(events) != 1 {
				t.Fatalf("events=%d", len(events))
			}
			result := events[0].(agent.ResultEvent)
			if (result.ObservedCostUsd != nil) != tc.wantCost || (result.ObservedTurns != nil) != tc.wantTurns {
				t.Fatalf("availability confused: %+v", result)
			}
			if result.ObservedCostUsd != nil && *result.ObservedCostUsd != 0 {
				t.Fatalf("reported cost changed: %+v", result)
			}
		})
	}
}

// A nested sub-agent delegation carries the delegating call's id on every
// message emitted inside it: assistant text, tool use, and tool result all
// surface the same ParentToolUseID, while the top-level delegating call
// itself stays empty. Table-driven on the raw stream-json lines so removing
// the mapping turns the assertions RED.
func TestMapLine_SubagentNestedParentToolUseID(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "subagent_nested.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}

	// The delegating call is top-level: no parent id.
	parent := mapLine(lines[0])
	parent = requireResponseModelPrefix(t, parent, "claude-opus-4-7")
	if len(parent) != 1 {
		t.Fatalf("parent line: got %d events, want 1", len(parent))
	}
	tu, ok := parent[0].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("parent event %T, want ToolUseEvent", parent[0])
	}
	if tu.ToolUseID != "toolu_parent" {
		t.Fatalf("parent ToolUseID = %q, want toolu_parent", tu.ToolUseID)
	}
	if tu.ParentToolUseID != "" {
		t.Fatalf("parent ParentToolUseID = %q, want empty", tu.ParentToolUseID)
	}

	// Everything emitted inside the sub-agent names the delegating call.
	cases := []struct {
		name      string
		line      []byte
		wantKind  agent.EventKind
		wantID    string
		wantEmpty bool
	}{
		{"assistant text", lines[1], agent.EventAssistantText, "", false},
		{"tool use", lines[2], agent.EventToolUse, "toolu_child", false},
		{"tool result", lines[3], agent.EventToolResult, "toolu_child", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := mapLine(tc.line)
			if tc.wantKind == agent.EventAssistantText || tc.wantKind == agent.EventToolUse {
				events = requireResponseModelPrefix(t, events, "claude-opus-4-7")
			}
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			var got string
			switch ev := events[0].(type) {
			case agent.AssistantTextEvent:
				got = ev.ParentToolUseID
			case agent.ToolUseEvent:
				got = ev.ParentToolUseID
				if ev.ToolUseID != tc.wantID {
					t.Fatalf("ToolUseID = %q, want %q", ev.ToolUseID, tc.wantID)
				}
			case agent.ToolResultEvent:
				got = ev.ParentToolUseID
				if ev.ToolUseID != tc.wantID {
					t.Fatalf("ToolUseID = %q, want %q", ev.ToolUseID, tc.wantID)
				}
			default:
				t.Fatalf("event %T, want %s", events[0], tc.wantKind)
			}
			if got != "toolu_parent" {
				t.Fatalf("ParentToolUseID = %q, want %q", got, "toolu_parent")
			}
		})
	}
}

// Lines without the delegation marker stay parentless: adapters never guess.
func TestMapLine_TopLevelLinesHaveEmptyParentToolUseID(t *testing.T) {
	t.Parallel()

	for _, line := range [][]byte{
		readFixture(t, "assistant_tool_use.jsonl"),
		readFixture(t, "assistant_mixed.jsonl"),
		readFixture(t, "assistant_text.jsonl"),
		readFixture(t, "user_tool_result.jsonl"),
	} {
		for _, ev := range mapLine(line) {
			switch ev := ev.(type) {
			case agent.AssistantTextEvent:
				if ev.ParentToolUseID != "" {
					t.Fatalf("AssistantText ParentToolUseID = %q, want empty", ev.ParentToolUseID)
				}
			case agent.ToolUseEvent:
				if ev.ParentToolUseID != "" {
					t.Fatalf("ToolUse ParentToolUseID = %q, want empty", ev.ParentToolUseID)
				}
			case agent.ToolResultEvent:
				if ev.ParentToolUseID != "" {
					t.Fatalf("ToolResult ParentToolUseID = %q, want empty", ev.ParentToolUseID)
				}
			}
		}
	}
}

// A fixture with a native delegation (Agent) call emits the typed
// sub-agent lifecycle: `started` on the tool_use line, then
// `completed` on the matching successful result and `failed` on the
// matching error result. Table-driven on the raw stream-json lines so
// removing the emission turns the assertions RED.
func TestLineMapper_SubagentLifecycleFromFixture(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "subagent_agent_call.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}

	var mapper LineMapper
	got := make([]agent.Event, 0, 8)
	for _, line := range lines {
		got = append(got, mapper.MapLine(line)...)
	}

	var phases []agent.SubagentPhase
	var ids []string
	for _, ev := range got {
		sub, ok := ev.(agent.SubagentEvent)
		if !ok {
			continue
		}
		phases = append(phases, sub.Phase)
		ids = append(ids, sub.ToolUseID)
		if sub.ToolName != "Agent" {
			t.Fatalf("SubagentEvent ToolName = %q, want Agent", sub.ToolName)
		}
	}
	wantPhases := []agent.SubagentPhase{agent.SubagentStarted, agent.SubagentCompleted, agent.SubagentStarted, agent.SubagentFailed}
	wantIDs := []string{"toolu_agent_1", "toolu_agent_1", "toolu_agent_2", "toolu_agent_2"}
	if !reflect.DeepEqual(phases, wantPhases) {
		t.Fatalf("subagent phases = %v, want %v", phases, wantPhases)
	}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("subagent toolUseIds = %v, want %v", ids, wantIDs)
	}
}

// The stateless line decoder stays name-blind: a raw delegation tool_use
// line decodes to plain tool events only, with no typed lifecycle. The
// stateful mapper adds the lifecycle; removing it turns the lifecycle
// test RED while this pin stays GREEN.
func TestMapLine_DelegationToolUseHasNoSubagentEvent(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "subagent_agent_call.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte("\n"))
	for _, ev := range mapLine(lines[0]) {
		if _, ok := ev.(agent.SubagentEvent); ok {
			t.Fatalf("stateless mapLine emitted SubagentEvent: %#v", ev)
		}
	}
}

// Non-delegation tool calls never emit a lifecycle, even across the
// matching result line.
func TestLineMapper_NonDelegationToolsEmitNoSubagentEvent(t *testing.T) {
	t.Parallel()

	var mapper LineMapper
	assistant := []byte(`{"type":"assistant","session_id":"s","message":{"model":"m","id":"msg","role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls"}}]}}`)
	user := []byte(`{"type":"user","session_id":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bash","content":"ok","is_error":false}]}}`)
	for _, ev := range append(mapper.MapLine(assistant), mapper.MapLine(user)...) {
		if _, ok := ev.(agent.SubagentEvent); ok {
			t.Fatalf("non-delegation tool emitted SubagentEvent: %#v", ev)
		}
	}
}

// TestMapLine_CacheWriteTokens pins the Anthropic cache-write bucket on both
// usage carriers: a complete assistant message's per-call usage and the
// terminal result's session usage. input_tokens excludes both cache classes,
// so cache_creation_input_tokens must ride CacheWriteTokens; before it was
// read, every cache write a session made (billed above the input price) was
// dropped before the status post.
func TestMapLine_CacheWriteTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want agent.CostData
	}{
		{
			name: "assistant usage carries every class",
			line: `{"type":"assistant","message":{"model":"claude-opus-4","stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":30,"cache_read_input_tokens":16526,"cache_creation_input_tokens":1200},"content":[{"type":"text","text":"done"}]}}`,
			want: agent.CostData{InputTokens: 4, OutputTokens: 30, CachedInputTokens: 16526, CacheWriteTokens: 1200},
		},
		{
			name: "assistant usage with only a cache write is still usage",
			line: `{"type":"assistant","message":{"model":"claude-opus-4","usage":{"cache_creation_input_tokens":900},"content":[]}}`,
			want: agent.CostData{CacheWriteTokens: 900},
		},
		{
			name: "assistant usage without a cache write reports none",
			line: `{"type":"assistant","message":{"model":"claude-opus-4","stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":30,"cache_read_input_tokens":80},"content":[]}}`,
			want: agent.CostData{InputTokens: 4, OutputTokens: 30, CachedInputTokens: 80},
		},
		{
			name: "result usage carries every class",
			line: `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":1.25,"usage":{"input_tokens":12,"output_tokens":900,"cache_read_input_tokens":250000,"cache_creation_input_tokens":42000}}`,
			want: agent.CostData{InputTokens: 12, OutputTokens: 900, CachedInputTokens: 250000, CacheWriteTokens: 42000, TotalCostUsd: 1.25, NumTurns: 3},
		},
		{
			name: "result usage without a cache write reports none",
			line: `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{"input_tokens":5,"output_tokens":8,"cache_read_input_tokens":16526}}`,
			want: agent.CostData{InputTokens: 5, OutputTokens: 8, CachedInputTokens: 16526, NumTurns: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got *agent.CostData
			for _, ev := range mapLine([]byte(tt.line)) {
				switch e := ev.(type) {
				case agent.LlmCallEvent:
					got = &agent.CostData{
						InputTokens: e.InputTokens, OutputTokens: e.OutputTokens,
						CachedInputTokens: e.CachedInputTokens, CacheWriteTokens: e.CacheWriteTokens,
					}
				case agent.ResultEvent:
					got = e.Cost
				}
			}
			if got == nil {
				t.Fatalf("no usage-carrying event mapped from %s", tt.line)
			}
			if *got != tt.want {
				t.Errorf("usage = %+v; want %+v", *got, tt.want)
			}
		})
	}
}
