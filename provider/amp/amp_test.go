package amp

import (
	"context"
	"errors"
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
	if err == nil {
		t.Fatal("expected error when AMP_API_KEY is unset")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), EnvAPIKey) {
		t.Fatalf("err: want %s in message, got %v", EnvAPIKey, err)
	}
}

func TestNew_OptionsKeyWins(t *testing.T) {
	t.Parallel()
	p, err := New(Options{APIKey: "explicit-key", Getenv: fakeEnv(nil)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "explicit-key" {
		t.Fatalf("apiKey: want %q, got %q", "explicit-key", p.apiKey)
	}
}

func TestNew_FallsBackToEnv(t *testing.T) {
	t.Parallel()
	p, err := New(Options{Getenv: fakeEnv(map[string]string{EnvAPIKey: "env-key"})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "env-key" {
		t.Fatalf("apiKey: want %q, got %q", "env-key", p.apiKey)
	}
}

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if got := p.Name(); got != agent.ProviderAmp {
		t.Fatalf("Name: want %q, got %q", agent.ProviderAmp, got)
	}
}

func TestProvider_Capabilities_AllConservative(t *testing.T) {
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
	if caps.EmitsSubagentEvents {
		t.Error("EmitsSubagentEvents: want false (registration-only)")
	}
	if caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort: want false (registration-only)")
	}
	// Tool-use surface (002 v2): false/false — registration-only.
	if caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want false (registration-only)")
	}
	if caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want false (registration-only)")
	}
	if caps.HumanLabel != "Amp" {
		t.Errorf("HumanLabel: want %q, got %q", "Amp", caps.HumanLabel)
	}
}

func TestProvider_Spawn_AlwaysFails(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "anything"})
	if h != nil {
		t.Fatal("Spawn: want nil handle for registration-only runner")
	}
	if err == nil {
		t.Fatal("Spawn: want non-nil error for registration-only runner")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want wrapping ErrSpawnFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "amp runner not yet implemented") {
		t.Fatalf("Spawn err: want diagnostic message, got %v", err)
	}
}

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	_, err := p.Resume(context.Background(), "amp-session-1", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err: want wrapping ErrUnsupported, got %v", err)
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
	p, err := New(Options{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}
