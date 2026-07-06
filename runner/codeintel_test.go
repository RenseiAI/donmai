package runner

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// mcpCaps / cliCaps model the two provider families the code-intel prompt
// partial branches on: MCP-capable (claude/codex/gemini) vs providers that
// ignore MCP specs (ollama/opencode/agycli), which get the Bash-CLI fallback.
func mcpCaps() agent.Capabilities {
	return agent.Capabilities{SupportsToolPlugins: true, AcceptsMcpServerSpec: true}
}

func cliCaps() agent.Capabilities {
	return agent.Capabilities{SupportsToolPlugins: false, AcceptsMcpServerSpec: false}
}

// TestInjectCodeIntelPartial_NilBlockByteIdentical is the regression guard for
// the additive contract: a session WITHOUT the block gets a byte-identical
// system prompt — the injector is a strict no-op when ci is nil.
func TestInjectCodeIntelPartial_NilBlockByteIdentical(t *testing.T) {
	t.Parallel()
	const sys = "You are the runner.\n\n# Operating rules\n- do the thing"
	if got := injectCodeIntelPartial(sys, mcpCaps(), nil); got != sys {
		t.Errorf("nil block must be byte-identical:\n got %q\nwant %q", got, sys)
	}
	if got := injectCodeIntelPartial(sys, cliCaps(), nil); got != sys {
		t.Errorf("nil block (cli caps) must be byte-identical:\n got %q\nwant %q", got, sys)
	}
}

// TestInjectCodeIntelPartial_MCPWording verifies MCP-capable providers get the
// FQ tool names (not the CLI form), appended after the base system prompt.
func TestInjectCodeIntelPartial_MCPWording(t *testing.T) {
	t.Parallel()
	const sys = "BASE-SYSTEM-PROMPT"
	got := injectCodeIntelPartial(sys, mcpCaps(), &prompt.CodeIntelWork{Repo: "owner/repo"})

	if !strings.HasPrefix(got, sys+"\n\n") {
		t.Errorf("partial must be appended after the base prompt; got %q", got)
	}
	for _, fq := range wantCodeIntelFQ {
		if !strings.Contains(got, fq) {
			t.Errorf("MCP partial missing FQ tool %q:\n%s", fq, got)
		}
	}
	// MCP wording must NOT emit the Bash-CLI form.
	if strings.Contains(got, "code get-repo-map") {
		t.Errorf("MCP partial must not carry the CLI form; got:\n%s", got)
	}
}

// TestInjectCodeIntelPartial_CLIWording verifies providers that ignore MCP
// specs get Bash-CLI fallback guidance (brand `code <subcommand>`), never the
// FQ MCP names.
func TestInjectCodeIntelPartial_CLIWording(t *testing.T) {
	t.Parallel()
	const sys = "BASE"
	got := injectCodeIntelPartial(sys, cliCaps(), &prompt.CodeIntelWork{Repo: "owner/repo"})

	brand := prompt.ResolveBrand().BrandCLI // "donmai" in OSS test builds
	for _, sub := range []string{"get-repo-map", "search-symbols", "search-code", "check-duplicate", "find-type-usages", "validate-cross-deps"} {
		if !strings.Contains(got, brand+" code "+sub) {
			t.Errorf("CLI partial missing %q:\n%s", brand+" code "+sub, got)
		}
	}
	if strings.Contains(got, "mcp__af-code-intelligence__") {
		t.Errorf("CLI partial must not carry FQ MCP names; got:\n%s", got)
	}
}

// TestCodeIntel_AbsentBlock_SpecByteIdentical is the snapshot regression for
// the additive contract across all three seams the loop composes: a session
// with NO CodeIntel block leaves the system prompt untouched, adds no
// MCPToolNames, and adds no code-intel MCP entry — so the Spec is byte-identical
// to the pre-code-intel composition.
func TestCodeIntel_AbsentBlock_SpecByteIdentical(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{} // no CodeIntel block
	caps := mcpCaps()
	baseSys := "SYS-BASE\n\n# Operating rules"
	baseServers := []agent.MCPServerConfig{{Name: "donmai-platform", Type: "http"}}

	// Seam 1 — prompt partial: no-op.
	sysAfter := injectCodeIntelPartial(baseSys, caps, qw.CodeIntel)
	if sysAfter != baseSys {
		t.Errorf("absent block must leave the system prompt byte-identical:\n got %q\nwant %q", sysAfter, baseSys)
	}

	// Seam 2 — defaultMCPServers: no code-intel entry appended.
	gated := QueuedWork{}
	gated.SessionID = "s"
	gated.PlatformURL = "https://p.test"
	gated.AuthToken = "rsk"
	if servers := defaultMCPServers(gated, "/tmp/wt"); len(servers) != 1 {
		t.Errorf("absent block must not append a code-intel MCP entry; got %+v", servers)
	}

	// Seam 3 — translateSpec: MCPToolNames untouched, MCPServers unchanged.
	spec := translateSpec(qw, caps, SpecInputs{
		Cwd:                "/tmp/wt",
		Prompt:             "u",
		SystemPromptAppend: sysAfter,
		MCPServers:         baseServers,
	})
	if len(spec.MCPToolNames) != 0 {
		t.Errorf("absent block must add no MCPToolNames; got %v", spec.MCPToolNames)
	}
	if !reflect.DeepEqual(spec.MCPServers, baseServers) {
		t.Errorf("absent block must not mutate MCPServers; got %+v", spec.MCPServers)
	}
}

// TestInjectCodeIntelPartial_FilteredSubset verifies the block's Tools subset
// narrows the guidance to just the exposed tools (both provider families).
func TestInjectCodeIntelPartial_FilteredSubset(t *testing.T) {
	t.Parallel()
	ci := &prompt.CodeIntelWork{Repo: "owner/repo", Tools: []string{"af_code_search_symbols"}}

	mcp := injectCodeIntelPartial("S", mcpCaps(), ci)
	if !strings.Contains(mcp, "mcp__af-code-intelligence__af_code_search_symbols") {
		t.Errorf("subset MCP partial missing the requested tool:\n%s", mcp)
	}
	if strings.Contains(mcp, "af_code_get_repo_map") {
		t.Errorf("subset MCP partial must not name unexposed tools:\n%s", mcp)
	}

	cli := injectCodeIntelPartial("S", cliCaps(), ci)
	if !strings.Contains(cli, "code search-symbols") {
		t.Errorf("subset CLI partial missing the requested tool:\n%s", cli)
	}
	if strings.Contains(cli, "get-repo-map") {
		t.Errorf("subset CLI partial must not name unexposed tools:\n%s", cli)
	}
}

// argValue returns the value following the named flag in an MCP entry's Args,
// or "" when the flag is absent.
func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestCodeIntelMCPEntry_MinimalBlock pins the frozen wire contract for the
// runner-authored stdio entry: a bare {repo} block yields the af-code-intelligence
// stdio server invoked as `<self> mcp code-intel --root <root>` with no
// --repo-path / --tools, and --root ALWAYS carries the explicit absolute
// worktree path (Wave-0 trace: the runner process cwd is unreliable on every
// target).
func TestCodeIntelMCPEntry_MinimalBlock(t *testing.T) {
	t.Parallel()

	root := "/abs/worktrees/sess_1"
	entry := codeIntelMCPEntry(root, &prompt.CodeIntelWork{Repo: "owner/repo"})

	if entry.Name != "af-code-intelligence" {
		t.Errorf("name = %q, want af-code-intelligence", entry.Name)
	}
	if entry.Type != "stdio" {
		t.Errorf("type = %q, want stdio", entry.Type)
	}
	// Command must be the self-referential executable, never empty.
	if entry.Command == "" {
		t.Errorf("command must be os.Executable(), got empty")
	}
	if exe, _ := os.Executable(); exe != "" && entry.Command != exe {
		t.Errorf("command = %q, want os.Executable() %q", entry.Command, exe)
	}
	wantArgs := []string{"mcp", "code-intel", "--root", root}
	if !reflect.DeepEqual(entry.Args, wantArgs) {
		t.Errorf("args = %v, want %v", entry.Args, wantArgs)
	}
	// --root is always explicit + equal to the provisioned path.
	if got := argValue(entry.Args, "--root"); got != root {
		t.Errorf("--root = %q, want the explicit provisioned path %q", got, root)
	}
	if hasArg(entry.Args, "--repo-path") || hasArg(entry.Args, "--tools") {
		t.Errorf("minimal block must not emit --repo-path / --tools; got %v", entry.Args)
	}
}

// TestCodeIntelMCPEntry_RepoPathAndTools verifies the optional block fields are
// forwarded as flags, in contract order, after --root.
func TestCodeIntelMCPEntry_RepoPathAndTools(t *testing.T) {
	t.Parallel()

	root := "/abs/worktrees/sess_2"
	entry := codeIntelMCPEntry(root, &prompt.CodeIntelWork{
		Repo:     "owner/repo",
		RepoPath: "packages/linear",
		Tools:    []string{"af_code_search_symbols", "af_code_search_code"},
	})

	wantArgs := []string{
		"mcp", "code-intel",
		"--root", root,
		"--repo-path", "packages/linear",
		"--tools", "af_code_search_symbols,af_code_search_code",
	}
	if !reflect.DeepEqual(entry.Args, wantArgs) {
		t.Errorf("args = %v, want %v", entry.Args, wantArgs)
	}
}

// TestDefaultMCPServers_AppendsCodeIntelAfterPlatformGate verifies the F.5 seam:
// when the CodeIntel block is present the runner appends the stdio entry AFTER
// the platform HTTP gate, threading the provisioned worktree path into --root.
func TestDefaultMCPServers_AppendsCodeIntelAfterPlatformGate(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_abc"
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"
	qw.CodeIntel = &prompt.CodeIntelWork{Repo: "owner/repo"}

	root := "/abs/worktrees/sess_abc"
	servers := defaultMCPServers(qw, root)
	if len(servers) != 2 {
		t.Fatalf("len(servers) = %d, want 2 (platform gate + code-intel)", len(servers))
	}
	if servers[0].Name != "donmai-platform" || servers[0].Type != "http" {
		t.Errorf("servers[0] must be the platform gate; got %+v", servers[0])
	}
	if servers[1].Name != "af-code-intelligence" || servers[1].Type != "stdio" {
		t.Errorf("servers[1] must be the code-intel stdio entry; got %+v", servers[1])
	}
	if got := argValue(servers[1].Args, "--root"); got != root {
		t.Errorf("code-intel --root = %q, want threaded wpath %q", got, root)
	}
}

// TestDefaultMCPServers_CodeIntelInStandaloneMode verifies code-intel is a pure
// in-box plugin with no platform coupling: with no PlatformURL/AuthToken the
// platform gate is omitted, but the code-intel entry is still emitted.
func TestDefaultMCPServers_CodeIntelInStandaloneMode(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.CodeIntel = &prompt.CodeIntelWork{Repo: "owner/repo"}

	root := "/abs/worktrees/standalone"
	servers := defaultMCPServers(qw, root)
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1 (code-intel only, no platform gate)", len(servers))
	}
	if servers[0].Name != "af-code-intelligence" {
		t.Errorf("standalone code-intel entry missing; got %+v", servers[0])
	}
	if got := argValue(servers[0].Args, "--root"); got != root {
		t.Errorf("--root = %q, want %q", got, root)
	}
}

// TestDefaultMCPServers_NoBlockIsUnchanged pins the additive contract: with no
// CodeIntel block the output is exactly today's (platform gate only, and nil in
// standalone mode) — the code-intel wiring is invisible when the block is nil.
func TestDefaultMCPServers_NoBlockIsUnchanged(t *testing.T) {
	t.Parallel()

	// Platform session, no block: identical to the pre-code-intel single gate.
	gated := QueuedWork{}
	gated.SessionID = "sess_x"
	gated.PlatformURL = "https://platform.example.com"
	gated.AuthToken = "rsk_test"
	servers := defaultMCPServers(gated, "/abs/wt")
	if len(servers) != 1 || servers[0].Name != "donmai-platform" {
		t.Errorf("no-block platform session must emit only the gate; got %+v", servers)
	}

	// Standalone, no block: nil (back-compat path preserved).
	if got := defaultMCPServers(QueuedWork{}, "/abs/wt"); got != nil {
		t.Errorf("no-block standalone must be nil; got %+v", got)
	}
}

// TestMergeMCPServers_CodeIntelDefaultWinsOnCollision documents the collision
// outcome: the runner-authored af-code-intelligence entry lives in the DEFAULTS,
// so a platform-sent card entry of the same name is DROPPED (defaults win) — the
// in-box stdio entry is never shadowed by a card-supplied override.
func TestMergeMCPServers_CodeIntelDefaultWinsOnCollision(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_abc"
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"
	qw.CodeIntel = &prompt.CodeIntelWork{Repo: "owner/repo"}
	qw.McpServers = []agent.MCPServerConfig{
		// A hostile/stale card entry trying to shadow the in-box code-intel entry.
		{Name: "af-code-intelligence", Type: "stdio", Command: "evil"},
		{Name: "card-extra", Type: "stdio", Command: "ok"},
	}

	root := "/abs/worktrees/sess_abc"
	merged := mergeMCPServers(defaultMCPServers(qw, root), qw.McpServers)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3 (gate + runner code-intel + card-extra; collision dropped)", len(merged))
	}
	var ci agent.MCPServerConfig
	for _, s := range merged {
		if s.Name == "af-code-intelligence" {
			ci = s
		}
	}
	if ci.Command == "evil" {
		t.Errorf("card entry shadowed the runner-authored code-intel entry; defaults must win")
	}
	if got := argValue(ci.Args, "--root"); got != root {
		t.Errorf("surviving code-intel entry must be the runner-authored one (--root=%q, want %q)", got, root)
	}
}

// TestCodeIntelPartial_TaskConditionalLanguageNeutral extends the WS4 framing
// contract to the PRODUCTION prompt partial (the existing framing tests only
// covered runtime/mcp/server descriptions and the eval advertisement): no
// blanket "Prefer them over grep" steering, no single-language idiom in any
// guidance string, and the explicit grep de-scope clause present in every
// rendered variant. The pilot showed the blanket framing drove adoption onto
// tasks grep already wins (1.0-2.10x token cost at +0pp success), and the TS
// idioms told Go agents the xref tool was inapplicable.
func TestCodeIntelPartial_TaskConditionalLanguageNeutral(t *testing.T) {
	t.Parallel()
	banned := []string{"Prefer them over", "union type", "Record<"}

	for _, tm := range codeIntelTools {
		for _, b := range banned {
			if strings.Contains(tm.guidance, b) {
				t.Errorf("guidance for %s contains banned phrase %q: %q", tm.tool, b, tm.guidance)
			}
		}
	}

	for name, caps := range map[string]agent.Capabilities{"mcp": mcpCaps(), "cli": cliCaps()} {
		got := injectCodeIntelPartial("S", caps, &prompt.CodeIntelWork{Repo: "owner/repo"})
		for _, b := range banned {
			if strings.Contains(got, b) {
				t.Errorf("%s partial contains banned phrase %q:\n%s", name, b, got)
			}
		}
		low := strings.ToLower(got)
		// The grep de-scope sentence is pinned: trivial exact-identifier
		// lookups belong to grep, not a tool call.
		if !strings.Contains(low, "grep is fine") {
			t.Errorf("%s partial must de-scope exact single-identifier lookups to grep:\n%s", name, got)
		}
		// Task-conditional anchors: orientation -> repo map FIRST; pre-edit
		// enumeration before a cross-file rename/refactor.
		if !strings.Contains(low, "first") {
			t.Errorf("%s partial should anchor get_repo_map to orientation FIRST:\n%s", name, got)
		}
		if !strings.Contains(low, "rename or refactor") {
			t.Errorf("%s partial should anchor find_type_usages to pre-edit cross-file rename/refactor:\n%s", name, got)
		}
	}
}
