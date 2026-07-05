package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRepo writes a tiny Go repo the engine can index and returns its root.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package greet

// Greeter greets people.
type Greeter struct{ Name string }

// GreetUser returns a greeting.
func GreetUser(name string) string { return "Hello, " + name }

// Greet returns a greeting from the Greeter.
func (g *Greeter) Greet() string { return "Hello, " + g.Name }
`
	if err := os.WriteFile(filepath.Join(dir, "greet.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func newTestServer(t *testing.T, root string) *Server {
	t.Helper()
	s, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Warm-up writes .donmai/code-index/ under root asynchronously; wait for it
	// so the TempDir cleanup does not race the index write.
	t.Cleanup(func() { <-s.warmDone })
	return s
}

func TestServer_InvalidRootRejected(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Root: ""}); err == nil {
		t.Fatal("New with empty root should fail")
	}
	if _, err := New(Config{Root: "relative/path"}); err == nil {
		t.Fatal("New with relative root should fail")
	}
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("New with missing root should fail")
	}
	if _, err := New(Config{Root: t.TempDir(), Tools: []string{"af_code_bogus"}}); err == nil {
		t.Fatal("New with unknown tool should fail")
	}
}

func TestCallTool_UnknownToolIsProtocolError(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	_, rerr := s.callTool(context.Background(), "af_code_not_a_tool", nil)
	if rerr == nil {
		t.Fatal("unknown tool should be a JSON-RPC error, got nil")
	}
	if rerr.Code != codeInvalidParams {
		t.Fatalf("unknown tool code = %d, want %d", rerr.Code, codeInvalidParams)
	}
}

func TestCallTool_PanicRecovered(t *testing.T) {
	t.Parallel()
	// White-box: inject a tool whose invoke panics; a panicking tool call must
	// be recovered and returned as an isError result, never crash the process.
	closed := make(chan struct{})
	close(closed)
	boom := &toolDef{
		name:   "boom",
		invoke: func(json.RawMessage) (any, error) { panic("kaboom") },
	}
	s := &Server{
		tools:      []*toolDef{boom},
		toolByName: map[string]*toolDef{"boom": boom},
		warmDone:   closed,
		logf:       func(string, ...any) {},
	}
	res, rerr := s.callTool(context.Background(), "boom", nil)
	if rerr != nil {
		t.Fatalf("panic should surface as an isError result, not a protocol error: %v", rerr)
	}
	if !res.IsError {
		t.Fatal("panicking tool should return isError=true")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "panic") {
		t.Fatalf("isError content should mention the panic, got %+v", res.Content)
	}
}

func TestCallTool_WaitsForWarmupThenCtxCancel(t *testing.T) {
	t.Parallel()
	// warmDone never closes → callTool must block, then return promptly when
	// the context is cancelled (graceful shutdown), not hang forever.
	td := &toolDef{name: "x", invoke: func(json.RawMessage) (any, error) { return map[string]any{}, nil }}
	s := &Server{
		tools:      []*toolDef{td},
		toolByName: map[string]*toolDef{"x": td},
		warmDone:   make(chan struct{}), // never closed
		logf:       func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, rerr := s.callTool(ctx, "x", nil)
		if rerr == nil {
			t.Errorf("cancelled warm-wait should return a protocol error")
		}
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callTool did not return after context cancel (warm-wait hung)")
	}
}

func TestServer_SearchSymbolsInProcess(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	res, rerr := s.callTool(context.Background(), ToolSearchSymbols, json.RawMessage(`{"query":"Greeter"}`))
	if rerr != nil {
		t.Fatalf("search-symbols protocol error: %v", rerr)
	}
	if res.IsError {
		t.Fatalf("search-symbols returned isError: %+v", res.Content)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("want a single text content item, got %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].Text, "Greeter") {
		t.Fatalf("search-symbols result should mention Greeter, got: %s", res.Content[0].Text)
	}
}

func TestServer_ToolsListRespectsSubset(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Root: fixtureRepo(t), Tools: []string{ToolSearchSymbols, ToolGetRepoMap}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { <-s.warmDone })
	list := s.handleToolsList()
	if len(list.Tools) != 2 {
		t.Fatalf("tools/list len = %d, want 2", len(list.Tools))
	}
	// A disabled tool is not callable.
	if _, rerr := s.callTool(context.Background(), ToolSearchCode, nil); rerr == nil {
		t.Fatal("disabled tool should not be callable")
	}
}

func TestServer_CheckDuplicateRefusesTraversalContentFile(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	res, rerr := s.callTool(context.Background(), ToolCheckDuplicate,
		json.RawMessage(`{"contentFile":"../../../../etc/passwd"}`))
	if rerr != nil {
		// A refused traversal may surface either as a protocol error or an
		// isError result; both are acceptable so long as it does not read the
		// file. A protocol error is fine here.
		return
	}
	if !res.IsError {
		t.Fatal("traversal contentFile must be refused (isError), not served")
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("refusal should explain the escape, got %+v", res.Content)
	}
}
