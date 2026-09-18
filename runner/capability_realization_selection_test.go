package runner

import (
	"encoding/json"
	"strings"
	"sync/atomic"
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
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"recipeDigest", "declaredSurfaceDigest", "observationDigest"} {
		t.Run(field, func(t *testing.T) {
			var changedPayload map[string]any
			if err := json.Unmarshal(basePayload, &changedPayload); err != nil {
				t.Fatal(err)
			}
			selection := changedPayload["capabilityRealizationSelection"].(map[string]any)
			selection[field] = strings.Repeat("0", 64)
			changed, err := executioncell.CanonicalJSON(changedPayload)
			if err != nil {
				t.Fatal(err)
			}
			changedWork := qw
			changedWork.OperationalPayload = changed
			changedWork = attachAdmittedExecutionCell(t, changedWork, cell)
			if _, err := bindCapabilityRealizationSelection(changedWork, realizations, policy, false); err == nil {
				t.Fatalf("changed %s evidence was accepted", field)
			}
		})
	}
}

func TestBindProtectedRuntimeMCPSelectionRequiresAdmittedRawDigest(t *testing.T) {
	qw, realizations, selector := admittedSelectionWork(t)
	policy, err := newProtectedRuntimeMCPSelectionPolicy(selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}, realizations)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(qw.OperationalPayload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["body"] = "changed after admission"
	qw.OperationalPayload, err = executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindCapabilityRealizationSelection(qw, realizations, policy, false); err == nil || !strings.Contains(err.Error(), "operational payload digest mismatch") {
		t.Fatalf("changed retained raw digest error = %v", err)
	}
}

func TestConfiguredSelectionPolicyPreservesOnlyTrulyLegacyAbsentPayload(t *testing.T) {
	_, realizations, selector := admittedSelectionWork(t)
	policy, err := newProtectedRuntimeMCPSelectionPolicy(selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}, realizations)
	if err != nil {
		t.Fatal(err)
	}
	legacy := exactReceiptQueuedWork("ordinary-legacy")
	bound, err := bindCapabilityRealizationSelection(legacy, realizations, policy, false)
	if err != nil || bound.SessionID != legacy.SessionID {
		t.Fatalf("ordinary typed legacy work = %#v err=%v", bound, err)
	}

	admitted, _, _ := admittedSelectionWork(t)
	for name, mutate := range map[string]func(*QueuedWork){
		"admission": func(qw *QueuedWork) { qw.AdmissionReceipt = admitted.AdmissionReceipt },
		"claim":     func(qw *QueuedWork) { qw.ClaimReceipt = json.RawMessage(`{}`) },
		"effective": func(qw *QueuedWork) { qw.EffectiveCell = admitted.EffectiveCell },
		"runtime":   func(qw *QueuedWork) { qw.ExecutionRuntimeBinding = admitted.ExecutionRuntimeBinding },
		"host":      func(qw *QueuedWork) { qw.HostAdaptationReceipt = admitted.HostAdaptationReceipt },
	} {
		t.Run(name, func(t *testing.T) {
			partial := legacy
			mutate(&partial)
			if _, err := bindCapabilityRealizationSelection(partial, realizations, policy, false); err == nil {
				t.Fatal("partial authority without retained raw payload was accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*QueuedWork){
		"claim":     func(qw *QueuedWork) { qw.ClaimReceipt = json.RawMessage(`{}`) },
		"effective": func(qw *QueuedWork) { qw.EffectiveCell = admitted.EffectiveCell },
		"runtime":   func(qw *QueuedWork) { qw.ExecutionRuntimeBinding = admitted.ExecutionRuntimeBinding },
		"host":      func(qw *QueuedWork) { qw.HostAdaptationReceipt = admitted.HostAdaptationReceipt },
	} {
		t.Run("nonempty raw "+name, func(t *testing.T) {
			partial := legacy
			partial.OperationalPayload = json.RawMessage(`{}`)
			mutate(&partial)
			if _, err := bindCapabilityRealizationSelection(partial, realizations, policy, false); err == nil {
				t.Fatal("nonempty raw partial authority without admission was accepted")
			}
		})
	}
	for _, raw := range []json.RawMessage{[]byte(" "), []byte("null")} {
		changed := legacy
		changed.OperationalPayload = raw
		if _, err := bindCapabilityRealizationSelection(changed, realizations, policy, false); err == nil {
			t.Fatalf("explicit malformed payload %q was treated as absence", raw)
		}
	}
}

func TestRegistryPreflightHarnessValidatesPresentSelection(t *testing.T) {
	qw, realizations, _ := admittedSelectionWork(t)
	provider := &manifestSelectorProvider{
		selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		manifest:             codexManifestForTest(), capabilities: codexCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(qw.OperationalPayload, &payload); err != nil {
		t.Fatal(err)
	}
	selection := payload["capabilityRealizationSelection"].(map[string]any)
	selection["observationDigest"] = strings.Repeat("0", 64)
	malformed, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = malformed
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	if admission, err := registry.PreflightHarness(qw, realizations); err == nil {
		t.Fatalf("public preflight accepted malformed explicit selection: %+v", admission)
	}
}

func TestRegistryPreflightHarnessDerivesExplicitRegisteredProfile(t *testing.T) {
	manifest := codexManifestForTest()
	v1Profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing autonomous profile")
	}
	v2Profile := v1Profile
	v2Profile.ID += "-generic-explicit"
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v2Profile)
	provider := &manifestSelectorProvider{
		selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		manifest:             manifest, capabilities: codexCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("generic-explicit-profile")
	compiled := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, qw, v2Profile.ID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload, err = executioncell.CanonicalJSON(map[string]any{
		"capabilityRealizationSelection": selectionFromCompiled(compiled),
	})
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell(
		"harness/v2", "gpt-test", executioncell.SessionAutonomous,
		[]executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}},
	))
	qw = rewriteReadyHostToolProfile(t, qw, v2Profile.ID)
	if _, err := registry.PreflightHarness(qw, realizations); err != nil {
		t.Fatalf("public preflight did not derive explicit registered profile: %v", err)
	}
}

func TestConfigRequirementResolverHasZeroEffectsForInvalidSelection(t *testing.T) {
	legacyView, _, qw, provider, realizations := protectedRuntimeMCPPreflightFixture(t, true)
	selector := protectedRuntimeMCPTestSelector(t, provider, agent.PromptModeAutonomous)
	compiled, ok := realizations.Resolve(selector.CapabilityID, selector.HarnessID, selector.AdapterProfileID, selector.Mode)
	if !ok {
		t.Fatal("missing exact fixture realization")
	}
	basePayload, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(basePayload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["capabilityRealizationSelection"] = selectionFromCompiled(compiled)
	qw.OperationalPayload, err = executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	qw, detail := protectedRuntimeMCPPreflightInput(t, qw)
	var calls atomic.Int32
	view, err := NewProviderViewWithOptions(legacyView.reg, ProviderViewOptions{
		CapabilityRealizations:      realizations,
		ProtectedRuntimeMCPSelector: selector,
		ConfigRequirements: func(ExecutionPreflightConfigRequirementContext) ([]executioncell.PreflightConfigRequirementV1, error) {
			calls.Add(1)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(detail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.ResolveExecutionPreflightConfigRequirements(detail, receipt); err != nil || calls.Load() != 1 {
		t.Fatalf("valid baseline resolver = calls %d err=%v", calls.Load(), err)
	}
	calls.Store(0)
	changedSelection := selectionFromCompiled(compiled)
	changedSelection.RecipeDigest = strings.Repeat("0", 64)
	payload["capabilityRealizationSelection"] = changedSelection
	qw.OperationalPayload, err = executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	extracted, err := executioncell.ExtractCapabilityRealizationSelectionV1(qw.OperationalPayload)
	if err != nil || extracted == nil || extracted.RecipeDigest == compiled.Declaration.Recipe.RecipeDigest {
		t.Fatalf("invalid selection fixture was not retained: selection=%+v err=%v", extracted, err)
	}
	qw = attachAdmittedExecutionCell(t, qw, cell)
	if _, err := BindProtectedRuntimeMCPSelection(qw, realizations, selector, ProtectedRuntimeMCPV2Selector{}, ProtectedRuntimeMCPDualSelectionPolicy{}); err == nil || !strings.Contains(err.Error(), "exact local registry evidence") {
		t.Fatalf("invalid selection direct bind error = %v", err)
	}
	changedReceipt := append(json.RawMessage(nil), qw.HostAdaptationReceipt...)
	qw, changedDetail := protectedRuntimeMCPPreflightInput(t, qw)
	if _, err := view.ResolveExecutionPreflightConfigRequirements(changedDetail, changedReceipt); err == nil || !strings.Contains(err.Error(), "exact local registry evidence") {
		t.Fatalf("invalid selection resolver error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("configured common resolver calls = %d, want 0", calls.Load())
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
	v3Profile := profile
	v3Profile.ID += "-v3"
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v2Profile, v3Profile)
	provider := &manifestSelectorProvider{selectorFakeProvider: base, manifest: manifest, capabilities: codexCapabilitiesForTest()}
	seed := exactReceiptQueuedWork("selection-dual")
	seed.PlatformURL, seed.McpAuthToken = "https://platform.example/", "session-bearer"
	v1 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, profile.ID, agent.PromptModeAutonomous)
	v2 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, v2Profile.ID, agent.PromptModeAutonomous)
	v3 := protectedRuntimeMCPCompiledForProfile(t, protectedRuntimeMCPTestCapability, provider, seed, v3Profile.ID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{v1, v2, v3})
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
	third := seed
	thirdRaw, err := executioncell.CanonicalJSON(map[string]any{"capabilityRealizationSelection": selectionFromCompiled(v3)})
	if err != nil {
		t.Fatal(err)
	}
	third.OperationalPayload = thirdRaw
	third = attachAdmittedExecutionCell(t, third, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}))
	internal, err := newProtectedRuntimeMCPSelectionPolicy(ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy, realizations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindCapabilityRealizationSelection(third, realizations, internal, false); err == nil {
		t.Fatal("dual policy accepted a registered third adapter")
	}

	absent := seed
	absent.OperationalPayload = []byte(`{}`)
	absent = attachAdmittedExecutionCell(t, absent, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: protectedRuntimeMCPTestCapability}}))
	internal, err = newProtectedRuntimeMCPSelectionPolicy(ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, policy, realizations)
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
