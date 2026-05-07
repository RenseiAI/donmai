package afclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httpResponseFunc is a tiny shim so each test can express its server
// behavior inline without spinning up a custom mux.
type httpResponseFunc func(w http.ResponseWriter, r *http.Request)

func newRoutingTestServer(handler httpResponseFunc) (*DaemonClient, func()) {
	ts := httptest.NewServer(http.HandlerFunc(handler))
	return NewDaemonClientFromURL(ts.URL), ts.Close
}

func TestGetRoutingConfig_HappyPath(t *testing.T) {
	t.Parallel()
	want := RoutingConfigResponse{
		Config: RoutingConfig{
			Weights: RoutingWeights{Cost: 0.7, Latency: 0.3},
			SandboxProviders: []SandboxProviderState{
				{ProviderID: "local", Alpha: 1, Beta: 1, SelectionCount: 0},
			},
			LLMProviders: []LLMProviderState{
				{ProviderID: "claude", Alpha: 1, Beta: 1},
			},
			RecentDecisions: []RoutingDecision{
				{
					SessionID:     "sess-a",
					ChosenSandbox: "local",
					ChosenLLM:     "claude",
					Score:         0.18,
					DecidedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
				},
			},
			CapturedAt: time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC),
		},
	}
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/routing/config" {
			t.Errorf("path = %q, want /api/daemon/routing/config", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		// localhost-only auth: no Authorization header per ADR D2.
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("Authorization header should not be set; got %q", h)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	})
	defer cleanup()

	got, err := c.GetRoutingConfig()
	if err != nil {
		t.Fatalf("GetRoutingConfig: %v", err)
	}
	if got.Config.Weights != want.Config.Weights {
		t.Errorf("Weights = %+v, want %+v", got.Config.Weights, want.Config.Weights)
	}
	if len(got.Config.SandboxProviders) != 1 || got.Config.SandboxProviders[0].ProviderID != "local" {
		t.Errorf("SandboxProviders = %+v, want single local row", got.Config.SandboxProviders)
	}
	if len(got.Config.RecentDecisions) != 1 || got.Config.RecentDecisions[0].SessionID != "sess-a" {
		t.Errorf("RecentDecisions = %+v, want sess-a", got.Config.RecentDecisions)
	}
}

func TestGetRoutingConfig_ServerError(t *testing.T) {
	t.Parallel()
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer cleanup()

	if _, err := c.GetRoutingConfig(); err == nil {
		t.Fatalf("GetRoutingConfig: expected error on 500")
	} else if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError-wrapped", err)
	}
}

func TestExplainRouting_HappyPath(t *testing.T) {
	t.Parallel()
	want := RoutingExplainResponse{
		SessionID: "sess-explain-1",
		Decision: RoutingDecision{
			SessionID:     "sess-explain-1",
			ChosenSandbox: "local",
			ChosenLLM:     "claude",
			Score:         0.18,
			DecidedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		},
		Trace: []RoutingTraceStep{
			{Step: 1, Phase: "capability-filter", Dimension: "sandbox", Remaining: []string{"local"}},
			{Step: 2, Phase: "score", Dimension: "sandbox", Remaining: []string{"local"}, Note: "winner"},
		},
	}
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/routing/explain/") {
			t.Errorf("path = %q, want explain prefix", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	})
	defer cleanup()

	got, err := c.ExplainRouting("sess-explain-1")
	if err != nil {
		t.Fatalf("ExplainRouting: %v", err)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if len(got.Trace) != 2 {
		t.Errorf("len(Trace) = %d, want 2", len(got.Trace))
	}
}

func TestExplainRouting_NotFound(t *testing.T) {
	t.Parallel()
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"routing decision not found","sessionId":"unknown"}`))
	})
	defer cleanup()

	_, err := c.ExplainRouting("unknown")
	if err == nil {
		t.Fatalf("ExplainRouting: expected error on 404")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound-wrapped", err)
	}
}

func TestExplainRouting_EmptySessionID(t *testing.T) {
	t.Parallel()
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called when sessionID is empty")
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer cleanup()

	_, err := c.ExplainRouting("")
	if err == nil {
		t.Fatalf("ExplainRouting(\"\"): expected error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestExplainRouting_PathEscape(t *testing.T) {
	t.Parallel()
	// Session IDs may contain characters that require URL-escaping. The
	// raw RequestURI must contain the percent-encoded form (the stdlib
	// transparently decodes r.URL.Path, but RequestURI preserves the
	// wire-form, which is what we want to assert).
	const sessionID = "sess with space/and-slash"
	var observedURI string
	c, cleanup := newRoutingTestServer(func(w http.ResponseWriter, r *http.Request) {
		observedURI = r.RequestURI
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"x","sessionId":"x"}`))
	})
	defer cleanup()

	_, _ = c.ExplainRouting(sessionID)
	if observedURI == "" {
		t.Fatalf("server saw no request")
	}
	// PathEscape encodes space as %20; the literal space MUST NOT
	// appear in the wire-form URI.
	if strings.Contains(observedURI, " ") {
		t.Errorf("RequestURI = %q, contains literal space (must be percent-encoded)", observedURI)
	}
	if !strings.Contains(observedURI, "%20") {
		t.Errorf("RequestURI = %q, want %%20 for space", observedURI)
	}
	// The slash is also escaped — handler-side prefix-strip therefore
	// sees a single segment, which is the correct behaviour for ids
	// like "sess/foo".
	if !strings.Contains(observedURI, "%2F") && !strings.Contains(observedURI, "%2f") {
		t.Errorf("RequestURI = %q, want slash percent-encoded", observedURI)
	}
}
