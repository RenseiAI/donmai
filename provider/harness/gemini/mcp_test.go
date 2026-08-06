package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

func TestMain(m *testing.M) {
	if os.Getenv("GEMINI_MCP_FAKE_SERVER") == "1" {
		runGeminiFakeMCPServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runGeminiFakeMCPServer() {
	if report := os.Getenv("GEMINI_MCP_ENV_REPORT"); report != "" {
		status := "clean"
		for _, key := range []string{"ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL"} {
			if _, ok := os.LookupEnv(key); ok {
				status = "leaked"
			}
		}
		if os.Getenv("GEMINI_MCP_SAFE_ENV") != "present" {
			status = "missing-safe-env"
		}
		_ = os.WriteFile(report, []byte(status), 0o600) //nolint:gosec // test re-exec writes its caller-owned temp report
	}

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": mcp.ProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "gemini-test", "version": "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo"}}}
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

// fakeMCPClient is an in-memory mcp.Client for bridge unit tests.
type fakeMCPClient struct {
	tools    []mcp.ToolDef
	listErr  error
	calls    []string // "<tool>:<text-arg>" per CallTool
	closed   bool
	callFunc func(name string, args map[string]any) (mcp.ToolResult, error)
}

func (f *fakeMCPClient) ListTools(context.Context) ([]mcp.ToolDef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

func (f *fakeMCPClient) CallTool(_ context.Context, name string, args map[string]any) (mcp.ToolResult, error) {
	text, _ := args["text"].(string)
	f.calls = append(f.calls, name+":"+text)
	if f.callFunc != nil {
		return f.callFunc(name, args)
	}
	return mcp.ToolResult{Content: "ran " + name}, nil
}

func (f *fakeMCPClient) Close() error {
	f.closed = true
	return nil
}

// dialerFor builds an mcpDialer returning canned clients/errors by server
// name.
func dialerFor(t *testing.T, clients map[string]*fakeMCPClient, errs map[string]error) mcpDialer {
	t.Helper()
	return func(_ context.Context, s agent.MCPServerConfig) (mcp.Client, error) {
		if err, ok := errs[s.Name]; ok {
			return nil, err
		}
		c, ok := clients[s.Name]
		if !ok {
			t.Fatalf("unexpected dial for server %q", s.Name)
		}
		return c, nil
	}
}

func TestMCPBridge_DiscoveryDeclaresAndRoutes(t *testing.T) {
	t.Parallel()
	fake := &fakeMCPClient{tools: []mcp.ToolDef{
		{Name: "get_issue", Description: "fetch one issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":false,"$schema":"x"}`)},
		{Name: "list_issues"},
	}}
	servers := []agent.MCPServerConfig{{Name: "af_linear", Type: "http", URL: "http://unused"}}
	b := newMCPBridge(context.Background(), servers, dialerFor(t, map[string]*fakeMCPClient{"af_linear": fake}, nil))
	if b == nil {
		t.Fatal("bridge: want non-nil")
	}

	decls := b.decls["af_linear"]
	if len(decls) != 2 || decls[0].Name != "mcp__af_linear__get_issue" {
		t.Fatalf("decls = %+v, want discovered mcp__af_linear__* pair", decls)
	}
	if decls[0].Description != "fetch one issue" {
		t.Errorf("description = %q", decls[0].Description)
	}
	// Schema sanitized: whitelisted keys survive, foreign keys stripped.
	params := decls[0].Parameters
	if params["type"] != "object" {
		t.Errorf("schema type = %v", params["type"])
	}
	if _, leaked := params["additionalProperties"]; leaked {
		t.Error("additionalProperties must be stripped (Gemini rejects unknown schema keys)")
	}
	if _, leaked := params["$schema"]; leaked {
		t.Error("$schema must be stripped")
	}
	// Empty-description tool gets a fallback description.
	if decls[1].Description == "" {
		t.Error("empty tool description should get a server fallback")
	}

	// Exact-route call.
	res, err := b.call(context.Background(), "mcp__af_linear__get_issue", map[string]any{"text": "REQ"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Content != "ran get_issue" || len(fake.calls) != 1 || fake.calls[0] != "get_issue:REQ" {
		t.Errorf("routing: res=%+v calls=%v", res, fake.calls)
	}

	// Name-parse fallback: a tool not discovered at spawn (e.g. asserted
	// via MCPToolNames) still routes by server name.
	if _, err := b.call(context.Background(), "mcp__af_linear__undeclared_tool", nil); err != nil {
		t.Fatalf("fallback routing: %v", err)
	}
	if fake.calls[1] != "undeclared_tool:" {
		t.Errorf("fallback call = %v", fake.calls)
	}

	b.Close()
	if !fake.closed {
		t.Error("Close must close the underlying client")
	}
}

func TestMCPBridge_RealStdioChildEnvSanitized(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	report := t.TempDir() + "/env-report.txt"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	bridge := newMCPBridge(ctx, []agent.MCPServerConfig{{
		Name:    "real-stdio",
		Command: os.Args[0],
		Env: map[string]string{
			"GEMINI_MCP_FAKE_SERVER": "1",
			"GEMINI_MCP_ENV_REPORT":  report,
			"GEMINI_MCP_SAFE_ENV":    "present",
			"ATTACH_TOKEN":           "explicit-secret",
			"ATTACH_TOKEN_FILE":      "/explicit/token",
			"ATTACH_URL":             "wss://explicit.invalid/v1/rooms/room-1",
		},
	}}, mcp.Dial)
	if bridge == nil {
		t.Fatal("bridge: want non-nil")
	}
	defer bridge.Close()
	if err := bridge.failures["real-stdio"]; err != nil {
		t.Fatalf("real stdio bridge failed: %v", err)
	}
	if _, ok := bridge.clients["real-stdio"]; !ok {
		t.Fatalf("real stdio bridge did not retain connected client: %+v", bridge)
	}

	body, err := os.ReadFile(report) //nolint:gosec // report is a test-owned temp file
	if err != nil {
		t.Fatalf("read env report: %v", err)
	}
	if got, want := string(body), "clean"; got != want {
		t.Fatalf("Gemini MCP child env report = %q, want %q", got, want)
	}
}

func TestMCPBridge_FailuresAreTypedForAdmission(t *testing.T) {
	t.Parallel()
	listFail := &fakeMCPClient{listErr: errors.New("list exploded")}
	dialer := dialerFor(t,
		map[string]*fakeMCPClient{"half": listFail},
		map[string]error{"down": errors.New("connection refused")},
	)
	servers := []agent.MCPServerConfig{
		{Name: "down", Type: "http", URL: "http://unused"},
		{Name: "half", Type: "http", URL: "http://unused"},
	}
	b := newMCPBridge(context.Background(), servers, dialer)

	if len(b.clients) != 0 || len(b.failures) != 2 {
		t.Fatalf("want both servers recorded as failed, got clients=%v failures=%v", b.clients, b.failures)
	}
	if !listFail.closed {
		t.Error("a client whose tools/list failed must be closed")
	}
	if err := b.connectionError(); err == nil || !strings.Contains(err.Error(), "down, half") {
		t.Errorf("connectionError = %v, want deterministic failed-server list", err)
	}

	// Calls against failed servers carry the cause.
	_, err := b.call(context.Background(), "mcp__down__anything", nil)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("want connect failure surfaced, got %v", err)
	}
	// Unknown server name.
	if _, err := b.call(context.Background(), "mcp__ghost__t", nil); err == nil {
		t.Error("want error for undeclared server")
	}
	// Unroutable shape.
	if _, err := b.call(context.Background(), "mcp__justserver", nil); err == nil {
		t.Error("want error for catch-all shaped name")
	}
}

func TestMCPBridge_AmendPlan(t *testing.T) {
	t.Parallel()
	fake := &fakeMCPClient{tools: []mcp.ToolDef{{Name: "echo"}, {Name: "asserted"}}}
	servers := []agent.MCPServerConfig{
		{Name: "fake", Type: "http", URL: "http://unused"},
		{Name: "down", Type: "http", URL: "http://unused"},
	}
	b := newMCPBridge(context.Background(), servers,
		dialerFor(t, map[string]*fakeMCPClient{"fake": fake}, map[string]error{"down": errors.New("nope")}))

	spec := agent.Spec{
		Prompt:       "x",
		AllowedTools: []string{"Edit"},
		// Already asserted by the platform — must not be double-declared.
		MCPToolNames: []string{"mcp__fake__asserted"},
		MCPServers:   servers,
	}
	plan, err := buildSpawnPlan(spec, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	b.amendPlan(&plan)

	names := sortedToolNames(plan.tools)
	want := []string{
		"Edit",
		"mcp__down", // failed server keeps its catch-all
		"mcp__fake__asserted",
		"mcp__fake__echo",
	}
	if !equalStrings(names, want) {
		t.Errorf("amended declarations: want %v, got %v", want, names)
	}
}

func TestNewMCPBridge_NoServersIsNil(t *testing.T) {
	t.Parallel()
	if b := newMCPBridge(context.Background(), nil, dialerFor(t, nil, nil)); b != nil {
		t.Errorf("want nil bridge for no servers, got %+v", b)
	}
	var nilBridge *mcpBridge
	nilBridge.Close() // nil-safe
	var plan spawnPlan
	nilBridge.amendPlan(&plan) // nil-safe
}

func TestGeminiParametersFromMCPSchema(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want func(t *testing.T, got map[string]any)
	}{
		{
			name: "empty falls back to permissive default",
			raw:  "",
			want: func(t *testing.T, got map[string]any) {
				if got["type"] != "object" {
					t.Errorf("default type = %v", got["type"])
				}
			},
		},
		{
			name: "invalid JSON falls back",
			raw:  "{nope",
			want: func(t *testing.T, got map[string]any) {
				if got["type"] != "object" {
					t.Errorf("fallback type = %v", got["type"])
				}
			},
		},
		{
			name: "nested foreign keys stripped recursively",
			raw:  `{"type":"object","properties":{"q":{"type":"string","$ref":"#/x"},"n":{"type":"array","items":{"type":"object","oneOf":[{}],"properties":{}}}},"required":["q"],"oneOf":[{}]}`,
			want: func(t *testing.T, got map[string]any) {
				body, _ := json.Marshal(got)
				if strings.Contains(string(body), "$ref") || strings.Contains(string(body), "oneOf") {
					t.Errorf("foreign keys leaked: %s", body)
				}
				props := got["properties"].(map[string]any)
				if props["q"].(map[string]any)["type"] != "string" {
					t.Errorf("nested whitelisted keys lost: %s", body)
				}
				if _, ok := got["required"]; !ok {
					t.Errorf("required dropped: %s", body)
				}
			},
		},
		{
			name: "missing type defaults to object with properties",
			raw:  `{"description":"free-form"}`,
			want: func(t *testing.T, got map[string]any) {
				if got["type"] != "object" {
					t.Errorf("type = %v", got["type"])
				}
				if _, ok := got["properties"]; !ok {
					t.Error("object schema must carry a properties object")
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.want(t, geminiParametersFromMCPSchema(json.RawMessage(tc.raw)))
		})
	}
}

func TestToolExecutor_MCPWithoutBridgeErrors(t *testing.T) {
	t.Parallel()
	e := newToolExecutor("", nil, nil)
	res := e.execute(context.Background(), candidateFuncCall{Name: "mcp__af_linear__get_issue"})
	if !res.isError || !strings.Contains(res.text, "no MCP servers") {
		t.Errorf("want structured no-servers error, got %+v", res)
	}
}

func TestToolExecutor_MCPDomainErrorKeepsServerText(t *testing.T) {
	t.Parallel()
	fake := &fakeMCPClient{
		tools: []mcp.ToolDef{{Name: "boom"}},
		callFunc: func(string, map[string]any) (mcp.ToolResult, error) {
			return mcp.ToolResult{Content: "tool blew up", IsError: true}, nil
		},
	}
	b := newMCPBridge(context.Background(),
		[]agent.MCPServerConfig{{Name: "srv", Type: "http", URL: "http://unused"}},
		dialerFor(t, map[string]*fakeMCPClient{"srv": fake}, nil))
	e := newToolExecutor("", nil, b)

	res := e.execute(context.Background(), candidateFuncCall{Name: "mcp__srv__boom"})
	if !res.isError || res.text != "tool blew up" {
		t.Errorf("want server error text passed through, got %+v", res)
	}
	if msg, _ := res.response["error"].(string); msg != "tool blew up" {
		t.Errorf("functionResponse error = %v", res.response)
	}
}

// failingDialer always errors — used to assert Spawn degrades instead of
// failing when no MCP server is reachable.
func failingDialer(_ context.Context, s agent.MCPServerConfig) (mcp.Client, error) {
	return nil, fmt.Errorf("dial %s: connection refused", s.Name)
}
