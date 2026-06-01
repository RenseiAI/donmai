package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// namedFakeProvider is a minimal agent.Provider whose Name() is configurable,
// so registry candidate-set construction can be exercised under arbitrary
// provider names (the real stub provider is hard-coded to ProviderStub).
type namedFakeProvider struct{ name agent.ProviderName }

func (p namedFakeProvider) Name() agent.ProviderName         { return p.name }
func (p namedFakeProvider) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (p namedFakeProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (p namedFakeProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (p namedFakeProvider) Shutdown(context.Context) error { return nil }

// selectRunner builds a Runner with the given http client and a registry whose
// registered provider names are `names`.
func selectRunner(t *testing.T, client *http.Client, names ...string) *Runner {
	t.Helper()
	reg := NewRegistry()
	for _, n := range names {
		if err := reg.Register(namedFakeProvider{name: agent.ProviderName(n)}); err != nil {
			t.Fatalf("Register(%q): %v", n, err)
		}
	}
	return &Runner{
		registry:   reg,
		httpClient: client,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func selectWork(srvURL string) QueuedWork {
	qw := QueuedWork{}
	qw.SessionID = "sess-1"
	qw.WorkType = "development"
	qw.PlatformURL = srvURL
	qw.AuthToken = "tok"
	qw.ResolvedProfile.Provider = agent.ProviderClaude
	return qw
}

func TestSelectProviderByPosterior_FlagOff_NoPost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	r := selectRunner(t, srv.Client(), "claude", "codex")

	// Flag unset == OFF.
	t.Run("unset", func(t *testing.T) {
		name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
		if ok || name != "" {
			t.Fatalf("flag-off must not override: got (%q,%v)", name, ok)
		}
	})
	t.Run("explicitly false", func(t *testing.T) {
		t.Setenv("ROUTING_SELECTOR_ENABLED", "false")
		name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
		if ok || name != "" {
			t.Fatalf("flag-off must not override: got (%q,%v)", name, ok)
		}
	})

	if hits.Load() != 0 {
		t.Fatalf("flag-off must POST nothing, got %d POSTs", hits.Load())
	}
}

func TestSelectProviderByPosterior_FlagOn_Overrides(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody routingSelectRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		_ = json.NewDecoder(req.Body).Decode(&gotBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"selectedProvider":"codex","source":"mab-routing"}`))
	}))
	defer srv.Close()
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	r := selectRunner(t, srv.Client(), "claude", "codex")

	name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
	if !ok || name != "codex" {
		t.Fatalf("expected override codex, got (%q,%v)", name, ok)
	}
	if gotPath != "/api/sessions/sess-1/routing-select" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.WorkType != "development" {
		t.Errorf("workType = %q", gotBody.WorkType)
	}
	// stub is filtered out; claude + codex are real candidates.
	if len(gotBody.Candidates) != 2 {
		t.Errorf("candidates = %v, want [claude codex]", gotBody.Candidates)
	}
}

func TestSelectProviderByPosterior_FiltersStubCandidate(t *testing.T) {
	var gotBody routingSelectRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&gotBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"selectedProvider":"claude","source":"mab-routing"}`))
	}))
	defer srv.Close()
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	// Registry has stub + claude; stub must be excluded from candidates.
	r := selectRunner(t, srv.Client(), "stub", "claude")

	_, _ = r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
	for _, c := range gotBody.Candidates {
		if c == "stub" {
			t.Fatalf("stub must be filtered from candidates, got %v", gotBody.Candidates)
		}
	}
	if len(gotBody.Candidates) != 1 || gotBody.Candidates[0] != "claude" {
		t.Errorf("candidates = %v, want [claude]", gotBody.Candidates)
	}
}

func TestSelectProviderByPosterior_NotInCandidates_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		// amp is not registered as a candidate.
		_, _ = w.Write([]byte(`{"selectedProvider":"amp","source":"mab-routing"}`))
	}))
	defer srv.Close()
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	r := selectRunner(t, srv.Client(), "claude", "codex")

	name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
	if ok || name != "" {
		t.Fatalf("provider not in candidates must fall back, got (%q,%v)", name, ok)
	}
}

func TestSelectProviderByPosterior_NullProvider_Fallback(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled", `{"selectedProvider":null,"source":"disabled"}`},
		{"explicit", `{"selectedProvider":null,"source":"explicit"}`},
		{"fallback", `{"selectedProvider":null,"source":"fallback"}`},
		{"empty string", `{"selectedProvider":"","source":"fallback"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
			r := selectRunner(t, srv.Client(), "claude", "codex")
			name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
			if ok || name != "" {
				t.Fatalf("%s must fall back, got (%q,%v)", tc.name, name, ok)
			}
		})
	}
}

func TestSelectProviderByPosterior_ProxyError_Fallback(t *testing.T) {
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")

	t.Run("5xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		r := selectRunner(t, srv.Client(), "claude", "codex")
		name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
		if ok || name != "" {
			t.Fatalf("5xx must fall back, got (%q,%v)", name, ok)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		// A closed server's URL → dial refused.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		client := srv.Client()
		srv.Close()
		r := selectRunner(t, client, "claude", "codex")
		name, ok := r.selectProviderByPosterior(context.Background(), selectWork(url))
		if ok || name != "" {
			t.Fatalf("refused must fall back, got (%q,%v)", name, ok)
		}
	})
}

func TestSelectProviderByPosterior_MissingCoordinates_NoPost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"selectedProvider":"codex","source":"mab-routing"}`))
	}))
	defer srv.Close()
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	r := selectRunner(t, srv.Client(), "claude", "codex")

	cases := map[string]func(*QueuedWork){
		"no sessionId":   func(q *QueuedWork) { q.SessionID = "" },
		"no platformURL": func(q *QueuedWork) { q.PlatformURL = "" },
		"no authToken":   func(q *QueuedWork) { q.AuthToken = "" },
		"no workType":    func(q *QueuedWork) { q.WorkType = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			qw := selectWork(srv.URL)
			mut(&qw)
			n, ok := r.selectProviderByPosterior(context.Background(), qw)
			if ok || n != "" {
				t.Fatalf("missing coordinate must no-op, got (%q,%v)", n, ok)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("missing coordinates must POST nothing, got %d", hits.Load())
	}
}

func TestSelectProviderByPosterior_NoCandidates_NoPost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	// Only stub registered → filtered out → no candidates.
	r := selectRunner(t, srv.Client(), "stub")
	name, ok := r.selectProviderByPosterior(context.Background(), selectWork(srv.URL))
	if ok || name != "" {
		t.Fatalf("no candidates must no-op, got (%q,%v)", name, ok)
	}
	if hits.Load() != 0 {
		t.Fatalf("no candidates must POST nothing, got %d", hits.Load())
	}
}
