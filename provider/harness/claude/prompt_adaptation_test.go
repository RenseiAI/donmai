package claude

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExactCLIWire(t *testing.T) {
	t.Parallel()
	plan := claudePromptPlan()
	headless, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, mustClaudePromptProfile(t, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	argv, stdin := buildArgs(headless, "", "")
	wantSystem := "protocol\n\nbase\n\nrole\n\ncontext"
	if !containsPair(argv, "--append-system-prompt", wantSystem) {
		t.Fatalf("headless argv does not carry exact system append: %q", argv)
	}
	if stdin != "user\n\namend" {
		t.Fatalf("headless stdin = %q", stdin)
	}
	if receipt.ProfileID != "claude-code/headless/cli-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}

	interactiveSpec := agent.Spec{PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}
	interactive, interactiveReceipt, err := agent.AdaptPrompt(interactiveSpec, mustClaudePromptProfile(t, agent.PromptModeHumanControlled))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := interactiveArgs(interactive), []string{"--append-system-prompt", wantSystem, "user\n\namend"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("interactive argv = %#v, want %#v", got, want)
	}
	assertClaudeSemanticParity(t, receipt, interactiveReceipt)
}

func assertClaudeSemanticParity(t *testing.T, headless, interactive agent.PromptDeliveryReceipt) {
	t.Helper()
	if len(headless.Entries) != len(interactive.Entries) {
		t.Fatalf("receipt entry counts differ: %d vs %d", len(headless.Entries), len(interactive.Entries))
	}
	for i := range headless.Entries {
		h, in := headless.Entries[i], interactive.Entries[i]
		h.Delivery, in.Delivery = "", ""
		if !reflect.DeepEqual(h, in) {
			t.Fatalf("semantic receipt entry %d differs: %+v vs %+v", i, h, in)
		}
	}
}

func claudePromptPlan() agent.PromptPlan {
	return agent.PromptPlan{
		ContractVersion: agent.PromptContractVersion,
		HarnessProtocol: &agent.PromptContent{ID: "protocol", Text: "protocol", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{
			Strategy: agent.BaseInstructionsAppend,
			Content:  &agent.PromptContent{ID: "base", Text: "base", Required: true},
		},
		RoleIntent:     &agent.PromptContent{ID: "role", Text: "role", Required: true},
		InitialContext: []agent.PromptContent{{ID: "context", Text: "context", Required: true}},
		UserPrompt:     agent.PromptContent{ID: "user", Text: "user", Required: true},
		UserAmendments: []agent.UserPromptAmendment{{ID: "amend", Position: agent.UserPromptAppend, Content: agent.PromptContent{ID: "amend-content", Text: "amend", Required: true}}},
	}
}

func mustClaudePromptProfile(t *testing.T, mode agent.PromptSessionMode) agent.PromptDeliveryProfile {
	t.Helper()
	profile, ok := (&Provider{}).Manifest().PromptProfile(mode)
	if !ok {
		t.Fatalf("missing profile for %s", mode)
	}
	return profile
}

func containsPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
