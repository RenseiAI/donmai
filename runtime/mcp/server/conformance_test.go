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

	"github.com/RenseiAI/donmai/afcli"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	mcp "github.com/RenseiAI/donmai/runtime/mcp"
	"github.com/RenseiAI/donmai/runtime/mcp/server"
	"github.com/spf13/cobra"
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
	if os.Getenv("MCP_CODEINTEL_AFCLI_SERVE") == "1" {
		serveAFCLIForTest()
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

// serveAFCLIForTest drives the hidden MCP command through afcli's public
// RegisterCommands entry point. The subprocess is otherwise a clean process:
// it receives only the explicit root path and stdio JSON-RPC transport.
func serveAFCLIForTest() {
	root := &cobra.Command{Use: "donmai", SilenceUsage: true}
	afcli.RegisterCommands(root, afcli.Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
	})
	root.SetArgs([]string{"mcp", "code-intel", "--root", os.Getenv("MCP_CODEINTEL_ROOT")})
	root.SetIn(os.Stdin)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "afcli mcp code-intel:", err)
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

func dialAFCLIServer(t *testing.T, root string) mcp.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, err := mcp.Dial(ctx, agent.MCPServerConfig{
		Name:    server.ServerName,
		Command: os.Args[0],
		Env: map[string]string{
			"MCP_CODEINTEL_AFCLI_SERVE": "1",
			"MCP_CODEINTEL_ROOT":        root,
		},
	})
	if err != nil {
		t.Fatalf("Dial afcli mcp code-intel: %v", err)
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

// TestConformance_AFCLIPublicContract is the externally-shaped contract gate.
// It starts a clean subprocess through afcli.RegisterCommands, performs the
// MCP handshake, lists exactly the canonical six tools, and calls every tool.
func TestConformance_AFCLIPublicContract(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := dialAFCLIServer(t, fixtureRepo(t))

	type contract struct {
		name        string
		description string
		properties  []string
		required    []string
		arguments   map[string]any
		output      func(t *testing.T, content string)
	}
	contracts := []contract{
		{
			name:        "af_code_get_repo_map",
			description: "Repo map ranked by import centrality: most important files + their symbols. Call FIRST to orient.",
			properties:  []string{"maxFiles", "filePatterns"},
			arguments:   map[string]any{},
			output: func(t *testing.T, content string) {
				requireObjectFields(t, "af_code_get_repo_map", content, "entries", "rootHash", "files")
			},
		},
		{
			name:        "af_code_search_symbols",
			description: "Search symbols by name (functions, methods, types, ...); exact names return only the exact hits.",
			properties:  []string{"query", "maxResults", "kinds", "filePattern", "includeDoc"},
			required:    []string{"query"},
			arguments:   map[string]any{"query": "Greeter"},
			output: func(t *testing.T, content string) {
				requireSearchResult(t, "af_code_search_symbols", content)
			},
		},
		{
			name:        "af_code_search_code",
			description: "Keyword search over code content with code-aware tokenization (camelCase/snake_case).",
			properties:  []string{"query", "maxResults", "language", "includeDoc"},
			required:    []string{"query"},
			arguments:   map[string]any{"query": "Greet"},
			output: func(t *testing.T, content string) {
				requireSearchResult(t, "af_code_search_code", content)
			},
		},
		{
			name:        "af_code_check_duplicate",
			description: "Check whether code already exists (exact or near duplicate). Pass content OR contentFile.",
			properties:  []string{"content", "contentFile", "maxResults"},
			arguments:   map[string]any{"content": "func NotInFixture() {}"},
			output: func(t *testing.T, content string) {
				requireObjectFields(t, "af_code_check_duplicate", content, "isDuplicate", "matchType", "existingId", "hammingDistance")
			},
		},
		{
			name:        "af_code_find_type_usages",
			description: "Find every usage site of a named type. Call BEFORE a cross-file rename/refactor to list all sites.",
			properties:  []string{"typeName", "maxResults"},
			required:    []string{"typeName"},
			arguments:   map[string]any{"typeName": "Greeter"},
			output: func(t *testing.T, content string) {
				requireObjectFields(t, "af_code_find_type_usages", content, "typeName", "totalUsages", "usages", "switchStatements", "mappingObjects")
			},
		},
		{
			name:        "af_code_validate_cross_deps",
			description: "Validate monorepo cross-package imports against package.json dependency declarations.",
			properties:  []string{"path"},
			arguments:   map[string]any{},
			output: func(t *testing.T, content string) {
				requireObjectFields(t, "af_code_validate_cross_deps", content, "valid", "missingDeps", "packagesChecked", "filesChecked")
			},
		},
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools through afcli: %v", err)
	}
	if len(tools) != len(contracts) {
		t.Fatalf("afcli tools/list returned %d tools, want %d: %+v", len(tools), len(contracts), tools)
	}
	for i, want := range contracts {
		got := tools[i]
		if got.Name != want.name {
			t.Errorf("tools[%d].Name = %q, want %q", i, got.Name, want.name)
		}
		if got.Description != want.description {
			t.Errorf("tool %s description = %q, want %q", want.name, got.Description, want.description)
		}
		var schema struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties bool                       `json:"additionalProperties"`
		}
		if err := json.Unmarshal(got.InputSchema, &schema); err != nil {
			t.Errorf("tool %s inputSchema is invalid JSON: %v", want.name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("tool %s inputSchema.type = %q, want object", want.name, schema.Type)
		}
		if schema.AdditionalProperties {
			t.Errorf("tool %s inputSchema allows unknown properties", want.name)
		}
		for _, property := range want.properties {
			if _, ok := schema.Properties[property]; !ok {
				t.Errorf("tool %s inputSchema missing property %q", want.name, property)
			}
		}
		if len(schema.Properties) != len(want.properties) {
			t.Errorf("tool %s inputSchema has %d properties, want %d: %v", want.name, len(schema.Properties), len(want.properties), schema.Properties)
		}
		if strings.Join(schema.Required, ",") != strings.Join(want.required, ",") {
			t.Errorf("tool %s inputSchema.required = %v, want %v", want.name, schema.Required, want.required)
		}

		res, err := c.CallTool(ctx, want.name, want.arguments)
		if err != nil {
			t.Errorf("CallTool %s through afcli: %v", want.name, err)
			continue
		}
		if res.IsError {
			t.Errorf("CallTool %s through afcli returned isError: %s", want.name, res.Content)
			continue
		}
		want.output(t, res.Content)
	}
}

func TestConformance_AFCLICapabilityDiscovery(t *testing.T) {
	t.Parallel()
	root := fixtureRepo(t)
	cmd := exec.Command(os.Args[0]) //nolint:gosec // re-exec of the test binary
	cmd.Env = append(os.Environ(), "MCP_CODEINTEL_AFCLI_SERVE=1", "MCP_CODEINTEL_ROOT="+root)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start afcli MCP server: %v", err)
	}
	if _, err := fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"contract-test","version":"0"}}}`); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	var response struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]json.RawMessage `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(stdout).Decode(&response); err != nil {
		t.Fatalf("decode initialize response: %v (stderr: %s)", err, stderr.String())
	}
	if response.JSONRPC != "2.0" || response.ID != 1 {
		t.Errorf("initialize envelope = %+v, want jsonrpc 2.0 with id 1", response)
	}
	if response.Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize protocolVersion = %q, want 2025-03-26", response.Result.ProtocolVersion)
	}
	if response.Result.ServerInfo.Name != "af-code-intelligence" {
		t.Errorf("initialize serverInfo.name = %q, want af-code-intelligence", response.Result.ServerInfo.Name)
	}
	if response.Result.ServerInfo.Version != "0.1.0" {
		t.Errorf("initialize serverInfo.version = %q, want 0.1.0", response.Result.ServerInfo.Version)
	}
	if response.Result.Capabilities.Tools == nil {
		t.Error("initialize capabilities must advertise tools")
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("afcli MCP server exited with error: %v (stderr: %s)", err, stderr.String())
	}
}

func requireObjectFields(t *testing.T, tool, content string, fields ...string) {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatalf("%s output is not a JSON object: %v\n%s", tool, err, content)
	}
	for _, field := range fields {
		if _, ok := result[field]; !ok {
			t.Errorf("%s output missing required field %q: %s", tool, field, content)
		}
	}
}

func requireSearchResult(t *testing.T, tool, content string) {
	t.Helper()
	var result []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatalf("%s output is not a JSON array: %v\n%s", tool, err, content)
	}
	if len(result) == 0 {
		t.Fatalf("%s output is empty for the fixture: %s", tool, content)
	}
	for _, field := range []string{"symbol", "score", "matchType"} {
		if _, ok := result[0][field]; !ok {
			t.Errorf("%s output[0] missing required field %q: %s", tool, field, content)
		}
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
