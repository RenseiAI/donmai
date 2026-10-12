package muse

// MSP wire types for the `muse serve` host (JSON-RPC 2.0 over stdio JSONL).
//
// GENERATED — do not hand-edit the shapes. This file is a hand-shaped
// projection of the allowlisted $defs members of the MSP bundle exported
// by `muse schema generate-json-schema` (stable surface); see generate.go
// for the generator, the allowlist, and the provenance stamp. Each type
// names its $defs source in the doc comment. Regenerate (re-run the
// allowlist check) after a binary upgrade; extending the wire surface
// means extending the allowlist AND the mapping code together.
//
// Provenance: `muse` 1.4.6, schema version 1, bundle fingerprint
// sha256:eae467ab60cb9a5d5af4de616830f065e59375c987fd59269c8cc33ed2a1137f.

// SchemaVersion is the MSP envelope schema version this harness requires.
// The host reports it on every initialize result (SchemaInfo.version);
// anything else fails the handshake closed.
const SchemaVersion = 1

// SchemaFingerprint is the stable-surface bundle fingerprint the types
// above were projected from. It is recorded here so a binary upgrade
// that moves the bundle is visible at review time; the handshake checks
// the version, not the fingerprint (a mismatch is a warning condition
// per the schema's own SS1.4.1, not an error).
const SchemaFingerprint = "sha256:eae467ab60cb9a5d5af4de616830f065e59375c987fd59269c8cc33ed2a1137f"

// ClientName is the initialize clientInfo.name this harness presents.
// The host requires ^[a-z0-9_]+$.
const ClientName = "donmai"

// RequestedCapabilities are the capability names this harness requests
// at handshake. sessionMcp gates inline MCP servers on session/start.
var RequestedCapabilities = []string{"sessionMcp"}

// CapabilitySessionMCP is the granted-capability name that gates inline
// MCP servers on session/start and session/resume.
const CapabilitySessionMCP = "sessionMcp"

// DefaultProviderID is the provider routing selected on session/start
// when the Spec names no model endpoint: the host's own default.
const DefaultProviderID = "meta"

// DefaultModelDocumentation is the host default model observed live on 1.4.6
// (model/list; the CLI default). The harness omits modelId rather than
// sending a "default" sentinel, so this is documentation, not a value
// ever placed on the wire.
const DefaultModelDocumentation = "muse-wonder-1.0"

// ClientInfo ($defs/ClientInfo) identifies this client inside
// initialize params. Name must match ^[a-z0-9_]+$.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// ClientCapabilities ($defs/ClientCapabilities) is the handshake
// capability posture. Absent means all defaults.
type ClientCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi,omitempty"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
	RequestedCapabilities     []string `json:"requestedCapabilities,omitempty"`
}

// InitializeParams ($defs/InitializeParams) opens the handshake.
type InitializeParams struct {
	Capabilities *ClientCapabilities `json:"capabilities,omitempty"`
	ClientInfo   ClientInfo          `json:"clientInfo"`
}

// SchemaInfo ($defs/SchemaInfo) is the envelope schema pair on the
// initialize result.
type SchemaInfo struct {
	Fingerprint string `json:"fingerprint"`
	Version     int    `json:"version"`
}

// ServerInfo is the server identification on the initialize result.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult ($defs/InitializeResult) is the handshake reply. The
// harness requires schema.version == 1 and checks grantedCapabilities
// for sessionMcp before issuing session/start with inline MCP servers.
type InitializeResult struct {
	ExperimentalAPI     bool       `json:"experimentalApi"`
	GrantedCapabilities []string   `json:"grantedCapabilities"`
	MuseHome            string     `json:"museHome"`
	PlatformFamily      string     `json:"platformFamily,omitempty"`
	PlatformOS          string     `json:"platformOs,omitempty"`
	Schema              SchemaInfo `json:"schema"`
	ServerInfo          ServerInfo `json:"serverInfo"`
	SessionDurability   string     `json:"sessionDurability,omitempty"`
	UserAgent           string     `json:"userAgent"`
}

// SessionMCPServer ($defs/SessionMcpServerConfig) is one inline MCP
// server on session/start + session/resume. Closed union on transport:
// "stdio" (command + args + env) or "streamableHttp" (url + headers).
// Mode defaults to required; this harness always sends "optional".
type SessionMCPServer struct {
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Mode      string            `json:"mode,omitempty"`
}

// SessionConfig ($defs/SessionConfig) is the open configuration
// extension object shared by session/start and session/resume. Only the
// mcpServers member is admitted today.
type SessionConfig struct {
	MCPServers map[string]SessionMCPServer `json:"mcpServers,omitempty"`
}

// SessionStartParams ($defs/SessionStartParams) opens a session.
// CommandID (UUIDv7) is required; SessionID carries the client-minted
// UUIDv7 identity for the new root session.
type SessionStartParams struct {
	SessionID      string         `json:"sessionId,omitempty"`
	CommandID      string         `json:"commandId"`
	WorkspaceRoot  string         `json:"workspaceRoot,omitempty"`
	WorkspaceRoots []string       `json:"workspaceRoots,omitempty"`
	ModelID        string         `json:"modelId,omitempty"`
	ProviderID     string         `json:"providerId,omitempty"`
	ApprovalMode   string         `json:"approvalMode,omitempty"`
	Config         *SessionConfig `json:"config,omitempty"`
}

// SessionResumeParams ($defs/SessionResumeParams) reloads a session.
// ExcludeItems returns metadata only; history pages via view/page.
type SessionResumeParams struct {
	SessionID    string         `json:"sessionId"`
	CommandID    string         `json:"commandId"`
	Config       *SessionConfig `json:"config,omitempty"`
	ExcludeItems *bool          `json:"excludeItems,omitempty"`
	Cursor       *string        `json:"cursor,omitempty"`
}

// TurnInputPart ($defs/TurnInputPart) is one ordered content part of a
// turn submission. This harness emits text parts (and image parts for
// attachments); skill/video parts are rejected by the host.
type TurnInputPart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Base64Data string `json:"base64Data,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Width      *int   `json:"width,omitempty"`
	Height     *int   `json:"height,omitempty"`
}

// TurnStartParams ($defs/TurnStartParams) submits a turn. The fresh
// turn's turnId derives from CommandID (UUIDv7).
type TurnStartParams struct {
	SessionID       string          `json:"sessionId"`
	CommandID       string          `json:"commandId"`
	Input           []TurnInputPart `json:"input"`
	DisplayText     string          `json:"displayText,omitempty"`
	WorkspaceRoots  []string        `json:"workspaceRoots,omitempty"`
	IfBusy          string          `json:"ifBusy,omitempty"`
	ReasoningEffort string          `json:"reasoningEffort,omitempty"`
}

// TurnInterruptParams ($defs/TurnInterruptParams) is the user-pressed
// stop gesture. TurnID may be omitted to target the foreground turn.
type TurnInterruptParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	TurnID    string `json:"turnId,omitempty"`
	Retract   *bool  `json:"retract,omitempty"`
}

// TurnSteerParams ($defs/TurnSteerParams) steers the running turn.
// ExpectedTurnID closes the race where the turn completes between the
// client's read and its steer.
type TurnSteerParams struct {
	SessionID       string          `json:"sessionId"`
	CommandID       string          `json:"commandId"`
	ExpectedTurnID  string          `json:"expectedTurnId"`
	Input           []TurnInputPart `json:"input"`
	ReasoningEffort string          `json:"reasoningEffort,omitempty"`
}

// ApprovalDecideParams ($defs/ApprovalDecideParams) answers an approval
// request. RequirementID must equal the approval's
// currentRequirementId (the multi-stage race guard).
type ApprovalDecideParams struct {
	SessionID     string  `json:"sessionId"`
	CommandID     string  `json:"commandId"`
	ApprovalID    string  `json:"approvalId"`
	RequirementID string  `json:"requirementId"`
	ChoiceID      string  `json:"choiceId"`
	Feedback      *string `json:"feedback,omitempty"`
}

// ViewSubscribeParams ($defs/ViewSubscribeParams) re-attaches this
// connection's live view subscription at an explicit cursor — the
// gap-free re-attachment path after view/unsubscribe, and this
// harness's view/gap recovery primitive. After, when present, replays
// (after, head] before any live event.
type ViewSubscribeParams struct {
	SessionID string  `json:"sessionId"`
	After     *string `json:"after,omitempty"`
}

// ViewPageParams ($defs/ViewPageParams) pages the session view forward
// from an exclusive cursor — the splice-fill half of gap recovery.
type ViewPageParams struct {
	SessionID string  `json:"sessionId"`
	Cursor    *string `json:"cursor,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Limit     int     `json:"limit"`
}

// ItemKind ($defs/ItemKind) is the open transcript-item vocabulary. New
// kinds are additive; clients render unknown kinds generically.
type ItemKind string

// Item kinds this harness maps. userMessage and reminderChild are
// skipped (the caller owns the input; reminders hold no turn content).
const (
	ItemKindUserMessage   ItemKind = "userMessage"
	ItemKindAgentMessage  ItemKind = "agentMessage"
	ItemKindReasoning     ItemKind = "reasoning"
	ItemKindToolCall      ItemKind = "toolCall"
	ItemKindUserShell     ItemKind = "userShell"
	ItemKindSubagent      ItemKind = "subagent"
	ItemKindWorkflow      ItemKind = "workflow"
	ItemKindReminderChild ItemKind = "reminderChild"
	ItemKindCompaction    ItemKind = "compaction"
	ItemKindHookRun       ItemKind = "hookRun"
	ItemKindSideChat      ItemKind = "sideChat"
	ItemKindWebSearch     ItemKind = "webSearch"
)

// Item ($defs/Item) is one transcript item at one revision. Only the
// members this harness reads are projected; the full object (notably
// args verbatim, output refs, child session links) stays on Raw.
type Item struct {
	ItemID         string   `json:"itemId"`
	Kind           ItemKind `json:"kind"`
	Revision       int      `json:"revision"`
	Status         string   `json:"status"`
	TurnID         string   `json:"turnId,omitempty"`
	Text           string   `json:"text,omitempty"`
	Summary        string   `json:"summary,omitempty"`
	Content        string   `json:"content,omitempty"`
	Tool           string   `json:"tool,omitempty"`
	CallID         string   `json:"callId,omitempty"`
	Args           string   `json:"args,omitempty"`
	CommandText    string   `json:"commandText,omitempty"`
	ExitCode       *int     `json:"exitCode,omitempty"`
	ChildSessionID string   `json:"childSessionId,omitempty"`
	FallbackText   string   `json:"fallbackText,omitempty"`
}

// ItemStartedParams ($defs/ItemStartedParams) opens an item. SourceRange
// is absent on ephemeral-sourced opens (delta-streamed kinds).
type ItemStartedParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor"`
	Item       Item   `json:"item"`
}

// ItemUpdatedParams ($defs/ItemUpdatedParams) re-emits an open item at
// a higher revision. Apply rule: replace iff higher.
type ItemUpdatedParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor"`
	Item       Item   `json:"item"`
}

// ItemDeltaParams ($defs/ItemDeltaParams) appends text to an open
// item's field. Ephemeral-sourced: no sourceRange. Deltas never bump
// revision. Field defaults to "text".
type ItemDeltaParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor"`
	ItemID     string `json:"itemId"`
	Field      string `json:"field,omitempty"`
	Delta      string `json:"delta"`
}

// ItemCompletedParams ($defs/ItemCompletedParams) is the authoritative
// terminal object. Clients must accept it for an itemId they never saw
// started (single-shot kinds; after a gap fill).
type ItemCompletedParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor"`
	Item       Item   `json:"item"`
}

// ViewGapParams ($defs/ViewGapParams) names a hole in push delivery:
// After is the last cursor delivered before the hole, Next the first
// cursor delivered after it. Fatal to the live view: the client must
// splice-fill (buffer live events at cursors >= next, page (after,
// next], discard overlap, splice) or re-anchor before trusting the
// transcript again.
type ViewGapParams struct {
	SessionID string `json:"sessionId"`
	After     string `json:"after"`
	Next      string `json:"next"`
}

// TurnTerminal ($defs/TurnTerminal) is the turn terminal vocabulary:
// completed, failed, cancelled (open enum on the wire).
type TurnTerminal string

// Turn terminal values (the $defs/TurnTerminal vocabulary).
const (
	TurnTerminalCompleted TurnTerminal = "completed"
	TurnTerminalFailed    TurnTerminal = "failed"
	TurnTerminalCancelled TurnTerminal = "cancelled"
)

// TurnError ($defs/TurnError) is the settled turn-failure object.
type TurnError struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// TokenUsage ($defs/TokenUsage) carries raw counters verbatim. Not
// directly summable across providers — sum counted-once derivations.
type TokenUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
}

// CumulativeCost ($defs/CumulativeCost) is the server-computed session
// cost. Partial marks at least one unpriced leg.
type CumulativeCost struct {
	USD     float64 `json:"usd"`
	Partial bool    `json:"partial"`
}

// CumulativeTokenUsage ($defs/CumulativeTokenUsage) folds the session
// running totals. Accumulate-only (except cost, which tracks price
// re-adoption and is labeled estimate).
type CumulativeTokenUsage struct {
	PromptTokens     int64           `json:"promptTokens"`
	OutputTokens     int64           `json:"outputTokens"`
	TotalTokens      int64           `json:"totalTokens"`
	CacheReadTokens  int64           `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64           `json:"cacheWriteTokens,omitempty"`
	Cost             *CumulativeCost `json:"cost,omitempty"`
}

// SessionTokenUsageParams ($defs/SessionTokenUsageParams) arrives per
// model completion: raw counters plus counted-once derivations plus
// the session cumulative block.
type SessionTokenUsageParams struct {
	SessionID    string               `json:"sessionId"`
	ViewCursor   string               `json:"viewCursor"`
	TurnID       string               `json:"turnId,omitempty"`
	ModelID      *string              `json:"modelId"`
	Usage        TokenUsage           `json:"usage"`
	PromptTokens int64                `json:"promptTokens"`
	TotalTokens  int64                `json:"totalTokens"`
	Cumulative   CumulativeTokenUsage `json:"cumulative"`
}

// SessionContextUsageParams ($defs/SessionContextUsageParams) is the
// counted-once context-window occupancy. Replace wholesale; emitted
// only when the (windowTokens, usedTokens, pressure) triple changes.
type SessionContextUsageParams struct {
	SessionID    string `json:"sessionId"`
	ViewCursor   string `json:"viewCursor"`
	WindowTokens *int64 `json:"windowTokens,omitempty"`
	UsedTokens   int64  `json:"usedTokens"`
	Pressure     string `json:"pressure,omitempty"`
}

// TurnStartedParams ($defs/TurnStartedParams) marks a foreground turn
// beginning to run. turnId == commandId for fresh turns.
type TurnStartedParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor"`
	TurnID     string `json:"turnId"`
	CommandID  string `json:"commandId,omitempty"`
}

// TurnForegroundCompletedParams ($defs/TurnForegroundCompletedParams)
// marks foreground work done while background reminders hold the turn
// open. Explicitly non-terminal.
type TurnForegroundCompletedParams struct {
	SessionID      string   `json:"sessionId"`
	ViewCursor     string   `json:"viewCursor"`
	TurnID         string   `json:"turnId"`
	BlockingAgents []string `json:"blockingAgents,omitempty"`
}

// TurnCompletedParams ($defs/TurnCompletedParams) is the run's durable
// terminal record. Usage, when present, overrides the folded per-step
// usage for the terminal ResultEvent.
type TurnCompletedParams struct {
	SessionID  string       `json:"sessionId"`
	ViewCursor string       `json:"viewCursor"`
	TurnID     string       `json:"turnId"`
	Terminal   TurnTerminal `json:"terminal"`
	Error      *TurnError   `json:"error,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Usage      *TokenUsage  `json:"usage,omitempty"`
	DurationMs *int64       `json:"durationMs,omitempty"`
}

// SessionStartedParams ($defs/SessionStartedParams) broadcasts a newly
// loaded session. Never sent for session/resume.
type SessionStartedParams struct {
	Session SessionSummary `json:"session"`
}

// SessionSummary is the session object projection this harness reads
// (session/start result, session/started broadcast, session/resume
// result). Unknown fields are ignored per additive-optional evolution.
type SessionSummary struct {
	SessionID    string  `json:"sessionId"`
	Status       string  `json:"status,omitempty"`
	ActiveTurnID *string `json:"activeTurnId"`
	ModelID      string  `json:"modelId,omitempty"`
	ProviderID   string  `json:"providerId,omitempty"`
	TurnCount    *int    `json:"turnCount,omitempty"`
}

// SessionStatusChangedParams ($defs/SessionStatusChangedParams) is the
// command-plane (status, attention) broadcast. Never gated on the view
// subscription set.
type SessionStatusChangedParams struct {
	SessionID  string   `json:"sessionId"`
	Status     string   `json:"status"`
	Attention  []string `json:"attention,omitempty"`
	ViewCursor *string  `json:"viewCursor"`
}

// SessionTodoItem is one row of the wholesale todo list.
type SessionTodoItem struct {
	Text   string `json:"text,omitempty"`
	Status string `json:"status,omitempty"`
}

// SessionTodoListChangedParams ($defs/SessionTodoListChangedParams)
// replaces the whole todo list; empty items clears it.
type SessionTodoListChangedParams struct {
	SessionID  string            `json:"sessionId"`
	ViewCursor string            `json:"viewCursor"`
	Items      []SessionTodoItem `json:"items,omitempty"`
	SourceTool string            `json:"sourceTool,omitempty"`
}

// ApprovalChoice ($defs/ApprovalChoice) is one presented decision arm.
type ApprovalChoice struct {
	ChoiceID        string `json:"choiceId"`
	Decision        string `json:"decision"`
	Label           string `json:"label"`
	Scope           string `json:"scope,omitempty"`
	AcceptsFeedback bool   `json:"acceptsFeedback,omitempty"`
}

// ApprovalSubject ($defs/ApprovalSubject) is the open approval subject
// union. Unknown kinds render generically and are never auto-approved.
type ApprovalSubject struct {
	Kind          string `json:"kind"`
	Command       string `json:"command,omitempty"`
	Path          string `json:"path,omitempty"`
	ToolName      string `json:"toolName,omitempty"`
	Target        string `json:"target,omitempty"`
	Access        string `json:"access,omitempty"`
	Host          string `json:"host,omitempty"`
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
}

// ApprovalRequestParams ($defs/ApprovalRequestParams) is shared by the
// approval/request server-request and the approval/requested
// notification: the fold of the durable Requested record.
type ApprovalRequestParams struct {
	SessionID            string           `json:"sessionId"`
	ViewCursor           string           `json:"viewCursor,omitempty"`
	ApprovalID           string           `json:"approvalId"`
	CurrentRequirementID string           `json:"currentRequirementId"`
	AvailableChoices     []ApprovalChoice `json:"availableChoices"`
	Subject              ApprovalSubject  `json:"subject"`
	ToolName             string           `json:"toolName,omitempty"`
	ToolCallID           string           `json:"toolCallId,omitempty"`
	ItemID               string           `json:"itemId,omitempty"`
	TurnID               string           `json:"turnId,omitempty"`
	RawArgs              string           `json:"rawArgs,omitempty"`
}

// ApprovalResolvedParams ($defs/ApprovalResolvedParams) is the first
// durable terminal decision. The harness answers the decision at
// approval/decide time; this broadcast confirms it.
type ApprovalResolvedParams struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor,omitempty"`
	ApprovalID string `json:"approvalId"`
	Decision   string `json:"decision"`
	ItemID     string `json:"itemId,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
}

// ErrorKind ($defs/ErrorKind) is the stable error.data.kind category
// clients branch on. Session-gone kinds drive resume classification.
type ErrorKind string

// Session-gone error kinds: the host's stable error.data.kind values
// that mean the session is gone (resume must surface ErrSessionNotFound).
const (
	ErrorKindSessionNotFound ErrorKind = "sessionNotFound"
	ErrorKindNotFound        ErrorKind = "notFound"
)

// MSPErrorData ($defs/ErrorData) is the structured error detail. Kind,
// when present, is what clients branch on.
type MSPErrorData struct {
	Kind      ErrorKind `json:"kind,omitempty"`
	CommandID string    `json:"commandId,omitempty"`
}

// MSPError ($defs/ErrorObject) is the error member of an error
// response.
type MSPError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *MSPErrorData `json:"data,omitempty"`
}
