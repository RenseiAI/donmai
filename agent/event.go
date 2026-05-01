package agent

import (
	"encoding/json"
	"fmt"
)

// EventKind is the discriminant for Event variants.
//
// Verbatim port of the legacy TS AgentEvent.type literal union.
// Each variant is a dedicated struct; the discriminated-union pattern
// uses a Go interface with a private marker method.
type EventKind string

// EventKind constants. The wire values match the legacy TS literal
// union (e.g. "init", "assistant_text", "tool_use") so providers can
// dispatch off the JSON type field.
const (
	EventInit          EventKind = "init"
	EventSystem        EventKind = "system"
	EventAssistantText EventKind = "assistant_text"
	EventToolUse       EventKind = "tool_use"
	EventToolResult    EventKind = "tool_result"
	EventToolProgress  EventKind = "tool_progress"
	EventResult        EventKind = "result"
	EventError         EventKind = "error"
)

// Event is the sealed-interface base type for all agent event variants.
//
// Implementations: InitEvent, SystemEvent, AssistantTextEvent,
// ToolUseEvent, ToolResultEvent, ToolProgressEvent, ResultEvent,
// ErrorEvent. The unexported isAgentEvent marker prevents external
// packages from satisfying the interface, keeping the discriminated
// union closed.
//
// To decode an Event polymorphically from JSON use UnmarshalEvent.
type Event interface {
	// Kind returns the discriminant for this variant.
	Kind() EventKind
	// isAgentEvent is the unexported marker that seals the interface.
	isAgentEvent()
}

// InitEvent fires when the agent has initialized; it captures the
// provider-native session identifier. Verbatim port of AgentInitEvent.
type InitEvent struct {
	// SessionID is the provider-native session/thread id (Claude UUID,
	// Codex thread id). Captured by the runner for resume.
	SessionID string `json:"sessionId"`

	// Raw is the provider-native event payload, opaque to callers.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (InitEvent) Kind() EventKind { return EventInit }
func (InitEvent) isAgentEvent()   {}

// SystemEvent is a provider-emitted lifecycle/status event (compaction,
// rate-limit notice, etc.). Verbatim port of AgentSystemEvent.
type SystemEvent struct {
	// Subtype is the provider-defined event subtype (e.g. "compaction",
	// "rate_limited").
	Subtype string `json:"subtype"`

	// Message is an optional human-readable message.
	Message string `json:"message,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (SystemEvent) Kind() EventKind { return EventSystem }
func (SystemEvent) isAgentEvent()   {}

// AssistantTextEvent carries an incremental assistant-text output chunk.
// Verbatim port of AgentAssistantTextEvent. The runner accumulates Text
// across multiple events to scan for the WORK_RESULT marker.
type AssistantTextEvent struct {
	// Text is the assistant text chunk.
	Text string `json:"text"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (AssistantTextEvent) Kind() EventKind { return EventAssistantText }
func (AssistantTextEvent) isAgentEvent()   {}

// ToolUseEvent fires when the agent invokes a tool. Verbatim port of
// AgentToolUseEvent.
type ToolUseEvent struct {
	// ToolName is the tool identifier (e.g. "Bash", "Edit",
	// "mcp__af_linear__af_linear_get_issue").
	ToolName string `json:"toolName"`

	// ToolUseID is the provider-native tool-call identifier; pairs
	// with ToolResultEvent.ToolUseID.
	ToolUseID string `json:"toolUseId,omitempty"`

	// Input is the tool-call input map.
	Input map[string]any `json:"input"`

	// ToolCategory is the runner's categorization
	// ("filesystem"|"shell"|"network"|"linear"|"code-intel"). Empty
	// when the runner has not yet categorized the call.
	ToolCategory string `json:"toolCategory,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ToolUseEvent) Kind() EventKind { return EventToolUse }
func (ToolUseEvent) isAgentEvent()   {}

// ToolResultEvent carries a tool-execution result (success or error).
// Verbatim port of AgentToolResultEvent.
type ToolResultEvent struct {
	// ToolName is the tool identifier; may be empty when the provider
	// only reports the tool-use id.
	ToolName string `json:"toolName,omitempty"`

	// ToolUseID pairs with ToolUseEvent.ToolUseID.
	ToolUseID string `json:"toolUseId,omitempty"`

	// Content is the tool's output text or stringified result.
	Content string `json:"content"`

	// IsError is true when the tool reported failure.
	IsError bool `json:"isError"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ToolResultEvent) Kind() EventKind { return EventToolResult }
func (ToolResultEvent) isAgentEvent()   {}

// ToolProgressEvent is a long-running tool's progress tick. Verbatim
// port of AgentToolProgressEvent.
type ToolProgressEvent struct {
	// ToolName is the tool identifier.
	ToolName string `json:"toolName"`

	// ElapsedSeconds is how long the tool has been running.
	ElapsedSeconds float64 `json:"elapsedSeconds"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ToolProgressEvent) Kind() EventKind { return EventToolProgress }
func (ToolProgressEvent) isAgentEvent()   {}

// ResultEvent is the terminal session-outcome event from the provider.
// Distinct from agent.Result which is the runner's higher-level
// session-result struct (see types.go). Verbatim port of AgentResultEvent.
type ResultEvent struct {
	// Success reports whether the session ended successfully.
	Success bool `json:"success"`

	// Message is the completion message (typically only set on success).
	Message string `json:"message,omitempty"`

	// Errors is the list of error messages (typically only set on
	// failure).
	Errors []string `json:"errors,omitempty"`

	// ErrorSubtype is the provider-defined error subtype (e.g.
	// "error_during_execution", "error_max_turns").
	ErrorSubtype string `json:"errorSubtype,omitempty"`

	// Cost is the rolled-up cost/usage for the session.
	Cost *CostData `json:"cost,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ResultEvent) Kind() EventKind { return EventResult }
func (ResultEvent) isAgentEvent()   {}

// ErrorEvent is a non-recoverable provider error fired before any
// terminal ResultEvent. The provider closes the events channel after
// emitting an ErrorEvent. Verbatim port of AgentErrorEvent.
type ErrorEvent struct {
	// Message is the human-readable error message.
	Message string `json:"message"`

	// Code is an optional provider-defined error code (e.g.
	// "spawn_no_result", "rate_limited").
	Code string `json:"code,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ErrorEvent) Kind() EventKind { return EventError }
func (ErrorEvent) isAgentEvent()   {}

// MarshalEvent encodes an Event to JSON, embedding the discriminant
// "kind" field so the value can be round-tripped through UnmarshalEvent.
//
// The wire shape is:
//
//	{"kind": "<EventKind>", ...variant fields...}
//
// Variant fields are flattened onto the outer object. Use MarshalEvent
// when persisting events to JSONL / streaming over HTTP; the JSONL
// shape is what runner/ writes to <worktree>/.agent/events.jsonl per
// F.1.1 §4 step 9.
func MarshalEvent(e Event) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("agent: cannot marshal nil Event")
	}
	// Marshal the variant first to capture its native field layout.
	variantBytes, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal event variant: %w", err)
	}
	// Decode into a generic map and inject the kind discriminant.
	var fields map[string]any
	if err := json.Unmarshal(variantBytes, &fields); err != nil {
		return nil, fmt.Errorf("agent: re-decode event variant: %w", err)
	}
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["kind"] = string(e.Kind())
	return json.Marshal(fields)
}

// UnmarshalEvent decodes an Event from JSON written by MarshalEvent.
// It reads the "kind" discriminator and dispatches to the matching
// variant struct. Unknown kinds return a wrapped error.
//
// This is the polymorphic decode entry point per F.1.1 §2; runner/
// uses it to read <worktree>/.agent/events.jsonl back during recovery.
func UnmarshalEvent(data []byte) (Event, error) {
	var head struct {
		Kind EventKind `json:"kind"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("agent: decode event kind: %w", err)
	}
	switch head.Kind {
	case EventInit:
		var ev InitEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode InitEvent: %w", err)
		}
		return ev, nil
	case EventSystem:
		var ev SystemEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode SystemEvent: %w", err)
		}
		return ev, nil
	case EventAssistantText:
		var ev AssistantTextEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode AssistantTextEvent: %w", err)
		}
		return ev, nil
	case EventToolUse:
		var ev ToolUseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ToolUseEvent: %w", err)
		}
		return ev, nil
	case EventToolResult:
		var ev ToolResultEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ToolResultEvent: %w", err)
		}
		return ev, nil
	case EventToolProgress:
		var ev ToolProgressEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ToolProgressEvent: %w", err)
		}
		return ev, nil
	case EventResult:
		var ev ResultEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ResultEvent: %w", err)
		}
		return ev, nil
	case EventError:
		var ev ErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ErrorEvent: %w", err)
		}
		return ev, nil
	case "":
		return nil, fmt.Errorf("agent: missing kind discriminator on event JSON")
	default:
		return nil, fmt.Errorf("agent: unknown event kind %q", head.Kind)
	}
}
