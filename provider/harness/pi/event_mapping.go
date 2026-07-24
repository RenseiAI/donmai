package pi

import (
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// mapperState carries cross-event state for the live mapping: the buffered
// assistant text (flushed as one AssistantTextEvent on message_end — the
// anti-spam rule, design §4) and the session id once agent_start resolves it.
type mapperState struct {
	sessionID string
	textBuf   strings.Builder
	debug     bool // when true, thinking deltas are surfaced; default drops them
}

// mapEvent translates one pi event into zero or more agent.Events (design §4).
// extension_ui_request events are NOT mapped here — the handle intercepts them
// for handshake/adjudication before the mapper ever sees them.
//
// The returned bool `terminal` is true when the event is the session terminal
// (agent_end → ResultEvent, or a fatal extension_error → ErrorEvent), so the
// pump knows to stop after emitting.
func mapEvent(ev rawEvent, st *mapperState) (out []agent.Event, terminal bool) {
	f := ev.Fields
	switch ev.Type {
	case "agent_start":
		if id := stringField(f, "sessionId", "session_id", "id"); id != "" {
			st.sessionID = id
		}
		return []agent.Event{agent.InitEvent{SessionID: st.sessionID, Raw: raw(ev)}}, false

	case "message_update":
		// Buffer text deltas; drop thinking parts unless debug. Emit nothing
		// until message_end (anti-spam).
		if part := stringField(f, "text", "delta", "content"); part != "" {
			if isThinking(f) && !st.debug {
				return nil, false
			}
			st.textBuf.WriteString(part)
		}
		return nil, false

	case "message_end":
		text := st.textBuf.String()
		st.textBuf.Reset()
		if text == "" {
			return nil, false
		}
		return []agent.Event{agent.AssistantTextEvent{Text: text, Raw: raw(ev)}}, false

	case "tool_execution_start":
		return []agent.Event{agent.ToolUseEvent{
			ToolName:  stringField(f, "tool", "name", "toolName"),
			ToolUseID: stringField(f, "callId", "call_id", "id"),
			Input:     mapField(f, "args", "input"),
			Raw:       raw(ev),
		}}, false

	case "tool_execution_update":
		return []agent.Event{agent.ToolProgressEvent{
			ToolName:       stringField(f, "tool", "name", "toolName"),
			ElapsedSeconds: floatField(f, "elapsedSeconds", "elapsed"),
			Raw:            raw(ev),
		}}, false

	case "tool_execution_end":
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  stringField(f, "tool", "name", "toolName"),
			ToolUseID: stringField(f, "callId", "call_id", "id"),
			Content:   stringField(f, "content", "output", "result"),
			IsError:   boolField(f, "isError", "error"),
			Raw:       raw(ev),
		}}, false

	case "turn_end", "get_session_stats":
		return []agent.Event{agent.LlmCallEvent{
			System:       stringField(f, "provider", "system"),
			Model:        stringField(f, "model"),
			InputTokens:  intField(f, "inputTokens", "input_tokens"),
			OutputTokens: intField(f, "outputTokens", "output_tokens"),
			UsageSource:  agent.LlmUsageProvider,
		}}, false

	case "agent_end":
		success := boolFieldDefault(f, true, "success", "ok")
		res := agent.ResultEvent{Success: success, Raw: raw(ev)}
		if cost := costFrom(f); cost != nil {
			res.Cost = cost
		}
		if msg := stringField(f, "message"); msg != "" {
			res.Message = msg
		}
		if !success {
			res.Errors = stringsField(f, "errors")
			res.ErrorSubtype = stringField(f, "errorSubtype", "error_subtype")
		}
		return []agent.Event{res}, true

	case "extension_error":
		// An extension_error referencing the donmai extension is a
		// policy-integrity failure: abort rather than continue unguarded
		// (design §5.3). The handle decides whether it references our
		// extension; here we surface it as a fatal ErrorEvent.
		return []agent.Event{agent.ErrorEvent{
			Message: stringField(f, "message", "error"),
			Code:    "policy_extension_failed",
			Raw:     raw(ev),
		}}, true

	case "auto_retry", "auto_retry_start", "auto_retry_end",
		"compaction", "compaction_start", "compaction_end",
		"queue_update":
		return []agent.Event{agent.SystemEvent{
			Subtype: ev.Type,
			Message: stringField(f, "message"),
			Raw:     raw(ev),
		}}, false

	default:
		// Unknown/typeless events become observability SystemEvents; never
		// fatal (a malformed line must not tear the session down).
		if ev.Type == "" {
			return nil, false
		}
		return []agent.Event{agent.SystemEvent{Subtype: ev.Type, Raw: raw(ev)}}, false
	}
}

// raw returns the raw event bytes as a string for the agent.Event Raw field.
func raw(ev rawEvent) any { return string(ev.Line) }

func isThinking(f map[string]any) bool {
	if p, ok := f["part"].(string); ok {
		return p == "thinking" || p == "reasoning"
	}
	if b, ok := f["thinking"].(bool); ok {
		return b
	}
	return false
}

func costFrom(f map[string]any) *agent.CostData {
	c := &agent.CostData{
		InputTokens:  intField(f, "inputTokens", "input_tokens"),
		OutputTokens: intField(f, "outputTokens", "output_tokens"),
		TotalCostUsd: floatField(f, "costUsd", "totalCostUsd", "cost"),
		NumTurns:     int(intField(f, "numTurns", "turns")),
	}
	if c.InputTokens == 0 && c.OutputTokens == 0 && c.TotalCostUsd == 0 && c.NumTurns == 0 {
		return nil
	}
	return c
}

// --- field accessors (tolerant of pi's unverified key naming) ---

func stringField(f map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := f[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func stringsField(f map[string]any, key string) []string {
	arr, ok := f[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func mapField(f map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := f[k].(map[string]any); ok {
			return v
		}
	}
	return map[string]any{}
}

func boolField(f map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := f[k].(bool); ok {
			return v
		}
	}
	return false
}

func boolFieldDefault(f map[string]any, def bool, keys ...string) bool {
	for _, k := range keys {
		if v, ok := f[k].(bool); ok {
			return v
		}
	}
	return def
}

func intField(f map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := f[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
	}
	return 0
}

func floatField(f map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := f[k].(float64); ok {
			return v
		}
	}
	return 0
}
