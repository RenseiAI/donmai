package geminicli

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// rawEnvelope is the discriminator-only decode used to dispatch to a
// typed mapper. The gemini CLI stream-json events all carry a top-level
// `type` field.
//
// Source: JsonStreamEventType enum from the gemini CLI source
// (node_modules/@google/gemini-cli/bundle/chunk-GPVT36PL.js):
//
//	JsonStreamEventType["INIT"]        = "init"
//	JsonStreamEventType["MESSAGE"]     = "message"
//	JsonStreamEventType["TOOL_USE"]    = "tool_use"
//	JsonStreamEventType["TOOL_RESULT"] = "tool_result"
//	JsonStreamEventType["ERROR"]       = "error"
//	JsonStreamEventType["RESULT"]      = "result"
//
// Captured from gemini CLI v0.44.1 (2026-06-03).
type rawEnvelope struct {
	Type string `json:"type"`
}

// rawInitEvent is the init line emitted when the session starts.
//
//	{"type":"init","timestamp":"...","session_id":"<uuid>","model":"gemini-2.5-pro"}
type rawInitEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitempty"`
}

// rawMessageEvent is the message line emitted for user and assistant
// content chunks.
//
// For user input:
//
//	{"type":"message","timestamp":"...","role":"user","content":"<prompt text>"}
//
// For assistant streaming output:
//
//	{"type":"message","timestamp":"...","role":"assistant","content":"<text delta>","delta":true}
type rawMessageEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	Role      string `json:"role"`      // "user" | "assistant"
	Content   string `json:"content"`   // text content (may be a delta chunk)
	Delta     bool   `json:"delta"`     // true when this is an incremental chunk
}

// rawToolUseEvent is the line emitted when the agent invokes a tool.
//
//	{"type":"tool_use","timestamp":"...","tool_name":"shell","tool_id":"<id>","parameters":{...}}
type rawToolUseEvent struct {
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp,omitempty"`
	ToolName   string          `json:"tool_name"`
	ToolID     string          `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// rawToolResultEvent is the line emitted after a tool execution completes.
//
//	{"type":"tool_result","timestamp":"...","tool_id":"<id>","status":"success"|"error","output":"...","error":{...}}
type rawToolResultEvent struct {
	Type      string  `json:"type"`
	Timestamp string  `json:"timestamp,omitempty"`
	ToolID    string  `json:"tool_id"`
	Status    string  `json:"status"` // "success" | "error"
	Output    string  `json:"output,omitempty"`
	Error     *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// rawErrorEvent is the line emitted for non-fatal warnings and errors.
//
//	{"type":"error","timestamp":"...","severity":"warning"|"error","message":"..."}
type rawErrorEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	Severity  string `json:"severity"` // "warning" | "error"
	Message   string `json:"message"`
}

// rawResultEvent is the terminal line emitted when the session ends
// successfully.
//
//	{"type":"result","timestamp":"...","status":"success","stats":{
//	  "total_tokens":N,"input_tokens":N,"output_tokens":N,"cached":N,"duration_ms":N
//	}}
type rawResultEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	Status    string `json:"status"` // "success"
	Stats     struct {
		TotalTokens  int64 `json:"total_tokens"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		Cached       int64 `json:"cached"`
		DurationMs   int64 `json:"duration_ms,omitempty"`
		Models       map[string]struct {
			TotalTokens  int64 `json:"total_tokens"`
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"models,omitempty"`
	} `json:"stats"`
}

// mapLine decodes one JSONL line emitted by `gemini --output-format stream-json`
// and returns the resulting agent.Event values.
//
// Event mapping table (gemini CLI v0.44.1 → donmai agent.Event):
//
//	"init"        → agent.InitEvent{SessionID: session_id}
//	"message"     role="user"      → agent.SystemEvent{Subtype:"user_message"}
//	"message"     role="assistant" → agent.AssistantTextEvent{Text: content}
//	"tool_use"    → agent.ToolUseEvent{ToolName, ToolUseID: tool_id, Input: parameters}
//	"tool_result" status="success" → agent.ToolResultEvent{ToolUseID, Content: output, IsError:false}
//	"tool_result" status="error"   → agent.ToolResultEvent{ToolUseID, Content: error.message, IsError:true}
//	"error"       severity="error" → agent.ErrorEvent{Message, Code:"gemini_cli_error"}
//	"error"       severity="warning"→ agent.SystemEvent{Subtype:"warning", Message}
//	"result"      status="success" → agent.ResultEvent{Success:true, Cost:...}
//	unknown type                   → agent.SystemEvent{Subtype:"unknown"}
//	parse failure                  → agent.ErrorEvent{Code:"decode_envelope"}
//
// The `Raw` field on each event carries the original JSON line so the
// runner can persist it to <worktree>/.agent/events.jsonl per F.1.1 §4
// step 9.
func mapLine(line []byte) []agent.Event {
	var head rawEnvelope
	if err := json.Unmarshal(line, &head); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode JSONL envelope: %v", err),
			Code:    "decode_envelope",
			Raw:     json.RawMessage(line),
		}}
	}

	switch head.Type {
	case "init":
		return mapInit(line)
	case "message":
		return mapMessage(line)
	case "tool_use":
		return mapToolUse(line)
	case "tool_result":
		return mapToolResult(line)
	case "error":
		return mapError(line)
	case "result":
		return mapResult(line)
	case "":
		return []agent.Event{agent.ErrorEvent{
			Message: "provider/geminicli: JSONL line missing top-level type",
			Code:    "missing_type",
			Raw:     json.RawMessage(line),
		}}
	default:
		// Surface as a system event with subtype "unknown" so the runner
		// records it rather than silently dropping it.
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: fmt.Sprintf("Unhandled gemini CLI event type: %s", head.Type),
			Raw:     json.RawMessage(line),
		}}
	}
}

func mapInit(line []byte) []agent.Event {
	var e rawInitEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode init event: %v", err),
			Code:    "decode_init",
			Raw:     json.RawMessage(line),
		}}
	}
	return []agent.Event{agent.InitEvent{
		SessionID: e.SessionID,
		Raw:       json.RawMessage(line),
	}}
}

func mapMessage(line []byte) []agent.Event {
	var e rawMessageEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode message event: %v", err),
			Code:    "decode_message",
			Raw:     json.RawMessage(line),
		}}
	}

	switch e.Role {
	case "assistant":
		if e.Content == "" {
			// Empty assistant chunk; skip rather than emit a blank text event.
			return nil
		}
		return []agent.Event{agent.AssistantTextEvent{
			Text: e.Content,
			Raw:  json.RawMessage(line),
		}}
	case "user":
		// User message echoed by the CLI; surface as a system event so the
		// runner sees it (mirrors the claude provider's user_message handling).
		return []agent.Event{agent.SystemEvent{
			Subtype: "user_message",
			Raw:     json.RawMessage(line),
		}}
	default:
		// Unknown role; surface as system event.
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: fmt.Sprintf("Unhandled message role: %s", e.Role),
			Raw:     json.RawMessage(line),
		}}
	}
}

func mapToolUse(line []byte) []agent.Event {
	var e rawToolUseEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode tool_use event: %v", err),
			Code:    "decode_tool_use",
			Raw:     json.RawMessage(line),
		}}
	}
	input := decodeParameters(e.Parameters)
	return []agent.Event{agent.ToolUseEvent{
		ToolName:  e.ToolName,
		ToolUseID: e.ToolID,
		Input:     input,
		Raw:       json.RawMessage(line),
	}}
}

func mapToolResult(line []byte) []agent.Event {
	var e rawToolResultEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode tool_result event: %v", err),
			Code:    "decode_tool_result",
			Raw:     json.RawMessage(line),
		}}
	}
	isError := e.Status == "error"
	content := e.Output
	if isError && e.Error != nil && e.Error.Message != "" {
		content = e.Error.Message
	}
	return []agent.Event{agent.ToolResultEvent{
		ToolUseID: e.ToolID,
		Content:   content,
		IsError:   isError,
		Raw:       json.RawMessage(line),
	}}
}

func mapError(line []byte) []agent.Event {
	var e rawErrorEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode error event: %v", err),
			Code:    "decode_error",
			Raw:     json.RawMessage(line),
		}}
	}
	// severity="error" → hard ErrorEvent that may terminate the stream.
	// severity="warning" → soft SystemEvent so the runner records it
	//   without treating it as a fatal failure.
	if e.Severity == "error" {
		return []agent.Event{agent.ErrorEvent{
			Message: e.Message,
			Code:    "gemini_cli_error",
			Raw:     json.RawMessage(line),
		}}
	}
	// warning (or unknown severity) → SystemEvent.
	return []agent.Event{agent.SystemEvent{
		Subtype: "warning",
		Message: e.Message,
		Raw:     json.RawMessage(line),
	}}
}

func mapResult(line []byte) []agent.Event {
	var e rawResultEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/geminicli: decode result event: %v", err),
			Code:    "decode_result",
			Raw:     json.RawMessage(line),
		}}
	}

	cost := &agent.CostData{
		InputTokens:       e.Stats.InputTokens,
		OutputTokens:      e.Stats.OutputTokens,
		CachedInputTokens: e.Stats.Cached,
		// The CLI does not expose per-model cost-usd; TotalCostUsd left 0.
	}

	if e.Status == "success" {
		return []agent.Event{agent.ResultEvent{
			Success: true,
			Cost:    cost,
			Raw:     json.RawMessage(line),
		}}
	}

	// Non-success result (e.g. partial status in future CLI versions).
	return []agent.Event{agent.ResultEvent{
		Success:      false,
		ErrorSubtype: e.Status,
		Errors:       []string{fmt.Sprintf("gemini CLI session ended with status: %s", e.Status)},
		Cost:         cost,
		Raw:          json.RawMessage(line),
	}}
}

// decodeParameters unmarshals a tool_use parameters JSON object as
// map[string]any. Any decode failure returns nil so the runner still
// sees the tool call.
func decodeParameters(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}
