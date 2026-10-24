package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
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
	EventUsage         EventKind = "usage"
	EventContextUsage  EventKind = "context_usage"
	EventSubagent      EventKind = "subagent"
	EventResult        EventKind = "result"
	EventError         EventKind = "error"
)

// Event is the sealed-interface base type for all agent event variants.
//
// Implementations: InitEvent, SystemEvent, AssistantTextEvent,
// LlmCallEvent, ToolUseEvent, ToolResultEvent, ToolProgressEvent,
// ContextUsageEvent, SubagentEvent, ResultEvent, ErrorEvent. The unexported isAgentEvent marker prevents external
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

// ObservedModelIdentity carries only native response evidence. Components
// remain empty independently when the response does not report them.
type ObservedModelIdentity struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Version  string `json:"version,omitempty"`
}

// SystemSubtypeModelIdentity is a metadata-only serving observation. It is
// not a model call or usage measurement and must not suppress aggregate usage.
const SystemSubtypeModelIdentity = "model_identity"

// SystemSubtypeGenerationID identifies a metadata-only response-id observation.
const SystemSubtypeGenerationID = "generation_id"

// SystemEvent is a provider-emitted lifecycle/status event (compaction,
// rate-limit notice, etc.). Verbatim port of AgentSystemEvent.
type SystemEvent struct {
	// GenerationID is a native per-request response id, independent of usage.
	GenerationID string `json:"generationId,omitempty"`
	// ObservedModel is native response metadata, independent of configured
	// model/provider and ordinary status Message. Nil means not observed.
	ObservedModel *ObservedModelIdentity `json:"observedModel,omitempty"`
	// Subtype is the provider-defined event subtype (e.g. "compaction",
	// "rate_limited").
	Subtype string `json:"subtype"`

	// Message is an optional human-readable message.
	Message string `json:"message,omitempty"`

	// Upstream is the structured endpoint error captured where the
	// harness exposes it (pi provider-error observations and the
	// stream-json API-error event). Nil elsewhere.
	Upstream *UpstreamError `json:"upstream,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (SystemEvent) Kind() EventKind { return EventSystem }
func (SystemEvent) isAgentEvent()   {}

// SystemSubtypeToolCallRefusedOutputLimit is the SystemEvent subtype a
// harness emits for a tool call the runtime refused BEFORE execution because
// the model's response stopped on its output-token limit (the call's
// arguments may be truncated). It is an observation, not an error: nothing
// ran. The activity poster forwards it as a generic context marker so the
// refusal — and a session repeatedly hitting the limit — is visible beyond
// the local event log.
const SystemSubtypeToolCallRefusedOutputLimit = "tool_call_refused_output_limit"

// SystemSubtypeProviderError is the SystemEvent subtype a harness emits when
// a model call ended on a provider error (a gateway 503/504, an overloaded or
// unreachable model) and the harness did not recover within the turn — the
// turn is about to end on that error rather than because the agent stopped.
// It is an observation, not a terminal: the turn still ends with its
// ResultEvent. Message carries the provider's error text, with a
// "; isRetryable=false" suffix when the harness reports the failure as not
// retryable (a deterministic 4xx such as invalid parameters). A later
// assistant message or tool call in the same turn means the model recovered.
// The runner retries a retryable turn rather than nudging it, and ends a
// non-retryable one at once with the error recorded (runner/turn_continuation.go).
const SystemSubtypeProviderError = "provider_error"

// ProviderErrorNotRetryableSuffix marks a provider-error observation the
// harness reports as not retryable. The runner strips it before recording
// the error text, so the receipt keeps the provider's own message.
const ProviderErrorNotRetryableSuffix = "; isRetryable=false"

// SystemSubtypeStreamProgress is the SystemEvent subtype a harness emits
// while a model response is still being generated: a streaming delta
// arrived from the model endpoint. It carries no content — the complete
// message still arrives on the harness's usual completion event — and
// exists only as a liveness signal so the runner can tell a healthy long
// generation or reasoning pass (deltas keep arriving) from a hung model
// request (no deltas arrive). Harnesses that expose no streaming signal
// simply never emit it. The runner's stall detector disarms on it; the
// activity poster drops it like other runner-internal lifecycle signals.
const SystemSubtypeStreamProgress = "stream_progress"

// StreamProgressEvent builds the SystemSubtypeStreamProgress liveness
// signal: one streaming delta arrived from the model endpoint while a
// response streams. Content-free by design; Message is always empty.
func StreamProgressEvent() SystemEvent {
	return SystemEvent{Subtype: SystemSubtypeStreamProgress}
}

// SystemSubtypeReasoningEffort is the SystemEvent subtype the runner emits
// once per session, after spawn and before any turn output, recording the
// reasoning effort it requested from the harness. Message carries the
// configured EffortLevel, or is empty when the session carried none — in
// which case no level was requested and the harness or model default applies.
// The activity poster forwards it as a context marker built from that fixed
// vocabulary, so every session's record states the effort it ran at.
const SystemSubtypeReasoningEffort = "reasoning_effort"

// ReasoningEffortEvent builds the SystemSubtypeReasoningEffort event for the
// effort a session was spawned with.
func ReasoningEffortEvent(effort EffortLevel) SystemEvent {
	return SystemEvent{Subtype: SystemSubtypeReasoningEffort, Message: string(effort)}
}

// SystemSubtypeToolCallBounds is the SystemEvent subtype a harness emits
// once per session, after the session's InitEvent (which stays first, per
// the event contract) and before any turn output, recording the bound it
// applies to a single tool call: a call that runs past the bound is stopped
// and returns an error to the agent, which can then continue — the bound
// ends the CALL, never the session. Message carries the bound in
// seconds as a decimal integer ("300"), or is empty when the session
// carries no bound. The activity poster forwards it as a context marker
// built from that fixed vocabulary, so every session's record states the
// bound it ran at.
const SystemSubtypeToolCallBounds = "tool_call_bounds"

// SystemSubtypeHarnessLifecycle is the SystemEvent subtype a harness emits
// when one of its per-launch activity hooks fires: the CLI's own
// session-start, prompt-submit, stop, and permission-request moments,
// relayed observe-only (the hook prints nothing, so the session proceeds
// exactly as it would with no hooks installed). Message carries the moment
// in fixed vocabulary — one of the HarnessLifecycleMoment constants — so
// the activity poster can forward it as a context marker without ever
// carrying free-form hook output.
const SystemSubtypeHarnessLifecycle = "harness_lifecycle"

// Harness lifecycle moments, in the snake_case vocabulary the CLIs use on
// the wire for these events.
const (
	HarnessLifecycleSessionStart      = "session_start"
	HarnessLifecycleUserPromptSubmit  = "user_prompt_submit"
	HarnessLifecycleStop              = "stop"
	HarnessLifecyclePermissionRequest = "permission_request"
)

// HarnessLifecycleEvent builds the SystemSubtypeHarnessLifecycle event for
// one relayed hook fire. moment must be one of the HarnessLifecycleMoment
// constants; the relay parser allowlists them before this is ever called.
func HarnessLifecycleEvent(moment string) SystemEvent {
	return SystemEvent{Subtype: SystemSubtypeHarnessLifecycle, Message: moment}
}

// ToolCallBoundsEvent builds the SystemSubtypeToolCallBounds event for the
// per-tool-call timeout bound a session was spawned with, in seconds.
// A non-positive bound means no bound is enforced.
func ToolCallBoundsEvent(boundSeconds int) SystemEvent {
	if boundSeconds <= 0 {
		return SystemEvent{Subtype: SystemSubtypeToolCallBounds}
	}
	return SystemEvent{Subtype: SystemSubtypeToolCallBounds, Message: strconv.Itoa(boundSeconds)}
}

// AssistantTextEvent carries an incremental assistant-text output chunk.
// Verbatim port of AgentAssistantTextEvent. The runner accumulates Text
// across multiple events to scan for the WORK_RESULT marker.
type AssistantTextEvent struct {
	// Text is the assistant text chunk.
	Text string `json:"text"`

	// ParentToolUseID is the provider-native id of the delegation tool
	// call this text was emitted inside (the sub-agent's parent). Empty
	// when the text belongs to the top-level agent or the adapter cannot
	// know the parent — adapters never guess.
	ParentToolUseID string `json:"parentToolUseId,omitempty"`

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
	// GenerationID is the native response id when the harness exposes it.
	GenerationID string `json:"generationId,omitempty"`
	// TraceID, SpanID, and ParentSpanID are populated by the runner's session
	// correlator. Provider adapters may leave them empty.
	TraceID      string `json:"traceId,omitempty"`
	SpanID       string `json:"spanId,omitempty"`
	ParentSpanID string `json:"parentSpanId,omitempty"`

	// System is the accepted GenAI provider/system identifier (for example,
	// "anthropic" or "openai"). Model is the requested model identifier.
	System string `json:"system,omitempty"`
	Model  string `json:"model,omitempty"`

	// ResponseModel and ResponseModelProvider are observed response metadata.
	// Producers must leave either empty when the native response does not
	// identify it. Requested aliases, configured providers and System are not
	// response evidence. ModelSnapshotID separately identifies an exact version.
	ResponseModel         string `json:"responseModel,omitempty"`
	ResponseModelProvider string `json:"responseModelProvider,omitempty"`

	InputTokens       int64 `json:"inputTokens,omitempty"`
	OutputTokens      int64 `json:"outputTokens,omitempty"`
	CachedInputTokens int64 `json:"cachedInputTokens,omitempty"`
	// CacheWriteTokens is the cache-write (cache creation) input token
	// count. Like CachedInputTokens it is excluded from InputTokens.
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// ReasoningTokens is the reasoning token count inside OutputTokens,
	// never an additive extra (see CostData).
	ReasoningTokens int64  `json:"reasoningTokens,omitempty"`
	FinishReason    string `json:"finishReason,omitempty"`
	// ObservedCostUsd is the provider's price for this exact call. A pointer
	// distinguishes a reported zero from an absent price. Estimated prices
	// and aggregate fallbacks must not populate it.
	ObservedCostUsd *float64 `json:"observedCostUsd,omitempty"`
	// TurnCompleted is set only by a native completed model-turn event.
	// Synthetic usage, tool results and text output never set it.
	TurnCompleted bool `json:"turnCompleted,omitempty"`

	// Attempts counts the serving attempts behind this call. Pi reports 1
	// per model request: a harness auto-retry that fails surfaces as its
	// own error call, so the session total (the sum over calls) still
	// counts every attempt. Zero means the harness did not report
	// (unknown) — never "zero attempts" — so consumers sum only positive
	// values and omit an all-unknown session total.
	Attempts int `json:"attempts,omitempty"`

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

	// UsageV2 is the optional versioned inference-usage atom: one logical
	// inference and its ordered physical attempts. Nil means legacy/partial
	// evidence — only the scalar fields above apply — and stays valid so
	// old events keep decoding. When present it is strictly validated on
	// marshal and decode; see LlmInferenceUsage.
	UsageV2 *LlmInferenceUsage `json:"usageV2,omitempty"`
}

// Kind reports the EventKind discriminant.
func (LlmCallEvent) Kind() EventKind { return EventLlmCall }
func (LlmCallEvent) isAgentEvent()   {}

// LlmInferenceUsageVersionV2 is the only accepted LlmInferenceUsage
// version. The version is exact-match and fail-closed: empty or unknown
// versions are rejected on marshal and decode.
const LlmInferenceUsageVersionV2 = "v2"

// LlmInferenceEvidence records how an LlmInferenceUsage was evidenced.
// Provider means the model endpoint reported usage for the exact call;
// aggregate means only a rolled-up total was exposed.
type LlmInferenceEvidence string

// Accepted LlmInferenceEvidence values.
const (
	LlmInferenceEvidenceProvider  LlmInferenceEvidence = "provider"
	LlmInferenceEvidenceAggregate LlmInferenceEvidence = "aggregate"
)

// LlmInferenceCompleteness records whether an LlmInferenceUsage accounts
// for every physical attempt. Only complete usage requires logical totals
// to equal attempt sums; partial and unknown evidence never invent values.
type LlmInferenceCompleteness string

// Accepted LlmInferenceCompleteness values.
const (
	LlmInferenceCompletenessComplete LlmInferenceCompleteness = "complete"
	LlmInferenceCompletenessPartial  LlmInferenceCompleteness = "partial"
	LlmInferenceCompletenessUnknown  LlmInferenceCompleteness = "unknown"
)

// LlmInferenceStatus is the outcome of the logical inference.
type LlmInferenceStatus string

// Accepted LlmInferenceStatus values.
const (
	LlmInferenceStatusSucceeded LlmInferenceStatus = "succeeded"
	LlmInferenceStatusFailed    LlmInferenceStatus = "failed"
)

// LlmInferenceCounts carries presence-preserving token classes and the
// native charge: pointers so absent differs from reported zero through
// the JSON round-trip. Reasoning sits inside output, never as an extra
// (see CostData); cache classes are excluded from input.
type LlmInferenceCounts struct {
	InputTokens       *int64   `json:"inputTokens,omitempty"`
	OutputTokens      *int64   `json:"outputTokens,omitempty"`
	CachedInputTokens *int64   `json:"cachedInputTokens,omitempty"`
	CacheWriteTokens  *int64   `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens   *int64   `json:"reasoningTokens,omitempty"`
	NativeCostUsd     *float64 `json:"nativeCostUsd,omitempty"`
}

// LlmInferenceUsage is the versioned additive usage atom on LlmCallEvent:
// one logical inference and its ordered physical attempts. A nil group is
// legacy/partial evidence and always valid.
type LlmInferenceUsage struct {
	// Version must be LlmInferenceUsageVersionV2; InferenceID is the
	// logical identity. Evidence, Completeness and Status accept only
	// their declared constants.
	Version      string                   `json:"version"`
	InferenceID  string                   `json:"inferenceId"`
	Evidence     LlmInferenceEvidence     `json:"evidence"`
	Completeness LlmInferenceCompleteness `json:"completeness"`
	Status       LlmInferenceStatus       `json:"status"`

	// Provider and GenerationID are serving identity when exposed;
	// RequestedModel is asked for, ResolvedModel is what responded;
	// Route and Auth are free-form serving metadata.
	Provider       string `json:"provider,omitempty"`
	GenerationID   string `json:"generationId,omitempty"`
	RequestedModel string `json:"requestedModel,omitempty"`
	ResolvedModel  string `json:"resolvedModel,omitempty"`
	Route          string `json:"route,omitempty"`
	Auth           string `json:"auth,omitempty"`

	// Counts flattens the token classes and native charge onto the object.
	LlmInferenceCounts

	// Attempts are the ordered physical attempts behind the inference.
	Attempts []LlmInferenceAttempt `json:"attempts"`
}

// LlmInferenceAttempt is one physical serving attempt behind a logical
// inference. Ordinals are 1-based, contiguous, and slice-ordered.
// AttemptID is unique per inference; stamps are decimal nanoseconds that
// may be absent, with end never preceding start when both are present.
type LlmInferenceAttempt struct {
	AttemptID         string `json:"attemptId"`
	Ordinal           int    `json:"ordinal"`
	StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   string `json:"endTimeUnixNano,omitempty"`

	// Counts flattens the token classes and native charge onto the object.
	LlmInferenceCounts
}

// validate reports whether the usage atom is well-formed. A nil receiver
// is valid: absent usage-v2 is legacy/partial evidence. MarshalEvent and
// the UnmarshalEvent llm_call branch both apply it, so neither in-memory
// values nor stored JSON bypass the checks.
func (u *LlmInferenceUsage) validate() error {
	if u == nil {
		return nil
	}
	if u.Version != LlmInferenceUsageVersionV2 {
		return fmt.Errorf("agent: llm inference usage version %q must be %q", u.Version, LlmInferenceUsageVersionV2)
	}
	if u.InferenceID == "" {
		return fmt.Errorf("agent: llm inference usage requires an inference id")
	}
	switch u.Evidence {
	case LlmInferenceEvidenceProvider, LlmInferenceEvidenceAggregate:
	default:
		return fmt.Errorf("agent: llm inference usage evidence %q is unknown", string(u.Evidence))
	}
	switch u.Completeness {
	case LlmInferenceCompletenessComplete, LlmInferenceCompletenessPartial, LlmInferenceCompletenessUnknown:
	default:
		return fmt.Errorf("agent: llm inference usage completeness %q is unknown", string(u.Completeness))
	}
	switch u.Status {
	case LlmInferenceStatusSucceeded, LlmInferenceStatusFailed:
	default:
		return fmt.Errorf("agent: llm inference usage status %q is unknown", string(u.Status))
	}
	if err := checkInferenceCounts("inference", u.LlmInferenceCounts); err != nil {
		return err
	}
	if len(u.Attempts) == 0 {
		return fmt.Errorf("agent: llm inference usage requires at least one attempt")
	}
	seen := make(map[string]struct{}, len(u.Attempts))
	for i := range u.Attempts {
		a := &u.Attempts[i]
		if a.AttemptID == "" {
			return fmt.Errorf("agent: llm inference usage attempt %d requires an attempt id", i+1)
		}
		if _, dup := seen[a.AttemptID]; dup {
			return fmt.Errorf("agent: llm inference usage attempt id %q is duplicated", a.AttemptID)
		}
		seen[a.AttemptID] = struct{}{}
		if a.Ordinal != i+1 {
			return fmt.Errorf("agent: llm inference usage attempt %q ordinal %d must be %d in order", a.AttemptID, a.Ordinal, i+1)
		}
		if err := checkInferenceCounts(fmt.Sprintf("attempt %q", a.AttemptID), a.LlmInferenceCounts); err != nil {
			return err
		}
		if err := validateInferenceInterval(a.AttemptID, a.StartTimeUnixNano, a.EndTimeUnixNano); err != nil {
			return err
		}
	}
	if u.Completeness == LlmInferenceCompletenessComplete {
		if err := u.validateCompleteTotals(); err != nil {
			return err
		}
	}
	return nil
}

// checkInferenceCounts rejects negative counts, non-finite or negative
// charges, and reasoning above output. Nil means absent and is valid.
func checkInferenceCounts(where string, c LlmInferenceCounts) error {
	counts := []struct {
		name  string
		value *int64
	}{{"inputTokens", c.InputTokens}, {"outputTokens", c.OutputTokens}, {"cachedInputTokens", c.CachedInputTokens}, {"cacheWriteTokens", c.CacheWriteTokens}, {"reasoningTokens", c.ReasoningTokens}}
	for _, f := range counts {
		if f.value != nil && *f.value < 0 {
			return fmt.Errorf("agent: llm inference usage %s %s count %d is negative", where, f.name, *f.value)
		}
	}
	if c.NativeCostUsd != nil && (*c.NativeCostUsd < 0 || math.IsNaN(*c.NativeCostUsd) || math.IsInf(*c.NativeCostUsd, 0)) {
		return fmt.Errorf("agent: llm inference usage %s native charge %v is not finite nonnegative", where, *c.NativeCostUsd)
	}
	if c.ReasoningTokens != nil && c.OutputTokens != nil && *c.ReasoningTokens > *c.OutputTokens {
		return fmt.Errorf("agent: llm inference usage %s reasoning %d exceeds output %d", where, *c.ReasoningTokens, *c.OutputTokens)
	}
	return nil
}

// validateInferenceInterval rejects malformed decimal-nanosecond stamps
// and an end that precedes its start. Either stamp may be absent.
func validateInferenceInterval(attemptID, start, end string) error {
	if start == "" && end == "" {
		return nil
	}
	var startV, endV int64
	if start != "" {
		v, err := strconv.ParseInt(start, 10, 64)
		if err != nil {
			return fmt.Errorf("agent: llm inference usage attempt %q start time %q is not decimal nanoseconds", attemptID, start)
		}
		startV = v
	}
	if end != "" {
		v, err := strconv.ParseInt(end, 10, 64)
		if err != nil {
			return fmt.Errorf("agent: llm inference usage attempt %q end time %q is not decimal nanoseconds", attemptID, end)
		}
		endV = v
	}
	if start != "" && end != "" && endV < startV {
		return fmt.Errorf("agent: llm inference usage attempt %q end time precedes start time", attemptID)
	}
	return nil
}

// intCounts returns the five token classes in wire order for indexed scans.
func (c LlmInferenceCounts) intCounts() []*int64 {
	return []*int64{c.InputTokens, c.OutputTokens, c.CachedInputTokens, c.CacheWriteTokens, c.ReasoningTokens}
}

// inferenceChargeEqual compares native-charge totals within a small
// relative tolerance so decimal sums that are inexact in binary
// (0.1 + 0.2 vs 0.3) still validate. Exact equality stays the fast path.
func inferenceChargeEqual(total, sum float64) bool {
	if total == sum {
		return true
	}
	diff := math.Abs(total - sum)
	tol := 1e-9 * math.Max(1, math.Max(math.Abs(total), math.Abs(sum)))
	return diff <= tol
}

// validateCompleteTotals requires each present logical total to equal its
// attempt sum with uniform presence per class; a class absent everywhere
// stays consistent. Partial and unknown evidence skip this.
func (u *LlmInferenceUsage) validateCompleteTotals() error {
	names := []string{"inputTokens", "outputTokens", "cachedInputTokens", "cacheWriteTokens", "reasoningTokens"}
	logical := u.LlmInferenceCounts.intCounts()
	for i, name := range names {
		var sum int64
		present := 0
		for _, a := range u.Attempts {
			if v := a.LlmInferenceCounts.intCounts()[i]; v != nil {
				sum += *v
				present++
			}
		}
		switch {
		case logical[i] == nil:
			if present > 0 {
				return fmt.Errorf("agent: llm inference usage complete %s total is absent but attempts report values", name)
			}
		case present != len(u.Attempts):
			return fmt.Errorf("agent: llm inference usage complete %s total is present but an attempt omits it", name)
		case *logical[i] != sum:
			return fmt.Errorf("agent: llm inference usage complete %s total %d does not equal attempt sum %d", name, *logical[i], sum)
		}
	}
	var chargeSum float64
	chargePresent := 0
	for _, a := range u.Attempts {
		if a.NativeCostUsd != nil {
			chargeSum += *a.NativeCostUsd
			chargePresent++
		}
	}
	switch {
	case u.NativeCostUsd == nil:
		if chargePresent > 0 {
			return fmt.Errorf("agent: llm inference usage complete native charge total is absent but attempts report values")
		}
	case chargePresent != len(u.Attempts):
		return fmt.Errorf("agent: llm inference usage complete native charge total is present but an attempt omits it")
	case !inferenceChargeEqual(*u.NativeCostUsd, chargeSum):
		return fmt.Errorf("agent: llm inference usage complete native charge total %v does not equal attempt sum %v", *u.NativeCostUsd, chargeSum)
	}
	return nil
}

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

	// ParentToolUseID is the provider-native id of the delegation tool
	// call this tool call was emitted inside (the sub-agent's parent).
	// Empty on top-level calls and on adapters that cannot know the
	// parent — adapters never guess.
	ParentToolUseID string `json:"parentToolUseId,omitempty"`

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

	// ParentToolUseID mirrors ToolUseEvent.ParentToolUseID: the
	// provider-native id of the delegation tool call this result belongs
	// to. Empty on top-level results and on adapters that cannot know
	// the parent — adapters never guess.
	ParentToolUseID string `json:"parentToolUseId,omitempty"`

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

// UsageEvent carries a sparse subscription quota-window update observed
// mid-turn (for example the streamed rate-limit event). Windows merge by
// ID onto the published snapshot; omitted windows are unchanged.
type UsageEvent struct {
	// Message is the provider's status for the update, when reported
	// (for example "allowed_warning").
	Message string `json:"message,omitempty"`

	// Usage is the sparse update. Never nil on a well-formed event.
	Usage *UsageLimitsUpdate `json:"usage,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (UsageEvent) Kind() EventKind { return EventUsage }
func (UsageEvent) isAgentEvent()   {}

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

// SubagentPhase is the lifecycle phase carried on a SubagentEvent.
type SubagentPhase string

// Subagent lifecycle phases. Stable wire values.
const (
	SubagentStarted   SubagentPhase = "started"
	SubagentCompleted SubagentPhase = "completed"
	SubagentFailed    SubagentPhase = "failed"
)

// SubagentEvent is the typed lifecycle event an adapter emits around a
// native sub-agent delegation tool call: `started` when the delegation
// tool is invoked, `completed` when its result arrives successfully,
// `failed` when its result arrives as an error.
//
// ToolName is the adapter's own delegation tool name (the adapter names
// its tools; the runner keeps no list). ToolUseID pairs the lifecycle
// with the delegating ToolUseEvent/ToolResultEvent. ChildSessionID is
// the sub-agent's session ref when the harness exposes one, else empty
// — adapters never guess.
type SubagentEvent struct {
	// TraceID, SpanID, and ParentSpanID are populated by the runner's session
	// correlator. SpanID identifies the subagent span the runtime span
	// processor emits for this lifecycle.
	TraceID      string `json:"traceId,omitempty"`
	SpanID       string `json:"spanId,omitempty"`
	ParentSpanID string `json:"parentSpanId,omitempty"`

	// ToolName is the delegation tool identifier (e.g. "Task", "Agent").
	ToolName string `json:"toolName"`

	// ToolUseID pairs with the delegating ToolUseEvent.ToolUseID.
	ToolUseID string `json:"toolUseId,omitempty"`

	// Phase is the lifecycle phase: started | completed | failed.
	Phase SubagentPhase `json:"phase"`

	// ChildSessionID is the sub-agent's session ref when known.
	ChildSessionID string `json:"childSessionId,omitempty"`

	// ChildSessionTransport is the transport reported by the delegation result.
	ChildSessionTransport string `json:"childSessionTransport,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (SubagentEvent) Kind() EventKind { return EventSubagent }
func (SubagentEvent) isAgentEvent()   {}

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
	// ObservedCostUsd and ObservedTurns are native cumulative observations.
	// They can be independently absent; a non-nil zero is reported zero.
	// Cost remains the legacy aggregate used by existing billing readers.
	ObservedCostUsd *float64 `json:"observedCostUsd,omitempty"`
	ObservedTurns   *int     `json:"observedTurns,omitempty"`

	// Upstream is the structured endpoint error captured where the
	// harness exposes it (pi provider-error observations and the
	// stream-json API-error event). Nil elsewhere.
	Upstream *UpstreamError `json:"upstream,omitempty"`

	// Raw is the provider-native event payload.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ResultEvent) Kind() EventKind { return EventResult }
func (ResultEvent) isAgentEvent()   {}

// ErrorEvent is a non-recoverable provider error fired before any
// terminal ResultEvent. The provider closes the events channel after
// emitting an ErrorEvent. Verbatim port of AgentErrorEvent.
//
// The one exception is an ErrorEvent with SessionContinues set: an error
// the provider reports for the record while the session runs on to its own
// terminal. It is not the session's terminal, so a consumer records it but
// never treats it as the session's failure.
type ErrorEvent struct {
	// Message is the human-readable error message.
	Message string `json:"message"`

	// Code is an optional provider-defined error code (e.g.
	// "spawn_no_result", "rate_limited").
	Code string `json:"code,omitempty"`

	// SessionContinues marks an error the provider reports without ending
	// the session: the stream carries on, and the session ends on its own
	// terminal event. Such an error is never the session's failure.
	SessionContinues bool `json:"sessionContinues,omitempty"`

	// Upstream is the structured endpoint error captured where the
	// harness exposes it (pi provider-error observations and the
	// stream-json API-error event). Nil elsewhere.
	Upstream *UpstreamError `json:"upstream,omitempty"`

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
	switch ev := e.(type) {
	case LlmCallEvent:
		if err := ev.UsageV2.validate(); err != nil {
			return nil, err
		}
	case *LlmCallEvent:
		if err := ev.UsageV2.validate(); err != nil {
			return nil, err
		}
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
		if err := ev.UsageV2.validate(); err != nil {
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
	case EventUsage:
		var ev UsageEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode UsageEvent: %w", err)
		}
		return ev, nil
	case EventContextUsage:
		var ev ContextUsageEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode ContextUsageEvent: %w", err)
		}
		return ev, nil
	case EventSubagent:
		var ev SubagentEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode SubagentEvent: %w", err)
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
