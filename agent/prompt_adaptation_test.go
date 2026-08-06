package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/agycli"
	"github.com/RenseiAI/donmai/provider/harness/amp"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/gemini"
	"github.com/RenseiAI/donmai/provider/harness/ollama"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/shell"
)

func TestPromptAdaptation_AllConcreteHarnessModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		manifest          agent.HarnessManifest
		mode              agent.PromptSessionMode
		supportsSystem    bool
		supportsDowngrade bool
		contextInUser     bool
	}{
		{name: "claude/headless", manifest: (&claude.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsSystem: true},
		{name: "claude/interactive", manifest: (&claude.Provider{}).Manifest(), mode: agent.PromptModeHumanControlled, supportsSystem: true},
		{name: "codex/headless", manifest: (&codex.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsSystem: true},
		{name: "codex/interactive", manifest: (&codex.Provider{}).Manifest(), mode: agent.PromptModeHumanControlled, supportsSystem: true, contextInUser: true},
		{name: "gemini/raw", manifest: (&gemini.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsSystem: true},
		{name: "ollama/raw", manifest: (&ollama.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsSystem: true},
		{name: "amp/headless", manifest: (&amp.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsDowngrade: true},
		{name: "agy/headless", manifest: (&agycli.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsDowngrade: true},
		{name: "opencode/headless", manifest: (&opencode.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsDowngrade: true},
		{name: "pi/headless", manifest: (&pi.Provider{}).Manifest(), mode: agent.PromptModeAutonomous, supportsSystem: true},
		{name: "shell/interactive", manifest: (&shell.Provider{}).Manifest(), mode: agent.PromptModeHumanControlled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			profile, ok := tt.manifest.PromptProfile(tt.mode)
			if !ok {
				t.Fatalf("manifest has no prompt profile for %s", tt.mode)
			}
			plan := fullPromptPlan()
			spec := agent.Spec{PromptPlan: &plan}
			if tt.mode == agent.PromptModeHumanControlled {
				spec.Interactive = &agent.InteractiveSpec{}
			}

			adapted, deniedReceipt, err := agent.AdaptPrompt(spec, profile)
			if !tt.supportsSystem {
				var adaptationErr *agent.PromptAdaptationError
				if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.PromptDenialDeliveryUnsupported {
					t.Fatalf("unsupported required system delivery error = %v", err)
				}
				if deniedReceipt.Decision != "denied" || len(deniedReceipt.Entries) == 0 {
					t.Fatalf("denied receipt = %+v", deniedReceipt)
				}

				plan.AuthorizedDowngrades = []agent.PromptDowngradeAuthorization{
					{ID: "downgrade-protocol", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
					{ID: "downgrade-base", Channel: agent.PromptChannelBaseInstructions, To: agent.PromptChannelUserPrompt},
					{ID: "downgrade-role", Channel: agent.PromptChannelRoleIntent, To: agent.PromptChannelUserPrompt},
					{ID: "downgrade-context", Channel: agent.PromptChannelInitialContext, To: agent.PromptChannelUserPrompt},
				}
				spec.PromptPlan = &plan
				adapted, deniedReceipt, err = agent.AdaptPrompt(spec, profile)
				if !tt.supportsDowngrade {
					if !agent.IsPromptAdaptationError(err, agent.PromptDenialDowngradeUnauthorized) {
						t.Fatalf("forbidden user downgrade error = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("authorized downgrade: %v", err)
				}
				if adapted.SystemPromptAppend != "" {
					t.Fatalf("unsupported system surface received %q", adapted.SystemPromptAppend)
				}
				assertOrdered(t, adapted.Prompt, "protocol-nonce", "base-nonce", "role-nonce", "context-nonce", "prepend-1", "prepend-2", "user-nonce", "append-1")
				assertReceiptOutcome(t, deniedReceipt, agent.PromptChannelHarnessProtocol, agent.PromptOutcomeDowngraded)
				return
			}

			if err != nil {
				t.Fatalf("AdaptPrompt: %v", err)
			}
			if deniedReceipt.Decision != "ready" {
				t.Fatalf("receipt decision = %q", deniedReceipt.Decision)
			}
			assertOrdered(t, adapted.SystemPromptAppend, "protocol-nonce", "base-nonce", "role-nonce")
			switch profile.ContextDelivery {
			case agent.PromptDeliveryCodexTurnInput:
				if adapted.InitialContext != "context-nonce" {
					t.Fatalf("InitialContext = %q", adapted.InitialContext)
				}
			default:
				if tt.contextInUser {
					assertOrdered(t, adapted.Prompt, "context-nonce", "prepend-1", "prepend-2", "user-nonce", "append-1")
				} else {
					assertOrdered(t, adapted.SystemPromptAppend, "role-nonce", "context-nonce")
				}
			}
			assertOrdered(t, adapted.Prompt, "prepend-1", "prepend-2", "user-nonce", "append-1")
		})
	}
}

func TestPromptAdaptation_ReplaceRequiresAuthority(t *testing.T) {
	t.Parallel()
	profile, _ := (&codex.Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := fullPromptPlan()
	plan.BaseInstructions.Strategy = agent.BaseInstructionsReplace

	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialReplacementAuth) {
		t.Fatalf("missing replacement authority error = %v", err)
	}

	plan.BaseInstructions.ReplacementAuthorizationID = "policy-auth-1"
	adapted, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatalf("authorized replace: %v", err)
	}
	assertOrdered(t, adapted.SystemPromptAppend, "protocol-nonce", "role-nonce")
	if adapted.BaseInstructions != "base-nonce" {
		t.Fatalf("native base replacement = %q", adapted.BaseInstructions)
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q", receipt.Decision)
	}
	var found bool
	for _, entry := range receipt.Entries {
		if entry.Channel == agent.PromptChannelBaseInstructions {
			found = true
			if entry.BaseInstructionStrategy != agent.BaseInstructionsReplace || entry.ReplacementAuthorizationID != "policy-auth-1" {
				t.Fatalf("replacement receipt = %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("receipt lacks base-instruction replacement entry")
	}
}

func TestPromptAdaptation_ReplaceDeniedOnAppendOnlyHarness(t *testing.T) {
	t.Parallel()
	profile, _ := (&claude.Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := fullPromptPlan()
	plan.BaseInstructions.Strategy = agent.BaseInstructionsReplace
	plan.BaseInstructions.ReplacementAuthorizationID = "policy-auth-1"
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialUnsupportedStrategy) {
		t.Fatalf("append-only replacement error = %v", err)
	}
}

func TestPromptAdaptation_ReplacementMatrix(t *testing.T) {
	t.Parallel()
	manifests := []agent.HarnessManifest{
		(&claude.Provider{}).Manifest(), (&codex.Provider{}).Manifest(), (&gemini.Provider{}).Manifest(),
		(&ollama.Provider{}).Manifest(), (&amp.Provider{}).Manifest(), (&agycli.Provider{}).Manifest(),
		(&opencode.Provider{}).Manifest(), (&pi.Provider{}).Manifest(), (&shell.Provider{}).Manifest(),
	}
	for _, manifest := range manifests {
		for _, profile := range manifest.PromptDelivery {
			profile := profile
			t.Run(profile.ID, func(t *testing.T) {
				t.Parallel()
				plan := agent.PromptPlan{
					ContractVersion: agent.PromptContractVersion,
					BaseInstructions: agent.BaseInstructionPlan{
						Strategy: agent.BaseInstructionsReplace,
						Content:  &agent.PromptContent{ID: "base", Text: "replacement", Required: true}, ReplacementAuthorizationID: "policy-auth",
					},
					UserPrompt: agent.PromptContent{ID: "user", Text: "task", Required: true},
				}
				_, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
				if profile.BaseReplaceDelivery == agent.PromptDeliveryUnsupported {
					if !agent.IsPromptAdaptationError(err, agent.PromptDenialUnsupportedStrategy) {
						t.Fatalf("unsupported replace error = %v", err)
					}
					return
				}
				if err != nil || receipt.Decision != "ready" {
					t.Fatalf("supported replace: receipt=%+v err=%v", receipt, err)
				}
			})
		}
	}
}

func TestPromptAdaptation_RejectsUnknownDeliveryAndMalformedIdentity(t *testing.T) {
	t.Parallel()
	profile, _ := (&claude.Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	profile.SystemDelivery = agent.PromptDeliveryKind("invented")
	plan := fullPromptPlan()
	_, _, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialMalformedPlan) {
		t.Fatalf("unknown delivery error = %v", err)
	}

	profile, _ = (&claude.Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan.HarnessProtocol.ID = ""
	_, _, err = agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialMalformedPlan) {
		t.Fatalf("missing stable id error = %v", err)
	}
}

func TestPromptAdaptation_ReceiptContainsDigestsNotBodies(t *testing.T) {
	t.Parallel()
	profile, _ := (&ollama.Provider{}).Manifest().PromptProfile(agent.PromptModeAutonomous)
	plan := fullPromptPlan()
	_, receipt, err := agent.AdaptPrompt(agent.Spec{PromptPlan: &plan}, profile)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"protocol-nonce", "base-nonce", "role-nonce", "context-nonce", "user-nonce"} {
		if strings.Contains(string(raw), body) {
			t.Fatalf("receipt leaked prompt body %q: %s", body, raw)
		}
	}
	if !strings.Contains(string(raw), "sha256:") {
		t.Fatalf("receipt lacks content digests: %s", raw)
	}
}

func TestPreparePrompt_PersistsReceiptBeforeReturningReady(t *testing.T) {
	t.Parallel()
	plan := fullPromptPlan()
	var persisted agent.PromptDeliveryReceipt
	spec := agent.Spec{
		PromptPlan: &plan,
		OnPromptAdapted: func(receipt agent.PromptDeliveryReceipt) error {
			persisted = receipt
			return nil
		},
	}
	adapted, err := agent.PreparePrompt(spec, (&claude.Provider{}).Manifest())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Decision != "ready" || adapted.PromptReceipt == nil || adapted.PromptReceipt.ProfileID != persisted.ProfileID {
		t.Fatalf("persisted=%+v adapted=%+v", persisted, adapted.PromptReceipt)
	}
}

func TestPreparePrompt_ReceiptPersistenceFailureDenies(t *testing.T) {
	t.Parallel()
	plan := fullPromptPlan()
	_, err := agent.PreparePrompt(agent.Spec{
		PromptPlan: &plan,
		OnPromptAdapted: func(agent.PromptDeliveryReceipt) error {
			return errors.New("disk unavailable")
		},
	}, (&claude.Provider{}).Manifest())
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialApplicationFailed) {
		t.Fatalf("receipt persistence error = %v", err)
	}
}

func fullPromptPlan() agent.PromptPlan {
	return agent.PromptPlan{
		ContractVersion: agent.PromptContractVersion,
		HarnessProtocol: &agent.PromptContent{ID: "protocol", Text: "protocol-nonce", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{
			Strategy: agent.BaseInstructionsAppend,
			Content:  &agent.PromptContent{ID: "base", Text: "base-nonce", Required: true},
		},
		RoleIntent:     &agent.PromptContent{ID: "role", Text: "role-nonce", Required: true},
		InitialContext: []agent.PromptContent{{ID: "context", Text: "context-nonce", Required: true}},
		UserPrompt:     agent.PromptContent{ID: "user", Text: "user-nonce", Required: true},
		UserAmendments: []agent.UserPromptAmendment{
			{ID: "prepend-b", Position: agent.UserPromptPrepend, Order: 2, Content: agent.PromptContent{ID: "prepend-b-body", Text: "prepend-2", Required: true}},
			{ID: "append-a", Position: agent.UserPromptAppend, Order: 1, Content: agent.PromptContent{ID: "append-a-body", Text: "append-1", Required: true}},
			{ID: "prepend-a", Position: agent.UserPromptPrepend, Order: 1, Content: agent.PromptContent{ID: "prepend-a-body", Text: "prepend-1", Required: true}},
		},
	}
}

func assertOrdered(t *testing.T, text string, values ...string) {
	t.Helper()
	last := -1
	for _, value := range values {
		index := strings.Index(text, value)
		if index < 0 || index <= last {
			t.Fatalf("%q is missing or out of order in %q", value, text)
		}
		last = index
	}
}

func assertReceiptOutcome(t *testing.T, receipt agent.PromptDeliveryReceipt, channel agent.PromptChannel, outcome agent.PromptDeliveryOutcome) {
	t.Helper()
	for _, entry := range receipt.Entries {
		if entry.Channel == channel && entry.Outcome == outcome {
			return
		}
	}
	t.Fatalf("receipt lacks channel=%s outcome=%s: %+v", channel, outcome, receipt)
}
