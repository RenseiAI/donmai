package claude

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// rawJSONLEnvelope is the discriminator-only decode used to dispatch
// to a typed mapper. The Claude CLI stream-json events all carry a
// top-level `type` field; specific subtypes refine the variant
// (system.subtype = "init" → InitEvent, system.subtype = "*" →
// SystemEvent, etc.).
type rawJSONLEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

// rawAssistantEnvelope captures the fields needed to translate an
// `assistant` stream-json line into one or more agent.Event values.
//
// The CLI shape (verified empirically against claude 2.1.126):
//
//	{
//	  "type": "assistant",
//	  "session_id": "...",
//	  "message": {
//	    "content": [
//	       { "type": "text", "text": "..." },
//	       { "type": "tool_use", "id": "tu-1", "name": "Bash", "input": {...} }
//	    ],
//	    ...
//	  }
//	}
type rawAssistantEnvelope struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Message   struct {
		Content []rawContentBlock `json:"content"`
	} `json:"message"`
}

// rawContentBlock is one block inside an assistant or user message.
// We only care about text + tool_use for assistant messages and
// tool_result for user messages.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// rawUserEnvelope captures user-message lines, which the CLI emits to
// echo back tool results so JSONL consumers can pair tool_use with
// tool_result. Mirrors the legacy TS mapUserMessage handling.
type rawUserEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Content []rawContentBlock `json:"content"`
	} `json:"message"`
}

// rawSystemEnvelope decodes system messages, including the init
// subtype that carries the session id.
type rawSystemEnvelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message,omitempty"`
	Status    string `json:"status,omitempty"`
}

// rawToolProgressEnvelope is the optional progress tick the CLI emits
// for long-running tools. Mirrors the legacy SDK's tool_progress
// shape — port verbatim per F.1.1 §3.1.
type rawToolProgressEnvelope struct {
	Type               string  `json:"type"`
	ToolName           string  `json:"tool_name"`
	ElapsedTimeSeconds float64 `json:"elapsed_time_seconds"`
}

// rawAuthStatusEnvelope mirrors the legacy SDK's auth_status events
// (claude-provider.ts mapSDKMessage auth_status branch). The CLI may
// emit equivalents during plugin-sync / interactive auth flows.
type rawAuthStatusEnvelope struct {
	Type             string `json:"type"`
	IsAuthenticating bool   `json:"isAuthenticating,omitempty"`
	Error            string `json:"error,omitempty"`
}

// rawResultEnvelope captures the terminal result line. The CLI's
// `result` shape is rich; we map the fields we care about into
// agent.ResultEvent:
//
//	{
//	  "type": "result",
//	  "subtype": "success" | "error_during_execution" | "error_max_turns" | ...,
//	  "is_error": false,
//	  "result": "Hi!",
//	  "session_id": "...",
//	  "total_cost_usd": 0.117,
//	  "num_turns": 1,
//	  "usage": {
//	    "input_tokens": 5,
//	    "output_tokens": 8,
//	    "cache_read_input_tokens": 16526,
//	    ...
//	  }
//	}
type rawResultEnvelope struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	Errors       []struct {
		Message string `json:"message,omitempty"`
	} `json:"errors,omitempty"`
	Usage struct {
		InputTokens          int64 `json:"input_tokens"`
		OutputTokens         int64 `json:"output_tokens"`
		CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// mapLine decodes one JSONL line and returns the resulting
// agent.Event values. The Claude CLI may produce 0, 1, or N events
// per line:
//
//   - 0 events: stream_event partial-message lines, hook_started /
//     hook_response housekeeping that the runner never consumes,
//     unknown / unhandled types.
//   - 1 event: most types (system.init, system.*, result, error).
//   - N events: assistant / user lines whose `message.content` array
//     fans out into multiple text / tool_use / tool_result events.
//
// Any decode failure becomes a single ErrorEvent so the runner
// observes the parse failure rather than silently dropping it. The
// caller's loop is responsible for whether a parse error is fatal.
//
// This is the verbatim Go port of the legacy TS mapSDKMessage from
// ../agentfactory/packages/core/src/providers/claude-provider.ts.
//
// Design note: each Event variant carries the original raw line in
// its `Raw` field as json.RawMessage so the runner can persist the
// full provider-native event to <worktree>/.agent/events.jsonl per
// F.1.1 §4 step 9.
func mapLine(line []byte) []agent.Event {
	var head rawJSONLEnvelope
	if err := json.Unmarshal(line, &head); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode JSONL envelope: %v", err),
			Code:    "decode_envelope",
			Raw:     json.RawMessage(line),
		}}
	}

	switch head.Type {
	case "system":
		return mapSystem(line)
	case "assistant":
		return mapAssistant(line)
	case "user":
		return mapUser(line)
	case "result":
		return mapResult(line)
	case "tool_progress":
		return mapToolProgress(line)
	case "auth_status":
		return mapAuthStatus(line)
	case "stream_event":
		// Partial-message frames: high-frequency, low value to the
		// orchestrator. Drop per the legacy port.
		return nil
	case "rate_limit_event":
		// Surface as a system event so the runner can record it.
		return []agent.Event{agent.SystemEvent{
			Subtype: "rate_limit",
			Raw:     json.RawMessage(line),
		}}
	case "":
		return []agent.Event{agent.ErrorEvent{
			Message: "provider/claude: JSONL line missing top-level type",
			Code:    "missing_type",
			Raw:     json.RawMessage(line),
		}}
	default:
		// Fall-through: surface as a system event with subtype "unknown".
		// Mirrors the legacy mapSDKMessage default branch.
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: fmt.Sprintf("Unhandled message type: %s", head.Type),
			Raw:     json.RawMessage(line),
		}}
	}
}

func mapSystem(line []byte) []agent.Event {
	var s rawSystemEnvelope
	if err := json.Unmarshal(line, &s); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode system event: %v", err),
			Code:    "decode_system",
			Raw:     json.RawMessage(line),
		}}
	}
	if s.Subtype == "init" {
		return []agent.Event{agent.InitEvent{
			SessionID: s.SessionID,
			Raw:       json.RawMessage(line),
		}}
	}
	subtype := s.Subtype
	if subtype == "" {
		subtype = "unknown"
	}
	msg := s.Status
	if msg == "" {
		msg = s.Message
	}
	return []agent.Event{agent.SystemEvent{
		Subtype: subtype,
		Message: msg,
		Raw:     json.RawMessage(line),
	}}
}

func mapAssistant(line []byte) []agent.Event {
	var a rawAssistantEnvelope
	if err := json.Unmarshal(line, &a); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode assistant event: %v", err),
			Code:    "decode_assistant",
			Raw:     json.RawMessage(line),
		}}
	}
	out := make([]agent.Event, 0, len(a.Message.Content))
	for _, block := range a.Message.Content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			out = append(out, agent.AssistantTextEvent{
				Text: block.Text,
				Raw:  json.RawMessage(line),
			})
		case "tool_use":
			input := decodeInput(block.Input)
			out = append(out, agent.ToolUseEvent{
				ToolName:  block.Name,
				ToolUseID: block.ID,
				Input:     input,
				Raw:       json.RawMessage(line),
			})
		}
	}
	return out
}

func mapUser(line []byte) []agent.Event {
	var u rawUserEnvelope
	if err := json.Unmarshal(line, &u); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode user event: %v", err),
			Code:    "decode_user",
			Raw:     json.RawMessage(line),
		}}
	}
	out := make([]agent.Event, 0, len(u.Message.Content))
	for _, block := range u.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		content := decodeToolResultContent(block.Content)
		out = append(out, agent.ToolResultEvent{
			ToolUseID: block.ToolUseID,
			Content:   content,
			IsError:   block.IsError,
			Raw:       json.RawMessage(line),
		})
	}
	if len(out) == 0 {
		// Legacy-port behavior: emit a generic system event so the
		// runner sees the user message rather than silently dropping.
		out = append(out, agent.SystemEvent{
			Subtype: "user_message",
			Raw:     json.RawMessage(line),
		})
	}
	return out
}

func mapResult(line []byte) []agent.Event {
	var r rawResultEnvelope
	if err := json.Unmarshal(line, &r); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode result event: %v", err),
			Code:    "decode_result",
			Raw:     json.RawMessage(line),
		}}
	}

	cost := &agent.CostData{
		InputTokens:       r.Usage.InputTokens,
		OutputTokens:      r.Usage.OutputTokens,
		CachedInputTokens: r.Usage.CacheReadInputTokens,
		TotalCostUsd:      r.TotalCostUSD,
		NumTurns:          r.NumTurns,
	}

	if r.Subtype == "success" && !r.IsError {
		return []agent.Event{agent.ResultEvent{
			Success: true,
			Message: r.Result,
			Cost:    cost,
			Raw:     json.RawMessage(line),
		}}
	}

	errMsgs := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		if e.Message != "" {
			errMsgs = append(errMsgs, e.Message)
		}
	}
	if len(errMsgs) == 0 && r.Result != "" {
		errMsgs = append(errMsgs, r.Result)
	}
	subtype := r.Subtype
	if subtype == "" {
		subtype = "error"
	}
	return []agent.Event{agent.ResultEvent{
		Success:      false,
		Errors:       errMsgs,
		ErrorSubtype: subtype,
		Cost:         cost,
		Raw:          json.RawMessage(line),
	}}
}

func mapToolProgress(line []byte) []agent.Event {
	var p rawToolProgressEnvelope
	if err := json.Unmarshal(line, &p); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode tool_progress event: %v", err),
			Code:    "decode_tool_progress",
			Raw:     json.RawMessage(line),
		}}
	}
	return []agent.Event{agent.ToolProgressEvent{
		ToolName:       p.ToolName,
		ElapsedSeconds: p.ElapsedTimeSeconds,
		Raw:            json.RawMessage(line),
	}}
}

func mapAuthStatus(line []byte) []agent.Event {
	var a rawAuthStatusEnvelope
	if err := json.Unmarshal(line, &a); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/claude: decode auth_status event: %v", err),
			Code:    "decode_auth_status",
			Raw:     json.RawMessage(line),
		}}
	}
	if a.Error != "" {
		return []agent.Event{agent.ErrorEvent{
			Message: a.Error,
			Code:    "auth_status",
			Raw:     json.RawMessage(line),
		}}
	}
	msg := "Authenticated"
	if a.IsAuthenticating {
		msg = "Authenticating..."
	}
	return []agent.Event{agent.SystemEvent{
		Subtype: "auth_status",
		Message: msg,
		Raw:     json.RawMessage(line),
	}}
}

// decodeInput unmarshals a tool_use input as map[string]any. Any
// decode failure returns nil so the runner still sees the tool call.
func decodeInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// decodeToolResultContent normalizes the legacy SDK's flexible
// content shape (string | object) to a string. Mirrors the
// `typeof block.content === 'string' ? block.content : JSON.stringify`
// branch of the legacy mapUserMessage.
func decodeToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Otherwise stringify the raw JSON body.
	return string(raw)
}
