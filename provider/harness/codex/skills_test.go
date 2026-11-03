package codex

import (
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

// TestSkillPlanGolden_Codex pins the prompt fallback this harness takes
// until a native location is proven: every skill with a body rides the
// prompt, the repository policy is recorded as all, and a skill that
// requires native delivery refuses the spawn.
func TestSkillPlanGolden_Codex(t *testing.T) {
	t.Parallel()
	manifest := (&Provider{}).Manifest()
	pkg := agent.SealSkillPackage(stagewire.Skill{
		ID: "review", Body: "review fallback",
		Files: []stagewire.SkillFile{agent.NewSkillFile("SKILL.md", []byte("---\nname: review\ndescription: d\n---\n"), 0o644)},
	})
	delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: agent.PromptModeAutonomous,
		Skills:     []stagewire.Skill{pkg, {ID: "legacy", Body: "legacy body"}},
		RepoSkills: &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "harness=codex layout= scripts=allowed trust=false repo=none->all\n" +
		"review prompt reason=harness_prompt_only\n" +
		"legacy prompt reason=harness_prompt_only"
	if got := delivery.Plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
	if len(residue) != 2 {
		t.Fatalf("residue = %d, want both bodies on the prompt", len(residue))
	}

	pkg.Delivery = stagewire.SkillDeliveryNativeRequired
	_, _, err = agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: manifest.Name, Profile: manifest.Skills, Skills: []stagewire.Skill{pkg}})
	var deliveryErr *agent.SkillDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Reason != agent.SkillReasonHarnessPromptOnly {
		t.Fatalf("native_required: err = %v, want a harness_prompt_only refusal", err)
	}
}
