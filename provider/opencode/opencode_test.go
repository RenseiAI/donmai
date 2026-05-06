package opencode

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

func TestNew_LiveServer_Succeeds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	p, err := New(Options{Endpoint: srv.URL, Getenv: fakeEnv(nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.endpoint != srv.URL {
		t.Errorf("endpoint: want %q, got %q", srv.URL, p.endpoint)
	}
}

func TestNew_404IsLive(t *testing.T) {
	t.Parallel()
	// OpenCode's pre-1.0 server may not have a / handler; any < 500
	// status counts as alive.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := New(Options{Endpoint: srv.URL, Getenv: fakeEnv(nil)}); err != nil {
		t.Fatalf("New: want nil for 404, got %v", err)
	}
}

func TestNew_5xxFailsAsUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(Options{Endpoint: srv.URL, Getenv: fakeEnv(nil)})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
}

func TestNew_ConnectionRefused(t *testing.T) {
	t.Parallel()
	// Use a port that's almost certainly closed locally.
	_, err := New(Options{
		Endpoint: "http://127.0.0.1:1",
		Getenv:   fakeEnv(nil),
	})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
}

func TestNew_EnvEndpointFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := New(Options{Getenv: fakeEnv(map[string]string{EnvEndpoint: srv.URL})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.endpoint != srv.URL {
		t.Errorf("endpoint: want %q (from env), got %q", srv.URL, p.endpoint)
	}
}

func TestNew_APIKeyForwardedAsBearer(t *testing.T) {
	t.Parallel()
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, err := New(Options{
		Endpoint: srv.URL,
		APIKey:   "secret-token",
		Getenv:   fakeEnv(nil),
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization: want %q, got %q", "Bearer secret-token", gotAuth)
	}
}

func TestNew_SkipProbeBypassesNetwork(t *testing.T) {
	t.Parallel()
	// Endpoint is invalid; SkipProbe makes New still succeed.
	p, err := New(Options{
		Endpoint:  "http://invalid.example.test:9",
		SkipProbe: true,
		Getenv:    fakeEnv(nil),
	})
	if err != nil {
		t.Fatalf("New with SkipProbe: %v", err)
	}
	if p.endpoint != "http://invalid.example.test:9" {
		t.Errorf("endpoint: want preserved, got %q", p.endpoint)
	}
}

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if got := p.Name(); got != agent.ProviderOpenCode {
		t.Fatalf("Name: want %q, got %q", agent.ProviderOpenCode, got)
	}
}

func TestProvider_Capabilities_Conservative(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	caps := p.Capabilities()
	if caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want false (registration-only)")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume: want false (registration-only)")
	}
	if caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want false (registration-only)")
	}
	// Tool-use surface (002 v2): false/false — registration-only.
	if caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want false (registration-only)")
	}
	if caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want false (registration-only)")
	}
	if caps.HumanLabel != "OpenCode" {
		t.Errorf("HumanLabel: want %q, got %q", "OpenCode", caps.HumanLabel)
	}
}

func TestProvider_Spawn_AlwaysFails(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "anything"})
	if h != nil {
		t.Fatal("Spawn: want nil handle for registration-only runner")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want wrapping ErrSpawnFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "opencode runner not yet implemented") {
		t.Fatalf("Spawn err: want diagnostic, got %v", err)
	}
}

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	_, err := p.Resume(context.Background(), "session", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err: want ErrUnsupported, got %v", err)
	}
}

func TestProvider_Shutdown_NoOp(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}
}

func mustNew(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Options{SkipProbe: true, Getenv: fakeEnv(nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}
