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
