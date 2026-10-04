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
	"time"

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
	if !caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want true (in-box MCP bridge routes mcp__* calls to live servers)")
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
	var llm *agent.LlmCallEvent
	for _, ev := range events {
		if event, ok := ev.(agent.LlmCallEvent); ok {
			llm = &event
			break
		}
	}
	if llm == nil || llm.StartTimeUnixNano == "" || llm.EndTimeUnixNano == "" {
		t.Fatalf("events missing timed LlmCallEvent: %#v", events)
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

// TestProvider_Spawn_ThinkingLevelAndDispatcherCost drives the production
// entry point end to end: Spawn POSTs the plan to an HTTP fake, the fake
// answers with usage, and the terminal ResultEvent carries the
// wire-selected thinking knob plus dispatcher-supplied pricing. It goes
// RED when providerConfig forwarding is dropped (cost falls back to the
// table), when family routing sends the budget knob (no thinkingLevel on
// the wire), and when cached-rate assignment is lost (the cached slice is
// priced at the input rate).
func TestProvider_Spawn_ThinkingLevelAndDispatcherCost(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotLevel string
	var gotBudget *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req requestBody
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		if req.GenerationConfig != nil && req.GenerationConfig.ThinkingConfig != nil {
			gotLevel = req.GenerationConfig.ThinkingConfig.ThinkingLevel
			gotBudget = req.GenerationConfig.ThinkingConfig.ThinkingBudget
		}
		mu.Unlock()
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000000,"candidatesTokenCount":1000000,"cachedContentTokenCount":200000}}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "x",
		Model:  "gemini-4-pro",
		Effort: agent.EffortMedium,
		ProviderConfig: map[string]any{
			"inputPricePer1M": 2.0, "cachedPricePer1M": 0.5, "outputPricePer1M": 8.0,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	events := drainUntilResult(t, h)
	res, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("events[-1]: want ResultEvent, got %T", events[len(events)-1])
	}

	mu.Lock()
	defer mu.Unlock()
	if gotLevel != "medium" {
		t.Errorf("wire thinkingLevel: want medium (gemini-4-pro effort medium), got %q", gotLevel)
	}
	if gotBudget != nil {
		t.Errorf("wire thinkingBudget: want nil for a level-knob model, got %d", *gotBudget)
	}
	// 800k fresh @ 2.00/M + 200k cached @ 0.50/M + 1M out @ 8.00/M.
	want := 1.6 + 0.1 + 8.0
	if res.Cost == nil {
		t.Fatal("Result.Cost: want non-nil")
	}
	if diff := res.Cost.TotalCostUsd - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Cost.TotalCostUsd: want %g (dispatcher rates), got %g", want, res.Cost.TotalCostUsd)
	}
	if res.Cost.CachedInputTokens != 200000 {
		t.Errorf("Cost.CachedInputTokens: want 200000, got %d", res.Cost.CachedInputTokens)
	}
}

// TestProvider_Spawn_CachedOnlyOverrideApplies verifies a dispatcher spec
// carrying ONLY a cached price still overrides the cached slice: 1M cached
// tokens at $0.25/M costs $0.25. Before the fix the override path required
// a positive input or output price, so the spec was ignored and the table
// priced the cached slice at the input rate ($1.50).
func TestProvider_Spawn_CachedOnlyOverrideApplies(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000000,"candidatesTokenCount":0,"cachedContentTokenCount":1000000}}`)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:         "x",
		Model:          "gemini-3.5-flash",
		ProviderConfig: map[string]any{"cachedPricePer1M": 0.25},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	events := drainUntilResult(t, h)
	res, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("events[-1]: want ResultEvent, got %T", events[len(events)-1])
	}
	if res.Cost == nil {
		t.Fatal("Result.Cost: want non-nil")
	}
	if diff := res.Cost.TotalCostUsd - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Cost.TotalCostUsd: want 0.25 (cached-only override), got %g", res.Cost.TotalCostUsd)
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
		epEnv    map[string]string // nil = no Spec.Endpoint
		env      map[string]string
		fallback string
		want     string
	}{
		{"primary wins", nil, map[string]string{EnvAPIKeyPrimary: "p", EnvAPIKeyFallback: "g"}, "c", "p"},
		{"google fallback", nil, map[string]string{EnvAPIKeyFallback: "g"}, "c", "g"},
		{"construction fallback", nil, nil, "c", "c"},
		{"blank env values ignored", nil, map[string]string{EnvAPIKeyPrimary: "  "}, "c", "c"},
		{"endpoint binding wins over session env", map[string]string{EnvAPIKeyPrimary: "ep"}, map[string]string{EnvAPIKeyPrimary: "p"}, "c", "ep"},
		{"endpoint google key wins over session primary", map[string]string{EnvAPIKeyFallback: "epg"}, map[string]string{EnvAPIKeyPrimary: "p"}, "c", "epg"},
		{"keyless endpoint falls through to session env", map[string]string{}, map[string]string{EnvAPIKeyPrimary: "p"}, "c", "p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := agent.Spec{Env: tc.env}
			if tc.epEnv != nil {
				spec.Endpoint = &agent.EndpointBinding{Company: agent.CompanyGoogle, Env: tc.epEnv}
			}
			got := resolveSpawnKey(spec, tc.fallback)
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
	// Slow server that parks the request until the client goes away.
	// The release backstop plus CloseClientConnections keep teardown
	// bounded even if request-context cancellation never tears down the
	// connection: httptest Close waits for active handlers, so a parked
	// handler would otherwise hang the suite to the package timeout.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer srv.CloseClientConnections()
	defer close(release)

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-h.Events() // InitEvent

	// Stop must return promptly even though no turn ever ran.
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Stop did not return within 10s with no turn run")
	}

	// The driver must exit and close the events channel. Bounded: a
	// regressed Stop that never unblocks the driver fails here within
	// the test's own deadline instead of hanging the suite.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		//nolint:revive // draining to verify close
		for range h.Events() {
		}
	}()
	select {
	case <-drainDone:
	case <-time.After(30 * time.Second):
		t.Fatalf("events channel did not close within 30s after Stop")
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

// TestProvider_Spawn_MaxTurnsCap verifies that the driver terminates with
// a non-success ResultEvent (ErrorSubtype "error_max_turns") when the model
// keeps emitting functionCalls beyond the MaxTurns ceiling. Without the
// cap a model that loops on tool calls would run unbounded with unbounded
// API spend.
func TestProvider_Spawn_MaxTurnsCap(t *testing.T) {
	t.Parallel()

	// The fake server always responds with a functionCall so the driver
	// would loop forever without a MaxTurns cap.
	var turnNum int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turnNum++
		mu.Unlock()
		// Every response is a functionCall (Bash echo) so the loop
		// never reaches outcomeFinal on its own.
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"Bash","args":{"command":"echo loop"}}}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":5}}`)
	}))
	defer srv.Close()

	maxTurns := 2
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:       "loop forever",
		MaxTurns:     &maxTurns,
		AllowedTools: []string{"Bash"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := drainUntilResult(t, h)

	// The last event must be a ResultEvent with Success=false and the
	// "error_max_turns" subtype. It must NOT be an ErrorEvent (which
	// would indicate a driver panic/bug rather than a clean cap).
	last := events[len(events)-1]
	res, ok := last.(agent.ResultEvent)
	if !ok {
		t.Fatalf("last event: want ResultEvent (clean cap), got %T: %#v", last, last)
	}
	if res.Success {
		t.Error("Result.Success: want false when MaxTurns exceeded")
	}
	if res.ErrorSubtype != "error_max_turns" {
		t.Errorf("ErrorSubtype: want error_max_turns, got %q", res.ErrorSubtype)
	}
	if res.Cost == nil {
		t.Fatal("Result.Cost: want non-nil (cost must be reported even on cap)")
	}
	// The driver must have executed exactly cap turns before stopping.
	if res.Cost.NumTurns != maxTurns {
		t.Errorf("Cost.NumTurns: want %d (cap hit), got %d", maxTurns, res.Cost.NumTurns)
	}

	mu.Lock()
	gotTurns := turnNum
	mu.Unlock()
	if gotTurns != maxTurns {
		t.Errorf("server received %d requests, want exactly %d (cap=%d)", gotTurns, maxTurns, maxTurns)
	}
}

// TestProvider_Spawn_MaxTurnsZeroUncapped verifies that MaxTurns=0 (or
// absent) does NOT cap the session (the session finishes normally).
func TestProvider_Spawn_MaxTurnsZeroUncapped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Single clean STOP response — no tool calls.
		writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`)
	}))
	defer srv.Close()

	zero := 0
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:   "simple task",
		MaxTurns: &zero,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := drainUntilResult(t, h)
	last := events[len(events)-1]
	res, ok := last.(agent.ResultEvent)
	if !ok {
		t.Fatalf("last event: want ResultEvent, got %T", last)
	}
	if !res.Success {
		t.Errorf("Result.Success: want true for STOP with MaxTurns=0 (uncapped), got %#v", res)
	}
}

// TestProvider_Spawn_MCPToolRoundTrip drives the full bridged-MCP loop
// against a REAL Streamable HTTP MCP server (httptest, via runtime/mcp's
// default dialer): Spawn dials it, discovers its tools, declares
// mcp__fake__echo to the model (replacing the catch-all placeholder), the
// model calls it, the bridge routes the call to the live server, and the
// server's output folds back into turn 2 as the functionResponse.
func TestProvider_Spawn_MCPToolRoundTrip(t *testing.T) {
	t.Parallel()

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("MCP server: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil { // notifications/initialized
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer rsk-test" {
			t.Errorf("MCP Authorization = %q, want configured bearer", got)
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "0"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "echoes text back",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
			}}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name != "echo" {
				t.Errorf("tools/call name = %q, want echo", p.Name)
			}
			text, _ := p.Arguments["text"].(string)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo: " + text}}}
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer mcpSrv.Close()

	var mu sync.Mutex
	var turnNum int
	geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turnNum++
		n := turnNum
		mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var req requestBody
		_ = json.Unmarshal(body, &req)

		switch n {
		case 1:
			// The discovered tool must be declared under its real name and
			// the per-server catch-all must be gone.
			names := sortedToolNames(req.Tools)
			hasReal, hasCatchAll := false, false
			for _, name := range names {
				hasReal = hasReal || name == "mcp__fake__echo"
				hasCatchAll = hasCatchAll || name == "mcp__fake"
			}
			if !hasReal || hasCatchAll {
				t.Errorf("turn 1 declarations = %v, want mcp__fake__echo and no catch-all", names)
			}
			writeJSON(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-7","name":"mcp__fake__echo","args":{"text":"hi"}}}]}}]}`)
		case 2:
			last := req.Contents[len(req.Contents)-1]
			fr := last.Parts[0].FunctionResponse
			if fr == nil || fr.ID != "call-7" {
				t.Errorf("turn 2: want functionResponse call-7, got %#v", last)
			} else if out, _ := fr.Response["output"].(string); out != "echo: hi" {
				t.Errorf("turn 2 functionResponse = %#v, want live MCP server output", fr.Response)
			}
			writeJSON(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
		default:
			t.Errorf("unexpected turn %d", n)
			writeJSON(w, `{"candidates":[{"finishReason":"STOP"}]}`)
		}
	}))
	defer geminiSrv.Close()

	p := mustNew(t, geminiSrv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:     "use the MCP tool",
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{
			Name:    "fake",
			Type:    "http",
			URL:     mcpSrv.URL,
			Headers: map[string]string{"Authorization": "Bearer rsk-test"},
		}},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := drainUntilResult(t, h)
	var sawToolResult bool
	for _, ev := range events {
		if e, ok := ev.(agent.ToolResultEvent); ok {
			sawToolResult = true
			if e.IsError || e.Content != "echo: hi" {
				t.Errorf("ToolResult = %#v, want live MCP output", e)
			}
		}
	}
	if !sawToolResult {
		t.Fatal("never observed the bridged ToolResultEvent")
	}
	res, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok || !res.Success {
		t.Fatalf("terminal event: want successful ResultEvent, got %#v", events[len(events)-1])
	}
}

// TestProvider_Spawn_MCPServerUnavailableFailsClosed proves a required MCP
// service is connected and enumerated before the first model turn.
func TestProvider_Spawn_MCPServerUnavailableFailsClosed(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests int
	geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "model must not be called", http.StatusInternalServerError)
	}))
	defer geminiSrv.Close()

	p := mustNew(t, geminiSrv.URL)
	p.dialMCP = failingDialer
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:     "try the MCP tool",
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{Name: "bad", Type: "http", URL: "http://127.0.0.1:1"}},
	})
	if err == nil || !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), "required MCP servers unavailable: bad") {
		if h != nil {
			_ = h.Stop(context.Background())
		}
		t.Fatalf("Spawn error = %v, want typed fail-closed MCP denial", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("model received %d requests after MCP admission failed", requests)
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
