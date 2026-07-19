package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestMain doubles the test binary as a fake MCP stdio server: when
// re-executed with MCP_FAKE_STDIO_SERVER=1 it speaks newline-delimited
// JSON-RPC on stdin/stdout instead of running tests. This exercises the
// real subprocess dial path without external fixtures.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_STDIO_SERVER") == "1" {
		runFakeStdioServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeStdioServer() {
	if report := os.Getenv("MCP_FAKE_ENV_REPORT"); report != "" {
		status := "clean"
		if _, ok := os.LookupEnv("ATTACH_TOKEN"); ok {
			status = "leaked"
		}
		if _, ok := os.LookupEnv("ATTACH_TOKEN_FILE"); ok {
			status = "leaked"
		}
		if _, ok := os.LookupEnv("ATTACH_URL"); ok {
			status = "leaked"
		}
		if os.Getenv("MCP_SAFE_ENV") != "present" {
			status = "missing-safe-env"
		}
		_ = os.WriteFile(report, []byte(status), 0o600) //nolint:gosec // test re-exec writes its caller-owned temp report
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	respond := func(id int64, result any, rpcErr map[string]any) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = result
		}
		_ = enc.Encode(msg)
	}

	for sc.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue // notification or noise
		}
		switch req.Method {
		case "initialize":
			// Interleave a server-initiated notification before the
			// response to prove the demux skips it.
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/log", "params": map[string]any{"level": "info"}})
			respond(*req.ID, map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake-stdio", "version": "0"},
			}, nil)
		case "tools/list":
			respond(*req.ID, map[string]any{"tools": []map[string]any{{
				"name":        "shout",
				"description": "upper-cases text",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
			}}}, nil)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := p.Arguments["text"].(string)
			respond(*req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": strings.ToUpper(text)}}}, nil)
		default:
			respond(*req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
		}
	}
}

func TestStdioClient_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Dial(ctx, agent.MCPServerConfig{
		Name:    "fake-stdio",
		Command: os.Args[0],
		Env:     map[string]string{"MCP_FAKE_STDIO_SERVER": "1"},
	})
	if err != nil {
		t.Fatalf("Dial(stdio): %v", err)
	}
	defer func() { _ = c.Close() }()

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "shout" {
		t.Fatalf("tools = %+v, want [shout]", tools)
	}

	res, err := c.CallTool(ctx, "shout", map[string]any{"text": "quiet"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.Content != "QUIET" {
		t.Errorf("CallTool = %+v, want QUIET", res)
	}

	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	_ = c.Close() // idempotent
}

func TestStdioClient_ChildEnvSanitized(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	report := t.TempDir() + "/env-report.txt"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, agent.MCPServerConfig{
		Name:    "fake-stdio-env",
		Command: os.Args[0],
		Env: map[string]string{
			"MCP_FAKE_STDIO_SERVER": "1",
			"MCP_FAKE_ENV_REPORT":   report,
			"MCP_SAFE_ENV":          "present",
			"ATTACH_TOKEN":          "explicit-secret",
			"ATTACH_TOKEN_FILE":     "/explicit/token",
			"ATTACH_URL":            "wss://explicit.invalid/v1/rooms/room-1",
		},
	})
	if err != nil {
		t.Fatalf("Dial(stdio): %v", err)
	}
	defer func() { _ = c.Close() }()

	body, err := os.ReadFile(report) //nolint:gosec // report is a test-owned temp file
	if err != nil {
		t.Fatalf("read env report: %v", err)
	}
	if got, want := string(body), "clean"; got != want {
		t.Fatalf("MCP child env report = %q, want %q", got, want)
	}
}

func TestStdioClient_MissingCommandRejected(t *testing.T) {
	t.Parallel()
	if _, err := Dial(context.Background(), agent.MCPServerConfig{Name: "x", Type: "stdio"}); err == nil {
		t.Fatal("want error for empty Command")
	}
}

func TestStdioClient_ServerDiesMidCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A "server" that exits immediately: the handshake call must fail with
	// a closed-stream error rather than hanging.
	_, err := Dial(ctx, agent.MCPServerConfig{
		Name:    "dead",
		Command: "true",
	})
	if err == nil {
		t.Fatal("want handshake failure against an immediately-exiting server")
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("error should identify the failing phase: %v", err)
	}
}
