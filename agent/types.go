package agent

// This file ports the type-only declarations from
// ../agentfactory/packages/core/src/providers/types.ts.
//
// Per the F.1.1 design doc §2 (Type signatures (Go)), these types are
// the verbatim Go translation. JSON tags use camelCase to match the TS
// wire format consumed by QueuedWork.resolvedProfile readers.

// ProviderName is the stable identifier for an agent provider family.
//
// It mirrors AgentProviderName from the legacy TS port. v0.5.0 ships
// claude, codex, and stub. Future families (spring-ai, a2a, gemini,
// ollama, opencode, jules, amp) extend this enum without breaking the
// contract.
//
// Source: ../agentfactory/packages/core/src/providers/types.ts (AgentProviderName).
type ProviderName string

// ProviderName constants. v0.5.0 ships ProviderClaude, ProviderCodex,
// and ProviderStub. The remaining identifiers are reserved for v0.6.0+
// providers per F.1.1 §1; declaring them now keeps the wire enum stable
// across waves so platform-side dispatch routing does not regress.
const (
	ProviderClaude   ProviderName = "claude"
	ProviderCodex    ProviderName = "codex"
	ProviderStub     ProviderName = "stub" // test-only; deterministic
	ProviderSpringAI ProviderName = "spring-ai"
	ProviderA2A      ProviderName = "a2a"
	ProviderAmp      ProviderName = "amp"
	ProviderGemini   ProviderName = "gemini"
	ProviderOllama   ProviderName = "ollama"
	ProviderOpenCode ProviderName = "opencode"
	ProviderJules    ProviderName = "jules"
)

// Capability names a single optional behavior a provider may support.
//
// The 9 named capabilities mirror the AgentProviderCapabilities fields
// from the legacy TS interface verbatim. Use IsSupported(caps, cap) to
// gate runner behavior rather than try-catching unsupported provider
// operations.
//
// Source: ../agentfactory/packages/core/src/providers/types.ts
// (AgentProviderCapabilities).
type Capability string

// Named capability constants. Each maps 1:1 to a field on Capabilities.
const (
	CapMessageInjection           Capability = "message_injection"
	CapSessionResume              Capability = "session_resume"
	CapToolPlugins                Capability = "tool_plugins"
	CapBaseInstructions           Capability = "base_instructions"
	CapPermissionConfig           Capability = "permission_config"
	CapCodeIntelEnforcement       Capability = "code_intel_enforcement"
	CapSubagentEvents             Capability = "subagent_events"
	CapReasoningEffort            Capability = "reasoning_effort"
	CapToolPermissionFormatClaude Capability = "tool_perm_format_claude"
	// CapAcceptsAllowedToolsList reports whether the provider honors
	// Spec.AllowedTools end-to-end (translation into upstream API).
	// Tracks the v2 contract's `acceptsAllowedToolsList` flag from
	// 002-provider-base-contract.md §"Tool-use surface — forward declaration".
	CapAcceptsAllowedToolsList Capability = "accepts_allowed_tools_list"
	// CapAcceptsMcpServerSpec reports whether the provider honors
	// Spec.MCPServers end-to-end (translation into upstream API or a
	// session-local MCP bridge). Tracks the v2 contract's
	// `acceptsMcpServerSpec` flag.
	CapAcceptsMcpServerSpec Capability = "accepts_mcp_server_spec"
)

// Capabilities is the typed capability matrix every provider declares.
//
// Verbatim port of AgentProviderCapabilities. Flat struct (no nested
// objects) per the rensei-architecture base-contract validator
// constraint (002-provider-base-contract.md §Capabilities).
//
// Per F.1.1 §3.1 and the locked coordinator decision, the v0.5.0
// claude provider ships SupportsMessageInjection=false (no mid-session
// injection in CLI JSON-stream mode); flip when a wrapper sidecar
// lands in F.5.
//
// Source: ../agentfactory/packages/core/src/providers/types.ts
// (AgentProviderCapabilities).
type Capabilities struct {
	// SupportsMessageInjection reports whether Handle.Inject works
	// (stateful providers: legacy TS Claude SDK, A2A). v0.5.0 Claude
	// CLI provider returns false here; steering reduces to stop+resume.
	SupportsMessageInjection bool `json:"supportsMessageInjection"`

	// SupportsSessionResume reports whether Provider.Resume can
	// continue a prior session.
	SupportsSessionResume bool `json:"supportsSessionResume"`

	// SupportsToolPlugins reports whether the provider can use MCP
	// tool plugins delivered via stdio servers (af_linear_*, af_code_*).
	SupportsToolPlugins bool `json:"supportsToolPlugins,omitempty"`

	// NeedsBaseInstructions reports whether the provider requires
	// persistent base instructions via Spec.BaseInstructions
	// (Codex app-server thread/start ‘instructions' field).
	NeedsBaseInstructions bool `json:"needsBaseInstructions,omitempty"`

	// NeedsPermissionConfig reports whether the provider requires
	// structured permission config via Spec.PermissionConfig (Codex
	// approval bridge).
	NeedsPermissionConfig bool `json:"needsPermissionConfig,omitempty"`

	// SupportsCodeIntelligenceEnforcement reports whether the provider
	// supports canUseTool-style code intelligence enforcement
	// (redirect Grep/Glob to af_code_* tools). v0.5.0 Claude provider
	// ships false; flip when a CLI wrapper exposes the callback.
	SupportsCodeIntelligenceEnforcement bool `json:"supportsCodeIntelligenceEnforcement,omitempty"`

	// EmitsSubagentEvents reports whether the provider emits
	// Anthropic-style subagent events (Claude Task tool progress).
	// Drives the Topology view's subagent stream rendering.
	EmitsSubagentEvents bool `json:"emitsSubagentEvents"`

	// SupportsReasoningEffort reports whether the provider honors the
	// per-step Spec.Effort (low | medium | high | xhigh). When false,
	// the dispatch path drops the value and emits a capability-mismatch
	// hook event so observers can flag silently-ignored cost-control
	// hints. Optional for backwards compatibility — a zero value is
	// treated as not supporting reasoning effort.
	SupportsReasoningEffort bool `json:"supportsReasoningEffort,omitempty"`

	// ToolPermissionFormat names the tool-permission grammar this
	// provider uses ("claude" | "codex" | "spring-ai"). When empty
	// callers should default to "claude".
	ToolPermissionFormat string `json:"toolPermissionFormat,omitempty"`

	// AcceptsAllowedToolsList reports whether the provider honors
	// Spec.AllowedTools end-to-end at Spawn time — translating it into
	// the upstream API's allow/permission grammar. Providers that ship
	// SupportsToolPlugins=true but cannot pass through AllowedTools
	// (e.g. codex routes permissions through the approval bridge) MUST
	// declare false here so the runner knows the field is silently
	// dropped. Tracks the v2 contract's acceptsAllowedToolsList flag
	// from 002-provider-base-contract.md §"Tool-use surface — forward
	// declaration".
	AcceptsAllowedToolsList bool `json:"acceptsAllowedToolsList,omitempty"`

	// AcceptsMcpServerSpec reports whether the provider honors
	// Spec.MCPServers end-to-end at Spawn time — either passing the
	// stdio configs through to the upstream API or spawning a local
	// MCP bridge that the upstream session can call. Distinct from
	// SupportsToolPlugins which only states whether tools are usable
	// at all; this flag specifies whether the Spec field shape is
	// honored.
	AcceptsMcpServerSpec bool `json:"acceptsMcpServerSpec,omitempty"`

	// HumanLabel is a human-readable provider-family display name
	// (e.g. "Claude", "Codex"). Used in TUI/dashboard surfaces and
	// log messages where the raw name is not user-friendly.
	HumanLabel string `json:"humanLabel,omitempty"`
}

// IsSupported reports whether a Capabilities matrix has the named
// Capability flag set. This is the runner's preferred gate over reading
// individual fields, because it keeps gating expressions consistent
// across providers.
func IsSupported(caps Capabilities, c Capability) bool {
	switch c {
	case CapMessageInjection:
		return caps.SupportsMessageInjection
	case CapSessionResume:
		return caps.SupportsSessionResume
	case CapToolPlugins:
		return caps.SupportsToolPlugins
	case CapBaseInstructions:
		return caps.NeedsBaseInstructions
	case CapPermissionConfig:
		return caps.NeedsPermissionConfig
	case CapCodeIntelEnforcement:
		return caps.SupportsCodeIntelligenceEnforcement
	case CapSubagentEvents:
		return caps.EmitsSubagentEvents
	case CapReasoningEffort:
		return caps.SupportsReasoningEffort
	case CapToolPermissionFormatClaude:
		return caps.ToolPermissionFormat == "" || caps.ToolPermissionFormat == "claude"
	case CapAcceptsAllowedToolsList:
		return caps.AcceptsAllowedToolsList
	case CapAcceptsMcpServerSpec:
		return caps.AcceptsMcpServerSpec
	default:
		return false
	}
}

// SandboxLevel mirrors AgentSpawnConfig.sandboxLevel from the legacy TS.
//
// Source: ../agentfactory/packages/core/src/providers/types.ts.
type SandboxLevel string

// SandboxLevel constants align with Codex sandbox policies (readOnly /
// workspaceWrite / dangerFullAccess) and the cross-provider sandbox
// taxonomy in F.1.1 §3.2.
const (
	SandboxReadOnly       SandboxLevel = "read-only"
	SandboxWorkspaceWrite SandboxLevel = "workspace-write"
	SandboxFullAccess     SandboxLevel = "full-access"
)

// EffortLevel mirrors EffortLevel from
// ../agentfactory/packages/core/src/providers/index.ts. Providers map
// this to their native reasoning-effort knob:
//   - Claude  : --effort flag
//   - Codex   : reasoningEffort / model_reasoning_effort
//   - Gemini  : thinkingBudget
type EffortLevel string

// EffortLevel constants. The xhigh tier matches Anthropic's
// reasoning-effort scale.
const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortXHigh  EffortLevel = "xhigh"
)

// MCPServerConfig is one stdio-transport MCP server the provider should
// configure on session start. Providers that support MCP tool plugins
// inject these on session init (Claude: via --mcp-config; Codex: via
// config/batchWrite mcpStdioServers).
//
// Verbatim port of the legacy TS AgentSpawnConfig.mcpStdioServers element.
type MCPServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// PermissionConfig is the runtime permission policy for the codex
// approval bridge. Verbatim port of CodexPermissionConfig from
// ../agentfactory/packages/core/src/templates/adapters.ts.
//
// Providers without NeedsPermissionConfig=true ignore this field.
type PermissionConfig struct {
	// AllowPatterns is the list of tool-call patterns auto-approved
	// without prompting (e.g. "Bash(pnpm:*)").
	AllowPatterns []string `json:"allowPatterns,omitempty"`

	// DisallowPatterns is the list of tool-call patterns auto-denied.
	DisallowPatterns []string `json:"disallowPatterns,omitempty"`

	// DefaultDecision is the fallback when no pattern matches.
	// Valid values: "allow" | "deny" | "prompt". Empty defaults to
	// "prompt" (provider-specific).
	DefaultDecision string `json:"defaultDecision,omitempty"`
}

// CodeIntelEnforcement is the Grep/Glob redirect-to-af_code_* config.
// When set, the provider's tool callback redirects native Grep/Glob
// calls to af_code_* tools until the agent has attempted code
// intelligence at least once.
type CodeIntelEnforcement struct {
	// EnforceUsage requires the agent to call at least one af_code_*
	// tool before native Grep/Glob is allowed.
	EnforceUsage bool `json:"enforceUsage"`

	// FallbackAfterAttempt allows native Grep/Glob after a single
	// af_code_* attempt, regardless of result.
	FallbackAfterAttempt bool `json:"fallbackAfterAttempt"`
}

// Spec is the input contract for Provider.Spawn.
//
// Verbatim port of the AgentSpawnConfig from the legacy TS providers/types.ts.
// Capability-gated: providers consume the fields they support;
// unsupported fields are silently ignored. The runner is responsible
// for not setting incompatible fields (gate on Capabilities before
// invoking Spawn).
//
// Source: ../agentfactory/packages/core/src/providers/types.ts
// (AgentSpawnConfig).
type Spec struct {
	// Prompt is the task-specific directive.
	Prompt string `json:"prompt"`

	// Cwd is the working directory the agent session runs in.
	// Typically the runner's worktree path.
	Cwd string `json:"cwd"`

	// Env is the environment variable map merged onto the process
	// environment (after AGENT_ENV_BLOCKLIST filtering).
	Env map[string]string `json:"env"`

	// Autonomous indicates the session has no interactive user;
	// providers should disable user-input tools when true.
	Autonomous bool `json:"autonomous"`

	// SandboxEnabled enables the provider's default filesystem/network
	// sandbox.
	SandboxEnabled bool `json:"sandboxEnabled"`

	// SandboxLevel overrides the sandbox tier when SandboxEnabled is
	// true. Maps to provider-native policies.
	SandboxLevel SandboxLevel `json:"sandboxLevel,omitempty"`

	// AllowedTools is the list of tool-call patterns auto-allowed
	// without prompting (Claude permission-pattern format
	// "Bash(prefix:glob)").
	AllowedTools []string `json:"allowedTools,omitempty"`

	// DisallowedTools is the list of tool-call patterns the provider
	// must reject.
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// MCPServers is the list of stdio MCP servers the provider should
	// configure on session start.
	MCPServers []MCPServerConfig `json:"mcpStdioServers,omitempty"`

	// MCPToolNames is the list of fully-qualified MCP tool names
	// (e.g. "mcp__af-code-intelligence__af_code_get_repo_map") that
	// should be added to AllowedTools so autonomous agents can call
	// them.
	MCPToolNames []string `json:"mcpToolNames,omitempty"`

	// MaxTurns caps agentic turns (API round-trips) before the
	// provider stops. nil falls back to the provider default.
	MaxTurns *int `json:"maxTurns,omitempty"`

	// Model is the model identifier (e.g. "claude-sonnet-4-5"). Empty
	// falls back to provider/env default.
	Model string `json:"model,omitempty"`

	// Effort is the normalized reasoning-effort tier. Honored only
	// when Capabilities.SupportsReasoningEffort is true.
	Effort EffortLevel `json:"effort,omitempty"`

	// BaseInstructions are persistent system instructions
	// (Codex thread/start ‘instructions'). Honored only when
	// Capabilities.NeedsBaseInstructions is true.
	BaseInstructions string `json:"baseInstructions,omitempty"`

	// SystemPromptAppend is appended to the system prompt after
	// standard instruction sections (sourced from
	// RepositoryConfig.systemPrompt).
	SystemPromptAppend string `json:"systemPromptAppend,omitempty"`

	// PermissionConfig is the structured permission policy for
	// providers that consume one (Codex approval bridge).
	PermissionConfig *PermissionConfig `json:"permissionConfig,omitempty"`

	// CodeIntelEnforcement enables the af_code_* redirect for
	// providers that support it.
	CodeIntelEnforcement *CodeIntelEnforcement `json:"codeIntelligenceEnforcement,omitempty"`

	// ProviderConfig is provider-specific settings from the matched
	// model profile (e.g. {"serviceTier":"fast"} for OpenAI,
	// {"speed":"fast"} for Anthropic).
	ProviderConfig map[string]any `json:"providerConfig,omitempty"`

	// SubAgentProvider names the provider for spawned sub-agents when
	// different from the parent. Used by coordination templates.
	SubAgentProvider ProviderName `json:"subAgentProvider,omitempty"`

	// OnProcessSpawned is an optional callback that fires once the
	// provider has spawned its underlying process; used by the runner
	// to capture PIDs for metrics. Not serialized.
	OnProcessSpawned func(pid int) `json:"-"`
}

// CostData mirrors AgentCostData from the legacy TS providers/types.ts.
//
// All fields are optional; providers populate what they have available.
type CostData struct {
	InputTokens       int64   `json:"inputTokens,omitempty"`
	OutputTokens      int64   `json:"outputTokens,omitempty"`
	CachedInputTokens int64   `json:"cachedInputTokens,omitempty"`
	TotalCostUsd      float64 `json:"totalCostUsd,omitempty"`
	NumTurns          int     `json:"numTurns,omitempty"`
}

// Result is the runner's final session-result shape, distinct from the
// per-provider terminal ResultEvent.
//
// The runner builds this from the terminal Event plus side-channel data
// (worktree path, backstop report, quality report, WORK_RESULT marker)
// and posts it to the platform via POST /api/sessions/<id>/completion
// (see F.1.1 §6).
type Result struct {
	// Status is the terminal session status. Drives platform-side
	// agent_session.status mutations downstream of /completion.
	// Valid values: "completed" | "failed" | "stopped".
	Status string `json:"status"`

	// ProviderName identifies which provider produced this result.
	ProviderName ProviderName `json:"providerName"`

	// ProviderSessionID is the provider-native session id (Claude UUID,
	// Codex thread id) captured from InitEvent.
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// WorktreePath is the absolute path of the worktree the session
	// ran in. Useful for debugging and for the
	// PreserveWorktreeOnFailure path.
	WorktreePath string `json:"worktreePath,omitempty"`

	// PullRequestURL is the URL of the PR opened (by the agent or the
	// backstop). Empty when no PR exists.
	PullRequestURL string `json:"pullRequestUrl,omitempty"`

	// CommitSHA is the head commit sha of the work branch when known.
	CommitSHA string `json:"commitSha,omitempty"`

	// Summary is a short human-readable summary of the work done.
	Summary string `json:"summary,omitempty"`

	// WorkResult is the QA/acceptance verdict. Valid values:
	// "passed" | "failed" | "unknown". Drives the acceptance gate on
	// the platform side.
	WorkResult string `json:"workResult,omitempty"`

	// Cost rolls up token usage and dollars across the session.
	Cost *CostData `json:"cost,omitempty"`

	// FailureMode classifies why a non-completed session failed.
	// Enum values are owned by runner/failure.go.
	FailureMode string `json:"failureMode,omitempty"`

	// BackstopReport captures what the post-session backstop did,
	// if it ran.
	BackstopReport *BackstopReport `json:"backstopReport,omitempty"`

	// QualityReport captures the test/typecheck/lint deltas vs the
	// session-start baseline.
	QualityReport *QualityReport `json:"qualityReport,omitempty"`

	// Error is the human-readable error message when Status is
	// "failed".
	Error string `json:"error,omitempty"`
}

// BackstopReport captures what the post-session backstop did, if
// anything. Populated by runner/backstop.go (F.1.1 §5.6).
type BackstopReport struct {
	// Triggered is true when the backstop ran (i.e. the contract had
	// unfilled fields).
	Triggered bool `json:"triggered"`

	// Pushed is true when the backstop ran git push -u origin <branch>.
	Pushed bool `json:"pushed,omitempty"`

	// PRCreated is true when the backstop ran gh pr create.
	PRCreated bool `json:"prCreated,omitempty"`

	// PRURL is the URL of a PR the backstop created or detected.
	PRURL string `json:"prUrl,omitempty"`

	// UnfilledFields lists the contract fields the backstop could not
	// auto-recover (e.g. work_result, comment_posted). The platform
	// posts a diagnostic Linear comment for these.
	UnfilledFields []string `json:"unfilledFields,omitempty"`

	// Diagnostics is human-readable text describing what happened.
	Diagnostics string `json:"diagnostics,omitempty"`
}

// QualityReport captures the test/typecheck/lint deltas vs the
// session-start baseline. Populated by runner/quality.go (F.1.1 §5.5).
type QualityReport struct {
	// BaselineCaptured is true when the session-start baseline was
	// successfully recorded; false when the gate was unavailable
	// (e.g. testCommand missing).
	BaselineCaptured bool `json:"baselineCaptured"`

	// TestDelta is the change in failing test count vs baseline.
	// Negative means improvement, positive means regression.
	TestDelta int `json:"testDelta"`

	// TypeDelta is the change in typecheck error count vs baseline.
	TypeDelta int `json:"typeDelta"`

	// LintDelta is the change in lint error count vs baseline.
	LintDelta int `json:"lintDelta"`

	// BlockedPromotion is true when a regression triggered a
	// status-promotion block on the platform side.
	BlockedPromotion bool `json:"blockedPromotion"`
}
