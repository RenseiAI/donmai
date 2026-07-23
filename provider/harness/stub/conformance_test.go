package stub

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestRoundtrip_ConformsToTerminalEventContract wires the shared
// cross-harness event-contract conformance check (agent/conformance,
// seeded by donmai PR #199 for opencode) onto the stub harness: the
// canonical successful sequence (Test_Roundtrip_SucceedWithPR) must
// emit exactly one terminal event (ResultEvent/ErrorEvent), and it
// must be the last event on the channel — the same D-1-class invariant
// opencode's adapter once violated (ADR-2026-06-06 D6 / ADR-C row 6;
// runs/2026-07-21-open-harness-strategy/12-work-breakdown.md W0 item 2).
func TestRoundtrip_ConformsToTerminalEventContract(t *testing.T) {
	t.Parallel()
	p, err := New(WithSessionIDFunc(func() string { return "stub-conformance-test" }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorSucceedWithPR)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var events []agent.Event
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
loop:
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				break loop
			}
			events = append(events, ev)
		case <-deadline.C:
			t.Fatalf("timed out waiting for events; collected %d so far", len(events))
		}
	}

	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal-event contract violated: %v\nevents: %v", err, eventKinds(events))
	}
}
