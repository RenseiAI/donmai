package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// fakeProviderRegistry is a minimal ProviderRegistry used to assert the
// LLM provider state projection in handleGetRoutingConfig.
type fakeProviderRegistry struct {
	names []string
	caps  map[string]map[string]any
}

func (f *fakeProviderRegistry) Names() []string { return append([]string(nil), f.names...) }
func (f *fakeProviderRegistry) Capabilities(name string) (map[string]any, bool) {
	c, ok := f.caps[name]
	return c, ok
}

// newRoutingTestServer builds a Server wired around a Daemon with the
// given provider registry. The daemon is NOT started (HTTP control API
// pieces only) — the test only exercises mux dispatch + handlers.
func newRoutingTestServer(t *testing.T, reg ProviderRegistry) (*Server, *Daemon) {
	t.Helper()
	d := New(Options{
		ConfigPath:       "/dev/null", // unused: we never call Start
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		ProviderRegistry: reg,
	})
	srv := &Server{daemon: d}
	return srv, d
}

func TestHandleGetRoutingConfig_EmptyStore(t *testing.T) {
	t.Parallel()
	srv, _ := newRoutingTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/config", nil)
	rec := httptest.NewRecorder()
	srv.handleGetRoutingConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp afclient.RoutingConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Config.Weights != DefaultRoutingWeights {
		t.Errorf("weights = %+v, want %+v", resp.Config.Weights, DefaultRoutingWeights)
	}
	if len(resp.Config.LLMProviders) != 0 {
		t.Errorf("len(LLMProviders) = %d, want 0", len(resp.Config.LLMProviders))
	}
	if len(resp.Config.SandboxProviders) != 1 || resp.Config.SandboxProviders[0].ProviderID != "local" {
		t.Errorf("SandboxProviders = %+v, want single local row", resp.Config.SandboxProviders)
	}
	if len(resp.Config.RecentDecisions) != 0 {
		t.Errorf("len(RecentDecisions) = %d, want 0", len(resp.Config.RecentDecisions))
	}
}

func TestHandleGetRoutingConfig_WithProviderRegistryAndDecisions(t *testing.T) {
	t.Parallel()
	reg := &fakeProviderRegistry{names: []string{"codex", "claude", "stub"}}
	srv, d := newRoutingTestServer(t, reg)

	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	d.routingTraces.RecordDecision(makeDecision("sess-1", "local", "claude", base), nil)
	d.routingTraces.RecordDecision(makeDecision("sess-2", "vercel", "codex", base.Add(time.Minute)), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/config", nil)
	rec := httptest.NewRecorder()
	srv.handleGetRoutingConfig(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d, body=%s", rec.Code, body)
	}
	var resp afclient.RoutingConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(resp.Config.LLMProviders); got != 3 {
		t.Errorf("len(LLMProviders) = %d, want 3", got)
	}
	wantOrder := []string{"claude", "codex", "stub"}
	for i, p := range resp.Config.LLMProviders {
		if p.ProviderID != wantOrder[i] {
			t.Errorf("LLMProviders[%d].ProviderID = %q, want %q", i, p.ProviderID, wantOrder[i])
		}
	}
	// Local sandbox SelectionCount counts only "local"-chosen decisions.
	if resp.Config.SandboxProviders[0].SelectionCount != 1 {
		t.Errorf("local SelectionCount = %d, want 1",
			resp.Config.SandboxProviders[0].SelectionCount)
	}
	if got := len(resp.Config.RecentDecisions); got != 2 {
		t.Errorf("len(RecentDecisions) = %d, want 2", got)
	}
}

func TestHandleGetRoutingConfig_NilStoreSafe(t *testing.T) {
	t.Parallel()
	// Defensively constructed daemon with nil routingTraces — handler
	// must still produce a 200.
	d := &Daemon{}
	d.state.Store(StateRunning)
	srv := &Server{daemon: d}

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/config", nil)
	rec := httptest.NewRecorder()
	srv.handleGetRoutingConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp afclient.RoutingConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Config.Weights != DefaultRoutingWeights {
		t.Errorf("weights = %+v, want defaults", resp.Config.Weights)
	}
}

func TestHandleExplainRouting_HappyPath(t *testing.T) {
	t.Parallel()
	srv, d := newRoutingTestServer(t, nil)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	decision := makeDecision("sess-explain-1", "local", "claude", now)
	trace := makeTrace(2)
	trace[1].Note = "winner"
	d.routingTraces.RecordDecision(decision, trace)

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/explain/sess-explain-1", nil)
	rec := httptest.NewRecorder()
	srv.handleExplainRouting(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d, body=%s", rec.Code, body)
	}
	var resp afclient.RoutingExplainResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != "sess-explain-1" {
		t.Errorf("SessionID = %q, want sess-explain-1", resp.SessionID)
	}
	if resp.Decision.ChosenSandbox != "local" {
		t.Errorf("ChosenSandbox = %q, want local", resp.Decision.ChosenSandbox)
	}
	if len(resp.Trace) != 2 {
		t.Errorf("len(Trace) = %d, want 2", len(resp.Trace))
	}
	if resp.Trace[1].Note != "winner" {
		t.Errorf("Trace[1].Note = %q, want winner", resp.Trace[1].Note)
	}
}

func TestHandleExplainRouting_NotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newRoutingTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/explain/missing-session", nil)
	rec := httptest.NewRecorder()
	srv.handleExplainRouting(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("error envelope missing 'error' key: %+v", body)
	}
	if body["sessionId"] != "missing-session" {
		t.Errorf("sessionId = %q, want missing-session", body["sessionId"])
	}
}

func TestHandleExplainRouting_EmptyOrNestedID(t *testing.T) {
	t.Parallel()
	srv, _ := newRoutingTestServer(t, nil)

	for _, path := range []string{
		"/api/daemon/routing/explain/",
		"/api/daemon/routing/explain/foo/bar",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.handleExplainRouting(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path=%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestHandleExplainRouting_RejectsNonGET(t *testing.T) {
	t.Parallel()
	srv, _ := newRoutingTestServer(t, nil)

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, "/api/daemon/routing/explain/sess-1", nil)
		rec := httptest.NewRecorder()
		srv.handleExplainRouting(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method=%s status = %d, want 405", m, rec.Code)
		}
	}
}

func TestHandleExplainRouting_NilStoreSafe(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.state.Store(StateRunning)
	srv := &Server{daemon: d}

	req := httptest.NewRequest(http.MethodGet, "/api/daemon/routing/explain/anything", nil)
	rec := httptest.NewRecorder()
	srv.handleExplainRouting(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleExplainRouting_LiveSessionEndToEnd covers the Wave 11 / S6a
// wire-up at the HTTP layer: a real WorkerSpawner accepts a session →
// the daemon's Started listener records a routing decision → GET
// /api/daemon/routing/explain/<id> returns 200 with the recorded
// decision JSON.
//
// Constructed without calling Daemon.Start() to sidestep the known
// port-7734 bind flake when -race tests share the default daemon HTTP
// port; the spawner is wired directly and the same Started listener
// Daemon.Start would install is registered manually. Mirrors the
// pattern Phase 4's TestHandleWorkareas_List_IncludesSpawnerLivePool
// uses for the same reason.
func TestHandleExplainRouting_LiveSessionEndToEnd(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath:       "/dev/null",
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		ProviderRegistry: &fakeProviderRegistry{names: []string{"claude", "codex"}},
	})
	d.state.Store(StateRunning)

	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{
			ID:         "smoke-alpha",
			Repository: "https://github.com/foo/rensei-smokes-alpha",
		}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	d.spawner = spawner
	t.Cleanup(func() { _ = spawner.Drain(time.Second) })

	// Same listener body Daemon.Start installs.
	spawner.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventStarted || d.routingTraces == nil {
			return
		}
		d.recordOSSRoutingDecision(ev.Spec.SessionID)
	})

	if _, err := spawner.AcceptWork(SessionSpec{
		SessionID:  "sess-e2e-routing",
		Repository: "smoke-alpha",
		Ref:        "feat/routing",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	// Listener fires synchronously from the spawn goroutine before
	// AcceptWork returns the handle, so the recording is visible
	// immediately. Brief poll for resilience against future ordering
	// tweaks.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := d.RoutingTraces().Explain("sess-e2e-routing"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	srv := &Server{daemon: d}
	mux := http.NewServeMux()
	srv.register(mux)
	hsrv := httptest.NewServer(mux)
	t.Cleanup(hsrv.Close)

	resp, err := http.Get(hsrv.URL + "/api/daemon/routing/explain/sess-e2e-routing")
	if err != nil {
		t.Fatalf("GET explain: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s, want 200", resp.StatusCode, body)
	}
	var explain afclient.RoutingExplainResponse
	if err := json.NewDecoder(resp.Body).Decode(&explain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if explain.SessionID != "sess-e2e-routing" {
		t.Errorf("SessionID = %q, want sess-e2e-routing", explain.SessionID)
	}
	if explain.Decision.ChosenSandbox != "local" {
		t.Errorf("Decision.ChosenSandbox = %q, want local", explain.Decision.ChosenSandbox)
	}
	if explain.Decision.ChosenLLM != "claude" {
		t.Errorf("Decision.ChosenLLM = %q, want claude (first registry name)", explain.Decision.ChosenLLM)
	}
	if explain.Decision.DecidedAt.IsZero() {
		t.Error("Decision.DecidedAt is zero, want now()")
	}
	if len(explain.Trace) != 1 {
		t.Fatalf("len(Trace) = %d, want 1", len(explain.Trace))
	}
	if explain.Trace[0].Phase != "capability-filter" {
		t.Errorf("Trace[0].Phase = %q, want capability-filter", explain.Trace[0].Phase)
	}
	if explain.Trace[0].Note == "" {
		t.Error("Trace[0].Note empty, want OSS-only rationale string")
	}
}

// TestRoutingRoutes_RegisteredOnRealServer pins that the new routes are
// wired into register() — sanity check against drift.
func TestRoutingRoutes_RegisteredOnRealServer(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	d := &Daemon{routingTraces: NewRoutingTraceStore(0)}
	d.state.Store(StateRunning)
	srv := &Server{daemon: d}
	srv.register(mux)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// /api/daemon/routing/config — 200
	res, err := http.Get(ts.URL + "/api/daemon/routing/config")
	if err != nil {
		t.Fatalf("GET config: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("config status = %d, want 200", res.StatusCode)
	}

	// /api/daemon/routing/explain/<id> — 404 on unknown
	res, err = http.Get(ts.URL + "/api/daemon/routing/explain/no-such-session")
	if err != nil {
		t.Fatalf("GET explain: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("explain status = %d, body=%s, want 404", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "routing decision not found") {
		t.Errorf("explain body = %q, want canonical error message", body)
	}
}
