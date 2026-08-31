package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// piManifestForTest and piCapabilitiesForTest mirror codexManifestForTest /
// codexCapabilitiesForTest (executioncell_adaptation_test.go) for the pi
// harness — the reported shape below is human-controlled pi.
func piManifestForTest() agent.HarnessManifest { return (&pi.Provider{}).Manifest() }

func piCapabilitiesForTest() agent.Capabilities { return (&pi.Provider{}).Capabilities() }

// piReceiptCell mirrors exactReceiptCell (executioncell_adaptation_test.go)
// but names the pi harness — exactReceiptCell hardcodes codex.
func piReceiptCell(harnessVersion, model string, mode executioncell.SessionMode, capabilities []executioncell.CapabilityRequirement) executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessPi), Version: harnessVersion},
		Model:           executioncell.ModelRef{ID: model, Author: "openai"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "endpoint:openai/direct", Protocol: string(agent.ProtoOpenAIChat), Operator: "vercel", Revision: "2026-08-06",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "auth-binding:byok", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled,
			Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment,
		},
		Placement:   executioncell.PlacementRef{ID: "host_test", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode: mode, GrantedCapabilities: append([]executioncell.CapabilityRequirement{}, capabilities...), EvidenceTier: executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
}

// resolvedProfileFixtureJSON builds the raw JSON a daemon.SessionResolvedProfile
// would marshal to, carrying the fields ReconcileResolvedProfile reads:
// provider/model/effort plus the receipt-bearing endpoint identity
// ("Receipt-bearing work must carry it explicitly" — session_detail.go). The
// endpoint models this issue's reported shape: an openai-chat endpoint, byok
// api_key auth (host "direct" — pi's manifest does not yet declare
// agent.HostGateway in DrivesHosts; see provider/harness/pi/manifest.go's
// "DRIFT vs design" comment — "direct" is what the shipped pi manifest
// actually drives).
func resolvedProfileFixtureJSON(t *testing.T, model string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"provider": "pi",
		"model":    model,
		"effort":   "high",
		"endpoint": map[string]any{
			"company": "openai", "model": model, "protocol": "openai-chat", "host": "direct",
			"endpointId": "endpoint:openai/direct", "endpointOperator": "vercel", "endpointRevision": "2026-08-06",
			"modelAuthor": "openai", "authBindingId": "auth-binding:byok", "authAuthority": "openai",
			"authCommercialMode": "usage_billed", "authBindingScope": "process", "authPortability": "portable",
			"authDelivery": "environment", "mechanism": "api_key",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestPreflightAndSpawnAgreeForHumanControlledPiWithSiblingResolvedProfile is
// a regression test reproducing a real production shape: a receipt-bearing,
// human-controlled pi session whose Model/Endpoint arrive on SessionDetail's
// sibling ResolvedProfile field (never embedded in OperationalPayload — see
// resolvedProfileWire's doc comment and session_detail.go's ResolvedProfile
// field comment, "Receipt-bearing work must carry it explicitly").
//
// Before this fix, ProviderView.PreflightExecution never read that sibling
// field (daemon.go's preflightInput only forwarded OperationalPayload),
// so the host-compiled plan's Model/Endpoint came from OperationalPayload's
// own (here: absent) ResolvedProfile mirror, while afcli.detailToQueuedWork
// authoritatively overwrote the SPAWNED qw.ResolvedProfile from the sibling
// field. ApplyPreparedHarness's authority digest could then never agree —
// this test's first sub-test proves it now does, by running the exact
// reconciliation (ReconcileResolvedProfile) on BOTH the preflight side (via
// PreflightExecution) and the simulated spawn side with the SAME sibling
// JSON. The control proves the digest check still fails, naming "model",
// when a genuine authority difference (not a reconciliation gap) is
// introduced independently at spawn.
func TestPreflightAndSpawnAgreeForHumanControlledPiWithSiblingResolvedProfile(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	providerWithManifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: piManifestForTest(), capabilities: piCapabilitiesForTest()}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}

	const realModel = "gpt-real-routed-model"
	buildBaseQW := func() QueuedWork {
		qw := QueuedWork{}
		qw.SessionID = "host-preflight-pi-human-sibling-profile"
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = "actual human-controlled initial turn"
		// The capability pack / prompt-adaptation surface: an inline skill
		// body folds into the SystemPromptAppend composition
		// (buildPreparedSourceSpec -> foldInlineSkills), so its digest field
		// is genuinely exercised by this shape, not left at its zero value.
		qw.Skills = []prompt.SkillSpec{{ID: "capability-pack-skill", Body: "capability pack tool-use guidance"}}
		// ResolvedProfile is deliberately left at its zero value here: the
		// OperationalPayload's own resolvedProfile mirror carries no
		// Model/Endpoint for this dispatch (the realistic "platform writes
		// only the sibling field for receipt-bearing work" shape) — the
		// sibling JSON below is the only place realModel/the endpoint
		// identity appear.
		return qw
	}

	baseQW := buildBaseQW()
	baseQW = attachAdmittedExecutionCell(t, baseQW, piReceiptCell("harness/v2", realModel, executioncell.SessionHumanControlled, nil))
	operational, err := CanonicalOperationalPayload(baseQW)
	if err != nil {
		t.Fatal(err)
	}
	baseQW.OperationalPayload = operational

	resolvedProfileJSON := resolvedProfileFixtureJSON(t, realModel)
	detail := map[string]any{
		"sessionId": baseQW.SessionID, "workerId": baseQW.WorkerID, "admissionReceipt": baseQW.AdmissionReceipt,
		"effectiveCell": baseQW.EffectiveCell, "executionRuntimeBinding": baseQW.ExecutionRuntimeBinding,
		"operationalPayload": baseQW.OperationalPayload,
		// The sibling fields daemon.go's preflightInput now forwards —
		// SessionDetail.ModelProfile/ResolvedProfile, never embedded in
		// OperationalPayload itself.
		"resolvedProfile": resolvedProfileJSON,
	}

	t.Run("preflight and spawn agree once both reconcile the sibling profile", func(t *testing.T) {
		receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail))
		if err != nil {
			t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
		}
		host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var plan agent.PreparedHarness
		if err := json.Unmarshal(host.Plan, &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != agent.PromptModeHumanControlled || plan.Harness != string(agent.HarnessPi) {
			t.Fatalf("host compiled wrong identity: %+v", plan)
		}

		// Simulate the SPAWNED child: afcli.detailToQueuedWork decodes
		// OperationalPayload into qw (the zero-value ResolvedProfile, same
		// as baseQW), then reconciles the SAME sibling resolvedProfile JSON
		// the platform sent — exactly what ReconcileResolvedProfile does.
		spawnQW, err := ReconcileResolvedProfile(buildBaseQW(), nil, resolvedProfileJSON)
		if err != nil {
			t.Fatalf("ReconcileResolvedProfile (spawn side): %v", err)
		}
		spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
		spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
		spawnQW.WorkerID = baseQW.WorkerID
		if spawnQW.ResolvedProfile.Model != realModel {
			t.Fatalf("spawn-side reconciliation did not apply the sibling profile: %+v", spawnQW.ResolvedProfile)
		}

		source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
			Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
			effectiveCell: piReceiptCell("harness/v2", realModel, executioncell.SessionHumanControlled, nil),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if source.Model != realModel {
			t.Fatalf("spawn Spec.Model = %q, want %q (the sibling profile's model)", source.Model, realModel)
		}
		source.PreparedHarness = &plan

		if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
			t.Fatalf("ApplyPreparedHarness must pass once preflight and spawn reconcile the same sibling profile: %v", err)
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
		}
	})

	t.Run("control: a genuine model authority difference at spawn still fails, naming model", func(t *testing.T) {
		receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail))
		if err != nil {
			t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
		}
		host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var plan agent.PreparedHarness
		if err := json.Unmarshal(host.Plan, &plan); err != nil {
			t.Fatal(err)
		}

		spawnQW, err := ReconcileResolvedProfile(buildBaseQW(), nil, resolvedProfileJSON)
		if err != nil {
			t.Fatalf("ReconcileResolvedProfile (spawn side): %v", err)
		}
		spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
		spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
		spawnQW.WorkerID = baseQW.WorkerID
		// A GENUINE authority drift, unrelated to profile reconciliation: the
		// resolved model swaps between preflight and spawn (e.g. a routing
		// decision that changed independently). This must never be
		// normalized away.
		spawnQW.ResolvedProfile.Model = "different-model-swapped-after-preflight"

		source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
			Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
			effectiveCell: piReceiptCell("harness/v2", realModel, executioncell.SessionHumanControlled, nil),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		source.PreparedHarness = &plan

		_, err = agent.PrepareHarness(source, providerWithManifest.Manifest())
		if err == nil {
			t.Fatal("expected an authority drift error for the swapped model, got nil")
		}
		var driftErr *agent.AuthorityDriftError
		if !errors.As(err, &driftErr) {
			t.Fatalf("expected *agent.AuthorityDriftError, got %T: %v", err, err)
		}
		found := false
		for _, field := range driftErr.Fields {
			if field == "model" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Fields = %v, want it to name %q", driftErr.Fields, "model")
		}
		if !strings.Contains(err.Error(), "model") {
			t.Fatalf("Error() = %q must name the drifting field", err.Error())
		}
	})
}
