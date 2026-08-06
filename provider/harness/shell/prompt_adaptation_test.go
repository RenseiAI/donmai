package shell

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_UserTaskUsesPTYSeed(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeHumanControlled)
	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "  user command  ", Required: true},
	}
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := adapted.Prompt, "  user command  "; got != want {
		t.Fatalf("PTY seed = %q, want %q", got, want)
	}
	if receipt.ProfileID != "shell/interactive/pty-seed-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func TestPromptAdaptation_RejectsNonUserAuthorityDowngrade(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeHumanControlled)
	plan := shellPromptPlan()
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialDowngradeUnauthorized) {
		t.Fatalf("error = %v", err)
	}
}

func TestPromptAdaptation_RejectsUserAmendment(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeHumanControlled)
	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "user", Required: true},
		UserAmendments: []agent.UserPromptAmendment{{
			ID: "amend", Position: agent.UserPromptAppend,
			Content: agent.PromptContent{ID: "amend-content", Text: "forbidden amendment", Required: true},
		}},
	}
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialDeliveryUnsupported) {
		t.Fatalf("error = %v", err)
	}
}

func shellPromptPlan() agent.PromptPlan {
	return agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		HarnessProtocol:  &agent.PromptContent{ID: "protocol", Text: "protocol", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		RoleIntent:       &agent.PromptContent{ID: "role", Text: "role", Required: true},
		InitialContext:   []agent.PromptContent{{ID: "context", Text: "context", Required: true}},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "user", Required: true},
		UserAmendments:   []agent.UserPromptAmendment{{ID: "amend", Position: agent.UserPromptAppend, Content: agent.PromptContent{ID: "amend-content", Text: "amend", Required: true}}},
		AuthorizedDowngrades: []agent.PromptDowngradeAuthorization{
			{ID: "protocol-to-user", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
			{ID: "role-to-user", Channel: agent.PromptChannelRoleIntent, To: agent.PromptChannelUserPrompt},
			{ID: "context-to-user", Channel: agent.PromptChannelInitialContext, To: agent.PromptChannelUserPrompt},
		},
	}
}
