package muse

// Wire types for the `muse serve` MSP protocol (JSON-RPC 2.0 over stdio
// JSONL), GENERATED from the host binary's own `muse schema` export —
// never hand-copied. Regenerate with:
//
//	muse schema generate-json-schema --out /tmp/muse-schema-gen
//	# then reconcile the $defs below against msp.schema.json
//
// Source: Muse Code 1.4.14 msp.schema.json, schemaVersion 1, stable surface. The
// fingerprint of the exact export these types were generated from is
// pinned in schema_fingerprint.go.
//
// Conventions:
//   - Structs project $defs; opaque nested additions use json.RawMessage.
//     Field names follow the schema's
//     camelCase wire spelling via json tags.
//   - Open enums are plain strings; closed enums get a typed string +
//     constants so the compiler catches typos on the request path.
//   - Optional (non-required) members are pointers/omitempty so we never
//     send a zero value the host would read as an explicit selection.

import "encoding/json"

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

// ClientInfo is $defs/ClientInfo: client identification in initialize.
// Name must match ^[a-z0-9_]+$.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities is $defs/ClientCapabilities.
type ClientCapabilities struct {
	ExperimentalAPI           *bool    `json:"experimentalApi,omitempty"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
	RequestedCapabilities     []string `json:"requestedCapabilities,omitempty"`
	UserInputDialogs          *bool    `json:"userInputDialogs,omitempty"`
}

// CapabilityName is $defs/CapabilityName (open enum).
type CapabilityName string

// Grantable capability names.
const (
	CapabilityUserShell         CapabilityName = "userShell"
	CapabilitySessionMCP        CapabilityName = "sessionMcp"
	CapabilitySessionListStream CapabilityName = "sessionListStream"
	CapabilityFeedback          CapabilityName = "feedback"
	CapabilityComputerUseSetup  CapabilityName = "computerUseSetup"
)

// InitializeParams is $defs/InitializeParams.
type InitializeParams struct {
	Capabilities *ClientCapabilities `json:"capabilities,omitempty"`
	ClientInfo   ClientInfo          `json:"clientInfo"`
}

// SchemaInfo is $defs/SchemaInfo: the envelope schema version pair.
type SchemaInfo struct {
	Fingerprint string `json:"fingerprint"`
	Version     int    `json:"version"`
}

// ServerInfo is $defs/ServerInfo.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SessionDurability is $defs/SessionDurability.
type SessionDurability string

const (
	// SessionDurabilityDurable is the MSP wire value.
	SessionDurabilityDurable SessionDurability = "durable"
	// SessionDurabilityEphemeral is the MSP wire value.
	SessionDurabilityEphemeral SessionDurability = "ephemeral"
)

// InitializeResult is $defs/InitializeResult.
type InitializeResult struct {
	ExperimentalAPI          bool                      `json:"experimentalApi"`
	GrantedCapabilities      []CapabilityName          `json:"grantedCapabilities"`
	MuseHome                 string                    `json:"museHome"`
	PlatformFamily           string                    `json:"platformFamily"`
	PlatformOS               string                    `json:"platformOs"`
	Schema                   SchemaInfo                `json:"schema"`
	ServerInfo               ServerInfo                `json:"serverInfo"`
	UserAgent                string                    `json:"userAgent"`
	SessionDurability        *SessionDurability        `json:"sessionDurability,omitempty"`
	SandboxAvailability      *SandboxAvailability      `json:"sandboxAvailability,omitempty"`
	SandboxUnavailableReason *SandboxUnavailableReason `json:"sandboxUnavailableReason,omitempty"`
}

// ---------------------------------------------------------------------------
// Closed enums used on the request path
// ---------------------------------------------------------------------------

// ReasoningEffort is $defs/ReasoningEffort (closed).
type ReasoningEffort string

const (
	// ReasoningEffortNone is the MSP wire value.
	ReasoningEffortNone ReasoningEffort = "none"
	// ReasoningEffortMinimal is the MSP wire value.
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	// ReasoningEffortLow is the MSP wire value.
	ReasoningEffortLow ReasoningEffort = "low"
	// ReasoningEffortMedium is the MSP wire value.
	ReasoningEffortMedium ReasoningEffort = "medium"
	// ReasoningEffortHigh is the MSP wire value.
	ReasoningEffortHigh ReasoningEffort = "high"
	// ReasoningEffortXHigh is the MSP wire value.
	ReasoningEffortXHigh ReasoningEffort = "xhigh"
	// ReasoningEffortMax is the MSP wire value.
	ReasoningEffortMax ReasoningEffort = "max"
	// ReasoningEffortUltra is the MSP wire value.
	ReasoningEffortUltra ReasoningEffort = "ultra"
)

// ApprovalMode is $defs/ApprovalMode (closed, select-never-create).
type ApprovalMode string

const (
	// ApprovalModeAllowAll is the MSP wire value.
	ApprovalModeAllowAll ApprovalMode = "allowAll"
	// ApprovalModePromptUnmatched is the MSP wire value.
	ApprovalModePromptUnmatched ApprovalMode = "promptUnmatched"
	// ApprovalModeOnRequest is the MSP wire value.
	ApprovalModeOnRequest ApprovalMode = "onRequest"
	// ApprovalModeDenyUnmatched is the MSP wire value.
	ApprovalModeDenyUnmatched ApprovalMode = "denyUnmatched"
)

// IfBusy is $defs/IfBusy (closed).
type IfBusy string

const (
	// IfBusyQueue is the MSP wire value.
	IfBusyQueue IfBusy = "queue"
	// IfBusySteer is the MSP wire value.
	IfBusySteer IfBusy = "steer"
	// IfBusyReplace is the MSP wire value.
	IfBusyReplace IfBusy = "replace"
)

// TurnTerminal is $defs/TurnTerminal (wire-open; the runtime's vocabulary).
type TurnTerminal string

const (
	// TurnTerminalCompleted is the MSP wire value.
	TurnTerminalCompleted TurnTerminal = "completed"
	// TurnTerminalFailed is the MSP wire value.
	TurnTerminalFailed TurnTerminal = "failed"
	// TurnTerminalCancelled is the MSP wire value.
	TurnTerminalCancelled TurnTerminal = "cancelled"
)

// TurnStartDisposition is $defs/TurnStartDisposition (open).
type TurnStartDisposition string

const (
	// TurnDispositionStarted is the MSP wire value.
	TurnDispositionStarted TurnStartDisposition = "started"
	// TurnDispositionQueued is the MSP wire value.
	TurnDispositionQueued TurnStartDisposition = "queued"
	// TurnDispositionSteered is the MSP wire value.
	TurnDispositionSteered TurnStartDisposition = "steered"
)

// CommandStatus is $defs/CommandStatus.
type CommandStatus string

const (
	// CommandStatusAccepted is the MSP wire value.
	CommandStatusAccepted CommandStatus = "accepted"
)

// ItemKind is $defs/ItemKind (open; render unknown kinds generically).
type ItemKind string

const (
	// ItemKindUserMessage is the MSP wire value.
	ItemKindUserMessage ItemKind = "userMessage"
	// ItemKindAgentMessage is the MSP wire value.
	ItemKindAgentMessage ItemKind = "agentMessage"
	// ItemKindReasoning is the MSP wire value.
	ItemKindReasoning ItemKind = "reasoning"
	// ItemKindToolCall is the MSP wire value.
	ItemKindToolCall ItemKind = "toolCall"
	// ItemKindUserShell is the MSP wire value.
	ItemKindUserShell ItemKind = "userShell"
	// ItemKindSubagent is the MSP wire value.
	ItemKindSubagent ItemKind = "subagent"
	// ItemKindWorkflow is the MSP wire value.
	ItemKindWorkflow ItemKind = "workflow"
	// ItemKindReminderChild is the MSP wire value.
	ItemKindReminderChild ItemKind = "reminderChild"
	// ItemKindCompaction is the MSP wire value.
	ItemKindCompaction ItemKind = "compaction"
	// ItemKindHookRun is the MSP wire value.
	ItemKindHookRun ItemKind = "hookRun"
	// ItemKindSideChat is the MSP wire value.
	ItemKindSideChat ItemKind = "sideChat"
	// ItemKindWebSearch is the MSP wire value.
	ItemKindWebSearch ItemKind = "webSearch"
)

// ItemStatus is $defs/ItemStatus (open; terminal = anything but inProgress).
type ItemStatus string

const (
	// ItemStatusInProgress is the MSP wire value.
	ItemStatusInProgress ItemStatus = "inProgress"
	// ItemStatusCompleted is the MSP wire value.
	ItemStatusCompleted ItemStatus = "completed"
	// ItemStatusFailed is the MSP wire value.
	ItemStatusFailed ItemStatus = "failed"
	// ItemStatusCancelled is the MSP wire value.
	ItemStatusCancelled ItemStatus = "cancelled"
	// ItemStatusRejected is the MSP wire value.
	ItemStatusRejected ItemStatus = "rejected"
	// ItemStatusTimedOut is the MSP wire value.
	ItemStatusTimedOut ItemStatus = "timedOut"
)

// ErrorKind is $defs/ErrorKind (open; the value clients branch on).
type ErrorKind string

const (
	// ErrorKindSessionNotFound is the MSP wire value.
	ErrorKindSessionNotFound ErrorKind = "sessionNotFound"
	// ErrorKindNotFound is the MSP wire value.
	ErrorKindNotFound ErrorKind = "notFound"
)

// MSPApprovalDecision is $defs/ApprovalDecision (open): the host-side
// decision vocabulary carried on choices and resolutions. Distinct from
// the bridge's ApprovalAction (allow/decline) in approval.go.
type MSPApprovalDecision string

const (
	// MSPApprovalApproved is the MSP wire value.
	MSPApprovalApproved MSPApprovalDecision = "approved"
	// MSPApprovalApprovedForSession is the MSP wire value.
	MSPApprovalApprovedForSession MSPApprovalDecision = "approvedForSession"
	// MSPApprovalApprovedPolicyAmendment is the MSP wire value.
	MSPApprovalApprovedPolicyAmendment MSPApprovalDecision = "approvedPolicyAmendment"
	// MSPApprovalDenied is the MSP wire value.
	MSPApprovalDenied MSPApprovalDecision = "denied"
	// MSPApprovalDeniedPolicyAmendment is the MSP wire value.
	MSPApprovalDeniedPolicyAmendment MSPApprovalDecision = "deniedPolicyAmendment"
	// MSPApprovalTimedOut is the MSP wire value.
	MSPApprovalTimedOut MSPApprovalDecision = "timedOut"
	// MSPApprovalAbort is the MSP wire value.
	MSPApprovalAbort MSPApprovalDecision = "abort"
)

// SessionStatus is $defs/SessionStatus (open).
type SessionStatus string

const (
	// SessionStatusNotLoaded is the MSP wire value.
	SessionStatusNotLoaded SessionStatus = "notLoaded"
	// SessionStatusIdle is the MSP wire value.
	SessionStatusIdle SessionStatus = "idle"
	// SessionStatusRunning is the MSP wire value.
	SessionStatusRunning SessionStatus = "running"
)

// HistoryPreference is $defs/HistoryPreference (closed).
type HistoryPreference string

const (
	// HistoryPreferenceAuto is the MSP wire value.
	HistoryPreferenceAuto HistoryPreference = "auto"
	// HistoryPreferenceInline is the MSP wire value.
	HistoryPreferenceInline HistoryPreference = "inline"
	// HistoryPreferenceSnapshot is the MSP wire value.
	HistoryPreferenceSnapshot HistoryPreference = "snapshot"
	// HistoryPreferenceAnchored is the MSP wire value.
	HistoryPreferenceAnchored HistoryPreference = "anchored"
)

// ---------------------------------------------------------------------------
// Session lifecycle
// ---------------------------------------------------------------------------

// SessionMCPServerMode is $defs/SessionMcpServerMode.
type SessionMCPServerMode string

const (
	// SessionMCPServerModeRequired is the MSP wire value.
	SessionMCPServerModeRequired SessionMCPServerMode = "required"
	// SessionMCPServerModeOptional is the MSP wire value.
	SessionMCPServerModeOptional SessionMCPServerMode = "optional"
)

// SessionMCPStdioFraming is $defs/SessionMcpStdioFraming.
type SessionMCPStdioFraming string

// SessionMCPServerConfig is $defs/SessionMcpServerConfig: one native MCP
// server supplied at session construction. Closed union on transport.
type SessionMCPServerConfig struct {
	Transport string `json:"transport"`
	// stdio arm.
	Command string                  `json:"command,omitempty"`
	Args    []string                `json:"args,omitempty"`
	Env     map[string]string       `json:"env,omitempty"`
	Framing *SessionMCPStdioFraming `json:"framing,omitempty"`
	// streamableHttp arm.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// both arms.
	Mode *SessionMCPServerMode `json:"mode,omitempty"`
}

// SessionConfig is $defs/SessionConfig: the open construction-time
// extension object. Only mcpServers is admitted today.
type SessionConfig struct {
	MCPServers map[string]SessionMCPServerConfig `json:"mcpServers,omitempty"`
}

// SessionStartParams is $defs/SessionStartParams.
type SessionStartParams struct {
	ApprovalMode      *ApprovalMode   `json:"approvalMode,omitempty"`
	CommandID         string          `json:"commandId"`
	Config            *SessionConfig  `json:"config,omitempty"`
	ModelID           *string         `json:"modelId,omitempty"`
	PermissionProfile *string         `json:"permissionProfile,omitempty"`
	ProviderID        *string         `json:"providerId,omitempty"`
	SessionID         *string         `json:"sessionId,omitempty"`
	WorkspaceRoot     *string         `json:"workspaceRoot,omitempty"`
	WorkspaceRoots    []string        `json:"workspaceRoots,omitempty"`
	PreviousSessionID *string         `json:"previousSessionId,omitempty"`
	RecoveryMode      *RecoveryMode   `json:"recoveryMode,omitempty"`
	SpawnedBy         json.RawMessage `json:"spawnedBy,omitempty"`
}

// Session is $defs/Session (subset of stable members we read).
type Session struct {
	SessionID       string             `json:"sessionId"`
	Status          SessionStatus      `json:"status"`
	ActiveTurnID    *string            `json:"activeTurnId"`
	ModelID         *string            `json:"modelId,omitempty"`
	ApprovalMode    json.RawMessage    `json:"approvalMode,omitempty"`
	Attention       json.RawMessage    `json:"attention,omitempty"`
	Branch          *string            `json:"branch,omitempty"`
	CreatedAt       string             `json:"createdAt"`
	CreatedByClient json.RawMessage    `json:"createdByClient,omitempty"`
	DriveHolder     json.RawMessage    `json:"driveHolder,omitempty"`
	FirstUserPrompt *string            `json:"firstUserPrompt,omitempty"`
	ForkedFrom      json.RawMessage    `json:"forkedFrom"`
	GeneratedName   *string            `json:"generatedName,omitempty"`
	ItemCount       *int64             `json:"itemCount,omitempty"`
	Kind            *SessionKind       `json:"kind,omitempty"`
	LastActivityAt  *string            `json:"lastActivityAt,omitempty"`
	LogBytes        *int64             `json:"logBytes,omitempty"`
	Name            *string            `json:"name,omitempty"`
	NameOrigin      *SessionNameOrigin `json:"nameOrigin,omitempty"`
	ParentSessionID *string            `json:"parentSessionId,omitempty"`
	Path            string             `json:"path"`
	ProviderID      json.RawMessage    `json:"providerId"`
	ResumedByClient json.RawMessage    `json:"resumedByClient,omitempty"`
	SideFrom        json.RawMessage    `json:"sideFrom,omitempty"`
	SpawnedBy       json.RawMessage    `json:"spawnedBy,omitempty"`
	Title           *string            `json:"title,omitempty"`
	TurnCount       int64              `json:"turnCount"`
	UpdatedAt       string             `json:"updatedAt"`
	WorkspaceRoot   json.RawMessage    `json:"workspaceRoot"`
}

// SessionStartResult is $defs/SessionStartResult.
type SessionStartResult struct {
	Session    Session `json:"session"`
	ViewCursor string  `json:"viewCursor"`
}

// SessionResumeParams is $defs/SessionResumeParams.
type SessionResumeParams struct {
	CommandID         string             `json:"commandId"`
	Config            *SessionConfig     `json:"config,omitempty"`
	Cursor            *string            `json:"cursor,omitempty"`
	ExcludeItems      *bool              `json:"excludeItems,omitempty"`
	History           *HistoryPreference `json:"history,omitempty"`
	SessionID         string             `json:"sessionId"`
	ModelID           *string            `json:"modelId,omitempty"`
	PermissionProfile *string            `json:"permissionProfile,omitempty"`
	RecoveryMode      *RecoveryMode      `json:"recoveryMode,omitempty"`
}

// SessionResumeResult is $defs/SessionResumeResult (subset).
type SessionResumeResult struct {
	Session         Session           `json:"session"`
	ViewCursor      string            `json:"viewCursor"`
	PendingRequests []json.RawMessage `json:"pendingRequests"`
	LastTurn        *LastTurn         `json:"lastTurn,omitempty"`
	BlockedPrompt   json.RawMessage   `json:"blockedPrompt,omitempty"`
	History         json.RawMessage   `json:"history"`
}

// LastTurn is $defs/LastTurn.
type LastTurn struct {
	Terminal TurnTerminal `json:"terminal"`
	TurnID   string       `json:"turnId"`
	Error    *TurnError   `json:"error,omitempty"`
	Reason   *string      `json:"reason,omitempty"`
}

// TurnError is $defs/TurnError.
type TurnError struct {
	Kind          string          `json:"kind"`
	Message       string          `json:"message"`
	Retryable     bool            `json:"retryable"`
	Quota         json.RawMessage `json:"quota,omitempty"`
	RequestDenied *bool           `json:"requestDenied,omitempty"`
}

// ---------------------------------------------------------------------------
// Turns
// ---------------------------------------------------------------------------

// TurnInputPart is $defs/TurnInputPart (flat discriminated object).
type TurnInputPart struct {
	Type       string                 `json:"type"`
	Text       *string                `json:"text,omitempty"`
	Base64Data *string                `json:"base64Data,omitempty"`
	MediaType  *string                `json:"mediaType,omitempty"`
	Width      *int                   `json:"width,omitempty"`
	Height     *int                   `json:"height,omitempty"`
	Selector   *string                `json:"selector,omitempty"`
	Arguments  *string                `json:"arguments,omitempty"`
	UploadID   *string                `json:"uploadId,omitempty"`
	Source     *SkillInvocationSource `json:"source,omitempty"`
}

// TurnInputPartType is $defs/TurnInputPartType.
type TurnInputPartType string

const (
	// TurnInputText is the MSP wire value.
	TurnInputText TurnInputPartType = "text"
	// TurnInputImage is the MSP wire value.
	TurnInputImage TurnInputPartType = "image"
	// TurnInputSkill is the MSP wire value.
	TurnInputSkill TurnInputPartType = "skill"
	// TurnInputVideo is the MSP wire value.
	TurnInputVideo TurnInputPartType = "video"
)

// TurnStartParams is $defs/TurnStartParams.
type TurnStartParams struct {
	CommandID       string           `json:"commandId"`
	DisplayText     *string          `json:"displayText,omitempty"`
	IfBusy          *IfBusy          `json:"ifBusy,omitempty"`
	Input           []TurnInputPart  `json:"input"`
	ReasoningEffort *ReasoningEffort `json:"reasoningEffort,omitempty"`
	SessionID       string           `json:"sessionId"`
	WorkspaceRoots  []string         `json:"workspaceRoots,omitempty"`
	UserShell       json.RawMessage  `json:"userShell,omitempty"`
}

// TurnStartResult is $defs/TurnStartResult.
type TurnStartResult struct {
	CommandID      string               `json:"commandId"`
	Disposition    TurnStartDisposition `json:"disposition"`
	StartedNewTurn bool                 `json:"startedNewTurn"`
	Status         CommandStatus        `json:"status"`
	TurnID         string               `json:"turnId"`
}

// TurnInterruptParams is $defs/TurnInterruptParams.
type TurnInterruptParams struct {
	CommandID string  `json:"commandId"`
	Retract   *bool   `json:"retract,omitempty"`
	SessionID string  `json:"sessionId"`
	TurnID    *string `json:"turnId,omitempty"`
}

// TurnSteerParams is $defs/TurnSteerParams.
type TurnSteerParams struct {
	CommandID       string           `json:"commandId"`
	ExpectedTurnID  string           `json:"expectedTurnId"`
	Input           []TurnInputPart  `json:"input"`
	ReasoningEffort *ReasoningEffort `json:"reasoningEffort,omitempty"`
	SessionID       string           `json:"sessionId"`
}

// ---------------------------------------------------------------------------
// Items (revision-versioned transcript)
// ---------------------------------------------------------------------------

// Item is $defs/Item: one transcript item at one revision. Nested
// extensions not consumed by the mapper retain their opaque JSON.
type Item struct {
	ItemID         string          `json:"itemId"`
	Kind           ItemKind        `json:"kind"`
	Revision       int             `json:"revision"`
	Status         ItemStatus      `json:"status"`
	Text           *string         `json:"text,omitempty"`
	Summary        json.RawMessage `json:"summary,omitempty"`
	Tool           *string         `json:"tool,omitempty"`
	Args           *string         `json:"args,omitempty"`
	CommandText    *string         `json:"commandText,omitempty"`
	CommandID      *string         `json:"commandId,omitempty"`
	TurnID         *string         `json:"turnId,omitempty"`
	ExitCode       *int            `json:"exitCode,omitempty"`
	ApprovalID     *string         `json:"approvalId,omitempty"`
	VisibleOutput  *string         `json:"visibleOutput,omitempty"`
	FallbackText   *string         `json:"fallbackText,omitempty"`
	FailureKind    *string         `json:"failureKind,omitempty"`
	FailureReason  *string         `json:"failureReason,omitempty"`
	Objective      *string         `json:"objective,omitempty"`
	FinalSummary   *string         `json:"finalSummary,omitempty"`
	Message        *string         `json:"message,omitempty"`
	Outcome        *string         `json:"outcome,omitempty"`
	DisplayText    *string         `json:"displayText,omitempty"`
	DurationMs     *int64          `json:"durationMs,omitempty"`
	ChildSessionID *string         `json:"childSessionId,omitempty"`
	SubagentID     *string         `json:"subagentId,omitempty"`
	Retracted      *bool           `json:"retracted,omitempty"`
	Steered        *bool           `json:"steered,omitempty"`
	TokensBefore   *int64          `json:"tokensBefore,omitempty"`
	TokensAfter    *int64          `json:"tokensAfter,omitempty"`

	// Raw carries the full item object for diagnostics.
	Raw                          json.RawMessage        `json:"-"`
	AgentPath                    *string                `json:"agentPath,omitempty"`
	Attachments                  json.RawMessage        `json:"attachments,omitempty"`
	Background                   *bool                  `json:"background,omitempty"`
	BackgroundInitiator          *BackgroundInitiator   `json:"backgroundInitiator,omitempty"`
	BackgroundTerminalCompletion json.RawMessage        `json:"backgroundTerminalCompletion,omitempty"`
	Blocking                     *bool                  `json:"blocking,omitempty"`
	CallID                       *string                `json:"callId,omitempty"`
	ChildPromptRefs              json.RawMessage        `json:"childPromptRefs,omitempty"`
	ChildSessionLogPath          *string                `json:"childSessionLogPath,omitempty"`
	Children                     json.RawMessage        `json:"children,omitempty"`
	ControlStatus                *SubagentControlStatus `json:"controlStatus,omitempty"`
	CutOff                       *bool                  `json:"cutOff,omitempty"`
	DamagedRange                 json.RawMessage        `json:"damagedRange,omitempty"`
	DeclaredPhase                *string                `json:"declaredPhase,omitempty"`
	DeliveryNumber               *int64                 `json:"deliveryNumber,omitempty"`
	Depth                        *int64                 `json:"depth,omitempty"`
	DisplayLabel                 json.RawMessage        `json:"displayLabel,omitempty"`
	EarlierHistory               json.RawMessage        `json:"earlierHistory,omitempty"`
	EntryID                      *string                `json:"entryId,omitempty"`
	Event                        *HookEventName         `json:"event,omitempty"`
	ExitSignal                   *int64                 `json:"exitSignal,omitempty"`
	GenerationID                 *int64                 `json:"generationId,omitempty"`
	Held                         *bool                  `json:"held,omitempty"`
	HeldFor                      *HeldFor               `json:"heldFor,omitempty"`
	HookKey                      *string                `json:"hookKey,omitempty"`
	JobID                        *string                `json:"jobId,omitempty"`
	Label                        *string                `json:"label,omitempty"`
	MessagePhase                 *MessagePhase          `json:"messagePhase,omitempty"`
	ModelVisibleContent          json.RawMessage        `json:"modelVisibleContent,omitempty"`
	MonitorDescription           *string                `json:"monitorDescription,omitempty"`
	MonitorID                    *string                `json:"monitorId,omitempty"`
	NextFireAtMs                 *int64                 `json:"nextFireAtMs,omitempty"`
	OutputRef                    json.RawMessage        `json:"outputRef,omitempty"`
	PatchRef                     json.RawMessage        `json:"patchRef,omitempty"`
	PatchSummary                 json.RawMessage        `json:"patchSummary,omitempty"`
	Paused                       *bool                  `json:"paused,omitempty"`
	ProviderItemID               *string                `json:"providerItemId,omitempty"`
	Reason                       *string                `json:"reason,omitempty"`
	RecordedAt                   *string                `json:"recordedAt,omitempty"`
	ReminderAgentID              *string                `json:"reminderAgentId,omitempty"`
	ReplayHint                   json.RawMessage        `json:"replayHint,omitempty"`
	Result                       json.RawMessage        `json:"result,omitempty"`
	ResumeFromRunID              *string                `json:"resumeFromRunId,omitempty"`
	Role                         *string                `json:"role,omitempty"`
	RunStatus                    *HookRunStatus         `json:"runStatus,omitempty"`
	ScheduledFireAtMs            *int64                 `json:"scheduledFireAtMs,omitempty"`
	ScriptID                     *string                `json:"scriptId,omitempty"`
	SideSessionID                *string                `json:"sideSessionId,omitempty"`
	StatusAttempt                json.RawMessage        `json:"statusAttempt,omitempty"`
	StatusChildren               json.RawMessage        `json:"statusChildren,omitempty"`
	StatusHealth                 json.RawMessage        `json:"statusHealth,omitempty"`
	StatusLine                   *string                `json:"statusLine,omitempty"`
	StatusPhase                  *string                `json:"statusPhase,omitempty"`
	StatusProgress               json.RawMessage        `json:"statusProgress,omitempty"`
	StatusStep                   json.RawMessage        `json:"statusStep,omitempty"`
	StopAvailable                *bool                  `json:"stopAvailable,omitempty"`
	StrategyID                   *string                `json:"strategyId,omitempty"`
	SummarizedThrough            *string                `json:"summarizedThrough,omitempty"`
	TaskID                       *string                `json:"taskId,omitempty"`
	TerminalRowAfterItemID       *string                `json:"terminalRowAfterItemId,omitempty"`
	TokenBudget                  json.RawMessage        `json:"tokenBudget,omitempty"`
	ToolReceipt                  json.RawMessage        `json:"toolReceipt,omitempty"`
	Trigger                      *CompactionTrigger     `json:"trigger,omitempty"`
	TriggerSource                *string                `json:"triggerSource,omitempty"`
	Truncated                    *bool                  `json:"truncated,omitempty"`
	Usage                        json.RawMessage        `json:"usage,omitempty"`
	WakeSourceDescriptions       json.RawMessage        `json:"wakeSourceDescriptions,omitempty"`
	WorkID                       *string                `json:"workId,omitempty"`
	WorkflowRunID                *string                `json:"workflowRunId,omitempty"`
}

// ItemStartedParams is $defs/ItemStartedParams.
type ItemStartedParams struct {
	Item        json.RawMessage `json:"item"`
	SessionID   string          `json:"sessionId"`
	ViewCursor  string          `json:"viewCursor"`
	SourceRange json.RawMessage `json:"sourceRange,omitempty"`
}

// ItemUpdatedParams is $defs/ItemUpdatedParams.
type ItemUpdatedParams struct {
	Item        json.RawMessage `json:"item"`
	SessionID   string          `json:"sessionId"`
	ViewCursor  string          `json:"viewCursor"`
	SourceRange json.RawMessage `json:"sourceRange"`
}

// ItemCompletedParams is $defs/ItemCompletedParams.
type ItemCompletedParams struct {
	Item        json.RawMessage `json:"item"`
	SessionID   string          `json:"sessionId"`
	ViewCursor  string          `json:"viewCursor"`
	SourceRange json.RawMessage `json:"sourceRange"`
}

// ItemDeltaParams is $defs/ItemDeltaParams.
type ItemDeltaParams struct {
	Delta      string  `json:"delta"`
	Field      *string `json:"field,omitempty"`
	ItemID     string  `json:"itemId"`
	SessionID  string  `json:"sessionId"`
	ViewCursor string  `json:"viewCursor"`
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// TokenUsage is $defs/TokenUsage: raw counters verbatim.
type TokenUsage struct {
	CachedTokens     int64  `json:"cachedTokens"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	ReasoningTokens  int64  `json:"reasoningTokens"`
	CacheReadTokens  *int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens,omitempty"`
}

// CumulativeCost is $defs/CumulativeCost.
type CumulativeCost struct {
	Partial bool    `json:"partial"`
	USD     float64 `json:"usd"`
}

// CumulativeTokenUsage is $defs/CumulativeTokenUsage.
type CumulativeTokenUsage struct {
	CacheReadTokens  *int64          `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64          `json:"cacheWriteTokens,omitempty"`
	Cost             *CumulativeCost `json:"cost,omitempty"`
	OutputTokens     int64           `json:"outputTokens"`
	PromptTokens     int64           `json:"promptTokens"`
	TotalTokens      int64           `json:"totalTokens"`
}

// SessionTokenUsageParams is $defs/SessionTokenUsageParams.
type SessionTokenUsageParams struct {
	Cumulative   CumulativeTokenUsage `json:"cumulative"`
	ModelID      *string              `json:"modelId"`
	SessionID    string               `json:"sessionId"`
	TurnID       *string              `json:"turnId,omitempty"`
	Usage        TokenUsage           `json:"usage"`
	PromptTokens int64                `json:"promptTokens"`
	TotalTokens  int64                `json:"totalTokens"`
	FinishReason *string              `json:"finishReason,omitempty"`
	ViewCursor   string               `json:"viewCursor"`
	DurationMs   *int64               `json:"durationMs,omitempty"`
	SourceRange  json.RawMessage      `json:"sourceRange"`
}

// SessionContextUsageParams is $defs/SessionContextUsageParams.
type SessionContextUsageParams struct {
	Pressure     string          `json:"pressure"`
	SessionID    string          `json:"sessionId"`
	UsedTokens   int64           `json:"usedTokens"`
	WindowTokens *int64          `json:"windowTokens,omitempty"`
	ViewCursor   string          `json:"viewCursor"`
	SourceRange  json.RawMessage `json:"sourceRange"`
}

// ---------------------------------------------------------------------------
// Turn + session terminal + gap notifications
// ---------------------------------------------------------------------------

// TurnCompletedParams is $defs/TurnCompletedParams.
type TurnCompletedParams struct {
	DurationMs         *int64          `json:"durationMs,omitempty"`
	Error              *TurnError      `json:"error,omitempty"`
	Reason             *string         `json:"reason,omitempty"`
	SessionID          string          `json:"sessionId"`
	Terminal           TurnTerminal    `json:"terminal"`
	TurnID             string          `json:"turnId"`
	Usage              *TokenUsage     `json:"usage,omitempty"`
	ViewCursor         string          `json:"viewCursor"`
	SourceRange        json.RawMessage `json:"sourceRange"`
	TimeToFirstTokenMs *int64          `json:"timeToFirstTokenMs,omitempty"`
}

// TurnStartedParams carries turn/started (sessionId + turnId).
type TurnStartedParams struct {
	SessionID   string          `json:"sessionId"`
	TurnID      string          `json:"turnId"`
	ViewCursor  string          `json:"viewCursor"`
	CommandID   string          `json:"commandId"`
	SourceRange json.RawMessage `json:"sourceRange"`
}

// TurnForegroundCompletedParams is $defs/TurnForegroundCompletedParams.
type TurnForegroundCompletedParams struct {
	BlockingAgents []string        `json:"blockingAgents"`
	SessionID      string          `json:"sessionId"`
	TurnID         string          `json:"turnId"`
	ViewCursor     string          `json:"viewCursor"`
	SourceRange    json.RawMessage `json:"sourceRange"`
}

// SessionStartedParams is $defs/SessionStartedParams.
type SessionStartedParams struct {
	Session Session `json:"session"`
}

// SessionStatusChangedParams is $defs/SessionStatusChangedParams.
type SessionStatusChangedParams struct {
	SessionID  string          `json:"sessionId"`
	Status     SessionStatus   `json:"status"`
	ViewCursor *string         `json:"viewCursor"`
	Attention  json.RawMessage `json:"attention,omitempty"`
}

// ViewGapParams is $defs/ViewGapParams.
type ViewGapParams struct {
	After     string `json:"after"`
	Next      string `json:"next"`
	SessionID string `json:"sessionId"`
}

// TodoItem is $defs/TodoItem.
type TodoItem struct {
	ActiveForm *string `json:"activeForm,omitempty"`
	Status     string  `json:"status"`
	Text       string  `json:"text"`
}

// SessionTodoListChangedParams is $defs/SessionTodoListChangedParams.
type SessionTodoListChangedParams struct {
	Items       []TodoItem      `json:"items"`
	Revision    int             `json:"revision"`
	SessionID   string          `json:"sessionId"`
	SourceTool  string          `json:"sourceTool"`
	ViewCursor  string          `json:"viewCursor"`
	SourceRange json.RawMessage `json:"sourceRange"`
}

// ---------------------------------------------------------------------------
// Approvals
// ---------------------------------------------------------------------------

// ApprovalRequirementRef is $defs/ApprovalRequirementRef.
type ApprovalRequirementRef struct {
	ApprovalID  string `json:"approvalId"`
	SourceIndex int    `json:"sourceIndex"`
}

// ApprovalChoice is $defs/ApprovalChoice.
type ApprovalChoice struct {
	AcceptsFeedback *bool               `json:"acceptsFeedback,omitempty"`
	ChoiceID        string              `json:"choiceId"`
	Decision        MSPApprovalDecision `json:"decision"`
	Label           string              `json:"label"`
	Scope           string              `json:"scope"`
	RulePreview     *string             `json:"rulePreview,omitempty"`
}

// ApprovalSubject is $defs/ApprovalSubject (open union; kind + extras).
type ApprovalSubject struct {
	Kind          string          `json:"kind"`
	Command       *string         `json:"command,omitempty"`
	Path          *string         `json:"path,omitempty"`
	ToolName      *string         `json:"toolName,omitempty"`
	Target        *string         `json:"target,omitempty"`
	Access        *string         `json:"access,omitempty"`
	Host          *string         `json:"host,omitempty"`
	Port          *int            `json:"port,omitempty"`
	Protocol      *string         `json:"protocol,omitempty"`
	WorkspaceRoot *string         `json:"workspaceRoot,omitempty"`
	Origin        json.RawMessage `json:"origin,omitempty"`
	Stages        json.RawMessage `json:"stages,omitempty"`
}

// ApprovalRequestParams is $defs/ApprovalRequestParams: shared by
// approval/requested, approval/request, approval/resolved.
type ApprovalRequestParams struct {
	ApprovalID           string                 `json:"approvalId"`
	AvailableChoices     []ApprovalChoice       `json:"availableChoices"`
	CurrentRequirementID ApprovalRequirementRef `json:"currentRequirementId"`
	ItemID               string                 `json:"itemId"`
	SessionID            string                 `json:"sessionId"`
	Subject              ApprovalSubject        `json:"subject"`
	TaskID               string                 `json:"taskId"`
	ToolCallID           string                 `json:"toolCallId"`
	ToolName             string                 `json:"toolName"`
	TurnID               string                 `json:"turnId"`
	RawArgs              string                 `json:"rawArgs"`
	ViewCursor           string                 `json:"viewCursor"`
	CuaCardFacts         json.RawMessage        `json:"cuaCardFacts,omitempty"`
	JudgeEscalated       bool                   `json:"judgeEscalated"`
	ProtectedWrite       bool                   `json:"protectedWrite"`
	ReviewerReason       *string                `json:"reviewerReason,omitempty"`
	SourceRange          json.RawMessage        `json:"sourceRange"`
	SubagentOrigin       json.RawMessage        `json:"subagentOrigin,omitempty"`
}

// ApprovalDecideParams is $defs/ApprovalDecideParams.
type ApprovalDecideParams struct {
	ApprovalID    string                 `json:"approvalId"`
	ChoiceID      string                 `json:"choiceId"`
	CommandID     string                 `json:"commandId"`
	Feedback      *string                `json:"feedback,omitempty"`
	RequirementID ApprovalRequirementRef `json:"requirementId"`
	SessionID     string                 `json:"sessionId"`
}

// ApprovalDecideResult is $defs/ApprovalDecideResult.
type ApprovalDecideResult struct {
	ApprovalID string        `json:"approvalId"`
	CommandID  string        `json:"commandId"`
	Status     CommandStatus `json:"status"`
	Terminal   bool          `json:"terminal"`
}

// ApprovalResolvedParams is $defs/ApprovalResolvedParams (subset).
type ApprovalResolvedParams struct {
	ApprovalID         string               `json:"approvalId"`
	Decision           MSPApprovalDecision  `json:"decision"`
	ItemID             string               `json:"itemId"`
	SessionID          string               `json:"sessionId"`
	TurnID             string               `json:"turnId"`
	ViewCursor         string               `json:"viewCursor"`
	Amendment          json.RawMessage      `json:"amendment,omitempty"`
	DecidedAt          *string              `json:"decidedAt,omitempty"`
	DecidedByCommandID *string              `json:"decidedByCommandId,omitempty"`
	PolicyResult       ApprovalPolicyResult `json:"policyResult"`
	ResolvedBy         ApprovalResolvedBy   `json:"resolvedBy"`
	SourceRange        json.RawMessage      `json:"sourceRange"`
	StageEvidence      json.RawMessage      `json:"stageEvidence"`
	SubagentOrigin     json.RawMessage      `json:"subagentOrigin,omitempty"`
}

// UserInputRequestParams is $defs/UserInputRequestParams (subset).
type UserInputRequestParams struct {
	ItemID           string          `json:"itemId"`
	SessionID        string          `json:"sessionId"`
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	TurnID           string          `json:"turnId"`
	UserInputID      string          `json:"userInputId"`
	ViewCursor       string          `json:"viewCursor"`
	AutoResolutionMs *int64          `json:"autoResolutionMs,omitempty"`
	Questions        json.RawMessage `json:"questions"`
	SourceRange      json.RawMessage `json:"sourceRange,omitempty"`
}

// ---------------------------------------------------------------------------
// Session commands we drive
// ---------------------------------------------------------------------------

// SessionCompactParams is $defs/SessionCompactParams.
type SessionCompactParams struct {
	CommandID string  `json:"commandId"`
	SessionID string  `json:"sessionId"`
	TurnID    *string `json:"turnId,omitempty"`
}

// SessionCompactResult is $defs/SessionCompactResult.
type SessionCompactResult struct {
	CommandID string  `json:"commandId"`
	Reason    *string `json:"reason,omitempty"`
	Status    string  `json:"status"`
}

// ModelSelection is $defs/ModelSelection.
type ModelSelection struct {
	ModelID      string          `json:"modelId"`
	DisplayLabel *string         `json:"displayLabel,omitempty"`
	ProfileID    json.RawMessage `json:"profileId,omitempty"`
	ProviderID   *string         `json:"providerId,omitempty"`
}

// SessionSetModelParams is $defs/SessionSetModelParams.
type SessionSetModelParams struct {
	CommandID string         `json:"commandId"`
	Model     ModelSelection `json:"model"`
	SessionID string         `json:"sessionId"`
}

// SessionSetModelResult is $defs/SessionSetModelResult.
type SessionSetModelResult struct {
	CommandID string        `json:"commandId"`
	Status    CommandStatus `json:"status"`
}

// ModelCatalogEntry is $defs/ModelCatalogEntry (subset).
type ModelCatalogEntry struct {
	ModelID                 string           `json:"modelId"`
	DisplayLabel            string           `json:"displayLabel"`
	IsActive                bool             `json:"isActive"`
	IsDefault               bool             `json:"isDefault"`
	ProviderID              string           `json:"providerId"`
	ProfileID               *string          `json:"profileId"`
	Description             *string          `json:"description,omitempty"`
	ContextLimit            *int64           `json:"contextLimit"`
	OutputLimit             *int64           `json:"outputLimit"`
	Cost                    json.RawMessage  `json:"cost"`
	DefaultReasoningEffort  *ReasoningEffort `json:"defaultReasoningEffort,omitempty"`
	ReasoningEffortVariants json.RawMessage  `json:"reasoningEffortVariants,omitempty"`
	ReleaseDate             json.RawMessage  `json:"releaseDate"`
	Variants                json.RawMessage  `json:"variants,omitempty"`
}

// ModelListResult is $defs/ModelListResult.
type ModelListResult struct {
	Models     []ModelCatalogEntry `json:"models"`
	ProfileID  *string             `json:"profileId"`
	ProviderID string              `json:"providerId"`
	Source     string              `json:"source"`
}

// ---------------------------------------------------------------------------
// JSON-RPC error envelope
// ---------------------------------------------------------------------------

// ErrorData is $defs/ErrorData.
type ErrorData struct {
	Kind                 ErrorKind       `json:"kind"`
	Reason               *string         `json:"reason,omitempty"`
	Details              json.RawMessage `json:"details,omitempty"`
	AlignedNextOffset    *int64          `json:"alignedNextOffset,omitempty"`
	Anchor               json.RawMessage `json:"anchor,omitempty"`
	ApprovalID           *string         `json:"approvalId,omitempty"`
	Availability         *string         `json:"availability,omitempty"`
	Capability           *CapabilityName `json:"capability,omitempty"`
	Capacity             *int64          `json:"capacity,omitempty"`
	ChoiceID             *string         `json:"choiceId,omitempty"`
	CommandID            *string         `json:"commandId,omitempty"`
	CurrentRequirementID json.RawMessage `json:"currentRequirementId,omitempty"`
	Descriptor           *string         `json:"descriptor,omitempty"`
	EarliestCursor       json.RawMessage `json:"earliestCursor,omitempty"`
	ItemID               *string         `json:"itemId,omitempty"`
	LastTurnID           *string         `json:"lastTurnId,omitempty"`
	LatestBoundaryCursor json.RawMessage `json:"latestBoundaryCursor,omitempty"`
	LimitBytes           *int64          `json:"limitBytes,omitempty"`
	Marketplace          *string         `json:"marketplace,omitempty"`
	Name                 *string         `json:"name,omitempty"`
	OutputRef            *string         `json:"outputRef,omitempty"`
	Paths                json.RawMessage `json:"paths,omitempty"`
	RecoveryObservation  json.RawMessage `json:"recoveryObservation,omitempty"`
	Resolution           json.RawMessage `json:"resolution,omitempty"`
	Retryable            *bool           `json:"retryable,omitempty"`
	SeedFallbackReason   *string         `json:"seedFallbackReason,omitempty"`
	SeedPath             *string         `json:"seedPath,omitempty"`
	Selector             *string         `json:"selector,omitempty"`
	SessionID            *string         `json:"sessionId,omitempty"`
	Settlement           json.RawMessage `json:"settlement,omitempty"`
	UploadID             *string         `json:"uploadId,omitempty"`
	UserInputID          *string         `json:"userInputId,omitempty"`
	ViewCursor           *string         `json:"viewCursor,omitempty"`
}

// Additional CapabilityName wire values from the stable export.
const (
	CapabilityNameComputerUseHostSetup CapabilityName = "computerUseHostSetup"
	CapabilityNameSessionRename        CapabilityName = "sessionRename"
)

// ItemKind wire values from the stable export.
const (
	ItemKindMonitorFeed                  ItemKind = "monitorFeed"
	ItemKindDamagedRange                 ItemKind = "damagedRange"
	ItemKindScheduledFire                ItemKind = "scheduledFire"
	ItemKindBackgroundTerminalCompletion ItemKind = "backgroundTerminalCompletion"
	ItemKindOverdueNotice                ItemKind = "overdueNotice"
	ItemKindEarlierHistory               ItemKind = "earlierHistory"
)

// ErrorKind wire values from the stable export.
const (
	ErrorKindParseError                  ErrorKind = "parseError"
	ErrorKindInvalidRequest              ErrorKind = "invalidRequest"
	ErrorKindNotInitialized              ErrorKind = "notInitialized"
	ErrorKindAlreadyInitialized          ErrorKind = "alreadyInitialized"
	ErrorKindMethodNotFound              ErrorKind = "methodNotFound"
	ErrorKindInvalidParams               ErrorKind = "invalidParams"
	ErrorKindExperimentalRequired        ErrorKind = "experimentalRequired"
	ErrorKindInternal                    ErrorKind = "internal"
	ErrorKindPageEventTooLarge           ErrorKind = "pageEventTooLarge"
	ErrorKindOutputResultTooLarge        ErrorKind = "outputResultTooLarge"
	ErrorKindOverloaded                  ErrorKind = "overloaded"
	ErrorKindInputTooLarge               ErrorKind = "inputTooLarge"
	ErrorKindCapabilityRequired          ErrorKind = "capabilityRequired"
	ErrorKindInterrupted                 ErrorKind = "interrupted"
	ErrorKindCancelled                   ErrorKind = "cancelled"
	ErrorKindSessionInUse                ErrorKind = "sessionInUse"
	ErrorKindSessionAmbiguous            ErrorKind = "sessionAmbiguous"
	ErrorKindForkBoundaryInvalid         ErrorKind = "forkBoundaryInvalid"
	ErrorKindSessionNotLoaded            ErrorKind = "sessionNotLoaded"
	ErrorKindSessionStreamMismatch       ErrorKind = "sessionStreamMismatch"
	ErrorKindCommandRejected             ErrorKind = "commandRejected"
	ErrorKindBackpressured               ErrorKind = "backpressured"
	ErrorKindSkillNotFound               ErrorKind = "skillNotFound"
	ErrorKindComputerUseUnavailable      ErrorKind = "computerUseUnavailable"
	ErrorKindMarketplaceNotFound         ErrorKind = "marketplaceNotFound"
	ErrorKindPluginNotFound              ErrorKind = "pluginNotFound"
	ErrorKindMarketplaceAlreadyExists    ErrorKind = "marketplaceAlreadyExists"
	ErrorKindPluginMutationsDisabled     ErrorKind = "pluginMutationsDisabled"
	ErrorKindUploadNotFound              ErrorKind = "uploadNotFound"
	ErrorKindViewTruncated               ErrorKind = "viewTruncated"
	ErrorKindOutputUnavailable           ErrorKind = "outputUnavailable"
	ErrorKindBoundaryPruned              ErrorKind = "boundaryPruned"
	ErrorKindBoundaryUnusable            ErrorKind = "boundaryUnusable"
	ErrorKindNoBoundary                  ErrorKind = "noBoundary"
	ErrorKindApprovalNotFound            ErrorKind = "approvalNotFound"
	ErrorKindApprovalAlreadyResolved     ErrorKind = "approvalAlreadyResolved"
	ErrorKindApprovalChoiceInvalid       ErrorKind = "approvalChoiceInvalid"
	ErrorKindApprovalRequirementStale    ErrorKind = "approvalRequirementStale"
	ErrorKindApprovalReviewerUnavailable ErrorKind = "approvalReviewerUnavailable"
	ErrorKindUserInputNotFound           ErrorKind = "userInputNotFound"
	ErrorKindUserInputAlreadySettled     ErrorKind = "userInputAlreadySettled"
	ErrorKindUserInputAnswerInvalid      ErrorKind = "userInputAnswerInvalid"
)

// SessionMCPStdioFraming wire values from the stable export.
const (
	SessionMCPStdioFramingAuto              SessionMCPStdioFraming = "auto"
	SessionMCPStdioFramingContentLength     SessionMCPStdioFraming = "contentLength"
	SessionMCPStdioFramingLineDelimitedJSON SessionMCPStdioFraming = "lineDelimitedJson"
)

// SandboxAvailability is $defs/SandboxAvailability.
type SandboxAvailability string

// SandboxAvailability wire values from the stable export.
const (
	SandboxAvailabilityAvailable   SandboxAvailability = "available"
	SandboxAvailabilityUnavailable SandboxAvailability = "unavailable"
)

// SandboxUnavailableReason is $defs/SandboxUnavailableReason.
type SandboxUnavailableReason string

// SandboxUnavailableReason wire values from the stable export.
const (
	SandboxUnavailableReasonUserNamespacesDenied  SandboxUnavailableReason = "userNamespacesDenied"
	SandboxUnavailableReasonBubblewrapUnavailable SandboxUnavailableReason = "bubblewrapUnavailable"
)

// RecoveryMode is $defs/RecoveryMode.
type RecoveryMode string

// RecoveryMode wire values from the stable export.
const (
	RecoveryModeOn  RecoveryMode = "on"
	RecoveryModeOff RecoveryMode = "off"
)

// SessionKind is $defs/SessionKind.
type SessionKind string

// SessionKind wire values from the stable export.
const (
	SessionKindRoot     SessionKind = "root"
	SessionKindSubagent SessionKind = "subagent"
)

// SessionNameOrigin is $defs/SessionNameOrigin.
type SessionNameOrigin string

// Additional SessionNameOrigin wire values from the stable export.
const (
	SessionNameOriginAutomatic SessionNameOrigin = "automatic"
	SessionNameOriginCustom    SessionNameOrigin = "custom"
)

// SkillInvocationSource is $defs/SkillInvocationSource.
type SkillInvocationSource string

// Additional SkillInvocationSource wire values from the stable export.
const (
	SkillInvocationSourceBundled SkillInvocationSource = "bundled"
	SkillInvocationSourceUser    SkillInvocationSource = "user"
	SkillInvocationSourceProject SkillInvocationSource = "project"
	SkillInvocationSourcePlugin  SkillInvocationSource = "plugin"
)

// BackgroundInitiator is $defs/BackgroundInitiator.
type BackgroundInitiator string

// BackgroundInitiator wire values from the stable export.
const (
	BackgroundInitiatorUser    BackgroundInitiator = "user"
	BackgroundInitiatorTimeout BackgroundInitiator = "timeout"
)

// SubagentControlStatus is $defs/SubagentControlStatus.
type SubagentControlStatus string

// SubagentControlStatus wire values from the stable export.
const (
	SubagentControlStatusAccepted             SubagentControlStatus = "accepted"
	SubagentControlStatusStarting             SubagentControlStatus = "starting"
	SubagentControlStatusRunning              SubagentControlStatus = "running"
	SubagentControlStatusResultReady          SubagentControlStatus = "resultReady"
	SubagentControlStatusClosing              SubagentControlStatus = "closing"
	SubagentControlStatusClosed               SubagentControlStatus = "closed"
	SubagentControlStatusRecoveryPending      SubagentControlStatus = "recoveryPending"
	SubagentControlStatusManualReconciliation SubagentControlStatus = "manualReconciliation"
)

// HookEventName is $defs/HookEventName.
type HookEventName string

// HookEventName wire values from the stable export.
const (
	HookEventNameSessionStart       HookEventName = "sessionStart"
	HookEventNameUserPromptSubmit   HookEventName = "userPromptSubmit"
	HookEventNamePreToolUse         HookEventName = "preToolUse"
	HookEventNamePermissionRequest  HookEventName = "permissionRequest"
	HookEventNamePostToolUse        HookEventName = "postToolUse"
	HookEventNamePreLLMCall         HookEventName = "preLLMCall"
	HookEventNamePostLLMCall        HookEventName = "postLLMCall"
	HookEventNamePreCompact         HookEventName = "preCompact"
	HookEventNamePostCompact        HookEventName = "postCompact"
	HookEventNameSubagentStart      HookEventName = "subagentStart"
	HookEventNameSubagentStop       HookEventName = "subagentStop"
	HookEventNameStop               HookEventName = "stop"
	HookEventNameSessionEnd         HookEventName = "sessionEnd"
	HookEventNameNotification       HookEventName = "notification"
	HookEventNamePostToolUseFailure HookEventName = "postToolUseFailure"
	HookEventNameStopFailure        HookEventName = "stopFailure"
	HookEventNamePostToolBatch      HookEventName = "postToolBatch"
	HookEventNameInterrupt          HookEventName = "interrupt"
	HookEventNameSessionFork        HookEventName = "sessionFork"
	HookEventNameToolUseStart       HookEventName = "toolUseStart"
)

// HeldFor is $defs/HeldFor.
type HeldFor string

// Additional HeldFor wire values from the stable export.
const (
	HeldForApproval HeldFor = "approval"
	HeldForHook     HeldFor = "hook"
)

// MessagePhase is $defs/MessagePhase.
type MessagePhase string

// MessagePhase wire values from the stable export.
const (
	MessagePhaseCommentary  MessagePhase = "commentary"
	MessagePhaseFinalAnswer MessagePhase = "finalAnswer"
)

// HookRunStatus is $defs/HookRunStatus.
type HookRunStatus string

// Additional HookRunStatus wire values from the stable export.
const (
	HookRunStatusCompleted HookRunStatus = "completed"
	HookRunStatusBlocked   HookRunStatus = "blocked"
	HookRunStatusFailed    HookRunStatus = "failed"
	HookRunStatusTimedOut  HookRunStatus = "timedOut"
	HookRunStatusCancelled HookRunStatus = "cancelled"
)

// CompactionTrigger is $defs/CompactionTrigger.
type CompactionTrigger string

// Additional CompactionTrigger wire values from the stable export.
const (
	CompactionTriggerManual CompactionTrigger = "manual"
	CompactionTriggerAuto   CompactionTrigger = "auto"
)

// ApprovalPolicyResult is $defs/ApprovalPolicyResult.
type ApprovalPolicyResult string

// Additional ApprovalPolicyResult wire values from the stable export.
const (
	ApprovalPolicyResultAllow ApprovalPolicyResult = "allow"
	ApprovalPolicyResultDeny  ApprovalPolicyResult = "deny"
)

// ApprovalResolvedBy is $defs/ApprovalResolvedBy.
type ApprovalResolvedBy string

// ApprovalResolvedBy wire values from the stable export.
const (
	ApprovalResolvedByUser     ApprovalResolvedBy = "user"
	ApprovalResolvedByPolicy   ApprovalResolvedBy = "policy"
	ApprovalResolvedByLlmJudge ApprovalResolvedBy = "llmJudge"
)
