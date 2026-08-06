package gemini

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExactGenerateContentWire(t *testing.T) {
	t.Parallel()
	profile, ok := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing raw Gemini prompt profile")
	}
	plan := geminiPromptPlan()
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := buildSpawnPlan(adapted, "gemini-3.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := wire.systemInstruction.Parts[0].Text, "protocol\n\nbase\n\nrole\n\ncontext"; got != want {
		t.Fatalf("systemInstruction = %q, want %q", got, want)
	}
	if got, want := wire.initialContents[0].Parts[0].Text, "user\n\namend"; got != want {
		t.Fatalf("user content = %q, want %q", got, want)
	}
	if receipt.ProfileID != "raw/gemini-generate/direct-api-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func geminiPromptPlan() agent.PromptPlan {
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
