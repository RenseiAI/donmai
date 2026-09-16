//go:build codex_integration

package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const realMCPFixtureVersion = "codex-cli 0.154.0"

var errRealMCPFixtureComplete = errors.New("real MCP fixture completed before PTY")

type realMCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Arguments   map[string]any
}

func realMCPTools() []realMCPTool {
	return []realMCPTool{
		{
			Name: "fixture_object", Description: "accepts a nested object",
			InputSchema: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"payload": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "label": map[string]any{"type": "string"}},
					"required":   []any{"enabled", "label"},
				}},
				"required": []any{"payload"},
			},
			Arguments: map[string]any{"payload": map[string]any{"enabled": true, "label": "object"}},
		},
		{
			Name: "fixture_text", Description: "accepts bounded text",
			InputSchema: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"text": map[string]any{"type": "string", "minLength": float64(1), "maxLength": float64(32)}},
				"required":   []any{"text"},
			},
			Arguments: map[string]any{"text": "alpha"},
		},
		{
			Name: "fixture_integer", Description: "accepts one positive integer",
			InputSchema: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(9)}},
				"required":   []any{"count"},
			},
			Arguments: map[string]any{"count": float64(3)},
		},
		{
			Name: "fixture_array", Description: "accepts an ordered string array",
			InputSchema: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": float64(2)}},
				"required":   []any{"items"},
			},
			Arguments: map[string]any{"items": []any{"first", "second"}},
		},
		{
			Name: "fixture_choice", Description: "accepts one closed choice",
			InputSchema: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"choice": map[string]any{"type": "string", "enum": []any{"left", "right"}}},
				"required":   []any{"choice"},
			},
			Arguments: map[string]any{"choice": "right"},
		},
	}
}

type realMCPHTTPFixture struct {
	t              *testing.T
	server         *httptest.Server
	expectedHeader string
	tools          map[string]realMCPTool

	mu          sync.Mutex
	requests    []string
	headerOK    bool
	callArgs    map[string]map[string]any
	callResults map[string]string
}

func newRealMCPHTTPFixture(t *testing.T, tools []realMCPTool, expectedHeader string) *realMCPHTTPFixture {
	t.Helper()
	f := &realMCPHTTPFixture{
		t: t, expectedHeader: expectedHeader, tools: make(map[string]realMCPTool, len(tools)),
		headerOK: true, callArgs: make(map[string]map[string]any), callResults: make(map[string]string),
	}
	for _, tool := range tools {
		f.tools[tool.Name] = tool
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *realMCPHTTPFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headerOK = f.headerOK && r.Header.Get("Authorization") == f.expectedHeader
	f.mu.Unlock()
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, request.Method)
	f.mu.Unlock()
	if len(request.ID) == 0 || bytes.Equal(request.ID, []byte("null")) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "generic-five-tool-fixture", "version": "1"},
		}
	case "tools/list":
		names := make([]string, 0, len(f.tools))
		for name := range f.tools {
			names = append(names, name)
		}
		sort.Strings(names)
		listed := make([]map[string]any, 0, len(names))
		for _, name := range names {
			tool := f.tools[name]
			listed = append(listed, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema})
		}
		result = map[string]any{"tools": listed}
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &call); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tool, ok := f.tools[call.Name]
		if !ok || !reflect.DeepEqual(call.Arguments, tool.Arguments) {
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "invalid call"}}, "isError": true}
			break
		}
		text := "called:" + call.Name
		f.mu.Lock()
		f.callArgs[call.Name] = call.Arguments
		f.callResults[call.Name] = text
		f.mu.Unlock()
		result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": text}},
			"structuredContent": map[string]any{"tool": call.Name, "arguments": call.Arguments},
			"isError":           false,
		}
	case "ping":
		result = map[string]any{}
	default:
		f.writeRPC(w, request.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
		return
	}
	f.writeRPC(w, request.ID, result, nil)
}

func (f *realMCPHTTPFixture) writeRPC(w http.ResponseWriter, id json.RawMessage, result, rpcErr any) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test-selected installed fixture path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func requireCodexFixtureBinaries(t *testing.T) (launcher, native string) {
	t.Helper()
	launcher = strings.TrimSpace(os.Getenv("CODEX_BIN"))
	native = strings.TrimSpace(os.Getenv("CODEX_NATIVE_BIN"))
	if launcher == "" || native == "" {
		t.Fatal("CODEX_BIN and CODEX_NATIVE_BIN are required for the mandatory real MCP fixture")
	}
	for label, path := range map[string]string{"launcher": launcher, "native": native} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			t.Fatalf("%s executable %q is unavailable: %v", label, path, err)
		}
		out, err := exec.Command(path, "--version").CombinedOutput() //nolint:gosec // explicitly supplied pinned fixture executable
		if err != nil || strings.TrimSpace(string(out)) != realMCPFixtureVersion {
			t.Fatalf("%s version = %q, %v; want %q", label, strings.TrimSpace(string(out)), err, realMCPFixtureVersion)
		}
	}
	t.Logf("Codex launcher sha256=%s", sha256File(t, launcher))
	t.Logf("Codex native executable sha256=%s", sha256File(t, native))
	return launcher, native
}

func writeNoUIFixtureWrapper(t *testing.T, launcher string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "unexpected-pty")
	path := filepath.Join(dir, "codex-fixture-wrapper")
	body := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = mcp ]; then exec " + strconv.Quote(launcher) + " \"$@\"; fi\ndone\nprintf forbidden > " + strconv.Quote(marker) + "\nexit 86\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, marker
}

type directToolCallResponse struct {
	Content           []map[string]any `json:"content"`
	StructuredContent map[string]any   `json:"structuredContent"`
	IsError           *bool            `json:"isError"`
}

func runDirectMCPFixture(
	t *testing.T,
	ctx context.Context,
	native string,
	spec agent.Spec,
	launch interactiveLaunch,
	ownedHome string,
	tools []realMCPTool,
) {
	t.Helper()
	configArgs, err := interactiveConfigArgs(launch.argv)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"app-server", "--stdio"}, configArgs...)
	args = append(args, "--strict-config")
	cmd := exec.CommandContext(ctx, native, args...) //nolint:gosec // pinned fixture executable and production-derived config args
	cmd.Dir = spec.Cwd
	cmd.Env = mergeEnv(nil, launch.env, ownedHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		if finished {
			return
		}
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	client := NewClient(stdin, stdout)
	t.Cleanup(func() { client.Stop(errors.New("fixture complete")) })
	methods := make([]string, 0, 4+len(tools))
	request := func(method string, params map[string]any) json.RawMessage {
		t.Helper()
		methods = append(methods, method)
		raw, err := client.Request(ctx, method, params, 15*time.Second)
		if err != nil {
			t.Fatalf("%s: %v (stderr=%q)", method, err, stderr.String())
		}
		return raw
	}
	_ = request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "generic-mcp-fixture", "title": "Generic MCP Fixture", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	methods = append(methods, "initialized")
	threadRaw := request("thread/start", map[string]any{
		"cwd": spec.Cwd, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true,
	})
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(threadRaw, &thread); err != nil || thread.Thread.ID == "" {
		t.Fatalf("thread/start response: %v, %s", err, threadRaw)
	}
	statusRaw := request("mcpServerStatus/list", map[string]any{"threadId": thread.Thread.ID, "detail": "toolsAndAuthOnly"})
	var status struct {
		Data []struct {
			Name          string `json:"name"`
			RuntimeStatus string `json:"runtimeStatus"`
			Tools         map[string]struct {
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal(statusRaw, &status); err != nil || len(status.Data) != 1 || status.Data[0].Name != "fixture-platform" || status.Data[0].RuntimeStatus != "connected" || len(status.Data[0].Tools) != len(tools) {
		t.Fatalf("MCP status = %+v, %v", status, err)
	}
	for _, tool := range tools {
		listed, ok := status.Data[0].Tools[tool.Name]
		if !ok || !reflect.DeepEqual(listed.InputSchema, tool.InputSchema) {
			t.Fatalf("tool schema %q = %#v, want %#v", tool.Name, listed.InputSchema, tool.InputSchema)
		}
		raw := request("mcpServer/tool/call", map[string]any{
			"threadId": thread.Thread.ID, "server": "fixture-platform", "tool": tool.Name, "arguments": tool.Arguments,
		})
		var response directToolCallResponse
		if err := json.Unmarshal(raw, &response); err != nil || response.IsError == nil || *response.IsError || response.StructuredContent["tool"] != tool.Name || len(response.Content) != 1 || response.Content[0]["text"] != "called:"+tool.Name {
			t.Fatalf("tool call %q response = %+v, %v", tool.Name, response, err)
		}
	}
	for _, method := range methods {
		if method == "turn/start" {
			t.Fatal("fixture sent a model turn")
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("app-server exit: %v (stderr=%q)", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("app-server did not exit after owned stdin closed")
	}
	if strings.TrimSpace(stderr.String()) != "" {
		t.Fatalf("app-server stderr = %q", stderr.String())
	}
	methodSummary, _ := json.Marshal(methods)
	t.Logf("real MCP method summary sha256=%s", func() string { sum := sha256.Sum256(methodSummary); return hex.EncodeToString(sum[:]) }())
}

func TestIntegration_RealCodexInteractiveMCPDirectCalls(t *testing.T) {
	launcher, native := requireCodexFixtureBinaries(t)
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	ambientHome := filepath.Join(root, "ambient-home")
	boundaryRoot := filepath.Join(root, "boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{ambientHome, boundaryRoot, workdir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ambientHome, "config.toml"), []byte("[mcp_servers.ambient]\ncommand=\"/usr/bin/false\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", ambientHome)
	tools := realMCPTools()
	const fakeBearer = "fixture-bearer-not-a-real-credential"
	fixture := newRealMCPHTTPFixture(t, tools, "Bearer "+fakeBearer)
	wrapper, unexpectedPTYMarker := writeNoUIFixtureWrapper(t, launcher)
	var callbackCalls int

	_, err := SpawnInteractive(t.Context(), Options{
		CodexBin:      wrapper,
		configTempDir: boundaryRoot,
		interactiveAuthSeeder: func(context.Context, string, string, interactiveCodexAuthProjection) error {
			return nil
		},
		interactiveBeforePTYSpawn: func(ctx context.Context, _ string, spec agent.Spec, launch interactiveLaunch, ownedHome string) error {
			callbackCalls++
			if sameResolvedPath(ownedHome, ambientHome) {
				t.Fatal("interactive boundary reused ambient CODEX_HOME")
			}
			for _, arg := range launch.argv {
				if strings.Contains(arg, fakeBearer) || strings.Contains(arg, "ambient") {
					t.Fatal("interactive args exposed a bearer or ambient MCP config")
				}
			}
			runDirectMCPFixture(t, ctx, native, spec, launch, ownedHome, tools)
			return errRealMCPFixtureComplete
		},
	}, agent.Spec{
		Cwd: workdir,
		Env: map[string]string{
			"OPENAI_API_KEY": "fixture-model-auth-not-a-real-credential",
		},
		MCPServers: []agent.MCPServerConfig{{
			Name: "fixture-platform", Type: "http", URL: fixture.server.URL + "/api/mcp/fixture-session",
			Headers: map[string]string{"Authorization": "Bearer " + fakeBearer},
		}},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if !errors.Is(err, errRealMCPFixtureComplete) || callbackCalls != 1 {
		t.Fatalf("SpawnInteractive = %v, callbackCalls=%d", err, callbackCalls)
	}
	if _, err := os.Stat(unexpectedPTYMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interactive PTY path ran: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !fixture.headerOK {
		t.Fatal("fixture Authorization header was missing or changed")
	}
	if len(fixture.callArgs) != len(tools) || len(fixture.callResults) != len(tools) {
		t.Fatalf("fixture calls = args:%d results:%d, want %d", len(fixture.callArgs), len(fixture.callResults), len(tools))
	}
	for _, tool := range tools {
		if !reflect.DeepEqual(fixture.callArgs[tool.Name], tool.Arguments) || fixture.callResults[tool.Name] != "called:"+tool.Name {
			t.Fatalf("fixture call %q = args:%#v result:%q", tool.Name, fixture.callArgs[tool.Name], fixture.callResults[tool.Name])
		}
	}
	for _, method := range fixture.requests {
		if method != "initialize" && method != "notifications/initialized" && method != "tools/list" && method != "tools/call" && method != "ping" {
			t.Fatalf("unexpected MCP method %q", method)
		}
	}
	t.Logf("real MCP calls=%d HTTP requests=%d", len(fixture.callArgs), len(fixture.requests))
}
