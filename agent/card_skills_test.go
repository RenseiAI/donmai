package agent_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

// TestApplyPreparedHarnessNamesCardSkillsDrift proves the card-skills surface
// is bound into the prepared-harness authority: a spawn that changes
// CardSkills after preflight fails closed naming cardSkills, never silently
// running on a different skill set.
func TestApplyPreparedHarnessNamesCardSkillsDrift(t *testing.T) {
	t.Parallel()

	manifest := (&codex.Provider{}).Manifest()
	source := agent.Spec{
		PromptMode:         agent.PromptModeHumanControlled,
		SandboxEnabled:     true,
		SandboxLevel:       agent.SandboxWorkspaceWrite,
		Model:              "gpt-test",
		SystemPromptAppend: "base",
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "actual-user-task", Text: "inspect", Required: true},
		},
		Interactive: &agent.InteractiveSpec{},
		CardSkills:  []agent.CardSkill{{ID: "go-review", Body: "review carefully"}},
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, manifest, operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("CompilePreparedHarness: %v", err)
	}
	mutated := source
	mutated.CardSkills = []agent.CardSkill{{ID: "go-review", Body: "mutated body"}}
	mutated.PreparedHarness = plan
	_, err = agent.PrepareHarness(mutated, manifest)
	if err == nil {
		t.Fatal("expected an authority drift error, got nil")
	}
	var driftErr *agent.AuthorityDriftError
	if !errors.As(err, &driftErr) {
		t.Fatalf("expected *agent.AuthorityDriftError, got %T: %v", err, err)
	}
	if !reflect.DeepEqual(driftErr.Fields, []string{"cardSkills"}) {
		t.Fatalf("Fields = %v, want [cardSkills]", driftErr.Fields)
	}
}
