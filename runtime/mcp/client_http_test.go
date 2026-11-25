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
	"time"

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

// hangServer is a Streamable HTTP MCP server that answers the initialize
// handshake (assigning a session id) and then never answers the hung method:
// the handler blocks until the client's request context ends. hangDelete
// additionally hangs the session DELETE so Close's bound is exercised.
type hangServer struct {
	hungMethod string
	hangDelete bool
}

func (h hangServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if h.hangDelete {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == h.hungMethod {
			<-r.Context().Done()
			return
		}
		if req.Method == "initialize" {
			w.Header().Set(sessionIDHeader, "sess-hung")
		}
		writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
	}
}

// TestHTTPClient_TimeoutsAgainstNeverAnsweringServer pins the transport
// bound: every client call runs under an explicit client timeout, and Close
// runs under its own bound, so a server that never answers cannot stall the
// CLI. Each case dials (except the handshake case, where the dial itself
// hangs) and drives one operation against a server that hangs exactly that
// operation.
func TestHTTPClient_TimeoutsAgainstNeverAnsweringServer(t *testing.T) {
	t.Parallel()

	const clientBound = 150 * time.Millisecond
	const closeBound = 150 * time.Millisecond

	tests := []struct {
		name string
		// hungMethod is the JSON-RPC method the server never answers.
		// "initialize" hangs the Dial handshake itself.
		hungMethod string
		hangDelete bool
		op         string // "dial" | "list" | "call" | "close"
	}{
		{name: "dial handshake", hungMethod: "initialize", op: "dial"},
		{name: "tools/list", hungMethod: "tools/list", op: "list"},
		{name: "tools/call", hungMethod: "tools/call", op: "call"},
		{name: "close delete", hangDelete: true, op: "close"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(hangServer{hungMethod: tc.hungMethod, hangDelete: tc.hangDelete}.handler())
			t.Cleanup(srv.Close)
			cfg := agent.MCPServerConfig{Name: "hung", Type: "http", URL: srv.URL}
			dial := func() (Client, error) {
				return dialHTTPTimeout(context.Background(), cfg, clientBound, closeBound)
			}

			if tc.op == "dial" {
				start := time.Now()
				if _, err := dial(); err == nil {
					t.Fatal("Dial against a never-answering server succeeded; want a timeout error")
				}
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("Dial took %v against a never-answering server; want the explicit client timeout", elapsed)
				}
				return
			}

			c, err := dial()
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })

			switch tc.op {
			case "list":
				start := time.Now()
				if _, err := c.ListTools(context.Background()); err == nil {
					t.Fatal("ListTools against a never-answering server succeeded; want a timeout error")
				}
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("ListTools took %v; want the explicit client timeout", elapsed)
				}
			case "call":
				start := time.Now()
				if _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
					t.Fatal("CallTool against a never-answering server succeeded; want a timeout error")
				}
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("CallTool took %v; want the explicit client timeout", elapsed)
				}
			case "close":
				start := time.Now()
				if err := c.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("Close took %v against a never-answering DELETE; want the explicit close bound", elapsed)
				}
			}
		})
	}
}

// slowServer answers initialize at once but tools/call after a delay: the
// shape of a healthy long tool (a test run that takes seconds) under a
// harness call bound far longer than the transport's own ceiling.
type slowServer struct {
	callDelay time.Duration
}

func (s slowServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "initialize" {
			w.Header().Set(sessionIDHeader, "sess-slow")
			writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
			return
		}
		if req.Method == "tools/call" {
			select {
			case <-time.After(s.callDelay):
			case <-r.Context().Done():
				return
			}
			writeRPC(w, false, map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "slow ok"}}},
			})
			return
		}
		writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
	}
}

// TestHTTPClient_CallWithCallerDeadlineOutlivesClientBound pins the B2 fix:
// a tools/call carrying a caller deadline longer than the transport ceiling
// (the Gemini bridge's 5-minute tool bound over a 30s transport) runs under
// the caller's deadline. A slow-but-healthy tool must complete; only a call
// with NO deadline falls back to the transport bound.
func TestHTTPClient_CallWithCallerDeadlineOutlivesClientBound(t *testing.T) {
	t.Parallel()

	const clientBound = 150 * time.Millisecond
	const callDelay = 500 * time.Millisecond // past the transport ceiling, inside the caller deadline

	newSlowClient := func(t *testing.T) Client {
		t.Helper()
		srv := httptest.NewServer(slowServer{callDelay: callDelay}.handler())
		t.Cleanup(srv.Close)
		c, err := dialHTTPTimeout(context.Background(),
			agent.MCPServerConfig{Name: "slow", Type: "http", URL: srv.URL}, clientBound, clientBound)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	t.Run("caller deadline wins over the shorter transport ceiling", func(t *testing.T) {
		t.Parallel()
		c := newSlowClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := c.CallTool(ctx, "echo", nil)
		if err != nil {
			t.Fatalf("CallTool with a 5s caller deadline failed past the 150ms transport ceiling: %v", err)
		}
		if res.Content != "slow ok" {
			t.Fatalf("CallTool = %q; want %q", res.Content, "slow ok")
		}
	})

	t.Run("a call without a deadline still takes the transport bound", func(t *testing.T) {
		t.Parallel()
		c := newSlowClient(t)
		start := time.Now()
		if _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
			t.Fatal("CallTool without a deadline succeeded past the transport ceiling; want a timeout error")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("CallTool took %v without a deadline; want the explicit client timeout", elapsed)
		}
	})
}

// slowListServer answers initialize at once but tools/list after a delay: the
// shape of a healthy-but-slow discovery surface under a harness call bound.
type slowListServer struct {
	listDelay time.Duration
}

func (s slowListServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "initialize" {
			w.Header().Set(sessionIDHeader, "sess-slow-list")
			writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
			return
		}
		if req.Method == "tools/list" {
			select {
			case <-time.After(s.listDelay):
			case <-r.Context().Done():
				return
			}
			writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{"tools": []any{}}})
			return
		}
		writeRPC(w, false, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
	}
}

// TestHTTPClient_CancelOnlySlowCallCompletes pins the library-caller fix: a
// tools/call with a cancel-only context (cancellable, no deadline) rides the
// caller's cancellation with no extra transport ceiling, so a healthy slow
// tool completes as it did before the bound existed. Only a Background
// context — nothing to cancel and no deadline — takes the fallback bound.
// Drives the production transport (CallTool over real HTTP).
func TestHTTPClient_CancelOnlySlowCallCompletes(t *testing.T) {
	t.Parallel()
	const clientBound = 150 * time.Millisecond
	const callDelay = 500 * time.Millisecond
	srv := httptest.NewServer(slowServer{callDelay: callDelay}.handler())
	t.Cleanup(srv.Close)
	c, err := dialHTTPTimeout(context.Background(),
		agent.MCPServerConfig{Name: "slow", Type: "http", URL: srv.URL}, clientBound, clientBound)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := c.CallTool(ctx, "echo", nil)
	if err != nil {
		t.Fatalf("CallTool with cancel-only context failed past the %v fallback: %v", clientBound, err)
	}
	if res.Content != "slow ok" {
		t.Fatalf("CallTool = %q; want %q", res.Content, "slow ok")
	}
}

// TestHTTPClient_CallerDeadlineOverridesDiscovery pins the discovery half of
// the override: a tools/list carrying a caller deadline longer than the
// transport ceiling runs under that deadline, while the same call on a
// Background context takes the fallback bound. Drives ListTools over real HTTP.
func TestHTTPClient_CallerDeadlineOverridesDiscovery(t *testing.T) {
	t.Parallel()
	const clientBound = 150 * time.Millisecond
	const listDelay = 500 * time.Millisecond
	newListClient := func(t *testing.T) Client {
		t.Helper()
		srv := httptest.NewServer(slowListServer{listDelay: listDelay}.handler())
		t.Cleanup(srv.Close)
		c, err := dialHTTPTimeout(context.Background(),
			agent.MCPServerConfig{Name: "slow-list", Type: "http", URL: srv.URL}, clientBound, clientBound)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	t.Run("deadline wins", func(t *testing.T) {
		t.Parallel()
		c := newListClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.ListTools(ctx); err != nil {
			t.Fatalf("ListTools with a 5s caller deadline failed past the 150ms ceiling: %v", err)
		}
	})
	t.Run("background takes the fallback", func(t *testing.T) {
		t.Parallel()
		c := newListClient(t)
		start := time.Now()
		if _, err := c.ListTools(context.Background()); err == nil {
			t.Fatal("ListTools on Background succeeded past the ceiling; want a timeout error")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("ListTools took %v; want the explicit client timeout", elapsed)
		}
	})
}

// TestHTTPClient_PerServerTimeoutsTuneFallbackAndClose pins the explicit
// declaration: RequestTimeoutSeconds tunes the Background fallback and
// CloseTimeoutSeconds tunes teardown, both through the production dial path
// (dialHTTP reading MCPServerConfig). A hanging server honours the
// per-server bounds instead of the package defaults.
func TestHTTPClient_PerServerTimeoutsTuneFallbackAndClose(t *testing.T) {
	t.Parallel()
	t.Run("request bound", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(hangServer{hungMethod: "tools/list"}.handler())
		t.Cleanup(srv.Close)
		c, err := dialHTTP(context.Background(), agent.MCPServerConfig{
			Name: "hung", Type: "http", URL: srv.URL, RequestTimeoutSeconds: 1,
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		start := time.Now()
		if _, err := c.ListTools(context.Background()); err == nil {
			t.Fatal("ListTools against a never-answering server succeeded; want a timeout error")
		}
		elapsed := time.Since(start)
		if elapsed > 10*time.Second || elapsed < 900*time.Millisecond {
			t.Fatalf("ListTools took %v; want ~1s per-server bound, not the 30s default", elapsed)
		}
	})
	t.Run("close bound", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(hangServer{hangDelete: true}.handler())
		t.Cleanup(srv.Close)
		c, err := dialHTTP(context.Background(), agent.MCPServerConfig{
			Name: "hung-close", Type: "http", URL: srv.URL, CloseTimeoutSeconds: 1,
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		start := time.Now()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second || elapsed < 900*time.Millisecond {
			t.Fatalf("Close took %v against a never-answering DELETE; want ~1s per-server bound", elapsed)
		}
	})
	t.Run("close caller deadline overrides", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(hangServer{hangDelete: true}.handler())
		t.Cleanup(srv.Close)
		c, err := dialHTTPTimeout(context.Background(),
			agent.MCPServerConfig{Name: "hung-close-override", Type: "http", URL: srv.URL}, 150*time.Millisecond, 30*time.Second)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		hc, ok := c.(*httpMCPClient)
		if !ok {
			t.Fatalf("client is %T; want *httpMCPClient", c)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		if err := hc.CloseWithContext(ctx); err != nil {
			t.Fatalf("CloseWithContext: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second || elapsed < 900*time.Millisecond {
			t.Fatalf("CloseWithContext took %v; want the ~1s caller deadline, not the 30s per-client bound", elapsed)
		}
	})
}
