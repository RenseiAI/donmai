package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

type syntheticParameterBinder struct {
	contractID   string
	sourceDigest string
	calls        int
	mutate       func(*agent.CapabilityParameterBindingV1)
}

func (b *syntheticParameterBinder) ContractID() string   { return b.contractID }
func (b *syntheticParameterBinder) SourceDigest() string { return b.sourceDigest }
func (b *syntheticParameterBinder) Bind(context CapabilityParameterBindingContext) (agent.CapabilityParameterBindingV1, error) {
	b.calls++
	selected := append([]agent.CapabilitySurfaceIdentity(nil), context.Realization.Declaration.Recipe.DeclaredSurface[:1]...)
	selectedDigest, err := agent.CapabilitySelectedSurfaceDigest(selected)
	if err != nil {
		return agent.CapabilityParameterBindingV1{}, err
	}
	entries := make([]agent.CapabilityBoundEntryV1, len(context.Realization.Declaration.Recipe.Entries))
	for i, recipeEntry := range context.Realization.Declaration.Recipe.Entries {
		entryDigest, err := agent.CapabilityEntryParametersDigest(recipeEntry.EntryID, recipeEntry.InputDigest, context.Requirement.ParametersDigest, selectedDigest, context.Requirement.OperationalPayloadDigest)
		if err != nil {
			return agent.CapabilityParameterBindingV1{}, err
		}
		entries[i] = agent.CapabilityBoundEntryV1{EntryID: recipeEntry.EntryID, StaticInputDigest: recipeEntry.InputDigest, EntryParametersDigest: entryDigest}
	}
	binding := agent.CapabilityParameterBindingV1{ContractVersion: agent.CapabilityParameterBindingVersionV1, CapabilityID: context.Requirement.CapabilityID, ParameterContractID: b.contractID, ParametersDigest: context.Requirement.ParametersDigest, OperationalPayloadDigest: context.Requirement.OperationalPayloadDigest, StaticRecipeDigest: context.Realization.Declaration.Recipe.RecipeDigest, SelectedSurface: selected, SelectedSurfaceDigest: selectedDigest, Entries: entries}
	if b.mutate != nil {
		b.mutate(&binding)
	}
	binding.BindingDigest, err = agent.CapabilityParameterBindingDigest(binding)
	return binding, err
}

func syntheticParameterizedRegistry(t *testing.T, harness agent.HarnessName, adapter string, mode agent.PromptSessionMode) (*agent.CapabilityRealizationRegistry, *syntheticParameterBinder) {
	t.Helper()
	binderSource := []byte("synthetic process-owned binder")
	binderDigest := sha256.Sum256(binderSource)
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceMCPTool, ID: "tool_a"}, {Kind: agent.CapabilitySurfaceMCPTool, ID: "tool_b"}}
	inputDigest, err := agent.CapabilityRecipeInputDigest([]string{"fixture"})
	if err != nil {
		t.Fatal(err)
	}
	contract := &agent.CapabilityParameterContractV1{ContractVersion: agent.CapabilityParameterContractVersionV1, ID: "example.parameter-binder/v1", BinderSourceDigest: hex.EncodeToString(binderDigest[:]), SurfaceProjection: "subset", RuntimeObservation: "none"}
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: "example.parameterized/v1", HarnessID: harness, AdapterVersion: adapter, Mode: mode, RecipeID: "example/parameterized/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}}, DeclaredSurface: surface, ParameterContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic-real-binary", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, InputDigest: inputDigest}}, ObservedSurface: surface})
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
	binder := &syntheticParameterBinder{contractID: contract.ID, sourceDigest: contract.BinderSourceDigest}
	return registry, binder
}

func TestCapabilityParameterBinderRegistryValidatesBeforeAndAfterBinder(t *testing.T) {
	realizations, binder := syntheticParameterizedRegistry(t, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	registry, err := NewCapabilityParameterBinderRegistry(binder)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"codeIntel":{"tools":["tool_a"]}}`)
	operationalDigest, err := executioncell.DigestOperationalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	requirement := agent.CapabilityParameterRequirementFacts{CapabilityID: "example.parameterized/v1", ParametersDigest: strings.Repeat("b", 64), OperationalPayloadDigest: operationalDigest}
	first, err := registry.ResolveAndBind(realizations, requirement, payload, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	if err != nil || first.ParameterBinding == nil || binder.calls != 1 {
		t.Fatalf("first binding=%+v calls=%d err=%v", first, binder.calls, err)
	}
	first.ParameterBinding.SelectedSurface[0].ID = "caller-mutated"
	second, err := registry.ResolveAndBind(realizations, requirement, payload, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	if err != nil || second.ParameterBinding.SelectedSurface[0].ID != "tool_a" || binder.calls != 2 {
		t.Fatalf("independent binding=%+v calls=%d err=%v", second, binder.calls, err)
	}

	wrongRequirement := requirement
	wrongRequirement.OperationalPayloadDigest = strings.Repeat("c", 64)
	if _, err := registry.ResolveAndBind(realizations, wrongRequirement, payload, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous); err == nil || binder.calls != 2 {
		t.Fatalf("wrong payload reached binder: calls=%d err=%v", binder.calls, err)
	}

	binder.mutate = func(binding *agent.CapabilityParameterBindingV1) { binding.ParametersDigest = strings.Repeat("d", 64) }
	if _, err := registry.ResolveAndBind(realizations, requirement, payload, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous); err == nil || binder.calls != 3 {
		t.Fatalf("forged binder result accepted: calls=%d err=%v", binder.calls, err)
	}
}

func TestCapabilityParameterBinderRegistryRefusesMalformedDuplicateAndDrift(t *testing.T) {
	_, binder := syntheticParameterizedRegistry(t, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	if _, err := NewCapabilityParameterBinderRegistry(nil); err == nil {
		t.Fatal("nil binder registered")
	}
	if _, err := NewCapabilityParameterBinderRegistry(binder, binder); err == nil {
		t.Fatal("duplicate binder registered")
	}
	malformed := *binder
	malformed.contractID = " invalid"
	if _, err := NewCapabilityParameterBinderRegistry(&malformed); err == nil {
		t.Fatal("malformed binder id registered")
	}
	registry, err := NewCapabilityParameterBinderRegistry(binder)
	if err != nil {
		t.Fatal(err)
	}
	binder.sourceDigest = strings.Repeat("0", 64)
	payload := json.RawMessage(`{}`)
	digest, _ := executioncell.DigestOperationalPayload(payload)
	realizations, _ := syntheticParameterizedRegistry(t, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	_, err = registry.ResolveAndBind(realizations, agent.CapabilityParameterRequirementFacts{CapabilityID: "example.parameterized/v1", ParametersDigest: strings.Repeat("b", 64), OperationalPayloadDigest: digest}, payload, agent.HarnessCodex, "codex/headless/tool-lifecycle-v1", agent.PromptModeAutonomous)
	if err == nil {
		t.Fatal("post-construction binder descriptor drift accepted")
	}
}

func TestPreparedHarnessUsesProcessOwnedParameterBinder(t *testing.T) {
	manifest := (&codex.Provider{}).Manifest()
	profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("codex autonomous profile missing")
	}
	realizations, binder := syntheticParameterizedRegistry(t, agent.HarnessCodex, profile.ID, agent.PromptModeAutonomous)
	binders, err := NewCapabilityParameterBinderRegistry(binder)
	if err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("parameterized-prepared-source")
	qw.Body = "exercise parameterized prepared source"
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.parameterized/v1", ParametersDigest: strings.Repeat("b", 64)}}))
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	provider := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: codexCapabilitiesForTest()}
	selection := harnessSelection{Provider: provider, receipt: mustAdmissionReceipt(t, qw.AdmissionReceipt), effectiveCell: exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.parameterized/v1", ParametersDigest: strings.Repeat("b", 64)}})}
	resolver := newParameterBoundCapabilityResolver(realizations, binders)
	spec, _, err := buildPreparedSourceSpec(qw, selection, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if binder.calls != 1 || spec.ToolLifecyclePlan == nil || len(spec.ToolLifecyclePlan.CapabilityRealizations) != 1 || spec.ToolLifecyclePlan.CapabilityRealizations[0].ParameterBinding == nil {
		t.Fatalf("prepared source omitted process-owned binding: calls=%d plan=%+v", binder.calls, spec.ToolLifecyclePlan)
	}
	if _, _, err := buildPreparedSourceSpec(qw, selection, nil, realizations); err == nil {
		t.Fatal("parameterized realization proceeded without process-owned binder registry")
	}
}
