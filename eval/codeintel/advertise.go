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

// fqToolNames returns the fully-qualified MCP tool names
// (mcp__af-code-intelligence__<tool>).
func fqToolNames() []string {
	out := make([]string, 0, len(codeIntelToolNames))
	for _, t := range codeIntelToolNames {
		out = append(out, "mcp__"+CodeIntelServerName+"__"+t)
	}
	return out
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

// Advertisement is the swappable WITH-arm advertisement mechanism.
type Advertisement interface {
	Mode() AdvertiseMode
	// Apply produces the WITH-arm attachment: the MCP servers to wire (empty for
	// prompt-help) and a system-prompt suffix telling the agent the tools exist.
	Apply(ctx context.Context, donmaiBin, workarea, repoPath string, env []string) (servers []agent.MCPServerConfig, promptSuffix string, err error)
	// AdvertisedToolNames is the set of tool identifiers the arm was told about
	// (FQ MCP names, or CLI subcommands) — recorded on the transcript so the
	// tool-use grader knows what adoption was possible.
	AdvertisedToolNames() []string
}

// NewAdvertisement returns the advertisement implementation for mode.
func NewAdvertisement(mode AdvertiseMode) Advertisement {
	if mode == AdvertisePromptHelp {
		return promptHelpAdvertisement{}
	}
	return mcpAdvertisement{}
}

// ── MCP advertisement (default) ──────────────────────────────────────────────

type mcpAdvertisement struct{}

func (mcpAdvertisement) Mode() AdvertiseMode { return AdvertiseMCP }

func (mcpAdvertisement) Apply(_ context.Context, donmaiBin, workarea, repoPath string, _ []string) ([]agent.MCPServerConfig, string, error) {
	entry := BuildMCPEntry(donmaiBin, workarea, repoPath)
	var b strings.Builder
	b.WriteString("# Code Intelligence\n\n")
	b.WriteString("This session has the af-code-intelligence tools available over MCP. " +
		"Prefer them over ad-hoc grep/glob/find when navigating or searching this repository:\n")
	for _, fq := range fqToolNames() {
		b.WriteString("\n- " + fq)
	}
	return []agent.MCPServerConfig{entry}, b.String(), nil
}

func (mcpAdvertisement) AdvertisedToolNames() []string { return fqToolNames() }

// ── Prompt-help advertisement (alternate) ────────────────────────────────────

type promptHelpAdvertisement struct{}

func (promptHelpAdvertisement) Mode() AdvertiseMode { return AdvertisePromptHelp }

// Apply runs LIVE `donmai code --help` (and captures the subcommand list) via the
// arm env, and returns it as the prompt suffix. It deliberately reads the live
// binary's help rather than any doc so the WITH arm never advertises a command
// that doesn't exist (brief 06 §5 risk 7).
func (promptHelpAdvertisement) Apply(ctx context.Context, donmaiBin, _, _ string, env []string) ([]agent.MCPServerConfig, string, error) {
	help, err := runHelp(ctx, donmaiBin, env, "code", "--help")
	if err != nil {
		return nil, "", fmt.Errorf("prompt-help: capture `donmai code --help`: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Code Intelligence\n\n")
	b.WriteString("This session has code-intelligence CLI commands available (run them with Bash). " +
		"Prefer them over ad-hoc grep/glob/find. Live `donmai code --help`:\n\n")
	b.WriteString("```\n")
	b.WriteString(strings.TrimSpace(help))
	b.WriteString("\n```\n")
	return nil, b.String(), nil
}

// AdvertisedToolNames lists the CLI subcommand forms (the prompt-help arm cannot
// advertise MCP FQ names).
func (promptHelpAdvertisement) AdvertisedToolNames() []string {
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
