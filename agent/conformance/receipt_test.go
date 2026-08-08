package conformance

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// headlessProfiles are a well-formed pair of adaptation profiles for the
// autonomous session mode — the shape a harness must declare before any
// prompt or tool policy can be admitted against it.
func headlessProfiles() ([]agent.PromptDeliveryProfile, []agent.ToolLifecycleProfile) {
	events := []agent.EventKind{
		agent.EventInit, agent.EventAssistantText, agent.EventToolUse,
		agent.EventToolResult, agent.EventResult, agent.EventError,
	}
	prompt := []agent.PromptDeliveryProfile{{
		ID: "fake/headless/prompt-v1", Mode: agent.PromptModeAutonomous,
		SystemDelivery:      agent.PromptDeliveryClaudeSystemAppend,
		BaseAppendDelivery:  agent.PromptDeliveryClaudeSystemAppend,
		BaseReplaceDelivery: agent.PromptDeliveryUnsupported,
		ContextDelivery:     agent.PromptDeliveryClaudeSystemAppend,
		UserDelivery:        agent.PromptDeliveryClaudeUserStdin,
		AmendmentDelivery:   agent.PromptDeliveryClaudeUserStdin,
	}}
	tools := []agent.ToolLifecycleProfile{{
		ID: "fake/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
		ToolPluginDelivery:       agent.ToolDeliveryUnsupported,
		MCPDelivery:              agent.ToolDeliveryClaudeMCPConfig,
		NativeToolPolicyDelivery: agent.ToolDeliveryClaudeCLIAllowDeny,
		PermissionConfigDelivery: agent.ToolDeliveryUnsupported,
		MCPToolPolicyDelivery:    agent.ToolDeliveryClaudeCLIAllowDeny,
		ToolHookDelivery:         agent.ToolDeliveryUnsupported,
		LifecycleDelivery:        agent.ToolDeliveryStructuredProviderEvents,
		LifecycleFidelity:        agent.EvidenceStructured,
		LifecycleEvents:          events,
		ReplayDelivery:           agent.ToolDeliveryStructuredEventReplay,
		ReplayFidelity:           agent.EvidenceStructured,
		ReplayEvents:             events,
		CleanupDelivery:          agent.ToolDeliveryHandleCleanup,
		EvidenceTier:             "unit_verified",
	}}
	return prompt, tools
}

func adaptableConfig() fakeConfig {
	return fakeConfig{notice: agent.NoticeDeliveryInBoxLoop, supportInject: true, inject: injectDeliver}
}

// TestRequiredMaterializationChannelsMatchAgentPackage pins this package's
// copy of the closed materialization-channel set against the agent package's
// unexported original. If the contract gains or loses a channel, this goes
// red here rather than silently denying every subject's row-10 tier.
func TestRequiredMaterializationChannelsMatchAgentPackage(t *testing.T) {
	t.Parallel()
	prompt, tools := headlessProfiles()
	manifest := agent.HarnessManifest{
		Name: "fake", ContractABI: "harness/v2", Family: agent.FamilyHarness,
		PromptDelivery: prompt, ToolLifecycle: tools,
	}
	spec := agent.Spec{Prompt: "do the thing"}
	digest := stableDigest(spec)

	materializations := make([]agent.HarnessMaterialization, 0, len(requiredMaterializationChannels))
	for _, channel := range requiredMaterializationChannels {
		materializations = append(materializations, agent.HarnessMaterialization{
			Channel: channel, SourceDigest: digest, Required: true,
		})
	}
	plan, err := agent.CompilePreparedHarness(spec, manifest, digest, nil, materializations)
	if err != nil {
		t.Fatalf("CompilePreparedHarness() = %v", err)
	}
	if err := agent.ValidatePreparedHarness(plan, digest); err != nil {
		t.Fatalf("the channel set this package mirrors is no longer the one agent.ValidatePreparedHarness requires: %v", err)
	}

	// And the mirror must be exact in the other direction too: dropping any
	// one channel has to be rejected, or the list could silently be a subset.
	for i := range requiredMaterializationChannels {
		partial := make([]agent.HarnessMaterialization, 0, len(materializations)-1)
		partial = append(partial, materializations[:i]...)
		partial = append(partial, materializations[i+1:]...)
		short, compileErr := agent.CompilePreparedHarness(spec, manifest, digest, nil, partial)
		if compileErr != nil {
			t.Fatalf("CompilePreparedHarness() without %q = %v", requiredMaterializationChannels[i], compileErr)
		}
		if agent.ValidatePreparedHarness(short, digest) == nil {
			t.Errorf("an authority missing channel %q validated; this package's mirror is a superset of what the contract requires", requiredMaterializationChannels[i])
		}
	}
}

func TestAdaptationReceiptTier(t *testing.T) {
	t.Parallel()
	secret := "sk-live-conformance-fixture-value"

	cases := []struct {
		name       string
		cfg        func() fakeConfig
		fixture    *AdaptationFixture
		wantCheck  CheckID
		wantStatus Status
		wantReason string
		wantTier   bool
	}{
		{
			name: "a complete fixture earns the tier",
			cfg:  adaptableConfig,
			fixture: &AdaptationFixture{
				Spec:         agent.Spec{Prompt: "do the thing", BaseInstructions: "be careful"},
				SecretValues: []string{secret},
			},
			wantCheck: IDReceiptPlanValid, wantStatus: StatusPass, wantTier: true,
		},
		{
			name:      "no fixture: not applicable, and the tier is not earned",
			cfg:       adaptableConfig,
			wantCheck: IDReceiptPlanValid, wantStatus: StatusNotApplicable,
			wantReason: "no Subject.Adaptation fixture",
		},
		{
			name: "a manifest with no prompt profile cannot compile an authority",
			cfg: func() fakeConfig {
				cfg := adaptableConfig()
				cfg.omitPromptProfile = true
				return cfg
			},
			fixture:   &AdaptationFixture{Spec: agent.Spec{Prompt: "do the thing"}, SecretValues: []string{secret}},
			wantCheck: IDReceiptPlanValid, wantStatus: StatusFail,
			wantReason: "compiling the adaptation authority failed",
		},
		{
			name: "a manifest with no tool profile cannot compile an authority",
			cfg: func() fakeConfig {
				cfg := adaptableConfig()
				cfg.omitToolProfile = true
				return cfg
			},
			fixture:   &AdaptationFixture{Spec: agent.Spec{Prompt: "do the thing"}, SecretValues: []string{secret}},
			wantCheck: IDReceiptPlanValid, wantStatus: StatusFail,
			wantReason: "compiling the adaptation authority failed",
		},
		{
			name: "claiming the PTY mode without a profile for it is caught",
			cfg: func() fakeConfig {
				cfg := adaptableConfig()
				cfg.interactivePTY = true
				return cfg
			},
			fixture:   &AdaptationFixture{Spec: agent.Spec{Prompt: "do the thing"}, SecretValues: []string{secret}},
			wantCheck: IDReceiptModeProfiles, wantStatus: StatusFail,
			wantReason: "human_controlled",
		},
		{
			name: "a secret smuggled into a runtime MCP name is caught",
			cfg:  adaptableConfig,
			fixture: &AdaptationFixture{
				Spec:            agent.Spec{Prompt: "do the thing"},
				RuntimeMCPNames: []string{"gateway-" + secret},
				SecretValues:    []string{secret},
			},
			wantCheck: IDReceiptSecretFree, wantStatus: StatusFail,
			wantReason: "appears verbatim in the serialized adaptation authority",
		},
		{
			name: "a fixture that declares no secrets cannot prove their absence",
			cfg:  adaptableConfig,
			fixture: &AdaptationFixture{
				Spec: agent.Spec{Prompt: "do the thing"},
			},
			wantCheck: IDReceiptSecretFree, wantStatus: StatusNotApplicable,
			wantReason: "vacuous pass",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			subject := conformantSubject(tc.cfg())
			subject.Adaptation = tc.fixture
			report := runSubject(t, subject)

			res := mustResult(t, report, tc.wantCheck)
			if res.Status != tc.wantStatus {
				t.Fatalf("%s = %q, want %q (reason %q)\n%s", tc.wantCheck, res.Status, tc.wantStatus, res.Reason, report.Text())
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("%s reason = %q, want it to contain %q", tc.wantCheck, res.Reason, tc.wantReason)
			}
			if got := report.Earned(TierAdaptationReceipt); got != tc.wantTier {
				t.Errorf("adaptation-receipt earned = %v, want %v\n%s", got, tc.wantTier, report.Text())
			}
		})
	}
}
