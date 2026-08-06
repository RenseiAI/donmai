package ollama

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExactChatMessages(t *testing.T) {
	t.Parallel()
	profile, ok := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing raw Ollama prompt profile")
	}
	plan := ollamaPromptPlan()
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	body, err := buildChatRequest("llama3.3", adapted)
	if err != nil {
		t.Fatal(err)
	}
	var wire chatRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	want := []chatMessage{
		{Role: "system", Content: "protocol\n\nbase\n\nrole\n\ncontext"},
		{Role: "user", Content: "user\n\namend"},
	}
	if len(wire.Messages) != len(want) || wire.Messages[0] != want[0] || wire.Messages[1] != want[1] {
		t.Fatalf("messages = %#v, want %#v", wire.Messages, want)
	}
	if receipt.ProfileID != "ollama/ollama-chat/direct-api-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func ollamaPromptPlan() agent.PromptPlan {
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
