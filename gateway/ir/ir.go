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
}

// ToolCall is a model's request to invoke a named tool with JSON arguments.
// Arguments is the raw JSON argument object as the model emitted it (kept as a
// string so a streaming assembler can append fragments without re-parsing).
type ToolCall struct {
	ID        string `json:"id"`
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
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
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
	Model      string       `json:"model"`
	System     []Part       `json:"system,omitempty"`
	Messages   []Message    `json:"messages"`
	Tools      []ToolDef    `json:"tools,omitempty"`
	ToolChoice ToolChoice   `json:"toolChoice,omitempty"`
	Sampling   Sampling     `json:"sampling,omitempty"`
	Thinking   ThinkingSpec `json:"thinking,omitempty"`
	Stream     bool         `json:"stream,omitempty"`
	Metadata   Metadata     `json:"-"`
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
	Model        string       `json:"model"`
	Content      []Part       `json:"content"`
	FinishReason FinishReason `json:"finishReason"`
	Usage        Usage        `json:"usage"`
	// AppliedThinking records the thinking configuration actually sent to the
	// upstream (normalize.go), so cost/eval sees what really ran.
	AppliedThinking *AppliedThinking `json:"appliedThinking,omitempty"`
}

// StreamDelta is one incremental chunk on the hot path. Exactly the fields
// that changed are set; the rest stay zero. A terminal delta carries Finish
// (and Usage when the upstream reports it on the final chunk).
type StreamDelta struct {
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
}
