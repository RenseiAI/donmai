package codex

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestPromptAdaptation_ExactAppServerAndInteractiveWire(t *testing.T) {
	t.Parallel()
	plan := codexPromptPlan()
	wantSystem := "protocol\n\nbase\n\nrole"
	headless, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, mustCodexPromptProfile(t, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	spawn := NewSpawnPlan(headless)
	if got := spawn.ThreadStart["developerInstructions"]; got != wantSystem {
		t.Fatalf("thread/start developerInstructions = %#v", got)
	}
	if got, exists := spawn.ThreadStart["baseInstructions"]; exists {
		t.Fatalf("append unexpectedly replaced base instructions: %#v", got)
	}
	wantInput := []map[string]any{
		{"type": "text", "text": "context"},
		{"type": "text", "text": "user\n\namend"},
	}
	if !reflect.DeepEqual(spawn.PromptInput, wantInput) {
		t.Fatalf("turn input = %#v, want %#v", spawn.PromptInput, wantInput)
	}
	if receipt.ProfileID != "codex/headless/app-server-v2" {
		t.Fatalf("profile = %q", receipt.ProfileID)
	}

	workspace := t.TempDir()
	interactiveSpec := agent.Spec{Cwd: workspace, PromptPlan: &plan, Interactive: &agent.InteractiveSpec{}}
	interactive, interactiveReceipt, err := agent.AdaptPrompt(interactiveSpec, mustCodexPromptProfile(t, agent.PromptModeHumanControlled))
	if err != nil {
		t.Fatal(err)
	}
	// The startup-trust seed (trust.go) leads every interactive argv; the
	// prompt wire this test pins is what follows it.
	wantArgs := append(trustPrefixFor(t, workspace),
		"--config", "developer_instructions="+strconv.Quote(wantSystem),
		"context\n\nuser\n\namend",
	)
	if got := interactiveArgs(interactive); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("interactive argv = %#v, want %#v", got, wantArgs)
	}
	assertCodexSemanticParity(t, receipt, interactiveReceipt)

	replacePlan := codexPromptPlan()
	replacePlan.BaseInstructions.Strategy = agent.BaseInstructionsReplace
	replacePlan.BaseInstructions.ReplacementAuthorizationID = "mode-policy"
	replaced, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &replacePlan}, mustCodexPromptProfile(t, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	replaceWire := NewSpawnPlan(replaced).ThreadStart
	if replaceWire["baseInstructions"] != "base" || replaceWire["developerInstructions"] != "protocol\n\nrole" {
		t.Fatalf("replacement wire = %#v", replaceWire)
	}
}

func assertCodexSemanticParity(t *testing.T, headless, interactive agent.PromptDeliveryReceipt) {
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

func codexPromptPlan() agent.PromptPlan {
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

func mustCodexPromptProfile(t *testing.T, mode agent.PromptSessionMode) agent.PromptDeliveryProfile {
	t.Helper()
	profile, ok := (&Provider{}).Manifest().PromptProfile(mode)
	if !ok {
		t.Fatalf("missing profile for %s", mode)
	}
	return profile
}
