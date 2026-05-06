package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
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

func TestProvider_Capabilities_Conservative(t *testing.T) {
	t.Parallel()
	p := mustNew(t, "")
	caps := p.Capabilities()
	if caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want false in v0.1")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume: want false (stateless REST)")
	}
	if caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want false in v0.1")
	}
	if caps.HumanLabel != "Gemini" {
		t.Errorf("HumanLabel: want %q, got %q", "Gemini", caps.HumanLabel)
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
		if !strings.Contains(r.URL.Path, "gemini-2.0-flash:streamGenerateContent") {
			t.Errorf("path: want default model in URL, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"world"}]}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}` + "\n\n"))
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)

	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := drain(t, h.Events())
	if len(events) < 4 {
		t.Fatalf("events: want at least Init+2 text+Result (>=4), got %d: %#v", len(events), events)
	}
	if _, ok := events[0].(agent.InitEvent); !ok {
		t.Fatalf("events[0]: want InitEvent, got %T", events[0])
	}
	if _, ok := events[len(events)-1].(agent.ResultEvent); !ok {
		t.Fatalf("events[-1]: want ResultEvent, got %T", events[len(events)-1])
	}
	res := events[len(events)-1].(agent.ResultEvent)
	if !res.Success {
		t.Errorf("Result.Success: want true for finishReason=STOP, got false: %#v", res)
	}
	if res.Cost == nil {
		t.Error("Result.Cost: want populated from usageMetadata")
	}
}

func TestProvider_Spawn_ProbeReturnsHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"API key invalid"}}`))
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err == nil {
		t.Fatal("Spawn: want error on HTTP 400")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want wrapping ErrSpawnFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("Spawn err: want HTTP 400 mention, got %v", err)
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

func TestProvider_Spawn_MidStreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"first"}]}}]}` + "\n\n"))
		// Malformed JSON on next data line.
		_, _ = w.Write([]byte("data: {not-json\n\n"))
	}))
	defer srv.Close()
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drain(t, h.Events())
	last := events[len(events)-1]
	if _, ok := last.(agent.ErrorEvent); !ok {
		t.Fatalf("last event: want ErrorEvent on bad JSON, got %T (events=%#v)", last, events)
	}
}

func TestProvider_Spawn_EOFWithoutTerminal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"truncated"}]}}]}` + "\n\n"))
	}))
	defer srv.Close()
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drain(t, h.Events())
	last := events[len(events)-1]
	errEv, ok := last.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("last event: want ErrorEvent on EOF without finish, got %T", last)
	}
	if errEv.Code != "spawn_no_result" {
		t.Errorf("ErrorEvent.Code: want %q, got %q", "spawn_no_result", errEv.Code)
	}
}

func TestHandle_Stop_ClosesChannel(t *testing.T) {
	t.Parallel()
	// Slow stream that never ends — Stop should unblock the reader.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"a"}]}}]}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Block until client closes; honour ctx.
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Drain at least the init event before stopping.
	<-h.Events()
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Channel must close. drain whatever remains.
	//nolint:revive // empty body is intentional — we just want to verify close
	for range h.Events() {
	}
}

func TestHandle_Inject_Unsupported(t *testing.T) {
	t.Parallel()
	h := &Handle{events: make(chan agent.Event)}
	if err := h.Inject(context.Background(), "follow-up"); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Inject err: want ErrUnsupported, got %v", err)
	}
}

func TestHandle_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"candidates":[{"finishReason":"STOP"}]}` + "\n\n"))
	}))
	defer srv.Close()
	p := mustNew(t, srv.URL)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h.Events())
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #1: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #2: %v", err)
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

func drain(t *testing.T, ch <-chan agent.Event) []agent.Event {
	t.Helper()
	out := make([]agent.Event, 0, 8)
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}
