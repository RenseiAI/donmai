package codex

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestHandle_ConformsToTerminalEventContract wires the shared
// cross-harness event-contract conformance check (agent/conformance,
// seeded by donmai PR #199 for opencode) onto the codex harness: a
// successful turn against the fake app-server must emit exactly one
// terminal event (ResultEvent/ErrorEvent), and it must be the last
// event on the channel — the same D-1-class invariant opencode's
// adapter once violated (ADR-2026-06-06 D6 / ADR-C row 6;
// runs/2026-07-21-open-harness-strategy/12-work-breakdown.md W0 item 2).
func TestHandle_ConformsToTerminalEventContract(t *testing.T) {
	p, _ := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainEvents(t, h.Events(), 5*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal-event contract violated: %v\nevents: %v", err, kindsOf(events))
	}
}
