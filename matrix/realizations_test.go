package matrix

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func matrixRealization(t *testing.T, tier agent.CapabilityRealizationEvidenceTier) agent.CapabilityRealizationDeclaration {
	t.Helper()
	d, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{CapabilityID: "example.capability/v1", HarnessID: agent.HarnessCodex, AdapterVersion: "codex/interactive/tool-lifecycle-v1", Mode: agent.PromptModeHumanControlled, RecipeID: "example.recipe/v1", Entries: []agent.CapabilityRecipeEntry{{EntryID: "mcp-servers", Channel: agent.ToolChannelMCPServer, Required: true}}, DeclaredSurface: agent.CapabilityDeclaredSurface{MCPServerNames: []string{"example"}, MCPToolNames: []string{"one"}}, Evidence: agent.CapabilityRealizationEvidence{FixtureID: "real", FixtureDigest: strings.Repeat("a", 64), Tier: tier}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCompileCapabilityRealizationsDerivesEligibility(t *testing.T) {
	rows, err := CompileCapabilityRealizations([]agent.CapabilityRealizationDeclaration{matrixRealization(t, agent.RealizationEvidenceRealBinary)})
	if err != nil || len(rows) != 1 || !rows[0].ProductionEligible {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if _, err := CompileCapabilityRealizations([]agent.CapabilityRealizationDeclaration{matrixRealization(t, agent.RealizationEvidenceFixture)}); err == nil {
		t.Fatal("fixture-only registration compiled")
	}
}
