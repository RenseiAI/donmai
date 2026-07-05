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

// codeIntelFQPrefix is the fully-qualified MCP tool-name prefix for the
// code-intel server (mcp__<serverName>__). Concatenated with a tool name it
// yields e.g. "mcp__af-code-intelligence__af_code_get_repo_map".
const codeIntelFQPrefix = "mcp__" + codeIntelServerName + "__"

// codeIntelToolMeta describes one code-intel tool: its unqualified MCP name,
// the equivalent `donmai code <subcommand>` form (for the Bash-CLI fallback
// guidance on providers that ignore MCP specs), and one-line when-to-use
// guidance kept consistent with the CLI help in afcli/code.go.
type codeIntelToolMeta struct {
	// tool is the unqualified MCP tool name (e.g. "af_code_search_symbols").
	tool string
	// subcommand is the equivalent `code` subcommand + arg hint.
	subcommand string
	// guidance is a compact one-line when-to-use string.
	guidance string
}

// codeIntelTools is the canonical, ordered set of the six code-intel tools —
// the single source of truth the entry args, the FQ allow-list, and the prompt
// partial all derive from. Order matches the frozen wire contract.
var codeIntelTools = []codeIntelToolMeta{
	{
		tool:       "af_code_get_repo_map",
		subcommand: "get-repo-map",
		guidance:   "get a PageRank-ranked map of the most important files and their key symbols to orient in an unfamiliar repo",
	},
	{
		tool:       "af_code_search_symbols",
		subcommand: "search-symbols <query>",
		guidance:   "find a function, class, interface, or type by name or query (BM25 over the symbol index) instead of grepping",
	},
	{
		tool:       "af_code_search_code",
		subcommand: "search-code <query>",
		guidance:   "BM25 keyword search over code content (upgrades to hybrid vector + rerank when VOYAGE_AI_API_KEY / COHERE_API_KEY are set)",
	},
	{
		tool:       "af_code_check_duplicate",
		subcommand: "check-duplicate --content <snippet>",
		guidance:   "check whether a snippet is an exact (xxHash64) or near (SimHash) duplicate before writing new code",
	},
	{
		tool:       "af_code_find_type_usages",
		subcommand: "find-type-usages <TypeName>",
		guidance:   "find every switch/case, mapping, and usage site of a union type or enum before adding a member",
	},
	{
		tool:       "af_code_validate_cross_deps",
		subcommand: "validate-cross-deps",
		guidance:   "check that cross-package imports have matching package.json dependency declarations",
	},
}

// codeIntelToolSet is the canonical tool-name set, used to filter a block's
// requested Tools subset down to the real six (unknown names are ignored).
var codeIntelToolSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(codeIntelTools))
	for _, tm := range codeIntelTools {
		m[tm.tool] = struct{}{}
	}
	return m
}()

// effectiveCodeIntelTools returns the canonical tool metas the block exposes,
// in canonical order. An empty (or all-unknown) Tools subset exposes all six;
// a non-empty subset is intersected with the canonical set so a typo can never
// allow-list a non-existent tool. Pure — no I/O.
func effectiveCodeIntelTools(ci *prompt.CodeIntelWork) []codeIntelToolMeta {
	requested := map[string]struct{}{}
	if ci != nil {
		for _, t := range ci.Tools {
			s := strings.TrimSpace(t)
			if s == "" {
				continue
			}
			if _, ok := codeIntelToolSet[s]; ok {
				requested[s] = struct{}{}
			}
		}
	}
	out := make([]codeIntelToolMeta, 0, len(codeIntelTools))
	for _, tm := range codeIntelTools {
		if len(requested) == 0 {
			out = append(out, tm)
			continue
		}
		if _, ok := requested[tm.tool]; ok {
			out = append(out, tm)
		}
	}
	return out
}

// codeIntelFQToolNames returns the fully-qualified MCP tool names the runner
// allow-lists (mcp__af-code-intelligence__<tool>), filtered to the block's
// exposed subset and in canonical order. Pure — no I/O.
func codeIntelFQToolNames(ci *prompt.CodeIntelWork) []string {
	eff := effectiveCodeIntelTools(ci)
	out := make([]string, 0, len(eff))
	for _, tm := range eff {
		out = append(out, codeIntelFQPrefix+tm.tool)
	}
	return out
}

// codeIntelToolsCSV renders the block's EXPOSED tool subset as the stdio
// server's `--tools` CSV, in canonical order. It derives from
// effectiveCodeIntelTools — the SAME already-filtered canonical set the FQ
// allow-list (codeIntelFQToolNames) and the prompt partial use — so an unknown
// / typo name is dropped consistently across every derivation and never reaches
// the server's `--tools` flag. This matters because server.validateTools
// rejects any unknown name all-or-nothing at startup: forwarding a single bad
// name verbatim would fail New() and take the ENTIRE code-intel server down
// while the composed Spec still advertised (and allow-listed) the tools.
//
// Returns "" when the exposed set is the full six — whether because the block
// requested no subset, an all-unknown subset (which collapses to all six), or
// explicitly listed all six — since the server defaults to all six when
// `--tools` is omitted, so listing them is redundant and the minimal-block wire
// contract omits the flag entirely.
func codeIntelToolsCSV(ci *prompt.CodeIntelWork) string {
	eff := effectiveCodeIntelTools(ci)
	// Full set → omit --tools and let the server default to all six. This keeps
	// the all-unknown case consistent with the FQ/prompt lanes (which also expose
	// all six) instead of passing a bogus name that would kill the server.
	if len(eff) == len(codeIntelTools) {
		return ""
	}
	names := make([]string, 0, len(eff))
	for _, tm := range eff {
		names = append(names, tm.tool)
	}
	return strings.Join(names, ",")
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

// injectCodeIntelPartial appends the compact code-intel usage partial to the
// composed system prompt. It is a STRICT no-op when ci is nil — a session
// without the capability block gets a byte-identical prompt to today
// (additive contract). Otherwise it appends a provider-appropriate partial:
//
//   - MCP-capable providers (SupportsToolPlugins && AcceptsMcpServerSpec:
//     claude/codex/gemini) get the six mcp__af-code-intelligence__* FQ tool
//     names with one-line when-to-use guidance.
//   - Providers that ignore MCP specs (ollama/opencode/agycli) get Bash-CLI
//     fallback guidance (`<brand> code <subcommand>`) — the binary is in-box on
//     every target — so the capability is still discoverable without an MCP
//     surface.
//
// No enforcement/lockout (deferred per Q7): the partial only advertises the
// tools; it never redirects Grep/Glob.
func injectCodeIntelPartial(systemPrompt string, caps agent.Capabilities, ci *prompt.CodeIntelWork) string {
	if ci == nil {
		return systemPrompt
	}
	mcpCapable := caps.SupportsToolPlugins && caps.AcceptsMcpServerSpec
	partial := codeIntelUsagePartial(mcpCapable, ci)
	if partial == "" {
		return systemPrompt
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return partial
	}
	return systemPrompt + "\n\n" + partial
}

// codeIntelUsagePartial renders the code-intel usage block for the resolved
// provider family, scoped to the block's exposed tool subset. Returns "" when
// the block exposes no tools (nothing to advertise). Pure aside from resolving
// the active brand for the CLI-fallback command name.
func codeIntelUsagePartial(mcpCapable bool, ci *prompt.CodeIntelWork) string {
	tools := effectiveCodeIntelTools(ci)
	if len(tools) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Code Intelligence\n\n")
	if mcpCapable {
		b.WriteString("This session has the af-code-intelligence tools available. " +
			"Prefer them over ad-hoc grep/glob/find when navigating or searching this repository:\n")
		for _, tm := range tools {
			b.WriteString("\n- ")
			b.WriteString(codeIntelFQPrefix + tm.tool)
			b.WriteString(" — ")
			b.WriteString(tm.guidance)
		}
	} else {
		cli := prompt.ResolveBrand().BrandCLI
		b.WriteString("This session has code-intelligence CLI commands available (run them with Bash). " +
			"Prefer them over ad-hoc grep/glob/find when navigating or searching this repository:\n")
		for _, tm := range tools {
			b.WriteString("\n- `")
			b.WriteString(cli + " code " + tm.subcommand)
			b.WriteString("` — ")
			b.WriteString(tm.guidance)
		}
	}
	return b.String()
}
