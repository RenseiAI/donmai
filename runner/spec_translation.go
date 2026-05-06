package runner

import (
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/mcp"
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

	// MCPServers is the list of MCP stdio configs the runtime/mcp
	// builder produced. Empty when no plugins are enabled.
	MCPServers []agent.MCPServerConfig

	// Env is the merged session env (output of runtime/env.Compose,
	// already rebuilt as a map for Spec.Env).
	Env map[string]string

	// Autonomous mirrors the daemon's session-mode flag — true for
	// agent sessions invoked from the work queue.
	Autonomous bool
}

// translateSpec converts a QueuedWork plus per-session SpecInputs into
// an agent.Spec ready for Provider.Spawn. Pure function; no I/O.
//
// Capability gating is applied here: fields the resolved provider does
// not advertise are silently zeroed so providers do not have to
// defensively ignore them. The runner does not error on
// capability-mismatch — that path runs in the loop's recovery layer.
func translateSpec(qw QueuedWork, caps agent.Capabilities, in SpecInputs) agent.Spec {
	spec := agent.Spec{
		Prompt:             in.Prompt,
		Cwd:                in.Cwd,
		Env:                in.Env,
		Autonomous:         in.Autonomous,
		SandboxEnabled:     true,
		SandboxLevel:       agent.SandboxWorkspaceWrite,
		AllowedTools:       defaultAllowedTools(),
		DisallowedTools:    defaultDisallowedTools(),
		MCPServers:         in.MCPServers,
		Model:              strings.TrimSpace(qw.ResolvedProfile.Model),
		SystemPromptAppend: in.SystemPromptAppend,
		ProviderConfig:     copyProviderConfig(qw.ResolvedProfile.ProviderConfig),
	}

	// Capability-gated fields — silently zeroed when the resolved
	// provider does not declare support. The runner emits a Debug log
	// in the loop when it strips a value the caller set, so operators
	// can detect silently-ignored knobs.
	if caps.SupportsReasoningEffort && qw.ResolvedProfile.Effort != "" {
		spec.Effort = qw.ResolvedProfile.Effort
	}

	// MCP tool plugins: only forward MCPServers when the provider
	// declares SupportsToolPlugins AND honours the Spec field shape
	// (AcceptsMcpServerSpec). Other providers ignore the field anyway,
	// but zeroing it keeps the on-the-wire Spec faithful to what the
	// provider will actually consume. Per 002 v2 §"Tool-use surface".
	if !caps.SupportsToolPlugins || !caps.AcceptsMcpServerSpec {
		spec.MCPServers = nil
	}

	// AllowedTools: only forward when the provider honours the Spec
	// field shape (AcceptsAllowedToolsList). Codex routes per-tool
	// permission through the approval bridge (Spec.PermissionConfig)
	// and ignores AllowedTools; zero the field to match what the
	// provider actually consumes. Behaviour is warn-and-ignore in the
	// loop's spec-prep path: stripped values surface as a Debug log
	// (see loop.go) so operators can detect silently dropped knobs.
	if !caps.AcceptsAllowedToolsList {
		spec.AllowedTools = nil
	}

	// MCPToolNames is derived from MCPServers — every server we ship
	// today (af_linear, af_code) advertises tools under its name
	// prefix; the loop will populate MCPToolNames after building the
	// MCP config when a richer derivation lands. v0.5.0 leaves the
	// list empty; providers that need it (codex) accept the empty
	// list as "all tools allowed".

	return spec
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
