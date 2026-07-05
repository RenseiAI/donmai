package afcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

// execCodeCmd builds a fresh `donmai code <subArgs>` command tree with a fake
// binary and runs it, capturing stdout. Returns the decoded JSON map and any
// error.
func execCodeCmd(t *testing.T, subArgs ...string) (map[string]any, error) {
	t.Helper()
	fakeCodeBin(t)

	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))

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
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
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
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
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
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
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
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
	root.SetArgs([]string{"code", "check-duplicate", "--content", "x", "--content-file", "/tmp/f"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when both --content and --content-file are provided")
	}
}

// ── find-type-usages ──────────────────────────────────────────────────────────

func TestCodeFindTypeUsages_RequiresArg(t *testing.T) {
	fakeCodeBin(t)
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
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

// TestCodeCmd_NativeNoExecRequired verifies that get-repo-map and
// search-symbols succeed without any external binary because they use the
// native Go implementation. Only exec-shim commands (search-code, etc.)
// require the donmai-code binary.
func TestCodeCmd_NativeNoExecRequired(t *testing.T) {
	// Clear any binary resolution env and shadow PATH so donmai-code / pnpm
	// cannot be found.
	t.Setenv("AGENTFACTORY_CODE_BIN", "")
	t.Setenv("DONMAI_CODE_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // dir with no binaries
	defer func() { _ = os.Setenv("PATH", origPath) }()

	// get-repo-map must succeed without a binary (native implementation).
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))
	root.SetArgs([]string{"code", "get-repo-map"})
	if err := root.Execute(); err != nil {
		t.Errorf("get-repo-map should not error without exec binary: %v", err)
	}
}

// TestCodeCmd_SearchCodeNativeNoExec verifies that search-code works without
// any external binary (S2 native implementation). DONMAI_CODE_BIN is explicitly
// unset so the native path is used; the command must succeed (returning JSON)
// even when no donmai-code binary exists on PATH.
// ── git-root discovery / --repo-path scoping ──────────────────────────────────

// runNativeCodeCmd runs a real (non-exec-shim) `donmai code <args...>` in dir,
// capturing decoded JSON stdout. Unlike execCodeCmd, this does NOT install a
// fake binary — it exercises the native Go path exactly as a real invocation
// would, which is required to observe cwd()/git-root/--repo-path scoping
// behavior.
func runNativeCodeCmd(t *testing.T, dir string, args ...string) (map[string]any, error) {
	t.Helper()
	t.Setenv("AGENTFACTORY_CODE_BIN", "")
	t.Setenv("DONMAI_CODE_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // no donmai-code/pnpm resolvable
	defer func() { _ = os.Setenv("PATH", origPath) }()

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newCodeCmd(Config{}))

	oldOut := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root.SetArgs(append([]string{"code"}, args...))
	runErr := root.Execute()

	_ = w.Close()
	os.Stdout = oldOut

	var out bytes.Buffer
	if _, readErr := out.ReadFrom(r); readErr != nil {
		t.Fatalf("read stdout pipe: %v", readErr)
	}

	if runErr != nil {
		return nil, runErr
	}

	var m map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &m); jsonErr != nil {
		return nil, fmt.Errorf("parse stdout JSON: %w (raw: %q)", jsonErr, out.String())
	}
	return m, nil
}

// writeSymbolFile writes a minimal Go source file with one exported func, so
// it counts as an indexable file with a symbol.
func writeSymbolFile(t *testing.T, path, funcName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("package p\n\nfunc %s() {}\n", funcName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCodeGetRepoMap_ScopesToGitRootFromSubdirectory is the RED-FIRST test for
// git-root discovery (founder Q4 prerequisite): running a code command from a
// subdirectory of a git checkout must index the WHOLE repo, not just the
// subtree below the invocation cwd. Before the fix, cwd() is a bare
// os.Getwd(), so this fails (the repo map only sees the subdirectory's file).
func TestCodeGetRepoMap_ScopesToGitRootFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSymbolFile(t, filepath.Join(root, "root.go"), "Root")
	sub := filepath.Join(root, "sub")
	writeSymbolFile(t, filepath.Join(sub, "sub.go"), "Sub")

	m, err := runNativeCodeCmd(t, sub, "get-repo-map")
	if err != nil {
		t.Fatalf("get-repo-map: %v", err)
	}
	filesCount, _ := m["files"].(float64)
	if filesCount < 2 {
		t.Errorf("get-repo-map invoked from subdirectory %s indexed %v files, want >= 2 (whole git repo, including root.go outside the invocation cwd)", sub, filesCount)
	}
}

// TestCodeGetRepoMap_RepoPathScopesSubtree verifies --repo-path narrows
// indexing to gitRoot/repoPath (the monorepo subtree scoping the Wave-2 MCP
// server will drive via this exact flag).
func TestCodeGetRepoMap_RepoPathScopesSubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSymbolFile(t, filepath.Join(root, "pkga", "a.go"), "A")
	writeSymbolFile(t, filepath.Join(root, "pkgb", "b.go"), "B")
	writeSymbolFile(t, filepath.Join(root, "pkgb", "b2.go"), "B2")

	m, err := runNativeCodeCmd(t, root, "get-repo-map", "--repo-path", "pkgb")
	if err != nil {
		t.Fatalf("get-repo-map --repo-path pkgb: %v", err)
	}
	filesCount, _ := m["files"].(float64)
	if filesCount != 2 {
		t.Errorf("get-repo-map --repo-path pkgb indexed %v files, want exactly 2 (pkgb only)", filesCount)
	}
}

// TestCodeGetRepoMap_RepoPathRejectsAbsolutePath verifies --repo-path must be
// relative to the git root; an absolute path is rejected.
func TestCodeGetRepoMap_RepoPathRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := runNativeCodeCmd(t, root, "get-repo-map", "--repo-path", "/etc")
	if err == nil {
		t.Fatal("expected error for absolute --repo-path")
	}
}

// TestCodeGetRepoMap_RepoPathRejectsTraversal is the required path-traversal
// test: a --repo-path that escapes the git root via ../ must be rejected even
// after filepath.Clean.
func TestCodeGetRepoMap_RepoPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := runNativeCodeCmd(t, sub, "get-repo-map", "--repo-path", "../../../../../../etc")
	if err == nil {
		t.Fatal("expected error for --repo-path that escapes the git root via ../")
	}
}

// TestCodeGetRepoMap_RepoPathRejectsNonexistentPath verifies a --repo-path
// that doesn't exist under the git root is rejected.
func TestCodeGetRepoMap_RepoPathRejectsNonexistentPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := runNativeCodeCmd(t, root, "get-repo-map", "--repo-path", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent --repo-path")
	}
}

// TestCodeGetRepoMap_RepoPathRejectsNonDirectory verifies a --repo-path
// pointing at a file (not a directory) is rejected.
func TestCodeGetRepoMap_RepoPathRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSymbolFile(t, filepath.Join(root, "file.go"), "F")

	_, err := runNativeCodeCmd(t, root, "get-repo-map", "--repo-path", "file.go")
	if err == nil {
		t.Fatal("expected error for --repo-path that is a file, not a directory")
	}
}

func TestCodeCmd_SearchCodeNativeNoExec(t *testing.T) {
	t.Setenv("AGENTFACTORY_CODE_BIN", "")
	t.Setenv("DONMAI_CODE_BIN", "")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	defer func() { _ = os.Setenv("PATH", origPath) }()

	// Run in a temp dir with a minimal Go file so the index has something.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Change into the temp dir so cwd() picks it up.
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	var buf bytes.Buffer
	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(&buf)
	root.AddCommand(newCodeCmd(Config{}))
	root.SetArgs([]string{"code", "search-code", "main"})
	if err := root.Execute(); err != nil {
		t.Fatalf("search-code should not error with native S2 impl: %v", err)
	}
}
