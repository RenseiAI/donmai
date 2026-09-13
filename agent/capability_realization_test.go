package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
