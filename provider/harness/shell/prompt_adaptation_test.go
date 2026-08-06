package shell

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExplicitDowngradeToPTYSeed(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeHumanControlled)
	plan := shellPromptPlan(true)
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := adapted.Prompt, "protocol\n\nrole\n\ncontext\n\nuser\n\namend"; got != want {
		t.Fatalf("PTY seed = %q, want %q", got, want)
	}
	if receipt.ProfileID != "shell/interactive/pty-seed-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func TestPromptAdaptation_RequiredSystemDeniedWithoutAuthority(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeHumanControlled)
	plan := shellPromptPlan(false)
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialDeliveryUnsupported) {
		t.Fatalf("error = %v", err)
	}
}

func shellPromptPlan(authorize bool) agent.PromptPlan {
	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		HarnessProtocol:  &agent.PromptContent{ID: "protocol", Text: "protocol", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		RoleIntent:       &agent.PromptContent{ID: "role", Text: "role", Required: true},
		InitialContext:   []agent.PromptContent{{ID: "context", Text: "context", Required: true}},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "user", Required: true},
		UserAmendments:   []agent.UserPromptAmendment{{ID: "amend", Position: agent.UserPromptAppend, Content: agent.PromptContent{ID: "amend-content", Text: "amend", Required: true}}},
	}
	if authorize {
		plan.AuthorizedDowngrades = []agent.PromptDowngradeAuthorization{
			{ID: "protocol-to-user", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
			{ID: "role-to-user", Channel: agent.PromptChannelRoleIntent, To: agent.PromptChannelUserPrompt},
			{ID: "context-to-user", Channel: agent.PromptChannelInitialContext, To: agent.PromptChannelUserPrompt},
		}
	}
	return plan
}
