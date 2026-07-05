package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	mcp "github.com/RenseiAI/donmai/runtime/mcp"
	"github.com/RenseiAI/donmai/runtime/mcp/server"
)

// TestMain doubles the test binary as the code-intel MCP server: when
// re-executed with MCP_CODEINTEL_SERVE=1 it runs server.Serve on
// stdin/stdout instead of the test suite. This exercises the real subprocess
// stdio transport that runtime/mcp.Dial drives — the conformance oracle.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_CODEINTEL_SERVE") == "1" {
		serveForTest()
		return
	}
	os.Exit(m.Run())
}

func serveForTest() {
	cfg := server.Config{
		Root:     os.Getenv("MCP_CODEINTEL_ROOT"),
		RepoPath: os.Getenv("MCP_CODEINTEL_REPO_PATH"),
		// Warm-up + lifecycle logging MUST go to stderr, never stdout (the
		// protocol channel). The stdout-purity test asserts this.
		Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[mcp] "+format+"\n", args...) },
	}
	if t := os.Getenv("MCP_CODEINTEL_TOOLS"); t != "" {
		cfg.Tools = strings.Split(t, ",")
	}
	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp code-intel:", err)
		os.Exit(2)
	}
	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp code-intel serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// fixtureRepo writes a tiny multi-file Go repo the engine can index.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"greet.go": `package greet

// Greeter greets people.
type Greeter struct{ Name string }

// GreetUser returns a greeting.
func GreetUser(name string) string { return "Hello, " + name }

// Greet returns a greeting from the Greeter.
func (g *Greeter) Greet() string { return "Hello, " + g.Name }
`,
		"util.go": `package greet

// Shout upper-cases loudly.
func Shout(s string) string { return s }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func dialServer(t *testing.T, root string, tools string) mcp.Client {
	t.Helper()
	env := map[string]string{"MCP_CODEINTEL_SERVE": "1", "MCP_CODEINTEL_ROOT": root}
	if tools != "" {
		env["MCP_CODEINTEL_TOOLS"] = tools
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, err := mcp.Dial(ctx, agent.MCPServerConfig{
		Name:    server.ServerName,
		Command: os.Args[0],
		Env:     env,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConformance_ListAndCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := dialServer(t, fixtureRepo(t), "")

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]bool, len(tools))
	for _, td := range tools {
		got[td.Name] = true
		if len(td.InputSchema) == 0 {
			t.Errorf("tool %s missing inputSchema", td.Name)
		}
	}
	want := []string{
		"af_code_get_repo_map", "af_code_search_symbols", "af_code_search_code",
		"af_code_check_duplicate", "af_code_find_type_usages", "af_code_validate_cross_deps",
	}
	if len(tools) != len(want) {
		t.Fatalf("ListTools returned %d tools, want %d: %+v", len(tools), len(want), tools)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing tool %q", name)
		}
	}

	// Call search-symbols on the fixture: non-empty, correct result.
	res, err := c.CallTool(ctx, "af_code_search_symbols", map[string]any{"query": "Greeter"})
	if err != nil {
		t.Fatalf("CallTool search-symbols: %v", err)
	}
	if res.IsError {
		t.Fatalf("search-symbols reported isError: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Greeter") {
		t.Fatalf("search-symbols result should mention Greeter, got: %s", res.Content)
	}

	// get-repo-map returns the indexed file set.
	rm, err := c.CallTool(ctx, "af_code_get_repo_map", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool get-repo-map: %v", err)
	}
	if rm.IsError || !strings.Contains(rm.Content, "greet.go") {
		t.Fatalf("get-repo-map should list greet.go, got: %s", rm.Content)
	}
}

func TestConformance_ToolsFilter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := dialServer(t, fixtureRepo(t), "af_code_search_symbols,af_code_get_repo_map")

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("filtered tools/list = %d, want 2: %+v", len(tools), tools)
	}

	// A disabled tool must not be callable — the client surfaces the JSON-RPC
	// error as a Go error (transport-level), not an isError result.
	if _, err := c.CallTool(ctx, "af_code_search_code", map[string]any{"query": "x"}); err == nil {
		t.Fatal("calling a disabled tool should error")
	}
}

func TestConformance_TraversalFilePatternStaysInRoot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Plant a secret OUTSIDE the served root; the engine is root-scoped via
	// New(root), so a malicious file-pattern must never reach it.
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "in.go"), []byte("package p\nfunc InRoot() {}\n"), 0o600); err != nil {
		t.Fatalf("write in-root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.go"), []byte("package s\nfunc TopSecret() {}\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	c := dialServer(t, root, "")
	res, err := c.CallTool(ctx, "af_code_get_repo_map", map[string]any{
		"filePatterns": []string{"../**", "../*.go", "**"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if strings.Contains(res.Content, "secret") || strings.Contains(res.Content, "TopSecret") {
		t.Fatalf("root-scope breach: repo-map leaked a file outside --root: %s", res.Content)
	}
}

func TestConformance_ConcurrentCallsSafe(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := dialServer(t, fixtureRepo(t), "")

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var res mcp.ToolResult
			var err error
			switch i % 3 {
			case 0:
				res, err = c.CallTool(ctx, "af_code_search_symbols", map[string]any{"query": "Greeter"})
			case 1:
				res, err = c.CallTool(ctx, "af_code_search_code", map[string]any{"query": "Shout"})
			case 2:
				res, err = c.CallTool(ctx, "af_code_get_repo_map", map[string]any{})
			}
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if res.IsError {
				errs <- fmt.Errorf("call %d isError: %s", i, res.Content)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestStdoutPurity drives the server subprocess directly (bypassing the
// client) and asserts EVERY line it writes to stdout is a well-formed JSON-RPC
// message, and that warm-up logging lands on stderr — never stdout.
func TestStdoutPurity(t *testing.T) {
	t.Parallel()
	root := fixtureRepo(t)
	cmd := exec.Command(os.Args[0]) //nolint:gosec // re-exec of the test binary
	cmd.Env = append(os.Environ(), "MCP_CODEINTEL_SERVE=1", "MCP_CODEINTEL_ROOT="+root)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"af_code_search_symbols","arguments":{"query":"Greeter"}}}`,
	}
	for _, r := range reqs {
		if _, err := stdin.Write([]byte(r + "\n")); err != nil {
			t.Fatalf("write req: %v", err)
		}
	}
	_ = stdin.Close() // EOF → graceful shutdown

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("server exited with error: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("server did not exit on stdin EOF")
	}

	// Every stdout line must be a valid JSON-RPC 2.0 message and nothing else.
	sc := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	lines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("non-JSON line on stdout: %q (%v)", line, err)
		}
		if msg.JSONRPC != "2.0" {
			t.Fatalf("stdout line missing jsonrpc:2.0: %q", line)
		}
		if len(msg.Result) == 0 && len(msg.Error) == 0 {
			t.Fatalf("stdout response has neither result nor error: %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	// initialize, ping, tools/list, tools/call → 4 responses (the notification
	// gets none).
	if lines != 4 {
		t.Fatalf("stdout produced %d response lines, want 4:\n%s", lines, stdout.String())
	}
	// Warm-up log proves logging went to stderr, not stdout.
	if !strings.Contains(stderr.String(), "[mcp]") {
		t.Fatalf("expected warm-up logging on stderr, got: %q", stderr.String())
	}
}
