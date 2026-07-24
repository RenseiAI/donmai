package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ─── SSE /api/event → agent.Event mapping (07 §4.2) ──────────────────────────
//
// The v2 event feed (verified live against opencode 1.17.18's OpenAPI) is a
// global SSE stream of {id, type, properties} frames. The mapper filters to a
// single session, synthesizes exactly one InitEvent, buffers text so only
// COMPLETE assistant texts surface (anti-spam rule, ADR-2026-06-06 D6 — never
// per-token deltas), and folds tool/step/error frames onto the same
// agent.Event vocabulary Lane A already uses. It emits exactly one terminal
// event (ResultEvent / ErrorEvent) then nothing further, satisfying the shared
// conformance contract.
//
// DRIFT NOTE (code wins over design §4.2): 07 §4.2 named older 1.x event
// shapes ("message part text deltas", "tool part state transitions"). The
// pinned binary emits the v2 `session.next.*` vocabulary instead:
//
//	session.created                 → InitEvent (once)
//	session.next.text.ended         → AssistantTextEvent (complete text)
//	session.next.text.delta         → buffered/ignored (anti-spam)
//	session.next.tool.called        → ToolUseEvent
//	session.next.tool.success       → ToolResultEvent (ok)
//	session.next.tool.failed        → ToolResultEvent (error)
//	session.next.step.ended         → LlmCallEvent (+ terminal ResultEvent when finished=stop)
//	session.next.step.failed        → terminal ResultEvent{Success:false}
//	session.error                   → terminal ResultEvent{Success:false}

// v2 event type discriminators.
const (
	evSessionCreated = "session.created"
	evTextEnded      = "session.next.text.ended"
	evToolCalled     = "session.next.tool.called"
	evToolSuccess    = "session.next.tool.success"
	evToolFailed     = "session.next.tool.failed"
	evStepEnded      = "session.next.step.ended"
	evStepFailed     = "session.next.step.failed"
	evSessionError   = "session.error"
)

// sseMapper holds the per-session mapping state: which session to filter to,
// whether the synthetic InitEvent has fired, and a dedup set over SSE frame
// ids (a replay after an SSE drop re-delivers frames — dedup keeps the
// terminal-contract "exactly one" invariant intact).
type sseMapper struct {
	sessionID string
	initSent  bool
	terminal  bool
	seen      map[string]bool
}

func newSSEMapper(sessionID string) *sseMapper {
	return &sseMapper{sessionID: sessionID, seen: make(map[string]bool)}
}

// property decode targets (only the fields the mapping reads).
type propSessionID struct {
	SessionID string `json:"sessionID"`
}

type propText struct {
	SessionID string `json:"sessionID"`
	Text      string `json:"text"`
}

type propToolCalled struct {
	SessionID string         `json:"sessionID"`
	CallID    string         `json:"callID"`
	Tool      string         `json:"tool"`
	Input     map[string]any `json:"input"`
}

type propToolSuccess struct {
	SessionID string `json:"sessionID"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	Content   string `json:"content"`
}

type propToolFailed struct {
	SessionID string `json:"sessionID"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	Error     string `json:"error"`
}

type propStepEnded struct {
	SessionID string          `json:"sessionID"`
	Finish    json.RawMessage `json:"finish"`
	Cost      float64         `json:"cost"`
	Tokens    *tokens         `json:"tokens"`
}

type propError struct {
	SessionID string          `json:"sessionID"`
	Error     json.RawMessage `json:"error"`
}

// eventSessionID pulls the sessionID out of any frame's properties for the
// filter check.
func eventSessionID(ev serverEvent) string {
	var p propSessionID
	_ = json.Unmarshal(ev.Properties, &p)
	if p.SessionID != "" {
		return p.SessionID
	}
	// session.created carries the id under info.id too; try that shape.
	var alt struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	_ = json.Unmarshal(ev.Properties, &alt)
	return alt.Info.ID
}

// Map translates one SSE frame into zero or more agent.Events, applying the
// session filter, init-once, dedup, and terminal-latch rules.
func (m *sseMapper) Map(ev serverEvent) []agent.Event {
	if m.terminal {
		return nil
	}
	// Session filter: only frames for our session (or global frames with no
	// session, which we ignore).
	if sid := eventSessionID(ev); sid != "" && m.sessionID != "" && sid != m.sessionID {
		return nil
	}
	// Dedup on frame id (replay tolerance).
	if ev.ID != "" {
		if m.seen[ev.ID] {
			return nil
		}
		m.seen[ev.ID] = true
	}

	var out []agent.Event
	// Synthesize InitEvent from the first in-session frame that carries our id.
	if !m.initSent {
		if sid := eventSessionID(ev); sid != "" && (m.sessionID == "" || sid == m.sessionID) {
			m.initSent = true
			out = append(out, agent.InitEvent{SessionID: sid, Raw: ev.Properties})
		}
	}

	switch ev.Type {
	case evSessionCreated:
		// InitEvent already handled above; nothing more.
		return out

	case evTextEnded:
		var p propText
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return append(out, decodeErr("text.ended", ev, err))
		}
		if p.Text == "" {
			return out
		}
		return append(out, agent.AssistantTextEvent{Text: p.Text, Raw: ev.Properties})

	case evToolCalled:
		var p propToolCalled
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return append(out, decodeErr("tool.called", ev, err))
		}
		return append(out, agent.ToolUseEvent{
			ToolName:  p.Tool,
			ToolUseID: p.CallID,
			Input:     p.Input,
			Raw:       ev.Properties,
		})

	case evToolSuccess:
		var p propToolSuccess
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return append(out, decodeErr("tool.success", ev, err))
		}
		return append(out, agent.ToolResultEvent{
			ToolName:  p.Tool,
			ToolUseID: p.CallID,
			Content:   p.Content,
			IsError:   false,
			Raw:       ev.Properties,
		})

	case evToolFailed:
		var p propToolFailed
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return append(out, decodeErr("tool.failed", ev, err))
		}
		return append(out, agent.ToolResultEvent{
			ToolName:  p.Tool,
			ToolUseID: p.CallID,
			Content:   p.Error,
			IsError:   true,
			Raw:       ev.Properties,
		})

	case evStepEnded:
		var p propStepEnded
		if err := json.Unmarshal(ev.Properties, &p); err != nil {
			return append(out, decodeErr("step.ended", ev, err))
		}
		reason := extractFinishReason(p.Finish)
		llm := agent.LlmCallEvent{
			System:       "opencode",
			FinishReason: reason,
			UsageSource:  agent.LlmUsageProvider,
		}
		if p.Tokens != nil {
			llm.InputTokens = p.Tokens.Input
			llm.OutputTokens = p.Tokens.Output
		}
		out = append(out, llm)
		if isStopReason(reason) {
			// Turn complete — the terminal ResultEvent for this Lane-B session
			// (mirrors Lane A step_finish reason=stop).
			m.terminal = true
			var cost *agent.CostData
			if p.Tokens != nil || p.Cost != 0 {
				cost = &agent.CostData{TotalCostUsd: p.Cost}
				if p.Tokens != nil {
					cost.InputTokens = p.Tokens.Input
					cost.OutputTokens = p.Tokens.Output
				}
			}
			out = append(out, agent.ResultEvent{Success: true, Cost: cost, Raw: ev.Properties})
		}
		return out

	case evStepFailed:
		var p propError
		_ = json.Unmarshal(ev.Properties, &p)
		m.terminal = true
		return append(out, agent.ResultEvent{
			Success:      false,
			Errors:       []string{errString(p.Error, "opencode step failed")},
			ErrorSubtype: "step_failed",
			Raw:          ev.Properties,
		})

	case evSessionError:
		var p propError
		_ = json.Unmarshal(ev.Properties, &p)
		m.terminal = true
		return append(out, agent.ResultEvent{
			Success:      false,
			Errors:       []string{errString(p.Error, "opencode session error")},
			ErrorSubtype: "session_error",
			Raw:          ev.Properties,
		})

	default:
		// Unmodeled in-session frame — drop (init may still have been emitted).
		return out
	}
}

// serverCrashed returns the terminal ErrorEvent the handle emits when the
// serve child dies mid-session (mirrors codex app_server_crashed).
func (m *sseMapper) serverCrashed(reason string) []agent.Event {
	if m.terminal {
		return nil
	}
	m.terminal = true
	return []agent.Event{agent.ErrorEvent{
		Message: "opencode serve child terminated mid-session: " + reason,
		Code:    "server_crashed",
	}}
}

func decodeErr(what string, ev serverEvent, err error) agent.Event {
	return agent.ErrorEvent{
		Message: fmt.Sprintf("provider/opencode: decode %s: %v", what, err),
		Code:    "decode_" + strings.ReplaceAll(what, ".", "_"),
		Raw:     ev.Properties,
	}
}

// extractFinishReason leniently pulls a finish reason out of the untyped
// step.ended `finish` field, which may be a bare string ("stop") or an object
// ({reason|type|finishReason: "stop"}).
func extractFinishReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Reason       string `json:"reason"`
		Type         string `json:"type"`
		FinishReason string `json:"finishReason"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch {
		case obj.Reason != "":
			return obj.Reason
		case obj.FinishReason != "":
			return obj.FinishReason
		case obj.Type != "":
			return obj.Type
		}
	}
	return ""
}

// isStopReason reports whether a finish reason marks a completed turn (no more
// tool calls pending). Anything tool-call-shaped is NON-terminal.
func isStopReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool-calls", "tool_calls", "tool", "continue":
		return false
	case "stop", "end_turn", "end", "done", "complete", "completed", "":
		return true
	default:
		// Unknown finish reasons (e.g. "length", "content-filter") end the turn.
		return true
	}
}

// errString renders an untyped error payload (string or {message}/{_tag}) to a
// human line, falling back to def.
func errString(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Tag     string `json:"_tag"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch {
		case obj.Message != "":
			return obj.Message
		case obj.Tag != "":
			return obj.Tag
		case obj.Name != "":
			return obj.Name
		}
	}
	return def
}
