package runner

import (
	"os"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// ── Code-intelligence capability: frozen wire contract ──────────────────────
//
// The runner exposes the in-box Go code-intelligence engine as a
// self-referential stdio MCP server. Both the runner-wiring lane (here) and the
// MCP-server lane build against the SAME frozen literals below
// (runs/2026-07-04-code-intel-capability); changing one without the other
// breaks the stdio handshake or the provider allow-list.
//
//   - Server name: "af-code-intelligence". The FQ tool-name prefix providers
//     allow-list is mcp__af-code-intelligence__<tool>, matching the pre-typed
//     example in agent/types.go (MCPToolNames doc) and the codex/gemini event
//     mapping.
//   - Subcommand: `donmai mcp code-intel` (hidden), owned by the sibling lane.
//   - Command: os.Executable() — self-referential, portable across every
//     execution target (local / docker / e2b / daytona / modal) with no
//     platform coupling to the in-box binary name or path.
//   - --root is ALWAYS the explicit, absolute session worktree path. Wave-0
//     traced that the runner process cwd is unreliable on every target
//     (no WORKDIR in the worker image; e2b/daemon cwd is host-controlled;
//     MCPServerConfig has no Cwd and dialStdio never sets cmd.Dir), so the root
//     is passed explicitly on every invocation — never inferred from cwd.
const codeIntelServerName = "af-code-intelligence"

// codeIntelToolsCSV joins the block's requested Tools verbatim (trimmed,
// non-empty), preserving the caller's order, for the stdio server's `--tools`
// flag. Returns "" when the block requests no subset (server default = all).
func codeIntelToolsCSV(ci *prompt.CodeIntelWork) string {
	if ci == nil {
		return ""
	}
	var parts []string
	for _, t := range ci.Tools {
		if s := strings.TrimSpace(t); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ",")
}

// codeIntelExecutable resolves the self-referential binary path used as the
// stdio server Command. Mirrors daemon/worker_command.go: os.Executable(),
// falling back to the brand CLI name (PATH lookup) when the executable cannot
// be resolved.
func codeIntelExecutable() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return prompt.ResolveBrand().BrandCLI
	}
	return exe
}

// codeIntelMCPEntry builds the runner-authored stdio MCP entry for the in-box
// code-intelligence engine per the frozen contract. root is the provisioned
// session worktree path and is ALWAYS passed explicitly as --root (never
// inferred from cwd). --repo-path and --tools are appended only when the block
// supplies them. Pure aside from resolving os.Executable().
func codeIntelMCPEntry(root string, ci *prompt.CodeIntelWork) agent.MCPServerConfig {
	args := []string{"mcp", "code-intel", "--root", root}
	if ci != nil {
		if rp := strings.TrimSpace(ci.RepoPath); rp != "" {
			args = append(args, "--repo-path", rp)
		}
		if tools := codeIntelToolsCSV(ci); tools != "" {
			args = append(args, "--tools", tools)
		}
	}
	return agent.MCPServerConfig{
		Name:    codeIntelServerName,
		Type:    "stdio",
		Command: codeIntelExecutable(),
		Args:    args,
	}
}
