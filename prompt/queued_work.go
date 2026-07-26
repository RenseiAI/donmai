package prompt

import (
	"encoding/json"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/kit"
)

// InteractiveRunMode is the QueuedWork.Mode value for a live PTY-hosted
// interactive session (spawn-under-PTY + relay attach). It is the single
// canonical spelling of the wire literal: upstream dispatchers emit it on
// the "mode" field, the daemon forwards it opaquely, and both the runner
// (dispatch branching) and this package (prompt selection — interactive
// sessions never receive a work-type-templated user prompt) consume it.
const InteractiveRunMode = "interactive"

// QueuedWork is the input contract for prompt rendering. It mirrors the
// session payload the platform stores in Redis under
// "agent:session:<sessionId>" and serves to the daemon via
// GET /api/workers/<id>/poll. Field names follow the platform wire
// shape (camelCase JSON tags) and are kept compatible with any future
// afclient.QueuedWork mirror.
//
// Field set is the verbatim subset the prompt renderer consumes today;
// callers may pass values they have available and leave the rest empty.
//
// Source: legacy TS QueuedWork
// (../donmai-libraries/packages/server/src/work-queue.ts) and the live
// Redis session payload observed during F.2.7 verification.
type QueuedWork struct {
	// SessionID is the Rensei session UUID (e.g.
	// "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c"). It uniquely identifies
	// this session record on the platform side.
	SessionID string `json:"sessionId,omitempty"`

	// IssueID is the Linear issue UUID this session was triggered for.
	// May be empty for governor-generated sessions.
	IssueID string `json:"issueId,omitempty"`

	// IssueIdentifier is the human-readable Linear identifier
	// (e.g. "ENG-1457"). Used in the user prompt header so the agent
	// knows which issue it is working on.
	IssueIdentifier string `json:"issueIdentifier,omitempty"`

	// LinearSessionID is the Linear-side agent-session id the platform
	// posts activities to. Distinct from SessionID — same value today,
	// but reserved as a separate field per the platform's wire shape.
	LinearSessionID string `json:"linearSessionId,omitempty"`

	// ProviderSessionID is the provider-native session id (Claude UUID,
	// Codex thread id) when this is a resume. Empty for a fresh spawn.
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// ProjectName is the canonical project identifier (Linear project
	// name). Used both for routing and as a context hint in the system
	// prompt so the agent knows which project it is operating in.
	ProjectName string `json:"projectName,omitempty"`

	// OrganizationID is the Rensei tenant UUID (e.g.
	// "org_ejkmv9ojdyifipydw5l1"). Surfaced in the system prompt so
	// templated org-aware instructions can render.
	OrganizationID string `json:"organizationId,omitempty"`

	// Repository is the git remote URL or owner/name slug the agent
	// should operate on. Empty for governor work types that do not
	// touch a repo (e.g. research-only on issue description).
	Repository string `json:"repository,omitempty"`

	// Ref is the base branch / ref the worktree was checked out at.
	Ref string `json:"ref,omitempty"`

	// WorkType is the work-type discriminant (e.g. "development",
	// "qa", "research"). Drives template selection in [Builder.Build].
	// Unknown values fall through to the development template.
	WorkType string `json:"workType,omitempty"`

	// PromptContext is the rendered Linear issue context block produced
	// by the platform-side dispatcher. Includes the <issue>, <user>,
	// <team>, <project>, <title>, <description> XML envelope. The
	// renderer embeds it verbatim into the user prompt — it already
	// carries the issue body, identifier, title, and project metadata.
	PromptContext string `json:"promptContext,omitempty"`

	// Body is the raw Linear issue description text. Optional; when
	// non-empty and PromptContext is empty, the renderer falls back to
	// composing a minimal context block from Body + IssueIdentifier.
	Body string `json:"body,omitempty"`

	// Title is the Linear issue title. Optional; used when Body is
	// present but PromptContext is empty.
	Title string `json:"title,omitempty"`

	// MentionContext is the optional user-mention text from the Linear
	// agent-session create event (e.g. "please take this on"). Surfaced
	// in the user prompt when present.
	MentionContext string `json:"mentionContext,omitempty"`

	// ParentContext is the optional parent-issue context block built by
	// the coordinator when this session is a sub-agent. Surfaced in the
	// user prompt when present.
	ParentContext string `json:"parentContext,omitempty"`

	// ── Phase 2 stage-driven SDLC fields ────────
	//
	// These fields are populated when the platform's
	// `agent.dispatch_stage` action queues the work (the new
	// thinking-agent dispatcher). When `StagePrompt` is non-empty the
	// runner SHORT-CIRCUITS the embedded user-template renderer and
	// uses StagePrompt verbatim as the agent's directive. Stage prompts
	// are pre-rendered platform-side so the runner does not duplicate
	// per-stage Markdown.
	//
	// Cardinal rule 1: legacy `prompt`/`workType` paths stay working —
	// when StagePrompt is empty the runner falls back to the
	// PromptContext / Body / IssueIdentifier path it has always used.
	//
	// Wire shape: matches the platform's `QueuedStageWork` extension on
	// `QueuedWork` (see platform's
	// src/lib/nodes/action/agent.dispatch_stage/backend.ts). All five
	// fields round-trip opaquely through Redis JSON.

	// StagePrompt is the pre-rendered user-prompt body the
	// platform-side dispatcher built from the stage prompt template +
	// the issue context. When non-empty it replaces the template-driven
	// user prompt the legacy renderer produces.
	StagePrompt string `json:"stagePrompt,omitempty"`

	// StageID is the canonical stage id (e.g. "research",
	// "development", "qa", "acceptance"). Used for log correlation and
	// surfaced into the agent's env via AGENTFACTORY_STAGE_ID.
	StageID string `json:"stageId,omitempty"`

	// StageBudget is the per-stage runtime budget the runner enforces
	// when non-nil. See runner.BudgetEnforcer for cap-breach semantics.
	// All caps default to 0 (= unlimited / not enforced) when absent
	// per-field; a fully-zero budget on a non-nil pointer means
	// "no caps set, proceed unbounded" — same as legacy work.
	StageBudget *StageBudget `json:"stageBudget,omitempty"`

	// StageLifecycle is the lifecycle config for the workflow this
	// stage instance belongs to. The runner forwards it opaquely on the
	// WORK_RESULT envelope so the platform can resolve which native
	// state to drive the issue to on success / failure. The runner does
	// not parse it.
	StageLifecycle map[string]any `json:"stageLifecycle,omitempty"`

	// StageSourceEventID is the source CloudEvent id the stage trigger
	// normaliser emitted. Carried through for end-to-end audit
	// correlation.
	StageSourceEventID string `json:"stageSourceEventId,omitempty"`

	// SystemPromptOverride is an upstream-supplied system prompt that
	// replaces the runner's default system_base.tmpl content when
	// non-empty. The override arrives opaquely from the platform's
	// dispatch layer and is not interpreted by the runner beyond
	// substituting the system prompt string. When empty or absent the
	// runner falls back to the baseline system_base.tmpl rendering
	// (backward-compatible; all existing dispatches without this field
	// are unaffected).
	//
	// Wire shape: "systemPromptOverride" (camelCase, omitempty). Populated
	// by the platform's agent.dispatch_stage action when the resolved
	// agent card carries a non-empty systemPrompt in its card jsonb.
	SystemPromptOverride string `json:"systemPromptOverride,omitempty"`

	// Kits is the platform-resolved kit toolchain demand for this session
	// (KITS PIVOT #3). The platform composes the agent composition's
	// KitRef[] into a kit.ToolchainDemand and threads it here so the runner
	// runs toolchain_install + post_acquire AFTER the repo is cloned
	// (runner/loop.go step 2b). When non-nil and non-empty it is the
	// authoritative lifecycle demand for this session and OVERRIDES the
	// repo-detection fallback (OD-1: explicit-overrides-detection). Its exact
	// kit selections still undergo local command-ownership preflight. When
	// nil/empty the runner falls back to detecting kits from the cloned
	// worktree via its KitDetector — backward-compatible.
	//
	// Wire shape: "kits" (camelCase, omitempty). Opaque to the prompt
	// renderer — consumed only by the runner loop. Mirrors the
	// SystemPromptOverride threading: every wire hop (PollWorkItem,
	// SessionDetail, detailToQueuedWork) carries it so Go's strict JSON
	// decoder never drops the platform's emit (the v0.9.3
	// SystemPromptOverride wire-gap precedent).
	Kits *kit.ToolchainDemand `json:"kits,omitempty"`

	// DisallowedTools is the platform-supplied set of additional tool
	// patterns to block for this session. The runner APPENDS these to
	// its own defaultDisallowedTools() baseline — it never replaces the
	// baseline — so the runner's static policy remains the floor.
	//
	// This is the Option B wire field for Layer 3 (cred-surface
	// restriction). The platform stamps it via stampDisallowedTools() in
	// credential-injection.ts. Round-trips opaquely through Redis JSON;
	// absent/null/empty is safe and backward-compatible (omitempty).
	//
	// Wire shape: "disallowedTools" (camelCase, omitempty). Pairs with
	// platform PR #196.
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// ── WS5 agent-card → runner fidelity fields ─────────────────────────
	//
	// AllowedTools, McpServers, and Skills carry the resolved agent card's
	// tool-allowlist, MCP servers, and inline skills to the runtime. The
	// platform's emit side stamps them onto QueuedWork from the resolved
	// agent composition. Each is additive + omitempty so a pre-WS5,
	// field-less payload decodes unchanged (the v0.9.3 SystemPromptOverride
	// wire-gap precedent: a field absent on the wire is silently dropped, so
	// every wire hop — PollWorkItem, SessionDetail, detailToQueuedWork —
	// carries them).

	// AllowedTools is the platform-supplied set of tool-call patterns the
	// agent card authorises for this session. When non-empty it is
	// AUTHORITATIVE — the runner uses it verbatim in place of its own
	// defaultAllowedTools() baseline (the card is the source of truth for
	// what the agent may call). When empty/absent the runner falls back to
	// defaultAllowedTools() — backward-compatible. The runner's
	// defaultDisallowedTools() floor still applies regardless.
	//
	// Wire shape: "allowedTools" (camelCase, omitempty). Mirrors the
	// claude/gemini AllowedTools permission-pattern grammar
	// ("Bash(prefix:glob)", "Edit", "Read", …).
	AllowedTools []string `json:"allowedTools,omitempty"`

	// McpServers is the platform-supplied set of MCP servers the agent card
	// declares. The runner APPENDS these to its own per-session default MCP
	// set (the platform per-session HTTP gate is always retained; dedup by
	// name with the default winning on collision). Reuses agent.MCPServerConfig
	// (stdio + http transports) so the wire shape is shared with the Spec.
	//
	// Wire shape: "mcpServers" (camelCase, omitempty). Opaque to the prompt
	// renderer — consumed only by the runner loop.
	McpServers []agent.MCPServerConfig `json:"mcpServers,omitempty"`

	// Skills is the platform-supplied set of INLINE skills the agent card
	// declares (distinct from kit file-sourced skills). Each skill's body is
	// folded into the prompt builder's SkillAppend AFTER any kit skills, and
	// its disallowedTools are unioned into the kit-derived disallowed set
	// (subtractive: skills may only narrow the tool surface).
	//
	// Wire shape: "skills" (camelCase, omitempty). Opaque to the prompt
	// renderer — consumed only by the runner loop.
	Skills []SkillSpec `json:"skills,omitempty"`

	// MemoryBlock is the dispatch-time agent-memory context the platform
	// folds into the system prompt for this session (Wave 3 memory-inject
	// v1). When non-empty the prompt builder APPENDS it to the resolved
	// system prompt under a "# Agent Memory" heading — it never replaces
	// the base/override system prompt (additive). Blocks known only mid-
	// session arrive via the runtime lock-refresh inject path instead.
	//
	// Wire shape: "memoryBlock" (camelCase, omitempty). Threaded through
	// every wire hop (PollWorkItem, SessionDetail, detailToQueuedWork) so
	// Go's strict JSON decoder never drops the platform's emit — the v0.9.3
	// SystemPromptOverride wire-gap precedent (silent-drop hazard).
	MemoryBlock string `json:"memoryBlock,omitempty"`

	// ── Interactive run-mode fields (Wave 2 donmai wire-plumbing) ─
	//
	// Mode is the run-mode discriminant. "" or absent = normal headless
	// run. "interview" = inject-driven interview loop (non-terminating;
	// parks on injectCh between user turns). "interactive" = live PTY
	// session. The runner branches on this value; the prompt renderer does
	// not interpret it.
	//
	// Wire shape: "mode" (camelCase, omitempty). Canonical value:
	// internal/interview/wiretypes.go InterviewRunMode.
	Mode string `json:"mode,omitempty"`

	// InitialPrompt is opaque first-input data for [InteractiveRunMode]. The
	// interactive runner writes it verbatim plus one newline into the live PTY
	// before relay attach. The prompt renderer MUST NOT include it in either
	// headless or interview system/user prompts.
	//
	// Wire shape: "initialPrompt" (camelCase, omitempty). Empty/absent is a
	// no-op and preserves the pre-field wire shape.
	InitialPrompt string `json:"initialPrompt,omitempty"`

	// InterviewBudget is the per-interview runtime budget the runner
	// enforces when Mode="interview". nil = no caps. Carried through
	// every wire hop so the strict JSON decoder never drops it.
	//
	// Wire shape: "interviewBudget" (camelCase, omitempty).
	InterviewBudget *InterviewBudget `json:"interviewBudget,omitempty"`

	// InterviewDefinition is the compiled interview definition JSON the
	// platform emits from the interview.config node's publish-time
	// compiler. The runner reads it to assemble the agent's system prompt
	// for interview mode. Carried opaquely — the prompt package does not
	// parse it; only the interview loop consumer does.
	//
	// Wire shape: "interviewDefinition" (camelCase, omitempty).
	InterviewDefinition json.RawMessage `json:"interviewDefinition,omitempty"`

	// CodeIntel is the platform-resolved code-intelligence capability block
	// for this session (Wave 2 code-intel activation). When non-nil the runner
	// turns on the in-box `af-code-intelligence` stdio MCP plugin: it appends a
	// self-referential stdio server entry (os.Executable() + `mcp code-intel
	// --root <worktree>`) to the session's MCP set and allow-lists the six
	// mcp__af-code-intelligence__af_code_* tool names for MCP-capable
	// providers. When nil the capability is OFF and behaviour is byte-identical
	// to a pre-code-intel session (additive — nil = zero change).
	//
	// Wire shape: "codeIntel" (camelCase, omitempty). Threaded opaquely through
	// every wire hop (PollWorkItem, SessionDetail, detailToQueuedWork) so Go's
	// strict JSON decoder never drops the platform's emit — the v0.9.3
	// SystemPromptOverride wire-gap precedent (silent-drop hazard). Old runners
	// ignore the unknown field; old platforms simply never emit it (the runner
	// tolerates its absence — nil = capability off).
	CodeIntel *CodeIntelWork `json:"codeIntel,omitempty"`
}

// CodeIntelWork is the typed code-intelligence capability block on QueuedWork.
// It is the platform→runner signal that the session should run with the in-box
// code-intelligence engine exposed as an MCP tool surface. The runner — never
// the platform — constructs the concrete stdio MCP entry from this block (it
// owns os.Executable() and the provisioned worktree path), per the frozen wire
// contract (runs/2026-07-04-code-intel-capability).
//
// All inner fields except Repo are optional; a minimal block is `{"repo":…}`.
type CodeIntelWork struct {
	// Repo is the repository the capability is scoped to (owner/name slug or
	// remote URL). Present for audit/correlation and to support a future
	// cross-repo capability ref; the runner indexes the provisioned worktree
	// regardless, so an empty Repo does not disable the capability.
	Repo string `json:"repo,omitempty"`

	// Ref is the optional git ref (branch/tag/sha) the capability was pinned
	// to. Carried for correlation; the runner indexes whatever the worktree
	// was provisioned at.
	Ref string `json:"ref,omitempty"`

	// RepoPath is an optional path RELATIVE to the session worktree root that
	// scopes indexing to a subtree (e.g. a single package in a monorepo). Same
	// validation semantics as the `donmai code --repo-path` flag (must stay
	// inside the root). Forwarded to the stdio server as `--repo-path`. Empty
	// = index the whole worktree.
	RepoPath string `json:"repoPath,omitempty"`

	// Tools is an optional subset of the six code-intel tool names to expose
	// (e.g. ["af_code_search_symbols","af_code_get_repo_map"]). Empty = expose
	// all six. Forwarded to the stdio server as `--tools` and used to filter
	// the FQ tool names the runner allow-lists.
	Tools []string `json:"tools,omitempty"`
}

// InterviewBudget is the per-interview wall-clock and idle-grace cap the
// runner enforces when QueuedWork.Mode == "interview". A field with
// value 0 means "no cap" for that dimension (same convention as
// StageBudget). The struct is a flat value type; a nil pointer on
// InterviewBudget in QueuedWork means "no budget, proceed unbounded".
//
// Source of truth: CONTRACT-FREEZE §4 (runs/2026-06-02-interactive-interviews/01-CONTRACT-FREEZE.md).
type InterviewBudget struct {
	// MaxWallClockSeconds is the absolute wall-clock cap for the
	// entire interview session. 0 = no cap.
	MaxWallClockSeconds int `json:"maxWallClockSeconds,omitempty"`

	// IdleGraceSeconds is how long the runner waits for the next
	// user inject before tearing down the session. 0 = no cap.
	IdleGraceSeconds int `json:"idleGraceSeconds,omitempty"`
}

// StageBudget mirrors the platform's StageBudget type from
// src/lib/workflow/stages/index.ts. The runner enforces these caps via
// runner.BudgetEnforcer; see runner/budget.go for the cap-breach
// semantics. A field with value 0 is treated as "no cap" so partial
// budgets degrade gracefully.
type StageBudget struct {
	// MaxDurationSeconds is the wall-clock cap on the stage instance.
	// 0 = no cap.
	MaxDurationSeconds int `json:"maxDurationSeconds,omitempty"`

	// MaxSubAgents is the cap on Task tool invocations the agent may
	// spawn over the life of the stage. 0 = no cap. Sub-agents
	// counted: every ToolUseEvent whose ToolName is "Task".
	MaxSubAgents int `json:"maxSubAgents,omitempty"`

	// MaxTokens is the cap on total token consumption (input + output
	// across all turns, summed from per-turn ResultEvent.Cost or the
	// roll-up CostData on terminal). 0 = no cap.
	MaxTokens int64 `json:"maxTokens,omitempty"`
}

// SkillSpec is one INLINE skill carried by the agent card (WS5). It is
// distinct from a kit file-sourced skill: the skill body arrives verbatim
// on the wire rather than being read from a SKILL.md on disk. The runner
// folds Body into the system-prompt SkillAppend block (after kit skills)
// and unions DisallowedTools into the kit-derived disallowed set
// (subtractive — skills may only narrow the tool surface, never widen it).
type SkillSpec struct {
	// ID is the skill's canonical id (e.g. "spring-debugging"). Used for
	// log correlation; carried opaquely by the runner.
	ID string `json:"id,omitempty"`

	// Body is the inline skill body folded into the system prompt's
	// SkillAppend block. Markdown text, used verbatim.
	Body string `json:"body,omitempty"`

	// DisallowedTools is the optional set of tool-call patterns this skill
	// forbids. Unioned into the kit-derived disallowed set and appended to
	// the agent.Spec's DisallowedTools — subtractive (narrows only).
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}
