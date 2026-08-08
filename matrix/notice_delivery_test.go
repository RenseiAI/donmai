package matrix

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestEveryHarnessDeclaresNoticeDelivery is the omission guard for the
// notice-delivery axis.
//
// The axis only works if every harness answers. A manifest that leaves the
// field at its zero value has not said "no live delivery" — it has said
// nothing, and the whole point of declaring the mechanism is that nobody
// downstream should have to guess which of those two a blank means. Callers
// therefore treat blank as a refusal, and this test makes sure the refusal
// never happens by accident: a harness added without answering fails here, at
// the registry, rather than in production as a message that goes nowhere.
//
// Note what this does NOT check: whether the declaration is TRUE. No test can
// establish that a third-party CLI really exposes the door its manifest claims
// — that comes from reading its own docs/CLI before writing the constant down,
// and from the runner refusing to drive a channel it has not implemented.
func TestEveryHarnessDeclaresNoticeDelivery(t *testing.T) {
	for _, h := range HarnessHarvestList() {
		t.Run(string(h.Name), func(t *testing.T) {
			got := h.Manifest().Caps.NoticeDelivery
			if !got.Declared() {
				t.Fatalf("harness %q declares NoticeDelivery %q, which is not one of the known mechanisms — "+
					"answer the question on its manifest (agent.NoticeDeliveryNone is a legitimate answer; "+
					"leaving it blank is not)", h.Name, got)
			}
		})
	}
}

// TestPTYNoticeIsDeclaredOnlyWhereNoAgentOwnsTheTerminal is the standing fence
// around the one mechanism that writes bytes at a terminal.
//
// pty-notice is correct precisely when there is no agent behind the PTY to
// route around: the shell harness. For a harness running its own UI the same
// write is a keystroke into whatever is drawn, and the submit byte selects the
// highlighted option — a hazard the terminal layer genuinely cannot observe,
// which is why it is fenced by declaration instead of detection.
//
// A future harness that declares pty-notice has to come here and justify it in
// this list, which is the intended cost.
func TestPTYNoticeIsDeclaredOnlyWhereNoAgentOwnsTheTerminal(t *testing.T) {
	// The harnesses for which a terminal write reaches no agent UI.
	noAgentBehindTheTerminal := map[agent.HarnessName]struct{}{
		agent.HarnessShell: {},
	}

	for _, h := range HarnessHarvestList() {
		manifest := h.Manifest()
		if manifest.Caps.NoticeDelivery != agent.NoticeDeliveryPTYNotice {
			continue
		}
		if _, ok := noAgentBehindTheTerminal[h.Name]; !ok {
			t.Errorf("harness %q declares pty-notice, but an agent owns its terminal: a notice written there is "+
				"a keystroke into that agent's UI, not a message to the agent. Declare the harness's own "+
				"application-level channel instead", h.Name)
		}
	}
}

// TestInteractiveHarnessesDeclareANonPTYChannel guards the specific regression
// the ruling names: claude and codex are PTY-spawnable, which is exactly what
// made "just write into the PTY" look like a general mechanism. They must
// declare their own channel, so the generic path refuses them.
func TestInteractiveHarnessesDeclareANonPTYChannel(t *testing.T) {
	for _, h := range HarnessHarvestList() {
		manifest := h.Manifest()
		if !manifest.Caps.SupportsInteractivePTY || h.Name == agent.HarnessShell {
			continue
		}
		if manifest.Caps.NoticeDelivery == agent.NoticeDeliveryPTYNotice {
			t.Errorf("interactive harness %q declares pty-notice; an agent-driven interactive UI must declare "+
				"its own application-level channel", h.Name)
		}
		if !manifest.Caps.NoticeDelivery.Declared() {
			t.Errorf("interactive harness %q has not declared a notice-delivery mechanism", h.Name)
		}
	}
}
