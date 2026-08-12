package pi

import (
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// mapperState carries cross-event state for the live mapping (design §4):
//   - the buffered assistant text, flushed as one AssistantTextEvent on
//     message_end (anti-spam);
//   - the session id, resolved from the get_state response (agent_start carries
//     no id in the real protocol);
//   - initEmitted, so exactly one InitEvent is emitted whichever of
//     {get_state response, agent_start} arrives first;
//   - accumulated usage across turn_end events, folded into the terminal
//     ResultEvent's CostData (agent_settled carries no usage inline);
//   - sawAgentEnd/endSuccess, so a clean EOF after a non-retrying agent_end
//     that was somehow not followed by agent_settled still terminates cleanly
//     rather than as a crash;
//   - settled, so a stream close that arrives AFTER agent_settled (the
//     child process exiting once idle, or Stop's teardown) is recognized as
//     carrying no new terminal information — the ResultEvent was already
//     emitted when agent_settled was processed, and the sawAgentEnd/
//     endSuccess EOF-fallback below must not emit a second one.
type mapperState struct {
	sessionID   string
	textBuf     strings.Builder
	debug       bool // when true, thinking deltas are surfaced; default drops them
	initEmitted bool

	accInputTokens  int64
	accOutputTokens int64
	accCostUSD      float64
	accTurns        int

	sawAgentEnd bool
	endSuccess  bool
	settled     bool
}

// mapEvent translates one pi event (or command response) into zero or more
// agent.Events (design §4, verified against @earendil-works/pi-coding-agent@
// 0.80.10 docs/rpc.md). extension_ui_request events are NOT mapped here — the
// handle intercepts them for handshake/adjudication before the mapper ever
// sees them.
//
// The returned bool `terminal` is true when the event is the session terminal
// (agent_settled → ResultEvent, or a fatal extension_error → ErrorEvent), so
// the pump knows to stop after emitting.
func mapEvent(ev rawEvent, st *mapperState) (out []agent.Event, terminal bool) {
	f := ev.Fields
	switch ev.Type {
	case "response":
		// Command responses carry the session id (get_state) and otherwise are
		// acks the mapper does not surface. get_state is the only id source in
		// the real protocol (agent_start has none).
		if stringField(f, "command") == "get_state" {
			data := mapField(f, "data")
			if id := stringField(data, "sessionId", "session_id"); id != "" {
				st.sessionID = id
			}
			if !st.initEmitted {
				st.initEmitted = true
				return []agent.Event{agent.InitEvent{SessionID: st.sessionID, Raw: raw(ev)}}, false
			}
		}
		return nil, false

	case "agent_start":
		// The real agent_start carries no session id; if get_state already
		// resolved one it rides here, else it is empty and SessionID() fills in
		// when the get_state response arrives.
		if !st.initEmitted {
			st.initEmitted = true
			return []agent.Event{agent.InitEvent{SessionID: st.sessionID, Raw: raw(ev)}}, false
		}
		return nil, false

	case "message_update":
		// The streaming delta lives under assistantMessageEvent. Buffer
		// text_delta parts; drop thinking_delta unless debug. Emit nothing until
		// message_end (anti-spam).
		ame := mapField(f, "assistantMessageEvent")
		switch stringField(ame, "type") {
		case "text_delta":
			st.textBuf.WriteString(stringField(ame, "delta"))
		case "thinking_delta":
			if st.debug {
				st.textBuf.WriteString(stringField(ame, "delta"))
			}
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
			ToolName:  stringField(f, "toolName", "tool", "name"),
			ToolUseID: stringField(f, "toolCallId", "callId", "call_id", "id"),
			Input:     mapField(f, "args", "input"),
			Raw:       raw(ev),
		}}, false

	case "tool_execution_update":
		return []agent.Event{agent.ToolProgressEvent{
			ToolName:       stringField(f, "toolName", "tool", "name"),
			ElapsedSeconds: floatField(f, "elapsedSeconds", "elapsed"),
			Raw:            raw(ev),
		}}, false

	case "tool_execution_end":
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  stringField(f, "toolName", "tool", "name"),
			ToolUseID: stringField(f, "toolCallId", "callId", "call_id", "id"),
			Content:   toolResultContent(f),
			IsError:   boolField(f, "isError", "error"),
			Raw:       raw(ev),
		}}, false

	case "turn_end":
		// Per-turn usage rides message.usage (AssistantMessage). Accumulate it
		// so the terminal ResultEvent carries whole-session cost.
		msg := mapField(f, "message")
		usage := mapField(msg, "usage")
		in := intField(usage, "input", "inputTokens")
		outTok := intField(usage, "output", "outputTokens")
		st.accInputTokens += in
		st.accOutputTokens += outTok
		st.accCostUSD += floatField(mapField(usage, "cost"), "total")
		st.accTurns++
		return []agent.Event{agent.LlmCallEvent{
			System:       stringField(msg, "provider", "system"),
			Model:        stringField(msg, "model"),
			InputTokens:  in,
			OutputTokens: outTok,
			UsageSource:  agent.LlmUsageProvider,
		}}, false

	case "agent_end":
		// NOT terminal in the real protocol: a low-level run completed but retry,
		// compaction, or a queued continuation may still follow (agent_settled is
		// the true terminal). Record a clean-completion hint for the EOF fallback.
		st.sawAgentEnd = true
		st.endSuccess = !boolField(f, "willRetry")
		return nil, false

	case "agent_settled":
		// The true session terminal: no automatic retry, compaction retry, or
		// queued continuation remains. This ends the CURRENT turn, not
		// necessarily the RPC session — pi accepts a follow_up/steer command
		// after a completed turn and will drive another one, so the caller
		// (handle.go's dispatch/run) treats this as a non-fatal terminal:
		// the pump keeps consuming events so a later Handle.Inject has
		// somewhere to land.
		st.settled = true
		res := agent.ResultEvent{Success: true, Raw: raw(ev)}
		if cost := st.accumulatedCost(); cost != nil {
			res.Cost = cost
		}
		return []agent.Event{res}, true

	case "extension_error":
		// An extension_error referencing the donmai policy extension is a
		// policy-integrity failure: abort rather than continue unguarded
		// (design §5.3). The handle decides whether it references our extension;
		// here we surface it as a fatal ErrorEvent.
		return []agent.Event{agent.ErrorEvent{
			Message: stringField(f, "error", "message"),
			Code:    "policy_extension_failed",
			Raw:     raw(ev),
		}}, true

	case "auto_retry_start", "auto_retry_end",
		"compaction_start", "compaction_end",
		"queue_update", "turn_start", "message_start":
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

// accumulatedCost returns the session's accumulated CostData, or nil if nothing
// was observed.
func (st *mapperState) accumulatedCost() *agent.CostData {
	if st.accInputTokens == 0 && st.accOutputTokens == 0 && st.accCostUSD == 0 && st.accTurns == 0 {
		return nil
	}
	return &agent.CostData{
		InputTokens:  st.accInputTokens,
		OutputTokens: st.accOutputTokens,
		TotalCostUsd: st.accCostUSD,
		NumTurns:     st.accTurns,
	}
}

// toolResultContent flattens tool_execution_end's result.content[] text parts
// into a single string (real shape: {result:{content:[{type:"text",text:...}]}}).
func toolResultContent(f map[string]any) string {
	result := mapField(f, "result")
	parts, ok := result["content"].([]any)
	if !ok {
		// Some shapes may inline plain text.
		return stringField(f, "content", "output")
	}
	var b strings.Builder
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, _ := m["text"].(string); t != "" {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// raw returns the raw event bytes as a string for the agent.Event Raw field.
func raw(ev rawEvent) any { return string(ev.Line) }

// --- field accessors (tolerant of pi's key naming) ---

func stringField(f map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := f[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
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
