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
	ad := NewAdvertisement(AdvertiseMCP)
	servers, suffix, err := ad.Apply(context.Background(), "/bin/donmai", "/wa", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != CodeIntelServerName {
		t.Fatalf("expected one af-code-intelligence server, got %v", servers)
	}
	if !strings.Contains(suffix, "mcp__af-code-intelligence__af_code_search_symbols") {
		t.Errorf("prompt suffix should advertise FQ tool names; got %q", suffix)
	}
	if len(ad.AdvertisedToolNames()) != 6 {
		t.Errorf("expected 6 advertised tools, got %d", len(ad.AdvertisedToolNames()))
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
	ad := NewAdvertisement(AdvertisePromptHelp)
	servers, suffix, err := ad.Apply(context.Background(), bin, "/wa", "", os.Environ())
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
