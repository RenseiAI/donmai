package codeintelhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// newGitFixtureRepo writes and commits a small multi-file Go repo the code
// engine can meaningfully index (symbols/search/duplicate/type-usages all
// have real hits), mirroring runtime/mcp/server/conformance_test.go's
// fixtureRepo but as an actual git repository GitFactory can mirror/checkout.
func newGitFixtureRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
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
	runGitT(t, dir, "add", ".")
	runGitT(t, dir, "commit", "-q", "-m", "fixture")
	sha = runGitT(t, dir, "rev-parse", "HEAD")
	return dir, sha
}

// testHandlerFixture bundles a running Handler behind httptest, its backing
// Pool (so tests can inspect/lease directly), and the fixed now used to sign
// tokens.
type testHandlerFixture struct {
	server *httptest.Server
	pool   *Pool
	now    time.Time
}

func newTestHandlerFixture(t *testing.T, factory Factory, poolCfg PoolConfig, handlerCfg HandlerConfig) *testHandlerFixture {
	t.Helper()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	pool, err := NewPool(factory, poolCfg)
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pool.Close(ctx)
	})
	verifier := newTestVerifier(t, func() time.Time { return now })
	handlerCfg.Verifier = verifier
	handlerCfg.Pool = pool
	if handlerCfg.MaxConcurrentCalls == 0 {
		handlerCfg.MaxConcurrentCalls = 4
	}
	h, err := NewHandler(handlerCfg)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	return &testHandlerFixture{server: ts, pool: pool, now: now}
}

func (f *testHandlerFixture) sign(t *testing.T, c testClaims) string {
	t.Helper()
	return signToken(t, "HS256", testSecret, c)
}

func (f *testHandlerFixture) claimsFor(binding Binding, invocationID string) testClaims {
	return testClaims{
		Sub:     invocationID,
		Iss:     testIssuer,
		Aud:     testAudience,
		Exp:     ptr(f.now.Add(time.Hour).Unix()),
		Iat:     ptr(f.now.Add(-time.Minute).Unix()),
		Org:     binding.OrgID,
		Proj:    binding.ProjectID,
		Repo:    binding.RepositoryPathID,
		RevKind: string(binding.RevisionKind),
		Rev:     binding.Revision,
	}
}

func (f *testHandlerFixture) post(t *testing.T, token string, body []byte) (*http.Response, mcpserver.ToolResult, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/tools/call", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var result mcpserver.ToolResult
	if err := json.Unmarshal(raw.Bytes(), &result); err != nil {
		t.Fatalf("response body is not a ToolResult: %v\nbody: %s", err, raw.String())
	}
	return resp, result, raw.Bytes()
}

func marshalCallRequest(t *testing.T, req callRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal callRequest: %v", err)
	}
	return b
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

func gitBackedFixture(t *testing.T) (*testHandlerFixture, Binding) {
	t.Helper()
	dir, sha := newGitFixtureRepo(t)
	cat, err := NewCatalog([]CatalogRepository{{ID: "repo-1", ProjectID: "proj-1", Source: dir}})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	factory := &GitFactory{Catalog: cat, StateDir: t.TempDir()}
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 4}, HandlerConfig{})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: sha,
	}
	return f, binding
}

func TestHandlerToolsCallSuccess(t *testing.T) {
	t.Parallel()
	f, binding := gitBackedFixture(t)
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))
	body := marshalCallRequest(t, callRequest{
		Tool: mcpserver.ToolGetRepoMap, Arguments: json.RawMessage(`{}`),
		InvocationID: "invocation-1", Binding: binding,
	})

	resp, result, _ := f.post(t, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("result.Content = %+v, want one text item", result.Content)
	}
	requireObjectFields(t, "af_code_get_repo_map", result.Content[0].Text, "entries", "rootHash", "files")
}

// TestHandlerSixToolConformance drives every one of the six frozen tools
// through the real HTTP path against a real Git-backed workarea and the real
// MCP engine, checking the same field-shape contract
// runtime/mcp/server/conformance_test.go pins for the stdio transport.
func TestHandlerSixToolConformance(t *testing.T) {
	t.Parallel()
	f, binding := gitBackedFixture(t)
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))

	cases := []struct {
		name      string
		tool      string
		arguments map[string]any
		check     func(t *testing.T, content string)
	}{
		{
			name: "get-repo-map", tool: mcpserver.ToolGetRepoMap, arguments: map[string]any{},
			check: func(t *testing.T, content string) {
				requireObjectFields(t, mcpserver.ToolGetRepoMap, content, "entries", "rootHash", "files")
			},
		},
		{
			name: "search-symbols", tool: mcpserver.ToolSearchSymbols, arguments: map[string]any{"query": "Greeter"},
			check: func(t *testing.T, content string) { requireSearchResult(t, mcpserver.ToolSearchSymbols, content) },
		},
		{
			name: "search-code", tool: mcpserver.ToolSearchCode, arguments: map[string]any{"query": "Greet"},
			check: func(t *testing.T, content string) { requireSearchResult(t, mcpserver.ToolSearchCode, content) },
		},
		{
			name: "check-duplicate", tool: mcpserver.ToolCheckDuplicate, arguments: map[string]any{"content": "func NotInFixture() {}"},
			check: func(t *testing.T, content string) {
				requireObjectFields(t, mcpserver.ToolCheckDuplicate, content, "isDuplicate", "matchType", "existingId", "hammingDistance")
			},
		},
		{
			name: "find-type-usages", tool: mcpserver.ToolFindTypeUsages, arguments: map[string]any{"typeName": "Greeter"},
			check: func(t *testing.T, content string) {
				requireObjectFields(t, mcpserver.ToolFindTypeUsages, content, "typeName", "totalUsages", "usages", "switchStatements", "mappingObjects")
			},
		},
		{
			name: "validate-cross-deps", tool: mcpserver.ToolValidateCrossDeps, arguments: map[string]any{},
			check: func(t *testing.T, content string) {
				requireObjectFields(t, mcpserver.ToolValidateCrossDeps, content, "valid", "missingDeps", "packagesChecked", "filesChecked")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(tc.arguments)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			body := marshalCallRequest(t, callRequest{
				Tool: tc.tool, Arguments: args, InvocationID: "invocation-1", Binding: binding,
			})
			resp, result, _ := f.post(t, token, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if result.IsError {
				t.Fatalf("result.IsError = true: %+v", result)
			}
			if len(result.Content) != 1 {
				t.Fatalf("result.Content = %+v, want one item", result.Content)
			}
			tc.check(t, result.Content[0].Text)
		})
	}
}

func TestHandlerUnknownToolSemanticError(t *testing.T) {
	t.Parallel()
	f, binding := gitBackedFixture(t)
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))
	body := marshalCallRequest(t, callRequest{
		Tool: "af_code_not_a_real_tool", Arguments: json.RawMessage(`{}`),
		InvocationID: "invocation-1", Binding: binding,
	})

	resp, result, _ := f.post(t, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (semantic tool error)", resp.StatusCode)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for an unknown tool name")
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "unknown") {
		t.Errorf("result.Content = %+v, want text mentioning the unknown tool", result.Content)
	}
}

func simpleFixture(t *testing.T) (*testHandlerFixture, Binding) {
	t.Helper()
	factory := newFakeFactory()
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 4}, HandlerConfig{})
	return f, validBinding()
}

func TestHandlerAuthRefusal(t *testing.T) {
	t.Parallel()
	f, binding := simpleFixture(t)
	body := marshalCallRequest(t, callRequest{
		Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`),
		InvocationID: "invocation-1", Binding: binding,
	})

	cases := []struct {
		name  string
		token string
	}{
		{"missing bearer token", ""},
		{"malformed signature", f.sign(t, testClaims{Sub: "invocation-1", Iss: "wrong-issuer", Aud: testAudience, Exp: ptr(f.now.Add(time.Hour).Unix())})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, result, _ := f.post(t, tc.token, body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if !result.IsError {
				t.Error("result.IsError = false, want true on an auth refusal")
			}
		})
	}
}

func TestHandlerMalformedBearerScheme(t *testing.T) {
	t.Parallel()
	f, binding := simpleFixture(t)
	body := marshalCallRequest(t, callRequest{
		Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`),
		InvocationID: "invocation-1", Binding: binding,
	})
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/tools/call", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a non-Bearer scheme", resp.StatusCode)
	}
}

func TestHandlerBindingMismatchRefusal(t *testing.T) {
	t.Parallel()
	f, binding := simpleFixture(t)

	t.Run("subject mismatch", func(t *testing.T) {
		token := f.sign(t, f.claimsFor(binding, "some-other-invocation"))
		body := marshalCallRequest(t, callRequest{
			Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`),
			InvocationID: "invocation-1", Binding: binding,
		})
		resp, result, _ := f.post(t, token, body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		if !result.IsError {
			t.Error("result.IsError = false, want true")
		}
	})

	t.Run("claims binding does not match body binding", func(t *testing.T) {
		token := f.sign(t, f.claimsFor(binding, "invocation-1"))
		mismatched := binding
		mismatched.Revision = fullObjectID("some-other-revision")
		body := marshalCallRequest(t, callRequest{
			Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`),
			InvocationID: "invocation-1", Binding: mismatched,
		})
		resp, result, _ := f.post(t, token, body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		if !result.IsError {
			t.Error("result.IsError = false, want true")
		}
	})
}

func TestHandlerMalformedRequest(t *testing.T) {
	t.Parallel()
	f, binding := simpleFixture(t)
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))

	cases := []struct {
		name string
		body []byte
	}{
		{"invalid JSON", []byte(`{not json`)},
		{"unknown field", []byte(`{"tool":"af_code_get_repo_map","arguments":{},"invocationId":"invocation-1","binding":{},"extra":"nope"}`)},
		{"trailing content", append(marshalCallRequest(t, callRequest{
			Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1", Binding: binding,
		}), []byte(`{}`)...)},
		{"missing tool", marshalCallRequest(t, callRequest{
			Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1", Binding: binding,
		})},
		{"missing invocationId", marshalCallRequest(t, callRequest{
			Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`), Binding: binding,
		})},
		{"invalid binding", marshalCallRequest(t, callRequest{
			Tool: "af_code_get_repo_map", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, result, _ := f.post(t, token, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if !result.IsError {
				t.Error("result.IsError = false, want true")
			}
		})
	}
}

func TestHandlerBusyAtMaxConcurrency(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	factory.delay = 200 * time.Millisecond
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 4}, HandlerConfig{MaxConcurrentCalls: 1})
	binding := validBinding()
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))
	body := marshalCallRequest(t, callRequest{
		Tool: "some-tool", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1", Binding: binding,
	})

	type result struct {
		status int
		result mcpserver.ToolResult
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, r, _ := f.post(t, token, body)
			results <- result{resp.StatusCode, r}
		}()
	}
	var busyCount int
	for i := 0; i < 2; i++ {
		r := <-results
		if r.status != http.StatusOK {
			t.Errorf("status = %d, want 200", r.status)
		}
		if r.result.IsError && len(r.result.Content) == 1 && r.result.Content[0].Text == "code_intel_host_busy" {
			busyCount++
		}
	}
	if busyCount != 1 {
		t.Errorf("busy responses = %d, want exactly 1 of the 2 concurrent calls under MaxConcurrentCalls=1", busyCount)
	}
}

func TestHandlerPoolAtCapacityBackpressure(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 1}, HandlerConfig{})

	held := bindingWithRevision("rev-held")
	lease, err := f.pool.Acquire(context.Background(), held)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()

	other := bindingWithRevision("rev-other")
	token := f.sign(t, f.claimsFor(other, "invocation-1"))
	body := marshalCallRequest(t, callRequest{
		Tool: "some-tool", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1", Binding: other,
	})
	resp, result, _ := f.post(t, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (semantic capacity error)", resp.StatusCode)
	}
	if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "code_intel_host_busy" {
		t.Errorf("result = %+v, want isError busy result for ErrAtCapacity", result)
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()
	f, _ := simpleFixture(t)
	resp, err := http.Get(f.server.URL + "/v1/tools/call")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestHandlerRequestSizeLimit(t *testing.T) {
	t.Parallel()
	factory := newFakeFactory()
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 4}, HandlerConfig{MaxBodyBytes: 64})
	binding := validBinding()
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))
	oversized := marshalCallRequest(t, callRequest{
		Tool: "some-tool", Arguments: json.RawMessage(`{"padding":"` + strings.Repeat("x", 4096) + `"}`),
		InvocationID: "invocation-1", Binding: binding,
	})

	resp, result, _ := f.post(t, token, oversized)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an over-limit body", resp.StatusCode)
	}
	if !result.IsError {
		t.Error("result.IsError = false, want true")
	}
}

func TestHandlerHealthzReadyz(t *testing.T) {
	t.Parallel()
	f, _ := simpleFixture(t)

	resp, err := http.Get(f.server.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(f.server.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz status = %d, want 200 while the pool is open", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.pool.Close(ctx); err != nil {
		t.Fatalf("pool.Close() error = %v", err)
	}

	resp, err = http.Get(f.server.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 503 while the pool is draining/closed", resp.StatusCode)
	}
}

// blockingToolCaller is a ToolCaller whose Call blocks until unblock is
// closed, and signals its first entry via entered — letting a test wait
// deterministically for the underlying (unabortable) call to actually start
// before asserting on Handler's soft-timeout behavior around it.
type blockingToolCaller struct {
	entered   chan struct{}
	enterOnce sync.Once
	unblock   chan struct{}
}

func newBlockingToolCaller() *blockingToolCaller {
	return &blockingToolCaller{entered: make(chan struct{}), unblock: make(chan struct{})}
}

func (c *blockingToolCaller) Call(context.Context, string, json.RawMessage) (mcpserver.ToolResult, error) {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.unblock
	return mcpserver.ToolResult{Content: []mcpserver.ContentItem{{Type: "text", Text: "done"}}}, nil
}

func (c *blockingToolCaller) WaitReady(context.Context) error { return nil }

// TestHandlerSoftTimeoutKeepsLeaseAndSlotUntilCallFinishes exercises the
// Task 5 soft-timeout contract end to end against a real HTTP round trip: a
// request whose RequestTimeout fires while the (unabortable) native-engine
// call is still in flight must get a prompt, stable, non-sensitive timeout
// result — but the leased workarea and the global admission slot must
// remain held (protected from eviction/backpressure and still counted
// against MaxConcurrentCalls) until the blocked call actually finishes, at
// which point admission recovers.
func TestHandlerSoftTimeoutKeepsLeaseAndSlotUntilCallFinishes(t *testing.T) {
	t.Parallel()
	caller := newBlockingToolCaller()
	factory := newFakeFactory()
	factory.caller = caller
	f := newTestHandlerFixture(t, factory, PoolConfig{MaxWorkareas: 1},
		HandlerConfig{MaxConcurrentCalls: 1, RequestTimeout: 30 * time.Millisecond})
	binding := validBinding()
	token := f.sign(t, f.claimsFor(binding, "invocation-1"))
	body := marshalCallRequest(t, callRequest{
		Tool: "some-tool", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-1", Binding: binding,
	})

	resp, result, _ := f.post(t, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (semantic timeout error)", resp.StatusCode)
	}
	if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "code_intel_host_call_timeout" {
		t.Fatalf("result = %+v, want isError code_intel_host_call_timeout", result)
	}

	select {
	case <-caller.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("underlying Call never started")
	}

	// The admission slot must still be busy: a second concurrent call must
	// be refused by the semaphore, proving the timed-out request's slot was
	// not released early.
	otherToken := f.sign(t, f.claimsFor(binding, "invocation-2"))
	otherBody := marshalCallRequest(t, callRequest{
		Tool: "some-tool", Arguments: json.RawMessage(`{}`), InvocationID: "invocation-2", Binding: binding,
	})
	_, busy, _ := f.post(t, otherToken, otherBody)
	if !busy.IsError || len(busy.Content) != 1 || busy.Content[0].Text != "code_intel_host_busy" {
		t.Errorf("second concurrent call = %+v, want busy result (slot must remain held until the blocked call finishes)", busy)
	}

	// The leased workarea must still be protected from eviction: at
	// MaxWorkareas=1, acquiring a DIFFERENT binding directly against the
	// pool must be refused (ErrAtCapacity) for as long as the timed-out call
	// is still in flight.
	if _, err := f.pool.Acquire(context.Background(), bindingWithRevision("other")); !errors.Is(err, ErrAtCapacity) {
		t.Errorf("pool.Acquire(other) error = %v, want ErrAtCapacity (workarea must stay protected until the blocked call finishes)", err)
	}

	close(caller.unblock)

	// Once the underlying call actually finishes, the slot and lease are
	// released: admission recovers.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, r, _ := f.post(t, otherToken, otherBody)
		busyAgain := r.IsError && len(r.Content) == 1 && r.Content[0].Text == "code_intel_host_busy"
		if !busyAgain {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission never recovered after the blocked call finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewHandlerValidation(t *testing.T) {
	t.Parallel()
	now := time.Now
	verifier, err := NewVerifier(VerifierConfig{Secret: testSecret, Issuer: testIssuer, Audience: testAudience, Now: now})
	if err != nil {
		t.Fatalf("NewVerifier() error = %v", err)
	}
	pool, err := NewPool(newFakeFactory(), PoolConfig{MaxWorkareas: 1})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}

	cases := []struct {
		name string
		cfg  HandlerConfig
	}{
		{"missing verifier", HandlerConfig{Pool: pool, MaxConcurrentCalls: 1}},
		{"missing pool", HandlerConfig{Verifier: verifier, MaxConcurrentCalls: 1}},
		{"non-positive max concurrent calls", HandlerConfig{Verifier: verifier, Pool: pool}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHandler(tc.cfg); err == nil {
				t.Error("NewHandler() error = nil, want error")
			}
		})
	}
}
