package claude

import (
	"sort"
	"strconv"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// linearMCPDisallowList mirrors the legacy TS provider's hard block on
// Linear MCP tools. Agents must use the af-linear CLI / af_linear_*
// stdio MCP tools instead. Sourced from
// ../agentfactory/packages/core/src/providers/claude-provider.ts.
//
// The list is sorted to keep diffs stable.
var linearMCPDisallowList = []string{
	"AskUserQuestion",
	"mcp__claude_ai_Linear__create_attachment",
	"mcp__claude_ai_Linear__create_comment",
	"mcp__claude_ai_Linear__create_document",
	"mcp__claude_ai_Linear__create_issue_label",
	"mcp__claude_ai_Linear__delete_attachment",
	"mcp__claude_ai_Linear__extract_images",
	"mcp__claude_ai_Linear__get_attachment",
	"mcp__claude_ai_Linear__get_document",
	"mcp__claude_ai_Linear__get_issue",
	"mcp__claude_ai_Linear__get_issue_status",
	"mcp__claude_ai_Linear__get_milestone",
	"mcp__claude_ai_Linear__get_project",
	"mcp__claude_ai_Linear__get_team",
	"mcp__claude_ai_Linear__get_user",
	"mcp__claude_ai_Linear__list_comments",
	"mcp__claude_ai_Linear__list_cycles",
	"mcp__claude_ai_Linear__list_documents",
	"mcp__claude_ai_Linear__list_issue_labels",
	"mcp__claude_ai_Linear__list_issue_statuses",
	"mcp__claude_ai_Linear__list_issues",
	"mcp__claude_ai_Linear__list_milestones",
	"mcp__claude_ai_Linear__list_project_labels",
	"mcp__claude_ai_Linear__list_projects",
	"mcp__claude_ai_Linear__list_teams",
	"mcp__claude_ai_Linear__list_users",
	"mcp__claude_ai_Linear__save_issue",
	"mcp__claude_ai_Linear__save_milestone",
	"mcp__claude_ai_Linear__save_project",
	"mcp__claude_ai_Linear__search_documentation",
	"mcp__claude_ai_Linear__update_document",
}

// buildArgs translates an agent.Spec into the argv array passed to the
// claude CLI.
//
// The CLI is always invoked in "headless" mode:
//
//	claude -p \
//	   --output-format stream-json --verbose \
//	   --dangerously-skip-permissions \
//	   --add-dir <cwd> \
//	   [optional flags...]
//
// (--print, --output-format, --verbose are required to enable the JSONL
// stream that the JSONL parser consumes.)
//
// resumeSessionID is empty for fresh spawns; when non-empty it is
// rendered as `--resume <id>`. v0.5.0 does not exercise this path
// because SupportsSessionResume=false, but the implementation is in
// place for the F.5 capability flip.
//
// Spec → CLI mapping covers all 19 Spec fields. Fields the CLI cannot
// honor are silently dropped per the capability-gating contract:
//
//	Prompt              → stdin (returned alongside argv via stdinPrompt)
//	Cwd                 → cmd.Dir (set by handle.go) + --add-dir for read access
//	Env                 → cmd.Env (set by handle.go)
//	Autonomous          → --permission-mode bypassPermissions
//	SandboxEnabled      → no flag (CLI delegates to OS-level sandboxing); recorded
//	SandboxLevel        → no flag (Codex-only); recorded
//	AllowedTools        → --allowedTools <comma-separated>
//	DisallowedTools     → --disallowedTools <comma-separated> (always merged with linearMCPDisallowList when Autonomous)
//	MCPServers          → handled out-of-band via --mcp-config <tmpfile> (see mcp.go)
//	MCPToolNames        → appended to AllowedTools so headless agents can call them
//	MaxTurns            → --max-turns <n>
//	Model               → --model <id>
//	Effort              → --effort <level>
//	BaseInstructions    → no flag (Codex-only); silently dropped
//	SystemPromptAppend  → --append-system-prompt <text>
//	PermissionConfig    → no flag (Codex-only); silently dropped
//	CodeIntelEnforcement→ no flag (CLI does not expose canUseTool); silently dropped (v0.5.0)
//	ProviderConfig      → no flag (provider-specific opaque map); silently dropped
//	SubAgentProvider    → no flag; recorded for future Task-tool use
//	OnProcessSpawned    → invoked by handle.go after exec start
//
// The mcpConfigPath argument is the path to the per-session
// `--mcp-config` JSON tmpfile prepared by writeMCPConfig (see mcp.go).
// When empty (no MCP servers configured) the flag is omitted.
func buildArgs(spec agent.Spec, mcpConfigPath, resumeSessionID string) (argv []string, stdinPrompt string) {
	argv = []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}

	// Provide --add-dir so the agent can read outside the strict cwd
	// when the worktree references shared paths. Mirrors the legacy
	// SDK's `cwd` + project-root behavior.
	if spec.Cwd != "" {
		argv = append(argv, "--add-dir", spec.Cwd)
	}

	if resumeSessionID != "" {
		argv = append(argv, "--resume", resumeSessionID)
	}

	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	if spec.MaxTurns != nil && *spec.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(*spec.MaxTurns))
	}

	if spec.Effort != "" {
		argv = append(argv, "--effort", string(spec.Effort))
	}

	if spec.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", spec.SystemPromptAppend)
	}

	// Permission mode: autonomous sessions get bypassPermissions so
	// the CLI does not stall waiting for prompts when running in a
	// fleet. Interactive sessions inherit the CLI default.
	if spec.Autonomous {
		argv = append(argv, "--permission-mode", "bypassPermissions")
	}

	// Allowed tools = Spec.AllowedTools ∪ Spec.MCPToolNames. The
	// MCPToolNames merge mirrors the legacy provider so headless
	// agents can call MCP tools without explicit allowlisting.
	allowed := make([]string, 0, len(spec.AllowedTools)+len(spec.MCPToolNames))
	allowed = append(allowed, spec.AllowedTools...)
	allowed = append(allowed, spec.MCPToolNames...)
	allowed = dedupAndSort(allowed)
	if len(allowed) > 0 {
		argv = append(argv, "--allowedTools", strings.Join(allowed, ","))
	}

	// Disallowed tools always include the Linear MCP block list when
	// the session is autonomous (matches the legacy provider).
	disallowed := append([]string(nil), spec.DisallowedTools...)
	if spec.Autonomous {
		disallowed = append(disallowed, linearMCPDisallowList...)
	}
	disallowed = dedupAndSort(disallowed)
	if len(disallowed) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(disallowed, ","))
	}

	if mcpConfigPath != "" {
		argv = append(argv, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}

	// Prompt is delivered via stdin to avoid argv-length limits and
	// to keep large prompts off the process listing. Callers wire
	// this into cmd.Stdin in handle.go.
	stdinPrompt = spec.Prompt
	return argv, stdinPrompt
}

// dedupAndSort returns a stable, deduplicated, sorted copy of s with
// blanks removed. Sorting keeps argv stable across invocations so
// tests can assert byte-for-byte equality.
func dedupAndSort(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// composeEnv builds the child process environment by merging
// parentEnv (typically os.Environ()) with spec.Env. Per F.1.1 §3.1
// the runner is responsible for AGENT_ENV_BLOCKLIST filtering before
// calling Spawn — this provider trusts the spec.Env it receives.
//
// Order: parentEnv first, then spec.Env entries appended; later
// entries override earlier ones via standard exec.Cmd semantics
// (the kernel uses the last occurrence of each name on Unix).
func composeEnv(parentEnv []string, specEnv map[string]string) []string {
	out := make([]string, 0, len(parentEnv)+len(specEnv))
	out = append(out, parentEnv...)
	if len(specEnv) == 0 {
		return out
	}
	// Sort keys for deterministic order — important for tests.
	keys := make([]string, 0, len(specEnv))
	for k := range specEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+specEnv[k])
	}
	return out
}
