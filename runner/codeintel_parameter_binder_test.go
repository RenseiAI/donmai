package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/codeintelbridge"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
)

const testCodeIntelBinderDigest = "abababababababababababababababababababababababababababababababab"

func codeIntelRealizationFixture(t *testing.T, harness agent.HarnessName, profile string, mode agent.PromptSessionMode, native bool) (*agent.CapabilityRealizationRegistry, agent.CompiledCapabilityRealization) {
	return codeIntelRealizationFixtureForCapability(t, "example.code-intelligence/v1", harness, profile, mode, native)
}

func codeIntelRealizationFixtureForCapability(t *testing.T, capabilityID string, harness agent.HarnessName, profile string, mode agent.PromptSessionMode, native bool) (*agent.CapabilityRealizationRegistry, agent.CompiledCapabilityRealization) {
	t.Helper()
	surface := make([]agent.CapabilitySurfaceIdentity, 0, 7)
	toolKind := agent.CapabilitySurfaceMCPTool
	entryID := "mcp-servers"
	channel := agent.ToolChannelMCPServer
	inputDigest := strings.Repeat("b", 64)
	artifacts := []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: channel, InputDigest: inputDigest}}
	if native {
		toolKind = agent.CapabilitySurfaceNativeTool
		delivery := codeintelbridge.Delivery()
		entryID, _ = agent.AdditionalExtensionCapabilityEntryID(delivery.ID)
		channel = agent.ToolChannelToolPlugin
		inputDigest = agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
		artifacts = []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: channel, InputDigest: inputDigest}}
	} else {
		surface = append(surface, agent.CapabilitySurfaceIdentity{Kind: agent.CapabilitySurfaceMCPServer, ID: codeintelcontract.ServerName})
	}
	for _, descriptor := range codeintelcontract.Descriptors() {
		surface = append(surface, agent.CapabilitySurfaceIdentity{Kind: toolKind, ID: descriptor.Name})
	}
	contract := &agent.CapabilityParameterContractV1{ContractVersion: agent.CapabilityParameterContractVersionV1, ID: codeintelbridge.ParameterContractID, BinderSourceDigest: testCodeIntelBinderDigest, SurfaceProjection: "subset", RuntimeObservation: "none"}
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: capabilityID, HarnessID: harness, AdapterVersion: profile, Mode: mode, RecipeID: "example/code-intelligence/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: entryID, Channel: channel, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}}, DeclaredSurface: surface, ParameterContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: artifacts, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	return registry, compiled
}

func codeIntelBindingFixture(t *testing.T, raw string, harness agent.HarnessName, profile string, mode agent.PromptSessionMode, native bool) ResolvedCapabilityParameterBinding {
	t.Helper()
	realizations, _ := codeIntelRealizationFixture(t, harness, profile, mode, native)
	binder, err := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	if err != nil {
		t.Fatal(err)
	}
	binders, err := NewCapabilityParameterBinderRegistry(binder)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(raw)
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		t.Fatal(err)
	}
	parametersDigest, err := executioncell.DigestCapabilityParameters(members["codeIntel"])
	if err != nil {
		t.Fatal(err)
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := binders.ResolveAndBind(realizations, agent.CapabilityParameterRequirementFacts{CapabilityID: "example.code-intelligence/v1", ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest}, payload, harness, profile, mode)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestCodeIntelParameterBinderProducesAtomicNativeResult(t *testing.T) {
	resolved := codeIntelBindingFixture(t, `{"codeIntel":{"repo":"example/repo","repoPath":"./pkg//sub","tools":["af_code_search_symbols","af_code_get_repo_map"]}}`, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	binding := resolved.Realization.ParameterBinding
	if binding == nil || binding.ParameterContractID != codeintelbridge.ParameterContractID || len(binding.SelectedSurface) != 2 || len(resolved.AdditionalExtensions) != 1 {
		t.Fatalf("resolved=%+v", resolved)
	}
	config, digest, err := codeintelbridge.DecodeRuntimeConfig(resolved.Materialization.Config)
	if err != nil || digest != binding.RuntimeConfigDigest || config.RepoPath != "pkg/sub" || len(config.Tools) != 2 || config.Tools[0] != "af_code_get_repo_map" {
		t.Fatalf("config=%+v digest=%q binding=%+v err=%v", config, digest, binding, err)
	}
	resolved.AdditionalExtensions[0].Source[0] = 'x'
	again := codeIntelBindingFixture(t, `{"codeIntel":{"tools":[]}}`, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	if len(again.Realization.ParameterBinding.SelectedSurface) != 6 || again.AdditionalExtensions[0].Source[0] == 'x' {
		t.Fatal("all-six/default result or source cloning failed")
	}
}

func TestCodeIntelParameterBinderRejectsStrictToolAndPathInputs(t *testing.T) {
	tests := []string{
		`{"codeIntel":{"tools":null}}`,
		`{"codeIntel":{"tools":[" af_code_get_repo_map"]}}`,
		`{"codeIntel":{"tools":[""]}}`,
		`{"codeIntel":{"tools":["af_code_get_repo_map","af_code_get_repo_map"]}}`,
		`{"codeIntel":{"tools":["af_code_get_repo_map","unknown"]}}`,
		`{"codeIntel":{"repoPath":"../outside"}}`,
		`{"codeIntel":{"repoPath":"/absolute"}}`,
		`{"codeIntel":{"repoPath":"C:/drive"}}`,
		`{"codeIntel":{"repoPath":"pkg\\child"}}`,
		`{"codeIntel":{"unknown":true}}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			realizations, _ := codeIntelRealizationFixture(t, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
			binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
			binders, _ := NewCapabilityParameterBinderRegistry(binder)
			payload := json.RawMessage(raw)
			var members map[string]json.RawMessage
			_ = json.Unmarshal(payload, &members)
			parametersDigest, _ := executioncell.DigestCapabilityParameters(members["codeIntel"])
			operationalDigest, _ := executioncell.DigestOperationalPayload(payload)
			if _, err := binders.ResolveAndBind(realizations, agent.CapabilityParameterRequirementFacts{CapabilityID: "example.code-intelligence/v1", ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest}, payload, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous); err == nil {
				t.Fatal("malformed parameter input bound")
			}
		})
	}
}

func TestSameBinderContractSupportsDistinctCapabilityMaterializations(t *testing.T) {
	_, first := codeIntelRealizationFixtureForCapability(t, "example.code-intelligence/a", agent.HarnessCodex, "codex/test/mcp-v1", agent.PromptModeAutonomous, false)
	_, second := codeIntelRealizationFixtureForCapability(t, "example.code-intelligence/b", agent.HarnessCodex, "codex/test/mcp-v1", agent.PromptModeAutonomous, false)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{first, second})
	if err != nil {
		t.Fatal(err)
	}
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	payload := json.RawMessage(`{"codeIntel":{"tools":["af_code_get_repo_map"]}}`)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(payload, &fields)
	parametersDigest, _ := executioncell.DigestCapabilityParameters(fields["codeIntel"])
	operationalDigest, _ := executioncell.DigestOperationalPayload(payload)
	resolved := make([]ResolvedCapabilityParameterBinding, 0, 2)
	for _, capability := range []string{"example.code-intelligence/a", "example.code-intelligence/b"} {
		value, err := binders.ResolveAndBind(realizations, agent.CapabilityParameterRequirementFacts{CapabilityID: capability, ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest}, payload, agent.HarnessCodex, "codex/test/mcp-v1", agent.PromptModeAutonomous)
		if err != nil {
			t.Fatal(err)
		}
		resolved = append(resolved, value)
	}
	if resolved[0].Materialization.ParameterContractID != resolved[1].Materialization.ParameterContractID || resolved[0].Materialization.BindingDigest == resolved[1].Materialization.BindingDigest {
		t.Fatalf("materializations=%+v", resolved)
	}
	spec := agent.Spec{ToolLifecyclePlan: &agent.ToolLifecyclePlan{ContractVersion: agent.ToolLifecycleContractVersion}}
	for _, value := range resolved {
		spec.ToolLifecyclePlan.CapabilityRealizations = append(spec.ToolLifecyclePlan.CapabilityRealizations, value.Realization)
		spec.CapabilityRuntimeMaterializations = append(spec.CapabilityRuntimeMaterializations, value.Materialization)
	}
	if err := validateCapabilityRuntimeMaterializations(spec); err != nil {
		t.Fatalf("shared binder contract rejected distinct capability bindings: %v", err)
	}
}
