package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// TestTranslateSpec_CardSkillsProjected verifies the queued work's card
// skills reach the harness-native plugin surface: non-blank bodies are
// carried verbatim, whitespace-only bodies are skipped (mirroring
// foldInlineSkills), and an empty skill set keeps the Spec field nil so
// the wire stays byte-identical.
func TestTranslateSpec_CardSkillsProjected(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		Skills: []prompt.SkillSpec{
			{ID: "go-review", Body: "review carefully"},
			{ID: "blank", Body: "   \n "},
			{ID: "second", Body: "second body"},
		},
	}}
	spec := translateSpec(qw, agent.Capabilities{AcceptsAllowedToolsList: true}, SpecInputs{Autonomous: true})
	if len(spec.CardSkills) != 2 {
		t.Fatalf("CardSkills = %v, want 2 entries (blank skipped)", spec.CardSkills)
	}
	if spec.CardSkills[0].ID != "go-review" || spec.CardSkills[0].Body != "review carefully" {
		t.Errorf("CardSkills[0] = %+v, want go-review/review carefully", spec.CardSkills[0])
	}
	if spec.CardSkills[1].ID != "second" {
		t.Errorf("CardSkills[1] = %+v, want second", spec.CardSkills[1])
	}
}

// TestTranslateSpec_NoCardSkillsIsNil verifies a skill-less work item
// leaves CardSkills nil — the spawn argv stays byte-identical.
func TestTranslateSpec_NoCardSkillsIsNil(t *testing.T) {
	t.Parallel()

	for _, qw := range []QueuedWork{
		{},
		{QueuedWork: prompt.QueuedWork{Skills: nil}},
		{QueuedWork: prompt.QueuedWork{Skills: []prompt.SkillSpec{{ID: "blank", Body: "  "}}}},
	} {
		spec := translateSpec(qw, agent.Capabilities{AcceptsAllowedToolsList: true}, SpecInputs{Autonomous: true})
		if spec.CardSkills != nil {
			t.Fatalf("CardSkills = %v, want nil for skill-less work", spec.CardSkills)
		}
	}
}

// TestApplyPreparedSourceAuthority_CarriesCardSkills proves receipt-bearing
// sessions keep their card skills: the prepared-source authority copy must
// carry CardSkills, or a receipted Claude session would spawn without its
// --plugin-dir.
func TestApplyPreparedSourceAuthority_CarriesCardSkills(t *testing.T) {
	t.Parallel()

	source := agent.Spec{CardSkills: []agent.CardSkill{{ID: "go-review", Body: "review carefully"}}}
	got := applyPreparedSourceAuthority(agent.Spec{}, source, nil)
	if len(got.CardSkills) != 1 || got.CardSkills[0].ID != "go-review" {
		t.Fatalf("CardSkills = %v, want the source skill carried", got.CardSkills)
	}
	// Nil stays nil — skill-less sessions keep the byte-identical shape.
	if out := applyPreparedSourceAuthority(agent.Spec{}, agent.Spec{}, nil); out.CardSkills != nil {
		t.Fatalf("CardSkills = %v, want nil for skill-less source", out.CardSkills)
	}
}
