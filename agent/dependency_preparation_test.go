package agent_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// TestDependencyPreparationNeedsRecovery pins the closed recovery trigger:
// only failed and timed_out outcomes carry the handoff. Ready, both
// skipped outcomes, empty, and nil reports carry nothing.
func TestDependencyPreparationNeedsRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		prep *agent.DependencyPreparation
		want bool
	}{
		{name: "nil omits", prep: nil, want: false},
		{name: "empty omits", prep: &agent.DependencyPreparation{}, want: false},
		{name: "ready adds nothing", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationReady, Go: agent.DependencyPreparationReady}},
		{name: "read-only skip adds nothing", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationSkippedReadOnly}},
		{name: "security skip adds nothing", prep: &agent.DependencyPreparation{Go: agent.DependencyPreparationSkippedSecurity}},
		{name: "failed hands recovery", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed}, want: true},
		{name: "timed out hands recovery", prep: &agent.DependencyPreparation{Go: agent.DependencyPreparationTimedOut}, want: true},
		{name: "mixed failed and ready hands recovery", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed, Go: agent.DependencyPreparationReady}, want: true},
		{name: "mixed timed out and skipped hands recovery", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationTimedOut, Go: agent.DependencyPreparationSkippedReadOnly}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.prep.NeedsRecovery(); got != tc.want {
				t.Fatalf("NeedsRecovery() = %v; want %v for %+v", got, tc.want, tc.prep)
			}
		})
	}
}

// TestAppendDependencyPreparationRecovery pins the bounded handoff prose:
// ready/skipped/nil leave the protocol byte-identical, failed/timed_out
// preserve the existing protocol and name per-ecosystem outcomes without
// raw installer text.
func TestAppendDependencyPreparationRecovery(t *testing.T) {
	t.Parallel()
	base := "# Operating protocol\nFollow the harness protocol."
	for _, tc := range []struct {
		name       string
		prep       *agent.DependencyPreparation
		wantChange bool
	}{
		{name: "nil omits", prep: nil},
		{name: "ready adds nothing", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationReady}},
		{name: "read-only skip adds no bypass", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationSkippedReadOnly}},
		{name: "security skip adds no bypass", prep: &agent.DependencyPreparation{Go: agent.DependencyPreparationSkippedSecurity}},
		{name: "failed hands recovery", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed}, wantChange: true},
		{name: "timed out hands recovery", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed, Go: agent.DependencyPreparationTimedOut}, wantChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.AppendDependencyPreparationRecovery(base, tc.prep)
			if !tc.wantChange {
				if got != base {
					t.Fatalf("protocol changed for %+v; want byte-identical", tc.prep)
				}
				return
			}
			if got == base {
				t.Fatalf("protocol unchanged for %+v; want recovery handoff", tc.prep)
			}
			if !strings.Contains(got, base) {
				t.Fatalf("existing protocol lost in recovery handoff")
			}
			for _, want := range []string{"Dependency preparation did not complete", "Dependencies are not ready.", "report blocked with the reason"} {
				if !strings.Contains(got, want) {
					t.Errorf("recovery handoff missing %q: %q", want, got)
				}
			}
			if want := tc.prep.OutcomeSummary(); !strings.Contains(got, want) {
				t.Errorf("recovery handoff missing bounded summary %q: %q", want, got)
			}
			for _, sentinel := range []string{"binary missing", "install-stdout", "exit 1", "/bin/sh"} {
				if strings.Contains(got, sentinel) {
					t.Errorf("recovery prompt leaks raw installer text %q: %q", sentinel, got)
				}
			}
		})
	}
}

// TestDependencyPreparationWireShape pins the cross-repo ingest contract:
// a closed per-ecosystem object keyed only by detected ecosystems.
// Undetected ecosystems are omitted; unknown names never serialize.
func TestDependencyPreparationWireShape(t *testing.T) {
	t.Parallel()
	prep := &agent.DependencyPreparation{}
	prep.SetEcosystemOutcome("pnpm", agent.DependencyPreparationFailed)
	prep.SetEcosystemOutcome("go", agent.DependencyPreparationReady)
	prep.SetEcosystemOutcome("npm", agent.DependencyPreparationFailed)
	if got := prep.DetectedEcosystems(); len(got) != 2 || got[0] != "pnpm" || got[1] != "go" {
		t.Fatalf("DetectedEcosystems() = %v; want [pnpm go]", got)
	}
	// Unknown names are ignored: the object stays closed.
	if prep.EcosystemOutcome("npm") != "" {
		t.Fatalf("unknown ecosystem outcome = %q; want empty", prep.EcosystemOutcome("npm"))
	}
}

// recoveryTestSpec builds an autonomous pi spec carrying the runner-shaped
// harness protocol, so the prepared-harness apply tests below exercise the
// same prompt surfaces the runner delivers.
func recoveryTestSpec() agent.Spec {
	return agent.Spec{
		PromptMode:         agent.PromptModeAutonomous,
		Autonomous:         true,
		Model:              "gpt-test",
		SystemPromptAppend: "operator appendix",
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			HarnessProtocol:  &agent.PromptContent{ID: "runner-harness-protocol", Text: "operating protocol", Required: true},
			AuthorizedDowngrades: []agent.PromptDowngradeAuthorization{
				{ID: "runner-authorizes-protocol-to-user", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
			},
			UserPrompt: agent.PromptContent{ID: "runner-user-task", Text: "do the work", Required: true},
		},
	}
}

func compileRecoveryPlan(t *testing.T, source agent.Spec, manifest agent.HarnessManifest) *agent.PreparedHarness {
	t.Helper()
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, manifest, operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("CompilePreparedHarness: %v", err)
	}
	return plan
}

// TestApplyPreparedHarnessProjectsRecoveryAfterEqualityCheck is the B1
// contract at the agent layer: a receipt-bearing spec whose
// digest-exempt DependencyPreparation reports failed applies cleanly
// (no AuthorityDriftError — the field is outside the digest) and the
// adapted spec carries the bounded recovery handoff with the original
// task intact. Removing the post-check projection leaves a successful
// apply with no recovery text and fails it.
func TestApplyPreparedHarnessProjectsRecoveryAfterEqualityCheck(t *testing.T) {
	t.Parallel()
	manifest := (&pi.Provider{}).Manifest()
	source := recoveryTestSpec()
	plan := compileRecoveryPlan(t, source, manifest)

	for _, tc := range []struct {
		name      string
		prep      *agent.DependencyPreparation
		wantRecov bool
	}{
		{name: "failed", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed}, wantRecov: true},
		{name: "timed_out", prep: &agent.DependencyPreparation{Go: agent.DependencyPreparationTimedOut}, wantRecov: true},
		{name: "mixed", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed, Go: agent.DependencyPreparationReady}, wantRecov: true},
		{name: "ready adds nothing", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationReady, Go: agent.DependencyPreparationReady}},
		{name: "skipped adds nothing", prep: &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationSkippedSecurity}},
		{name: "nil adds nothing", prep: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			materialized := source
			materialized.PreparedHarness = plan
			materialized.DependencyPreparation = tc.prep
			before := materialized.PromptPlan.HarnessProtocol.Text
			adapted, err := agent.PrepareHarness(materialized, manifest)
			if err != nil {
				t.Fatalf("PrepareHarness with %+v: %v (want success: recovery must not disturb the authority digest)", tc.prep, err)
			}
			// The runner's source plan must be untouched: the projection
			// copies protocol content instead of mutating the shared plan.
			if materialized.PromptPlan.HarnessProtocol.Text != before {
				t.Fatalf("input spec was mutated through the shared prompt plan: %q", materialized.PromptPlan.HarnessProtocol.Text)
			}
			text := adapted.PromptPlan.HarnessProtocol.Text
			hasRecovery := strings.Contains(text, "Dependency preparation did not complete")
			if tc.wantRecov != hasRecovery {
				t.Fatalf("recovery present = %v; want %v (protocol %q)", hasRecovery, tc.wantRecov, text)
			}
			if tc.wantRecov {
				if !strings.Contains(text, "operating protocol") {
					t.Errorf("existing protocol lost in recovery projection: %q", text)
				}
				if !strings.Contains(text, tc.prep.OutcomeSummary()) {
					t.Errorf("recovery missing bounded summary %q: %q", tc.prep.OutcomeSummary(), text)
				}
				if !strings.Contains(adapted.SystemPromptAppend, "Dependency preparation did not complete") {
					t.Errorf("SystemPromptAppend lost recovery instruction: %q", adapted.SystemPromptAppend)
				}
				if !strings.Contains(adapted.PromptPlan.UserPrompt.Text, "do the work") {
					t.Errorf("original user task lost in adapted prompt plan: %+v", adapted.PromptPlan.UserPrompt)
				}
			} else if strings.Contains(adapted.SystemPromptAppend, "Dependency preparation did not complete") {
				t.Errorf("non-recovery outcome must leave SystemPromptAppend unchanged: %q", adapted.SystemPromptAppend)
			}
		})
	}
}

// TestApplyPreparedHarnessProjectsRecoveryOnNativeSurface is the
// prepared-lane delivery-surface contract: the recovery handoff must
// reach the native prompt the exact harness actually sends. A harness
// whose profile downgrades the operating protocol into the user prompt
// (OpenCode has no system surface) carries the handoff on Spec.Prompt;
// a harness with a native system surface (pi) carries it on
// SystemPromptAppend and leaves the user prompt free of duplication.
// Projecting only onto the post-adaptation protocol surfaces leaves
// OpenCode's native prompt without recovery and fails it.
func TestApplyPreparedHarnessProjectsRecoveryOnNativeSurface(t *testing.T) {
	t.Parallel()
	prep := &agent.DependencyPreparation{Pnpm: agent.DependencyPreparationFailed}
	for _, tc := range []struct {
		name     string
		manifest agent.HarnessManifest
		// wantPromptRecovery selects the native surface: true means the
		// handoff rides Spec.Prompt, false means SystemPromptAppend.
		wantPromptRecovery bool
	}{
		{name: "opencode downgraded protocol rides prompt", manifest: (&opencode.Provider{}).Manifest(), wantPromptRecovery: true},
		{name: "pi system protocol rides system surface", manifest: (&pi.Provider{}).Manifest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := recoveryTestSpec()
			plan := compileRecoveryPlan(t, source, tc.manifest)
			materialized := source
			materialized.PreparedHarness = plan
			materialized.DependencyPreparation = prep
			adapted, err := agent.PrepareHarness(materialized, tc.manifest)
			if err != nil {
				t.Fatalf("PrepareHarness: %v", err)
			}
			if !strings.Contains(adapted.Prompt, "do the work") {
				t.Fatalf("original user task lost in native prompt: %q", adapted.Prompt)
			}
			const marker = "Dependency preparation did not complete"
			if tc.wantPromptRecovery {
				if !strings.Contains(adapted.Prompt, marker) {
					t.Fatalf("native prompt missing recovery: %q", adapted.Prompt)
				}
				if !strings.Contains(adapted.Prompt, prep.OutcomeSummary()) {
					t.Errorf("native prompt missing bounded summary %q: %q", prep.OutcomeSummary(), adapted.Prompt)
				}
			} else {
				if !strings.Contains(adapted.SystemPromptAppend, marker) {
					t.Fatalf("system surface missing recovery: %q", adapted.SystemPromptAppend)
				}
				if strings.Contains(adapted.Prompt, marker) {
					t.Fatalf("user prompt must not duplicate system recovery: %q", adapted.Prompt)
				}
			}
		})
	}
}
