package runner

import (
	"log/slog"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// SpecInputs are the per-session inputs the [translateSpec] helper
// merges with the QueuedWork to produce an [agent.Spec]. Splitting
// these out keeps spec_translation pure (no I/O, no platform calls)
// and makes the loop easy to test in isolation.
type SpecInputs struct {
	// Cwd is the worktree path the worktree manager just provisioned.
	Cwd string

	// Prompt is the rendered user prompt (from prompt.Builder.Build).
	Prompt string

	// SystemPromptAppend is the rendered system-append block from the
	// prompt builder; threaded into Spec.SystemPromptAppend for
	// providers that consume it.
	SystemPromptAppend string

	// PromptPlan preserves prompt authority/provenance until the exact
	// harness/mode adapter compiles it onto native wire/config surfaces.
	PromptPlan *agent.PromptPlan

	// InitialContext is large/volatile session context (e.g. recalled
	// agent memory) the runner routes through Spec.InitialContext so it
	// rides the first turn's input rather than the re-sent system-prompt
	// prefix. Empty unless the resolved provider declares
	// Capabilities.SupportsTurnInputContext.
	InitialContext string

	// MCPServers is the list of MCP stdio configs the runtime/mcp
	// builder produced. Empty when no plugins are enabled.
	MCPServers []agent.MCPServerConfig

	// Env is the merged session env (output of runtime/env.Compose,
	// already rebuilt as a map for Spec.Env).
	Env map[string]string

	// Autonomous mirrors the daemon's session-mode flag — true for
	// agent sessions invoked from the work queue.
	Autonomous bool

	// Logger is reserved for structured translation diagnostics. Tool and MCP
	// requirements are never logger-only drops: exact adapters apply or deny
	// them before provider side effects. Nil is safe.
	Logger *slog.Logger

	// ProviderName names the resolved provider for structured translation
	// diagnostics. Empty is safe.
	ProviderName string
}

// translateSpec converts a QueuedWork plus per-session SpecInputs into
// an agent.Spec ready for Provider.Spawn. Pure function; no I/O.
//
// Capability translation is applied here only when the provider has an exact
// alternate grammar (for example Codex PermissionConfig). Requested tool/MCP
// fields are never zeroed: the exact harness adaptation profile either applies
// them or returns a typed pre-spawn denial.
func translateSpec(qw QueuedWork, caps agent.Capabilities, in SpecInputs) agent.Spec {
	// AllowedTools resolution: the agent card is AUTHORITATIVE when it
	// supplies an explicit allowlist (WS5) — the runner uses it verbatim in
	// place of its curated default. When the card sends none, the runner
	// falls back to defaultAllowedTools() (backward-compatible). The
	// DisallowedTools floor is applied below regardless.
	var allowedTools []string
	if !qw.isInteractive() {
		allowedTools = defaultAllowedTools()
	}
	if len(qw.AllowedTools) > 0 {
		allowedTools = append([]string(nil), qw.AllowedTools...)
	}
	var disallowedTools []string
	if !qw.isInteractive() {
		disallowedTools = defaultDisallowedTools()
	}

	spec := agent.Spec{
		SessionName:        strings.TrimSpace(qw.SessionName),
		Prompt:             in.Prompt,
		Cwd:                in.Cwd,
		Env:                in.Env,
		Autonomous:         in.Autonomous,
		SandboxEnabled:     true,
		SandboxLevel:       resolveSandboxLevel(qw, in.Logger),
		AllowedTools:       allowedTools,
		DisallowedTools:    disallowedTools,
		MCPServers:         in.MCPServers,
		Model:              strings.TrimSpace(qw.ResolvedProfile.Model),
		Effort:             qw.ResolvedProfile.Effort,
		SystemPromptAppend: in.SystemPromptAppend,
		PromptPlan:         in.PromptPlan,
		InitialContext:     in.InitialContext,
		ProviderConfig:     copyProviderConfig(qw.ResolvedProfile.ProviderConfig),
		Endpoint:           copyEndpointBinding(qw.ResolvedProfile.Endpoint),
	}

	// InitialContext is a legacy compatibility field. The typed PromptPlan
	// remains authoritative and is deliberately not capability-gated here;
	// its exact harness profile either delivers, explicitly downgrades, or
	// rejects each context item before spawn.
	if !caps.SupportsTurnInputContext {
		spec.InitialContext = ""
	}

	// Platform-supplied disallowed-tool patterns (Option B).
	// Appended AFTER the runner's own defaultDisallowedTools() baseline
	// so the static floor is never replaced, only extended.
	// qw.DisallowedTools is the embedded prompt.QueuedWork field stamped
	// by the platform's stampDisallowedTools() helper (platform PR #196).
	// Applied BEFORE the AllowedTools gate below so the codex
	// PermissionConfig bridge sees the full disallowed set.
	if len(qw.DisallowedTools) > 0 {
		spec.DisallowedTools = append(spec.DisallowedTools, qw.DisallowedTools...)
	}

	// AllowedTools: only forward as a flat allowlist when the provider
	// honours the Spec field shape (AcceptsAllowedToolsList). When it does
	// not, the field would otherwise be silently zeroed. Two sub-cases:
	//
	//   1. Codex — does NOT accept a flat allowlist but DOES consume a
	//      structured PermissionConfig (NeedsPermissionConfig). Rather than
	//      drop the card's allow/deny intent, we TRANSLATE it into the
	//      approval bridge: AllowedTools → PermissionConfig.AllowPatterns and
	//      the full DisallowedTools set → PermissionConfig.DisallowPatterns.
	//      The codex approval bridge (provider/harness/codex/approval.go)
	//      already consumes PermissionConfig, so the card's intent reaches
	//      the agent instead of being dropped.
	//
	// Providers without either exact surface retain the non-empty field. Their
	// tool/lifecycle adaptation profile rejects it before spawn; there is no
	// logger-only drop path.
	if !caps.AcceptsAllowedToolsList {
		if caps.NeedsPermissionConfig {
			pc := spec.PermissionConfig
			if pc == nil {
				pc = &agent.PermissionConfig{}
			}
			if len(spec.AllowedTools) > 0 {
				pc.AllowPatterns = append(pc.AllowPatterns, spec.AllowedTools...)
			}
			if len(spec.DisallowedTools) > 0 {
				pc.DisallowPatterns = append(pc.DisallowPatterns, spec.DisallowedTools...)
			}
			if len(pc.AllowPatterns) > 0 || len(pc.DisallowPatterns) > 0 {
				spec.PermissionConfig = pc
			}
			// The exact alternate grammar now owns the policy intent; retaining
			// either flat list would incorrectly ask the harness to apply it twice.
			spec.AllowedTools = nil
			spec.DisallowedTools = nil
		}
	}

	// MCPToolNames: allow-list the fully-qualified code-intel tool names so
	// autonomous agents may call them without a permission prompt. Gated on the
	// The exact harness adapter, not a coupled boolean pair, now decides whether
	// these names can be enforced. Unsupported names remain present and deny.
	if qw.CodeIntel != nil {
		spec.MCPToolNames = codeIntelFQToolNames(qw.CodeIntel)
	}

	return spec
}

// resolveSandboxLevel maps QueuedWork.PermissionProfile onto the sandbox tier
// forwarded to agent.Spec.SandboxLevel. Absent, empty, or the explicit
// PermissionProfileWorkspaceWrite value all resolve to
// agent.SandboxWorkspaceWrite — the byte-for-byte pre-field hardcoded default
// every prior release shipped.
//
// PermissionProfileAutonomous resolves to agent.SandboxFullAccess. A fully
// autonomous headless run has no approver present, and
// agent.SandboxWorkspaceWrite raises a sandbox-escalation review for git
// operations that need repo network/metadata access (fetch, cherry-pick) —
// with nobody present to answer it, the harness auto-rejects the escalation
// and the git operation fails. Full access removes the sandbox boundary the
// escalation would otherwise come from.
//
// An unrecognized value is fail-safe, never fail-closed: it logs a warning
// naming the bad value and falls back to workspace-write rather than failing
// the spawn. logger nil-safe (mirrors logMCPGatewayBearerExpiry).
func resolveSandboxLevel(qw QueuedWork, logger *slog.Logger) agent.SandboxLevel {
	switch qw.PermissionProfile {
	case "", PermissionProfileWorkspaceWrite:
		return agent.SandboxWorkspaceWrite
	case PermissionProfileAutonomous:
		return agent.SandboxFullAccess
	default:
		if logger != nil {
			logger.Warn("[runner] unrecognized permissionProfile; falling back to workspace-write",
				"sessionId", qw.SessionID,
				"permissionProfile", string(qw.PermissionProfile),
			)
		}
		return agent.SandboxWorkspaceWrite
	}
}

// defaultAllowedTools is the curated Bash + edit + read + grep
// allowlist every Claude session ships with by default. The list
// mirrors the legacy TS createAutonomousAllowedTools() output and is
// kept short on purpose — operators expand it via repository config
// when a project needs additional shell prefixes.
//
// Codex/stub providers ignore this list (they have their own
// permission grammar via Spec.PermissionConfig); only Claude consumes
// it, but the list lives here so spec translation stays pure.
func defaultAllowedTools() []string {
	return []string{
		"Bash(pnpm:*)",
		"Bash(git:*)",
		"Bash(gh:*)",
		"Bash(go:*)",
		"Bash(make:*)",
		"Bash(node:*)",
		"Edit",
		"Write",
		"Read",
		"Grep",
		"Glob",
		"Task",
	}
}

// defaultDisallowedTools is the verbatim port of the legacy TS
// disallowedTools list. AskUserQuestion is forbidden in autonomous
// mode; the mcp__claude_ai_Linear__* prefix blocks the Linear MCP
// tools so agents go through `pnpm af-linear` instead (per AGENTS.md).
func defaultDisallowedTools() []string {
	return []string{
		"AskUserQuestion",
		"mcp__claude_ai_Linear__*",
	}
}

// copyProviderConfig returns a defensive copy of the resolved profile's
// provider-config map so mutation on the Spec side does not affect the
// QueuedWork (which a caller may inspect post-Run).
func copyProviderConfig(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyEndpointBinding returns an independent binding so provider-side mutation
// cannot affect the queued work.
func copyEndpointBinding(in *agent.EndpointBinding) *agent.EndpointBinding {
	if in == nil {
		return nil
	}
	out := *in
	if in.Env != nil {
		out.Env = make(map[string]string, len(in.Env))
		for k, v := range in.Env {
			out.Env[k] = v
		}
	}
	return &out
}

// MCPConfigPath wraps the runtime/mcp.Builder output so the loop has a
// single path-and-cleanup pair to thread through Spec construction +
// teardown. The cleanup closure is no-op when no MCP servers were
// requested, matching mcp.Builder.Build semantics.
type MCPConfigPath struct {
	Path    string
	Cleanup func()
}

// buildMCPConfigPath calls the runtime/mcp builder for the given
// servers and returns a MCPConfigPath. Empty servers returns a
// no-op cleanup and an empty path so the caller can defer
// unconditionally. Errors propagate as-is — they almost always
// indicate a programmer error (empty server name) the caller should
// surface.
func buildMCPConfigPath(b *mcp.Builder, servers []agent.MCPServerConfig) (MCPConfigPath, error) {
	if b == nil {
		b = mcp.NewBuilder()
	}
	path, cleanup, err := b.Build(servers)
	if err != nil {
		return MCPConfigPath{}, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	return MCPConfigPath{Path: path, Cleanup: cleanup}, nil
}
