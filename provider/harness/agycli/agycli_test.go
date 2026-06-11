package agycli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestNew_ProbeSuccess(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		Binary:   "agy",
		LookPath: func(string) (string, error) { return "/fake/bin/agy", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.binary != "/fake/bin/agy" {
		t.Errorf("binary = %q", p.binary)
	}
	if !p.injectEnvelope {
		t.Errorf("injectEnvelope should default true")
	}
	if !p.enrichTranscript {
		t.Errorf("enrichTranscript should default true")
	}
	if p.trustWorkspace {
		t.Errorf("trustWorkspace should default FALSE (no surprise global-config mutation)")
	}
}

func TestNew_ProbeFailureWrapsUnavailable(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		LookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
	})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
}

func TestNew_TogglesDisable(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		LookPath:                    func(string) (string, error) { return "/fake/agy", nil },
		DisableResultEnvelope:       true,
		DisableTranscriptEnrichment: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.injectEnvelope {
		t.Errorf("DisableResultEnvelope not honored")
	}
	if p.enrichTranscript {
		t.Errorf("DisableTranscriptEnrichment not honored")
	}
}

func TestName(t *testing.T) {
	t.Parallel()
	p := &Provider{}
	if p.Name() != ProviderName || ProviderName != "agy-cli" {
		t.Errorf("Name = %q, want agy-cli", p.Name())
	}
	if ProviderName != agent.ProviderAGYCLI {
		t.Errorf("package ProviderName %q != agent.ProviderAGYCLI %q", ProviderName, agent.ProviderAGYCLI)
	}
}

func TestCapabilities_Conservative(t *testing.T) {
	t.Parallel()
	c := (&Provider{}).Capabilities()
	if c.SupportsMessageInjection || c.SupportsSessionResume || c.AcceptsMcpServerSpec || c.SupportsReasoningEffort {
		t.Errorf("agy-cli capabilities should all be conservative-false: %+v", c)
	}
	if c.HumanLabel == "" {
		t.Errorf("HumanLabel must be set")
	}
}

func TestResume_Unsupported(t *testing.T) {
	t.Parallel()
	_, err := (&Provider{}).Resume(context.Background(), "sid", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Resume should be ErrUnsupported, got %v", err)
	}
}

func TestShutdown_NoOp(t *testing.T) {
	t.Parallel()
	if err := (&Provider{}).Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
}
