package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func selectionFromCompiled(compiled agent.CompiledCapabilityRealization) executioncell.CapabilityRealizationSelectionV1 {
	return executioncell.CapabilityRealizationSelectionV1{
		ContractVersion:       executioncell.CapabilityRealizationSelectionContractVersionV1,
		CapabilityID:          compiled.Declaration.CapabilityID,
		HarnessID:             string(compiled.Declaration.HarnessID),
		AdapterVersion:        compiled.Declaration.AdapterVersion,
		Mode:                  string(compiled.Declaration.Mode),
		RecipeDigest:          compiled.Declaration.Recipe.RecipeDigest,
		DeclaredSurfaceDigest: compiled.Declaration.Recipe.DeclaredSurfaceDigest,
		ObservationDigest:     compiled.Observation.ObservationDigest,
	}
}

func admittedSelectionWork(t *testing.T) (QueuedWork, *agent.CapabilityRealizationRegistry, ProtectedRuntimeMCPSelector) {
	t.Helper()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	manifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	qw := exactReceiptQueuedWork("selection-bind")
	qw.PlatformURL, qw.McpAuthToken = "https://platform.example/", "session-bearer"
	compiled := protectedRuntimeMCPCompiled(t, protectedRuntimeMCPTestCapability, manifest, qw, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionFromCompiled(compiled)
	raw, err := executioncell.CanonicalJSON(map[string]any{
		"capabilityRealizationSelection": selection,
		"resolvedProfile":                map[string]any{"harness": string(agent.HarnessCodex), "model": "gpt-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = raw
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	return qw, realizations, ProtectedRuntimeMCPSelector{CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID, AdapterProfileID: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode}
}

func TestBindProtectedRuntimeMCPSelectionUsesRetainedRegistryEvidence(t *testing.T) {
	qw, realizations, selector := admittedSelectionWork(t)
	bound, err := BindProtectedRuntimeMCPSelection(qw, realizations, selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if bound.toolLifecycleProfileID != selector.AdapterProfileID {
		t.Fatalf("profile = %q, want %q", bound.toolLifecycleProfileID, selector.AdapterProfileID)
	}

	var payload map[string]any
	if err := json.Unmarshal(qw.OperationalPayload, &payload); err != nil {
		t.Fatal(err)
	}
	selection := payload["capabilityRealizationSelection"].(map[string]any)
	selection["observationDigest"] = strings.Repeat("0", 64)
	changed, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = changed
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	if _, err := BindProtectedRuntimeMCPSelection(qw, realizations, selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}); err == nil {
		t.Fatal("changed raw selection evidence was accepted")
	}
}

func TestProtectedRuntimeMCPDualSelectionPolicyRejectsAmbiguity(t *testing.T) {
	qw, realizations, selector := admittedSelectionWork(t)
	_ = qw
	dual := ProtectedRuntimeMCPDualSelectionPolicy{V1: selector, V2: ProtectedRuntimeMCPV2Selector{CapabilityID: selector.CapabilityID, HarnessID: selector.HarnessID, AdapterProfileID: selector.AdapterProfileID, Mode: selector.Mode, ConfigRequirementID: "example.session-config/v1"}}
	if err := validateProtectedRuntimeMCPDualSelectionPolicy(dual, realizations); err == nil {
		t.Fatal("dual policy accepted identical V1 and V2 adapter versions")
	}
}

func TestBindProtectedRuntimeMCPSelectionUsesExactV1AndV2PerSession(t *testing.T) {
	base := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	manifest := codexManifestForTest()
	profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing autonomous profile")
	}
	v2Profile := profile
	v2Profile.ID += "-v2"
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v2Profile)
	provider := &manifestSelectorProvider{selectorFakeProvider: base, manifest: manifest, capabilities: codexCapabilitiesForTest()}
	seed := exactReceiptQueuedWork("selection-dual")
	seed.PlatformURL, seed.McpAuthToken = "https://platform.example/", "session-bearer"
	v1 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, profile.ID, agent.PromptModeAutonomous)
	v2 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, v2Profile.ID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	policy := ProtectedRuntimeMCPDualSelectionPolicy{
		V1: ProtectedRuntimeMCPSelector{CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: agent.HarnessCodex, AdapterProfileID: profile.ID, Mode: agent.PromptModeAutonomous},
		V2: ProtectedRuntimeMCPV2Selector{CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: agent.HarnessCodex, AdapterProfileID: v2Profile.ID, Mode: agent.PromptModeAutonomous, ConfigRequirementID: "example.session-config/v1"},
	}
	for name, compiled := range map[string]agent.CompiledCapabilityRealization{"v1": v1, "v2": v2} {
		t.Run(name, func(t *testing.T) {
			qw := seed
			raw, err := executioncell.CanonicalJSON(map[string]any{"capabilityRealizationSelection": selectionFromCompiled(compiled)})
			if err != nil {
				t.Fatal(err)
			}
			qw.OperationalPayload = raw
			qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}))
			bound, err := BindProtectedRuntimeMCPSelection(qw, realizations, ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if bound.toolLifecycleProfileID != compiled.Declaration.AdapterVersion {
				t.Fatalf("profile = %q, want %q", bound.toolLifecycleProfileID, compiled.Declaration.AdapterVersion)
			}
		})
	}
}
