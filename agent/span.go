package agent

import (
	"encoding/json"
	"fmt"
)

// SpanKind is the discriminant for Span variants.
//
// A Span captures one node in a per-LLM-call observability trace. The
// wire contract is OpenTelemetry GenAI-semconv-aligned: every Span maps
// cleanly to an OTLP span, with GenAI usage attributes on llm spans and
// a donmai.* extension group carrying tenancy/governance context.
//
// The six kinds and their OTel SpanKind mapping are defined canonically
// in ADR-2026-06-28-per-llm-call-observability-span-contract.md. WS1
// ships the wire types only; emission (the harness poster) and ingest
// are separate workstreams that target this contract.
type SpanKind string

// SpanKind constants. The wire values are stable string literals so any
// OTLP-speaking consumer can dispatch off the JSON "kind" field. They
// are distinct from the legacy session-event projection kinds
// (model_request, tool_call, …); the two layers coexist.
const (
	// SpanKindLLM is one LLM request/response turn. Carries gen_ai.*
	// usage attributes (OTel SpanKind: CLIENT).
	SpanKindLLM SpanKind = "llm"
	// SpanKindTool is one tool invocation inside a turn; parentSpanId is
	// the enclosing llm span and toolUseId reuses the hook-bus
	// correlation spine (OTel SpanKind: CLIENT).
	SpanKindTool SpanKind = "tool"
	// SpanKindChain is a composed step grouping child spans (INTERNAL).
	SpanKindChain SpanKind = "chain"
	// SpanKindRetrieval is a context/document retrieval step (INTERNAL).
	SpanKindRetrieval SpanKind = "retrieval"
	// SpanKindAgent is the whole-session root span (INTERNAL).
	SpanKindAgent SpanKind = "agent"
	// SpanKindSubagent is a spawned sub-agent; parentSpanId is the
	// enclosing agent span (INTERNAL).
	SpanKindSubagent SpanKind = "subagent"
)

// AllSpanKinds returns every valid SpanKind. Validation + test helper.
func AllSpanKinds() []SpanKind {
	return []SpanKind{
		SpanKindLLM,
		SpanKindTool,
		SpanKindChain,
		SpanKindRetrieval,
		SpanKindAgent,
		SpanKindSubagent,
	}
}

// Valid reports whether k is a known SpanKind.
func (k SpanKind) Valid() bool {
	switch k {
	case SpanKindLLM, SpanKindTool, SpanKindChain,
		SpanKindRetrieval, SpanKindAgent, SpanKindSubagent:
		return true
	default:
		return false
	}
}

// SpanStatusCode is the OTel-aligned span status code.
type SpanStatusCode string

// Span status codes, matching the OTel status vocabulary.
const (
	StatusUnset SpanStatusCode = "UNSET"
	StatusOK    SpanStatusCode = "OK"
	StatusError SpanStatusCode = "ERROR"
)

// SpanStatus is the OTel span status (code + optional message).
type SpanStatus struct {
	// Code is the status code (UNSET, OK, or ERROR).
	Code SpanStatusCode `json:"code"`
	// Message is an optional human-readable status detail (typically
	// only set on ERROR).
	Message string `json:"message,omitempty"`
}

// GenAIAttributes is the accepted June-28 compatibility group carried by LLM
// spans. The camelCase JSON keys remain frozen for existing consumers. They
// originated from the then-current OpenTelemetry GenAI convention:
//
//	system                    → gen_ai.system
//	requestModel              → gen_ai.request.model
//	usageInputTokens          → gen_ai.usage.input_tokens
//	usageOutputTokens         → gen_ai.usage.output_tokens
//	usageCacheReadInputTokens → gen_ai.usage.cache_read_input_tokens
//	responseFinishReason      → gen_ai.response.finish_reason
//
// Current OpenTelemetry conventions have since renamed several attributes:
// system is now gen_ai.provider.name, operation name is separately required,
// cache-read usage is gen_ai.usage.cache_read.input_tokens, and finish reasons
// are plural. Emitters preserve this wire type; OTLP exporters/ingesters must
// translate it rather than silently changing the golden contract. See
// runtime/span/README.md for the pinned delta and primary-source links.
type GenAIAttributes struct {
	// System is the compatibility provider/system identifier (e.g.
	// "anthropic"). Current OTLP mapping targets gen_ai.provider.name.
	System string `json:"system"`
	// RequestModel is the requested model id (e.g. "claude-opus-4").
	RequestModel string `json:"requestModel"`
	// UsageInputTokens is the prompt/input token count.
	UsageInputTokens int64 `json:"usageInputTokens"`
	// UsageOutputTokens is the completion/output token count.
	UsageOutputTokens int64 `json:"usageOutputTokens"`
	// UsageCacheReadInputTokens is the cache-read input token count. Current
	// OTLP mapping targets gen_ai.usage.cache_read.input_tokens.
	UsageCacheReadInputTokens int64 `json:"usageCacheReadInputTokens,omitempty"`
	// ResponseFinishReason is the provider finish reason (e.g. "end_turn",
	// "max_tokens"). Current OTLP mapping wraps it as the singular element of
	// gen_ai.response.finish_reasons.
	ResponseFinishReason string `json:"responseFinishReason,omitempty"`
}

// DonmaiSpanExtensions is the donmai.* extension attribute group present
// on every span kind. It carries tenancy and governance context the
// GenAI semconv does not cover. The OSS namespace is donmai.*; the
// closed-source platform normalizer maps these at the ingest boundary
// (out of scope for WS1). The camelCase JSON wire keys map to OTLP
// attribute names:
//
//	orgId           → donmai.org_id
//	workspaceId     → donmai.workspace_id
//	sessionId       → donmai.session_id
//	workType        → donmai.work_type
//	poolId          → donmai.pool_id
//	cedarDecisionId → donmai.cedar_decision_id
//	promptHash      → donmai.prompt_hash
//	contextHash     → donmai.context_hash
//	modelSnapshotId → donmai.model_snapshot_id
type DonmaiSpanExtensions struct {
	// OrgID is the owning organization id.
	OrgID string `json:"orgId"`
	// WorkspaceID is the workspace id (equals OrgID in the current
	// tenant model).
	WorkspaceID string `json:"workspaceId"`
	// SessionID is the agent session id this span belongs to.
	SessionID string `json:"sessionId"`
	// WorkType is the work classification (e.g. "sdlc").
	WorkType string `json:"workType,omitempty"`
	// PoolID is the worker-pool id the session ran in.
	PoolID string `json:"poolId,omitempty"`
	// CedarDecisionID links the call to the Cedar authorization decision
	// that permitted it.
	CedarDecisionID string `json:"cedarDecisionId,omitempty"`
	// PromptHash is the digest of the prompt content (content-by-digest;
	// raw prompt text stays off the hot path).
	PromptHash string `json:"promptHash,omitempty"`
	// ContextHash is the digest of the assembled context.
	ContextHash string `json:"contextHash,omitempty"`
	// ModelSnapshotID is the exact model-snapshot identifier used.
	ModelSnapshotID string `json:"modelSnapshotId,omitempty"`
}

// SpanCore is the field set common to every span variant. It is
// anonymously embedded into each variant struct, so its JSON fields are
// flattened onto the variant object on the wire.
//
// Unlike the Event union, Kind is a stored field (not injected by the
// codec): a plain json.Marshal of any variant therefore already carries
// the "kind" discriminator, and UnmarshalSpan dispatches off the
// already-present field.
type SpanCore struct {
	// TraceID is the 16-byte trace id, hex-encoded (OTLP traceId).
	TraceID string `json:"traceId"`
	// SpanID is the 8-byte span id, hex-encoded (OTLP spanId).
	SpanID string `json:"spanId"`
	// ParentSpanID is the parent span's id; empty on a root span.
	ParentSpanID string `json:"parentSpanId,omitempty"`
	// Kind is the span-kind discriminator. Producers MUST set it to the
	// variant's canonical kind (see each variant's spanKind()).
	Kind SpanKind `json:"kind"`
	// Name is the span display name (e.g. "chat anthropic/claude-opus").
	Name string `json:"name"`
	// StartTimeUnixNano is the start time as decimal unix-nanoseconds in
	// a string, matching OTLP/JSON's fixed64 encoding and avoiding
	// precision loss in 53-bit-float consumers. Producers set this to
	// strconv.FormatInt(t.UnixNano(), 10).
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	// EndTimeUnixNano is the end time as decimal unix-nanoseconds in a
	// string (see StartTimeUnixNano).
	EndTimeUnixNano string `json:"endTimeUnixNano"`
	// Status is the OTel span status.
	Status SpanStatus `json:"status"`
	// Donmai is the donmai.* extension attribute group, present on every
	// span kind.
	Donmai DonmaiSpanExtensions `json:"donmai"`
}

// LlmCallSpan is one LLM request/response turn. It is the only kind that
// carries GenAI usage attributes.
type LlmCallSpan struct {
	SpanCore
	// GenAI is the OTel GenAI semconv attribute group (required on llm
	// spans).
	GenAI GenAIAttributes `json:"genAi"`
}

func (LlmCallSpan) spanKind() SpanKind { return SpanKindLLM }
func (LlmCallSpan) isSpan()            {}

// ToolSpan is one tool invocation inside a turn. Its parentSpanId
// anchors it under the enclosing LlmCallSpan and ToolUseID reuses the
// cross-process hook-bus correlation spine.
type ToolSpan struct {
	SpanCore
	// ToolName is the tool identifier (e.g. "Bash", "Edit").
	ToolName string `json:"toolName"`
	// ToolUseID is the hook-bus correlation id; pairs the tool span with
	// its hook-bus events.
	ToolUseID string `json:"toolUseId,omitempty"`
	// IsError reports whether the tool invocation failed.
	IsError bool `json:"isError,omitempty"`
}

func (ToolSpan) spanKind() SpanKind { return SpanKindTool }
func (ToolSpan) isSpan()            {}

// ChainSpan is a composed step grouping child spans.
type ChainSpan struct {
	SpanCore
	// ChainName is the composed-step name.
	ChainName string `json:"chainName,omitempty"`
}

func (ChainSpan) spanKind() SpanKind { return SpanKindChain }
func (ChainSpan) isSpan()            {}

// RetrievalSpan is a context/document retrieval step.
type RetrievalSpan struct {
	SpanCore
	// QueryHash is the digest of the retrieval query.
	QueryHash string `json:"queryHash,omitempty"`
	// DocumentCount is the number of documents retrieved.
	DocumentCount int `json:"documentCount,omitempty"`
}

func (RetrievalSpan) spanKind() SpanKind { return SpanKindRetrieval }
func (RetrievalSpan) isSpan()            {}

// SessionSpan is the whole-session root span.
type SessionSpan struct {
	SpanCore
	// AgentProvider is the harness/provider that ran the session.
	AgentProvider string `json:"agentProvider,omitempty"`
}

func (SessionSpan) spanKind() SpanKind { return SpanKindAgent }
func (SessionSpan) isSpan()            {}

// SubagentSpan is a spawned sub-agent; its parentSpanId anchors it under
// the enclosing SessionSpan.
type SubagentSpan struct {
	SpanCore
	// SubagentProvider is the sub-agent harness/provider.
	SubagentProvider string `json:"subagentProvider,omitempty"`
}

func (SubagentSpan) spanKind() SpanKind { return SpanKindSubagent }
func (SubagentSpan) isSpan()            {}

// Span is the sealed-interface base type for all span variants.
//
// Implementations: LlmCallSpan, ToolSpan, ChainSpan, RetrievalSpan,
// SessionSpan, SubagentSpan. The unexported isSpan marker seals the
// interface so external packages cannot add variants, keeping the
// discriminated union closed. spanKind reports the variant's canonical
// kind (it is unexported to avoid colliding with the SpanCore.Kind
// field).
//
// To decode a Span polymorphically from JSON use UnmarshalSpan.
type Span interface {
	// spanKind returns the canonical SpanKind constant for the variant.
	spanKind() SpanKind
	// isSpan is the unexported marker that seals the interface.
	isSpan()
}

// MarshalSpan validates s and JSON-encodes it. The "kind" discriminator
// is a stored SpanCore field, so the encoded bytes round-trip through
// UnmarshalSpan without any injection. MarshalSpan is a thin validated
// wrapper: it rejects a nil Span and a Span whose canonical kind is not
// a known SpanKind.
func MarshalSpan(s Span) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("agent: cannot marshal nil Span")
	}
	if !s.spanKind().Valid() {
		return nil, fmt.Errorf("agent: marshal span: unknown span kind %q", s.spanKind())
	}
	return json.Marshal(s)
}

// UnmarshalSpan decodes a Span from JSON. It reads the "kind"
// discriminator and dispatches to the matching variant struct. A missing
// kind and an unknown kind both return a wrapped error.
func UnmarshalSpan(data []byte) (Span, error) {
	var head struct {
		Kind SpanKind `json:"kind"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("agent: decode span kind: %w", err)
	}
	switch head.Kind {
	case SpanKindLLM:
		var s LlmCallSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode LlmCallSpan: %w", err)
		}
		return s, nil
	case SpanKindTool:
		var s ToolSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode ToolSpan: %w", err)
		}
		return s, nil
	case SpanKindChain:
		var s ChainSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode ChainSpan: %w", err)
		}
		return s, nil
	case SpanKindRetrieval:
		var s RetrievalSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode RetrievalSpan: %w", err)
		}
		return s, nil
	case SpanKindAgent:
		var s SessionSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode SessionSpan: %w", err)
		}
		return s, nil
	case SpanKindSubagent:
		var s SubagentSpan
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("agent: decode SubagentSpan: %w", err)
		}
		return s, nil
	case "":
		return nil, fmt.Errorf("agent: missing kind discriminator on span JSON")
	default:
		return nil, fmt.Errorf("agent: unknown span kind %q", head.Kind)
	}
}
