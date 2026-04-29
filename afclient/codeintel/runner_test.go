package codeintel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAFCode writes a small script that echos JSON to stdout, then returns its
// path. The script accepts sub-commands and echoes a JSON object describing
// what was called so tests can verify flag plumbing.
func fakeAFCode(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "af-code")

	// A minimal POSIX shell script that outputs JSON.
	content := `#!/bin/sh
echo '{"command":"'$1'","args":"'"$*"'"}'
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil { //nolint:gosec // #nosec G306 -- test fake binary; needs owner exec bit
		t.Fatalf("write fake af-code: %v", err)
	}
	return script
}

// runnerWithFakeCode returns a Runner wired to a fake af-code that echoes JSON.
func runnerWithFakeCode(t *testing.T) *Runner {
	t.Helper()
	bin := fakeAFCode(t)
	// Override the env so resolveCodeBin picks up our fake.
	t.Setenv("AGENTFACTORY_CODE_BIN", bin)
	return New(t.TempDir())
}

// ── GetRepoMap ───────────────────────────────────────────────────────────────

func TestGetRepoMap_DefaultOptions(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.GetRepoMap(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("GetRepoMap: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if cmd, _ := m["command"].(string); cmd != "get-repo-map" {
		t.Errorf("command: got %q, want %q", cmd, "get-repo-map")
	}
}

func TestGetRepoMap_WithOptions(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.GetRepoMap(GetRepoMapOptions{MaxFiles: 25, FilePatterns: []string{"*.go", "src/**"}})
	if err != nil {
		t.Fatalf("GetRepoMap: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	args, _ := m["args"].(string)
	if !strings.Contains(args, "--max-files") {
		t.Errorf("args %q missing --max-files", args)
	}
	if !strings.Contains(args, "25") {
		t.Errorf("args %q missing max-files value 25", args)
	}
	if !strings.Contains(args, "--file-patterns") {
		t.Errorf("args %q missing --file-patterns", args)
	}
}

// ── SearchSymbols ─────────────────────────────────────────────────────────────

func TestSearchSymbols_RequiresQuery(t *testing.T) {
	r := runnerWithFakeCode(t)
	_, err := r.SearchSymbols(SearchSymbolsOptions{})
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestSearchSymbols_BasicQuery(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.SearchSymbols(SearchSymbolsOptions{Query: "SearchEngine"})
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	args, _ := m["args"].(string)
	if !strings.Contains(args, "search-symbols") {
		t.Errorf("args %q missing search-symbols subcommand", args)
	}
	if !strings.Contains(args, "SearchEngine") {
		t.Errorf("args %q missing query SearchEngine", args)
	}
}

func TestSearchSymbols_WithKindsAndFilePattern(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.SearchSymbols(SearchSymbolsOptions{
		Query:       "handleRequest",
		MaxResults:  5,
		Kinds:       []string{"function", "method"},
		FilePattern: "*.go",
	})
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "--max-results") {
		t.Errorf("args %q missing --max-results", args)
	}
	if !strings.Contains(args, "--kinds") {
		t.Errorf("args %q missing --kinds", args)
	}
	if !strings.Contains(args, "--file-pattern") {
		t.Errorf("args %q missing --file-pattern", args)
	}
}

// ── SearchCode ────────────────────────────────────────────────────────────────

func TestSearchCode_RequiresQuery(t *testing.T) {
	r := runnerWithFakeCode(t)
	_, err := r.SearchCode(SearchCodeOptions{})
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestSearchCode_BasicQuery(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.SearchCode(SearchCodeOptions{Query: "incremental indexer"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "search-code") {
		t.Errorf("args %q missing search-code subcommand", args)
	}
}

func TestSearchCode_WithLanguage(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.SearchCode(SearchCodeOptions{Query: "pagerank", Language: "typescript"})
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "--language") {
		t.Errorf("args %q missing --language", args)
	}
}

// ── CheckDuplicate ───────────────────────────────────────────────────────────

func TestCheckDuplicate_RequiresContent(t *testing.T) {
	r := runnerWithFakeCode(t)
	_, err := r.CheckDuplicate(CheckDuplicateOptions{})
	if err == nil {
		t.Fatal("expected error when neither content nor content-file is set")
	}
}

func TestCheckDuplicate_WithContent(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.CheckDuplicate(CheckDuplicateOptions{Content: "function hello() {}"})
	if err != nil {
		t.Fatalf("CheckDuplicate: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "check-duplicate") {
		t.Errorf("args %q missing check-duplicate subcommand", args)
	}
	if !strings.Contains(args, "--content") {
		t.Errorf("args %q missing --content", args)
	}
}

func TestCheckDuplicate_WithContentFile(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.CheckDuplicate(CheckDuplicateOptions{ContentFile: "/tmp/snippet.ts"})
	if err != nil {
		t.Fatalf("CheckDuplicate: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "--content-file") {
		t.Errorf("args %q missing --content-file", args)
	}
}

// ── FindTypeUsages ────────────────────────────────────────────────────────────

func TestFindTypeUsages_RequiresTypeName(t *testing.T) {
	r := runnerWithFakeCode(t)
	_, err := r.FindTypeUsages(FindTypeUsagesOptions{})
	if err == nil {
		t.Fatal("expected error for empty type name")
	}
}

func TestFindTypeUsages_BasicTypeName(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.FindTypeUsages(FindTypeUsagesOptions{TypeName: "AgentWorkType"})
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "find-type-usages") {
		t.Errorf("args %q missing find-type-usages subcommand", args)
	}
	if !strings.Contains(args, "AgentWorkType") {
		t.Errorf("args %q missing type name AgentWorkType", args)
	}
}

func TestFindTypeUsages_WithMaxResults(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.FindTypeUsages(FindTypeUsagesOptions{TypeName: "WorkType", MaxResults: 10})
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "--max-results") {
		t.Errorf("args %q missing --max-results", args)
	}
}

// ── ValidateCrossDeps ─────────────────────────────────────────────────────────

func TestValidateCrossDeps_NoPath(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.ValidateCrossDeps(ValidateCrossDepsOptions{})
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "validate-cross-deps") {
		t.Errorf("args %q missing validate-cross-deps subcommand", args)
	}
}

func TestValidateCrossDeps_WithPath(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.ValidateCrossDeps(ValidateCrossDepsOptions{Path: "packages/linear"})
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	m := out.(map[string]any)
	args, _ := m["args"].(string)
	if !strings.Contains(args, "packages/linear") {
		t.Errorf("args %q missing scoping path packages/linear", args)
	}
}

// ── Binary resolution ─────────────────────────────────────────────────────────

func TestResolveCodeBin_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	fakebin := filepath.Join(dir, "my-af-code")
	if err := os.WriteFile(fakebin, []byte("#!/bin/sh\necho '{}'"), 0o755); err != nil { //nolint:gosec // #nosec G306 -- test fake binary; needs owner exec bit
		t.Fatal(err)
	}
	t.Setenv("AGENTFACTORY_CODE_BIN", fakebin)
	r := New(t.TempDir())
	bin, err := r.resolveCodeBin()
	if err != nil {
		t.Fatalf("resolveCodeBin: %v", err)
	}
	if len(bin) == 0 || bin[0] != fakebin {
		t.Errorf("expected %q, got %v", fakebin, bin)
	}
}

func TestResolveCodeBin_CachingIdempotent(t *testing.T) {
	t.Setenv("AGENTFACTORY_CODE_BIN", "/some/bin")
	r := New(t.TempDir())
	b1, _ := r.resolveCodeBin()
	b2, _ := r.resolveCodeBin()
	if strings.Join(b1, " ") != strings.Join(b2, " ") {
		t.Errorf("resolution not idempotent: %v vs %v", b1, b2)
	}
}

func TestIsCodeAvailable_WhenEnvSet(t *testing.T) {
	t.Setenv("AGENTFACTORY_CODE_BIN", "/some/path")
	r := New(t.TempDir())
	if !r.IsCodeAvailable() {
		t.Error("expected IsCodeAvailable=true when AGENTFACTORY_CODE_BIN is set")
	}
}

func TestIsCodeAvailable_WhenNotFound(t *testing.T) {
	t.Setenv("AGENTFACTORY_CODE_BIN", "")
	// Temporarily shadow PATH to ensure af-code and pnpm aren't found.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // dir with no binaries
	defer func() { _ = os.Setenv("PATH", origPath) }()

	r := New(t.TempDir())
	r.codeBinCache = "" // ensure fresh lookup
	if r.IsCodeAvailable() {
		// This test may legitimately pass if af-code or pnpm is in the new PATH dir.
		// Skip rather than fail if the system truly has pnpm somewhere unexpected.
		if _, err := exec.LookPath("pnpm"); err == nil {
			t.Skip("pnpm found in PATH; cannot test unavailable path")
		}
		t.Error("expected IsCodeAvailable=false when binary not found")
	}
}

// ── JSON output round-trip ────────────────────────────────────────────────────

// TestJSONOutputRoundTrip verifies that the output from runCode is
// a valid JSON value that survives a marshal/unmarshal cycle.
func TestJSONOutputRoundTrip(t *testing.T) {
	r := runnerWithFakeCode(t)
	out, err := r.GetRepoMap(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("GetRepoMap: %v", err)
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out2 any
	if err := json.Unmarshal(data, &out2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Both should be non-nil maps.
	if _, ok := out2.(map[string]any); !ok {
		t.Errorf("expected map after round-trip, got %T", out2)
	}
}
