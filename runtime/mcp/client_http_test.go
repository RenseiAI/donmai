package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fakeHTTPServer is a minimal Streamable HTTP MCP server for tests. It
// records the methods it saw, asserts protocol headers, assigns a session
// id on initialize, and serves a one-tool surface ("echo").
type fakeHTTPServer struct {
	t   *testing.T
	mu  sync.Mutex
	sse bool // respond with text/event-stream framing instead of JSON

	methods  []string
	deletes  int
	pageSize int // when >0, tools/list paginates with this page size
	tools    []map[string]any
}

func (f *fakeHTTPServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *fakeHTTPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			f.mu.Lock()
			f.deletes++
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			f.t.Errorf("Authorization header = %q, want bearer", got)
		}
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "text/event-stream") {
			f.t.Errorf("Accept header missing event-stream: %q", accept)
		}

		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.mu.Unlock()

		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method != "initialize" && r.Header.Get(sessionIDHeader) != "sess-42" {
			f.t.Errorf("%s: missing session id header", req.Method)
		}

		var result any
		switch req.Method {
		case "initialize":
			var p initializeParams
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" || p.ClientInfo.Name != "donmai" {
				f.t.Errorf("initialize params malformed: %+v", p)
			}
			w.Header().Set(sessionIDHeader, "sess-42")
			result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "0"}}
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = f.listPage(p.Cursor)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "tool blew up"}}, "isError": true}
				break
			}
			text, _ := p.Arguments["text"].(string)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo: " + text}}}
		default:
			writeRPC(w, f.sse, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		writeRPC(w, f.sse, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

func (f *fakeHTTPServer) listPage(cursor string) map[string]any {
	tools := f.tools
	if tools == nil {
		tools = []map[string]any{{
			"name":        "echo",
			"description": "echoes text",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		}}
	}
	if f.pageSize <= 0 || f.pageSize >= len(tools) {
		return map[string]any{"tools": tools}
	}
	start := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "page-%d", &start)
	}
	end := start + f.pageSize
	page := map[string]any{"tools": tools[start:min(end, len(tools))]}
	if end < len(tools) {
		page["nextCursor"] = fmt.Sprintf("page-%d", end)
	}
	return page
}

func writeRPC(w http.ResponseWriter, sse bool, msg map[string]any) {
	body, _ := json.Marshal(msg)
	if !sse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// Prefix with an unrelated notification event to prove the reader
	// skips non-matching messages on the stream.
	_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
}

func dialFake(t *testing.T, f *fakeHTTPServer) (Client, *fakeHTTPServer) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := Dial(context.Background(), agent.MCPServerConfig{
		Name:    "fake",
		Type:    "http",
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, f
}

func TestHTTPClient_HandshakeListCall(t *testing.T) {
	t.Parallel()
	c, f := dialFake(t, &fakeHTTPServer{})

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" || tools[0].Description != "echoes text" {
		t.Fatalf("tools = %+v, want [echo]", tools)
	}
	if len(tools[0].InputSchema) == 0 {
		t.Error("InputSchema: want non-empty raw schema")
	}

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.Content != "echo: hi" {
		t.Errorf("CallTool = %+v, want echo: hi", res)
	}

	// Domain error: isError=true result, NOT a Go error.
	res, err = c.CallTool(context.Background(), "boom", nil)
	if err != nil {
		t.Fatalf("CallTool(boom): %v", err)
	}
	if !res.IsError || res.Content != "tool blew up" {
		t.Errorf("CallTool(boom) = %+v, want isError", res)
	}

	seen := f.seen()
	if len(seen) < 2 || seen[0] != "initialize" || seen[1] != "notifications/initialized" {
		t.Errorf("handshake order = %v", seen)
	}
}

func TestHTTPClient_SSEResponses(t *testing.T) {
	t.Parallel()
	c, _ := dialFake(t, &fakeHTTPServer{sse: true})

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "sse"})
	if err != nil {
		t.Fatalf("CallTool over SSE: %v", err)
	}
	if res.Content != "echo: sse" {
		t.Errorf("CallTool over SSE = %+v", res)
	}
}

func TestHTTPClient_ListToolsPaginates(t *testing.T) {
	t.Parallel()
	tools := []map[string]any{
		{"name": "a"}, {"name": "b"}, {"name": "c"},
	}
	c, _ := dialFake(t, &fakeHTTPServer{pageSize: 2, tools: tools})

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, len(got))
	for i, td := range got {
		names[i] = td.Name
	}
	if len(names) != 3 || names[0] != "a" || names[2] != "c" {
		t.Errorf("paginated tools = %v, want [a b c]", names)
	}
}

func TestHTTPClient_CloseSendsDelete(t *testing.T) {
	t.Parallel()
	c, f := dialFake(t, &fakeHTTPServer{})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = c.Close() // idempotent
	f.mu.Lock()
	deletes := f.deletes
	f.mu.Unlock()
	if deletes != 1 {
		t.Errorf("session DELETEs = %d, want 1", deletes)
	}
}

func TestHTTPClient_RPCErrorSurfacesAsError(t *testing.T) {
	t.Parallel()
	c, _ := dialFake(t, &fakeHTTPServer{})
	// The fake answers unknown methods with -32601; reach one through the
	// session layer by calling a tool on a conn whose tools/call we hijack.
	conn := c.(*httpMCPClient).conn
	if _, err := conn.call(context.Background(), "bogus/method", struct{}{}); err == nil || !strings.Contains(err.Error(), "-32601") {
		t.Errorf("want rpc -32601 error, got %v", err)
	}
}

func TestDial_UnknownTypeRejected(t *testing.T) {
	t.Parallel()
	if _, err := Dial(context.Background(), agent.MCPServerConfig{Name: "x", Type: "carrier-pigeon"}); err == nil {
		t.Fatal("want error for unknown transport type")
	}
}

func TestRenderContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		res  callToolResult
		want string
	}{
		{"text blocks joined", callToolResult{Content: []contentBlock{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}}, "a\nb"},
		{"non-text placeholder", callToolResult{Content: []contentBlock{{Type: "image"}}}, "[image content]"},
		{"structured fallback", callToolResult{StructuredContent: json.RawMessage(`{"k":1}`)}, `{"k":1}`},
		{"empty", callToolResult{}, ""},
	}
	for _, tc := range cases {
		if got := renderContent(tc.res); got != tc.want {
			t.Errorf("%s: renderContent = %q, want %q", tc.name, got, tc.want)
		}
	}
}
