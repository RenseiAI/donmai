package daemon

import (
	"encoding/json"
	"sync"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runner/access"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// CredentialEnvRequirement describes one non-secret credential requirement
// group. AnyOf contains environment-variable NAMES only: satisfying any one
// name satisfies the group. Values must never be placed on this wire surface.
// Group order and name order are preserved opaquely by the daemon.
type CredentialEnvRequirement struct {
	AnyOf []string `json:"anyOf,omitempty"`
}

// SessionDetail is the per-session payload `donmai agent run` reads from
// the daemon's local control HTTP API on spawn. It carries the full
// runner-side QueuedWork shape (issue context, resolved profile,
// branch) plus the platform-side credentials the runner needs to talk
// back (auth token, platform URL, worker id, lock id).
//
// The daemon stores one SessionDetail per accepted session in an
// in-memory map. A spawned `donmai agent run` process fetches its detail
// via GET /api/daemon/sessions/<id> at start-up.
//
// Wire shape: JSON, camelCase tags. Forward-compat — new fields can be
// added freely; clients ignore unknown fields.
type SessionDetail struct {
	// SessionID is the platform session UUID. Always populated.
	SessionID string `json:"sessionId"`

	// AdmissionReceipt is the opaque, immutable execution-cell admission
	// evidence produced before enqueue. The runner owns strict decoding.
	AdmissionReceipt json.RawMessage `json:"admissionReceipt,omitempty"`

	// ClaimReceipt and EffectiveCell carry claim-time narrowing and the exact
	// secret-free runtime identity opaquely. The runner owns strict decoding.
	ClaimReceipt            json.RawMessage `json:"claimReceipt,omitempty"`
	EffectiveCell           json.RawMessage `json:"effectiveCell,omitempty"`
	ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding,omitempty"`
	// OperationalPayload is forwarded without typed reconstruction. Its nested
	// omitted versus present-empty states are part of admission identity.
	OperationalPayload json.RawMessage `json:"operationalPayload,omitempty"`
	// HostAdaptationReceipt is the immutable ready receipt persisted by the
	// daemon before credentials are resolved or the child is started.
	HostAdaptationReceipt json.RawMessage `json:"hostAdaptationReceipt,omitempty"`

	// IssueID is the Linear issue UUID this session was triggered for.
	IssueID string `json:"issueId,omitempty"`

	// IssueIdentifier is the human-readable Linear identifier
	// (e.g. "ENG-1457").
	IssueIdentifier string `json:"issueIdentifier,omitempty"`

	// LinearSessionID is the Linear-side agent-session id.
	LinearSessionID string `json:"linearSessionId,omitempty"`

	// ProviderSessionID is the provider-native session id when this
	// is a resume (e.g. Claude session UUID).
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// ProjectName is the canonical Linear project identifier.
	ProjectName string `json:"projectName,omitempty"`

	// ProjectID is the stable project-admission identity from the dispatch.
	ProjectID string `json:"projectId,omitempty"`

	// RepositoryID is the stable selected repository-resource identity.
	RepositoryID string `json:"repositoryId,omitempty"`

	// OrganizationID is the platform's tenant UUID.
	OrganizationID string `json:"organizationId,omitempty"`

	// Repository is the git URL (or owner/name slug) the agent should
	// operate on.
	Repository string `json:"repository,omitempty"`

	// RepositoryDeclaration is forwarded after producer negotiation; the exact
	// runner/executor validates it again before provisioning.
	RepositoryDeclaration *workarea.RepositoryDeclarationV1 `json:"repositoryDeclaration,omitempty"`

	// Ref is the base branch / ref to check out from.
	Ref string `json:"ref,omitempty"`

	// WorkType is the workflow discriminant ("development", "qa",
	// "research", ...).
	WorkType string `json:"workType,omitempty"`

	// PromptContext is the rendered Linear issue context block produced
	// by the platform-side dispatcher.
	PromptContext string `json:"promptContext,omitempty"`

	// Body is the raw Linear issue description text.
	Body string `json:"body,omitempty"`

	// Title is the Linear issue title.
	Title string `json:"title,omitempty"`

	// MentionContext is the optional user-mention text from the Linear
	// agent-session create event.
	MentionContext string `json:"mentionContext,omitempty"`

	// ParentContext is the optional parent-issue context block built
	// by the coordinator when this session is a sub-agent.
	ParentContext string `json:"parentContext,omitempty"`

	// Branch is the working branch the agent should create/use.
	Branch string `json:"branch,omitempty"`

	// ResolvedProfile carries the model-profile knobs the platform
	// resolved before queueing this work. Daemon stores opaquely.
	ResolvedProfile *SessionResolvedProfile `json:"resolvedProfile,omitempty"`

	// ModelProfile is the richer, fully-rendered model-profile the
	// platform passes with each dispatch when workType+model-profile
	// routing is active (ADR-2026-05-12-worktype-and-model-profile-
	// routing). When present it supersedes ResolvedProfile.Provider /
	// Model / Effort in the runner. Forwarded opaquely by the daemon.
	ModelProfile *SessionModelProfile `json:"modelProfile,omitempty"`

	// CredentialRequirements is the ordered, non-secret set of environment-
	// variable name groups required by the resolved execution cell. Each group
	// is satisfied by any one of its AnyOf names. The daemon never resolves or
	// logs values; it only carries the metadata to spawn-time consumers.
	CredentialRequirements []CredentialEnvRequirement `json:"credentialRequirements,omitempty"`

	// Harness is the resolved loop-driver identity projected from the poll item
	// (for example, a CLI wrapper or native driver). It is duplicated from the
	// resolved profile intentionally so spawn-time consumers need not traverse
	// the opaque profile payload.
	Harness string `json:"harness,omitempty"`

	// ServingHost is the resolved model-serving location (for example direct,
	// bedrock, vertex, azure, local, or oauth-cli). It is non-secret metadata.
	ServingHost string `json:"servingHost,omitempty"`

	// WorkerID is the daemon worker id that claimed this session.
	WorkerID string `json:"workerId,omitempty"`

	// AuthToken is the runtime JWT the runner uses for platform API
	// calls (heartbeat, result post). Scoped to this worker.
	AuthToken string `json:"authToken,omitempty"`

	// PlatformURL is the base URL of the platform.
	PlatformURL string `json:"platformUrl,omitempty"`

	// McpAuthToken is an opaque platform-supplied bearer for the platform's
	// per-session MCP gateway, and for nothing else. The daemon never parses,
	// validates, or logs its value — it forwards the string from PollWorkItem
	// exactly as received.
	//
	// It is deliberately NOT refreshed by UpdateRuntimeCredentials: that path
	// exists because the worker bearer can be swapped under a running child,
	// whereas this one is written once into an MCP config file the harness
	// reads at spawn and nothing rewrites. Absent (a platform that mints none)
	// is normal and safe — the runner falls back to AuthToken.
	McpAuthToken string `json:"mcpAuthToken,omitempty"`

	// McpAuthTokenExpiresAt is the RFC3339 UTC instant McpAuthToken stops being
	// accepted. ADVISORY ONLY — log fodder so an operator can see the cliff
	// coming. No consumer may gate behaviour on it. Absent whenever
	// McpAuthToken is absent.
	McpAuthTokenExpiresAt string `json:"mcpAuthTokenExpiresAt,omitempty"`

	// CredentialPoolID is the non-secret pool accounting sentinel for
	// metered and shared auth modes: "metered_pool_<provider>" or
	// "shared_pool_<provider>". Absent for byok/host-session/local.
	// Forwarded from PollWorkItem.InjectedPoolID (safe at rest — not a
	// credential, just a billing tag). The runner should echo it in the
	// session cost-event metadata at completion so the platform can
	// attribute usage to the correct metered/shared pool.
	CredentialPoolID string `json:"credentialPoolId,omitempty"`

	// ── Phase 2 stage-driven SDLC fields ───────
	//
	// Forwarded opaquely from PollWorkItem; the daemon does not parse
	// them. The runner consumes them via the prompt.QueuedWork stage
	// fields (cardinal package-architecture rule: daemon does not
	// import runner).

	// StagePrompt is the pre-rendered user-prompt body the platform
	// dispatcher built from the stage prompt template. When present
	// the runner uses it verbatim and skips the embedded user template.
	StagePrompt string `json:"stagePrompt,omitempty"`

	// StageID is the canonical stage id (e.g. "research",
	// "development", "qa"). Used for log correlation + env injection.
	StageID string `json:"stageId,omitempty"`

	// StageBudget is the per-stage runtime budget the runner enforces.
	StageBudget *PollStageBudget `json:"stageBudget,omitempty"`

	// StageLifecycle is the lifecycle config for the workflow this
	// stage instance belongs to. Forwarded opaquely on WORK_RESULT.
	StageLifecycle map[string]any `json:"stageLifecycle,omitempty"`

	// StageSourceEventID is the source CloudEvent id the stage trigger
	// normaliser emitted. Carried for end-to-end audit correlation.
	StageSourceEventID string `json:"stageSourceEventId,omitempty"`

	// SystemPromptOverride forwards the per-session platform-supplied
	// system prompt from PollWorkItem onto the runner's QueuedWork.
	// Read by `prompt/builder.go` (already wired) — this field closes
	// the daemon→runner wire-shape gap (a missing field is silently dropped).
	SystemPromptOverride string `json:"systemPromptOverride,omitempty"`

	// Kits forwards the platform-resolved kit toolchain demand from
	// PollWorkItem onto the runner's QueuedWork (KITS PIVOT #3). The
	// daemon does not interpret it; the runner runs the demand's
	// toolchain_install + post_acquire AFTER repo clone (loop.go step 2b).
	// Mirror of the SystemPromptOverride / DisallowedTools wire-shape
	// forwarders — closes the daemon→runner gap so the platform's emit is
	// not dropped by the strict JSON decoder.
	Kits *kit.ToolchainDemand `json:"kits,omitempty"`

	// DisallowedTools forwards the platform-stamped credential-surface
	// tool restrictions from PollWorkItem onto the runner's QueuedWork.
	// Consumed by runner/spec_translation.go (already wired via 70bf4c0)
	// — this field closes the daemon→runner wire-shape gap.
	// Mirror of the v0.9.3 SystemPromptOverride fix.
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// ── WS5 agent-card → runner fidelity fields ─────────────────────────
	//
	// AllowedTools, McpServers, and Skills forward the resolved agent card's
	// tool-allowlist, MCP servers, and inline skills from PollWorkItem onto
	// the runner's QueuedWork. The daemon does not interpret them — opaque
	// forwarders, same pattern as DisallowedTools / SystemPromptOverride.
	// McpServers/Skills use the daemon-local PollMCPServer / PollSkill mirrors
	// so the daemon stays free of the runner/prompt/agent packages.

	// AllowedTools forwards the agent-card tool allowlist. When non-empty the
	// runner uses it verbatim in place of its default allowlist (card is
	// authoritative). Consumed by runner/spec_translation.go.
	AllowedTools []string `json:"allowedTools,omitempty"`

	// McpServers forwards the agent-card MCP server set. The runner appends
	// these to its per-session default MCP set (the platform HTTP gate is
	// always retained; dedup by name, default wins). Consumed by runner/loop.go.
	McpServers []PollMCPServer `json:"mcpServers,omitempty"`

	// Skills forwards the agent-card inline skill set. The runner folds each
	// skill body into SkillAppend after kit skills and unions their
	// disallowedTools into the disallowed set. Consumed by runner/loop.go.
	Skills []PollSkill `json:"skills,omitempty"`

	// MemoryBlock forwards the dispatch-time agent-memory context from
	// PollWorkItem onto the runner's QueuedWork (Wave 3 memory-inject v1).
	// Consumed by prompt/builder.go, which appends it to the system prompt.
	// Mirror of the SystemPromptOverride / DisallowedTools wire-shape
	// forwarders — closes the daemon→runner gap so the platform's emit is
	// not dropped by the strict JSON decoder.
	MemoryBlock string `json:"memoryBlock,omitempty"`

	// ── Interactive run-mode fields (Wave 2 donmai wire-plumbing) ─
	//
	// Mode forwards the run-mode discriminant from PollWorkItem onto the
	// runner's QueuedWork. "" / absent = headless (unchanged behaviour),
	// "interview" = non-terminating inject-driven loop, and "interactive" =
	// live PTY session. The daemon does not interpret this value — opaque
	// forwarder only (same pattern as
	// SystemPromptOverride / Kits / DisallowedTools).
	Mode string `json:"mode,omitempty"`

	// InitialPrompt forwards the optional first terminal input for a
	// mode:"interactive" session. It stays opaque through the daemon and is
	// never incorporated into headless or interview prompt builders.
	InitialPrompt string `json:"initialPrompt,omitempty"`

	// RecordingEnabled forwards the platform's host-side recording policy
	// decision from PollWorkItem onto the runner's QueuedWork. nil/absent
	// defaults to allowed (mixed-version-safe); explicit false/true is the
	// platform's decision to disable/allow the on-disk asciinema-v2 cast for
	// this interactive session. Opaque forwarder only — same pattern as
	// Mode / InitialPrompt.
	RecordingEnabled *bool `json:"recordingEnabled,omitempty"`

	// InterviewBudget forwards the per-interview wall-clock + idle-grace
	// budget from PollWorkItem onto the runner's QueuedWork. nil/absent
	// is safe and backward-compatible. Opaque forwarder only.
	InterviewBudget *PollInterviewBudget `json:"interviewBudget,omitempty"`

	// InterviewDefinition forwards the compiled interview definition JSON
	// from PollWorkItem onto the runner's QueuedWork. The daemon does not
	// parse it — opaque forwarder only.
	InterviewDefinition json.RawMessage `json:"interviewDefinition,omitempty"`

	// TerminalWorkareaLease forwards the optional provider-neutral retention
	// request. The daemon does not interpret it; the runner validates the finite
	// policy before using it. Nil preserves immediate teardown.
	TerminalWorkareaLease *workarea.TerminalLeaseRequest `json:"terminalWorkareaLease,omitempty"`

	// Capabilities carries the daemon-advertised worker capability flags for
	// this session (deterministic-landing, FD-3). The daemon stamps a flag
	// true only when it actually provides the corresponding adapter; the runner
	// reads them via runner.QueuedWork.Capabilities to gate adapter-dependent
	// behaviour (e.g. the merge-queue acceptance deferral). Absent/nil → every
	// capability is false, the mixed-version-safe default: an older daemon that
	// does not advertise capabilities, or one with no adapters, keeps the prior
	// behaviour. Forwarded opaquely; the daemon does not interpret the keys.
	Capabilities map[string]bool `json:"capabilities,omitempty"`

	// ── W3C trace-context correlation ────────────────────────────────────
	//
	// Platform dispatch stamps these opaquely per
	// src/lib/observability/trace-context.ts. Trace correlation only; never
	// authorization. Opaque forwarders.
	Traceparent      string `json:"traceparent,omitempty"`
	Tracestate       string `json:"tracestate,omitempty"`
	SessionStorageID string `json:"sessionStorageId,omitempty"`
	SessionPublicID  string `json:"sessionPublicId,omitempty"`
	TrackerSessionID string `json:"trackerSessionId,omitempty"`
}

// SessionResolvedProfile mirrors runner.ResolvedProfile but lives in
// the daemon package to avoid an import cycle (the daemon package must
// stay independent of the runner package — `donmai agent run` constructs
// its own runner from this opaque payload).
type SessionResolvedProfile struct {
	// Harness is the platform catalog's loop-driver attribute (e.g. "agy").
	// Forwarded opaquely; the daemon does not interpret it. The runner
	// reads it first and maps it onto the concrete provider impl, so the
	// platform can drop the transitional Provider="agy-cli" wire token.
	// Additive + omitempty: absent on every legacy dispatch (=> the runner
	// falls back to Provider/Runner). Round-tripped through the daemon's
	// SessionDetail wire shape.
	Harness string `json:"harness,omitempty"`

	// ServingHost is the resolved model-serving location. It stays a string in
	// the daemon mirror so this wire package does not import the agent package.
	ServingHost string `json:"servingHost,omitempty"`

	// CredentialRequirements carries ordered groups of environment-variable
	// names. It is metadata only; credential values never enter the poll/detail
	// JSON path.
	CredentialRequirements []CredentialEnvRequirement `json:"credentialRequirements,omitempty"`

	Provider       string         `json:"provider,omitempty"`
	Runner         string         `json:"runner,omitempty"`
	Model          string         `json:"model,omitempty"`
	Effort         string         `json:"effort,omitempty"`
	CredentialID   string         `json:"credentialId,omitempty"`
	ProviderConfig map[string]any `json:"providerConfig,omitempty"`

	// ContextWindow is the context-window size in tokens the control plane
	// resolved for this dispatch (e.g. 1_000_000 for a 1M-context model),
	// stamped as a top-level resolvedProfile field. The json name matches
	// agent.ModelDesc.ContextWindow; the field is deliberately NOT named
	// Context — that token means prompt/mention/parent context on the
	// adjacent wire types. Forwarded opaquely; the daemon does not interpret
	// it. detailToQueuedWork bridges it into the runner profile's
	// ProviderConfig under the same "contextWindow" key
	// runner.ResolvedModelProfile.ToResolvedProfile produces, so providers
	// read one key regardless of which wire field carried the value.
	// Additive + omitempty: absent (zero) on every legacy dispatch, which
	// preserves the model-default behaviour.
	ContextWindow int `json:"contextWindow,omitempty"`

	// Endpoint is the complete secret-free serving/auth projection selected at
	// admission. Receipt-bearing work must carry it explicitly; the runner does
	// not reconstruct an endpoint identity from ambient CLI or host defaults.
	Endpoint *SessionEndpointBinding `json:"endpoint,omitempty"`

	// AuthMode is the credential auth mode the platform resolved for this
	// session: "byok" | "metered" | "shared" | "host-session" | "local".
	// Used by the daemon's credential injection hook to decide whether a
	// missing snapshot is a fail-closed condition (byok/metered/shared) or
	// a safe fail-open (host-session/local). Absent on legacy dispatches
	// that predate the Phase-1 snapshot enrichment; the hook treats absence
	// as fail-open for backward compatibility.
	AuthMode string `json:"authMode,omitempty"`

	// ── P3 narrow-only gate inputs (ADR-2026-06-06 §5.3) ─────────────────
	//
	// These are the values a closed-source embedder's fail-closed gate (S3) hands to
	// access.ResolveMachineCell, one step before the credential hop. The
	// daemon does NOT enforce here — it only carries them through onto the
	// SessionSpec so the embedder's OnPreSpawn closure can read them. All
	// additive + omitempty; absent on every pre-P3 dispatch (=> identity).

	// Company is the endpoint company key (the SPEAK-axis cell identity,
	// e.g. "anthropic"). Stamped by the platform at dispatch alongside the
	// resolved model. The gate uses it as the matrix company-row key.
	Company string `json:"company,omitempty"`

	// PlatformAllowed is the CLOSED set of auth modes the platform already
	// narrowed against org∩project at dispatch — the immutable CEILING the
	// machine gate may only SUBTRACT from. Carried faithfully (same set the
	// platform stamps); the daemon never edits it. Absent/empty on pre-P3
	// dispatches; the gate (S3) treats it as the ceiling.
	PlatformAllowed []access.AuthMode `json:"platformAllowed,omitempty"`
}

// SessionEndpointBinding mirrors agent.EndpointBinding without credential
// values. String fields keep the daemon independent of the agent package while
// preserving every execution-cell axis across the poll/detail boundary.
type SessionEndpointBinding struct {
	Company string `json:"company"`
	Model   string `json:"model"`
	// BaseURL is the endpoint-driven harness's aggregator/base URL (the "same
	// protocol, different base URL" seam — e.g. a Host: direct binding onto an
	// OpenAI-compat aggregator reached over openai-chat). Additive; absent on
	// every dispatch that predates it, which is byte-identical to today's
	// behavior (mixed-version-safe both directions). When present it MUST pass
	// the fail-closed shape check the runner-path boundary applies
	// (agent.ValidateEndpointBindingBaseURL): an absolute http(s) URL with no
	// userinfo, https for any non-loopback host. Never carries credentials —
	// those stay off the wire on agent.EndpointBinding.Env (json:"-").
	BaseURL            string `json:"baseUrl,omitempty"`
	Protocol           string `json:"protocol"`
	Host               string `json:"host"`
	EndpointID         string `json:"endpointId"`
	EndpointOperator   string `json:"endpointOperator"`
	EndpointRevision   string `json:"endpointRevision"`
	ModelAuthor        string `json:"modelAuthor"`
	AuthBindingID      string `json:"authBindingId"`
	AuthAuthority      string `json:"authAuthority"`
	AuthCommercialMode string `json:"authCommercialMode"`
	AuthBindingScope   string `json:"authBindingScope"`
	AuthPortability    string `json:"authPortability"`
	AuthDelivery       string `json:"authDelivery"`
	Mechanism          string `json:"mechanism"`
}

// SessionModelProfile mirrors runner.ResolvedModelProfile but lives in
// the daemon package to avoid an import cycle. It carries the richer
// fully-rendered model-profile the platform resolves via the three-axis
// workType + model-profile routing algorithm
// (ADR-2026-05-12-worktype-and-model-profile-routing). The daemon
// forwards it opaquely; `donmai agent run` bridges it into
// runner.ResolvedModelProfile via detailToQueuedWork.
type SessionModelProfile struct {
	// ID is the model_profile row UUID (e.g. "mp_01jt5...").
	ID string `json:"id"`

	// ProviderID is the canonical provider family (e.g. "claude", "codex",
	// "gemini", "ollama").
	ProviderID string `json:"providerId"`

	// Harness is the loop-driver attribute the platform catalog models on
	// the model identity (e.g. "agy" for the Antigravity `agy` CLI-wrap).
	// When present it is AUTHORITATIVE for binary/provider selection in the
	// runner (it maps the harness token onto its concrete provider impl
	// regardless of ProviderID), mirroring SessionResolvedProfile.Harness.
	// Carried opaquely by the daemon and bridged into
	// runner.ResolvedModelProfile.Harness via detailToQueuedWork so the
	// modelProfile dispatch path produces the same harness-aware
	// ResolvedProfile the resolvedProfile path does. Absent today — the
	// platform writes only resolvedProfile — so this is defense-in-depth
	// for the day the platform populates modelProfile.
	Harness string `json:"harness,omitempty"`

	// Model is the model variant within the provider family.
	Model string `json:"model"`

	// Mode is the reasoning-effort/speed tier (e.g. "xhigh").
	Mode string `json:"mode,omitempty"`

	// Context is the context-window size in tokens required for this
	// dispatch. Zero means "use the model default".
	Context int `json:"context,omitempty"`

	// MaxOutputTokens is the per-response output-token budget. Zero
	// means "use the model default".
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// sessionDetailStore holds the per-session payloads the daemon hands
// to spawned `donmai agent run` processes. Concurrent-safe; the spawner
// writes on AcceptWork and the HTTP server reads on
// /api/daemon/sessions/<id>.
type sessionDetailStore struct {
	mu             sync.RWMutex
	details        map[string]*SessionDetail
	generations    map[string]uint64
	nextGeneration uint64
}

type sessionDetailLease struct {
	sessionID  string
	generation uint64
}

// newSessionDetailStore returns an empty store.
func newSessionDetailStore() *sessionDetailStore {
	return &sessionDetailStore{
		details:     make(map[string]*SessionDetail),
		generations: make(map[string]uint64),
	}
}

// Set stores the detail under d.SessionID. Overwrites any prior entry.
// Runtime admission uses StoreIfAbsent instead so a retry cannot replace an
// already-running session's detail.
func (s *sessionDetailStore) Set(d *SessionDetail) {
	if d == nil || d.SessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.details[d.SessionID] = d
	s.generations[d.SessionID] = s.nextGenerationLocked()
}

// StoreIfAbsent installs d only when its session id is not already owned. The
// returned lease identifies this exact installation so a failed attempt can
// roll itself back without deleting a later generation.
func (s *sessionDetailStore) StoreIfAbsent(d *SessionDetail) (sessionDetailLease, bool) {
	if d == nil || d.SessionID == "" {
		return sessionDetailLease{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.details[d.SessionID]; exists {
		return sessionDetailLease{}, false
	}
	generation := s.nextGenerationLocked()
	s.details[d.SessionID] = d
	s.generations[d.SessionID] = generation
	return sessionDetailLease{sessionID: d.SessionID, generation: generation}, true
}

func (s *sessionDetailStore) nextGenerationLocked() uint64 {
	s.nextGeneration++
	if s.nextGeneration == 0 {
		s.nextGeneration++
	}
	return s.nextGeneration
}

// Get returns the detail for the given session id, or (nil, false)
// when absent.
func (s *sessionDetailStore) Get(id string) (*SessionDetail, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	detail, ok := s.details[id]
	return detail, ok
}

// UpdateRuntimeCredentials refreshes the worker credentials exposed to the
// active child processes of ONE worker identity via /api/daemon/sessions/<id>.
// The daemon calls this after a runtime-token refresh so long-running children
// do not keep presenting an expired bearer token.
//
// The update is SCOPED to prevWorkerID: only details whose WorkerID matches it
// are touched, and that scope is the whole point of this signature. A single
// daemon process can serve SEVERAL worker identities at once — a host admitted
// to more than one organisation registers once per organisation, and an
// embedding binary drives a credential refresher per identity while every
// identity's sessions land in this one store. An unscoped sweep lets identity
// A's hourly refresh stamp A's bearer onto identity B's sessions; B's children
// then present the wrong identity's token on every subsequent platform call
// and are correctly rejected for the rest of their lives.
//
// prevWorkerID is the identity the sessions were claimed under, which is not
// necessarily the identity they end up on: a refresh that had to fall back to
// a full re-registration returns a NEW workerID, and re-stamping those rows
// onto it is exactly how a re-registered identity keeps its own children
// alive. When the refresh preserved the identity, prevWorkerID == workerID and
// only the token moves.
//
// An empty prevWorkerID updates nothing, and a detail stored without a
// WorkerID is never adopted by any refresh: nothing attributes it to the
// refreshing identity, and guessing is the defect this scope exists to
// prevent. Reports how many details were updated.
func (s *sessionDetailStore) UpdateRuntimeCredentials(prevWorkerID, workerID, authToken string) int {
	if prevWorkerID == "" || (workerID == "" && authToken == "") {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := 0
	for _, d := range s.details {
		if d == nil || d.WorkerID != prevWorkerID {
			continue
		}
		if workerID != "" {
			d.WorkerID = workerID
		}
		if authToken != "" {
			d.AuthToken = authToken
		}
		updated++
	}
	return updated
}

// Delete removes the detail for the given session id (idempotent).
// Called by the daemon when a session terminates so stale auth tokens
// don't linger in memory.
func (s *sessionDetailStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.details, id)
	delete(s.generations, id)
}

// DeleteIfOwner removes only the generation installed by lease. It is the
// rollback path for a rejected spawn attempt; a stale rollback must never
// remove a detail installed by a later attempt for the same session id.
func (s *sessionDetailStore) DeleteIfOwner(lease sessionDetailLease) bool {
	if lease.sessionID == "" || lease.generation == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.details[lease.sessionID]; !ok || s.generations[lease.sessionID] != lease.generation {
		return false
	}
	delete(s.details, lease.sessionID)
	delete(s.generations, lease.sessionID)
	return true
}

// Len reports the number of currently-tracked sessions. Useful for
// dashboards + tests.
func (s *sessionDetailStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.details)
}
