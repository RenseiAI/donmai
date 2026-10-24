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
	return codeIntelRealizationFixtureWithTools(t, capabilityID, harness, profile, mode, native, nil)
}

func codeIntelRealizationFixtureWithTools(t *testing.T, capabilityID string, harness agent.HarnessName, profile string, mode agent.PromptSessionMode, native bool, toolNames []string) (*agent.CapabilityRealizationRegistry, agent.CompiledCapabilityRealization) {
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
	selected := map[string]bool{}
	for _, name := range toolNames {
		selected[name] = true
	}
	for _, descriptor := range codeintelcontract.Descriptors() {
		if len(selected) == 0 || selected[descriptor.Name] {
			surface = append(surface, agent.CapabilitySurfaceIdentity{Kind: toolKind, ID: descriptor.Name})
		}
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
	resolved, err := binders.ResolveAndBind(realizations, codeIntelRequirementFacts(t, raw), json.RawMessage(raw), harness, profile, mode)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// codeIntelRequirementFacts derives admission facts from a well-formed payload;
// it fails the test when either digest cannot be computed.
func codeIntelRequirementFacts(t *testing.T, raw string) agent.CapabilityParameterRequirementFacts {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		t.Fatal(err)
	}
	parametersDigest, err := executioncell.DigestCapabilityParameters(members["codeIntel"])
	if err != nil {
		t.Fatal(err)
	}
	operationalDigest, err := executioncell.DigestOperationalPayload([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return agent.CapabilityParameterRequirementFacts{CapabilityID: "example.code-intelligence/v1", ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest}
}

// bindCodeIntelDirect calls the binder itself, bypassing the registry's own
// operational digest check, so each test pins the binder boundary alone.
func bindCodeIntelDirect(t *testing.T, raw string, facts agent.CapabilityParameterRequirementFacts) (CapabilityParameterBindResult, error) {
	t.Helper()
	_, compiled := codeIntelRealizationFixture(t, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	binder, err := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	if err != nil {
		t.Fatal(err)
	}
	return binder.Bind(CapabilityParameterBindingContext{Requirement: facts, OperationalPayload: json.RawMessage(raw), Realization: compiled})
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

func TestCodeIntelParameterBinderRefusesDuplicateJSONKeys(t *testing.T) {
	// Each duplicate-bearing payload decodes last-wins to its clean
	// counterpart, so the facts are exactly what an admitted clean payload
	// would carry. The clean control must bind; the duplicate must not.
	tests := []struct {
		name, clean, duplicate string
		secrets                []string
	}{
		{
			name:      "outer codeIntel member",
			clean:     `{"codeIntel":{"repoPath":"kept-path"}}`,
			duplicate: `{"codeIntel":{"repoPath":"shadowed-path"},"codeIntel":{"repoPath":"kept-path"}}`,
			secrets:   []string{"codeIntel", "shadowed-path", "kept-path"},
		},
		{
			name:      "sibling key",
			clean:     `{"codeIntel":{"tools":[]},"siblingNote":"kept-note"}`,
			duplicate: `{"codeIntel":{"tools":[]},"siblingNote":"shadowed-note","siblingNote":"kept-note"}`,
			secrets:   []string{"siblingNote", "shadowed-note", "kept-note"},
		},
		{
			name:      "nested sibling key",
			clean:     `{"codeIntel":{"tools":[]},"siblingMeta":{"depth":"kept-depth"}}`,
			duplicate: `{"codeIntel":{"tools":[]},"siblingMeta":{"depth":"shadowed-depth","depth":"kept-depth"}}`,
			secrets:   []string{"siblingMeta", "depth", "shadowed-depth", "kept-depth"},
		},
		{
			name:      "inside codeIntel",
			clean:     `{"codeIntel":{"repoPath":"kept-path"}}`,
			duplicate: `{"codeIntel":{"repoPath":"shadowed-path","repoPath":"kept-path"}}`,
			secrets:   []string{"repoPath", "shadowed-path", "kept-path"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := codeIntelRequirementFacts(t, tc.clean)
			if _, err := bindCodeIntelDirect(t, tc.clean, facts); err != nil {
				t.Fatalf("clean control refused: %v", err)
			}
			_, err := bindCodeIntelDirect(t, tc.duplicate, facts)
			if err == nil {
				t.Fatal("duplicate parameter key bound")
			}
			for _, secret := range tc.secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("refusal echoes payload content %q: %v", secret, err)
				}
			}
		})
	}
}

func TestCodeIntelParameterBinderRefusesInvalidUTF8Parameters(t *testing.T) {
	// encoding/json would bind these as U+FFFD replacements, and both
	// admission digests accept them, so only the raw-byte check refuses.
	tests := []struct{ name, clean, invalid string }{
		{name: "repoPath", clean: `{"codeIntel":{"repoPath":"pkg"}}`, invalid: "{\"codeIntel\":{\"repoPath\":\"pkg\xff\"}}"},
		{name: "repo", clean: `{"codeIntel":{"repo":"example/repo"}}`, invalid: "{\"codeIntel\":{\"repo\":\"example/\xffrepo\"}}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bindCodeIntelDirect(t, tc.clean, codeIntelRequirementFacts(t, tc.clean)); err != nil {
				t.Fatalf("clean control refused: %v", err)
			}
			result, err := bindCodeIntelDirect(t, tc.invalid, codeIntelRequirementFacts(t, tc.invalid))
			if err == nil {
				t.Fatalf("invalid UTF-8 parameter bound lossily: config=%s", result.Materialization.Config)
			}
			if strings.Contains(err.Error(), "\xff") || strings.Contains(err.Error(), "\uFFFD") {
				t.Fatalf("refusal echoes payload bytes: %q", err)
			}
		})
	}
}

func TestCodeIntelParameterBinderRefusesDigestMismatch(t *testing.T) {
	const payload = `{"codeIntel":{"repoPath":"pkg"}}`
	other := codeIntelRequirementFacts(t, `{"codeIntel":{"repoPath":"other"},"extra":true}`)
	tests := []struct {
		name   string
		mutate func(*agent.CapabilityParameterRequirementFacts)
	}{
		{name: "parameters digest", mutate: func(f *agent.CapabilityParameterRequirementFacts) { f.ParametersDigest = other.ParametersDigest }},
		{name: "operational payload digest", mutate: func(f *agent.CapabilityParameterRequirementFacts) {
			f.OperationalPayloadDigest = other.OperationalPayloadDigest
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := codeIntelRequirementFacts(t, payload)
			if _, err := bindCodeIntelDirect(t, payload, facts); err != nil {
				t.Fatalf("matching facts refused: %v", err)
			}
			tc.mutate(&facts)
			if _, err := bindCodeIntelDirect(t, payload, facts); err == nil {
				t.Fatal("payload bound against mismatched requirement facts")
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

func nativeRecipeRegistryFixture(t *testing.T, capability, raw string) (*agent.CapabilityRealizationRegistry, ResolvedCapabilityParameterBinding) {
	t.Helper()
	input, err := NewNativeCodeIntelRealizationInput(capability, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, testCodeIntelBinderDigest)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := agent.NewCapabilityRealization(input)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: input.Entries[0].EntryID, Channel: input.Entries[0].Channel, InputDigest: input.Entries[0].InputDigest}}, ObservedSurface: input.DeclaredSurface})
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
	binder, err := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	if err != nil {
		t.Fatal(err)
	}
	binders, err := NewCapabilityParameterBinderRegistry(binder)
	if err != nil {
		t.Fatal(err)
	}
	facts := codeIntelRequirementFacts(t, raw)
	facts.CapabilityID = capability
	resolved, err := binders.ResolveAndBind(registry, facts, json.RawMessage(raw), agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous)
	if err != nil {
		t.Fatal(err)
	}
	return registry, resolved
}

func TestNativeRecipeCompilesDeterministicExactFacts(t *testing.T) {
	cases := []struct {
		name, raw, repoPath string
		tools               []string
	}{
		{"allSix", `{"codeIntel":{"repoPath":"./pkg//sub"}}`, "pkg/sub", []string{"af_code_check_duplicate", "af_code_find_type_usages", "af_code_get_repo_map", "af_code_search_code", "af_code_search_symbols", "af_code_validate_cross_deps"}},
		{"subset", `{"codeIntel":{"repoPath":"pkg/sub","tools":["af_code_search_symbols","af_code_get_repo_map"]}}`, "pkg/sub", []string{"af_code_get_repo_map", "af_code_search_symbols"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", tc.raw)
			facts, digest, err := CompileNativeCodeIntelFacts("/abs/worktree", resolved)
			if err != nil || facts.Root != "/abs/worktree" || facts.RepoPath != tc.repoPath || strings.Join(facts.Tools, ",") != strings.Join(tc.tools, ",") {
				t.Fatalf("facts=%+v digest=%q err=%v", facts, digest, err)
			}
			again, againDigest, err := CompileNativeCodeIntelFacts("/abs/worktree", resolved)
			if err != nil || againDigest != digest || strings.Join(again.Tools, ",") != strings.Join(facts.Tools, ",") || again.Root != facts.Root || again.RepoPath != facts.RepoPath {
				t.Fatal("native facts are not deterministic")
			}
		})
	}
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	if _, _, err := CompileNativeCodeIntelFacts("", resolved); err == nil {
		t.Fatal("empty root compiled")
	}
}

func rebindNativeFactsWithValidDigests(t *testing.T, base ResolvedCapabilityParameterBinding, configTools, surfaceIDs []string) ResolvedCapabilityParameterBinding {
	t.Helper()
	rogueRaw, rogueDigest, err := codeintelbridge.CanonicalRuntimeConfig(codeintelbridge.RuntimeConfigV1{ContractVersion: codeintelbridge.RuntimeConfigVersion, RepoPath: "pkg/sub", Tools: configTools})
	if err != nil {
		t.Fatal(err)
	}
	surface := make([]agent.CapabilitySurfaceIdentity, 0, len(surfaceIDs))
	for _, id := range surfaceIDs {
		surface = append(surface, agent.CapabilitySurfaceIdentity{Kind: agent.CapabilitySurfaceNativeTool, ID: id})
	}
	selectedDigest, err := agent.CapabilitySelectedSurfaceDigest(surface)
	if err != nil {
		t.Fatal(err)
	}
	mutated := base
	bindingCopy := *mutated.Realization.ParameterBinding
	bindingCopy.SelectedSurface = surface
	bindingCopy.SelectedSurfaceDigest = selectedDigest
	bindingCopy.RuntimeConfigDigest = rogueDigest
	newBindingDigest, err := agent.CapabilityParameterBindingDigest(bindingCopy)
	if err != nil {
		t.Fatal(err)
	}
	bindingCopy.BindingDigest = newBindingDigest
	mutated.Realization.ParameterBinding = &bindingCopy
	mutated.Materialization.Config = rogueRaw
	mutated.Materialization.ConfigDigest = rogueDigest
	mutated.Materialization.BindingDigest = newBindingDigest
	return mutated
}

func TestNativeFactsRefuseUnadmittedToolWithValidDigests(t *testing.T) {
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	rogue := rebindNativeFactsWithValidDigests(t, resolved, []string{"no_such_tool"}, []string{"no_such_tool"})
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", rogue); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("unadmitted tool with valid digests compiled: err=%v", err)
	}
}

func TestNativeFactsRefuseSurfaceConfigDivergenceWithValidDigests(t *testing.T) {
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	rogue := rebindNativeFactsWithValidDigests(t, resolved, []string{"af_code_get_repo_map"}, []string{"af_code_get_repo_map", "af_code_search_symbols"})
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", rogue); err == nil || !strings.Contains(err.Error(), "surface and config differ") {
		t.Fatalf("surface/config divergence with valid digests compiled: err=%v", err)
	}
}

// nativeFactsOtherRepoConfig builds a self-consistent rogue config carrying
// the same admitted tools under a different subtree, for divergence probes.
func nativeFactsOtherRepoConfig(t *testing.T, resolved ResolvedCapabilityParameterBinding) (json.RawMessage, string) {
	t.Helper()
	base, _, err := codeintelbridge.DecodeRuntimeConfig(resolved.Materialization.Config)
	if err != nil {
		t.Fatal(err)
	}
	raw, digest, err := codeintelbridge.CanonicalRuntimeConfig(codeintelbridge.RuntimeConfigV1{ContractVersion: codeintelbridge.RuntimeConfigVersion, RepoPath: "other", Tools: base.Tools})
	if err != nil {
		t.Fatal(err)
	}
	return raw, digest
}

func TestNativeFactsRefuseMaterializationConfigDigestMismatch(t *testing.T) {
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	rogueRaw, rogueDigest := nativeFactsOtherRepoConfig(t, resolved)
	mutated := resolved
	mutated.Materialization.Config = rogueRaw
	mutated.Materialization.ConfigDigest = rogueDigest
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", mutated); err == nil || !strings.Contains(err.Error(), "binding identity differs") {
		t.Fatalf("materialization config drift compiled: err=%v", err)
	}
}

func TestNativeFactsRefuseMaterializationCapabilityMismatch(t *testing.T) {
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	mutated := resolved
	mutated.Materialization.CapabilityID = "example.other/v1"
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", mutated); err == nil || !strings.Contains(err.Error(), "binding identity differs") {
		t.Fatalf("materialization capability drift compiled: err=%v", err)
	}
}

func TestNativeFactsRefuseStaleMaterializationConfig(t *testing.T) {
	_, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	rogueRaw, _ := nativeFactsOtherRepoConfig(t, resolved)
	mutated := resolved
	mutated.Materialization.Config = rogueRaw
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", mutated); err == nil || !strings.Contains(err.Error(), "config differs") {
		t.Fatalf("stale materialization config compiled: err=%v", err)
	}
}

func TestNativeFactsRefusePolicyAndMismatch(t *testing.T) {
	registry, resolved := nativeRecipeRegistryFixture(t, "example.code-intelligence/v1", `{"codeIntel":{"repoPath":"pkg/sub"}}`)
	unknownRaw := `{"codeIntel":{"tools":["no_such_tool"]}}`
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	if _, err := binders.ResolveAndBind(registry, codeIntelRequirementFacts(t, unknownRaw), json.RawMessage(unknownRaw), agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous); err == nil {
		t.Fatal("unknown tool bound")
	}
	for _, root := range []string{"relative/path", "/abs//worktree", "/abs/worktree/../other"} {
		if _, _, err := CompileNativeCodeIntelFacts(root, resolved); err == nil {
			t.Fatalf("unclean root %q compiled", root)
		}
	}
	foreign := resolved
	foreign.Realization.ParameterContract = &agent.CapabilityParameterContractV1{ContractVersion: agent.CapabilityParameterContractVersionV1, ID: "foreign/contract/v1", BinderSourceDigest: testCodeIntelBinderDigest, SurfaceProjection: "subset", RuntimeObservation: "none"}
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", foreign); err == nil {
		t.Fatal("foreign contract compiled")
	}
	mismatched := resolved
	mismatched.Realization.ParameterBinding.SelectedSurface = append(append([]agent.CapabilitySurfaceIdentity(nil), resolved.Realization.ParameterBinding.SelectedSurface...), agent.CapabilitySurfaceIdentity{Kind: agent.CapabilitySurfaceNativeTool, ID: "af_code_search_symbols"})
	if _, _, err := CompileNativeCodeIntelFacts("/abs/worktree", mismatched); err == nil {
		t.Fatal("surface/config mismatch compiled")
	}
}

func TestNativeFactsLeaveHistoricalBytesUntouched(t *testing.T) {
	route, err := resolveCodeIntelDeliveryRoute(nil, nil)
	if err != nil || route.Route != codeIntelDeliveryLegacy || len(route.Tools) != 0 {
		t.Fatalf("legacy route drifted: %+v err=%v", route, err)
	}
}

func TestNativeRecipeCoexistsAndRefusesDuplicates(t *testing.T) {
	compile := func(capability string) agent.CompiledCapabilityRealization {
		input, err := NewNativeCodeIntelRealizationInput(capability, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, testCodeIntelBinderDigest)
		if err != nil {
			t.Fatal(err)
		}
		declaration, err := agent.NewCapabilityRealization(input)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: input.Entries[0].EntryID, Channel: input.Entries[0].Channel, InputDigest: input.Entries[0].InputDigest}}, ObservedSurface: input.DeclaredSurface})
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := agent.CompileCapabilityRealization(declaration, observation)
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}
	first := compile("example.code-intelligence/v1")
	second := compile("example.other/v1")
	if _, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{first, second}); err != nil {
		t.Fatalf("unrelated tuples refused: %v", err)
	}
	if _, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{first, first}); err == nil {
		t.Fatal("duplicate tuple compiled")
	}
	_, old := codeIntelRealizationFixture(t, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	if _, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{first, old}); err == nil {
		t.Fatal("conflicting tuple compiled")
	}
}
