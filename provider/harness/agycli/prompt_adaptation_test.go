package agycli

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExplicitDowngradeToPromptFlag(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := agyPromptPlan(true)
	plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
		ID: "agy-result-envelope", Position: agent.UserPromptAppend, Order: 1000,
		Content: agent.PromptContent{ID: "agy-result-envelope-content", Text: "envelope", Required: true},
	})
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	wantPrompt := "protocol\n\nrole\n\ncontext\n\nuser\n\namend\n\nenvelope"
	if got, want := buildArgs(adapted, false), []string{"-p", wantPrompt, "--dangerously-skip-permissions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agy argv = %#v, want %#v", got, want)
	}
	if receipt.ProfileID != "antigravity/headless/agy-pty-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func TestPromptAdaptation_RequiredSystemDeniedWithoutAuthority(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := agyPromptPlan(false)
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialDeliveryUnsupported) {
		t.Fatalf("error = %v", err)
	}
}

func TestPromptAdaptation_ResultEnvelopeDetectionUsesTypedAmendments(t *testing.T) {
	t.Parallel()
	plan := agyPromptPlan(true)
	plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
		ID: "existing-envelope", Position: agent.UserPromptAppend,
		Content: agent.PromptContent{ID: "existing-envelope-content", Text: resultEnvelopeInstruction, Required: true},
	})
	if !planContainsResultEnvelope(plan) {
		t.Fatal("typed result-envelope amendment was not detected")
	}
}

func agyPromptPlan(authorize bool) agent.PromptPlan {
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
