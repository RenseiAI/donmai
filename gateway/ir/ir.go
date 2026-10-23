// Package ir is the gateway's canonical intermediate representation. Every
// inbound wire surface DECODES into it and every outbound upstream ENCODES out
// of it, so translation is N-to-N through one hub (2N codecs, never N²).
//
// The types are deliberately protocol-neutral: they carry what all of
// OpenAI-chat, Anthropic-messages, and Gemini-generate can express (typed
// message parts, tool calls, streaming deltas, canonical finish reasons,
// usage, and a normalized thinking spec — see normalize.go). Surface- and
// upstream-specific quirks live in the codecs (translate/, upstream/), never
// here.
//
// PURELY DATA. No network, no provider SDK. Everything is JSON-tagged
// camelCase for auditability of any serialized fixture.
package ir

// Role names the author of a message in the canonical conversation.
type Role string

// Role constants — the canonical set every surface maps onto.
const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind discriminates a typed message part.
type PartKind string

// PartKind constants name each typed content part a message may carry.
const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartToolCall   PartKind = "tool_call"   // an assistant's request to call a tool
	PartToolResult PartKind = "tool_result" // a tool role's result for a prior call
	PartThinking   PartKind = "thinking"    // reasoning/thinking content
)

// Part is one typed piece of a message. Only the fields relevant to Kind are
// populated; the rest stay zero (omitempty keeps serialized fixtures tight).
type Part struct {
	Kind PartKind `json:"kind"`
	// Text carries PartText / PartThinking content and the human-readable
	// portion of a PartToolResult.
	Text string `json:"text,omitempty"`
	// ImageURL carries a PartImage source (data: URI or https URL). The
	// gateway does not fetch it; it re-encodes the reference per surface.
	ImageURL string `json:"imageUrl,omitempty"`
	// ToolCall is set for PartToolCall.
	ToolCall *ToolCall `json:"toolCall,omitempty"`
	// ToolCallID links a PartToolResult back to the PartToolCall it answers.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Opaque encrypted reasoning is never treated as readable text.
	EncryptedContent string        `json:"encryptedContent,omitempty"`
	Annotations      []URLCitation `json:"annotations,omitempty"`
}

// ToolCall is a model's request to invoke a named tool with JSON arguments.
// Arguments is the raw JSON argument object as the model emitted it (kept as a
// string so a streaming assembler can append fragments without re-parsing).
type ToolCall struct {
	ID        string `json:"id"`
	ItemID    string `json:"itemId,omitempty"`
	Kind      string `json:"kind,omitempty"` // function or custom
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one turn in the canonical conversation.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// ToolDef declares a callable tool. Parameters is a JSON Schema object.
type ToolDef struct {
	Kind         string           `json:"kind,omitempty"` // function or custom
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	Parameters   map[string]any   `json:"parameters,omitempty"`
	Format       map[string]any   `json:"format,omitempty"`
	Strict       *bool            `json:"strict,omitempty"`
	DeferLoading *bool            `json:"deferLoading,omitempty"`
	Tools        []ToolDef        `json:"tools,omitempty"`
	WebSearch    *WebSearchConfig `json:"webSearch,omitempty"`
}

// Item is a typed Responses conversation item. Item identity and call_id are
// distinct: the latter joins tool outputs to calls across HTTP requests.
type Item struct {
	Kind             string           `json:"kind"`
	ID               string           `json:"id,omitempty"`
	Role             Role             `json:"role,omitempty"`
	Phase            string           `json:"phase,omitempty"`
	Status           string           `json:"status,omitempty"`
	Namespace        string           `json:"namespace,omitempty"`
	Parts            []Part           `json:"parts,omitempty"`
	CallID           string           `json:"callId,omitempty"`
	Name             string           `json:"name,omitempty"`
	Arguments        string           `json:"arguments,omitempty"`
	Input            string           `json:"input,omitempty"`
	Output           string           `json:"output,omitempty"`
	Summary          []string         `json:"summary,omitempty"`
	EncryptedContent string           `json:"encryptedContent,omitempty"`
	WebSearchAction  *WebSearchAction `json:"webSearchAction,omitempty"`
}

// WebSearchConfig is the native provider's search tool configuration. The
// gateway preserves these values; the selected provider executes searches.
type WebSearchConfig struct {
	ExternalWebAccess  *bool                  `json:"externalWebAccess,omitempty"`
	IndexedWebAccess   *bool                  `json:"indexedWebAccess,omitempty"`
	Filters            *WebSearchFilters      `json:"filters,omitempty"`
	UserLocation       *WebSearchUserLocation `json:"userLocation,omitempty"`
	SearchContextSize  *string                `json:"searchContextSize,omitempty"`
	SearchContentTypes []string               `json:"searchContentTypes,omitempty"`
}

// WebSearchFilters limits the provider's search domains.
type WebSearchFilters struct {
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// WebSearchUserLocation carries the provider's approximate geographic context.
type WebSearchUserLocation struct {
	Type     string  `json:"type"`
	Country  *string `json:"country,omitempty"`
	Region   *string `json:"region,omitempty"`
	City     *string `json:"city,omitempty"`
	Timezone *string `json:"timezone,omitempty"`
}

// WebSearchAction records provider-executed search/page actions as data only.
// Kind determines which fields are meaningful; codecs reject cross-kind data.
type WebSearchAction struct {
	Kind    string   `json:"kind"`
	Query   *string  `json:"query,omitempty"`
	Queries []string `json:"queries,omitempty"`
	URL     *string  `json:"url,omitempty"`
	Pattern *string  `json:"pattern,omitempty"`
}

// URLCitation retains source attribution and text offsets from native output.
// URLs are data; the gateway never fetches them.
type URLCitation struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
}

// ResponsesControls carry request semantics that have no Chat equivalent.
// They are typed IR data, never an opaque raw-body pass-through.
type ResponsesControls struct {
	// ParallelToolCalls is nil when the client left the protocol default
	// (parallel) in place, so an absent field stays distinct from serial.
	ParallelToolCalls *bool             `json:"parallelToolCalls,omitempty"`
	Store             bool              `json:"store"`
	Include           []string          `json:"include,omitempty"`
	Reasoning         map[string]string `json:"reasoning,omitempty"`
	Text              map[string]any    `json:"text,omitempty"`
	ServiceTier       string            `json:"serviceTier,omitempty"`
	PromptCacheKey    string            `json:"promptCacheKey,omitempty"`
	ClientMetadata    map[string]string `json:"clientMetadata,omitempty"`
}

// Sampling holds the canonical decoding parameters. Pointers so "unset" is
// distinguishable from a deliberate zero, and only set fields are forwarded to
// an upstream (an upstream that lacks a knob simply drops it).
type Sampling struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// ToolChoice names the canonical tool-selection modes.
type ToolChoice string

// ToolChoice constants — the canonical set.
const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceNone     ToolChoice = "none"
	ToolChoiceRequired ToolChoice = "required"
)

// Request is the canonical request. Metadata carries the session token →
// dispatch identity used for cost attribution; it never travels to an upstream.
type Request struct {
	Model        string             `json:"model"`
	Instructions string             `json:"instructions,omitempty"`
	Items        []Item             `json:"items,omitempty"`
	Responses    *ResponsesControls `json:"responses,omitempty"`
	System       []Part             `json:"system,omitempty"`
	Messages     []Message          `json:"messages"`
	Tools        []ToolDef          `json:"tools,omitempty"`
	ToolChoice   ToolChoice         `json:"toolChoice,omitempty"`
	Sampling     Sampling           `json:"sampling,omitempty"`
	Thinking     ThinkingSpec       `json:"thinking,omitempty"`
	Stream       bool               `json:"stream,omitempty"`
	Metadata     Metadata           `json:"-"`
}

// Metadata is out-of-band request context for attribution — never serialized
// onto the wire to an upstream.
type Metadata struct {
	SessionID  string
	DispatchID string
	Harness    string
}

// FinishReason is the canonical completion reason. Every surface and upstream
// maps its native value onto exactly one of these.
type FinishReason string

// FinishReason constants — the canonical enum (08 §4).
const (
	FinishStop          FinishReason = "stop"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
)

// Usage is the token accounting an upstream reports. Cache fields are populated
// only where the upstream reports them.
type Usage struct {
	TokensIn         int `json:"tokensIn"`
	TokensOut        int `json:"tokensOut"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// Response is a fully-assembled non-streaming result.
type Response struct {
	ID            string           `json:"id,omitempty"`
	Details       *ResponseDetails `json:"details,omitempty"`
	Items         []Item           `json:"items,omitempty"`
	Model         string           `json:"model"`
	Content       []Part           `json:"content"`
	FinishReason  FinishReason     `json:"finishReason"`
	Usage         Usage            `json:"usage"`
	UsageReported bool             `json:"usageReported,omitempty"`
	// AppliedThinking records the thinking configuration actually sent to the
	// upstream (normalize.go), so cost/eval sees what really ran.
	AppliedThinking *AppliedThinking `json:"appliedThinking,omitempty"`
}

// ResponseDetails preserves the supported Responses envelope alongside canonical
// items and usage. Stateful/background responses are refused by the codec.
type ResponseDetails struct {
	Object            string            `json:"object,omitempty"`
	Status            string            `json:"status,omitempty"`
	CreatedAt         *int64            `json:"createdAt,omitempty"`
	Instructions      *string           `json:"instructions,omitempty"`
	ParallelToolCalls *bool             `json:"parallelToolCalls,omitempty"`
	ToolChoice        *string           `json:"toolChoice,omitempty"`
	Tools             []ToolDef         `json:"tools,omitempty"`
	Reasoning         map[string]string `json:"reasoning,omitempty"`
	Text              map[string]any    `json:"text,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ServiceTier       *string           `json:"serviceTier,omitempty"`
	PromptCacheKey    *string           `json:"promptCacheKey,omitempty"`
	Store             *bool             `json:"store,omitempty"`
	Background        *bool             `json:"background,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"topP,omitempty"`
	MaxOutputTokens   *int              `json:"maxOutputTokens,omitempty"`
	Truncation        *string           `json:"truncation,omitempty"`
}

// StreamDelta is one incremental chunk on the hot path. Exactly the fields
// that changed are set; the rest stay zero. A terminal delta carries Finish
// (and Usage when the upstream reports it on the final chunk).
type StreamDelta struct {
	Responses *ResponsesStreamEvent `json:"responses,omitempty"`
	// TextDelta is appended assistant text.
	TextDelta string `json:"textDelta,omitempty"`
	// ThinkingDelta is appended reasoning/thinking text.
	ThinkingDelta string `json:"thinkingDelta,omitempty"`
	// ToolCallDelta carries a tool-call fragment (id/name on first, argument
	// fragments after).
	ToolCallDelta *ToolCallDelta `json:"toolCallDelta,omitempty"`
	// Finish is non-empty only on the terminal delta.
	Finish FinishReason `json:"finish,omitempty"`
	// Usage is set on the terminal delta when the upstream reports it.
	Usage *Usage `json:"usage,omitempty"`
}

// ResponsesStreamEvent is one canonical item lifecycle or terminal event.
// The surface encodes it as SSE; upstream decoders never pass raw JSON through.
type ResponsesStreamEvent struct {
	Kind            string       `json:"kind"`
	SequenceNumber  *int         `json:"sequenceNumber,omitempty"`
	Response        *Response    `json:"response,omitempty"`
	ResponseID      string       `json:"responseId,omitempty"`
	ItemID          string       `json:"itemId,omitempty"`
	OutputIndex     int          `json:"outputIndex,omitempty"`
	ContentIndex    int          `json:"contentIndex,omitempty"`
	SummaryIndex    int          `json:"summaryIndex,omitempty"`
	AnnotationIndex int          `json:"annotationIndex,omitempty"`
	Annotation      *URLCitation `json:"annotation,omitempty"`
	Item            *Item        `json:"item,omitempty"`
	Part            *Part        `json:"part,omitempty"`
	Delta           string       `json:"delta,omitempty"`
	Usage           *Usage       `json:"usage,omitempty"`
	EndTurn         *bool        `json:"endTurn,omitempty"`
}

// ToolCallDelta is a streamed fragment of a tool call. Index disambiguates
// parallel tool calls; ID/Name arrive on the first fragment, ArgsDelta after.
type ToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	ArgsDelta string `json:"argsDelta,omitempty"`
}

// Stream is the upstream's incremental output. Deltas is closed by the
// producer when the exchange completes; Err, if set after the channel closes,
// carries a mid-stream transport failure. A non-streaming upstream returns a
// Stream that yields a single assembled delta then closes.
type Stream struct {
	Deltas <-chan StreamDelta
	// Err returns any terminal error observed after Deltas closed. Safe to
	// call only after the Deltas channel is drained.
	Err func() error
	// RefusedUsage returns the usage the upstream reported for a turn it
	// finished after the reader refused the stream, so a billed turn is
	// metered even though the client is served a failure. Nil (or a nil
	// result) when the upstream reported none. Same calling rule as Err.
	RefusedUsage func() *Usage
}
