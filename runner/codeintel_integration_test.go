package runner

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestIntegration_QueuedWorkJSON_ComposesCodeIntelSpec is the Wave-2 cross-lane
// proof: it starts from a REAL JSON QueuedWork payload carrying a codeIntel
// block and drives the exact composition loop.go performs (defaultMCPServers →
// mergeMCPServers → the prompt partial → translateSpec), then asserts the ONE
// resulting agent.Spec simultaneously carries
//
//   - the runner-authored af-code-intelligence stdio MCP entry with the frozen
//     Args ["mcp","code-intel","--root",<wpath>,...] and --root == the threaded
//     worktree path (the server lane's afcli/mcp.go flag contract), and
//   - the six mcp__af-code-intelligence__af_code_* names on Spec.MCPToolNames in
//     canonical order (the allow-list the model calls the tools through), and
//   - the code-intel usage partial folded into Spec.SystemPromptAppend.
//
// This pins the runner-wiring lane and the MCP-server lane together end to end:
// the flag surface the runner EMITS is exactly the one the server ACCEPTS, and
// the allow-list matches the six tools the server advertises.
func TestIntegration_QueuedWorkJSON_ComposesCodeIntelSpec(t *testing.T) {
	t.Parallel()

	// A realistic platform → runner Redis session payload. codeIntel is the
	// typed wire block; sessionId/mcpStdioServers ride alongside it. RepoPath +
	// Tools are set so the optional flags and the subset filter are exercised.
	const payload = `{
		"sessionId": "sess_e2e",
		"codeIntel": {
			"repo": "owner/monorepo",
			"ref": "main",
			"repoPath": "packages/linear",
			"tools": ["af_code_search_symbols", "af_code_get_repo_map"]
		},
		"mcpStdioServers": [
			{"name": "card-extra", "type": "stdio", "command": "some-card-tool"}
		]
	}`

	var qw QueuedWork
	if err := json.Unmarshal([]byte(payload), &qw); err != nil {
		t.Fatalf("unmarshal QueuedWork: %v", err)
	}
	if qw.CodeIntel == nil {
		t.Fatal("codeIntel block did not decode onto QueuedWork")
	}
	// AuthToken/PlatformURL are daemon-resolved (json:"-"); set them to model a
	// real platform session so the platform HTTP gate leads the MCP list.
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"

	const wpath = "/abs/worktrees/sess_e2e"
	caps := mcpCaps()

	// ── Replicate the loop.go composition exactly ──────────────────────────────
	// loop.go:270
	mcpServers := mergeMCPServers(defaultMCPServersForHarness(qw, wpath, mcpDeliveringHarness(), agent.PromptModeAutonomous), qw.McpServers)
	// loop.go:405
	systemPrompt := injectCodeIntelPartial("BASE SYSTEM PROMPT", caps, qw.CodeIntel)
	// loop.go builds the Spec from these inputs.
	spec := translateSpec(qw, caps, SpecInputs{
		Cwd:                wpath,
		Prompt:             "do the work",
		SystemPromptAppend: systemPrompt,
		MCPServers:         mcpServers,
	})

	// ── Assertion 1: the stdio MCP entry is present, correct, and un-shadowed ──
	var ci agent.MCPServerConfig
	found, platformLeads := false, false
	for i, s := range spec.MCPServers {
		if i == 0 && strings.HasSuffix(s.Name, "-platform") && s.Type == "http" {
			platformLeads = true
		}
		if s.Name == codeIntelServerName {
			ci, found = s, true
		}
	}
	if !platformLeads {
		t.Errorf("platform HTTP gate must lead the MCP list; got %+v", spec.MCPServers)
	}
	if !found {
		t.Fatalf("composed Spec is missing the af-code-intelligence stdio entry; got %+v", spec.MCPServers)
	}
	if ci.Type != "stdio" {
		t.Errorf("code-intel entry type = %q, want stdio", ci.Type)
	}
	if ci.Command == "" {
		t.Errorf("code-intel entry Command must be the self-referential executable, got empty")
	}
	// --tools is derived from the SAME filtered canonical set as the FQ
	// allow-list (Assertion 2), so it is emitted in canonical order
	// (get_repo_map precedes search_symbols) even though the block listed them in
	// the opposite order. This byte-consistency is the point: an unknown/typo
	// name would be dropped here identically to the allow-list, never reaching
	// the server's all-or-nothing --tools validator.
	wantArgs := []string{
		"mcp", "code-intel",
		"--root", wpath,
		"--repo-path", "packages/linear",
		"--tools", "af_code_get_repo_map,af_code_search_symbols",
	}
	if !slices.Equal(ci.Args, wantArgs) {
		t.Errorf("code-intel Args = %v\nwant %v", ci.Args, wantArgs)
	}
	if got := argValue(ci.Args, "--root"); got != wpath {
		t.Errorf("code-intel --root = %q, want the threaded worktree path %q", got, wpath)
	}

	// ── Assertion 2: the allow-list is the subset's FQ names, canonical order ──
	// The block requested two tools; the runner allow-lists exactly those two FQ
	// names (the server exposes the same two via --tools). Canonical order:
	// get_repo_map precedes search_symbols in the frozen contract even though the
	// block listed them in the opposite order.
	wantFQ := []string{
		"mcp__af-code-intelligence__af_code_get_repo_map",
		"mcp__af-code-intelligence__af_code_search_symbols",
	}
	if !slices.Equal(spec.MCPToolNames, wantFQ) {
		t.Errorf("Spec.MCPToolNames = %v\nwant %v (subset, canonical order)", spec.MCPToolNames, wantFQ)
	}

	// ── Assertion 3: the usage partial is folded into the system prompt ────────
	if !strings.HasPrefix(spec.SystemPromptAppend, "BASE SYSTEM PROMPT\n\n") {
		t.Errorf("code-intel partial must be appended after the base prompt; got %q", spec.SystemPromptAppend)
	}
	for _, fq := range wantFQ {
		if !strings.Contains(spec.SystemPromptAppend, fq) {
			t.Errorf("system prompt missing exposed tool %q:\n%s", fq, spec.SystemPromptAppend)
		}
	}
	// A tool the block did NOT request must not be advertised anywhere.
	if strings.Contains(spec.SystemPromptAppend, "af_code_validate_cross_deps") {
		t.Errorf("system prompt advertised an unexposed tool:\n%s", spec.SystemPromptAppend)
	}
}

// TestIntegration_QueuedWorkJSON_MinimalBlockAllSixTools proves the minimal
// {repo} block (no repoPath/tools) exposes all six tools consistently across
// the two derived surfaces: the stdio entry carries no --tools (server default =
// all six) AND the allow-list carries all six FQ names.
func TestIntegration_QueuedWorkJSON_MinimalBlockAllSixTools(t *testing.T) {
	t.Parallel()

	const payload = `{"sessionId":"sess_min","codeIntel":{"repo":"owner/repo"}}`
	var qw QueuedWork
	if err := json.Unmarshal([]byte(payload), &qw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	qw.PlatformURL = "https://p.example.com"
	qw.AuthToken = "rsk_test"

	const wpath = "/abs/worktrees/sess_min"
	caps := mcpCaps()
	mcpServers := mergeMCPServers(defaultMCPServersForHarness(qw, wpath, mcpDeliveringHarness(), agent.PromptModeAutonomous), qw.McpServers)
	spec := translateSpec(qw, caps, SpecInputs{Cwd: wpath, MCPServers: mcpServers})

	var ciArgs []string
	for _, s := range spec.MCPServers {
		if s.Name == codeIntelServerName {
			ciArgs = s.Args
		}
	}
	if ciArgs == nil {
		t.Fatalf("missing code-intel entry; got %+v", spec.MCPServers)
	}
	if hasArg(ciArgs, "--tools") || hasArg(ciArgs, "--repo-path") {
		t.Errorf("minimal block must emit neither --tools nor --repo-path; got %v", ciArgs)
	}
	if got := argValue(ciArgs, "--root"); got != wpath {
		t.Errorf("--root = %q, want %q", got, wpath)
	}
	if !slices.Equal(spec.MCPToolNames, wantCodeIntelFQ) {
		t.Errorf("minimal block must allow-list all six FQ tools in canonical order;\n got %v\nwant %v", spec.MCPToolNames, wantCodeIntelFQ)
	}
}
