package afclient

// SessionStatus matches the public API status union type.
type SessionStatus string

// Session status values matching the public API status union type.
const (
	StatusQueued    SessionStatus = "queued"
	StatusParked    SessionStatus = "parked"
	StatusWorking   SessionStatus = "working"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
	StatusStopped   SessionStatus = "stopped"
)

// StatsResponse matches GET /api/public/stats.
type StatsResponse struct {
	WorkersOnline     int     `json:"workersOnline"`
	AgentsWorking     int     `json:"agentsWorking"`
	QueueDepth        int     `json:"queueDepth"`
	CompletedToday    int     `json:"completedToday"`
	AvailableCapacity int     `json:"availableCapacity"`
	TotalCostToday    float64 `json:"totalCostToday"`
	TotalCostAllTime  float64 `json:"totalCostAllTime"`
	SessionCountToday int     `json:"sessionCountToday"`
	Timestamp         string  `json:"timestamp"`
}

// SessionResponse matches a single session in GET /api/public/sessions.
type SessionResponse struct {
	ID         string        `json:"id"`
	Identifier string        `json:"identifier"`
	Status     SessionStatus `json:"status"`
	WorkType   string        `json:"workType"`
	StartedAt  string        `json:"startedAt"`
	Duration   int           `json:"duration"`
	CostUsd    *float64      `json:"costUsd,omitempty"`
	Provider   *string       `json:"provider,omitempty"`
}

// SessionsListResponse matches GET /api/public/sessions.
type SessionsListResponse struct {
	Sessions  []SessionResponse `json:"sessions"`
	Count     int               `json:"count"`
	Timestamp string            `json:"timestamp"`
}

// SessionTimeline represents the timeline in a session detail response.
type SessionTimeline struct {
	Created   string  `json:"created"`
	Queued    *string `json:"queued,omitempty"`
	Started   *string `json:"started,omitempty"`
	Completed *string `json:"completed,omitempty"`
}

// SessionDetail is the inner session object in the detail response.
type SessionDetail struct {
	ID           string          `json:"id"`
	Identifier   string          `json:"identifier"`
	Status       SessionStatus   `json:"status"`
	WorkType     string          `json:"workType"`
	StartedAt    string          `json:"startedAt"`
	Duration     int             `json:"duration"`
	Timeline     SessionTimeline `json:"timeline"`
	Provider     *string         `json:"provider,omitempty"`
	Branch       *string         `json:"branch,omitempty"`
	IssueTitle   *string         `json:"issueTitle,omitempty"`
	CostUsd      *float64        `json:"costUsd,omitempty"`
	InputTokens  *int            `json:"inputTokens,omitempty"`
	OutputTokens *int            `json:"outputTokens,omitempty"`
}

// SessionDetailResponse matches GET /api/public/sessions/:id.
type SessionDetailResponse struct {
	Session   SessionDetail `json:"session"`
	Timestamp string        `json:"timestamp"`
}

// ActivityType represents the type of an agent activity event.
type ActivityType string

// Activity type values for agent activity events.
const (
	ActivityThought  ActivityType = "thought"
	ActivityAction   ActivityType = "action"
	ActivityResponse ActivityType = "response"
	ActivityError    ActivityType = "error"
	ActivityProgress ActivityType = "progress"
)

// ActivityEvent represents a single activity from the streaming API.
type ActivityEvent struct {
	ID        string       `json:"id"`
	Type      ActivityType `json:"type"`
	Content   string       `json:"content"`
	ToolName  *string      `json:"toolName,omitempty"`
	Timestamp string       `json:"timestamp"`
}

// ActivityListResponse matches GET /api/public/sessions/:id/activities.
type ActivityListResponse struct {
	Activities    []ActivityEvent `json:"activities"`
	Cursor        *string         `json:"cursor,omitempty"`
	SessionStatus SessionStatus   `json:"sessionStatus"`
}

// StopSessionResponse matches POST /api/public/sessions/:id/stop.
type StopSessionResponse struct {
	Stopped        bool          `json:"stopped"`
	SessionID      string        `json:"sessionId"`
	PreviousStatus SessionStatus `json:"previousStatus"`
	NewStatus      SessionStatus `json:"newStatus"`
}

// ChatSessionRequest is the body of POST /api/public/sessions/:id/prompt.
type ChatSessionRequest struct {
	Prompt string `json:"prompt"`
}

// ChatSessionResponse matches POST /api/public/sessions/:id/prompt.
type ChatSessionResponse struct {
	Delivered     bool          `json:"delivered"`
	PromptID      string        `json:"promptId"`
	SessionID     string        `json:"sessionId"`
	SessionStatus SessionStatus `json:"sessionStatus"`
}

// ReconnectSessionRequest is the body of POST /api/public/sessions/:id/reconnect.
// Cursor and LastEventID are both optional resume hints for the activity stream;
// callers may send either, neither, or both depending on what they have cached.
type ReconnectSessionRequest struct {
	Cursor      *string `json:"cursor,omitempty"`
	LastEventID *string `json:"lastEventId,omitempty"`
}

// ReconnectSessionResponse matches POST /api/public/sessions/:id/reconnect.
type ReconnectSessionResponse struct {
	Reconnected   bool          `json:"reconnected"`
	SessionID     string        `json:"sessionId"`
	SessionStatus SessionStatus `json:"sessionStatus"`
	MissedEvents  int           `json:"missedEvents"`
}

// SubmitTaskRequest matches POST /api/mcp/submit-task.
type SubmitTaskRequest struct {
	IssueID     string `json:"issueId"`
	Description string `json:"description,omitempty"`
	WorkType    string `json:"workType,omitempty"`
	Priority    int    `json:"priority,omitempty"`
}

// SubmitTaskResponse matches the submit-task response.
type SubmitTaskResponse struct {
	Submitted bool   `json:"submitted"`
	TaskID    string `json:"taskId"`
	IssueID   string `json:"issueId"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	WorkType  string `json:"workType"`
}

// StopAgentRequest matches POST /api/mcp/stop-agent.
type StopAgentRequest struct {
	TaskID string `json:"taskId"`
}

// StopAgentResponse matches the stop-agent response.
type StopAgentResponse struct {
	Stopped        bool   `json:"stopped"`
	TaskID         string `json:"taskId"`
	IssueID        string `json:"issueId"`
	PreviousStatus string `json:"previousStatus"`
	NewStatus      string `json:"newStatus"`
}

// ForwardPromptRequest matches POST /api/mcp/forward-prompt.
type ForwardPromptRequest struct {
	TaskID  string `json:"taskId"`
	Message string `json:"message"`
}

// ForwardPromptResponse matches the forward-prompt response.
type ForwardPromptResponse struct {
	Forwarded     bool   `json:"forwarded"`
	PromptID      string `json:"promptId"`
	TaskID        string `json:"taskId"`
	IssueID       string `json:"issueId"`
	SessionStatus string `json:"sessionStatus"`
}

// CostReportRequest matches GET /api/mcp/cost-report.
type CostReportRequest struct {
	TaskID string `json:"taskId,omitempty"`
}

// CostReportResponse matches the cost-report response (fleet-wide).
type CostReportResponse struct {
	TotalSessions        int     `json:"totalSessions"`
	SessionsWithCostData int     `json:"sessionsWithCostData"`
	TotalCostUsd         float64 `json:"totalCostUsd"`
	TotalInputTokens     int     `json:"totalInputTokens"`
	TotalOutputTokens    int     `json:"totalOutputTokens"`
}

// ListFleetRequest matches GET /api/mcp/list-fleet.
type ListFleetRequest struct {
	Status []string `json:"status,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// ListFleetResponse matches the list-fleet response.
type ListFleetResponse struct {
	Total    int               `json:"total"`
	Returned int               `json:"returned"`
	Sessions []SessionResponse `json:"sessions"`
}

// WhoAmIResponse matches GET /api/cli/whoami.
type WhoAmIResponse struct {
	Org      WhoAmIOrg       `json:"org"`
	Projects []WhoAmIProject `json:"projects"`
	Scopes   []string        `json:"scopes"`
}

// WhoAmIOrg is the organization details from whoami.
type WhoAmIOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Plan string `json:"plan"`
}

// WhoAmIProject is a project from whoami.
type WhoAmIProject struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	IsDefault       bool   `json:"isDefault"`
	SandboxProvider string `json:"sandboxProvider"`
	TeamSlug        string `json:"teamSlug"`
	TeamName        string `json:"teamName"`
}

// ── Machine / Daemon ─────────────────────────────────────────────────────────
// Types derived from 011-local-daemon-fleet.md and 004-sandbox-capability-matrix.md.

// MachineID is a provider-namespaced opaque identifier for a registered machine
// (local daemon host). Matches the `machine.id` field in daemon.yaml.
type MachineID = string

// DaemonStatus is the lifecycle state reported by a running rensei-daemon.
// Values mirror the wire format emitted in NDJSON daemon logs and the
// registered-status field the orchestrator observes.
type DaemonStatus string

const (
	// DaemonReady indicates the daemon is running and accepting new work.
	DaemonReady DaemonStatus = "ready"
	// DaemonDraining indicates the daemon is finishing in-flight work before restart.
	DaemonDraining DaemonStatus = "draining"
	// DaemonPaused indicates the daemon is running but not accepting new sessions.
	DaemonPaused DaemonStatus = "paused"
	// DaemonStopped indicates the daemon process is not running.
	DaemonStopped DaemonStatus = "stopped"
	// DaemonUpdating indicates the daemon is applying an auto-update.
	DaemonUpdating DaemonStatus = "updating"
)

// MachineCapacity describes the resource envelope for a single daemon machine.
// Field names match the capacity block in daemon.yaml (camelCase in wire format).
type MachineCapacity struct {
	MaxConcurrentSessions int `json:"maxConcurrentSessions"`
	MaxVCpuPerSession     int `json:"maxVCpuPerSession,omitempty"`
	MaxMemoryMbPerSession int `json:"maxMemoryMbPerSession,omitempty"`
	ReservedVCpu          int `json:"reservedVCpu,omitempty"`
	ReservedMemoryMb      int `json:"reservedMemoryMb,omitempty"`
}

// MachineStats is the per-machine snapshot surfaced by the orchestrator for
// multi-machine fleet views (014-tui-operator-surfaces §Worker+Fleet section).
// Extended into StatsResponse.Machines to enable the "MachinePivot" TUI primitive.
type MachineStats struct {
	// ID is the daemon-configured machine identifier (e.g. "mac-studio-marks-office").
	ID MachineID `json:"id"`
	// Region is the locality hint declared in daemon.yaml (e.g. "home-network").
	Region string `json:"region"`
	// Status is the current daemon lifecycle state.
	Status DaemonStatus `json:"status"`
	// Version is the rensei-daemon binary version string.
	Version string `json:"version"`
	// ActiveSessions is the count of sessions currently running on this machine.
	ActiveSessions int `json:"activeSessions"`
	// Capacity holds the declared resource envelope for this daemon.
	Capacity MachineCapacity `json:"capacity"`
	// UptimeSeconds is how long the daemon process has been running.
	UptimeSeconds int64 `json:"uptimeSeconds"`
	// LastSeenAt is an RFC3339 timestamp of the most recent heartbeat.
	LastSeenAt string `json:"lastSeenAt"`
}

// ── Provider cost breakdown ───────────────────────────────────────────────────

// ProviderCost is the per-provider cost breakdown entry used to extend
// StatsResponse. Provider is the sandbox/LLM provider name (e.g. "anthropic",
// "vercel", "e2b"). CostUsd is the accrued cost for the current reporting period.
type ProviderCost struct {
	Provider string  `json:"provider"`
	CostUsd  float64 `json:"costUsd"`
	Sessions int     `json:"sessions"`
}

// ── Workarea pool ─────────────────────────────────────────────────────────────
// Types derived from 003-workarea-provider.md (local-pool implementation section).

// WorkareaPoolMemberStatus is the lifecycle state of a single warm-pool member.
// Values match the state machine in 003-workarea-provider.md §Pool member states.
type WorkareaPoolMemberStatus string

const (
	// PoolMemberWarming means the clone + install is in progress.
	PoolMemberWarming WorkareaPoolMemberStatus = "warming"
	// PoolMemberReady means the member is clean and available for acquire.
	PoolMemberReady WorkareaPoolMemberStatus = "ready"
	// PoolMemberAcquired means the member is currently held by a session.
	PoolMemberAcquired WorkareaPoolMemberStatus = "acquired"
	// PoolMemberReleasing means a scoped clean is in progress before returning to ready.
	PoolMemberReleasing WorkareaPoolMemberStatus = "releasing"
	// PoolMemberInvalid means the lockfile changed or staleness exceeded; pending rebuild.
	PoolMemberInvalid WorkareaPoolMemberStatus = "invalid"
	// PoolMemberRetired means the member is slated for destruction.
	PoolMemberRetired WorkareaPoolMemberStatus = "retired"
)

// WorkareaPoolMember describes a single entry in the local workarea pool.
// Keyed by (Repository, ToolchainKey) per the pool management design in 003.
type WorkareaPoolMember struct {
	// ID is the provider-namespaced workarea identifier.
	ID string `json:"id"`
	// Repository is the git remote URL this pool member is cloned from.
	Repository string `json:"repository"`
	// Ref is the commit or branch actually checked out.
	Ref string `json:"ref"`
	// ToolchainKey is the canonical key for the toolchain set (e.g. "node-20+java-17").
	ToolchainKey string `json:"toolchainKey"`
	// Status is the current pool member lifecycle state.
	Status WorkareaPoolMemberStatus `json:"status"`
	// CleanStateChecksum is the sha256 of the canonical clean-state files.
	CleanStateChecksum string `json:"cleanStateChecksum"`
	// AcquiredBy is the session ID currently holding this member, or empty.
	AcquiredBy string `json:"acquiredBy,omitempty"`
	// CreatedAt is the RFC3339 timestamp of initial pool member creation.
	CreatedAt string `json:"createdAt"`
	// LastAcquiredAt is the RFC3339 timestamp of the most recent acquire (for LRU eviction).
	LastAcquiredAt string `json:"lastAcquiredAt,omitempty"`
	// DiskUsageMb is the approximate on-disk size of this pool member.
	DiskUsageMb int64 `json:"diskUsageMb,omitempty"`
}

// WorkareaPoolStats is the aggregate view of the local workarea pool returned
// by `af daemon stats --pool`. Matches the "WorkareaPoolPanel" TUI primitive in
// 014-tui-operator-surfaces.md.
type WorkareaPoolStats struct {
	// Members is the full list of pool members across all (repo, toolchain) keys.
	Members []WorkareaPoolMember `json:"members"`
	// TotalMembers is the count of all pool members regardless of status.
	TotalMembers int `json:"totalMembers"`
	// ReadyMembers is the count of members in the "ready" state.
	ReadyMembers int `json:"readyMembers"`
	// AcquiredMembers is the count of members currently held by sessions.
	AcquiredMembers int `json:"acquiredMembers"`
	// WarmingMembers is the count of members still being initialised.
	WarmingMembers int `json:"warmingMembers"`
	// InvalidMembers is the count of members pending rebuild.
	InvalidMembers int `json:"invalidMembers"`
	// TotalDiskUsageMb is the sum of DiskUsageMb across all pool members.
	TotalDiskUsageMb int64 `json:"totalDiskUsageMb"`
	// Timestamp is the RFC3339 capture time of this snapshot.
	Timestamp string `json:"timestamp"`
}

// ── Pool eviction / capacity config ──────────────────────────────────────────

// EvictPoolRequest is the body for POST /api/daemon/pool/evict.
// Exactly one eviction selector must be set.
type EvictPoolRequest struct {
	// RepoURL restricts eviction to pool members cloned from this repository.
	// Required — the daemon refuses a request that omits it.
	RepoURL string `json:"repoUrl"`
	// OlderThanSeconds evicts members whose LastAcquiredAt (or CreatedAt when
	// never acquired) is older than this duration.  Must be > 0.
	OlderThanSeconds int64 `json:"olderThanSeconds"`
}

// EvictPoolResponse is the response from POST /api/daemon/pool/evict.
type EvictPoolResponse struct {
	// Evicted is the count of pool members scheduled for destruction.
	Evicted int `json:"evicted"`
	// Message is a human-readable summary.
	Message string `json:"message"`
	// CorrelationID is the Layer 6 hook event correlation ID emitted by the
	// daemon's observability subscriber (REN-1313). Consumers can match this
	// against the pool-stats-evict hook stream.
	CorrelationID string `json:"correlationId,omitempty"`
}

// SetCapacityResponse is the response from POST /api/daemon/capacity.
type SetCapacityResponse struct {
	// OK is true when the config key was accepted and written to daemon.yaml.
	OK bool `json:"ok"`
	// Key is the dotted config key that was set (e.g. "capacity.poolMaxDiskGb").
	Key string `json:"key"`
	// Value is the string representation of the new value.
	Value string `json:"value"`
	// Message is a human-readable description of the outcome.
	Message string `json:"message"`
}

// ── SandboxProvider ───────────────────────────────────────────────────────────
// Types derived from 004-sandbox-capability-matrix.md.

// SandboxProviderID is an opaque identifier for a registered SandboxProvider.
// Corresponds to the `providerId` field on SandboxHandle in the architecture.
type SandboxProviderID = string

// SandboxTransportModel is the transport model declared by a SandboxProvider.
// Values match the `transportModel` capability field in 004.
type SandboxTransportModel string

const (
	// TransportDialIn means the orchestrator initiates connections to the sandbox.
	TransportDialIn SandboxTransportModel = "dial-in"
	// TransportDialOut means the worker boots and dials the orchestrator.
	TransportDialOut SandboxTransportModel = "dial-out"
	// TransportEither means both models are supported; orchestrator picks.
	TransportEither SandboxTransportModel = "either"
)

// SandboxBillingModel is the billing model declared by a SandboxProvider.
// Values match the `billingModel` capability field in 004.
type SandboxBillingModel string

const (
	// BillingWallClock means the provider charges for every second the sandbox runs.
	BillingWallClock SandboxBillingModel = "wall-clock"
	// BillingActiveCPU means the provider charges only for active CPU time.
	BillingActiveCPU SandboxBillingModel = "active-cpu"
	// BillingInvocation means the provider charges per invocation (FaaS-style).
	BillingInvocation SandboxBillingModel = "invocation"
	// BillingFixed means the provider uses user-owned hardware (no per-session charge).
	BillingFixed SandboxBillingModel = "fixed"
)

// SandboxProviderStats is the runtime snapshot for a registered SandboxProvider.
// Used by the dashboard "SandboxProvider awareness" panel and the scheduler's
// capacity-filter step (004 §Scheduling algorithm step 3).
type SandboxProviderStats struct {
	// ID is the unique identifier for this provider (e.g. "local", "e2b", "vercel").
	ID SandboxProviderID `json:"id"`
	// DisplayName is the human-readable provider label.
	DisplayName string `json:"displayName"`
	// TransportModel is the declared transport mode for this provider.
	TransportModel SandboxTransportModel `json:"transportModel"`
	// BillingModel is the declared billing model for this provider.
	BillingModel SandboxBillingModel `json:"billingModel"`
	// ProvisionedActive is the count of sandboxes currently running.
	ProvisionedActive int `json:"provisionedActive"`
	// ProvisionedPaused is the count of paused sandboxes (storage-cost only).
	ProvisionedPaused int `json:"provisionedPaused"`
	// MaxConcurrent is the declared ceiling, or -1 for unbounded.
	MaxConcurrent int `json:"maxConcurrent"`
	// Regions is the list of ISO region codes this provider serves; ["*"] means any.
	Regions []string `json:"regions"`
	// SupportsPauseResume indicates memory+FS preserve capability (e.g. E2B).
	SupportsPauseResume bool `json:"supportsPauseResume"`
	// SupportsFsSnapshot indicates FS-only snapshot capability (e.g. Vercel).
	SupportsFsSnapshot bool `json:"supportsFsSnapshot"`
	// IsA2ARemote indicates this provider represents a remote A2A agent peer.
	IsA2ARemote bool `json:"isA2ARemote"`
	// Healthy is false when the provider is in a degraded or unhealthy state.
	Healthy bool `json:"healthy"`
	// CapturedAt is the RFC3339 timestamp of this capacity snapshot.
	CapturedAt string `json:"capturedAt"`
}

// ── Kit ───────────────────────────────────────────────────────────────────────
// Types derived from 005-kit-manifest-spec.md.

// KitID is the namespaced kit identifier as declared in the kit manifest
// (e.g. "spring/java", "ts/nextjs").
type KitID = string

// KitDetection is the result of running kit detection against a workarea.
// Corresponds to KitDetectResult in 005 and the "KitDetectResult" TUI primitive
// in 014-tui-operator-surfaces.md.
type KitDetection struct {
	// KitID is the namespaced identifier of the detected kit.
	KitID KitID `json:"kitId"`
	// KitVersion is the semver version string from the kit manifest.
	KitVersion string `json:"kitVersion"`
	// Applies indicates whether the kit matched the workarea.
	Applies bool `json:"applies"`
	// Confidence is the match confidence score in the range [0, 1].
	Confidence float64 `json:"confidence"`
	// Reason is a human-readable explanation of the detection outcome.
	Reason string `json:"reason,omitempty"`
	// ToolchainDemand is the toolchain spec the kit would impose if applied.
	// Keys are toolchain names (e.g. "java", "node"); values are semver ranges.
	ToolchainDemand map[string]string `json:"toolchainDemand,omitempty"`
	// DetectPhase records which detection phase produced this result.
	// "declarative" means file-pattern match only; "executable" means the kit's
	// detect binary also ran.
	DetectPhase string `json:"detectPhase,omitempty"`
}

// KitContribution summarises what an applied kit added to a session.
// Corresponds to KitContribution in 005 and the "KitContributionDiff" TUI
// primitive in 014-tui-operator-surfaces.md.
type KitContribution struct {
	// KitID is the namespaced identifier of the contributing kit.
	KitID KitID `json:"kitId"`
	// KitVersion is the semver version string from the kit manifest.
	KitVersion string `json:"kitVersion"`
	// Commands is the set of build/test/validate commands contributed by this kit.
	// Keys are command names (e.g. "build", "test"); values are shell strings.
	Commands map[string]string `json:"commands,omitempty"`
	// PromptFragmentCount is the number of prompt fragments contributed.
	PromptFragmentCount int `json:"promptFragmentCount,omitempty"`
	// ToolPermissionCount is the number of tool permission grants contributed.
	ToolPermissionCount int `json:"toolPermissionCount,omitempty"`
	// MCPServerNames lists the names of MCP servers registered by this kit.
	MCPServerNames []string `json:"mcpServerNames,omitempty"`
	// SkillRefs lists the SKILL.md skill references contributed.
	SkillRefs []string `json:"skillRefs,omitempty"`
	// WorkareaCleanDirs is the union of clean_dirs declared by this kit.
	WorkareaCleanDirs []string `json:"workareaCleanDirs,omitempty"`
	// AppliedAt is the RFC3339 timestamp when this kit's provide() ran.
	AppliedAt string `json:"appliedAt"`
}

// ── Audit / Attestation ───────────────────────────────────────────────────────
// Types derived from 001-layered-execution-model.md Layer 6 and
// 014-tui-operator-surfaces.md §Layer 6 (Policy, Security, Observability).
// Attestation field shape follows the most-cited form across 003 and 006;
// 008 uses the same structure — see PR body for the disambiguation note.

// AttestationKeyAlgorithm is the signing algorithm used for an attestation.
type AttestationKeyAlgorithm string

const (
	// AttestationEd25519 is the EdDSA attestation algorithm.
	AttestationEd25519 AttestationKeyAlgorithm = "ed25519"
	// AttestationECDSA is the ECDSA P-256 attestation algorithm.
	AttestationECDSA AttestationKeyAlgorithm = "ecdsa-p256"
)

// Attestation is a signed proof-of-provenance attached to an audit chain entry.
// Format: `<algorithm>:<base64-signature>` with a truncated fingerprint for display.
// Chosen form cites the `format.AttestationFingerprint` helper in 014 ("ed25519:abc1234…d4f2").
type Attestation struct {
	// KeyAlgorithm is the signing algorithm.
	KeyAlgorithm AttestationKeyAlgorithm `json:"keyAlgorithm"`
	// KeyFingerprint is the truncated public key fingerprint for display (e.g. "abc1234…d4f2").
	KeyFingerprint string `json:"keyFingerprint"`
	// Signature is the base64-encoded signature over the audit entry payload.
	Signature string `json:"signature"`
	// SignedAt is the RFC3339 timestamp of signing.
	SignedAt string `json:"signedAt"`
	// Verified is true when the host has verified the signature at observation time.
	Verified bool `json:"verified"`
}

// AuditChainEntry is a single entry in a session's Layer 6 audit chain.
// Matches the "AuditEntry" TUI primitive in 014 and the Merkle-log row shape
// referenced in 009 §REN-46-49 reframe.
type AuditChainEntry struct {
	// Sequence is the monotonic index of this entry within the session's chain.
	Sequence uint64 `json:"sequence"`
	// EventKind is the Layer 6 event type (e.g. "session-accepted", "workarea-acquired").
	EventKind string `json:"eventKind"`
	// SessionID is the session this event belongs to.
	SessionID string `json:"sessionId"`
	// ActorID is the principal that caused the event (agent ID, daemon ID, user ID, etc.).
	ActorID string `json:"actorId"`
	// Payload is the event-specific data encoded as a JSON object.
	Payload map[string]any `json:"payload,omitempty"`
	// PreviousHash is the Merkle hash of the preceding chain entry, enabling integrity verification.
	PreviousHash string `json:"previousHash"`
	// EntryHash is the Merkle hash of this entry (covering Sequence + EventKind + Payload + PreviousHash).
	EntryHash string `json:"entryHash"`
	// Attestation is the optional cryptographic proof attached to this entry.
	Attestation *Attestation `json:"attestation,omitempty"`
	// OccurredAt is the RFC3339 timestamp of the event.
	OccurredAt string `json:"occurredAt"`
}

// ── StatsResponse extension ───────────────────────────────────────────────────

// StatsResponseV2 extends StatsResponse with per-machine and per-provider
// breakdowns required by the multi-machine fleet view (009 §issue #56) and the
// SandboxProvider awareness panel (009 §issue #59).
//
// We add the new fields as a separate embedded struct to keep the original
// StatsResponse wire-compatible; callers that don't need the new fields can
// continue using StatsResponse directly.
type StatsResponseV2 struct {
	StatsResponse
	// Machines is the per-machine capacity and status breakdown.
	// Empty when the platform has no multi-machine fleet registered.
	Machines []MachineStats `json:"machines,omitempty"`
	// Providers is the per-provider cost and capacity breakdown.
	// Empty when no provider cost data is available.
	Providers []ProviderCost `json:"providers,omitempty"`
}
