package claude

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// linearMCPDisallowList mirrors the legacy TS provider's hard block on
// Linear MCP tools. Agents must use the af-linear CLI / af_linear_*
// stdio MCP tools instead. Sourced from
// ../donmai-libraries/packages/core/src/providers/claude-provider.ts.
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
//	   [--settings <routing-pin>] \
//	   [optional flags...]
//
// (--print, --output-format, --verbose are required to enable the JSONL
// stream that the JSONL parser consumes.)
//
// --settings carries ONLY the routing pin (see pinnedRoutingEnv), stated on
// the layer the CLI resolves above the process environment and above repo
// settings files: a repo-local harness setting naming a routing var (for
// example env.ANTHROPIC_BASE_URL) must never redirect the session bearer.
// The JSON holds no hooks — the headless lane has no notice channel — and
// is absent entirely when the session carries no routing of its own, so a
// host-login spawn's argv is byte-identical to before the pin existed.
//
// resumeSessionID is empty for fresh spawns; when non-empty it is
// rendered as `--resume <id>`. v0.5.0 does not exercise this path
// because SupportsSessionResume=false, but the implementation is in
// place for the F.5 capability flip.
//
// Spec → CLI mapping accounts for every Spec field. Fields the CLI cannot
// honor are silently dropped per the capability-gating contract:
//
//	SessionName         → --name <name> on a fresh session
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
//	Endpoint            → applied BEFORE buildArgs by spawn via applyEndpoint
//	                      (endpoint.go): serving-host env knobs + binding
//	                      env/model are projected onto Spec.Env / Spec.Model
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
	// Routing pin first among the optional flags: it must be present
	// whenever the session carries routing of its own, on this lane exactly
	// as on the interactive lane (see spawnInteractive). Host-login sessions
	// pin nothing and skip the flag entirely.
	if pin := pinnedRoutingEnv(spec); len(pin) > 0 {
		if pinJSON, err := marshalRoutingPin(pin); err == nil && pinJSON != "" {
			argv = append(argv, "--settings", pinJSON)
		}
		// A pin that cannot be marshalled (string maps never fail in
		// practice) still spawns: the process environment the pin mirrors
		// remains, and failing the spawn would lie about what happened.
	}
	if spec.SessionName != "" && resumeSessionID == "" {
		argv = append(argv, "--name", spec.SessionName)
	}

	// Provide --add-dir so the agent can read outside the strict cwd
	// when the worktree references shared paths. Mirrors the legacy
	// SDK's `cwd` + project-root behavior.
	//
	// Under a session-root-v1 declaration every mutable repository path is
	// granted as an additional directory: the CLI's working directories are
	// exactly the declared write scope, which is how the agent can work in
	// each writable repository while its CWD stays the selected one. The
	// selected repository's own CWD entry is kept first (it is also the
	// process working directory set by the caller), and every other mutable
	// path is appended after, deduplicated case-sensitively against it.
	// Read-only declaration paths are never granted here: without a proven
	// executor boundary they must not enter the write scope.
	addDirs := additionalWorkdirs(spec)
	for _, dir := range addDirs {
		argv = append(argv, "--add-dir", dir)
	}

	if resumeSessionID != "" {
		argv = append(argv, "--resume", resumeSessionID)
	}

	// The interactive spawn mode applies the SAME mapping — see
	// interactive.go's interactiveArgsWith; the two must not diverge.
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	if spec.MaxTurns != nil && *spec.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(*spec.MaxTurns))
	}

	// Effort is emitted only when it is a level Claude recognises: an
	// unrecognised stored value (e.g. another harness's "none" or
	// "minimal") would be silently ignored by the CLI, which then falls
	// back to the operator's saved level while the session records a
	// different one. Omitting the flag leaves the operator's saved level in
	// place — except on stamped headless sessions, where the forced effort
	// env var (effort.go) carries the model default instead.
	if spec.Effort.Known() {
		argv = append(argv, "--effort", string(spec.Effort))
	}

	if spec.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", spec.SystemPromptAppend)
	}

	// Permission mode: autonomous sessions get bypassPermissions so
	// the CLI does not stall waiting for prompts when running in a
	// fleet. Non-autonomous sessions inherit the CLI default. The
	// interactive spawn mode applies the SAME mapping — see
	// interactive.go's interactiveArgs; the two must not diverge.
	if spec.Autonomous {
		argv = append(argv, "--permission-mode", "bypassPermissions")
	}

	// Allowed tools = Spec.AllowedTools ∪ Spec.MCPToolNames. The
	// MCPToolNames merge mirrors the legacy provider so headless
	// agents can call MCP tools without explicit allowlisting.
	// No capacity hint: summed len()s as an allocation size is the exact
	// shape a static scanner (go/allocation-size-overflow) flags as a
	// potential overflow; append grows the slice on demand just fine.
	allowed := make([]string, 0)
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

// marshalRoutingPin renders the routing pin as the --settings JSON string
// the headless lane passes. Only routingPinEnvNames members are honored —
// anything else is dropped rather than passed through, so a caller cannot
// smuggle an unrelated variable onto the CLI's winning layer. The envelope
// holds no hooks: the headless lane has no notice channel to declare.
func marshalRoutingPin(pin map[string]string) (string, error) {
	env := make(map[string]string, len(pin))
	for k, v := range pin {
		if _, ok := routingPinEnvNames[k]; !ok || v == "" {
			continue
		}
		env[k] = v
	}
	if len(env) == 0 {
		return "", nil
	}
	b, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// additionalWorkdirs returns the ordered --add-dir list for a spawn: the
// session working directory first, then every other declared mutable
// repository path. A spec without a repository authority keeps the legacy
// single entry.
func additionalWorkdirs(spec agent.Spec) []string {
	if spec.Cwd == "" {
		return nil
	}
	dirs := []string{spec.Cwd}
	if policy := spec.RepositoryAuthority; policy != nil {
		for _, mutablePath := range policy.MutablePaths {
			if mutablePath == "" || mutablePath == spec.Cwd {
				continue
			}
			dirs = append(dirs, mutablePath)
		}
	}
	return dirs
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
