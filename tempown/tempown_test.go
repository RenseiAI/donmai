package tempown

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseRecognisesOnlyOwnerTaggedRegisteredNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		wantOK  bool
		wantPID int
		wantPfx string
	}{
		{"donmai-claude-usage-p4821-3920481723", true, 4821, PrefixClaudeUsage},
		{"donmai-claude-mcp-p77-12.json", true, 77, PrefixClaudeMCPConfig},
		{"donmai-pi-catalog-probe-p9-1", true, 9, PrefixPiCatalogProbe},
		{"muse-session-p1-2", true, 1, PrefixMuseSession},
		// untagged legacy names
		{"donmai-claude-usage-3920481723", false, 0, ""},
		{"donmai-claude-mcp-12.json", false, 0, ""},
		// malformed tags
		{"donmai-claude-usage-p-1", false, 0, ""},
		{"donmai-claude-usage-p0-1", false, 0, ""},
		{"donmai-claude-usage-px-1", false, 0, ""},
		{"donmai-claude-usage-p12", false, 0, ""},
		{"donmai-claude-usage-p12-", false, 0, ""},
		{"donmai-claude-usage-p12-abc", false, 0, ""},
		{"donmai-claude-mcp-p12-1", false, 0, ""}, // file producer without its suffix
		// unregistered or shared names
		{"tsx-501", false, 0, ""},
		{"node-compile-cache", false, 0, ""},
		{"go-build3829104", false, 0, ""},
		{".com.openai.codex.p1-2", false, 0, ""},
		{"donmai-codex-home-p1-2", false, 0, ""},
		{"donmai-claude-usagep1-2", false, 0, ""},
	}
	for _, tc := range cases {
		p, pid, ok := Parse(tc.name)
		if ok != tc.wantOK || pid != tc.wantPID || (ok && p.Prefix != tc.wantPfx) {
			t.Errorf("Parse(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, p.Prefix, pid, ok, tc.wantPfx, tc.wantPID, tc.wantOK)
		}
	}
}

func TestLegacyMatchesRegisteredPrefixWithoutTag(t *testing.T) {
	t.Parallel()
	if p, ok := Legacy("donmai-signal-guard-3829"); !ok || p.Prefix != PrefixSignalGuard {
		t.Fatalf("Legacy(untagged signal guard) = (%q, %v)", p.Prefix, ok)
	}
	if _, ok := Legacy("donmai-signal-guard-p12-3829"); ok {
		t.Fatal("a tagged name is recorded, not legacy")
	}
	if _, ok := Legacy("tsx-501"); ok {
		t.Fatal("a shared cache is not a legacy daemon artefact")
	}
}

func TestRegistryPrefixesAreUniqueAndDelimited(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, p := range Producers() {
		if seen[p.Prefix] {
			t.Errorf("duplicate prefix %q", p.Prefix)
		}
		seen[p.Prefix] = true
		if !strings.HasSuffix(p.Prefix, "-") {
			t.Errorf("prefix %q must end in '-' so the owner tag is unambiguous", p.Prefix)
		}
		if p.Kind != KindDir && p.Kind != KindFile {
			t.Errorf("prefix %q has no kind", p.Prefix)
		}
	}
}

func TestMkdirTempAndCreateTempRecordTheCallingProcess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir, err := MkdirTemp(root, PrefixSignalGuard+"*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	p, pid, ok := Parse(filepath.Base(dir))
	if !ok || pid != os.Getpid() || p.Prefix != PrefixSignalGuard {
		t.Fatalf("Parse(%q) = (%q, %d, %v), want own pid %d", filepath.Base(dir), p.Prefix, pid, ok, os.Getpid())
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}

	f, err := CreateTemp(root, PrefixClaudeMCPConfig)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	_ = f.Close()
	base := filepath.Base(f.Name())
	if !strings.HasSuffix(base, ".json") || !strings.Contains(base, "-p"+strconv.Itoa(os.Getpid())+"-") {
		t.Fatalf("CreateTemp name %q lacks suffix or owner tag", base)
	}
	if _, pid, ok := Parse(base); !ok || pid != os.Getpid() {
		t.Fatalf("Parse(%q) did not recover own pid", base)
	}
}

func TestCreatorsRefuseUnregisteredOrMismatchedPrefixes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := MkdirTemp(root, "donmai-unknown-"); !errors.Is(err, ErrUnregisteredPrefix) {
		t.Fatalf("MkdirTemp(unregistered) err = %v", err)
	}
	if _, err := MkdirTemp(root, PrefixClaudeMCPConfig); !errors.Is(err, ErrUnregisteredPrefix) {
		t.Fatalf("MkdirTemp(file prefix) err = %v", err)
	}
	if _, err := CreateTemp(root, PrefixSignalGuard); !errors.Is(err, ErrUnregisteredPrefix) {
		t.Fatalf("CreateTemp(dir prefix) err = %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("refused calls created %d entries", len(entries))
	}
}
