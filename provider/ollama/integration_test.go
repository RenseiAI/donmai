package ollama

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// fakeOllama is a lightweight httptest server that mimics the subset
// of the Ollama API the provider exercises:
//
//   - GET  /api/tags  → 200 with a stub catalog
//   - POST /api/chat  → streams scripted NDJSON chunks then EOF
//
// Tests construct one per scenario and configure the chat-stream
// behavior via fields on the struct.
type fakeOllama struct {
	server *httptest.Server

	// chatChunks are the NDJSON lines the chat handler streams in
	// order. Tests write the literal JSON bodies they want returned;
	// the handler appends '\n' between them.
	chatChunks []string

	// chatStatus overrides the chat HTTP status. 0 means 200.
	chatStatus int

	// chatErrBody overrides the body served when chatStatus is non-2xx.
	chatErrBody string

	// chatBetween is the delay between chunks. Useful for cancellation
	// tests that need to reach the body reader before EOF.
	chatBetween time.Duration

	// hold blocks the chat handler until released; used to test Stop
	// against a long-running request.
	hold chan struct{}

	// reqMu / requests accumulate received requests for test assertions.
	reqMu    sync.Mutex
	requests []*http.Request
}

func newFakeOllama() *fakeOllama {
	f := &fakeOllama{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		f.recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.3:latest"}]}`)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		f.recordRequest(r)
		if f.chatStatus != 0 && (f.chatStatus < 200 || f.chatStatus >= 300) {
			body := f.chatErrBody
			if body == "" {
				body = `{"error":"unknown"}`
			}
			http.Error(w, body, f.chatStatus)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Flush headers immediately so the client's Do() returns
		// before any optional `hold` blocks chunk delivery — without
		// this the client blocks in roundTrip and tests that want to
		// exercise body-stream cancellation never reach the body
		// reader.
		if flusher != nil {
			flusher.Flush()
		}
		if f.hold != nil {
			select {
			case <-f.hold:
			case <-r.Context().Done():
				return
			}
		}
		for _, c := range f.chatChunks {
			if f.chatBetween > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(f.chatBetween):
				}
			}
			if _, err := io.WriteString(w, c+"\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeOllama) recordRequest(r *http.Request) {
	f.reqMu.Lock()
	defer f.reqMu.Unlock()
	f.requests = append(f.requests, r.Clone(context.Background()))
}

func (f *fakeOllama) URL() string { return f.server.URL }
func (f *fakeOllama) Close()      { f.server.Close() }

func TestNew_probeSucceeds(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()

	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != agent.ProviderOllama {
		t.Errorf("Name: got %q want %q", p.Name(), agent.ProviderOllama)
	}
	caps := p.Capabilities()
	if caps.SupportsMessageInjection || caps.SupportsSessionResume || caps.SupportsToolPlugins {
		t.Errorf("Capabilities: ollama should not advertise injection/resume/tools; got %+v", caps)
	}
	// Tool-use surface (002 v2): false/false — /api/chat does not expose
	// `tools` or MCP shape, so Spec.AllowedTools / Spec.MCPServers are
	// silently dropped by the runner before reaching this provider.
	if caps.AcceptsAllowedToolsList || caps.AcceptsMcpServerSpec {
		t.Errorf("Capabilities: ollama should not advertise tool-use accept flags; got %+v", caps)
	}
	if caps.HumanLabel != "Ollama" {
		t.Errorf("Capabilities.HumanLabel: got %q want Ollama", caps.HumanLabel)
	}
}

func TestNew_probeFailsWrapsErrProviderUnavailable(t *testing.T) {
	t.Parallel()
	// Listen-and-immediately-close to get a guaranteed-unreachable URL.
	srv := newFakeOllama()
	srv.Close()

	_, err := New(Options{Endpoint: srv.URL(), ProbeTimeout: 250 * time.Millisecond})
	if err == nil {
		t.Fatal("expected error for unreachable endpoint, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("error should wrap ErrProviderUnavailable; got %v", err)
	}
	if !strings.Contains(err.Error(), "ollama serve") {
		t.Errorf("error should hint at remediation; got %q", err.Error())
	}
}

func TestNew_probeNon2xxFails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(Options{Endpoint: srv.URL})
	if err == nil {
		t.Fatal("expected error for 500 probe, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("error should wrap ErrProviderUnavailable; got %v", err)
	}
}

func TestSpawn_streamsToTerminalEvent(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.chatChunks = []string{
		`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
		`{"message":{"role":"assistant","content":", world"},"done":false}`,
		`{"done":true,"done_reason":"stop","prompt_eval_count":4,"eval_count":7}`,
	}

	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: "say hello"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	got := drain(t, h.Events(), 5*time.Second)
	wantKinds := []agent.EventKind{
		agent.EventInit,
		agent.EventAssistantText,
		agent.EventAssistantText,
		agent.EventResult,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("event count: got %d want %d (got=%v)", len(got), len(wantKinds), kinds(got))
	}
	for i, w := range wantKinds {
		if got[i].Kind() != w {
			t.Errorf("event[%d]: kind got %q want %q", i, got[i].Kind(), w)
		}
	}
	res := got[len(got)-1].(agent.ResultEvent)
	if !res.Success {
		t.Errorf("ResultEvent.Success: got false want true")
	}
	if res.Cost == nil || res.Cost.InputTokens != 4 || res.Cost.OutputTokens != 7 {
		t.Errorf("ResultEvent.Cost: got %+v", res.Cost)
	}
	if h.SessionID() == "" {
		t.Errorf("SessionID: got empty want non-empty synthetic id")
	}
	if !strings.HasPrefix(h.SessionID(), "ollama-session-") {
		t.Errorf("SessionID: got %q want ollama-session-* prefix", h.SessionID())
	}
}

func TestSpawn_serverErrorWrapsErrSpawnFailed(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.chatStatus = http.StatusBadRequest
	srv.chatErrBody = `{"error":"model not loaded"}`

	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Spawn(t.Context(), agent.Spec{Model: "missing", Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("error should wrap ErrSpawnFailed; got %v", err)
	}
	if !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("error should include server tail; got %q", err.Error())
	}
}

func TestSpawn_midStreamErrorChunk(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.chatChunks = []string{
		`{"message":{"role":"assistant","content":"Hi"},"done":false}`,
		`{"error":"model crashed"}`,
	}
	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()
	got := drain(t, h.Events(), 5*time.Second)
	if len(got) < 3 {
		t.Fatalf("expected at least 3 events (init, text, error), got %v", kinds(got))
	}
	last := got[len(got)-1]
	if last.Kind() != agent.EventError {
		t.Errorf("last event: got %q want %q", last.Kind(), agent.EventError)
	}
}

func TestSpawn_eofWithoutTerminalEmitsSpawnNoResult(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.chatChunks = []string{
		`{"message":{"role":"assistant","content":"Hi"},"done":false}`,
		// No done=true chunk: server closes mid-stream.
	}
	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()
	got := drain(t, h.Events(), 5*time.Second)
	last := got[len(got)-1]
	er, ok := last.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("expected final ErrorEvent, got %T", last)
	}
	if er.Code != "spawn_no_result" {
		t.Errorf("code: got %q want spawn_no_result", er.Code)
	}
}

func TestStop_cancelsInFlightRequest(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.hold = make(chan struct{})
	// Provide a chunk so that once we release `hold` the stream proceeds,
	// but normally we'll Stop before any chunk arrives.
	srv.chatChunks = []string{
		`{"done":true,"done_reason":"stop"}`,
	}

	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Releasing the server now should be a no-op for the test; we just
	// want to make sure the goroutine doesn't leak.
	close(srv.hold)
	// Events channel should be closed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("events channel did not close after Stop")
		}
	}
}

func TestInject_returnsUnsupported(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	srv.chatChunks = []string{`{"done":true,"done_reason":"stop"}`}

	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{Model: "llama3.3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()
	err = h.Inject(t.Context(), "follow-up")
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Inject error: got %v want wrapping ErrUnsupported", err)
	}
}

func TestResume_returnsUnsupported(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Resume(t.Context(), "does-not-matter", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Resume error: got %v want wrapping ErrUnsupported", err)
	}
}

func TestShutdown_isNoop(t *testing.T) {
	t.Parallel()
	srv := newFakeOllama()
	defer srv.Close()
	p, err := New(Options{Endpoint: srv.URL()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: got %v want nil", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func drain(t *testing.T, ch <-chan agent.Event, deadline time.Duration) []agent.Event {
	t.Helper()
	var got []agent.Event
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-timer.C:
			t.Fatalf("drain: deadline exceeded; collected so far=%v", kinds(got))
			return got
		}
	}
}

func kinds(evs []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind())
	}
	return out
}

// Sanity check: the package implements the agent.Provider interface.
var _ agent.Provider = (*Provider)(nil)

func TestCompileTimeInterface(t *testing.T) {
	// Compile-time only — guarantees Provider/Handle satisfy the
	// agent contracts.
	t.Helper()
	var _ agent.Provider = (*Provider)(nil)
	var _ agent.Handle = (*Handle)(nil)
	_ = fmt.Sprintf("%T", New)
}
