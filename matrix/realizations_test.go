package matrix

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestCompileCapabilityRealizationsConsumesObservation(t *testing.T) {
	surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "one"}}
	d, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: "example/v1", HarnessID: agent.HarnessPi, AdapterVersion: "pi/interactive/tool-lifecycle-v4", Mode: agent.PromptModeHumanControlled, RecipeID: "recipe/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	o, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: d, FixtureID: "real", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}}, ObservedSurface: surface})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CompileCapabilityRealizations([]CapabilityRealizationSource{{Declaration: d, Observation: o}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	o.ObservedSurface = nil
	o.ObservationDigest = ""
	if _, err := CompileCapabilityRealizations([]CapabilityRealizationSource{{Declaration: d, Observation: o}}); err == nil {
		t.Fatal("missing fixture surface compiled")
	}
}

func TestCompileCapabilityRealizationsOrdersFullTupleAndRejectsDuplicates(t *testing.T) {
	base := func(capability string, h agent.HarnessName, adapter string, mode agent.PromptSessionMode) CapabilityRealizationSource {
		surface := []agent.CapabilitySurfaceIdentity{{Kind: agent.CapabilitySurfaceNativeTool, ID: "one"}}
		d, _ := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: capability, HarnessID: h, AdapterVersion: adapter, Mode: mode, RecipeID: "recipe/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, Required: true, InputDigest: strings.Repeat("b", 64), SurfaceRefs: surface}}, DeclaredSurface: surface})
		o, _ := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{Declaration: d, FixtureID: "real", BinaryDigest: strings.Repeat("a", 64), AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: "additional-extensions", Channel: agent.ToolChannelToolPlugin, InputDigest: strings.Repeat("b", 64)}}, ObservedSurface: surface})
		return CapabilityRealizationSource{Declaration: d, Observation: o}
	}
	a := base("same/v1", agent.HarnessPi, "z/v1", agent.PromptModeHumanControlled)
	b := base("same/v1", agent.HarnessCodex, "a/v1", agent.PromptModeAutonomous)
	rows, err := CompileCapabilityRealizations([]CapabilityRealizationSource{a, b})
	if err != nil || rows[0].Compiled.Declaration.HarnessID != agent.HarnessCodex {
		t.Fatalf("order=%+v err=%v", rows, err)
	}
	if _, err := CompileCapabilityRealizations([]CapabilityRealizationSource{a, a}); err == nil {
		t.Fatal("duplicate tuple compiled")
	}
}
