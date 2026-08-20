package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/worker"
)

// PollWorkItem mirrors one element of the platform's poll response `work[]`
// array. The platform serves GET /api/workers/<id>/poll and returns:
//
//	{
//	  work: QueuedWork[],
//	  inboxMessages: { [sessionId]: InboxMessage[] },
//	  hasInboxMessages: boolean,
//	  preClaimed: boolean,
//	  claimedSessionIds: string[],
//	  gitCredentials: { token, cloneUrl, expiresAt }[],
//	}
//
// QueuedWork carries the session-spec fields the daemon needs to dispatch a
// session to the spawner. Field names follow the platform wire shape (camelCase).
//
// QueuedAt is a Unix-millisecond epoch number on the wire — the platform's
// QueuedWork interface (@donmai/server work-queue.ts) defines it
// as `queuedAt: number`, and the Redis-stored session payload confirms a
// numeric value (e.g. 1777658441780). v0.4.1 mistakenly typed it as `string`,
// which caused the daemon's poll loop to fail decoding ("cannot unmarshal
// number into Go struct field PollWorkItem.work.queuedAt of type string") and
// silently drop pre-claimed sessions.
type PollWorkItem struct {
	SessionID string `json:"sessionId"`

	// AdmissionReceipt is forwarded opaquely to the per-session runner payload.
	// The daemon does not interpret or reconstruct this closed contract.
	AdmissionReceipt json.RawMessage `json:"admissionReceipt,omitempty"`
	ClaimReceipt     json.RawMessage `json:"claimReceipt,omitempty"`
	EffectiveCell    json.RawMessage `json:"effectiveCell,omitempty"`
	// ExecutionRuntimeBinding is authenticated poll/claim state, independent
	// of duplicated receipt evidence. Receipt-bearing work must bind to this
	// request, the current daemon worker, its placement, and active claim.
	ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding,omitempty"`

	// OperationalPayload is the byte-stable, canonical projection captured
	// from raw poll JSON before typed mirrors can erase present-empty state.
	OperationalPayload json.RawMessage `json:"operationalPayload,omitempty"`

	ProjectID          string            `json:"projectId,omitempty"`
	RepositoryID       string            `json:"repositoryId,omitempty"`
	ProjectName        string            `json:"projectName,omitempty"`
	Repository         string            `json:"repository,omitempty"`
	RequiresRepository bool              `json:"requiresRepository,omitempty"`
	Ref                string            `json:"ref,omitempty"`
	Priority           int               `json:"priority,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	MaxDuration        int               `json:"maxDurationSeconds,omitempty"`
	Resources          *SessionResources `json:"resources,omitempty"`
	QueuedAt           int64             `json:"queuedAt,omitempty"`
	ProjectScope       string            `json:"projectScope,omitempty"`

	// F.2.8 — enriched fields the platform may send so the
	// `donmai agent run` worker has the runner context it needs without
	// requiring a separate platform fetch. Optional during the rollout
	// window; absent fields fall through to the default render path.
	IssueID           string                  `json:"issueId,omitempty"`
	IssueIdentifier   string                  `json:"issueIdentifier,omitempty"`
	LinearSessionID   string                  `json:"linearSessionId,omitempty"`
	ProviderSessionID string                  `json:"providerSessionId,omitempty"`
	OrganizationID    string                  `json:"organizationId,omitempty"`
	WorkType          string                  `json:"workType,omitempty"`
	PromptContext     string                  `json:"promptContext,omitempty"`
	Body              string                  `json:"body,omitempty"`
	Title             string                  `json:"title,omitempty"`
	MentionContext    string                  `json:"mentionContext,omitempty"`
	ParentContext     string                  `json:"parentContext,omitempty"`
	Branch            string                  `json:"branch,omitempty"`
	ResolvedProfile   *SessionResolvedProfile `json:"resolvedProfile,omitempty"`
	ModelProfile      *SessionModelProfile    `json:"modelProfile,omitempty"`

	// CredentialRequirements, Harness, and ServingHost are additive top-level
	// projections accepted for producers that stamp resolved execution metadata
	// beside resolvedProfile. When absent, the conversion helpers fall back to
	// the same fields nested inside ResolvedProfile. CredentialRequirements
	// contains environment-variable names only, never values.
	CredentialRequirements []CredentialEnvRequirement `json:"credentialRequirements,omitempty"`
	Harness                string                     `json:"harness,omitempty"`
	ServingHost            string                     `json:"servingHost,omitempty"`

	// Phase 2 stage-driven SDLC fields. Populated
	// by the platform's `agent.dispatch_stage` action; absent when the
	// work was queued by the legacy `agent.dispatch_to_queue` action.
	// Round-trip opaquely on the QueuedWork JSON; the daemon forwards
	// them onto SessionDetail without interpreting them.
	StagePrompt        string           `json:"stagePrompt,omitempty"`
	StageID            string           `json:"stageId,omitempty"`
	StageBudget        *PollStageBudget `json:"stageBudget,omitempty"`
	StageLifecycle     map[string]any   `json:"stageLifecycle,omitempty"`
	StageSourceEventID string           `json:"stageSourceEventId,omitempty"`

	// SystemPromptOverride is the per-session platform-supplied system
	// prompt that replaces the runner's default system_base.tmpl render
	// when non-empty, after the runner's immutable content-safety preamble.
	// The leaf consumer at `prompt/builder.go` already reads
	// `qw.SystemPromptOverride`; this struct field is the wire-
	// shape forwarder. Without it Go's strict JSON decoder drops the
	// platform's emit (unknown-field discard) — backlog-writer
	// sessions fell through to system_base.tmpl and produced developer-
	// style behavior (`pnpm af-linear`, Bash/Write/Edit churn).
	SystemPromptOverride string `json:"systemPromptOverride,omitempty"`

	// Kits is the platform-resolved kit toolchain demand (KITS PIVOT #3).
	// The platform composes the agent composition's KitRef[] into a
	// kit.ToolchainDemand and threads it on the poll payload so the daemon
	// can forward it onto SessionDetail without interpreting it. The runner
	// runs the demand's toolchain_install + post_acquire AFTER repo clone
	// (runner/loop.go step 2b). Without this field Go's strict JSON decoder
	// would silently drop the platform's emit — the same wire-gap that bit
	// SystemPromptOverride (v0.9.3). Opaque forwarder only.
	Kits *kit.ToolchainDemand `json:"kits,omitempty"`

	// DisallowedTools is the platform-supplied set of tool-name patterns
	// the credential-injection layer stamps onto QueuedWork via
	// stampCredSurfaceDisallowedTools(). Without this field Go's strict
	// JSON decoder silently drops the platform's emit; the runner's
	// spec_translation.go then never appends the per-workType restrictions
	// to Spec.DisallowedTools. Mirror of the v0.9.3 SystemPromptOverride
	// fix — opaque forwarder only, no new logic.
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// ── WS5 agent-card → runner fidelity fields ─────────────────────────
	//
	// AllowedTools, McpServers, and Skills carry the resolved agent card's
	// tool-allowlist, MCP servers, and inline skills. The daemon forwards
	// them opaquely onto SessionDetail — it never interprets them. McpServers
	// and Skills use daemon-LOCAL mirror structs (PollMCPServer / PollSkill)
	// so the daemon does not depend on the runner/prompt/agent packages
	// (cardinal package-architecture rule: poll.go stays import-light — same
	// rule that produced PollStageBudget / PollInterviewBudget). Without these
	// fields Go's strict JSON decoder silently drops the platform's emit — the
	// v0.9.3 SystemPromptOverride wire-gap precedent.

	// AllowedTools is the platform-supplied agent-card tool allowlist.
	// Forwarded opaquely; absent/empty is safe (omitempty).
	AllowedTools []string `json:"allowedTools,omitempty"`

	// McpServers is the platform-supplied agent-card MCP server set.
	// Forwarded opaquely via the PollMCPServer mirror.
	McpServers []PollMCPServer `json:"mcpServers,omitempty"`

	// Skills is the platform-supplied agent-card inline skill set.
	// Forwarded opaquely via the PollSkill mirror.
	Skills []PollSkill `json:"skills,omitempty"`

	// MergeQueueLanding is the coordinator's per-org merge-queue landing flag for
	// THIS session's org, stamped per-item on the poll payload. true ⇒ the runner
	// DEFERS the acceptance Delivered→Accepted promotion to the landing finalizer;
	// false/absent ⇒ direct transition. *bool so absent (older coordinator) is
	// distinguishable from explicit false, preserving the legacy capability fallback.
	MergeQueueLanding *bool `json:"mergeQueueLanding,omitempty"`

	// InjectedPoolID is the non-secret pool accounting sentinel the
	// platform stamps for metered and shared auth modes:
	// "metered_pool_<provider>" or "shared_pool_<provider>". Safe at
	// rest — this is a billing tag, not a credential. Absent for
	// byok/host-session/local sessions.
	//
	// Model credential keys are NOT on the wire payload (Option C of the
	// credential-injection design); they reach the agent child exclusively
	// via the daemon's OnPreSpawn → credential-snapshot secure path. Only
	// the accounting sentinel rides the queue.
	InjectedPoolID string `json:"injectedPoolId,omitempty"`

	// McpAuthToken is an opaque platform-supplied bearer for the platform's
	// per-session MCP gateway, and for nothing else. The daemon never parses,
	// validates, or logs its value — it forwards the string onto SessionDetail
	// exactly as received.
	//
	// It is scoped and lived by the platform that minted it; the daemon must
	// not assume any relationship to AuthToken, which stays the bearer for
	// heartbeat, result-post, and the rest of the worker surface. Absent (a
	// platform that mints none — self-hosted or older) is normal and safe: the
	// runner falls back to the worker bearer, which is the standalone contract.
	McpAuthToken string `json:"mcpAuthToken,omitempty"`

	// McpAuthTokenExpiresAt is the RFC3339 UTC instant McpAuthToken stops being
	// accepted. ADVISORY ONLY: it exists so an operator can see the cliff
	// coming in the logs. Nothing may gate behaviour on it. Absent whenever
	// McpAuthToken is absent.
	McpAuthTokenExpiresAt string `json:"mcpAuthTokenExpiresAt,omitempty"`

	// MemoryBlock is the dispatch-time agent-memory context the platform
	// folds into the system prompt (Wave 3 memory-inject v1). The daemon
	// forwards it opaquely onto SessionDetail; the runner's prompt builder
	// appends it. Without this field Go's strict JSON decoder silently
	// drops the platform's emit — the same wire-gap that bit
	// SystemPromptOverride (v0.9.3). Opaque forwarder only.
	MemoryBlock string `json:"memoryBlock,omitempty"`

	// ── Interactive run-mode fields (Wave 2 donmai wire-plumbing) ─
	//
	// Mode is the run-mode discriminant ("" = headless, "interview" =
	// inject-driven interview loop, "interactive" = live PTY session).
	// Forwarded opaquely onto SessionDetail so the runner can branch on it.
	// Opaque forwarder only — same pattern as
	// SystemPromptOverride / Kits / DisallowedTools.
	Mode string `json:"mode,omitempty"`

	// InitialPrompt is the optional first terminal input for a
	// mode:"interactive" session. The daemon forwards it opaquely and never
	// folds it into headless or interview prompt construction. Empty/absent is
	// omitted and preserves the pre-field wire shape.
	InitialPrompt string `json:"initialPrompt,omitempty"`

	// RecordingEnabled is the platform's host-side recording policy decision
	// for a mode:"interactive" session. nil/absent means the platform made no
	// decision (a standalone run, or a platform that predates this field) —
	// the runner defaults to allowed, the same mixed-version-safe default as
	// every other opaque forwarder here. Explicit false disables the on-disk
	// asciinema-v2 cast for the session; explicit true allows it. The daemon
	// does not interpret this value — opaque forwarder only (same pattern as
	// Mode / InitialPrompt / MemoryBlock).
	RecordingEnabled *bool `json:"recordingEnabled,omitempty"`

	// TerminalWorkareaLease is the optional provider-neutral request to retain a
	// successful terminal workarea until semantic acknowledgement or bounded
	// expiry. Nil preserves the legacy immediate-teardown behavior.
	TerminalWorkareaLease *workarea.TerminalLeaseRequest `json:"terminalWorkareaLease,omitempty"`

	// InterviewBudget is the per-interview wall-clock + idle-grace cap.
	// Forwarded opaquely onto SessionDetail. Nil/absent is safe and
	// backward-compatible. Opaque forwarder only.
	InterviewBudget *PollInterviewBudget `json:"interviewBudget,omitempty"`

	// InterviewDefinition is the compiled interview definition JSON. The
	// daemon does not parse it; the runner's interview loop consumes it.
	// Forwarded opaquely as json.RawMessage so the strict decoder never
	// drops it. Opaque forwarder only.
	InterviewDefinition json.RawMessage `json:"interviewDefinition,omitempty"`

	// ── W3C trace-context correlation (REN-2649) ─────────────────────────
	//
	// Additive platform-dispatch fields per
	// src/lib/observability/trace-context.ts. Opaque forwarders only.
	Traceparent      string `json:"traceparent,omitempty"`
	Tracestate       string `json:"tracestate,omitempty"`
	SessionStorageID string `json:"sessionStorageId,omitempty"`
	SessionPublicID  string `json:"sessionPublicId,omitempty"`
	TrackerSessionID string `json:"trackerSessionId,omitempty"`
}

// UnmarshalJSON captures the operational projection at the authenticated poll
// boundary. This is intentionally the only place source JSON is projected.
func (item *PollWorkItem) UnmarshalJSON(raw []byte) error {
	type pollWorkItemAlias PollWorkItem
	var decoded pollWorkItemAlias
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*item = PollWorkItem(decoded)
	projected, err := executioncell.ProjectOperationalPayload(raw)
	if err != nil {
		return err
	}
	if len(item.OperationalPayload) > 0 {
		canonical, err := executioncell.NormalizeOperationalPayload(item.OperationalPayload)
		if err != nil {
			return err
		}
		if !bytes.Equal(canonical, projected) {
			return errors.New("daemon poll: operationalPayload does not match raw poll item")
		}
		item.OperationalPayload = canonical
		return nil
	}
	item.OperationalPayload = projected
	return nil
}

// PollInterviewBudget mirrors prompt.InterviewBudget for the daemon
// package. The daemon does not import the runner or prompt packages
// (cardinal package-architecture rule); this struct carries the budget
// opaquely so the daemon can forward it onto SessionDetail without
// depending on the prompt package. The runner re-types it into
// prompt.InterviewBudget via detailToQueuedWork.
type PollInterviewBudget struct {
	MaxWallClockSeconds int `json:"maxWallClockSeconds,omitempty"`
	IdleGraceSeconds    int `json:"idleGraceSeconds,omitempty"`
}

// PollStageBudget mirrors the platform's StageBudget shape so the
// daemon can decode + forward it without depending on the runner
// package (cardinal package-architecture rule: daemon does not import
// runner). The runner re-types this into prompt.StageBudget when it
// constructs the QueuedWork.
type PollStageBudget struct {
	MaxDurationSeconds int   `json:"maxDurationSeconds,omitempty"`
	MaxSubAgents       int   `json:"maxSubAgents,omitempty"`
	MaxTokens          int64 `json:"maxTokens,omitempty"`
}

// PollMCPServer mirrors agent.MCPServerConfig for the daemon package so the
// daemon can decode + forward the agent-card MCP set (WS5) without importing
// the agent package (cardinal package-architecture rule: poll.go stays
// import-light — same rule that produced PollStageBudget). The runner
// re-types this into agent.MCPServerConfig in detailToQueuedWork.
//
// JSON tags are byte-identical to agent.MCPServerConfig so the wire shape is
// shared: the transport discriminator is "type" (NOT "transport"), defaulting
// to "stdio" when empty. NOTE (platform mirror): the agreed WS5 shape named
// the discriminator "transport"; the existing agent.MCPServerConfig type
// already uses "type", so the platform emit side must serialize this field as
// "type" to match (delta from the agreed shape).
type PollMCPServer struct {
	Name    string            `json:"name"`
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// PollSkill mirrors prompt.SkillSpec for the daemon package so the daemon can
// decode + forward the agent-card inline skill set (WS5) without importing the
// prompt package. The runner re-types this into prompt.SkillSpec in
// detailToQueuedWork. JSON tags are byte-identical to prompt.SkillSpec.
type PollSkill struct {
	ID              string   `json:"id,omitempty"`
	Body            string   `json:"body,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}

// LandingWorkItem mirrors one element of the orchestrator's poll-response
// `landingWork[]` array. It is a per-(orgId,repoId) TRIGGER to run the landing
// serializer for that tenant — NOT a session. The item never becomes an agent
// session, never enters activeSessions, and never counts toward the agent
// concurrency quota; it is routed out-of-band to the landing handler.
//
// Field names follow the orchestrator wire shape (camelCase). The daemon only
// needs OrgID + RepoID (plus WorkType as a sanity tag) to locate the tenant's
// landing state and run the serializer. The orchestrator also emits a
// batchJobId (the stable per-(orgId,repoId) claim key) and a contractVersion;
// batchJobId is decoded for completeness, and contractVersion is intentionally
// omitted to avoid number/string decode ambiguity — it is unused by the daemon
// and harmlessly discarded by the JSON decoder (PollResponse does NOT use
// DisallowUnknownFields, mirroring the InboxMessages forward-compat note above).
type LandingWorkItem struct {
	BatchJobID string `json:"batchJobId,omitempty"`
	WorkType   string `json:"workType,omitempty"`
	OrgID      string `json:"orgId,omitempty"`
	RepoID     string `json:"repoId,omitempty"`
}

// PollResponse is the body of GET /api/workers/<id>/poll. Only the fields the
// daemon currently consumes are decoded; unknown fields are ignored.
type PollResponse struct {
	Work             []PollWorkItem `json:"work"`
	HasInboxMessages bool           `json:"hasInboxMessages,omitempty"`
	// InboxMessages is the per-session inbox the platform piggybacks on the
	// poll response: a map of sessionId → queued messages for that running
	// session. Interactive-interview user turns (kind="user") arrive here as
	// well as on the heartbeat lock-refresh piggyback; the daemon routes a
	// kind="user" inbox message to the running session via OnInbox so the
	// runner can inject it (CONTRACT-FREEZE §3). Without this
	// field Go's strict JSON decoder silently drops the platform's emit —
	// the messages were decoded into nothing and never routed.
	InboxMessages     map[string][]InboxMessage `json:"inboxMessages,omitempty"`
	PreClaimed        bool                      `json:"preClaimed,omitempty"`
	ClaimedSessionIDs []string                  `json:"claimedSessionIds,omitempty"`

	// LandingWork is the per-(orgId,repoId) landing-trigger lane. The
	// orchestrator emits `landingWork[]` to tell a capable worker to run the
	// landing serializer for a tenant; pollOnce routes each item through
	// OnWork as a synthesized LandingWorkType item, so the existing landing-run
	// branch (out-of-band, never a session) handles it. Previously the
	// orchestrator emitted landingWork[] but the daemon decoded only work[],
	// so the triggers were silently dropped and the per-tenant serializer
	// never started — that gap is what this field closes. Absent/empty is
	// safe (no producer emits it unless landing triggers are staged).
	LandingWork []LandingWorkItem `json:"landingWork,omitempty"`

	// KgExtractWork is the non-agent kg-extraction lane, a sibling of work[]
	// and landingWork[] on the same poll response. Each item is a batch job,
	// never a session: it is not admitted to the spawner, does not count toward
	// the agent quota, and never reaches runner.Run.
	//
	// The coordinator only emits it to a worker whose registration advertised
	// the kg-extraction capability, and emitting it POPS the item off the org
	// queue — so decoding this field without executing it destroys the work.
	// That is why NewPollService fills a nil OnKgExtractWork with the default
	// executor lane: no poll path can decode an item it is unable to run.
	KgExtractWork []worker.BatchWorkItem `json:"kgExtractWork,omitempty"`
}

// InboxMessage is one queued message for a running session, delivered in
// the poll response's inboxMessages map. The shape mirrors the heartbeat
// InjectPayload's interview fields so the same kind/turnId discriminants
// drive routing on both transports (CONTRACT-FREEZE §3).
//
// Kind discriminates the message type:
//   - "" or "memory" — agent-memory block (existing behaviour)
//   - "user"         — a user-turn message from an interactive interview
//
// TurnID carries the turn correlation id for kind="user" messages
// (stamped by the platform's enqueueUserTurnInject). DeliveryID is the
// platform-side queue row id the daemon/runner echoes back to stop
// re-delivery.
type InboxMessage struct {
	DeliveryID string `json:"deliveryId,omitempty"`
	Text       string `json:"text"`
	Kind       string `json:"kind,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
}

// PollHTTPError is returned by callPollEndpoint for non-2xx responses so the
// loop can branch on the HTTP status (401 → re-register).
type PollHTTPError struct {
	Status int
	Body   string
}

func (e *PollHTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// PollOptions configure a single poll loop run.
type PollOptions struct {
	WorkerID        string
	OrchestratorURL string
	RuntimeJWT      string
	IntervalSeconds int

	// HTTPClient is the client used for poll calls. Defaults to a 30s-timeout
	// http.Client.
	HTTPClient *http.Client
	// LogWarn is called for transient poll failures. Defaults to no-op.
	LogWarn func(format string, args ...any)
	// LogInfo is called when work is dispatched / re-register fires.
	LogInfo func(format string, args ...any)
	// OnWork is invoked for each item returned in the work[] slice. Errors are
	// logged at warn and do not stop the loop. Required.
	OnWork func(item PollWorkItem) error
	// OnInbox is invoked for each inbox message routed to a running
	// session. The poll loop decodes the inboxMessages map and calls
	// OnInbox(sessionID, msg) for every message; the daemon wires this to
	// forward the message into the running session — for a kind="user"
	// interview turn, into the runner's live agent.Handle.Inject path. Optional:
	// when nil the messages are decoded but not routed (back-compat — the
	// heartbeat lock-refresh piggyback remains the primary user-turn transport
	// per CONTRACT-FREEZE §3, so a nil OnInbox is not a hard drop). Errors are
	// logged at warn and do not stop the loop.
	OnInbox func(sessionID string, msg InboxMessage) error
	// OnReregister is called when the orchestrator rejects a poll: HTTP 401
	// (runtime JWT expired) or HTTP 404 (worker not recognised).
	// Implementations return the worker id + runtime token to continue with —
	// USUALLY THE SAME worker id, because the recovery path re-presents the
	// existing registration rather than replacing it. The poll loop swaps
	// credentials and continues. Returning an error logs and the loop retries
	// on the next tick.
	//
	// reason is the structured failure reason ("worker-not-found",
	// "runtime-token-expired", "unauthorized", "auth-failure"). Pass it
	// through to RefreshRuntimeToken, which decides recovery. Note that
	// "worker-not-found" does NOT by itself mean the registration is gone —
	// see RefreshRuntimeToken for why treating it that way made the daemon
	// re-register on every tick.
	OnReregister func(ctx context.Context, reason string) (workerID, runtimeJWT string, err error)

	// ClaimSuspended reports whether this host must currently stop claiming
	// NEW work, plus a short human-readable reason for the transition log.
	// It is consulted once per tick, immediately before the poll request —
	// the single point at which this daemon can take on work — so a true
	// result means no work item can be handed to this host at all.
	//
	// Suspension is claim-side only: in-flight sessions keep running, the
	// spawner keeps accepting nothing new but killing nothing either, and
	// the heartbeat loop keeps beating (it carries the in-flight sessions'
	// lock refresh and user-turn piggyback, so a suspended daemon still
	// serves the work it already owns).
	//
	// Nil ⇒ never suspended, byte-identical to the pre-gate behaviour. The
	// callback MUST NOT block: it runs on the poll goroutine.
	ClaimSuspended func() (suspended bool, reason string)

	// OnKgExtractWork executes ONE claimed kgExtractWork[] item. It runs on its
	// own goroutine (bounded by maxConcurrentKgExtract), never on the poll
	// goroutine, so a long extraction cannot stall session claiming or inbox
	// routing. Errors are logged at warn and never stop the loop.
	//
	// Nil is NOT "drop the item": NewPollService substitutes the default
	// kgextract lane, because a claimed item has already been popped off the
	// coordinator's queue and would otherwise be lost. Set it only to override
	// the executor (tests, or an embedder with its own emitter wiring).
	OnKgExtractWork worker.BatchHandler

	// WorkerVersion stamps the kg-extraction executor's log/result context.
	// Empty falls back to the executor default ("dev").
	WorkerVersion string
}

// maxConcurrentKgExtract bounds how many claimed kg-extraction items one poll
// service executes at a time. The coordinator hands out at most a couple per
// tick; anything above the bound waits on the semaphore inside its own
// goroutine (cheap) instead of spawning provider processes without limit. A
// waiting item is abandoned only when the service is shutting down.
const maxConcurrentKgExtract = 2

// PollService manages the periodic poll goroutine. Like HeartbeatService it is
// safe to Start / Stop multiple times; consecutive Starts are idempotent.
type PollService struct {
	opts PollOptions

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	running  bool
	workerID string // mutable: refreshed by OnReregister
	jwt      string // mutable: refreshed by OnReregister
	// claimsSuspended latches the last observed ClaimSuspended result so the
	// loop logs one line per state TRANSITION rather than one per tick.
	claimsSuspended bool

	// kgWG tracks in-flight kg-extraction executions so Stop joins them before
	// publishing completion (the loop goroutine waits on it before closing
	// done). kgSem bounds their concurrency.
	kgWG  sync.WaitGroup
	kgSem chan struct{}
}

// NewPollService constructs a PollService from opts. OnWork must be non-nil.
func NewPollService(opts PollOptions) *PollService {
	if opts.IntervalSeconds <= 0 {
		opts.IntervalSeconds = 5 // platform default in ms is 5000
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.LogWarn == nil {
		opts.LogWarn = func(string, ...any) {}
	}
	if opts.LogInfo == nil {
		opts.LogInfo = func(string, ...any) {}
	}
	// A poll response that carries kgExtractWork[] has already had those items
	// popped off the coordinator's queue for this worker. Decoding them without
	// an executor would destroy the work, so the lane is filled in here rather
	// than left to each embedder: every PollService that can DECODE an item can
	// also RUN it. (The coordinator still only sends items to a worker whose
	// registration advertised kgextract's capability tag.)
	if opts.OnKgExtractWork == nil {
		opts.OnKgExtractWork = kgextract.NewLane(kgextract.Options{
			WorkerVersion:   opts.WorkerVersion,
			PlatformBaseURL: opts.OrchestratorURL,
		}).Handler
	}
	return &PollService{
		opts:     opts,
		workerID: opts.WorkerID,
		jwt:      opts.RuntimeJWT,
		kgSem:    make(chan struct{}, maxConcurrentKgExtract),
	}
}

// CurrentCredentials returns the worker id and runtime JWT currently in
// use. Mirrors HeartbeatService.CurrentCredentials.
func (p *PollService) CurrentCredentials() (workerID, runtimeJWT string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workerID, p.jwt
}

// SetCredentials swaps the worker id + runtime JWT this service presents.
// Called by the daemon whenever ANOTHER path re-minted credentials (the
// proactive token refresher, or the heartbeat's reactive refresh) so the
// poll loop does not have to burn its own 401 round-trip — and its own log
// cycle — to discover them. Empty values are ignored.
func (p *PollService) SetCredentials(workerID, jwt string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if workerID != "" {
		p.workerID = workerID
	}
	if jwt != "" {
		p.jwt = jwt
	}
}

// Start launches the poll goroutine. Subsequent calls are no-ops.
func (p *PollService) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.running = true
	p.mu.Unlock()

	go func() {
		defer close(done)
		p.loop(ctx)
		// Join in-flight kg-extraction executions before publishing completion:
		// each one owns a claimed item and posts its result, so a stop that
		// reported "done" while they ran would look like a clean shutdown that
		// silently abandoned claimed work. Callers are still never held past
		// their own deadline — StopContext selects on the caller's context.
		p.kgWG.Wait()
	}()
}

// Stop terminates the poll goroutine. Safe to call multiple times.
func (p *PollService) Stop() {
	_ = p.StopContext(context.Background())
}

// beginStop cancels polling and returns the current loop completion signal
// without waiting for it. Repeated calls are idempotent and reuse the same done
// channel, letting a shutdown start every barrier before joining any one of
// them.
func (p *PollService) beginStop() <-chan struct{} {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	if p.running {
		p.running = false
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return done
}

// StopContext terminates the poll goroutine without allowing a blocked OnWork
// callback to outlive the caller's shutdown deadline. The callback still owns
// its own eventual return; a later caller can join the same done channel.
func (p *PollService) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := p.beginStop()
	if done == nil {
		return nil
	}
	// A completed loop is a successful idempotent stop even when the caller's
	// context was canceled before it asked to join it.
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsRunning reports whether the poll goroutine is active.
func (p *PollService) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *PollService) loop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(p.opts.IntervalSeconds) * time.Second)
	defer tick.Stop()
	// Immediate first poll so a worker comes online and requests work without
	// waiting one full interval.
	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.pollOnce(ctx)
		}
	}
}

// ClaimsSuspended reports the latched claim-gate state: true between the tick
// that observed a stop signal and the tick that observed its clearance. False
// when no ClaimSuspended callback is wired.
func (p *PollService) ClaimsSuspended() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claimsSuspended
}

// claimGateBlocks consults the ClaimSuspended callback and returns true when
// this tick must not claim. It logs exactly once per state transition —
// entering suspension and leaving it — so a long outage costs two lines, not
// one per tick.
//
// Lock discipline: the callback is invoked with NO PollService lock held (it
// reaches into the daemon's own mutex-guarded status snapshot, and holding
// p.mu across a foreign lock would invert lock order against any caller that
// takes the daemon lock first). p.mu is taken only to compare-and-latch the
// boolean afterwards.
func (p *PollService) claimGateBlocks() bool {
	if p.opts.ClaimSuspended == nil {
		return false
	}
	suspended, reason := p.opts.ClaimSuspended()

	p.mu.Lock()
	changed := p.claimsSuspended != suspended
	p.claimsSuspended = suspended
	p.mu.Unlock()

	if changed {
		if suspended {
			if reason == "" {
				reason = "the control plane reports this host must stop claiming"
			}
			p.opts.LogWarn(
				"daemon poll: suspending new-work claims (%s) — in-flight sessions continue; claiming resumes automatically",
				reason,
			)
		} else {
			p.opts.LogInfo("daemon poll: resuming new-work claims — host status is ok again")
		}
	}
	return suspended
}

func (p *PollService) pollOnce(ctx context.Context) {
	p.mu.Lock()
	workerID := p.workerID
	jwt := p.jwt
	p.mu.Unlock()
	if workerID == "" {
		return
	}
	// Claim gate. Evaluated BEFORE the request goes out: the poll response is
	// what binds a work item to this host, so skipping the request is the only
	// way to decline work without a claim-then-NACK round trip. Nothing else is
	// touched — running sessions, the spawner, and the heartbeat loop are
	// unaffected, and the next tick re-evaluates, so recovery is automatic.
	if p.claimGateBlocks() {
		return
	}
	resp, err := callPollEndpoint(ctx, p.opts.HTTPClient, p.opts.OrchestratorURL, workerID, jwt)
	if err == nil {
		if len(resp.Work) > 0 {
			p.opts.LogInfo("daemon poll: %d work item(s) received", len(resp.Work))
		}
		for _, item := range resp.Work {
			if herr := p.opts.OnWork(item); herr != nil {
				p.opts.LogWarn("poll handler error for session %s: %v", item.SessionID, herr)
			}
		}
		// Route landing-trigger items (the per-(orgId,repoId) landing lane).
		// The orchestrator emits landingWork[] separately from work[]; each
		// item is a TRIGGER, not a session. We synthesize a LandingWorkType
		// poll item and push it through the SAME OnWork callback so the
		// existing landing-run branch in the OnWork handler routes it
		// out-of-band to the landing serializer (never a session, never
		// counted toward the agent quota). Best-effort: a handler error is
		// logged at warn and does not stop the loop, mirroring the work[] loop.
		if len(resp.LandingWork) > 0 {
			p.opts.LogInfo("daemon poll: %d landing-trigger item(s) received", len(resp.LandingWork))
		}
		for _, lw := range resp.LandingWork {
			if lw.OrgID == "" || lw.RepoID == "" {
				p.opts.LogWarn("poll: dropping landing-trigger item with missing orgId/repoId (orgId=%q repoId=%q)", lw.OrgID, lw.RepoID)
				continue
			}
			item := PollWorkItem{
				WorkType:       LandingWorkType,
				OrganizationID: lw.OrgID,
				Repository:     lw.RepoID,
			}
			if herr := p.opts.OnWork(item); herr != nil {
				p.opts.LogWarn("poll handler error for landing-trigger %s/%s: %v", lw.OrgID, lw.RepoID, herr)
			}
		}
		// Route the kg-extraction lane. These items are batch jobs, not
		// sessions: they never reach the spawner, never count toward the agent
		// quota, and never touch runner.Run. The coordinator popped them off
		// the org queue to hand them here, so each one MUST be executed —
		// dispatchKgExtract runs it on its own goroutine so a multi-minute
		// extraction cannot stall session claiming on this loop.
		if len(resp.KgExtractWork) > 0 {
			p.opts.LogInfo("daemon poll: %d kg-extraction item(s) received", len(resp.KgExtractWork))
		}
		for _, kw := range resp.KgExtractWork {
			p.dispatchKgExtract(ctx, kw)
		}
		// Route any inbox messages (interactive-interview user turns +
		// memory blocks) to the running sessions. Previously the
		// inboxMessages map was not even decoded, so a kind="user" turn
		// delivered on the poll transport was silently discarded. We now
		// decode it and forward each message via OnInbox so the daemon can
		// inject it into the live session's agent.Handle.
		p.routeInbox(resp.InboxMessages)
		return
	}
	if isPollAuthFailure(err) && p.opts.OnReregister != nil {
		// Surface the structured [runtime-token] event mirroring the
		// heartbeat path — observers see one log line per
		// cycle on either path.
		reason := pollAuthFailureReason(err)
		slog.Info(
			"[runtime-token]",
			"event", "auth-failure-detected",
			"path", "poll",
			"reason", reason,
		)
		// Routine + self-healing (the refresh fires right below), so Info
		// not Warn — with the proactive refresher running this path is the
		// backstop, not the steady state.
		p.opts.LogInfo("daemon poll rejected (%v) — refreshing runtime token (reason=%s)", err, reason)
		newWorkerID, newJWT, regErr := p.opts.OnReregister(ctx, reason)
		if regErr != nil {
			p.opts.LogWarn("daemon poll runtime-token refresh failed: %v", regErr)
			return
		}
		p.mu.Lock()
		p.workerID = newWorkerID
		p.jwt = newJWT
		p.mu.Unlock()
		return
	}
	p.opts.LogWarn("daemon poll failed: %v", err)
}

// dispatchKgExtract executes one claimed kg-extraction item off the poll
// goroutine. The item is already claimed, so it is never skipped for load: when
// the concurrency bound is saturated the goroutine waits on the semaphore and
// runs as soon as a slot frees. Only shutdown abandons a waiting item, and that
// is logged at warn — the coordinator's claim lease expires and the item is
// re-staged for a later poll.
func (p *PollService) dispatchKgExtract(ctx context.Context, item worker.BatchWorkItem) {
	handler := p.opts.OnKgExtractWork
	if handler == nil {
		// Unreachable through NewPollService (it fills the default lane). Kept
		// as a loud guard: a zero-value PollService must complain rather than
		// silently drop a claimed item.
		p.opts.LogWarn("daemon poll: kg-extraction item %q received with no executor wired; item dropped",
			item.BatchJobID)
		return
	}
	p.kgWG.Add(1)
	go func() {
		defer p.kgWG.Done()
		select {
		case p.kgSem <- struct{}{}:
			defer func() { <-p.kgSem }()
		case <-ctx.Done():
			p.opts.LogWarn("daemon poll: kg-extraction item %q abandoned before execution (poll service stopping)",
				item.BatchJobID)
			return
		}
		if herr := handler(ctx, item); herr != nil {
			p.opts.LogWarn("daemon poll: kg-extraction item %q failed: %v", item.BatchJobID, herr)
		}
	}()
}

// routeInbox forwards every decoded inbox message to OnInbox, keyed by the
// running session id. A nil OnInbox or empty map is a no-op (the heartbeat
// lock-refresh piggyback remains the primary user-turn transport — a nil
// OnInbox is back-compat, not a hard drop). Whitespace-only messages are
// skipped. Errors from OnInbox are logged at warn and never stop the loop.
//
// CONTRACT-FREEZE §3: the kind="user" inbox message is the
// interactive-interview user turn; the daemon's OnInbox implementation
// routes it into the running session's runner so handle.Inject (claude
// --resume) delivers it as the next turn.
func (p *PollService) routeInbox(inbox map[string][]InboxMessage) {
	if len(inbox) == 0 || p.opts.OnInbox == nil {
		return
	}
	for sessionID, msgs := range inbox {
		for _, msg := range msgs {
			if strings.TrimSpace(msg.Text) == "" {
				continue
			}
			kind := msg.Kind
			if kind == "" {
				kind = "memory"
			}
			p.opts.LogInfo("daemon poll: routing inbox message to session %s (kind=%s, turnId=%s)",
				sessionID, kind, msg.TurnID)
			if herr := p.opts.OnInbox(sessionID, msg); herr != nil {
				p.opts.LogWarn("inbox route error for session %s (kind=%s): %v", sessionID, kind, herr)
			}
		}
	}
}

// callPollEndpoint issues a GET against /api/workers/<id>/poll with the given
// runtime JWT and returns the decoded response. Non-2xx responses surface as
// *PollHTTPError so the loop can switch on the status.
func callPollEndpoint(ctx context.Context, client *http.Client, orchestratorURL, workerID, jwt string) (*PollResponse, error) {
	if workerID == "" {
		return nil, errors.New("no worker id")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimRight(orchestratorURL, "/") + "/api/workers/" + workerID + "/poll"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, &PollHTTPError{Status: res.StatusCode, Body: strings.TrimSpace(string(errBuf))}
	}
	var resp PollResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	return &resp, nil
}

// callNackEndpoint POSTs to /api/sessions/<id>/nack so the orchestrator
// releases the claim and re-queues the work item when the daemon decides
// locally that it cannot execute a session it just claimed (allowlist
// mismatch, spawn failure, drain in flight, …). Without this NACK, a
// rejected session sits in `claimed` state until the orchestrator's
// stale-claim sweep eventually reclaims it — minutes of latency, and
// the session looks healthy to operators in the meantime.
//
// The body shape mirrors the orchestrator's NackRequestBody:
//
//	{ "workerId": "wkr_…", "reason": "<short>", "work": <queued work> }
//
// `work` must carry the five fields the orchestrator validates as
// `QueuedWork` (sessionId, issueId, issueIdentifier, priority,
// queuedAt). PollWorkItem already JSON-marshals to a superset of that
// shape, so we can pass it through verbatim.
//
// NACK errors are best-effort: returning an error here lets the caller
// log it, but the local rejection has already happened so a NACK
// failure is not fatal.
func callNackEndpoint(
	ctx context.Context,
	client *http.Client,
	orchestratorURL, sessionID, workerID, runtimeJWT, reason string,
	work *PollWorkItem,
) error {
	if sessionID == "" {
		return errors.New("nack: session id required")
	}
	if workerID == "" {
		return errors.New("nack: worker id required")
	}
	if work == nil {
		return errors.New("nack: original work item required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body := struct {
		WorkerID string        `json:"workerId"`
		Reason   string        `json:"reason,omitempty"`
		Work     *PollWorkItem `json:"work"`
	}{
		WorkerID: workerID,
		Reason:   reason,
		Work:     work,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal nack body: %w", err)
	}
	url := strings.TrimRight(orchestratorURL, "/") + "/api/sessions/" + sessionID + "/nack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build nack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+runtimeJWT)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("nack: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("nack rejected: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(errBuf)))
	}
	return nil
}

// isPollAuthFailure returns true for the HTTP statuses that mean "this poll
// was rejected on identity/credential grounds and the credentials must be
// refreshed": 401 (Unauthorized) and 404 (Worker not found). Refreshing does
// not imply re-registering — see RefreshRuntimeToken.
func isPollAuthFailure(err error) bool {
	var hErr *PollHTTPError
	if errors.As(err, &hErr) {
		return hErr.Status == http.StatusUnauthorized || hErr.Status == http.StatusNotFound
	}
	return false
}

// pollAuthFailureReason mirrors heartbeat.authFailureReason for the
// poll path: classifies a 401/404 into a short structured reason for
// the [runtime-token] log line. Uses the platform's specific
// "Runtime token expired" message as the smoking-gun signal for the
// runtime-token refresh path.
func pollAuthFailureReason(err error) string {
	var hErr *PollHTTPError
	if errors.As(err, &hErr) {
		switch hErr.Status {
		case http.StatusUnauthorized:
			if strings.Contains(hErr.Body, "Runtime token expired") {
				return "runtime-token-expired"
			}
			return "unauthorized"
		case http.StatusNotFound:
			return reasonWorkerNotFound
		}
	}
	return "auth-failure"
}

// resolveProjectFromAllowlist looks up a daemon ProjectConfig by the value
// the platform sent as the poll-item project identifier (a Linear project
// slug, the GitHub URL, or a suffix-equivalent of either).
//
// The match logic mirrors WorkerSpawner.findProjectLocked so
// the SessionDetail.repository the runner sees is the SAME entry the
// spawner will later validate the SessionSpec against:
//
//   - exact match on p.ID (the slug, e.g. "smoke-alpha")
//   - exact match on p.Repository (the URL)
//   - URL-suffixes ".../<id>" or ".../<repository>"
//
// Returns (nil, false) when the value is empty or no entry matches.
func resolveProjectFromAllowlist(value string, projects []ProjectConfig) (*ProjectConfig, bool) {
	if value == "" {
		return nil, false
	}
	for i := range projects {
		p := &projects[i]
		if p.ID == value ||
			p.Repository == value ||
			strings.HasSuffix(value, "/"+p.Repository) ||
			strings.HasSuffix(p.Repository, "/"+value) {
			return p, true
		}
	}
	return nil, false
}

// pollItemCredentialMetadata resolves the additive, non-secret execution-cell
// metadata accepted in either top-level projected form or nested inside
// ResolvedProfile. A non-nil top-level CredentialRequirements slice is
// authoritative even when empty, preserving absent-vs-present-empty semantics;
// strings use their non-empty top-level projection and otherwise fall back.
func pollItemCredentialMetadata(item PollWorkItem) (requirements []CredentialEnvRequirement, harness, servingHost string) {
	requirements = item.CredentialRequirements
	harness = item.Harness
	servingHost = item.ServingHost
	if profile := item.ResolvedProfile; profile != nil {
		if requirements == nil {
			requirements = profile.CredentialRequirements
		}
		if harness == "" {
			harness = profile.Harness
		}
		if servingHost == "" {
			servingHost = profile.ServingHost
		}
	}
	return requirements, harness, servingHost
}

// PollItemToSessionSpec maps a PollWorkItem to a SessionSpec the
// WorkerSpawner can dispatch.
//
// The platform's QueuedWork wire shape historically carried a
// projectName slug (e.g. "smoke-alpha") with no separate repository
// URL. The runner needs a clone target — a slug is not one. When the
// daemon's project allowlist matches the slug we substitute the URL
// from p.Repository so `git clone <repo>` actually targets a real URL
// instead of failing with "fatal: repository 'smoke-alpha' does not
// exist" (the v0.5.1 failure mode this v0.5.2 hotfix is for).
//
// When no allowlist match exists we fall through to whatever the
// platform sent (preserving prior behaviour) and emit a Warn log so
// operators can see the misconfiguration. The downstream
// WorkerSpawner.findProjectLocked check will reject the spec at
// AcceptWork time, but the explicit log makes the resolution failure
// observable immediately at poll dispatch.
//
// Exported so embedders can drive multi-identity poll loops (e.g. a
// downstream embedder that builds a SessionSpec from its own poll
// loop before calling a shared daemon's AcceptWorkWithDetail).
func PollItemToSessionSpec(item PollWorkItem, projects []ProjectConfig) SessionSpec {
	repo, matched := resolveAllowlistedRepo(item, projects)
	credentialRequirements, harness, servingHost := pollItemCredentialMetadata(item)
	spec := SessionSpec{
		SessionID:              item.SessionID,
		ProjectID:              item.ProjectID,
		RepositoryID:           item.RepositoryID,
		Repository:             repo,
		RequiresRepository:     item.RequiresRepository,
		Ref:                    item.Ref,
		Resources:              item.Resources,
		Env:                    item.Env,
		MaxDurationSeconds:     item.MaxDuration,
		CredentialRequirements: credentialRequirements,
		Harness:                harness,
		ServingHost:            servingHost,
		// ── P3 narrow-only gate inputs (ADR-2026-06-06 §5.3) ─────────────
		// Copied through (NOT enforced) so the embedder's OnPreSpawn closure
		// has everything access.ResolveMachineCell needs. WorkType + Mode are
		// top-level on the poll item; Company/Model/AuthMode/PlatformAllowed
		// ride the resolved profile. Every field is absent on a pre-P3 item,
		// so the spec stays byte-identical for the existing fields (identity).
		WorkType:              item.WorkType,
		Mode:                  item.Mode,
		McpAuthToken:          item.McpAuthToken,
		McpAuthTokenExpiresAt: item.McpAuthTokenExpiresAt,
	}
	if matched != nil {
		spec.ProjectName = matched.ID
	}
	if rp := item.ResolvedProfile; rp != nil {
		spec.AuthMode = rp.AuthMode
		spec.Company = rp.Company
		spec.Model = rp.Model
		spec.PlatformAllowed = rp.PlatformAllowed
	}
	return spec
}

// resolveAllowlistedRepo returns the canonical clone URL for a poll
// work item by consulting the daemon's project allowlist, plus the
// matched ProjectConfig pointer (nil on miss) so callers can read
// the canonical id. Silent — callers decide whether and where to
// warn so the same poll item being resolved by both
// PollItemToSessionSpec and PollItemToSessionDetail doesn't emit
// the same warn twice.
//
// Lookup order (most-specific first):
//  1. item.Repository — when the orchestrator dispatch sent the
//     canonical URL (post-2026-05-08 enrichment). This is the strong
//     signal: a URL match means the daemon is configured to clone
//     this repo.
//  2. item.ProjectName — falls back to the slug for orchestrators
//     that haven't shipped repository enrichment yet, or for entries
//     whose allowlist key is the slug not the URL.
//
// On no-match, returns whatever identifier the item carried so the
// runner has something to attempt; the caller surfaces the broken
// state. The pre-2026-05-08 implementation warned whenever the
// projectName-keyed lookup missed even when the URL lookup was
// about to succeed, which produced a daily flood of false-alarm
// warnings on healthy operator setups.
func resolveAllowlistedRepo(item PollWorkItem, projects []ProjectConfig) (repo string, matched *ProjectConfig) {
	if item.ProjectID != "" {
		if item.RepositoryID != "" {
			for i := range projects {
				project := &projects[i]
				if project.ID == item.ProjectID && project.RepositoryID == item.RepositoryID {
					return project.Repository, project
				}
			}
			return item.Repository, nil
		}
		if item.Repository == "" {
			if item.RequiresRepository {
				for i := range projects {
					project := &projects[i]
					if project.ID == item.ProjectID && project.Primary {
						return project.Repository, project
					}
				}
				return "", nil
			}
			return "", &ProjectConfig{ID: item.ProjectID}
		}
		for i := range projects {
			project := &projects[i]
			if project.ID == item.ProjectID && matchProject(project, item.Repository) != nil {
				return project.Repository, project
			}
		}
		return item.Repository, nil
	}
	if item.Repository != "" {
		if proj, ok := resolveProjectFromAllowlist(item.Repository, projects); ok {
			return proj.Repository, proj
		}
	}
	if item.ProjectName != "" {
		if proj, ok := resolveProjectFromAllowlist(item.ProjectName, projects); ok {
			return proj.Repository, proj
		}
	}
	repo = item.Repository
	if repo == "" {
		repo = item.ProjectName
	}
	return repo, nil
}

// SessionDetailOption customises the SessionDetail built by
// PollItemToSessionDetail. Added as variadic options so the function stays
// back-compatible for existing callers (the donmai-internal daemon loop passes
// none) while embedders can opt in to extra wiring — e.g. advertising
// daemon-side worker capabilities through to the runner.
type SessionDetailOption func(*SessionDetail)

// WithWorkerCapabilities advertises the daemon's worker capability flags on the
// built SessionDetail (deterministic-landing, FD-3). The runner reads these via
// runner.QueuedWork.Capabilities to gate adapter-dependent behaviour. Passing
// nil/empty (or omitting the option entirely) leaves Capabilities nil, the
// mixed-version-safe default where every capability reads false.
func WithWorkerCapabilities(caps map[string]bool) SessionDetailOption {
	return func(d *SessionDetail) {
		if len(caps) == 0 {
			return
		}
		// Defensive copy so a later mutation of the caller's map can't race the
		// stored SessionDetail.
		copied := make(map[string]bool, len(caps))
		for k, v := range caps {
			copied[k] = v
		}
		d.Capabilities = copied
	}
}

// capabilityMergeQueue mirrors runner.CapabilityMergeQueue ("merge-queue") — kept
// as a daemon-local literal to keep this package import-light.
const capabilityMergeQueue = "merge-queue"

// WithMergeQueueLanding stamps the coordinator's per-org merge-queue flag onto
// SessionDetail.Capabilities["merge-queue"] for THIS item. nil ⇒ no-op (legacy
// value stands). Append AFTER WithWorkerCapabilities so the per-item flag wins.
//
// This makes the runner's Delivered→Accepted deferral per-org-flag-driven: when
// the coordinator stamps mergeQueueLanding=true on the poll payload for an
// org that has the landing flag enabled, the runner defers the acceptance
// promotion to the landing finalizer. Absent (older coordinator) leaves the
// org-agnostic worker capability in place — the mixed-version-safe default.
func WithMergeQueueLanding(flag *bool) SessionDetailOption {
	return func(d *SessionDetail) {
		if flag == nil {
			return
		}
		if d.Capabilities == nil {
			d.Capabilities = make(map[string]bool, 1)
		}
		d.Capabilities[capabilityMergeQueue] = *flag
	}
}

// PollItemToSessionDetail constructs the SessionDetail payload `donmai agent
// run` will fetch from the daemon's HTTP API for the given poll item.
// platformURL + authToken + workerID come from the daemon's
// registration state; the issue-context fields come from the platform-
// supplied poll item (or are empty when absent during the rollout
// window).
//
// SessionDetail.Repository is resolved against the daemon's project
// allowlist using the SAME matcher as the WorkerSpawner (slug, URL, or
// URL-suffix). The runner uses this URL for `git clone` — a slug
// passed through unchanged would fail with "fatal: repository '<slug>'
// does not exist". When no match is found we
// fall back to whatever the platform sent and emit a Warn log so the
// fallback is visible in operator logs.
//
// SessionDetail.ProjectName is also normalised to the canonical
// allowlist `id` when a match is found, so downstream code that uses
// the project id (env vars, dashboards) sees a stable value.
//
// Exported so embedders can drive multi-identity poll loops (e.g. a
// downstream embedder that builds a SessionDetail from its own poll
// loop before calling a shared daemon's AcceptWorkWithDetail). Optional
// SessionDetailOption args customise the result without breaking existing
// callers (the donmai-internal loop passes none).
func PollItemToSessionDetail(item PollWorkItem, projects []ProjectConfig, platformURL, authToken, workerID string, opts ...SessionDetailOption) *SessionDetail {
	if len(item.OperationalPayload) == 0 {
		type pollWorkItemAlias PollWorkItem
		if raw, err := json.Marshal(pollWorkItemAlias(item)); err == nil {
			item.OperationalPayload, _ = executioncell.ProjectOperationalPayload(raw)
		}
	}
	repo, matched := resolveAllowlistedRepo(item, projects)
	credentialRequirements, harness, servingHost := pollItemCredentialMetadata(item)
	projectName := item.ProjectName
	projectID := item.ProjectID
	if matched != nil && matched.ID != "" {
		projectName = matched.ID
	}
	// Warn surfaces here (the SessionDetail builder runs once per work
	// item, immediately after PollItemToSessionSpec) rather than inside
	// resolveAllowlistedRepo so the same poll item doesn't produce two
	// identical warns. Fires only when NEITHER repo nor projectName
	// match the allowlist — a genuine config error the runner won't
	// recover from.
	if matched == nil && repo != "" {
		slog.Warn(
			"daemon poll: no allowlist match for repository or projectName; clone will fail unless the platform-supplied string is a real URL",
			"sessionId", item.SessionID,
			"projectName", item.ProjectName,
			"repository", item.Repository,
			"fallback", repo,
		)
	}
	detail := &SessionDetail{
		SessionID:               item.SessionID,
		AdmissionReceipt:        bytes.Clone(item.AdmissionReceipt),
		ClaimReceipt:            bytes.Clone(item.ClaimReceipt),
		EffectiveCell:           bytes.Clone(item.EffectiveCell),
		ExecutionRuntimeBinding: bytes.Clone(item.ExecutionRuntimeBinding),
		OperationalPayload:      bytes.Clone(item.OperationalPayload),
		IssueID:                 item.IssueID,
		IssueIdentifier:         item.IssueIdentifier,
		LinearSessionID:         item.LinearSessionID,
		ProviderSessionID:       item.ProviderSessionID,
		ProjectName:             projectName,
		ProjectID:               projectID,
		RepositoryID:            item.RepositoryID,
		OrganizationID:          item.OrganizationID,
		Repository:              repo,
		Ref:                     item.Ref,
		WorkType:                item.WorkType,
		PromptContext:           item.PromptContext,
		Body:                    item.Body,
		Title:                   item.Title,
		MentionContext:          item.MentionContext,
		ParentContext:           item.ParentContext,
		Branch:                  item.Branch,
		ResolvedProfile:         item.ResolvedProfile,
		ModelProfile:            item.ModelProfile,
		CredentialRequirements:  credentialRequirements,
		Harness:                 harness,
		ServingHost:             servingHost,
		WorkerID:                workerID,
		AuthToken:               authToken,
		PlatformURL:             platformURL,
		McpAuthToken:            item.McpAuthToken,
		McpAuthTokenExpiresAt:   item.McpAuthTokenExpiresAt,
		CredentialPoolID:        item.InjectedPoolID,
		StagePrompt:             item.StagePrompt,
		StageID:                 item.StageID,
		StageBudget:             item.StageBudget,
		StageLifecycle:          item.StageLifecycle,
		StageSourceEventID:      item.StageSourceEventID,
		SystemPromptOverride:    item.SystemPromptOverride,
		Kits:                    item.Kits,
		DisallowedTools:         item.DisallowedTools,
		AllowedTools:            item.AllowedTools,
		McpServers:              item.McpServers,
		Skills:                  item.Skills,
		MemoryBlock:             item.MemoryBlock,
		Mode:                    item.Mode,
		InitialPrompt:           item.InitialPrompt,
		RecordingEnabled:        item.RecordingEnabled,
		TerminalWorkareaLease:   item.TerminalWorkareaLease,
		InterviewBudget:         item.InterviewBudget,
		InterviewDefinition:     item.InterviewDefinition,
		Traceparent:             item.Traceparent,
		Tracestate:              item.Tracestate,
		SessionStorageID:        item.SessionStorageID,
		SessionPublicID:         item.SessionPublicID,
		TrackerSessionID:        item.TrackerSessionID,
	}
	for _, opt := range opts {
		opt(detail)
	}
	return detail
}

// firstNonEmptyStr returns the first non-empty string from values.
// Used by the allowlist resolver to prefer projectName (the slug) over
// the repository field when both are present, matching the platform's
// canonical wire shape.
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
