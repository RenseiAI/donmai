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
// not the terminal-ordering rule alone. The base fixture exercises all
// three: get_state resolves exactly one InitEvent (first), message_update/
// message_end buffer into one complete AssistantTextEvent, and agent_settled
// is the sole terminal event (last).
//
// The table also covers every launch-time notice the harness decides before
// the child's first event (the per-call timeout bound on every session, plus
// the unverified-version label and the code-intel-enforcement denial when
// they apply), on Spawn and Resume and whichever of get_state/agent_start
// resolves the InitEvent: none of them may precede the InitEvent, and each
// must sit directly behind it, before any turn output.
func TestConformance_EventContract(t *testing.T) {
	t.Parallel()
	textTurn := event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "done"}}) +
		event(map[string]any{"type": "message_end"})
	cases := []struct {
		name        string
		launch      scriptedLaunch
		wantNotices []string
	}{
		{
			name: "spawn",
			launch: scriptedLaunch{
				spec: agent.Spec{Prompt: "hi"},
				body: getStateResponse("ses_conf") +
					event(map[string]any{"type": "agent_start"}) +
					textTurn +
					event(map[string]any{"type": "agent_settled"}),
			},
			wantNotices: []string{agent.SystemSubtypeToolCallBounds},
		},
		{
			name: "spawn, init resolved by agent_start before get_state",
			launch: scriptedLaunch{
				spec: agent.Spec{Prompt: "hi"},
				body: event(map[string]any{"type": "agent_start"}) +
					textTurn +
					getStateResponse("ses_conf_late") +
					event(map[string]any{"type": "agent_settled"}),
			},
			wantNotices: []string{agent.SystemSubtypeToolCallBounds},
		},
		{
			name: "spawn, every launch notice",
			launch: scriptedLaunch{
				spec: agent.Spec{
					Prompt:               "hi",
					CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
				},
				configure: func(p *Provider) { p.unverified = true },
				body: getStateResponse("ses_conf_notices") +
					event(map[string]any{"type": "agent_start"}) +
					textTurn +
					event(map[string]any{"type": "agent_settled"}),
			},
			wantNotices: []string{
				unverifiedVersionSubtype,
				codeIntelEnforcementUnsupportedSubtype,
				agent.SystemSubtypeToolCallBounds,
			},
		},
		{
			name: "resume",
			launch: scriptedLaunch{
				spec:            agent.Spec{Prompt: "continue"},
				resumeSessionID: "ses_conf_cursor",
				body: getStateResponse("ses_conf_resumed") +
					event(map[string]any{"type": "agent_start"}) +
					event(map[string]any{"type": "agent_settled"}),
			},
			wantNotices: []string{agent.SystemSubtypeToolCallBounds},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sl := tc.launch
			sl.preSpawn = handshakeEvent("h1")
			_, h, _, err := launchScripted(t, sl)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			evs := drain(t, h)
			if err := conformance.CheckEventContract(evs); err != nil {
				t.Errorf("pi session violates the event contract: %v", err)
			}
			assertLaunchNoticesFollowInit(t, evs, tc.wantNotices)
		})
	}
}

// assertLaunchNoticesFollowInit asserts want names the launch-notice
// SystemEvent subtypes that sit directly behind the stream's first event (the
// InitEvent), in order, and that none of them appears anywhere else. A notice
// that went missing fails here too, so a case cannot pass by never exercising
// the notice it names.
func assertLaunchNoticesFollowInit(t *testing.T, evs []agent.Event, want []string) {
	t.Helper()
	if len(evs) < 1+len(want) {
		t.Fatalf("got %d events; want an InitEvent followed by %d launch notices %v", len(evs), len(want), want)
	}
	for i, subtype := range want {
		se, ok := evs[1+i].(agent.SystemEvent)
		if !ok || se.Subtype != subtype {
			t.Errorf("event %d = %T %+v; want launch notice %q directly behind the InitEvent", 1+i, evs[1+i], evs[1+i], subtype)
		}
	}
	for _, subtype := range want {
		n := 0
		for _, ev := range evs {
			if se, ok := ev.(agent.SystemEvent); ok && se.Subtype == subtype {
				n++
			}
		}
		if n != 1 {
			t.Errorf("launch notice %q appears %d times; want exactly once per session", subtype, n)
		}
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
