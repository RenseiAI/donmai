package pi

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestCodeIntelEnforcementNote_UnsetReturnsNil pins the non-note half of the
// contract: no CodeIntelEnforcement configured, no note (nothing to name).
func TestCodeIntelEnforcementNote_UnsetReturnsNil(t *testing.T) {
	t.Parallel()
	if note := codeIntelEnforcementNote(agent.Spec{}); note != nil {
		t.Errorf("codeIntelEnforcementNote(unset) = %+v, want nil", note)
	}
}

// TestCodeIntelEnforcementNote_SetNamesTheField pins the typed half: a set
// CodeIntelEnforcement returns a SpecFieldNote naming the field, not a nil
// (the silent-drop shape this codex-pattern note replaces).
func TestCodeIntelEnforcementNote_SetNamesTheField(t *testing.T) {
	t.Parallel()
	note := codeIntelEnforcementNote(agent.Spec{
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
	})
	if note == nil {
		t.Fatal("codeIntelEnforcementNote(set) = nil, want a typed note")
	}
	if note.Field != "CodeIntelEnforcement" {
		t.Errorf("note.Field = %q, want %q", note.Field, "CodeIntelEnforcement")
	}
	if note.Reason == "" {
		t.Error("note.Reason is empty; a note with no reason is not an improvement on silence")
	}
}

// TestSpawn_CodeIntelEnforcementDeniedPreTurn is the live-session half of the
// same claim: a Spec carrying CodeIntelEnforcement must surface the typed
// denial on the event stream, before the prompt command reaches the wire —
// the "pre-spawn" half of "typed pre-spawn denial". A caller draining events
// for this session sees the drop; nothing here silently proceeds as if the
// field had been honored.
func TestSpawn_CodeIntelEnforcementDeniedPreTurn(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_ci") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	cmds, h, err := spawnScripted(t, agent.Spec{
		Prompt:               "hi",
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
	}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	var found bool
	for _, ev := range evs {
		sys, ok := ev.(agent.SystemEvent)
		if !ok {
			continue
		}
		if sys.Subtype == codeIntelEnforcementUnsupportedSubtype {
			found = true
			if sys.Message == "" {
				t.Errorf("code-intel-enforcement SystemEvent carries no Message")
			}
		}
	}
	if !found {
		t.Fatalf("no %q SystemEvent in the drained stream (%d events); CodeIntelEnforcement was silently dropped", codeIntelEnforcementUnsupportedSubtype, len(evs))
	}

	// "Pre-spawn" means the notice is decided and emitted before the turn is
	// dispatched, not merely before the caller happens to notice: launch()
	// calls h.emit for this note before it writes the "prompt" wire command,
	// so the prompt command must still have gone out (the field is a denial
	// of ITSELF, never of the session).
	var sawPrompt bool
	for _, cmd := range cmds.commands() {
		if cmd["type"] == "prompt" {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("prompt command never sent; CodeIntelEnforcement must not block the session, only its own enforcement")
	}
}
