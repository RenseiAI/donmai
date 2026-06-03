package geminicli

import (
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestNew_BinaryNotFound verifies that New returns a wrapped
// agent.ErrProviderUnavailable when the gemini binary is not on PATH.
func TestNew_BinaryNotFound(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Binary:   "gemini",
		LookPath: func(_ string) (string, error) { return "", errors.New("not found") },
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("error %v does not wrap ErrProviderUnavailable", err)
	}
}

// TestNew_BinaryFound verifies that New succeeds when the binary is found.
func TestNew_BinaryFound(t *testing.T) {
	t.Parallel()

	p, err := New(Options{
		Binary:   "gemini",
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestProvider_Name verifies the ProviderName constant.
func TestProvider_Name(t *testing.T) {
	t.Parallel()

	p, _ := New(Options{
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	if p.Name() != ProviderName {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderName)
	}
	if p.Name() != "gemini-cli" {
		t.Errorf("Name() = %q, want gemini-cli", p.Name())
	}
}

// TestProvider_Capabilities verifies the capability matrix shape.
func TestProvider_Capabilities(t *testing.T) {
	t.Parallel()

	p, _ := New(Options{
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	caps := p.Capabilities()

	// Core claims.
	if caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection should be false (gemini --resume uses indexes)")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume should be false")
	}
	if !caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins should be true (native MCP client)")
	}
	if !caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec should be true (via .gemini/settings.json)")
	}
	if caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList should be false (--allowed-tools deprecated in v0.44)")
	}
	if caps.HumanLabel != "Gemini CLI" {
		t.Errorf("HumanLabel = %q, want Gemini CLI", caps.HumanLabel)
	}
}

// TestProvider_Resume_Unsupported verifies Resume returns ErrUnsupported.
func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()

	p, _ := New(Options{
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	_, err := p.Resume(t.Context(), "some-session-id", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Resume error %v does not wrap ErrUnsupported", err)
	}
}

// TestProvider_Shutdown_NoOp verifies Shutdown returns nil (no-op).
func TestProvider_Shutdown_NoOp(t *testing.T) {
	t.Parallel()

	p, _ := New(Options{
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	if err := p.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown error: %v", err)
	}
}
