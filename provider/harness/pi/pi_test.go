package pi

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestProvider_Identity pins the provider/manifest identity and the
// manifest↔Capabilities projection this package must keep in agreement (the
// matrix parity gate depends on it).
func TestProvider_Identity(t *testing.T) {
	t.Parallel()
	p := &Provider{}
	if p.Name() != agent.ProviderPi {
		t.Errorf("Name() = %q, want pi", p.Name())
	}
	mf := p.Manifest()
	if mf.Name != agent.HarnessPi || mf.ContractABI != "harness/v2" {
		t.Errorf("unexpected manifest header: %+v", mf)
	}
	caps := p.Capabilities()
	if caps.SupportsMessageInjection != mf.Caps.SupportsMessageInjection ||
		caps.SupportsSessionResume != mf.Caps.SupportsSessionResume ||
		caps.AcceptsMcpServerSpec != mf.Caps.AcceptsMcpServerSpec ||
		caps.AcceptsAllowedToolsList != mf.Caps.AcceptsAllowedToolsList {
		t.Errorf("Capabilities() disagrees with Manifest().Caps")
	}
	// pi ships no MCP by design; it must not advertise otherwise.
	if mf.Caps.AcceptsMcpServerSpec {
		t.Errorf("pi must not advertise AcceptsMcpServerSpec (no MCP by design)")
	}
}

// TestVersionPin_BelowMinFailsConstruction proves probe-time enforcement: a
// binary confirmed below MinVersion fails New with ErrProviderUnavailable;
// an above/unverifiable version proceeds but is labeled.
func TestVersionPin_BelowMinFailsConstruction(t *testing.T) {
	t.Parallel()
	below := func(_ context.Context, _ string) (string, error) { return "pi 0.1.0", nil }
	if _, err := New(Options{PiBin: "/usr/bin/true", VersionProbe: below}); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("below-min version should fail New with ErrProviderUnavailable, got %v", err)
	}

	above := func(_ context.Context, _ string) (string, error) { return "pi 999.0.0", nil }
	p, err := New(Options{PiBin: "/usr/bin/true", VersionProbe: above})
	if err != nil {
		t.Fatalf("above-verified version should construct (labeled), got %v", err)
	}
	if !p.unverified {
		t.Errorf("above-VerifiedAgainst version should mark the provider unverified")
	}
}

// TestConformance_TerminalContract wires the pi harness into the shared
// cross-harness conformance suite (ADR-C row 6): a drained pi session must
// satisfy the terminal-event ordering invariant.
func TestConformance_TerminalContract(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_conf") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "done"}}) +
		event(map[string]any{"type": "message_end"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)
	if err := conformance.CheckTerminalContract(evs); err != nil {
		t.Errorf("pi session violates the terminal-event contract: %v", err)
	}
}
