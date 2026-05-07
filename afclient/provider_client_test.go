package afclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newDaemonTestServer wires an httptest.Server and a DaemonClient pointed
// at it. Mirrors newTestServer in client_test.go but for the daemon
// surface.
func newDaemonTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *DaemonClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, NewDaemonClientFromURL(srv.URL)
}

func TestDaemonClientListProviders_HappyPath(t *testing.T) {
	want := ListProvidersResponse{
		Providers: []Provider{
			{
				ID:      "claude",
				Name:    "claude",
				Version: "0.5.5",
				Family:  FamilyAgentRuntime,
				Scope:   ScopeGlobal,
				Status:  StatusReady,
				Source:  SourceBundled,
				Trust:   TrustUnsigned,
				Capabilities: map[string]any{
					"supportsMessageInjection": false,
					"supportsSessionResume":    true,
				},
			},
		},
		PartialCoverage: true,
		CoveredFamilies: []ProviderFamily{FamilyAgentRuntime},
	}
	_, c := newDaemonTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/daemon/providers" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// Daemon never sends Authorization headers; verify the client
		// honours the localhost-only policy from
		// ADR-2026-05-07-daemon-http-control-api.md §D2.
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header set: %q (must be empty for daemon)", got)
		}
		_ = json.NewEncoder(w).Encode(want)
	})
	got, err := c.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if !got.PartialCoverage {
		t.Errorf("PartialCoverage = false, want true")
	}
	if len(got.CoveredFamilies) != 1 || got.CoveredFamilies[0] != FamilyAgentRuntime {
		t.Errorf("CoveredFamilies = %v, want [agent-runtime]", got.CoveredFamilies)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "claude" {
		t.Errorf("Providers = %+v", got.Providers)
	}
}

func TestDaemonClientListProviders_ServerError(t *testing.T) {
	_, c := newDaemonTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.ListProviders()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError", err)
	}
}

func TestDaemonClientListProviders_NetworkError(t *testing.T) {
	// Construct a client pointed at a closed server to force a transport
	// error.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	c := NewDaemonClientFromURL(srv.URL)
	srv.Close()
	_, err := c.ListProviders()
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
	if !strings.Contains(err.Error(), "list providers") {
		t.Errorf("err missing context wrapping: %v", err)
	}
}

func TestDaemonClientGetProvider_HappyPath(t *testing.T) {
	want := ProviderEnvelope{
		Provider: Provider{
			ID:      "codex",
			Name:    "codex",
			Version: "0.5.5",
			Family:  FamilyAgentRuntime,
			Scope:   ScopeGlobal,
			Status:  StatusReady,
			Source:  SourceBundled,
			Trust:   TrustUnsigned,
			Capabilities: map[string]any{
				"needsBaseInstructions": true,
			},
		},
	}
	_, c := newDaemonTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/daemon/providers/codex" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(want)
	})
	got, err := c.GetProvider("codex")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Provider.ID != "codex" {
		t.Errorf("ID = %q", got.Provider.ID)
	}
	if got.Provider.Capabilities["needsBaseInstructions"] != true {
		t.Errorf("Capabilities[needsBaseInstructions] = %v, want true",
			got.Provider.Capabilities["needsBaseInstructions"])
	}
}

func TestDaemonClientGetProvider_NotFound(t *testing.T) {
	_, c := newDaemonTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"provider not found","providerId":"nope"}`))
	})
	_, err := c.GetProvider("nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDaemonClientGetProvider_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	c := NewDaemonClientFromURL(srv.URL)
	srv.Close()
	_, err := c.GetProvider("claude")
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
	if !strings.Contains(err.Error(), "get provider") {
		t.Errorf("err missing context wrapping: %v", err)
	}
}
