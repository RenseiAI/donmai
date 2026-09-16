package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/build"
	"reflect"
	"strings"
	"testing"
)

func realizationFixture(t *testing.T) CompiledCapabilityRealization {
	t.Helper()
	surface := []CapabilitySurfaceIdentity{{Kind: CapabilitySurfaceMCPServer, ID: "example"}, {Kind: CapabilitySurfaceMCPTool, ID: "draft_create"}}
	inputDigest, err := CapabilityRecipeInputDigest([]string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: "example.workflow-authoring/v1", HarnessID: HarnessCodex, AdapterVersion: "codex/interactive/tool-lifecycle-v1", Mode: PromptModeHumanControlled, RecipeID: "example/http-mcp/v1", Entries: []CapabilityRecipeEntry{{EntryID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}}, DeclaredSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	o, err := NewCapabilityFixtureObservation(CapabilityFixtureObservationInput{Declaration: d, FixtureID: "real-binary", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []CapabilityAppliedArtifact{{EntryID: "mcp-servers", Channel: ToolChannelMCPServer, InputDigest: inputDigest}}, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CompileCapabilityRealization(d, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCapabilityRealizationRequiresFixtureObservedSurface(t *testing.T) {
	c := realizationFixture(t)
	short := c.Observation
	short.ObservedSurface = short.ObservedSurface[:1]
	short.ObservationDigest = ""
	short.ObservationDigest, _ = observationDigest(short)
	if _, err := CompileCapabilityRealization(c.Declaration, short); err == nil {
		t.Fatal("short surface compiled")
	}
}

func TestCapabilityRealizationPiNativeSurfaceNeedsNoMCPServer(t *testing.T) {
	surface := []CapabilitySurfaceIdentity{{Kind: CapabilitySurfaceNativeTool, ID: "draft_create"}}
	_, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: "example.native/v1", HarnessID: HarnessPi, AdapterVersion: "pi/interactive/tool-lifecycle-v5", Mode: PromptModeHumanControlled, RecipeID: "example/pi/v1", Entries: []CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityRealizationRefusesOptionalAndPartial(t *testing.T) {
	c := realizationFixture(t)
	input := CapabilityRealizationInput{CapabilityID: c.Declaration.CapabilityID, HarnessID: c.Declaration.HarnessID, AdapterVersion: c.Declaration.AdapterVersion, Mode: c.Declaration.Mode, RecipeID: c.Declaration.Recipe.RecipeID, Entries: append([]CapabilityRecipeEntry(nil), c.Declaration.Recipe.Entries...), DeclaredSurface: c.Declaration.Recipe.DeclaredSurface}
	input.Entries[0].Required = false
	if _, err := NewCapabilityRealization(input); err == nil {
		t.Fatal("optional recipe compiled")
	}
	b := BindCapabilityRealization(c)
	for _, out := range []ToolAdaptationOutcome{ToolOutcomeDenied, ToolOutcomeDowngraded, ToolOutcomePendingRuntime} {
		r, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true, InputDigest: b.Entries[0].InputDigest, Outcome: out}})
		if err == nil || r[0].Decision != "denied" {
			t.Fatalf("outcome %s became %+v", out, r)
		}
	}
}

func TestCapabilityRealizationArtifactBoundNotReady(t *testing.T) {
	c := realizationFixture(t)
	b := BindCapabilityRealization(c)
	if b.FixtureID != c.Observation.FixtureID || b.BinaryDigest != c.Observation.BinaryDigest ||
		!reflect.DeepEqual(b.AppliedArtifacts, c.Observation.AppliedArtifacts) ||
		!reflect.DeepEqual(b.ObservedSurface, c.Observation.ObservedSurface) {
		t.Fatalf("binding omitted executable observation provenance: %+v", b)
	}
	r, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true, InputDigest: b.Entries[0].InputDigest, Outcome: ToolOutcomeAdmitted}})
	if err != nil || r[0].Decision != "artifact_bound" {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestCapabilityRealizationResultRejectsChangedProvenance(t *testing.T) {
	c := realizationFixture(t)
	base := BindCapabilityRealization(c)
	entry := ToolLifecycleEntry{ID: base.Entries[0].EntryID, Channel: base.Entries[0].Channel, Required: true, InputDigest: base.Entries[0].InputDigest, Outcome: ToolOutcomeAdmitted}
	tests := map[string]func(*CapabilityRealizationBinding){
		"adapter":          func(v *CapabilityRealizationBinding) { v.AdapterVersion = "pi/interactive/tool-lifecycle-v3" },
		"recipe":           func(v *CapabilityRealizationBinding) { v.RecipeDigest = strings.Repeat("b", 64) },
		"entry":            func(v *CapabilityRealizationBinding) { v.Entries[0].InputDigest = strings.Repeat("c", 64) },
		"observed surface": func(v *CapabilityRealizationBinding) { v.ObservedSurface = v.ObservedSurface[:1] },
		"binary":           func(v *CapabilityRealizationBinding) { v.BinaryDigest = strings.Repeat("d", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := BindCapabilityRealization(c)
			mutate(&candidate)
			if _, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{candidate}, []ToolLifecycleEntry{entry}); err == nil {
				t.Fatal("changed realization provenance was accepted")
			}
		})
	}
}

func TestCapabilityRealizationRefusesAdvisoryAppliedEntry(t *testing.T) {
	c := realizationFixture(t)
	b := BindCapabilityRealization(c)
	r, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: b.Entries[0].EntryID, Channel: b.Entries[0].Channel, Required: false, InputDigest: b.Entries[0].InputDigest, Outcome: ToolOutcomeAdmitted}})
	if err == nil || r[0].Decision != "denied" {
		t.Fatalf("advisory entry=%+v err=%v", r, err)
	}
}

func TestCapabilityRealizationAbsentPreservesV1JSON(t *testing.T) {
	p, _ := json.Marshal(ToolLifecyclePlan{ContractVersion: ToolLifecycleContractVersion})
	if string(p) != `{"contractVersion":"donmai.tool-lifecycle/v1alpha1"}` {
		t.Fatalf("%s", p)
	}
	r, _ := json.Marshal(ToolLifecycleReceipt{ContractVersion: ToolLifecycleContractVersion, Decision: "ready", Entries: []ToolLifecycleEntry{}})
	if string(r) != `{"contractVersion":"donmai.tool-lifecycle/v1alpha1","profileId":"","decision":"ready","evidenceTier":"","productionEligible":false,"entries":[]}` {
		t.Fatalf("%s", r)
	}
}

func TestCapabilityRealizationV1WireHashesRemainStable(t *testing.T) {
	compiled := realizationFixture(t)
	execution := CapabilityFixtureExecution{
		Producer:    CapabilityFixtureProducer{ID: "example.real-binary-fixture/v1", Source: []byte("actual fixture producer source"), ReleaseGate: "real-binary-no-skip"},
		Observation: compiled.Observation,
	}
	evidence, err := CompileCapabilityRealizationEvidence(compiled.Declaration, execution)
	if err != nil {
		t.Fatal(err)
	}
	binding := BindCapabilityRealization(compiled)
	results, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{binding}, []ToolLifecycleEntry{{ID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true, InputDigest: binding.Entries[0].InputDigest, Outcome: ToolOutcomeAdmitted}})
	if err != nil {
		t.Fatal(err)
	}
	profile := ToolLifecycleProfile{ID: "fixture/v1", Mode: PromptModeHumanControlled, ToolPluginDelivery: ToolDeliveryUnsupported, MCPDelivery: ToolDeliveryUnsupported, NativeToolPolicyDelivery: ToolDeliveryUnsupported, PermissionConfigDelivery: ToolDeliveryUnsupported, MCPToolPolicyDelivery: ToolDeliveryUnsupported, ToolHookDelivery: ToolDeliveryUnsupported, LifecycleDelivery: ToolDeliveryUnsupported, LifecycleFidelity: EvidenceUnsupported, ReplayDelivery: ToolDeliveryUnsupported, ReplayFidelity: EvidenceUnsupported, CleanupDelivery: ToolDeliveryUnsupported, EvidenceTier: "fixture"}
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"declaration", compiled.Declaration, "128da9e4d34bd773a6c54181f9daf3903effee570ed9f4d2ba01b21f68db2104"},
		{"observation", compiled.Observation, "aa29fabde7b2a8337a7f6b9b8e5d538c6af9ceb06592d727955763a6da6e27b9"},
		{"evidence", evidence, "90c395207ba8385e3197384d0000ff42ecf7de25730d06c344df114447d0af53"},
		{"binding", binding, "2fd32839ad5a5f1dff5360427f0f82dd1c5979d7a78d40ab588e649af3d316eb"},
		{"result", results[0], "cea6822945756aa24bd1416d90b911c8561fc279c45b078f5002665e54a43006"},
		{"profile", profile, "509731818758b6de74c75c82677768fb18e5473fb0d6ba13da20318957a3dd42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			if got := hex.EncodeToString(digest[:]); got != tc.want {
				t.Fatalf("v1 wire hash=%s want=%s wire=%s", got, tc.want, raw)
			}
		})
	}
}

func parameterizedRealizationFixture(t *testing.T, observationPolicy string) (CompiledCapabilityRealization, []byte) {
	t.Helper()
	binderSource := []byte("reviewed synthetic binder source")
	binderDigest := sha256.Sum256(binderSource)
	surface := []CapabilitySurfaceIdentity{{Kind: CapabilitySurfaceMCPTool, ID: "tool_a"}, {Kind: CapabilitySurfaceMCPTool, ID: "tool_b"}}
	inputDigest, err := CapabilityRecipeInputDigest([]string{"fixture"})
	if err != nil {
		t.Fatal(err)
	}
	contract := &CapabilityParameterContractV1{ContractVersion: CapabilityParameterContractVersionV1, ID: "example.parameter-binder/v1", BinderSourceDigest: hex.EncodeToString(binderDigest[:]), SurfaceProjection: "subset", RuntimeObservation: observationPolicy}
	declaration, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: "example.parameterized/v1", HarnessID: HarnessCodex, AdapterVersion: "codex/interactive/tool-lifecycle-v1", Mode: PromptModeHumanControlled, RecipeID: "example/parameterized/v1", Entries: []CapabilityRecipeEntry{{EntryID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}}, DeclaredSurface: surface, ParameterContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := NewCapabilityFixtureObservation(CapabilityFixtureObservationInput{Declaration: declaration, FixtureID: "synthetic-real-binary", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []CapabilityAppliedArtifact{{EntryID: "mcp-servers", Channel: ToolChannelMCPServer, InputDigest: inputDigest}}, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	return compiled, binderSource
}

func parameterBindingFixture(t *testing.T, compiled CompiledCapabilityRealization, selected []CapabilitySurfaceIdentity, parametersDigest, operationalDigest string) CapabilityParameterBindingV1 {
	t.Helper()
	selectedDigest, err := CapabilitySelectedSurfaceDigest(selected)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]CapabilityBoundEntryV1, len(compiled.Declaration.Recipe.Entries))
	for i, recipeEntry := range compiled.Declaration.Recipe.Entries {
		entryDigest, err := CapabilityEntryParametersDigest(recipeEntry.EntryID, recipeEntry.InputDigest, parametersDigest, selectedDigest, operationalDigest)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = CapabilityBoundEntryV1{EntryID: recipeEntry.EntryID, StaticInputDigest: recipeEntry.InputDigest, EntryParametersDigest: entryDigest}
	}
	binding := CapabilityParameterBindingV1{ContractVersion: CapabilityParameterBindingVersionV1, CapabilityID: compiled.Declaration.CapabilityID, ParameterContractID: compiled.Declaration.ParameterContract.ID, ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest, StaticRecipeDigest: compiled.Declaration.Recipe.RecipeDigest, SelectedSurface: append([]CapabilitySurfaceIdentity(nil), selected...), SelectedSurfaceDigest: selectedDigest, Entries: entries}
	binding.BindingDigest, err = CapabilityParameterBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestCapabilityParameterizedRealizationBindsTrustedContractEvidence(t *testing.T) {
	compiled, binderSource := parameterizedRealizationFixture(t, "none")
	execution := CapabilityFixtureExecution{Producer: CapabilityFixtureProducer{ID: "example.parameter-fixture/v1", Source: []byte("trusted parameter fixture producer"), BinderSource: binderSource, ReleaseGate: "parameter-fixture-no-skip"}, Observation: compiled.Observation}
	evidence, err := CompileCapabilityRealizationEvidence(compiled.Declaration, execution)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Evidence.TemplateDigest != compiled.Declaration.TemplateDigest || evidence.Compiled.Observation.TemplateDigest != compiled.Declaration.TemplateDigest {
		t.Fatalf("template digest not carried through evidence: %+v", evidence)
	}
	if err := ValidateCompiledCapabilityRealizationEvidence(evidence); err != nil {
		t.Fatal(err)
	}

	changedSource := []byte("changed synthetic binder source")
	changedDigest := sha256.Sum256(changedSource)
	changedContract := *compiled.Declaration.ParameterContract
	changedContract.BinderSourceDigest = hex.EncodeToString(changedDigest[:])
	changedDeclaration, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID, AdapterVersion: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode, RecipeID: compiled.Declaration.Recipe.RecipeID, Entries: compiled.Declaration.Recipe.Entries, DeclaredSurface: compiled.Declaration.Recipe.DeclaredSurface, ParameterContract: &changedContract})
	if err != nil {
		t.Fatal(err)
	}
	changedObservation, err := NewCapabilityFixtureObservation(CapabilityFixtureObservationInput{Declaration: changedDeclaration, FixtureID: compiled.Observation.FixtureID, BinaryDigest: compiled.Observation.BinaryDigest, AppliedArtifacts: compiled.Observation.AppliedArtifacts, ObservedSurface: compiled.Observation.ObservedSurface})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileCapabilityRealizationEvidence(changedDeclaration, CapabilityFixtureExecution{Producer: execution.Producer, Observation: changedObservation}); err == nil {
		t.Fatal("recomputed outer evidence inherited trusted binder source")
	}
	changedExecution := execution
	changedExecution.Producer.BinderSource = changedSource
	changedExecution.Observation = changedObservation
	changedEvidence, err := CompileCapabilityRealizationEvidence(changedDeclaration, changedExecution)
	if err != nil {
		t.Fatal(err)
	}
	if changedEvidence.Evidence.EvidenceDigest == evidence.Evidence.EvidenceDigest || changedDeclaration.TemplateDigest == compiled.Declaration.TemplateDigest {
		t.Fatal("changed binder source inherited evidence identity")
	}
}

func TestCapabilityParameterBindingRejectsMutationAndWidening(t *testing.T) {
	compiled, _ := parameterizedRealizationFixture(t, "none")
	parametersDigest, operationalDigest := strings.Repeat("b", 64), strings.Repeat("c", 64)
	requirement := CapabilityParameterRequirementFacts{CapabilityID: compiled.Declaration.CapabilityID, ParametersDigest: parametersDigest, OperationalPayloadDigest: operationalDigest}
	valid := parameterBindingFixture(t, compiled, compiled.Declaration.Recipe.DeclaredSurface[:1], parametersDigest, operationalDigest)
	if err := ValidateCapabilityParameterBinding(valid, compiled.Declaration, requirement); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*CapabilityParameterBindingV1){
		"capability":     func(v *CapabilityParameterBindingV1) { v.CapabilityID = "other/v1" },
		"parameters":     func(v *CapabilityParameterBindingV1) { v.ParametersDigest = strings.Repeat("d", 64) },
		"payload":        func(v *CapabilityParameterBindingV1) { v.OperationalPayloadDigest = strings.Repeat("e", 64) },
		"recipe":         func(v *CapabilityParameterBindingV1) { v.StaticRecipeDigest = strings.Repeat("f", 64) },
		"surface digest": func(v *CapabilityParameterBindingV1) { v.SelectedSurfaceDigest = strings.Repeat("0", 64) },
		"entry input":    func(v *CapabilityParameterBindingV1) { v.Entries[0].StaticInputDigest = strings.Repeat("1", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.SelectedSurface = append([]CapabilitySurfaceIdentity(nil), valid.SelectedSurface...)
			candidate.Entries = append([]CapabilityBoundEntryV1(nil), valid.Entries...)
			mutate(&candidate)
			for i := range candidate.Entries {
				candidate.Entries[i].EntryParametersDigest, _ = CapabilityEntryParametersDigest(candidate.Entries[i].EntryID, candidate.Entries[i].StaticInputDigest, candidate.ParametersDigest, candidate.SelectedSurfaceDigest, candidate.OperationalPayloadDigest)
			}
			candidate.BindingDigest, _ = CapabilityParameterBindingDigest(candidate)
			if err := ValidateCapabilityParameterBinding(candidate, compiled.Declaration, requirement); err == nil {
				t.Fatal("mutated binding validated")
			}
		})
	}
	widened := valid
	widened.SelectedSurface = append(append([]CapabilitySurfaceIdentity(nil), valid.SelectedSurface...), CapabilitySurfaceIdentity{Kind: CapabilitySurfaceMCPTool, ID: "zz_outside"})
	widened.SelectedSurfaceDigest, _ = CapabilitySelectedSurfaceDigest(widened.SelectedSurface)
	for i := range widened.Entries {
		widened.Entries[i].EntryParametersDigest, _ = CapabilityEntryParametersDigest(widened.Entries[i].EntryID, widened.Entries[i].StaticInputDigest, widened.ParametersDigest, widened.SelectedSurfaceDigest, widened.OperationalPayloadDigest)
	}
	widened.BindingDigest, _ = CapabilityParameterBindingDigest(widened)
	if err := ValidateCapabilityParameterBinding(widened, compiled.Declaration, requirement); err == nil {
		t.Fatal("surface widening validated")
	}
	empty := valid
	empty.SelectedSurface = nil
	empty.SelectedSurfaceDigest = strings.Repeat("0", 64)
	empty.BindingDigest, _ = CapabilityParameterBindingDigest(empty)
	if err := ValidateCapabilityParameterBinding(empty, compiled.Declaration, requirement); err == nil {
		t.Fatal("empty selected surface validated")
	}
	duplicate := valid
	duplicate.SelectedSurface = append(append([]CapabilitySurfaceIdentity(nil), valid.SelectedSurface...), valid.SelectedSurface[0])
	duplicate.SelectedSurfaceDigest = strings.Repeat("0", 64)
	duplicate.BindingDigest, _ = CapabilityParameterBindingDigest(duplicate)
	if err := ValidateCapabilityParameterBinding(duplicate, compiled.Declaration, requirement); err == nil {
		t.Fatal("duplicate selected surface validated")
	}
}

func TestCapabilityRealizationRawV1RejectsV2MemberPresence(t *testing.T) {
	compiled := realizationFixture(t)
	binding := BindCapabilityRealization(compiled)
	evidence, err := CompileCapabilityRealizationEvidence(compiled.Declaration, CapabilityFixtureExecution{Producer: CapabilityFixtureProducer{ID: "example.real-binary-fixture/v1", Source: []byte("actual fixture producer source"), ReleaseGate: "real-binary-no-skip"}, Observation: compiled.Observation})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		value  any
		field  string
		target any
	}{
		{"declaration null contract", compiled.Declaration, "parameterContract", &CapabilityRealizationDeclaration{}},
		{"declaration empty contract digest", compiled.Declaration, "parameterContractDigest", &CapabilityRealizationDeclaration{}},
		{"declaration empty template", compiled.Declaration, "templateDigest", &CapabilityRealizationDeclaration{}},
		{"observation empty template", compiled.Observation, "templateDigest", &CapabilityFixtureObservation{}},
		{"binding null contract", binding, "parameterContract", &CapabilityRealizationBinding{}},
		{"binding empty contract digest", binding, "parameterContractDigest", &CapabilityRealizationBinding{}},
		{"binding empty template", binding, "templateDigest", &CapabilityRealizationBinding{}},
		{"binding null parameters", binding, "parameterBinding", &CapabilityRealizationBinding{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.value)
			var members map[string]json.RawMessage
			_ = json.Unmarshal(raw, &members)
			if strings.Contains(tc.name, "empty") {
				members[tc.field] = json.RawMessage(`""`)
			} else {
				members[tc.field] = json.RawMessage(`null`)
			}
			mutated, _ := json.Marshal(members)
			if err := json.Unmarshal(mutated, tc.target); err == nil {
				t.Fatal("v1 decoder erased v2-only member presence")
			}
		})
	}
	raw, _ := json.Marshal(evidence)
	var row map[string]json.RawMessage
	_ = json.Unmarshal(raw, &row)
	var evidenceMembers map[string]json.RawMessage
	_ = json.Unmarshal(row["evidence"], &evidenceMembers)
	evidenceMembers["templateDigest"] = json.RawMessage(`null`)
	row["evidence"], _ = json.Marshal(evidenceMembers)
	mutated, _ := json.Marshal(row)
	if err := json.Unmarshal(mutated, &CompiledCapabilityRealizationEvidence{}); err == nil {
		t.Fatal("v1 evidence decoder erased template member presence")
	}
}

func TestCapabilityRealizationRawV2RequiresEveryVersionedMember(t *testing.T) {
	compiled, binderSource := parameterizedRealizationFixture(t, "none")
	parameterBinding := parameterBindingFixture(t, compiled, compiled.Declaration.Recipe.DeclaredSurface[:1], strings.Repeat("b", 64), strings.Repeat("c", 64))
	binding := BindCapabilityRealization(compiled)
	binding.ParameterBinding = &parameterBinding
	evidence, err := CompileCapabilityRealizationEvidence(compiled.Declaration, CapabilityFixtureExecution{Producer: CapabilityFixtureProducer{ID: "example.parameter-fixture/v1", Source: []byte("trusted parameter fixture producer"), BinderSource: binderSource, ReleaseGate: "parameter-fixture-no-skip"}, Observation: compiled.Observation})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		value     any
		fields    []string
		newTarget func() any
	}{
		{"declaration", compiled.Declaration, []string{"parameterContract", "parameterContractDigest", "templateDigest"}, func() any { return &CapabilityRealizationDeclaration{} }},
		{"observation", compiled.Observation, []string{"templateDigest"}, func() any { return &CapabilityFixtureObservation{} }},
		{"binding", binding, []string{"parameterContract", "parameterContractDigest", "templateDigest", "parameterBinding"}, func() any { return &CapabilityRealizationBinding{} }},
	}
	for _, tc := range tests {
		for _, field := range tc.fields {
			t.Run(tc.name+" missing "+field, func(t *testing.T) {
				raw, _ := json.Marshal(tc.value)
				var members map[string]json.RawMessage
				_ = json.Unmarshal(raw, &members)
				delete(members, field)
				mutated, _ := json.Marshal(members)
				if err := json.Unmarshal(mutated, tc.newTarget()); err == nil {
					t.Fatal("v2 decoder accepted missing versioned member")
				}
			})
		}
	}
	raw, _ := json.Marshal(evidence)
	var row map[string]json.RawMessage
	_ = json.Unmarshal(raw, &row)
	var evidenceMembers map[string]json.RawMessage
	_ = json.Unmarshal(row["evidence"], &evidenceMembers)
	delete(evidenceMembers, "templateDigest")
	row["evidence"], _ = json.Marshal(evidenceMembers)
	mutated, _ := json.Marshal(row)
	if err := json.Unmarshal(mutated, &CompiledCapabilityRealizationEvidence{}); err == nil {
		t.Fatal("v2 evidence decoder accepted missing template digest")
	}
}

func TestComposeCapabilityRealizationRegistriesRefusesDuplicateTuple(t *testing.T) {
	compiled := realizationFixture(t)
	first, _ := NewCapabilityRealizationRegistry([]CompiledCapabilityRealization{compiled})
	second, _ := NewCapabilityRealizationRegistry([]CompiledCapabilityRealization{compiled})
	if _, err := ComposeCapabilityRealizationRegistries(first, second); err == nil {
		t.Fatal("duplicate exact tuple composed")
	}
	composed, err := ComposeCapabilityRealizationRegistries(first, nil)
	if err != nil || !composed.Knows(compiled.Declaration.CapabilityID) {
		t.Fatalf("unique composition failed: %v", err)
	}
}

func TestAgentPackageDoesNotImportExecutionCell(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range pkg.Imports {
		if imported == "github.com/RenseiAI/donmai/executioncell" {
			t.Fatal("agent package imports executioncell and creates an authority/package cycle")
		}
	}
}

func TestCapabilityRealizationRegistryDeepCopiesNestedAuthority(t *testing.T) {
	c := realizationFixture(t)
	r, err := NewCapabilityRealizationRegistry([]CompiledCapabilityRealization{c})
	if err != nil {
		t.Fatal(err)
	}
	c.Declaration.Recipe.Entries[0].InputDigest = strings.Repeat("0", 64)
	got, _ := r.Resolve("example.workflow-authoring/v1", HarnessCodex, "codex/interactive/tool-lifecycle-v1", PromptModeHumanControlled)
	got.Declaration.Recipe.Entries[0].SurfaceRefs[0].ID = "mutated"
	again, _ := r.Resolve("example.workflow-authoring/v1", HarnessCodex, "codex/interactive/tool-lifecycle-v1", PromptModeHumanControlled)
	if again.Declaration.Recipe.Entries[0].InputDigest == strings.Repeat("0", 64) || again.Declaration.Recipe.Entries[0].SurfaceRefs[0].ID == "mutated" {
		t.Fatal("registry authority aliases caller memory")
	}
	b := BindCapabilityRealization(again)
	b.Entries[0].SurfaceRefs[0].ID = "bound-mutated"
	final, _ := r.Resolve(again.Declaration.CapabilityID, again.Declaration.HarnessID, again.Declaration.AdapterVersion, again.Declaration.Mode)
	if final.Declaration.Recipe.Entries[0].SurfaceRefs[0].ID == "bound-mutated" {
		t.Fatal("binding aliases registry")
	}
}

func TestCapabilityObservationRejectsMalformedRecomputedFields(t *testing.T) {
	c := realizationFixture(t)
	for _, mutate := range []func(*CapabilityFixtureObservation){func(o *CapabilityFixtureObservation) { o.ContractVersion = "invalid" }, func(o *CapabilityFixtureObservation) { o.BinaryDigest = "" }, func(o *CapabilityFixtureObservation) { o.FixtureID = "" }, func(o *CapabilityFixtureObservation) {
		o.AppliedArtifacts = append(o.AppliedArtifacts, o.AppliedArtifacts[0])
	}} {
		o := c.Observation
		mutate(&o)
		o.ObservationDigest, _ = observationDigest(o)
		if _, err := CompileCapabilityRealization(c.Declaration, o); err == nil {
			t.Fatal("malformed recomputed observation compiled")
		}
	}
}

func TestCapabilityRecipeInputDigestPropagatesMarshalErrors(t *testing.T) {
	if _, err := CapabilityRecipeInputDigest(func() {}); err == nil {
		t.Fatal("function digest succeeded")
	}
	if _, err := CapabilityRecipeInputDigest(make(chan int)); err == nil {
		t.Fatal("channel digest succeeded")
	}
}

func TestCapabilityRealizationEvidenceDerivesSmokedEligibility(t *testing.T) {
	compiled := realizationFixture(t)
	execution := CapabilityFixtureExecution{
		Producer: CapabilityFixtureProducer{
			ID:          "example.real-binary-fixture/v1",
			Source:      []byte("actual fixture producer source"),
			ReleaseGate: "real-binary-no-skip",
		},
		Observation: compiled.Observation,
	}
	evidenced, err := CompileCapabilityRealizationEvidence(compiled.Declaration, execution)
	if err != nil {
		t.Fatal(err)
	}
	if evidenced.EvidenceTier != CapabilityEvidenceSmoked || !evidenced.ProductionEligible {
		t.Fatalf("derived eligibility=%+v", evidenced)
	}
	producerDigest := sha256.Sum256(execution.Producer.Source)
	if evidenced.Evidence.ProducerID != execution.Producer.ID ||
		evidenced.Evidence.ProducerSourceDigest != hex.EncodeToString(producerDigest[:]) ||
		evidenced.Evidence.ReleaseGate != execution.Producer.ReleaseGate ||
		evidenced.Evidence.ObservationDigest != compiled.Observation.ObservationDigest ||
		!realizationDigest.MatchString(evidenced.Evidence.EvidenceDigest) {
		t.Fatalf("evidence omitted fixture provenance: %+v", evidenced.Evidence)
	}
	if err := ValidateCompiledCapabilityRealizationEvidence(evidenced); err != nil {
		t.Fatalf("validate compiled evidence: %v", err)
	}
	registry, err := NewCapabilityRealizationRegistryFromEvidence([]CompiledCapabilityRealizationEvidence{evidenced})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Resolve(compiled.Declaration.CapabilityID, compiled.Declaration.HarnessID, compiled.Declaration.AdapterVersion, compiled.Declaration.Mode); !ok {
		t.Fatal("validated generated row did not reach runtime registry")
	}
}

func TestCapabilityRealizationEvidenceRejectsAuthoredOrChangedProvenance(t *testing.T) {
	compiled := realizationFixture(t)
	valid := CapabilityFixtureExecution{
		Producer:    CapabilityFixtureProducer{ID: "example.real-binary-fixture/v1", Source: []byte("actual fixture producer source"), ReleaseGate: "real-binary-no-skip"},
		Observation: compiled.Observation,
	}
	tests := map[string]func(*CapabilityFixtureExecution){
		"missing producer":         func(v *CapabilityFixtureExecution) { v.Producer.ID = "" },
		"missing source":           func(v *CapabilityFixtureExecution) { v.Producer.Source = nil },
		"missing release gate":     func(v *CapabilityFixtureExecution) { v.Producer.ReleaseGate = "" },
		"wrong adapter":            func(v *CapabilityFixtureExecution) { v.Observation.AdapterVersion = "other/v1" },
		"wrong binary":             func(v *CapabilityFixtureExecution) { v.Observation.BinaryDigest = strings.Repeat("c", 64) },
		"wrong observation digest": func(v *CapabilityFixtureExecution) { v.Observation.ObservationDigest = strings.Repeat("d", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if _, err := CompileCapabilityRealizationEvidence(compiled.Declaration, candidate); err == nil {
				t.Fatal("changed fixture execution compiled")
			}
		})
	}

	evidenced, err := CompileCapabilityRealizationEvidence(compiled.Declaration, valid)
	if err != nil {
		t.Fatal(err)
	}
	generatedMutations := map[string]func(*CompiledCapabilityRealizationEvidence){
		"eligibility":   func(v *CompiledCapabilityRealizationEvidence) { v.ProductionEligible = false },
		"evidence tier": func(v *CompiledCapabilityRealizationEvidence) { v.EvidenceTier = "production_eligible" },
		"producer digest": func(v *CompiledCapabilityRealizationEvidence) {
			v.Evidence.ProducerSourceDigest = strings.Repeat("e", 64)
		},
		"release gate":      func(v *CompiledCapabilityRealizationEvidence) { v.Evidence.ReleaseGate = "other-gate" },
		"evidence digest":   func(v *CompiledCapabilityRealizationEvidence) { v.Evidence.EvidenceDigest = strings.Repeat("f", 64) },
		"observation tuple": func(v *CompiledCapabilityRealizationEvidence) { v.Evidence.ObservationDigest = strings.Repeat("0", 64) },
	}
	for name, mutate := range generatedMutations {
		t.Run("generated "+name, func(t *testing.T) {
			candidate := evidenced
			mutate(&candidate)
			if err := ValidateCompiledCapabilityRealizationEvidence(candidate); err == nil {
				t.Fatal("changed generated evidence validated")
			}
		})
	}
}
