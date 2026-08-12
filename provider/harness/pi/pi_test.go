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

// TestConformance_EventContract wires the pi harness into the shared
// cross-harness conformance suite's full composite (ADR-C row 6): a drained
// pi session must satisfy every pure event-sequence invariant —
// CheckSingleInit, CheckTerminalContract, and CheckCompleteAssistantTexts —
// not the terminal-ordering rule alone. The fixture below exercises all
// three: get_state resolves exactly one InitEvent (first), message_update/
// message_end buffer into one complete AssistantTextEvent, and agent_settled
// is the sole terminal event (last).
func TestConformance_EventContract(t *testing.T) {
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
	if err := conformance.CheckEventContract(evs); err != nil {
		t.Errorf("pi session violates the event contract: %v", err)
	}
}

// TestResume_DrivesGetEntriesCursorNotAFreshPrompt is the "replay/resume"
// fixture (ADR-2026-08-06 D8, pi row): Resume must select the persisted
// session on the CLI (`--session <id>`, asserted directly against rpcArgs
// below) and replay from the caller's cursor over `get_entries since=<id>`
// (design §4) — never a fresh `prompt` command, which would start a new turn
// instead of continuing the old one. The resumed stream must still satisfy
// the full event contract (single init, one terminal, closed channel).
func TestResume_DrivesGetEntriesCursorNotAFreshPrompt(t *testing.T) {
	t.Parallel()

	const resumeCursor = "ses_original_cursor"
	layout := sessionLayout{root: "/session", extension: "/session/policy.ts"}
	if got, want := rpcArgs(layout, []string{layout.extension}, launchResume, resumeCursor, agent.Spec{}), "--session"; !argvContains(got, want) {
		t.Fatalf("rpcArgs(resume) = %#v, want it to include %q", got, want)
	}

	body := getStateResponse("ses_resumed") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	cmds, h, err := resumeScripted(t, resumeCursor, agent.Spec{Prompt: "continue"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evs := drain(t, h)
	if err := conformance.CheckEventContract(evs); err != nil {
		t.Errorf("resumed pi session violates the event contract: %v", err)
	}

	var sawGetEntries bool
	for _, cmd := range cmds.commands() {
		switch cmd["type"] {
		case "get_entries":
			sawGetEntries = true
			if cmd["since"] != resumeCursor {
				t.Errorf(`get_entries "since" = %v, want the resume cursor %q`, cmd["since"], resumeCursor)
			}
		case "prompt":
			t.Errorf("Resume must not send a fresh prompt command (would start a new turn instead of continuing the old one)")
		}
	}
	if !sawGetEntries {
		t.Fatalf("Resume never sent get_entries; the replay cursor was never driven onto the wire")
	}
}

func argvContains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
