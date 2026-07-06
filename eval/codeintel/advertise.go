package codeintel

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// AdvertiseMode selects how the WITH arm learns the code-intel tools exist
// (brief 06 §4.3.3, Q4). It is a swappable harness parameter, NOT baked into the
// grading logic.
type AdvertiseMode string

const (
	// AdvertiseMCP is the default/authoritative surface: the real
	// af-code-intelligence stdio MCP server shipped in v0.50.0, spawned per the
	// frozen wire contract exactly as a live session would.
	AdvertiseMCP AdvertiseMode = "mcp"
	// AdvertisePromptHelp is the alternate: no MCP; a system-prompt suffix
	// generated from LIVE `donmai code --help` output (never the stale README —
	// brief 06 §5 risk 7). Used to A/B the advertisement mechanism itself.
	AdvertisePromptHelp AdvertiseMode = "prompt-help"
)

// ParseAdvertiseMode validates a mode string.
func ParseAdvertiseMode(s string) (AdvertiseMode, error) {
	switch AdvertiseMode(s) {
	case AdvertiseMCP:
		return AdvertiseMCP, nil
	case AdvertisePromptHelp:
		return AdvertisePromptHelp, nil
	default:
		return "", fmt.Errorf("unknown advertise mode %q (want %q or %q)", s, AdvertiseMCP, AdvertisePromptHelp)
	}
}

// ── Frozen wire contract (consumed from runner/codeintel.go; NOT redefined
// there — mirrored here so the harness authors the identical entry a real
// session would). ────────────────────────────────────────────────────────────

// CodeIntelServerName is the MCP server name (mcp__af-code-intelligence__*).
const CodeIntelServerName = "af-code-intelligence"

// codeIntelToolNames is the canonical six-tool set, in the frozen order.
var codeIntelToolNames = []string{
	"af_code_get_repo_map", "af_code_search_symbols", "af_code_search_code",
	"af_code_check_duplicate", "af_code_find_type_usages", "af_code_validate_cross_deps",
}

// fqName returns the fully-qualified MCP name for one tool
// (mcp__af-code-intelligence__<tool>).
func fqName(tool string) string {
	return "mcp__" + CodeIntelServerName + "__" + tool
}

// fqNames maps tool names to their fully-qualified MCP forms.
func fqNames(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, fqName(t))
	}
	return out
}

// ── WS2: core-subset advertisement (discovery-tax reduction) ────────────────
//
// Discovery-tax finding: every pilot WITH session opened with a client-side
// ToolSearch round-trip loading the deferred MCP tools before the first real
// tool call (pilot-drycase.txt:82, plat-fs003-dry.txt:74; pilot-lu-dry.txt:74
// loaded THREE tools in one select). The deferral decision belongs to the
// claude CLI, not this server: the tools registered deferred even though the
// six-tool surface was only ~3.5KB of schema+description, so the server side
// cannot force eager loading. The two real levers are (a) advertising fewer
// tools per arm — this subset — and (b) smaller schemas (WS11); both shrink
// the ToolSearch payload and the loaded-schema context on every later turn.
//
// Subset rule: the WITH arm carries the CORE FOUR (get_repo_map,
// search_symbols, search_code, check_duplicate) for every family, plus
// find_type_usages for the refactor family only — the one family whose job
// (pre-edit cross-file site enumeration) that tool serves.
// validate_cross_deps is never in the default subset (no differentiated job
// on the current benchmark; WS12 pending). The rule is a DETERMINISTIC
// function of the case family only — never of case content or ground truth.
// Fairness: production stamps a fixed work.codeIntel tool list per project
// and the platform prompt partial describes family-appropriate tools, so a
// family-conditional subset mirrors what a real session advertises; the
// family is already explicit in the task prompt itself, so no per-task hint
// leaks into the WITH arm beyond what production grants.
func advertisedToolSubset(family TaskType, allTools bool) []string {
	switch {
	case allTools:
		return append([]string(nil), codeIntelToolNames...)
	case family == TaskRefactorAcrossFiles:
		// codeIntelToolNames order: repo_map, search_symbols, search_code,
		// check_duplicate, find_type_usages, validate_cross_deps.
		return append([]string(nil), codeIntelToolNames[:5]...)
	default:
		return append([]string(nil), codeIntelToolNames[:4]...)
	}
}

// BuildMCPEntry authors the frozen af-code-intelligence stdio MCP entry for the
// WITH arm: Command = the donmai binary, Args = ["mcp","code-intel","--root",
// <workarea>] (+ --repo-path when scoped). This is byte-identical to what
// runner.codeIntelMCPEntry authors for a live session (self-referential
// os.Executable() there; the explicit --donmai-bin here), so the harness
// exercises the real surface, not a mock.
func BuildMCPEntry(donmaiBin, workarea, repoPath string) agent.MCPServerConfig {
	args := []string{"mcp", "code-intel", "--root", workarea}
	if rp := strings.TrimSpace(repoPath); rp != "" {
		args = append(args, "--repo-path", rp)
	}
	return agent.MCPServerConfig{
		Name:    CodeIntelServerName,
		Type:    "stdio",
		Command: donmaiBin,
		Args:    args,
	}
}

// BuildMCPEntryWithTools is BuildMCPEntry plus the server's EXISTING --tools
// allow-list (afcli/mcp.go): the spawned server registers only the named
// subset. Empty tools = no --tools flag = the server's default all-six
// registration, so the frozen wire contract is untouched — surface reduction
// happens only when the eval opts in.
func BuildMCPEntryWithTools(donmaiBin, workarea, repoPath string, tools []string) agent.MCPServerConfig {
	entry := BuildMCPEntry(donmaiBin, workarea, repoPath)
	if len(tools) > 0 {
		entry.Args = append(entry.Args, "--tools", strings.Join(tools, ","))
	}
	return entry
}

// Advertisement is the swappable WITH-arm advertisement mechanism.
type Advertisement interface {
	Mode() AdvertiseMode
	// Apply produces the WITH-arm attachment for one case: the MCP servers to
	// wire (empty for prompt-help) and a system-prompt suffix telling the agent
	// the tools exist. family selects the WS2 advertised-tool subset (a
	// deterministic function of the family only — see advertisedToolSubset).
	Apply(ctx context.Context, donmaiBin, workarea, repoPath string, family TaskType, env []string) (servers []agent.MCPServerConfig, promptSuffix string, err error)
	// AdvertisedToolNames is the set of tool identifiers the arm was told about
	// (FQ MCP names, or CLI subcommands) — recorded on the transcript so the
	// tool-use grader knows what adoption was possible.
	AdvertisedToolNames(family TaskType) []string
}

// NewAdvertisement returns the advertisement implementation for mode.
// allTools=true restores the full six-tool surface (no WS2 subset) — the
// escape hatch for measuring the subset's own effect.
func NewAdvertisement(mode AdvertiseMode, allTools bool) Advertisement {
	if mode == AdvertisePromptHelp {
		return promptHelpAdvertisement{}
	}
	return mcpAdvertisement{allTools: allTools}
}

// ── MCP advertisement (default) ──────────────────────────────────────────────

type mcpAdvertisement struct {
	// allTools disables the WS2 core-subset rule and advertises all six.
	allTools bool
}

func (mcpAdvertisement) Mode() AdvertiseMode { return AdvertiseMCP }

func (a mcpAdvertisement) Apply(_ context.Context, donmaiBin, workarea, repoPath string, family TaskType, _ []string) ([]agent.MCPServerConfig, string, error) {
	subset := advertisedToolSubset(family, a.allTools)
	// The --tools allow-list is passed only when a strict subset is selected;
	// all-tools mode spawns the server with its default all-six registration.
	var toolsArg []string
	if len(subset) < len(codeIntelToolNames) {
		toolsArg = subset
	}
	entry := BuildMCPEntryWithTools(donmaiBin, workarea, repoPath, toolsArg)

	in := make(map[string]bool, len(subset))
	for _, t := range subset {
		in[t] = true
	}
	// WS4: capability-anchored, task-conditional framing. Each bullet names the
	// job the tool wins over grep+read; trivial exact-identifier lookups are
	// explicitly de-scoped to grep. The earlier blanket "prefer them over
	// grep/glob/find" framing steered adoption onto tasks grep already wins
	// (pilot: 1.0–2.10x token cost at +0pp success) — never reintroduce it.
	// Bullets appear only for tools the arm actually carries (WS2 subset).
	var b strings.Builder
	b.WriteString("# Code Intelligence\n\n")
	b.WriteString("This session has the af-code-intelligence MCP tools. Each is built for a job " +
		"where grep+read is weak; use them in exactly these situations:\n\n")
	if in["af_code_get_repo_map"] {
		b.WriteString("- Orienting in an unfamiliar repo or subsystem: call " + fqName("af_code_get_repo_map") +
			" FIRST — it ranks files by import centrality, which reading files one by one cannot.\n")
	}
	if in["af_code_find_type_usages"] {
		b.WriteString("- Before ANY cross-file rename or refactor: call " + fqName("af_code_find_type_usages") +
			" to enumerate every affected site BEFORE editing, then work from that list.\n")
	}
	if in["af_code_check_duplicate"] {
		b.WriteString("- Checking whether code like this already exists (exact or near-duplicate): call " +
			fqName("af_code_check_duplicate") + " with the candidate snippet.\n")
	}
	if in["af_code_search_symbols"] && in["af_code_search_code"] {
		b.WriteString("- Searching by name or concept across the codebase: " + fqName("af_code_search_symbols") +
			" (symbol names) or " + fqName("af_code_search_code") + " (code content).\n")
	}
	b.WriteString("\nFor an exact single-identifier lookup, plain grep is fine — do not add a tool call.\n")
	return []agent.MCPServerConfig{entry}, b.String(), nil
}

func (a mcpAdvertisement) AdvertisedToolNames(family TaskType) []string {
	return fqNames(advertisedToolSubset(family, a.allTools))
}

// ── Prompt-help advertisement (alternate) ────────────────────────────────────

type promptHelpAdvertisement struct{}

func (promptHelpAdvertisement) Mode() AdvertiseMode { return AdvertisePromptHelp }

// Apply runs LIVE `donmai code --help` (and captures the subcommand list) via the
// arm env, and returns it as the prompt suffix. It deliberately reads the live
// binary's help rather than any doc so the WITH arm never advertises a command
// that doesn't exist (brief 06 §5 risk 7).
func (promptHelpAdvertisement) Apply(ctx context.Context, donmaiBin, _, _ string, _ TaskType, env []string) ([]agent.MCPServerConfig, string, error) {
	help, err := runHelp(ctx, donmaiBin, env, "code", "--help")
	if err != nil {
		return nil, "", fmt.Errorf("prompt-help: capture `donmai code --help`: %w", err)
	}
	// Same WS4 framing contract as the MCP variant: task-conditional bullets,
	// no blanket prefer-over-grep, and an explicit grep de-scope clause.
	var b strings.Builder
	b.WriteString("# Code Intelligence\n\n")
	b.WriteString("This session has code-intelligence CLI commands available (run them with Bash). " +
		"Each is built for a job where grep+read is weak: `get-repo-map` FIRST when orienting in an " +
		"unfamiliar repo (ranks files by import centrality); `find-type-usages` BEFORE any cross-file " +
		"rename/refactor to enumerate every affected site before editing; `check-duplicate` to check " +
		"whether code like a candidate snippet already exists. For an exact single-identifier lookup, " +
		"plain grep is fine — do not add a tool call. Live `donmai code --help`:\n\n")
	b.WriteString("```\n")
	b.WriteString(strings.TrimSpace(help))
	b.WriteString("\n```\n")
	return nil, b.String(), nil
}

// AdvertisedToolNames lists the CLI subcommand forms (the prompt-help arm cannot
// advertise MCP FQ names, and its live --help output always shows every
// subcommand, so the WS2 subset does not apply).
func (promptHelpAdvertisement) AdvertisedToolNames(TaskType) []string {
	return []string{
		"donmai code get-repo-map", "donmai code search-symbols", "donmai code search-code",
		"donmai code check-duplicate", "donmai code find-type-usages", "donmai code validate-cross-deps",
	}
}

// runHelp execs the donmai binary with the given args and captures combined
// output. Used to author the prompt-help advertisement from live help text.
func runHelp(ctx context.Context, donmaiBin string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, donmaiBin, args...) // nolint:gosec // donmaiBin is the operator-supplied harness binary path.
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}
