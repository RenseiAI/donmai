package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func realizationFixture(t *testing.T, tier CapabilityRealizationEvidenceTier) CapabilityRealizationDeclaration {
	t.Helper()
	d, err := NewCapabilityRealization(CapabilityRealizationInput{
		CapabilityID: "example.workflow-authoring/v1", HarnessID: HarnessCodex,
		AdapterVersion: "codex/interactive/tool-lifecycle-v1", Mode: PromptModeHumanControlled,
		RecipeID:        "example/http-mcp/v1",
		Entries:         []CapabilityRecipeEntry{{EntryID: "mcp-servers", Channel: ToolChannelMCPServer, Required: true}},
		DeclaredSurface: CapabilityDeclaredSurface{MCPServerNames: []string{"example"}, MCPToolNames: []string{"draft_create", "draft_read"}},
		Evidence:        CapabilityRealizationEvidence{FixtureID: "fixture-real", FixtureDigest: strings.Repeat("a", 64), Tier: tier},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCapabilityRealizationCanonicalAndImmutable(t *testing.T) {
	d := realizationFixture(t, RealizationEvidenceRealBinary)
	r, err := NewCapabilityRealizationRegistry([]CapabilityRealizationDeclaration{d})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Resolve(d.CapabilityID, d.HarnessID, d.AdapterVersion, d.Mode)
	if !ok || !reflect.DeepEqual(got, d) || !got.ProductionEligible() {
		t.Fatalf("resolved = %+v,%v", got, ok)
	}
	d.Recipe.Entries[0].EntryID = "forged"
	again, _ := r.Resolve(got.CapabilityID, got.HarnessID, got.AdapterVersion, got.Mode)
	if again.Recipe.Entries[0].EntryID != "mcp-servers" {
		t.Fatal("registry aliased caller declaration")
	}
	forged := got
	forged.Recipe.RecipeDigest = strings.Repeat("0", 64)
	if _, err := NewCapabilityRealizationRegistry([]CapabilityRealizationDeclaration{forged}); err == nil {
		t.Fatal("forged digest admitted")
	}
}

func TestCapabilityRealizationProductionEligibilityDerivesFromEvidence(t *testing.T) {
	if realizationFixture(t, RealizationEvidenceFixture).ProductionEligible() {
		t.Fatal("fixture-only evidence became production eligible")
	}
}

func TestCapabilityRealizationPartialApplicationRefuses(t *testing.T) {
	b := BindCapabilityRealization(realizationFixture(t, RealizationEvidenceRealBinary))
	results, err := ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, nil)
	if err == nil || len(results) != 1 || results[0].Decision != "denied" {
		t.Fatalf("partial = %+v,%v", results, err)
	}
	results, err = ResolveCapabilityRealizationResults([]CapabilityRealizationBinding{b}, []ToolLifecycleEntry{{ID: "mcp-servers", Outcome: ToolOutcomeAdmitted}})
	if err != nil || results[0].Decision != "ready" {
		t.Fatalf("ready = %+v,%v", results, err)
	}
}

func TestCapabilityRealizationRidesExistingToolReceipt(t *testing.T) {
	d := realizationFixture(t, RealizationEvidenceRealBinary)
	plan := ToolLifecyclePlan{ContractVersion: ToolLifecycleContractVersion, CapabilityRealizations: []CapabilityRealizationBinding{BindCapabilityRealization(d)}}
	profile := ToolLifecycleProfile{ID: d.AdapterVersion, Mode: d.Mode, ToolPluginDelivery: ToolDeliveryUnsupported, MCPDelivery: ToolDeliveryCodexCLIMCPConfig, NativeToolPolicyDelivery: ToolDeliveryUnsupported, PermissionConfigDelivery: ToolDeliveryUnsupported, MCPToolPolicyDelivery: ToolDeliveryUnsupported, ToolHookDelivery: ToolDeliveryUnsupported, LifecycleDelivery: ToolDeliveryUnsupported, LifecycleFidelity: EvidenceUnsupported, ReplayDelivery: ToolDeliveryUnsupported, ReplayFidelity: EvidenceUnsupported, CleanupDelivery: ToolDeliveryUnsupported, EvidenceTier: "real_binary_verified", ProductionEligible: true}
	spec := Spec{MCPServers: []MCPServerConfig{{Name: "example", Type: "http", URL: "https://example.invalid/mcp"}}, ToolLifecyclePlan: &plan}
	_, receipt, err := AdaptToolLifecycle(spec, profile)
	if err != nil || receipt.Decision != "ready" || len(receipt.CapabilityRealizations) != 1 || receipt.CapabilityRealizations[0].Decision != "ready" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	plan.CapabilityRealizations[0].RequiredEntryIDs = []string{"missing"}
	spec.ToolLifecyclePlan = &plan
	_, receipt, err = AdaptToolLifecycle(spec, profile)
	if err == nil || receipt.Decision != "denied" || receipt.CapabilityRealizations[0].Decision != "denied" {
		t.Fatalf("partial receipt=%+v err=%v", receipt, err)
	}
}

func TestCapabilityRealizationAbsentPreservesV1JSON(t *testing.T) {
	plan, _ := json.Marshal(ToolLifecyclePlan{ContractVersion: ToolLifecycleContractVersion})
	receipt, _ := json.Marshal(ToolLifecycleReceipt{ContractVersion: ToolLifecycleContractVersion, Decision: "ready", Entries: []ToolLifecycleEntry{}})
	if string(plan) != `{"contractVersion":"donmai.tool-lifecycle/v1alpha1"}` {
		t.Fatalf("plan bytes changed: %s", plan)
	}
	if string(receipt) != `{"contractVersion":"donmai.tool-lifecycle/v1alpha1","profileId":"","decision":"ready","evidenceTier":"","productionEligible":false,"entries":[]}` {
		t.Fatalf("receipt bytes changed: %s", receipt)
	}
}
