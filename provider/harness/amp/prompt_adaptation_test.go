package amp

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExplicitDowngradeToStdin(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := ampPromptPlan(true)
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := adapted.Prompt, "protocol\n\nrole\n\ncontext\n\nuser\n\namend"; got != want {
		t.Fatalf("stdin prompt = %q, want %q", got, want)
	}
	if receipt.ProfileID != "amp/headless/execute-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
	if adapted.SystemPromptAppend != "" {
		t.Fatalf("unsupported system wire received %q", adapted.SystemPromptAppend)
	}
}

func TestPromptAdaptation_RequiredSystemDeniedWithoutAuthority(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := ampPromptPlan(false)
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialDeliveryUnsupported) {
		t.Fatalf("error = %v", err)
	}
}

func TestPromptAdaptation_DenialHasZeroSpawnSideEffects(t *testing.T) {
	t.Parallel()
	plan := ampPromptPlan(false)
	spawned := false
	var receipt agent.PromptDeliveryReceipt
	p := &Provider{binary: "/usr/bin/true", apiKey: "test-key"}
	_, err := p.Spawn(context.Background(), agent.Spec{
		PromptPlan:       &plan,
		OnProcessSpawned: func(int) { spawned = true },
		OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error {
			receipt = got
			return nil
		},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) || !agent.IsPromptAdaptationError(err, agent.PromptDenialDeliveryUnsupported) {
		t.Fatalf("typed spawn denial = %v", err)
	}
	if spawned {
		t.Fatal("provider process started after prompt denial")
	}
	if receipt.Decision != "denied" || len(receipt.Entries) == 0 {
		t.Fatalf("denied receipt = %+v", receipt)
	}
}

func ampPromptPlan(authorize bool) agent.PromptPlan {
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
