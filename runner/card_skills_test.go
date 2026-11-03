package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// TestTranslateSpec_CardSkillsRideMuseOnly proves the card-skill spec
// channel is populated exactly for the harness whose trusted workspace
// loads it from disk: a muse session carries its card skills on
// Spec.CardSkills, while any other harness (and an empty provider
// name) keeps the historical nil so the wire round-trip is unchanged.
func TestTranslateSpec_CardSkillsRideMuseOnly(t *testing.T) {
	t.Parallel()
	skills := []prompt.SkillSpec{
		{ID: "go-review", Body: "review carefully"},
		{ID: "triage", Body: "triage first"},
	}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "s-1", Skills: skills}}

	muse := translateSpec(qw, agent.Capabilities{}, SpecInputs{ProviderName: string(agent.ProviderMuse)})
	if len(muse.CardSkills) != 2 {
		t.Fatalf("muse Spec.CardSkills = %+v; want the 2 card skills", muse.CardSkills)
	}
	for i, want := range skills {
		if muse.CardSkills[i].ID != want.ID || muse.CardSkills[i].Body != want.Body {
			t.Errorf("muse Spec.CardSkills[%d] = %+v; want %+v", i, muse.CardSkills[i], want)
		}
	}

	for _, other := range []string{"", "claude", "codex", "pi"} {
		got := translateSpec(qw, agent.Capabilities{}, SpecInputs{ProviderName: other})
		if len(got.CardSkills) != 0 {
			t.Errorf("provider %q Spec.CardSkills = %+v; want nil (prompt append still carries them)", other, got.CardSkills)
		}
	}
}

// TestTranslateSpec_CardSkillsEmptyStaysNil proves a muse session with
// no card skills serializes exactly as before: no empty slice on the
// wire.
func TestTranslateSpec_CardSkillsEmptyStaysNil(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "s-1"}}
	got := translateSpec(qw, agent.Capabilities{}, SpecInputs{ProviderName: string(agent.ProviderMuse)})
	if got.CardSkills != nil {
		t.Errorf("muse Spec.CardSkills = %+v; want nil for a skill-less session", got.CardSkills)
	}
}
