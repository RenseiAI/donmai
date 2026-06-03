package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fakeEnv returns a Getenv stub that reads from the supplied map.
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestNew_MissingKey_ReturnsProviderUnavailable(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Getenv: fakeEnv(nil)})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), EnvAPIKeyPrimary) {
		t.Fatalf("err: want %s mention, got %v", EnvAPIKeyPrimary, err)
	}
}

func TestNew_FallsBackToGoogleAPIKey(t *testing.T) {
	t.Parallel()
	p, err := New(Options{Getenv: fakeEnv(map[string]string{
		EnvAPIKeyFallback: "fallback-key",
	})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "fallback-key" {
		t.Fatalf("apiKey: want %q, got %q", "fallback-key", p.apiKey)
	}
}

func TestNew_PrimaryKeyWinsOverFallback(t *testing.T) {
	t.Parallel()
	p, err := New(Options{Getenv: fakeEnv(map[string]string{
		EnvAPIKeyPrimary:  "primary-key",
		EnvAPIKeyFallback: "fallback-key",
	})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "primary-key" {
		t.Fatalf("apiKey: want %q, got %q", "primary-key", p.apiKey)
	}
}

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	if got := p.Name(); got != agent.ProviderGemini {
		t.Fatalf("Name: want %q, got %q", agent.ProviderGemini, got)
	}
}

func TestProvider_Capabilities_FullAgentic(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	caps := p.Capabilities()
	if !caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want true (append-and-redrive)")
	}
	if !caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want true")
	}
	if !caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want true (native tools executed in-box)")
	}
	if caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want false (no in-box MCP client; mcp__* calls are not routed)")
	}
	if !caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort: want true")
	}
	if caps.ToolPermissionFormat != ToolPermissionFormatGemini {
		t.Errorf("ToolPermissionFormat: want %q (not claude), got %q", ToolPermissionFormatGemini, caps.ToolPermissionFormat)
	}
	if agent.IsSupported(caps, agent.CapToolPermissionFormatClaude) {
		t.Error("CapToolPermissionFormatClaude: want false for gemini format")
	}
	if caps.HumanLabel != "Gemini" {
		t.Errorf("HumanLabel: want Gemini, got %q", caps.HumanLabel)
	}
}

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	_, err := p.Resume(context.Background(), "session", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err: want ErrUnsupported, got %v", err)
	}
}

func TestProvider_Shutdown_NoOp(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}
}

func TestProvider_Spawn_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("x-goog-api-key"), "test-key"; got != want {
			t.Errorf("x-goog-api-key: want %q, got %q", want, got)
		}
		if !strings.Contains(r.URL.Path, "gemini-3.5-flash:generateContent") {
			t.Errorf("path: want default model :generateContent, got %q", r.URL.Path)
		}
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"Hello world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := drainUntilResult(t, h)
	if _, ok := events[0].(agent.InitEvent); !ok {
		t.Fatalf("events[0]: want InitEvent, got %T", events[0])
	}
	res, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("events[-1]: want ResultEvent, got %T", events[len(events)-1])
	}
	if !res.Success {
		t.Errorf("Result.Success: want true for STOP, got %#v", res)
	}
	if res.Cost == nil || res.Cost.TotalCostUsd <= 0 {
		t.Errorf("Result.Cost: want positive TotalCostUsd, got %#v", res.Cost)
	}
}

// TestProvider_Spawn_ToolRoundTrip drives the full agentic loop: the
// model emits a Bash functionCall, the provider's session-local executor
// runs it in-box (NO runner Inject), and the model returns final text.
// This is the core deliverable — an autonomous session completes without
// any external tool-executor.
//
// It also locks in the critical wire-role fix: the tool-result turn the
// provider sends back MUST be role "user" (the live generateContent API
// rejects role "function").
func TestProvider_Spawn_ToolRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var turnNum int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turnNum++
		n := turnNum
		mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var req requestBody
		_ = json.Unmarshal(body, &req)

		switch n {
		case 1:
			// First turn carries the prompt + tool declarations.
			if len(req.Tools) == 0 {
				t.Errorf("turn 1: want tools in request, got none")
			}
			// Ask the executor to run a deterministic command in the
			// session cwd. The result is folded back in turn 2.
			writeJSON(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"Bash","args":{"command":"echo hello-gemini"}}}]}}]}`)
		case 2:
			// Second turn must carry the auto-folded functionResponse
			// turn. CRITICAL: role must be "user", not "function".
			last := req.Contents[len(req.Contents)-1]
			if last.Role != "user" {
				t.Errorf("turn 2: want last content role=user (live API rejects \"function\"), got %q", last.Role)
			}
			if last.Parts[0].FunctionResponse == nil {
				t.Fatalf("turn 2: want functionResponse part, got %#v", last.Parts[0])
			}
			if last.Parts[0].FunctionResponse.ID != "call-1" {
				t.Errorf("turn 2: functionResponse id: want call-1, got %q", last.Parts[0].FunctionResponse.ID)
			}
			// The executor ran `echo hello-gemini`; its output must ride
			// inside the functionResponse so the model can act on it.
			out, _ := last.Parts[0].FunctionResponse.Response["output"].(string)
			if !strings.Contains(out, "hello-gemini") {
				t.Errorf("turn 2: want executor output in functionResponse, got %#v", last.Parts[0].FunctionResponse.Response)
			}
			writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":10}}`)
		default:
			t.Errorf("unexpected turn %d", n)
			writeJSON(w, `{"candidates":[{"finishReason":"STOP"}]}`)
		}
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:       "list files",
		Cwd:          dir,
		Autonomous:   true,
		AllowedTools: []string{"Bash(echo:*)"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// The session runs autonomously: no Inject. We only observe events.
	var sawToolUse, sawToolResult bool
	for ev := range h.Events() {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			sawToolUse = true
			if e.ToolName != "Bash" || e.ToolUseID != "call-1" {
				t.Errorf("ToolUse: want Bash/call-1, got %s/%s", e.ToolName, e.ToolUseID)
			}
		case agent.ToolResultEvent:
			sawToolResult = true
			if e.ToolUseID != "call-1" {
				t.Errorf("ToolResult: want id call-1, got %q", e.ToolUseID)
			}
			if e.IsError {
				t.Errorf("ToolResult: want success, got error: %q", e.Content)
			}
			if !strings.Contains(e.Content, "hello-gemini") {
				t.Errorf("ToolResult: want executor output, got %q", e.Content)
			}
		case agent.ResultEvent:
			if !sawToolUse {
				t.Error("got ResultEvent before any ToolUse")
			}
			if !sawToolResult {
				t.Error("got ResultEvent without a ToolResult (executor did not run)")
			}
			if !e.Success {
				t.Errorf("Result.Success: want true, got %#v", e)
			}
			if e.Cost == nil || e.Cost.NumTurns != 2 {
				t.Errorf("Result.Cost.NumTurns: want 2, got %#v", e.Cost)
			}
			_ = h.Stop(context.Background())
			goto done
		}
	}
done:
	if !sawToolUse {
		t.Fatal("never observed a ToolUse event")
	}
}

// TestProvider_Spawn_PerSpawnKeyFromSpecEnv verifies the per-Spawn key
// resolution: Spec.Env[GEMINI_API_KEY] overrides the construction key.
func TestProvider_Spawn_PerSpawnKeyFromSpecEnv(t *testing.T) {
	t.Parallel()
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL) // construction key = "test-key"
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Env:    map[string]string{EnvAPIKeyPrimary: "per-session-key"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	drainUntilResult(t, h)

	if gotKey != "per-session-key" {
		t.Errorf("x-goog-api-key: want per-session-key (Spec.Env override), got %q", gotKey)
	}
}

func TestProvider_Spawn_PerSpawnKeyFallsBackToGoogle(t *testing.T) {
	t.Parallel()
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Env:    map[string]string{EnvAPIKeyFallback: "google-session-key"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	drainUntilResult(t, h)

	if gotKey != "google-session-key" {
		t.Errorf("x-goog-api-key: want google-session-key, got %q", gotKey)
	}
}

func TestResolveSpawnKey_Precedence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		fallback string
		want     string
	}{
		{"primary wins", map[string]string{EnvAPIKeyPrimary: "p", EnvAPIKeyFallback: "g"}, "c", "p"},
		{"google fallback", map[string]string{EnvAPIKeyFallback: "g"}, "c", "g"},
		{"construction fallback", nil, "c", "c"},
		{"blank env values ignored", map[string]string{EnvAPIKeyPrimary: "  "}, "c", "c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveSpawnKey(agent.Spec{Env: tc.env}, tc.fallback)
			if got != tc.want {
				t.Errorf("resolveSpawnKey: want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestProvider_Spawn_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"API key invalid"}}`))
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	events := drainUntilResult(t, h)
	last := events[len(events)-1]
	errEv, ok := last.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("last event: want ErrorEvent on HTTP 400, got %T", last)
	}
	if !strings.Contains(errEv.Message, "400") {
		t.Errorf("ErrorEvent.Message: want 400 mention, got %q", errEv.Message)
	}
}

func TestProvider_Spawn_EmptyPromptRejected(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	_, err := p.Spawn(context.Background(), agent.Spec{Prompt: ""})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want wrapping ErrSpawnFailed, got %v", err)
	}
}

func TestProvider_Spawn_NoKeyResolvable(t *testing.T) {
	t.Parallel()
	// Provider built with a key, but a Spec.Env that blanks both keys
	// while leaving the construction fallback intact still resolves; to
	// exercise the no-key path we use a provider with an explicit empty
	// apiKey (constructed directly, bypassing New's validation).
	p := &Provider{endpoint: DefaultEndpoint, sessionIDFn: func() string { return "s" }, defaultModel: DefaultModel}
	_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want ErrSpawnFailed for no key, got %v", err)
	}
}

func TestHandle_Stop_ClosesChannel(t *testing.T) {
	t.Parallel()
	// Slow server that blocks until ctx cancel — Stop must unblock.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-h.Events() // InitEvent
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	//nolint:revive // draining to verify close
	for range h.Events() {
	}
}

func TestHandle_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drainUntilResult(t, h)
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #1: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #2: %v", err)
	}
}

func TestHandle_Inject_BeforeStartReturnsNotReady(t *testing.T) {
	t.Parallel()
	h := &Handle{events: make(chan agent.Event), shutdown: make(chan struct{}), inject: make(chan injectMsg, 1)}
	if err := h.Inject(context.Background(), "x"); !errors.Is(err, ErrSessionNotReady) {
		t.Fatalf("Inject err: want ErrSessionNotReady, got %v", err)
	}
}

// TestHandle_Inject_Steering verifies post-completion steering: after a
// final turn the channel stays open, an injected user turn re-drives.
func TestHandle_Inject_Steering(t *testing.T) {
	t.Parallel()
	var turnNum int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turnNum++
		n := turnNum
		mu.Unlock()
		if n == 2 {
			body, _ := io.ReadAll(r.Body)
			var req requestBody
			_ = json.Unmarshal(body, &req)
			last := req.Contents[len(req.Contents)-1]
			if last.Role != "user" || last.Parts[0].Text != "open a PR" {
				t.Errorf("turn 2: want injected user turn, got role=%q text=%q", last.Role, last.Parts[0].Text)
			}
		}
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"ack"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	resultCount := 0
	for ev := range h.Events() {
		if _, ok := ev.(agent.ResultEvent); ok {
			resultCount++
			if resultCount == 1 {
				if err := h.Inject(context.Background(), "open a PR"); err != nil {
					t.Fatalf("Inject steering: %v", err)
				}
				continue
			}
			// Second result observed after steering; done.
			_ = h.Stop(context.Background())
			break
		}
	}
	if resultCount < 2 {
		t.Fatalf("want 2 ResultEvents (original + post-steering), got %d", resultCount)
	}
}

func mustNew(t *testing.T, endpoint string) *Provider {
	t.Helper()
	opts := Options{APIKey: "test-key"}
	if endpoint != "" {
		opts.Endpoint = endpoint
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// drainUntilResult consumes events up to and including the first terminal
// ResultEvent / ErrorEvent and returns them. It does not consume past the
// terminal so a follow-up steering inject test can continue.
func drainUntilResult(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	out := make([]agent.Event, 0, 8)
	for ev := range h.Events() {
		out = append(out, ev)
		switch ev.(type) {
		case agent.ResultEvent, agent.ErrorEvent:
			return out
		}
	}
	return out
}
