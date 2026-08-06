package pi

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExactCLIAndRPCWire(t *testing.T) {
	t.Parallel()
	profile, _ := (&Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := piPromptPlan()
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	wantSystem := "protocol\n\nbase\n\nrole\n\ncontext"
	layout := sessionLayout{root: "/session", extension: "/session/policy.ts"}
	wantArgs := []string{
		"--mode", "rpc", "-e", "/session/policy.ts", "--no-extensions", "--approve", "--session-dir", "/session",
		"--append-system-prompt", wantSystem,
	}
	if got := rpcArgs(layout, launchPrompt, "", adapted); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("pi argv = %#v, want %#v", got, wantArgs)
	}
	if got, want := adapted.Prompt, "user\n\namend"; got != want {
		t.Fatalf("RPC prompt message = %q, want %q", got, want)
	}
	if receipt.ProfileID != "pi/headless/rpc-v1" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}
}

func piPromptPlan() agent.PromptPlan {
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
