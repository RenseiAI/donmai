package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func selectionFromCompiled(compiled agent.CompiledCapabilityRealization) executioncell.CapabilityRealizationSelectionV1 {
	return executioncell.CapabilityRealizationSelectionV1{
		ContractVersion:       executioncell.CapabilityRealizationSelectionContractVersionV1,
		CapabilityID:          compiled.Declaration.CapabilityID,
		HarnessID:             string(compiled.Declaration.HarnessID),
		AdapterVersion:        compiled.Declaration.AdapterVersion,
		Mode:                  string(compiled.Declaration.Mode),
		RecipeDigest:          compiled.Declaration.Recipe.RecipeDigest,
		DeclaredSurfaceDigest: compiled.Declaration.Recipe.DeclaredSurfaceDigest,
		ObservationDigest:     compiled.Observation.ObservationDigest,
	}
}

func rewriteReadyHostToolProfile(t *testing.T, qw QueuedWork, profileID string) QueuedWork {
	t.Helper()
	host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
	if err != nil {
		t.Fatal(err)
	}
	var plan agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	plan.ToolLifecycleReceipt.ProfileID = profileID
	planRaw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	toolRaw, err := json.Marshal(plan.ToolLifecycleReceipt)
	if err != nil {
		t.Fatal(err)
	}
	host.Plan = planRaw
	host.PlanDigest = agent.DigestPreparedHarness(&plan)
	host.ToolLifecycleReceipt = toolRaw
	raw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	qw.HostAdaptationReceipt = raw
	return qw
}

func admittedSelectionWork(t *testing.T) (QueuedWork, *agent.CapabilityRealizationRegistry, ProtectedRuntimeMCPSelector) {
	t.Helper()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	manifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	qw := exactReceiptQueuedWork("selection-bind")
	qw.PlatformURL, qw.McpAuthToken = "https://platform.example/", "session-bearer"
	compiled := protectedRuntimeMCPCompiled(t, protectedRuntimeMCPTestCapability, manifest, qw, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionFromCompiled(compiled)
	raw, err := executioncell.CanonicalJSON(map[string]any{
		"capabilityRealizationSelection": selection,
		"resolvedProfile":                map[string]any{"harness": string(agent.HarnessCodex), "model": "gpt-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = raw
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	return qw, realizations, ProtectedRuntimeMCPSelector{CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID, AdapterProfileID: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode}
}

func TestBindProtectedRuntimeMCPSelectionUsesRetainedRegistryEvidence(t *testing.T) {
	qw, realizations, selector := admittedSelectionWork(t)
	policy, err := newProtectedRuntimeMCPSelectionPolicy(selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}, realizations)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindCapabilityRealizationSelection(qw, realizations, policy, false)
	if err != nil {
		t.Fatal(err)
	}
	if bound.toolLifecycleProfileID != selector.AdapterProfileID {
		t.Fatalf("profile = %q, want %q", bound.toolLifecycleProfileID, selector.AdapterProfileID)
	}

	basePayload, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(basePayload, &payload); err != nil {
		t.Fatal(err)
	}
	selection := payload["capabilityRealizationSelection"].(map[string]any)
	selection["observationDigest"] = strings.Repeat("0", 64)
	changed, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = changed
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	if _, err := bindCapabilityRealizationSelection(qw, realizations, policy, false); err == nil {
		t.Fatal("changed raw selection evidence was accepted")
	}
}

func TestProtectedRuntimeMCPDualSelectionPolicyRejectsAmbiguity(t *testing.T) {
	qw, realizations, selector := admittedSelectionWork(t)
	_ = qw
	dual := ProtectedRuntimeMCPDualSelectionPolicy{V1: selector, V2: ProtectedRuntimeMCPV2Selector{CapabilityID: selector.CapabilityID, HarnessID: selector.HarnessID, AdapterProfileID: selector.AdapterProfileID, Mode: selector.Mode, ConfigRequirementID: "example.session-config/v1"}}
	if err := validateProtectedRuntimeMCPDualSelectionPolicy(dual, realizations); err == nil {
		t.Fatal("dual policy accepted identical V1 and V2 adapter versions")
	}
}

func TestBindProtectedRuntimeMCPSelectionUsesExactV1AndV2PerSession(t *testing.T) {
	base := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	manifest := codexManifestForTest()
	profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing autonomous profile")
	}
	v2Profile := profile
	v2Profile.ID += "-v2"
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v2Profile)
	provider := &manifestSelectorProvider{selectorFakeProvider: base, manifest: manifest, capabilities: codexCapabilitiesForTest()}
	seed := exactReceiptQueuedWork("selection-dual")
	seed.PlatformURL, seed.McpAuthToken = "https://platform.example/", "session-bearer"
	v1 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, profile.ID, agent.PromptModeAutonomous)
	v2 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, v2Profile.ID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	policy := ProtectedRuntimeMCPDualSelectionPolicy{
		V1: ProtectedRuntimeMCPSelector{CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: agent.HarnessCodex, AdapterProfileID: profile.ID, Mode: agent.PromptModeAutonomous},
		V2: ProtectedRuntimeMCPV2Selector{CapabilityID: protectedRuntimeMCPTestCapability, HarnessID: agent.HarnessCodex, AdapterProfileID: v2Profile.ID, Mode: agent.PromptModeAutonomous, ConfigRequirementID: "example.session-config/v1"},
	}
	for name, compiled := range map[string]agent.CompiledCapabilityRealization{"v1": v1, "v2": v2} {
		t.Run(name, func(t *testing.T) {
			qw := seed
			raw, err := executioncell.CanonicalJSON(map[string]any{"capabilityRealizationSelection": selectionFromCompiled(compiled)})
			if err != nil {
				t.Fatal(err)
			}
			qw.OperationalPayload = raw
			qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}))
			internal, err := newProtectedRuntimeMCPSelectionPolicy(ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy, realizations)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := bindCapabilityRealizationSelection(qw, realizations, internal, false)
			if err != nil {
				t.Fatal(err)
			}
			if bound.toolLifecycleProfileID != compiled.Declaration.AdapterVersion {
				t.Fatalf("profile = %q, want %q", bound.toolLifecycleProfileID, compiled.Declaration.AdapterVersion)
			}
			if name == "v2" {
				legacy, err := newProtectedRuntimeMCPSelectionPolicy(policy.V1, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}, realizations)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := bindCapabilityRealizationSelection(qw, realizations, legacy, false); err == nil {
					t.Fatal("modern V1-only consumer accepted explicit V2 selection")
				}
				mismatched := rewriteReadyHostToolProfile(t, qw, profile.ID)
				if _, err := BindProtectedRuntimeMCPSelection(mismatched, realizations, ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy); err == nil {
					t.Fatal("child accepted V2 selection with self-consistent V1 host profile")
				}
			}
		})
	}

	absent := seed
	absent.OperationalPayload = []byte(`{}`)
	absent = attachAdmittedExecutionCell(t, absent, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}))
	internal, err := newProtectedRuntimeMCPSelectionPolicy(ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy, realizations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindCapabilityRealizationSelection(absent, realizations, internal, false); err == nil {
		t.Fatal("dual default accepted targeted absent selection")
	}
	policy.HistoricalAbsence = ProtectedRuntimeMCPHistoricalAbsenceV1
	internal, err = newProtectedRuntimeMCPSelectionPolicy(ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy, realizations)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindCapabilityRealizationSelection(absent, realizations, internal, false)
	if err != nil || bound.toolLifecycleProfileID != profile.ID {
		t.Fatalf("historical V1 binding = profile %q err=%v, want %q nil", bound.toolLifecycleProfileID, err, profile.ID)
	}
}

func TestDualSelectionLeavesExplicitPiTupleOutsideCodexMaterialization(t *testing.T) {
	fixture := newProtectedRuntimeMCPMixedFixture(t)
	manifest := fixture.codexProvider.manifest
	v1Profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeHumanControlled)
	if !ok {
		t.Fatal("missing Codex human profile")
	}
	v2Profile := v1Profile
	v2Profile.ID += "-v2"
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v2Profile)
	codex := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	if err := registry.Register(fixture.piProvider); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(codex); err != nil {
		t.Fatal(err)
	}
	seed := protectedRuntimeMCPMixedCodexWork(t, fixture.selector.CapabilityID, executioncell.SessionHumanControlled)
	v2 := protectedRuntimeMCPCompiledForProfile(t, fixture.selector.CapabilityID, codex, seed, v2Profile.ID, agent.PromptModeHumanControlled)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{fixture.piRow, fixture.codexHumanRow, v2})
	if err != nil {
		t.Fatal(err)
	}
	policy := ProtectedRuntimeMCPDualSelectionPolicy{
		V1: fixture.selector,
		V2: ProtectedRuntimeMCPV2Selector{CapabilityID: fixture.selector.CapabilityID, HarnessID: fixture.selector.HarnessID, AdapterProfileID: v2Profile.ID, Mode: fixture.selector.Mode, ConfigRequirementID: "example.session-config/v1"},
	}
	qw := protectedRuntimeMCPMixedPiWork(t, fixture.selector.CapabilityID)
	basePayload, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(basePayload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["capabilityRealizationSelection"] = selectionFromCompiled(fixture.piRow)
	raw, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = raw
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	_, detail := protectedRuntimeMCPPreflightInput(t, qw)
	view, err := NewProviderViewWithOptions(registry, ProviderViewOptions{Decorator: fixture.decorate, CapabilityRealizations: realizations, ProtectedRuntimeMCPDualSelectionPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	hostReceipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatalf("host rejected explicit Pi tuple: %v", err)
	}
	qw.HostAdaptationReceipt = hostReceipt
	bound, err := BindProtectedRuntimeMCPSelection(qw, realizations, ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy)
	if err != nil {
		t.Fatalf("child rejected explicit Pi tuple: %v", err)
	}
	if bound.toolLifecycleProfileID != fixture.piRow.Declaration.AdapterVersion {
		t.Fatalf("Pi tuple profile = %q, want its exact adapter %q", bound.toolLifecycleProfileID, fixture.piRow.Declaration.AdapterVersion)
	}
}

func TestProtectedRuntimeMCPV2MaterializationUsesPerWorkProfile(t *testing.T) {
	v2 := ProtectedRuntimeMCPV2Selector{CapabilityID: "example.capability/v1", HarnessID: agent.HarnessCodex, AdapterProfileID: "codex/profile-v2", Mode: agent.PromptModeAutonomous, ConfigRequirementID: "example.session-config/v1"}
	r := &Runner{protectedRuntimeMCPV2Selector: v2}
	selection := harnessSelection{Harness: executioncell.HarnessRef{ID: string(agent.HarnessCodex)}, effectiveCell: executioncell.ResolvedExecutionCell{SessionMode: executioncell.SessionAutonomous}}
	qw := QueuedWork{toolLifecycleProfileID: "codex/profile-v1"}
	if r.protectedRuntimeMCPV2Applies(qw, selection) {
		t.Fatal("V1 work selected V2 materialization from process-global configuration")
	}
	qw.toolLifecycleProfileID = v2.AdapterProfileID
	if !r.protectedRuntimeMCPV2Applies(qw, selection) {
		t.Fatal("V2 work did not select its V2 materialization")
	}
}
