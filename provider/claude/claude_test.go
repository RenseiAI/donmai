package claude

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()

	p, err := New(Options{
		Binary:   "claude-fake",
		LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
	})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("New: returned nil provider")
	}
	if got, want := p.Name(), agent.ProviderClaude; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if p.binary != "/usr/local/bin/claude-fake" {
		t.Errorf("binary = %q, want resolved path", p.binary)
	}
}

func TestNew_DefaultBinary(t *testing.T) {
	t.Parallel()

	var probed string
	_, err := New(Options{
		LookPath: func(name string) (string, error) {
			probed = name
			return "/x", nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if probed != DefaultBinary {
		t.Errorf("probed = %q, want %q", probed, DefaultBinary)
	}
}

func TestNew_BinaryNotFound(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Binary:   "claude-does-not-exist",
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	})
	if err == nil {
		t.Fatal("New: expected error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("New: error %v should wrap ErrProviderUnavailable", err)
	}
	// Sanity: error message names the binary so operators can fix.
	if !strings.Contains(err.Error(), "claude-does-not-exist") {
		t.Errorf("error message should reference binary name: %v", err)
	}
}

func TestProvider_Capabilities_v0_5_0(t *testing.T) {
	t.Parallel()

	p, err := New(Options{LookPath: func(string) (string, error) { return "/x", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := p.Capabilities()

	// Locked v0.5.0 capability matrix per F.1.1 §3.1
	// (post-F.2.3-cap-flip: SupportsMessageInjection flipped to true).
	if !caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection should be true in v0.5.0 (between-turn injection via --resume)")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume should be false in v0.5.0 (option C lands in v0.5.+)")
	}
	if caps.SupportsCodeIntelligenceEnforcement {
		t.Error("SupportsCodeIntelligenceEnforcement should be false in v0.5.0 (no canUseTool callback)")
	}
	if !caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins should be true")
	}
	if !caps.EmitsSubagentEvents {
		t.Error("EmitsSubagentEvents should be true")
	}
	if !caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort should be true")
	}
	if caps.NeedsBaseInstructions {
		t.Error("NeedsBaseInstructions should be false")
	}
	if caps.NeedsPermissionConfig {
		t.Error("NeedsPermissionConfig should be false")
	}
	if caps.ToolPermissionFormat != "claude" {
		t.Errorf("ToolPermissionFormat = %q, want %q", caps.ToolPermissionFormat, "claude")
	}
	if caps.HumanLabel != "Claude" {
		t.Errorf("HumanLabel = %q, want %q", caps.HumanLabel, "Claude")
	}
}

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()

	p, err := New(Options{LookPath: func(string) (string, error) { return "/x", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Resume(t.Context(), "any-session-id", agent.Spec{})
	if h != nil {
		t.Error("Resume: handle should be nil")
	}
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Resume: error %v should wrap ErrUnsupported", err)
	}
}

func TestProvider_Shutdown_NoOp(t *testing.T) {
	t.Parallel()

	p, err := New(Options{LookPath: func(string) (string, error) { return "/x", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: unexpected error: %v", err)
	}
}

// Compile-time assertions: Provider satisfies agent.Provider, Handle
// satisfies agent.Handle.
var (
	_ agent.Provider = (*Provider)(nil)
	_ agent.Handle   = (*Handle)(nil)
)
