package afcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

// fakeCodeBin writes a shell script that echoes JSON describing the invocation
// (subcommand + all args), installs it as AGENTFACTORY_CODE_BIN, and returns
// the path. Tests use this to verify that the Cobra commands build the correct
// argv without actually running af-code.
func fakeCodeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "af-code")
	// The script prints a JSON object: {"command": "<first arg>", "argv": "<all args>"}
	content := `#!/bin/sh
printf '{"command":"%s","argv":"%s"}' "$1" "$*"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil { //nolint:gosec // #nosec G306 -- test fake binary; needs owner exec bit
		t.Fatalf("write fake af-code: %v", err)
	}
	t.Setenv("AGENTFACTORY_CODE_BIN", script)
	return script
}

// execCodeCmd builds a fresh `af code <subArgs>` command tree with a fake
// binary and runs it, capturing stdout. Returns the decoded JSON map and any
// error.
func execCodeCmd(t *testing.T, subArgs ...string) (map[string]any, error) {
	t.Helper()
	fakeCodeBin(t)

	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())

	var buf bytes.Buffer
	root.SetOut(&buf)
	// Redirect stdout for the command tree (printJSON writes to os.Stdout so we
	// capture it via the real os.Stdout redirect trick below).
	// Because printJSON uses os.Stdout directly, we swap it for testing.
	oldOut := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	args := append([]string{"code"}, subArgs...)
	root.SetArgs(args)
	err := root.Execute()

	_ = w.Close()
	os.Stdout = oldOut

	var out bytes.Buffer
	if _, readErr := out.ReadFrom(r); readErr != nil {
		t.Fatalf("read stdout pipe: %v", readErr)
	}

	if err != nil {
		return nil, err
	}

	var m map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &m); jsonErr != nil {
		return nil, fmt.Errorf("parse stdout JSON: %w (raw: %q)", jsonErr, out.String())
	}
	return m, nil
}

// ── get-repo-map ─────────────────────────────────────────────────────────────

func TestCodeGetRepoMap_NoFlags(t *testing.T) {
	m, err := execCodeCmd(t, "get-repo-map")
	if err != nil {
		t.Fatalf("get-repo-map: %v", err)
	}
	if cmd, _ := m["command"].(string); cmd != "get-repo-map" {
		t.Errorf("command: got %q, want get-repo-map", cmd)
	}
}

func TestCodeGetRepoMap_WithMaxFiles(t *testing.T) {
	m, err := execCodeCmd(t, "get-repo-map", "--max-files", "30")
	if err != nil {
		t.Fatalf("get-repo-map --max-files: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--max-files") {
		t.Errorf("argv %q missing --max-files", argv)
	}
	if !strings.Contains(argv, "30") {
		t.Errorf("argv %q missing value 30", argv)
	}
}

func TestCodeGetRepoMap_WithFilePatterns(t *testing.T) {
	m, err := execCodeCmd(t, "get-repo-map", "--file-patterns", "*.go,src/**")
	if err != nil {
		t.Fatalf("get-repo-map --file-patterns: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--file-patterns") {
		t.Errorf("argv %q missing --file-patterns", argv)
	}
}

// ── search-symbols ────────────────────────────────────────────────────────────

func TestCodeSearchSymbols_RequiresArg(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "search-symbols"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when no query provided")
	}
}

func TestCodeSearchSymbols_BasicQuery(t *testing.T) {
	m, err := execCodeCmd(t, "search-symbols", "SearchEngine")
	if err != nil {
		t.Fatalf("search-symbols: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "search-symbols") {
		t.Errorf("argv %q missing search-symbols", argv)
	}
	if !strings.Contains(argv, "SearchEngine") {
		t.Errorf("argv %q missing query SearchEngine", argv)
	}
}

func TestCodeSearchSymbols_AllFlags(t *testing.T) {
	m, err := execCodeCmd(t, "search-symbols", "handleRequest",
		"--max-results", "5",
		"--kinds", "function,method",
		"--file-pattern", "*.go",
	)
	if err != nil {
		t.Fatalf("search-symbols all flags: %v", err)
	}
	argv, _ := m["argv"].(string)
	for _, want := range []string{"--max-results", "5", "--kinds", "function,method", "--file-pattern", "*.go"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
}

// ── search-code ───────────────────────────────────────────────────────────────

func TestCodeSearchCode_RequiresArg(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "search-code"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when no query provided")
	}
}

func TestCodeSearchCode_BasicQuery(t *testing.T) {
	m, err := execCodeCmd(t, "search-code", "incremental indexer")
	if err != nil {
		t.Fatalf("search-code: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "search-code") {
		t.Errorf("argv %q missing search-code", argv)
	}
}

func TestCodeSearchCode_WithLanguage(t *testing.T) {
	m, err := execCodeCmd(t, "search-code", "pagerank", "--language", "go")
	if err != nil {
		t.Fatalf("search-code --language: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--language") || !strings.Contains(argv, "go") {
		t.Errorf("argv %q missing --language go", argv)
	}
}

// ── check-duplicate ───────────────────────────────────────────────────────────

func TestCodeCheckDuplicate_RequiresContentOrFile(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "check-duplicate"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when neither --content nor --content-file is provided")
	}
}

func TestCodeCheckDuplicate_WithContent(t *testing.T) {
	m, err := execCodeCmd(t, "check-duplicate", "--content", "function hello() {}")
	if err != nil {
		t.Fatalf("check-duplicate --content: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "check-duplicate") {
		t.Errorf("argv %q missing check-duplicate", argv)
	}
	if !strings.Contains(argv, "--content") {
		t.Errorf("argv %q missing --content", argv)
	}
}

func TestCodeCheckDuplicate_ContentAndFileMutuallyExclusive(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "check-duplicate", "--content", "x", "--content-file", "/tmp/f"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when both --content and --content-file are provided")
	}
}

// ── find-type-usages ──────────────────────────────────────────────────────────

func TestCodeFindTypeUsages_RequiresArg(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "find-type-usages"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when no type name provided")
	}
}

func TestCodeFindTypeUsages_BasicTypeName(t *testing.T) {
	m, err := execCodeCmd(t, "find-type-usages", "AgentWorkType")
	if err != nil {
		t.Fatalf("find-type-usages: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "find-type-usages") {
		t.Errorf("argv %q missing find-type-usages", argv)
	}
	if !strings.Contains(argv, "AgentWorkType") {
		t.Errorf("argv %q missing AgentWorkType", argv)
	}
}

func TestCodeFindTypeUsages_WithMaxResults(t *testing.T) {
	m, err := execCodeCmd(t, "find-type-usages", "WorkType", "--max-results", "100")
	if err != nil {
		t.Fatalf("find-type-usages --max-results: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "--max-results") || !strings.Contains(argv, "100") {
		t.Errorf("argv %q missing --max-results 100", argv)
	}
}

// ── validate-cross-deps ───────────────────────────────────────────────────────

func TestCodeValidateCrossDeps_NoPath(t *testing.T) {
	m, err := execCodeCmd(t, "validate-cross-deps")
	if err != nil {
		t.Fatalf("validate-cross-deps: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "validate-cross-deps") {
		t.Errorf("argv %q missing validate-cross-deps", argv)
	}
}

func TestCodeValidateCrossDeps_WithPath(t *testing.T) {
	m, err := execCodeCmd(t, "validate-cross-deps", "packages/linear")
	if err != nil {
		t.Fatalf("validate-cross-deps with path: %v", err)
	}
	argv, _ := m["argv"].(string)
	if !strings.Contains(argv, "packages/linear") {
		t.Errorf("argv %q missing scoping path packages/linear", argv)
	}
}

// ── Unavailable binary ────────────────────────────────────────────────────────

func TestCodeCmd_UnavailableBinary(t *testing.T) {
	// Clear any binary resolution env and shadow PATH.
	t.Setenv("AGENTFACTORY_CODE_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // dir with no binaries
	defer func() { _ = os.Setenv("PATH", origPath) }()

	// Verify pnpm really can't be found (skip if it unexpectedly can).
	if _, err := exec.LookPath("pnpm"); err == nil {
		t.Skip("pnpm found in PATH; cannot test unavailable binary path")
	}

	root := &cobra.Command{Use: "af", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd())
	root.SetArgs([]string{"code", "get-repo-map"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when af-code binary is not available")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "af-code") {
		t.Errorf("expected 'not found' or 'af-code' in error, got: %v", err)
	}
}
