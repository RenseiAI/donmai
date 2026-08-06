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

func TestToolLifecycleAdapterMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		manifest   agent.HarnessManifest
		modes      []agent.PromptSessionMode
		mcp        bool
		policy     bool
		structured bool
	}{
		{"claude", (&claude.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}, true, true, true},
		{"codex", (&codex.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}, true, false, true},
		{"gemini", (&gemini.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, true, true},
		{"ollama", (&ollama.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, false, true},
		{"amp", (&amp.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, false, true},
		{"agy-cli", (&agycli.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, false, false},
		{"opencode", (&opencode.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, true, true, true},
		{"pi", (&pi.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeAutonomous}, false, true, true},
		{"shell", (&shell.Provider{}).Manifest(), []agent.PromptSessionMode{agent.PromptModeHumanControlled}, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := len(tc.manifest.ToolLifecycle), len(tc.modes); got != want {
				t.Fatalf("profile count = %d, want %d", got, want)
			}
			for _, mode := range tc.modes {
				profile, ok := tc.manifest.ToolLifecycleProfile(mode)
				if !ok {
					t.Fatalf("missing %s profile", mode)
				}
				if profile.ProductionEligible {
					t.Error("unit declarations must not imply production eligibility")
				}
				if got := profile.MCPDelivery != agent.ToolDeliveryUnsupported; got != tc.mcp && mode == agent.PromptModeAutonomous {
					t.Errorf("MCP support = %v, want %v", got, tc.mcp)
				}
				if got := profile.NativeToolPolicyDelivery != agent.ToolDeliveryUnsupported; got != tc.policy && mode == agent.PromptModeAutonomous {
					t.Errorf("native policy support = %v, want %v", got, tc.policy)
				}
				if got := profile.LifecycleFidelity == agent.EvidenceStructured; got != tc.structured && mode == agent.PromptModeAutonomous {
					t.Errorf("structured lifecycle = %v, want %v", got, tc.structured)
				}
			}
		})
	}
}

func TestToolLifecycleAdapterPositivePolicies(t *testing.T) {
	t.Parallel()
	stdio := agent.MCPServerConfig{Name: "tools", Command: "donmai", Args: []string{"mcp", "serve"}}
	policy := &agent.PermissionConfig{AllowPatterns: []string{"^git status$"}, DisallowPatterns: []string{"^git push"}, DefaultDecision: "deny"}
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		spec     agent.Spec
	}{
		{"claude", (&claude.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"codex", (&codex.Provider{}).Manifest(), agent.Spec{Autonomous: true, PermissionConfig: policy, MCPServers: []agent.MCPServerConfig{stdio}}},
		{"gemini", (&gemini.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"opencode", (&opencode.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, PermissionConfig: policy, MCPServers: []agent.MCPServerConfig{stdio}, MCPToolNames: []string{"mcp__tools__read"}}},
		{"pi", (&pi.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"}, PermissionConfig: policy}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, receipt, err := agent.AdaptToolLifecycle(tc.spec, mustProfile(t, tc.manifest, agent.PromptModeAutonomous))
			if err != nil {
				t.Fatalf("AdaptToolLifecycle: %v", err)
			}
			if receipt.Decision != "ready" || len(receipt.Entries) == 0 {
				t.Fatalf("receipt = %+v, want non-empty ready receipt", receipt)
			}
			for _, entry := range receipt.Entries {
				if entry.Outcome != agent.ToolOutcomeAdmitted {
					t.Errorf("entry %s outcome = %s, want admitted", entry.ID, entry.Outcome)
				}
			}
		})
	}
}

func TestToolLifecycleAdapterUnsupportedPoliciesDeny(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		spec     agent.Spec
		mode     agent.PromptSessionMode
	}{
		{"codex-flat-list", (&codex.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"amp", (&amp.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"agy-cli", (&agycli.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"ollama", (&ollama.Provider{}).Manifest(), agent.Spec{Autonomous: true, AllowedTools: []string{"Read"}}, agent.PromptModeAutonomous},
		{"shell", (&shell.Provider{}).Manifest(), agent.Spec{Interactive: &agent.InteractiveSpec{}, AllowedTools: []string{"Read"}}, agent.PromptModeHumanControlled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, receipt, err := agent.AdaptToolLifecycle(tc.spec, mustProfile(t, tc.manifest, tc.mode))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported || adaptationErr.Channel != agent.ToolChannelAllowedTools {
				t.Fatalf("error = %v, want typed allowed-tools denial", err)
			}
			if receipt.Decision != "denied" || receipt.Entries[len(receipt.Entries)-1].Outcome != agent.ToolOutcomeDenied {
				t.Fatalf("receipt = %+v, want denied entry", receipt)
			}
		})
	}
}

func TestToolLifecycleRuntimeEvidenceRequirementsDenyWithoutPromotion(t *testing.T) {
	t.Parallel()
	manifest := (&claude.Provider{}).Manifest()
	tests := []struct {
		name    string
		channel agent.ToolLifecycleChannel
		plan    agent.ToolLifecyclePlan
	}{
		{"lifecycle", agent.ToolChannelLifecycle, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, Lifecycle: []agent.LifecycleRequirement{{ID: "watch", Event: agent.EventInit, Required: true, MinimumFidelity: agent.EvidenceStructured}}}},
		{"replay", agent.ToolChannelReplay, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, Replay: &agent.LifecycleRequirement{ID: "replay", Event: agent.EventResult, Required: true, MinimumFidelity: agent.EvidenceStructured}}},
		{"cleanup", agent.ToolChannelCleanup, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, RequireCleanup: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &tc.plan}, mustProfile(t, manifest, agent.PromptModeAutonomous))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Channel != tc.channel || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported {
				t.Fatalf("error = %v, want typed %s denial", err, tc.channel)
			}
			if len(receipt.Entries) != 1 || receipt.Entries[0].Outcome != agent.ToolOutcomeDenied {
				t.Fatalf("receipt = %+v, want explicit denied entry", receipt)
			}
		})
	}
}

func TestToolLifecycleOptionalRuntimeEvidenceIsExplicitlyDeniedNotPending(t *testing.T) {
	t.Parallel()
	plan := agent.ToolLifecyclePlan{
		ContractVersion: agent.ToolLifecycleContractVersion,
		Lifecycle:       []agent.LifecycleRequirement{{ID: "watch", Event: agent.EventInit, MinimumFidelity: agent.EvidenceStructured}},
		Replay:          &agent.LifecycleRequirement{ID: "replay", Event: agent.EventResult, MinimumFidelity: agent.EvidenceStructured},
	}
	_, receipt, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &plan}, mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("AdaptToolLifecycle: %v", err)
	}
	if receipt.Decision != "ready" || len(receipt.Entries) != 2 {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, entry := range receipt.Entries {
		if entry.Outcome != agent.ToolOutcomeDenied {
			t.Fatalf("entry = %+v, want explicit denial rather than pending", entry)
		}
	}
}

func TestToolLifecycleFallbackCannotInventMissingEvent(t *testing.T) {
	t.Parallel()
	manifest := (&shell.Provider{}).Manifest()
	plan := agent.ToolLifecyclePlan{
		ContractVersion: agent.ToolLifecycleContractVersion,
		Lifecycle:       []agent.LifecycleRequirement{{ID: "tool-watch", Event: agent.EventToolUse, Required: true, MinimumFidelity: agent.EvidenceCoarse}},
		AuthorizedFallbacks: []agent.ToolLifecycleFallback{
			{ID: "coarse", Channel: agent.ToolChannelLifecycle, To: agent.ToolDeliveryCoarsePTYEvents},
		},
	}
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{Interactive: &agent.InteractiveSpec{}, ToolLifecyclePlan: &plan}, mustProfile(t, manifest, agent.PromptModeHumanControlled))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Channel != agent.ToolChannelLifecycle {
		t.Fatalf("error = %v, want missing tool event denial", err)
	}
}

func TestToolLifecycleReceiptIsDigestOnlyAndPersistenceFailsClosed(t *testing.T) {
	t.Parallel()
	secret := "REN2041_DO_NOT_LEAK"
	manifest := (&claude.Provider{}).Manifest()
	spec := agent.Spec{
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{Name: "secret", Command: "server", Env: map[string]string{"TOKEN": secret}}},
	}
	_, receipt, err := agent.AdaptToolLifecycle(spec, mustProfile(t, manifest, agent.PromptModeAutonomous))
	if err != nil {
		t.Fatalf("AdaptToolLifecycle: %v", err)
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("receipt leaked MCP environment secret: %s", body)
	}

	spec.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { return errors.New("store unavailable") }
	_, err = agent.PrepareToolLifecycle(spec, manifest)
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("PrepareToolLifecycle error = %v, want application-failed denial", err)
	}
}

func TestToolLifecycleDeniedReceiptPersistenceFailsClosed(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Autonomous:   true,
		AllowedTools: []string{"Read"},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			if receipt.Decision != "denied" {
				t.Fatalf("callback decision = %q, want denied", receipt.Decision)
			}
			return errors.New("store unavailable")
		},
	}
	_, err := agent.PrepareToolLifecycle(spec, (&amp.Provider{}).Manifest())
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("PrepareToolLifecycle error = %v, want application-failed denial", err)
	}
}

func TestToolLifecycleClosedDeliveryAndChannelEnums(t *testing.T) {
	t.Parallel()
	base := mustProfile(t, (&claude.Provider{}).Manifest(), agent.PromptModeAutonomous)
	unknownProfile := base
	unknownProfile.ToolPluginDelivery = agent.ToolDeliveryKind("future_delivery")
	tests := []struct {
		name    string
		profile agent.ToolLifecycleProfile
		plan    agent.ToolLifecyclePlan
	}{
		{"profile delivery", unknownProfile, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion}},
		{"fallback delivery", base, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, AuthorizedFallbacks: []agent.ToolLifecycleFallback{{ID: "f", Channel: agent.ToolChannelLifecycle, To: agent.ToolDeliveryKind("future_delivery")}}}},
		{"fallback channel", base, agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, AuthorizedFallbacks: []agent.ToolLifecycleFallback{{ID: "f", Channel: agent.ToolLifecycleChannel("future_channel"), To: agent.ToolDeliveryCoarsePTYEvents}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &tc.plan}, tc.profile)
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan {
				t.Fatalf("error = %v, want malformed-plan denial", err)
			}
		})
	}
}

func TestToolLifecycleUnusedPluginAndHookRequirementsDeny(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest agent.HarnessManifest
		plan     agent.ToolLifecyclePlan
		channel  agent.ToolLifecycleChannel
	}{
		{"pi plugin", (&pi.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, RequireToolPlugins: true}, agent.ToolChannelToolPlugin},
		{"gemini plugin", (&gemini.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, RequireToolPlugins: true}, agent.ToolChannelToolPlugin},
		{"pi hook", (&pi.Provider{}).Manifest(), agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion, ToolHooks: []agent.ToolHookRequirement{{ID: "audit", Kind: "pre_tool", Required: true}}}, agent.ToolChannelToolHook},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := agent.AdaptToolLifecycle(agent.Spec{Autonomous: true, ToolLifecyclePlan: &tc.plan}, mustProfile(t, tc.manifest, agent.PromptModeAutonomous))
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported || adaptationErr.Channel != tc.channel {
				t.Fatalf("error = %v, want typed %s denial", err, tc.channel)
			}
		})
	}
}

func TestToolLifecycleMalformedMCPFailsBeforeDelivery(t *testing.T) {
	t.Parallel()
	manifest := (&claude.Provider{}).Manifest()
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous: true,
		MCPServers: []agent.MCPServerConfig{{Name: "missing-command"}},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan || adaptationErr.Channel != agent.ToolChannelMCPServer {
		t.Fatalf("error = %v, want malformed MCP denial", err)
	}
}

func TestToolLifecycleMalformedPermissionRegexFailsBeforeDelivery(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	_, _, err := agent.AdaptToolLifecycle(agent.Spec{
		Autonomous:       true,
		PermissionConfig: &agent.PermissionConfig{AllowPatterns: []string{"("}},
	}, mustProfile(t, manifest, agent.PromptModeAutonomous))
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan || adaptationErr.Channel != agent.ToolChannelPermissionConfig {
		t.Fatalf("error = %v, want malformed permission denial", err)
	}
}

func mustProfile(t *testing.T, manifest agent.HarnessManifest, mode agent.PromptSessionMode) agent.ToolLifecycleProfile {
	t.Helper()
	profile, ok := manifest.ToolLifecycleProfile(mode)
	if !ok {
		t.Fatalf("manifest %s has no %s tool/lifecycle profile", manifest.Name, mode)
	}
	return profile
}
