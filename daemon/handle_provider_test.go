package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// stubProviderRegistry is a minimal in-memory ProviderRegistry for handler
// tests. Keeps the daemon test package free of a runner import.
type stubProviderRegistry struct {
	caps map[string]map[string]any
}

func (s *stubProviderRegistry) Names() []string {
	out := make([]string, 0, len(s.caps))
	for n := range s.caps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *stubProviderRegistry) Capabilities(name string) (map[string]any, bool) {
	c, ok := s.caps[name]
	return c, ok
}

// newProviderTestServer builds a Server backed by a Daemon whose
// ProviderRegistry is the supplied stub. The Server is exposed through
// a freshly registered http.ServeMux so handler tests skip the
// listen/serve goroutine — they call ServeHTTP directly.
func newProviderTestServer(t *testing.T, reg ProviderRegistry) *httptest.Server {
	t.Helper()
	d := &Daemon{
		opts: Options{ProviderRegistry: reg},
	}
	s := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/providers", s.method(http.MethodGet, s.handleListProviders))
	mux.HandleFunc("/api/daemon/providers/", s.handleGetProvider)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleListProviders_PartialCoverageEvenWhenEmpty(t *testing.T) {
	// No registry wired — endpoint must still emit the honesty marker.
	srv := newProviderTestServer(t, nil)
	res, err := http.Get(srv.URL + "/api/daemon/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var resp afclient.ListProvidersResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.PartialCoverage {
		t.Errorf("PartialCoverage = false, want true")
	}
	if len(resp.CoveredFamilies) != 1 || resp.CoveredFamilies[0] != afclient.FamilyAgentRuntime {
		t.Errorf("CoveredFamilies = %v, want [agent-runtime]", resp.CoveredFamilies)
	}
	if len(resp.Providers) != 0 {
		t.Errorf("Providers = %+v, want empty", resp.Providers)
	}
}

func TestHandleListProviders_EmitsRegisteredRuntimes(t *testing.T) {
	reg := &stubProviderRegistry{
		caps: map[string]map[string]any{
			"claude": {
				"supportsMessageInjection": false,
				"supportsSessionResume":    true,
			},
			"codex": {
				"needsBaseInstructions": true,
			},
		},
	}
	srv := newProviderTestServer(t, reg)
	res, err := http.Get(srv.URL + "/api/daemon/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var resp afclient.ListProvidersResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.PartialCoverage {
		t.Errorf("PartialCoverage = false, want true")
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("Providers count = %d, want 2", len(resp.Providers))
	}
	for _, p := range resp.Providers {
		if p.Family != afclient.FamilyAgentRuntime {
			t.Errorf("provider %q family = %q, want agent-runtime", p.ID, p.Family)
		}
		if p.Status != afclient.StatusReady {
			t.Errorf("provider %q status = %q, want ready", p.ID, p.Status)
		}
		if p.Source != afclient.SourceBundled {
			t.Errorf("provider %q source = %q, want bundled", p.ID, p.Source)
		}
	}
}

func TestHandleListProviders_RejectsNonGet(t *testing.T) {
	srv := newProviderTestServer(t, nil)
	res, err := http.Post(srv.URL+"/api/daemon/providers", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

func TestHandleGetProvider_HappyPath(t *testing.T) {
	reg := &stubProviderRegistry{
		caps: map[string]map[string]any{
			"claude": {"supportsSessionResume": true},
		},
	}
	srv := newProviderTestServer(t, reg)
	res, err := http.Get(srv.URL + "/api/daemon/providers/claude")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var env afclient.ProviderEnvelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Provider.ID != "claude" {
		t.Errorf("ID = %q, want claude", env.Provider.ID)
	}
	if env.Provider.Family != afclient.FamilyAgentRuntime {
		t.Errorf("Family = %q, want agent-runtime", env.Provider.Family)
	}
	if env.Provider.Capabilities["supportsSessionResume"] != true {
		t.Errorf("Capabilities[supportsSessionResume] = %v, want true",
			env.Provider.Capabilities["supportsSessionResume"])
	}
}

func TestHandleGetProvider_UnknownIDReturns404(t *testing.T) {
	reg := &stubProviderRegistry{caps: map[string]map[string]any{"claude": {}}}
	srv := newProviderTestServer(t, reg)
	res, err := http.Get(srv.URL + "/api/daemon/providers/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("error envelope missing 'error' key: %v", body)
	}
	if body["providerId"] != "nope" {
		t.Errorf("providerId = %q, want nope", body["providerId"])
	}
}

func TestHandleGetProvider_NoRegistry404(t *testing.T) {
	srv := newProviderTestServer(t, nil)
	res, err := http.Get(srv.URL + "/api/daemon/providers/claude")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestHandleGetProvider_RejectsNonGet(t *testing.T) {
	reg := &stubProviderRegistry{caps: map[string]map[string]any{"claude": {}}}
	srv := newProviderTestServer(t, reg)
	res, err := http.Post(srv.URL+"/api/daemon/providers/claude", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

func TestHandleGetProvider_RejectsTrailingSegments(t *testing.T) {
	reg := &stubProviderRegistry{caps: map[string]map[string]any{"claude": {}}}
	srv := newProviderTestServer(t, reg)
	// "/api/daemon/providers/claude/extra" — must 404, not match claude.
	res, err := http.Get(srv.URL + "/api/daemon/providers/claude/extra")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestHandleGetProvider_EmptyIDReturns404(t *testing.T) {
	reg := &stubProviderRegistry{caps: map[string]map[string]any{"claude": {}}}
	srv := newProviderTestServer(t, reg)
	// "/api/daemon/providers/" with trailing slash and nothing after.
	res, err := http.Get(srv.URL + "/api/daemon/providers/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}
