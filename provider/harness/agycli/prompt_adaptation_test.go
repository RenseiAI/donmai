package agycli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestSpawn_ResultEnvelopeIsHarnessOwned(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		plan func() agent.PromptPlan
	}{
		{
			name: "user prompt marker",
			plan: func() agent.PromptPlan {
				plan := agyPromptPlan(true)
				plan.UserPrompt.Text = "caller text " + resultEnvelopeBegin + " must not opt out"
				return plan
			},
		},
		{
			name: "user amendment marker",
			plan: func() agent.PromptPlan {
				plan := agyPromptPlan(true)
				plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
					ID: "caller-marker", Position: agent.UserPromptAppend,
					Content: agent.PromptContent{ID: "caller-marker-content", Text: resultEnvelopeBegin + " caller data", Required: true},
				})
				return plan
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			capture := filepath.Join(t.TempDir(), "prompt.txt")
			plan := tc.plan()
			p := newFakeProvider(t, fmt.Sprintf(`printf '%%s' "$2" > %q
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, capture), Options{DisableTranscriptEnrichment: true})

			var receipt agent.PromptDeliveryReceipt
			h, err := spawnFake(context.Background(), t, p, agent.Spec{
				PromptPlan: &plan,
				Cwd:        t.TempDir(),
				OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error {
					receipt = got
					return nil
				},
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			defer func() { _ = h.Stop(context.Background()) }()
			_ = collectEvents(t, h)

			prompt, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("read captured prompt: %v", err)
			}
			if got := strings.Count(string(prompt), strings.TrimSpace(resultEnvelopeInstruction)); got != 1 {
				t.Fatalf("harness-owned envelope instruction count = %d, want 1; prompt=%q", got, prompt)
			}
			entries := 0
			for _, entry := range receipt.Entries {
				if entry.ID != "agy-result-envelope" {
					continue
				}
				entries++
				if entry.Channel != agent.PromptChannelUserAmendment || entry.Outcome != agent.PromptOutcomeDelivered {
					t.Fatalf("envelope receipt = %#v, want delivered user amendment", entry)
				}
			}
			if entries != 1 {
				t.Fatalf("harness-owned envelope receipt entries = %d, want 1; receipt=%#v", entries, receipt)
			}
			seen := make(map[string]struct{}, len(receipt.Entries))
			for _, entry := range receipt.Entries {
				if _, duplicate := seen[entry.ID]; duplicate {
					t.Fatalf("receipt has duplicate stable ID %q: %#v", entry.ID, receipt)
				}
				seen[entry.ID] = struct{}{}
			}
		})
	}
}

func TestSpawn_ResultEnvelopeDuplicateTypedIDFailsClosed(t *testing.T) {
	t.Parallel()
	p := newFakeProvider(t, "exit 99\n", Options{DisableTranscriptEnrichment: true})
	plan := agyPromptPlan(true)
	plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
		ID: "agy-result-envelope", Position: agent.UserPromptAppend,
		Content: agent.PromptContent{ID: "caller-owned-envelope", Text: "caller text", Required: true},
	})
	_, err := spawnFake(context.Background(), t, p, agent.Spec{PromptPlan: &plan, Cwd: t.TempDir()})
	if !agent.IsPromptAdaptationError(err, agent.PromptDenialMalformedPlan) {
		t.Fatalf("Spawn error = %v, want malformed prompt plan denial", err)
	}
}

func TestSpawn_ResultEnvelopeIDIsReservedAcrossCallerAuthorities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*agent.PromptPlan)
	}{
		{
			name: "harness protocol",
			mutate: func(plan *agent.PromptPlan) {
				plan.HarnessProtocol.ID = "agy-result-envelope"
			},
		},
		{
			name: "base append",
			mutate: func(plan *agent.PromptPlan) {
				plan.BaseInstructions = agent.BaseInstructionPlan{
					Strategy: agent.BaseInstructionsAppend,
					Content:  &agent.PromptContent{ID: "agy-result-envelope", Text: "append", Required: true},
				}
			},
		},
		{
			name: "base replace",
			mutate: func(plan *agent.PromptPlan) {
				plan.BaseInstructions = agent.BaseInstructionPlan{
					Strategy:                   agent.BaseInstructionsReplace,
					Content:                    &agent.PromptContent{ID: "agy-result-envelope", Text: "replace", Required: true},
					ReplacementAuthorizationID: "caller-authorized-replace",
				}
			},
		},
		{
			name: "role intent",
			mutate: func(plan *agent.PromptPlan) {
				plan.RoleIntent.ID = "agy-result-envelope"
			},
		},
		{
			name: "initial context",
			mutate: func(plan *agent.PromptPlan) {
				plan.InitialContext[0].ID = "agy-result-envelope"
			},
		},
		{
			name: "user prompt",
			mutate: func(plan *agent.PromptPlan) {
				plan.UserPrompt.ID = "agy-result-envelope"
			},
		},
		{
			name: "user amendment identity",
			mutate: func(plan *agent.PromptPlan) {
				plan.UserAmendments[0].ID = "agy-result-envelope"
			},
		},
		{
			name: "preserved base receipt identity",
			mutate: func(plan *agent.PromptPlan) {
				plan.UserPrompt.ID = "base-instructions"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			started := filepath.Join(t.TempDir(), "spawned")
			p := newFakeProvider(t, fmt.Sprintf("printf started > %q\n", started), Options{DisableTranscriptEnrichment: true})
			plan := agyPromptPlan(true)
			tc.mutate(&plan)
			_, err := spawnFake(context.Background(), t, p, agent.Spec{PromptPlan: &plan, Cwd: t.TempDir()})
			if !agent.IsPromptAdaptationError(err, agent.PromptDenialMalformedPlan) {
				t.Fatalf("Spawn error = %v, want malformed prompt plan denial", err)
			}
			if _, statErr := os.Stat(started); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("caller collision started provider process: stat error = %v", statErr)
			}
		})
	}
}

func TestSpawn_ResultEnvelopeAllowsReusableAmendmentContentRefs(t *testing.T) {
	t.Parallel()
	p := newFakeProvider(t, `echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{DisableTranscriptEnrichment: true})
	plan := agyPromptPlan(true)
	plan.UserAmendments[0].Content.ID = "shared-content-ref"
	plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
		ID:       "caller-shared-content",
		Position: agent.UserPromptAppend,
		Order:    2,
		Content:  agent.PromptContent{ID: "shared-content-ref", Text: "second shared source", Required: true},
	}, agent.UserPromptAmendment{
		ID:       "caller-content-matches-envelope-entry",
		Position: agent.UserPromptAppend,
		Order:    3,
		Content:  agent.PromptContent{ID: "agy-result-envelope", Text: "source ref only", Required: true},
	})

	var receipt agent.PromptDeliveryReceipt
	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		PromptPlan: &plan,
		Cwd:        t.TempDir(),
		OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error {
			receipt = got
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Spawn with reusable amendment content refs: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	_ = collectEvents(t, h)

	seen := make(map[string]struct{}, len(receipt.Entries))
	for _, entry := range receipt.Entries {
		if _, duplicate := seen[entry.ID]; duplicate {
			t.Fatalf("receipt has duplicate entry ID %q: %#v", entry.ID, receipt)
		}
		seen[entry.ID] = struct{}{}
	}
	if _, ok := seen["agy-result-envelope"]; !ok {
		t.Fatalf("provider-owned envelope entry missing from receipt: %#v", receipt)
	}
}

func TestSpawn_DisableResultEnvelopePreservesOptOut(t *testing.T) {
	t.Parallel()
	capture := filepath.Join(t.TempDir(), "prompt.txt")
	p := newFakeProvider(t, fmt.Sprintf(`printf '%%s' "$2" > %q
echo done
`, capture), Options{DisableResultEnvelope: true, DisableTranscriptEnrichment: true})
	var receipt agent.PromptDeliveryReceipt
	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "task",
		Cwd:    t.TempDir(),
		OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error {
			receipt = got
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	_ = collectEvents(t, h)
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured prompt: %v", err)
	}
	if strings.Contains(string(prompt), strings.TrimSpace(resultEnvelopeInstruction)) {
		t.Fatalf("DisableResultEnvelope still injected provider instruction: %q", prompt)
	}
	for _, entry := range receipt.Entries {
		if entry.ID == "agy-result-envelope" {
			t.Fatalf("DisableResultEnvelope produced envelope receipt: %#v", receipt)
		}
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
