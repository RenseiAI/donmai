package pi

import (
	"math"
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

	accInputTokens       int64
	accOutputTokens      int64
	accCostUSD           float64
	accTurns             int
	allTurnCostsObserved bool

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
		// Command responses carry the session id (get_state) or the resume
		// cursor-replay payload (get_entries); every other command response
		// is an ack the mapper does not surface. get_state is the only id
		// source in the real protocol (agent_start has none).
		switch stringField(f, "command") {
		case "get_state":
			data := mapField(f, "data")
			if id := stringField(data, "sessionId", "session_id"); id != "" {
				st.sessionID = id
			}
			if !st.initEmitted {
				st.initEmitted = true
				return []agent.Event{agent.InitEvent{SessionID: st.sessionID, Raw: raw(ev)}}, false
			}
			return nil, false

		case "get_entries":
			// Resume's cursor replay (pi.go Resume → get_entries since=<id>)
			// lands here. Routed explicitly as a SystemEvent rather than
			// silently dropped like a plain ack: a bare Resume with no
			// follow-up prompt/steer previously produced only an InitEvent
			// and then went quiet forever, so the caller had no way to tell
			// the replay reply had even arrived. This does NOT decode the
			// entries into replayed Assistant/Tool/etc. events — that is a
			// separate, larger feature (see the Resume doc comment in
			// pi.go); it only stops the reply from vanishing unobserved.
			return []agent.Event{agent.SystemEvent{Subtype: "get_entries", Raw: raw(ev)}}, false

		default:
			return nil, false
		}

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
		if text != "" {
			out = append(out, agent.AssistantTextEvent{Text: text, Raw: raw(ev)})
		}
		// An assistant message that ended on a provider error (stopReason
		// "error": a gateway 503/504, an overloaded model) is surfaced as the
		// provider-error observation, so the runner can tell a turn that
		// ended on that error from one where the agent chose to stop. pi's
		// own auto-retry may still recover; a later message or tool call
		// then supersedes it.
		if msg := mapField(f, "message"); stringField(msg, "role") == "assistant" && stringField(msg, "stopReason") == "error" {
			detail := stringField(msg, "errorMessage")
			if detail == "" {
				detail = "model provider error"
			}
			out = append(out, agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: providerErrorDetail(msg, detail), Raw: raw(ev)})
		}
		return out, false

	case "tool_execution_start":
		return []agent.Event{agent.ToolUseEvent{
			ToolName:  stringField(f, "toolName", "tool", "name"),
			ToolUseID: toolCallID(f),
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
			ToolUseID: toolCallID(f),
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
		cost := observedPiCost(mapField(usage, "cost"))
		if st.accTurns == 0 {
			st.allTurnCostsObserved = true
		}
		if cost == nil {
			st.allTurnCostsObserved = false
		} else {
			next := st.accCostUSD + *cost
			if math.IsInf(next, 0) {
				st.allTurnCostsObserved = false
			} else {
				st.accCostUSD = next
			}
		}
		st.accTurns++
		return []agent.Event{agent.LlmCallEvent{
			System: stringField(msg, "provider", "system"),
			Model:  stringField(msg, "model"),
			// Pi pre-fills provider/model from its selected catalog entry.
			// responseModel alone is native response evidence when supplied.
			ResponseModel:   stringField(msg, "responseModel"),
			InputTokens:     in,
			OutputTokens:    outTok,
			UsageSource:     agent.LlmUsageProvider,
			ObservedCostUsd: cost,
			TurnCompleted:   true,
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
		if st.accTurns > 0 {
			turns := st.accTurns
			res.ObservedTurns = &turns
			if st.allTurnCostsObserved {
				cost := st.accCostUSD
				res.ObservedCostUsd = &cost
			}
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

func observedPiCost(cost map[string]any) *float64 {
	value, ok := cost["total"].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	return &value
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

// providerErrorDetail folds the harness-reported retryability into the
// provider-error observation the runner classifies. A gateway marks a
// deterministic failure (for example a 400 for invalid parameters) with
// isRetryable false; without it the text alone cannot tell a failure that
// cannot succeed on retry from a transient one. The marker rides as a
// "; isRetryable=false" suffix so older readers still see the provider's
// own text first, and the runner strips it before recording the receipt.
func providerErrorDetail(msg map[string]any, detail string) string {
	if retryable := providerErrorRetryable(msg); retryable == nil || *retryable {
		return detail
	}
	return detail + "; isRetryable=false"
}

// providerErrorRetryable reports the harness's own retryability verdict for
// a failed assistant message: an explicit isRetryable flag on the message
// or its diagnostics, else the HTTP status when the text carries one (408,
// 409, 429 and 5xx retry; any other 4xx does not). It returns nil when the
// message carries no verdict either way, so the runner keeps its current
// bounded retries. Unknown shapes stay retryable: only a positive signal
// that the request cannot succeed stops the retry.
func providerErrorRetryable(msg map[string]any) *bool {
	if v, ok := providerErrorFlag(msg); ok {
		return &v
	}
	for _, d := range providerErrorDiagnostics(msg) {
		if v, ok := providerErrorFlag(d); ok {
			return &v
		}
		if status, ok := providerErrorStatus(d); ok {
			retryable := providerErrorStatusRetryable(status)
			return &retryable
		}
	}
	if status, ok := providerErrorStatus(msg); ok {
		retryable := providerErrorStatusRetryable(status)
		return &retryable
	}
	return nil
}

// providerErrorFlag reads an explicit isRetryable verdict. The gateway
// nests it under error ({"error":{"isRetryable":false}}); accept a
// top-level flag too so a flatter shape is not missed.
func providerErrorFlag(m map[string]any) (bool, bool) {
	if v, ok := m["isRetryable"].(bool); ok {
		return v, true
	}
	if err, ok := m["error"].(map[string]any); ok {
		if v, ok := err["isRetryable"].(bool); ok {
			return v, true
		}
	}
	return false, false
}

// providerErrorDiagnostics returns the message's diagnostic entries: the
// pi-messages failure path attaches the gateway status there
// ({"details":{"status":400}}).
func providerErrorDiagnostics(msg map[string]any) []map[string]any {
	raw, ok := msg["diagnostics"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if d, ok := item.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// providerErrorStatus reads an HTTP status from a message or diagnostic
// entry: details.status first (the diagnostic shape), then status and
// statusCode spellings.
func providerErrorStatus(m map[string]any) (int, bool) {
	if details, ok := m["details"].(map[string]any); ok {
		if status, ok := providerErrorNumber(details, "status", "statusCode"); ok {
			return status, true
		}
	}
	return providerErrorNumber(m, "status", "statusCode")
}

func providerErrorNumber(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case int64:
			return int(v), true
		}
	}
	return 0, false
}

// providerErrorStatusRetryable mirrors the provider SDK retry policy: 408,
// 409, 429 and 5xx (plus unknown/no status, handled by the caller) retry;
// any other 4xx is deterministic and does not.
func providerErrorStatusRetryable(status int) bool {
	if status == 408 || status == 409 || status == 429 {
		return true
	}
	if status >= 500 && status <= 599 {
		return true
	}
	if status >= 400 && status <= 499 {
		return false
	}
	return true
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
