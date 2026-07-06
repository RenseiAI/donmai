package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildMCPEntry_FrozenContract(t *testing.T) {
	e := BuildMCPEntry("/usr/local/bin/donmai", "/work/area", "")
	if e.Name != "af-code-intelligence" {
		t.Errorf("server name = %q, want af-code-intelligence", e.Name)
	}
	if e.Type != "stdio" {
		t.Errorf("type = %q, want stdio", e.Type)
	}
	if e.Command != "/usr/local/bin/donmai" {
		t.Errorf("command = %q", e.Command)
	}
	wantArgs := []string{"mcp", "code-intel", "--root", "/work/area"}
	if strings.Join(e.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", e.Args, wantArgs)
	}
	// With a repo-path scope.
	e2 := BuildMCPEntry("donmai", "/work/area", "packages/linear")
	if strings.Join(e2.Args, " ") != "mcp code-intel --root /work/area --repo-path packages/linear" {
		t.Errorf("scoped args = %v", e2.Args)
	}
}

func TestMCPAdvertisement_Apply(t *testing.T) {
	ad := NewAdvertisement(AdvertiseMCP, true) // all-tools surface
	servers, suffix, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", TaskFindSymbol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != CodeIntelServerName {
		t.Fatalf("expected one af-code-intelligence server, got %v", servers)
	}
	if !strings.Contains(suffix, "mcp__af-code-intelligence__af_code_search_symbols") {
		t.Errorf("prompt suffix should advertise FQ tool names; got %q", suffix)
	}
	if n := len(ad.AdvertisedToolNames(TaskFindSymbol)); n != 6 {
		t.Errorf("expected 6 advertised tools, got %d", n)
	}
}

// writeFakeDonmai drops a fake `donmai` that answers `code --help` with a
// recognisable banner, so the prompt-help advertisement can be tested without
// the real binary.
func writeFakeDonmai(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = code ] && [ \"$2\" = --help ]; then\n" +
		"  echo 'Code intelligence commands: get-repo-map search-symbols search-code'\n" +
		"  exit 0\nfi\nexit 0\n"
	p := filepath.Join(dir, "donmai")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil { // nolint:gosec // must be executable to run as the WITH-arm donmai
		t.Fatalf("write fake donmai: %v", err)
	}
	return p
}

func TestPromptHelpAdvertisement_Apply_UsesLiveHelp(t *testing.T) {
	bin := writeFakeDonmai(t)
	ad := NewAdvertisement(AdvertisePromptHelp, false)
	servers, suffix, err := ad.Apply(context.Background(), bin, "/wa", "", TaskFindSymbol, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Errorf("prompt-help must attach NO MCP servers, got %d", len(servers))
	}
	if !strings.Contains(suffix, "search-symbols") {
		t.Errorf("prompt suffix should embed live `donmai code --help` output; got %q", suffix)
	}
}

// ── WS4: task-conditional, capability-anchored advertisement framing ─────────

// TestMCPAdvertisement_TaskConditionalFraming pins the WS4 rewrite: the prompt
// suffix must anchor each tool to the job it wins (orientation, pre-edit xref
// enumeration, near-duplicate) and explicitly de-scope trivial grep lookups —
// NOT the losing blanket "prefer them over grep" framing that drove adoption
// onto tasks grep wins.
func TestMCPAdvertisement_TaskConditionalFraming(t *testing.T) {
	ad := NewAdvertisement(AdvertiseMCP, true) // all-tools: every bullet present
	_, suffix, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", TaskFindSymbol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(suffix, "Prefer them over") {
		t.Errorf("blanket instead-of-grep framing must be gone; got %q", suffix)
	}
	low := strings.ToLower(suffix)
	// (a) orientation → repo map FIRST.
	if !strings.Contains(suffix, "af_code_get_repo_map") || !strings.Contains(low, "first") {
		t.Errorf("suffix should anchor af_code_get_repo_map to orientation FIRST; got %q", suffix)
	}
	// (b) cross-file rename/refactor → enumerate sites BEFORE editing.
	if !strings.Contains(suffix, "af_code_find_type_usages") || !strings.Contains(low, "before editing") {
		t.Errorf("suffix should anchor af_code_find_type_usages to pre-edit enumeration; got %q", suffix)
	}
	// (c) already-exists / near-duplicate → check_duplicate.
	if !strings.Contains(suffix, "af_code_check_duplicate") {
		t.Errorf("suffix should anchor af_code_check_duplicate to duplicate checks; got %q", suffix)
	}
	// (d) trivial lookup de-scope: grep is fine, no tool call.
	if !strings.Contains(low, "grep is fine") {
		t.Errorf("suffix should de-scope exact single-identifier lookups to grep; got %q", suffix)
	}
}

// TestPromptHelpAdvertisement_TaskConditionalFraming: the prompt-help variant
// shares the framing contract (no blanket prefer-over-grep; grep de-scope).
func TestPromptHelpAdvertisement_TaskConditionalFraming(t *testing.T) {
	bin := writeFakeDonmai(t)
	ad := NewAdvertisement(AdvertisePromptHelp, false)
	_, suffix, err := ad.Apply(context.Background(), bin, "/wa", "", TaskFindSymbol, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(suffix, "Prefer them over") {
		t.Errorf("blanket instead-of-grep framing must be gone; got %q", suffix)
	}
	if !strings.Contains(strings.ToLower(suffix), "grep is fine") {
		t.Errorf("suffix should de-scope exact single-identifier lookups to grep; got %q", suffix)
	}
}

// ── WS2: core-subset advertisement (discovery-tax reduction) ─────────────────

// TestMCPAdvertisement_CoreSubsetDefault pins the WS2 default: a non-refactor
// WITH arm carries only the core four tools — the MCP entry passes the
// server's --tools allow-list and the prompt/advertised names match it — so
// the arm's ToolSearch/schema weight shrinks. The MCP server binary itself
// still registers all six by default (wire contract).
func TestMCPAdvertisement_CoreSubsetDefault(t *testing.T) {
	ad := NewAdvertisement(AdvertiseMCP, false)
	servers, suffix, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", TaskFindSymbol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected one server, got %d", len(servers))
	}
	args := strings.Join(servers[0].Args, " ")
	wantTools := "af_code_get_repo_map,af_code_search_symbols,af_code_search_code,af_code_check_duplicate"
	if !strings.Contains(args, "--tools "+wantTools) {
		t.Errorf("MCP entry should carry the core-four --tools allow-list; args = %q", args)
	}
	names := ad.AdvertisedToolNames(TaskFindSymbol)
	if len(names) != 4 {
		t.Errorf("core subset should advertise 4 tools, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if strings.Contains(n, "find_type_usages") || strings.Contains(n, "validate_cross_deps") {
			t.Errorf("core subset must not advertise %s", n)
		}
	}
	if strings.Contains(suffix, "af_code_find_type_usages") {
		t.Errorf("prompt suffix must not mention an un-advertised tool; got %q", suffix)
	}
}

// TestMCPAdvertisement_RefactorAddsTypeUsages: the refactor family adds the
// xref tool (find_type_usages) to the core four — the one family whose job it
// serves — keeping the rule a deterministic function of the family only.
func TestMCPAdvertisement_RefactorAddsTypeUsages(t *testing.T) {
	ad := NewAdvertisement(AdvertiseMCP, false)
	servers, suffix, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", TaskRefactorAcrossFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(servers[0].Args, " ")
	if !strings.Contains(args, "af_code_find_type_usages") {
		t.Errorf("refactor arm should carry find_type_usages in --tools; args = %q", args)
	}
	if !strings.Contains(suffix, "af_code_find_type_usages") {
		t.Errorf("refactor prompt should anchor find_type_usages; got %q", suffix)
	}
	names := ad.AdvertisedToolNames(TaskRefactorAcrossFiles)
	if len(names) != 5 {
		t.Errorf("refactor subset should advertise 5 tools, got %d: %v", len(names), names)
	}
}

// TestMCPAdvertisement_AllToolsOption: the all-six escape hatch restores the
// full surface and passes NO --tools allow-list (server default = all six).
func TestMCPAdvertisement_AllToolsOption(t *testing.T) {
	ad := NewAdvertisement(AdvertiseMCP, true)
	servers, _, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", TaskFindSymbol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(servers[0].Args, " "); strings.Contains(args, "--tools") {
		t.Errorf("all-tools mode must not pass --tools; args = %q", args)
	}
	if n := len(ad.AdvertisedToolNames(TaskFindSymbol)); n != 6 {
		t.Errorf("all-tools mode should advertise 6 tools, got %d", n)
	}
}
