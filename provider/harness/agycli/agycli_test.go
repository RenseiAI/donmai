package agycli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestNew_ProbeSuccess(t *testing.T) {
	t.Parallel()
	binary := newHelpOnlyAgy(t, true)
	p, err := New(Options{
		Binary:   binary,
		LookPath: func(string) (string, error) { return binary, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.binary != binary {
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
	binary := newHelpOnlyAgy(t, true)
	p, err := New(Options{
		Binary:                      binary,
		LookPath:                    func(string) (string, error) { return binary, nil },
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

func TestVerifyAddDirSupport_FailsClosed(t *testing.T) {
	t.Parallel()
	err := verifyAddDirSupport("fake-agy", func(context.Context, string) ([]byte, error) {
		return []byte("Usage: fake-agy\n"), nil
	})
	if err == nil {
		t.Fatal("expected missing --add-dir to be rejected")
	}

	err = verifyAddDirSupport("fake-agy", func(context.Context, string) ([]byte, error) {
		return []byte("  --add-dir  Add a directory to the workspace\n"), nil
	})
	if err != nil {
		t.Fatalf("advertised --add-dir should be accepted: %v", err)
	}
}

func newHelpOnlyAgy(t *testing.T, addDir bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agy")
	help := "Usage: fake-agy\\n"
	if addDir {
		help += "  --add-dir  Add a directory to the workspace\\n"
	}
	script := "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then\n  printf '%b' '" + help + "'\nfi\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return path
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
