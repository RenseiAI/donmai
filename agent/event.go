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
	EventLlmCall       EventKind = "llm_call"
	EventToolUse       EventKind = "tool_use"
	EventToolResult    EventKind = "tool_result"
	EventToolProgress  EventKind = "tool_progress"
	EventResult        EventKind = "result"
	EventError         EventKind = "error"
)

// Event is the sealed-interface base type for all agent event variants.
//
// Implementations: InitEvent, SystemEvent, AssistantTextEvent,
// LlmCallEvent, ToolUseEvent, ToolResultEvent, ToolProgressEvent,
// ResultEvent, ErrorEvent. The unexported isAgentEvent marker prevents external
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

// LlmUsageSource records whether an LlmCallEvent carries usage reported for
// that exact provider call or a session/turn aggregate copied without
// apportionment. Consumers must never interpret aggregate usage as a measured
// per-call value.
type LlmUsageSource string

const (
	// LlmUsageProvider means the provider reported usage for this exact call.
	LlmUsageProvider LlmUsageSource = "provider"
	// LlmUsageAggregate means the provider exposed only a rolled-up total. The
	// event is necessarily synthetic and preserves that total without dividing
	// or otherwise inventing per-call usage.
	LlmUsageAggregate LlmUsageSource = "aggregate"
)

// LlmCallEvent captures one completed request/response exchange with a model.
// Provider adapters emit it when their native stream exposes per-call usage;
// the runtime span processor may synthesize one at a terminal ResultEvent when
// only aggregate usage is available. Prompt and completion bodies are never
// carried on this event: optional prompt/context correlation is digest-only.
type LlmCallEvent struct {
	// TraceID, SpanID, and ParentSpanID are populated by the runner's session
	// correlator. Provider adapters may leave them empty.
	TraceID      string `json:"traceId,omitempty"`
	SpanID       string `json:"spanId,omitempty"`
	ParentSpanID string `json:"parentSpanId,omitempty"`

	// System is the accepted GenAI provider/system identifier (for example,
	// "anthropic" or "openai"). Model is the requested model identifier.
	System string `json:"system,omitempty"`
	Model  string `json:"model,omitempty"`

	InputTokens       int64  `json:"inputTokens,omitempty"`
	OutputTokens      int64  `json:"outputTokens,omitempty"`
	CachedInputTokens int64  `json:"cachedInputTokens,omitempty"`
	FinishReason      string `json:"finishReason,omitempty"`

	// StartTimeUnixNano and EndTimeUnixNano use decimal strings so JSON
	// consumers do not lose fixed64 precision. Empty values are stamped by the
	// runner at the nearest observable call boundary.
	StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   string `json:"endTimeUnixNano,omitempty"`

	// UsageSource is explicit even when token counts are zero. Synthetic is
	// true only for aggregate fallback events; such events never fabricate
	// prompt/completion bodies or split aggregate counts across calls.
	UsageSource LlmUsageSource `json:"usageSource"`
	Synthetic   bool           `json:"synthetic,omitempty"`

	PromptHash      string `json:"promptHash,omitempty"`
	ContextHash     string `json:"contextHash,omitempty"`
	ModelSnapshotID string `json:"modelSnapshotId,omitempty"`
}

// Kind reports the EventKind discriminant.
func (LlmCallEvent) Kind() EventKind { return EventLlmCall }
func (LlmCallEvent) isAgentEvent()   {}

// ToolUseEvent fires when the agent invokes a tool. Verbatim port of
// AgentToolUseEvent.
type ToolUseEvent struct {
	// TraceID, SpanID, and ParentSpanID are populated by the runner's session
	// correlator. SpanID is stable through the matching ToolResultEvent and
	// ParentSpanID identifies the enclosing LLM call.
	TraceID      string `json:"traceId,omitempty"`
	SpanID       string `json:"spanId,omitempty"`
	ParentSpanID string `json:"parentSpanId,omitempty"`

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
	// TraceID, SpanID, and ParentSpanID mirror the matching ToolUseEvent.
	TraceID      string `json:"traceId,omitempty"`
	SpanID       string `json:"spanId,omitempty"`
	ParentSpanID string `json:"parentSpanId,omitempty"`

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
	case EventLlmCall:
		var ev LlmCallEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode LlmCallEvent: %w", err)
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
