package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func realizationFixture(t *testing.T) CompiledCapabilityRealization {
	t.Helper()
	surface := []CapabilitySurfaceIdentity{{Kind: CapabilitySurfaceMCPServer, ID: "example"}, {Kind: CapabilitySurfaceMCPTool, ID: "draft_create"}}
	inputDigest := CapabilityRecipeInputDigest([]string{"example"})
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
	short.ObservationDigest = domainDigest("donmai.capability-realization.observation/v1", short)
	if _, err := CompileCapabilityRealization(c.Declaration, short); err == nil {
		t.Fatal("short surface compiled")
	}
}

func TestCapabilityRealizationPiNativeSurfaceNeedsNoMCPServer(t *testing.T) {
	surface := []CapabilitySurfaceIdentity{{Kind: CapabilitySurfaceNativeTool, ID: "draft_create"}}
	_, err := NewCapabilityRealization(CapabilityRealizationInput{CapabilityID: "example.native/v1", HarnessID: HarnessPi, AdapterVersion: "pi/interactive/tool-lifecycle-v4", Mode: PromptModeHumanControlled, RecipeID: "example/pi/v1", Entries: []CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
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
		r, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: "mcp-servers", Channel: ToolChannelMCPServer, InputDigest: b.Entries[0].InputDigest, Outcome: out}})
		if err == nil || r[0].Decision != "denied" {
			t.Fatalf("outcome %s became %+v", out, r)
		}
	}
}

func TestCapabilityRealizationArtifactBoundNotReady(t *testing.T) {
	c := realizationFixture(t)
	b := BindCapabilityRealization(c)
	r, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: "mcp-servers", Channel: ToolChannelMCPServer, InputDigest: b.Entries[0].InputDigest, Outcome: ToolOutcomeAdmitted}})
	if err != nil || r[0].Decision != "artifact_bound" {
		t.Fatalf("result=%+v err=%v", r, err)
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
