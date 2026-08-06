package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

func exactReceiptQueuedWork(requestID string) QueuedWork {
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{
		Harness: string(agent.HarnessCodex), Model: "gpt-test",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: "gpt-test", Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
			EndpointID: "openai-direct", EndpointOperator: "openai", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
			AuthBindingID: "auth_test", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
			AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
			AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
		},
	}}
	qw.SessionID = requestID
	return qw
}

func exactReceiptCell(harnessVersion, model string, mode executioncell.SessionMode, capabilities []executioncell.CapabilityRequirement) executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessCodex), Version: harnessVersion},
		Model:           executioncell.ModelRef{ID: model, Author: "openai"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "openai-direct", Protocol: string(agent.ProtoOpenAIResponses), Operator: "openai", Revision: "2026-08-06",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "auth_test", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled,
			Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment,
		},
		Placement:   executioncell.PlacementRef{ID: "host_test", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode: mode, GrantedCapabilities: append([]executioncell.CapabilityRequirement{}, capabilities...), EvidenceTier: executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
}

func attachAdmittedExecutionCell(t *testing.T, qw QueuedWork, cell executioncell.ResolvedExecutionCell) QueuedWork {
	t.Helper()
	payloadDigest, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatalf("digest operational payload: %v", err)
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion:          executioncell.ContractVersion,
		ReceiptID:                "admission_" + qw.SessionID,
		RequestID:                qw.SessionID,
		Decision:                 executioncell.AdmissionAdmitted,
		IntentDigest:             strings.Repeat("1", 64),
		OperationalPayloadDigest: payloadDigest,
		Cell:                     &cell,
		ResolverDecisions: []executioncell.ResolverDecision{{
			Kind: executioncell.DecisionExplicit, Field: "harness", SelectedRef: "harness:codex@" + cell.Harness.Version, Reason: "test receipt pins the exact harness",
		}},
		RecordedAt: "2026-08-06T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal admission receipt: %v", err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatalf("validate admission receipt: %v", err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatalf("canonical effective cell: %v", err)
	}
	qw.AdmissionReceipt = immutable.Bytes()
	qw.EffectiveCell = effective
	qw.WorkerID = "worker_test"
	binding, err := json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       qw.SessionID, WorkerID: qw.WorkerID, PlacementID: cell.Placement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	qw.ExecutionRuntimeBinding = binding
	qw = attachReadyHostAdaptation(t, qw, "")
	return qw
}

func attachReadyHostAdaptation(t *testing.T, qw QueuedWork, claimReceiptID string) QueuedWork {
	t.Helper()
	admission, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(qw.ExecutionRuntimeBinding)
	if err != nil {
		t.Fatal(err)
	}
	promptReceipt := agent.PromptDeliveryReceipt{ContractVersion: agent.PromptContractVersion, ProfileID: "test-prompt", Decision: "ready", Entries: []agent.PromptDeliveryEntry{}}
	toolReceipt := agent.ToolLifecycleReceipt{ContractVersion: agent.ToolLifecycleContractVersion, AdmissionReceiptID: admission.Value().ReceiptID, ClaimReceiptID: claimReceiptID, OperationalPayloadDigest: admission.Value().OperationalPayloadDigest, ProfileID: "test-tools", Decision: "ready", EvidenceTier: string(agent.EvidenceStructured), ProductionEligible: true, Entries: []agent.ToolLifecycleEntry{}}
	host := map[string]any{
		"contractVersion": executioncell.HostAdaptationContractVersion, "requestId": binding.RequestID,
		"workerId": binding.WorkerID, "placementId": binding.PlacementID, "decision": "ready",
		"promptReceipt": promptReceipt, "toolLifecycleReceipt": toolReceipt,
	}
	if binding.ClaimID != "" {
		host["claimId"] = binding.ClaimID
	}
	qw.HostAdaptationReceipt = rawJSONForRunner(t, host)
	return qw
}

func TestAdmissionReceiptPreflightPinsExactCell(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	qw := exactReceiptQueuedWork("session_receipted")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))

	admission, err := registry.PreflightHarness(qw)
	if err != nil {
		t.Fatalf("PreflightHarness: %v", err)
	}
	ref, ok := admission.CanonicalHarnessRef()
	if !ok || ref.ID != string(agent.HarnessCodex) || ref.Version != "harness/v2" {
		t.Fatalf("canonical harness = %+v, %t", ref, ok)
	}
	selection, err := (&Runner{registry: registry}).admittedHarnessSelection(context.Background(), qw, admission)
	if err != nil {
		t.Fatalf("admittedHarnessSelection: %v", err)
	}
	if selection.Provider != provider || len(selection.receipt.Bytes()) == 0 {
		t.Fatalf("selection did not retain exact provider and immutable receipt: %+v", selection)
	}
}

func TestAdmissionReceiptRevalidatesOperationalPayloadAfterPreflight(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	qw := exactReceiptQueuedWork("session_gateway_binding")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
	admission, err := registry.PreflightHarness(qw)
	if err != nil {
		t.Fatalf("PreflightHarness: %v", err)
	}

	qw.Body = "POST_PREFLIGHT_PROMPT_DO_NOT_LEAK"
	_, err = (&Runner{registry: registry}).admittedHarnessSelection(context.Background(), qw, admission)
	if err == nil || !strings.Contains(err.Error(), "operational payload changed") {
		t.Fatalf("admittedHarnessSelection error = %v; want operational-payload mutation denial", err)
	}
	if strings.Contains(err.Error(), "POST_PREFLIGHT_PROMPT_DO_NOT_LEAK") {
		t.Fatalf("post-preflight denial leaked prompt evidence: %v", err)
	}
	if provider.spawnCalls.Load() != 0 {
		t.Fatal("post-preflight endpoint mismatch reached provider spawn")
	}
}

func TestAdmissionReceiptOmissionPreservesLegacyPreflight(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t, &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex})
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{Provider: agent.ProviderCodex}}
	qw.SessionID = "session_legacy_omission"
	admission, err := registry.PreflightHarness(qw)
	if err != nil || admission != nil {
		t.Fatalf("PreflightHarness legacy omission = admission %+v, error %v; want nil, nil", admission, err)
	}
}

func TestDeniedAdmissionReceiptRemainsAuthoritative(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	qw := exactReceiptQueuedWork("session_denied")
	payloadDigest, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission_denied", RequestID: "session_denied",
		Decision: executioncell.AdmissionDenied, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: payloadDigest,
		DenialCode: executioncell.DenialUnknownEndpoint, DenialDetail: "endpoint is not admitted", ResolverDecisions: []executioncell.ResolverDecision{},
		RecordedAt: "2026-08-06T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatalf("validate denied receipt: %v", err)
	}
	qw.AdmissionReceipt = immutable.Bytes()
	admission, err := registry.PreflightHarness(qw)
	var denial *HarnessAdmissionError
	if admission == nil || !errors.As(err, &denial) || denial.Code != receipt.DenialCode || !json.Valid(denial.Receipt.Bytes()) {
		t.Fatalf("PreflightHarness = admission %+v, error %v; want authoritative denied receipt", admission, err)
	}
	if denial.Receipt.Value().ReceiptID != receipt.ReceiptID || provider.spawnCalls.Load() != 0 {
		t.Fatalf("denial receipt = %+v, spawnCalls = %d", denial.Receipt.Value(), provider.spawnCalls.Load())
	}
}

func TestAdmissionReceiptPreflightRejectsMismatchedOrUnsupportedCell(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t,
		&selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		&selectorFakeProvider{name: agent.ProviderClaude, harness: agent.HarnessClaudeCode},
	)
	tests := []struct {
		name       string
		mutateWork func(*QueuedWork)
		mutateCell func(*executioncell.ResolvedExecutionCell)
		wantCode   executioncell.AdmissionDenialCode
	}{
		{name: "explicit harness mismatch", mutateWork: func(q *QueuedWork) { q.ResolvedProfile.Harness = string(agent.HarnessClaudeCode) }, wantCode: executioncell.DenialUnknownHarness},
		{name: "harness version mismatch", mutateCell: func(c *executioncell.ResolvedExecutionCell) { c.Harness.Version = "pinned-1" }, wantCode: executioncell.DenialUnsupportedHarnessVersion},
		{name: "model mismatch", mutateWork: func(q *QueuedWork) { q.ResolvedProfile.Model = "different" }, wantCode: executioncell.DenialUnknownModel},
		{name: "endpoint mismatch", mutateWork: func(q *QueuedWork) {
			q.ResolvedProfile.Endpoint.Protocol = agent.ProtoAnthropicMessages
			q.ResolvedProfile.Endpoint.BaseURL = "https://endpoint-evidence.invalid/private"
		}, wantCode: executioncell.DenialUnknownEndpoint},
		{name: "auth mismatch", mutateWork: func(q *QueuedWork) {
			q.ResolvedProfile.Endpoint.Mechanism = agent.AuthOAuth
			q.ResolvedProfile.Endpoint.Env = map[string]string{"TOKEN": "AUTH_EVIDENCE_DO_NOT_LEAK"}
		}, wantCode: executioncell.DenialUnknownAuthBinding},
		{name: "session mode mismatch", mutateCell: func(c *executioncell.ResolvedExecutionCell) { c.SessionMode = executioncell.SessionHumanControlled }, wantCode: executioncell.DenialUnsupportedSessionMode},
		{name: "unsupported capability", mutateCell: func(c *executioncell.ResolvedExecutionCell) {
			c.GrantedCapabilities = []executioncell.CapabilityRequirement{{Name: "take_control"}}
		}, wantCode: executioncell.DenialCapabilityUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qw := exactReceiptQueuedWork("session_mismatch")
			cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil)
			if tc.mutateWork != nil {
				tc.mutateWork(&qw)
			}
			if tc.mutateCell != nil {
				tc.mutateCell(&cell)
			}
			qw = attachAdmittedExecutionCell(t, qw, cell)
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || denial.Code != tc.wantCode {
				t.Fatalf("PreflightHarness = admission %+v, error %v; want code %q", admission, err, tc.wantCode)
			}
			if strings.Contains(err.Error(), "endpoint-evidence.invalid") || strings.Contains(err.Error(), "AUTH_EVIDENCE_DO_NOT_LEAK") {
				t.Fatalf("denial leaked endpoint or auth evidence: %v", err)
			}
			if raw := denial.Receipt.Bytes(); strings.Contains(string(raw), "endpoint-evidence.invalid") || strings.Contains(string(raw), "AUTH_EVIDENCE_DO_NOT_LEAK") {
				t.Fatalf("denial receipt leaked endpoint or auth evidence: %s", raw)
			}
			if provider := registry.providers[agent.ProviderCodex]; provider.(*selectorFakeProvider).spawnCalls.Load() != 0 {
				t.Fatal("receipt denial reached provider spawn")
			}
		})
	}
}

func TestAdmissionReceiptClosedDecoderDenialIsReusableAndPreSpawn(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	qw := exactReceiptQueuedWork("session_closed_receipt")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
	var document map[string]any
	if err := json.Unmarshal(qw.AdmissionReceipt, &document); err != nil {
		t.Fatal(err)
	}
	document["futureField"] = true
	qw.AdmissionReceipt, _ = json.Marshal(document)

	admission, err := registry.PreflightHarness(qw)
	var contractErr *executioncell.ContractError
	if admission == nil || !errors.As(err, &contractErr) || contractErr.Code != executioncell.ErrorUnknownField {
		t.Fatalf("PreflightHarness = admission %+v, error %v; want closed decoder denial", admission, err)
	}
	runner := &Runner{registry: registry, now: func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }}
	result, runErr := runner.runLoop(context.Background(), qw, 0, admission)
	if !errors.As(runErr, &contractErr) || result.Status != "failed" || provider.spawnCalls.Load() != 0 {
		t.Fatalf("runLoop = result %+v, error %v, spawnCalls %d", result, runErr, provider.spawnCalls.Load())
	}
}

func attachClaimedExecutionCell(t *testing.T, qw QueuedWork) QueuedWork {
	t.Helper()
	admitted := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil)
	admitted.Placement = executioncell.PlacementRef{ID: "pool_test", Kind: executioncell.PlacementPool, Resolution: executioncell.PlacementClaimBound}
	qw = attachAdmittedExecutionCell(t, qw, admitted)
	effective := admitted
	effective.Placement = executioncell.PlacementRef{ID: "host_claimed", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact}
	effective.RuntimeInventoryDigest = strings.Repeat("5", 64)
	claim := executioncell.ClaimReceipt{
		ContractVersion: executioncell.ContractVersion, ClaimReceiptID: "claim_receipt_test",
		AdmissionReceiptID: "admission_" + qw.SessionID, ClaimID: "claim_test", Decision: executioncell.ClaimClaimed,
		EffectiveCell: &effective, RecordedAt: "2026-08-06T12:01:00Z",
	}
	raw, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := executioncell.DecodeClaimReceipt(raw)
	if err != nil {
		t.Fatalf("decode claim fixture: %v", err)
	}
	qw.ClaimReceipt = immutable.Bytes()
	qw.EffectiveCell, err = executioncell.CanonicalJSON(effective)
	if err != nil {
		t.Fatal(err)
	}
	qw.ExecutionRuntimeBinding, err = json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       qw.SessionID, WorkerID: qw.WorkerID, PlacementID: effective.Placement.ID, ClaimID: claim.ClaimID,
	})
	if err != nil {
		t.Fatal(err)
	}
	qw = attachReadyHostAdaptation(t, qw, claim.ClaimReceiptID)
	return qw
}

func TestClaimBoundAdmissionRequiresNarrowClaimAndBindsLifecycle(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	qw := attachClaimedExecutionCell(t, exactReceiptQueuedWork("session_claimed"))
	admission, err := registry.PreflightHarness(qw)
	if err != nil {
		t.Fatalf("PreflightHarness: %v", err)
	}
	selection, err := (&Runner{registry: registry}).admittedHarnessSelection(context.Background(), qw, admission)
	if err != nil {
		t.Fatalf("admittedHarnessSelection: %v", err)
	}
	if selection.effectiveCell.Placement.ID != "host_claimed" || len(selection.claimReceipt.Bytes()) == 0 {
		t.Fatalf("effective claim selection = %+v", selection)
	}
	spec, err := bindAdmissionToolLifecyclePlan(agent.Spec{Autonomous: true}, selection.receipt, selection.claimReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ToolLifecyclePlan == nil || spec.ToolLifecyclePlan.ClaimReceiptID != "claim_receipt_test" {
		t.Fatalf("claim-bound lifecycle plan = %+v", spec.ToolLifecyclePlan)
	}
}

func TestSelfConsistentReceiptAndCellReplayMustMatchCurrentWorkerAndClaim(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	for _, tc := range []struct {
		name   string
		work   func(*testing.T) QueuedWork
		mutate func(*executioncell.RuntimeBinding)
	}{
		{name: "exact foreign worker", work: func(t *testing.T) QueuedWork {
			return attachAdmittedExecutionCell(t, exactReceiptQueuedWork("exact-replay"), exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
		}, mutate: func(b *executioncell.RuntimeBinding) { b.WorkerID = "worker-foreign" }},
		{name: "claim foreign active claim", work: func(t *testing.T) QueuedWork {
			return attachClaimedExecutionCell(t, exactReceiptQueuedWork("claim-replay"))
		}, mutate: func(b *executioncell.RuntimeBinding) { b.ClaimID = "claim-other-active" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qw := tc.work(t)
			binding, err := executioncell.DecodeRuntimeBinding(qw.ExecutionRuntimeBinding)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&binding)
			qw.ExecutionRuntimeBinding = rawJSONForRunner(t, binding)
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
				t.Fatalf("admission=%+v err=%v spawn=%d", admission, err, provider.spawnCalls.Load())
			}
		})
	}
}

func rawJSONForRunner(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClaimAndEffectiveCellFailuresDenyBeforeSpawn(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *QueuedWork)
	}{
		{name: "missing claim", mutate: func(_ *testing.T, q *QueuedWork) { q.ClaimReceipt = nil }},
		{name: "missing effective cell", mutate: func(_ *testing.T, q *QueuedWork) { q.EffectiveCell = nil }},
		{name: "claim references other admission", mutate: func(t *testing.T, q *QueuedWork) {
			claim := decodeClaimValue(t, q.ClaimReceipt)
			claim.AdmissionReceiptID = "admission_other"
			q.ClaimReceipt = marshalClaimValue(t, claim)
		}},
		{name: "claim changes immutable endpoint", mutate: func(t *testing.T, q *QueuedWork) {
			claim := decodeClaimValue(t, q.ClaimReceipt)
			claim.EffectiveCell.Endpoint.Revision = "other-revision"
			q.ClaimReceipt = marshalClaimValue(t, claim)
			q.EffectiveCell, _ = executioncell.CanonicalJSON(*claim.EffectiveCell)
		}},
		{name: "denied claim", mutate: func(t *testing.T, q *QueuedWork) {
			claim := decodeClaimValue(t, q.ClaimReceipt)
			claim.Decision = executioncell.ClaimDenied
			claim.EffectiveCell = nil
			claim.DenialCode = executioncell.ClaimHostIneligible
			claim.DenialDetail = "host unavailable"
			q.ClaimReceipt = marshalClaimValue(t, claim)
		}},
		{name: "runtime effective cell differs from claim", mutate: func(t *testing.T, q *QueuedWork) {
			cell, err := executioncell.DecodeResolvedExecutionCell(q.EffectiveCell)
			if err != nil {
				t.Fatal(err)
			}
			value := cell
			value.Placement.ID = "host_other"
			q.EffectiveCell, _ = executioncell.CanonicalJSON(value)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
			registry := selectorRegistry(t, provider)
			qw := attachClaimedExecutionCell(t, exactReceiptQueuedWork("session_claim_failure"))
			test.mutate(t, &qw)
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
				t.Fatalf("claim failure = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
			}
		})
	}

	t.Run("exact placement rejects claim", func(t *testing.T) {
		provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
		registry := selectorRegistry(t, provider)
		claimed := attachClaimedExecutionCell(t, exactReceiptQueuedWork("session_exact_with_claim"))
		exact := exactReceiptQueuedWork("session_exact_with_claim")
		exact.ClaimReceipt = claimed.ClaimReceipt
		exact = attachAdmittedExecutionCell(t, exact, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
		exact.ClaimReceipt = claimed.ClaimReceipt
		admission, err := registry.PreflightHarness(exact)
		var denial *HarnessAdmissionError
		if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
			t.Fatalf("exact placement claim = admission %+v, err %v", admission, err)
		}
	})
}

func decodeClaimValue(t *testing.T, raw []byte) executioncell.ClaimReceipt {
	t.Helper()
	immutable, err := executioncell.DecodeClaimReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return immutable.Value()
}

func marshalClaimValue(t *testing.T, claim executioncell.ClaimReceipt) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := executioncell.DecodeClaimReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return immutable.Bytes()
}

func TestEveryEffectiveCellFieldMutationDeniesBeforeSpawn(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*executioncell.ResolvedExecutionCell)
	}{
		{"contract version", func(c *executioncell.ResolvedExecutionCell) { c.ContractVersion = "future" }},
		{"harness id", func(c *executioncell.ResolvedExecutionCell) { c.Harness.ID = "claude-code" }},
		{"harness version", func(c *executioncell.ResolvedExecutionCell) { c.Harness.Version = "other" }},
		{"model id", func(c *executioncell.ResolvedExecutionCell) { c.Model.ID = "gpt-other" }},
		{"model author", func(c *executioncell.ResolvedExecutionCell) { c.Model.Author = "other-author" }},
		{"endpoint id", func(c *executioncell.ResolvedExecutionCell) { c.Endpoint.ID = "endpoint-other" }},
		{"endpoint protocol", func(c *executioncell.ResolvedExecutionCell) { c.Endpoint.Protocol = string(agent.ProtoOpenAIChat) }},
		{"endpoint operator", func(c *executioncell.ResolvedExecutionCell) { c.Endpoint.Operator = "other-operator" }},
		{"endpoint revision", func(c *executioncell.ResolvedExecutionCell) { c.Endpoint.Revision = "other-revision" }},
		{"auth id", func(c *executioncell.ResolvedExecutionCell) { c.AuthBinding.ID = "auth_other" }},
		{"auth mechanism", func(c *executioncell.ResolvedExecutionCell) { c.AuthBinding.Mechanism = executioncell.AuthOAuth }},
		{"auth commercial mode", func(c *executioncell.ResolvedExecutionCell) {
			c.AuthBinding.CommercialMode = executioncell.CommercialSubscription
		}},
		{"auth authority", func(c *executioncell.ResolvedExecutionCell) { c.AuthBinding.Authority = "other-authority" }},
		{"auth scope", func(c *executioncell.ResolvedExecutionCell) { c.AuthBinding.BindingScope = executioncell.ScopeSession }},
		{"auth portability", func(c *executioncell.ResolvedExecutionCell) { c.AuthBinding.Portability = executioncell.EndpointBound }},
		{"auth delivery", func(c *executioncell.ResolvedExecutionCell) {
			c.AuthBinding.Delivery = executioncell.DeliveryBrokeredToken
		}},
		{"placement id", func(c *executioncell.ResolvedExecutionCell) { c.Placement.ID = "host_other" }},
		{"placement kind", func(c *executioncell.ResolvedExecutionCell) { c.Placement.Kind = executioncell.PlacementRemotePeer }},
		{"placement resolution", func(c *executioncell.ResolvedExecutionCell) {
			c.Placement.Resolution = executioncell.PlacementClaimBound
		}},
		{"session mode", func(c *executioncell.ResolvedExecutionCell) { c.SessionMode = executioncell.SessionHumanControlled }},
		{"granted capabilities", func(c *executioncell.ResolvedExecutionCell) {
			c.GrantedCapabilities = []executioncell.CapabilityRequirement{{Name: "watch"}}
		}},
		{"evidence tier", func(c *executioncell.ResolvedExecutionCell) { c.EvidenceTier = executioncell.EvidenceSmoked }},
		{"compatibility digest", func(c *executioncell.ResolvedExecutionCell) { c.CompatibilityDigest = strings.Repeat("6", 64) }},
		{"runtime inventory digest", func(c *executioncell.ResolvedExecutionCell) { c.RuntimeInventoryDigest = strings.Repeat("7", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
			registry := selectorRegistry(t, provider)
			qw := exactReceiptQueuedWork("session_cell_mutation")
			qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
			cell, err := executioncell.DecodeResolvedExecutionCell(qw.EffectiveCell)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&cell)
			qw.EffectiveCell, _ = executioncell.CanonicalJSON(cell)
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
				t.Fatalf("cell mutation = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
			}
		})
	}
}

func TestEveryEndpointBindingFieldMismatchDeniesBeforeSpawn(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agent.EndpointBinding)
	}{
		{"company", func(e *agent.EndpointBinding) { e.Company = agent.CompanyAnthropic }},
		{"model", func(e *agent.EndpointBinding) { e.Model = "gpt-other" }},
		{"protocol", func(e *agent.EndpointBinding) { e.Protocol = agent.ProtoOpenAIChat }},
		{"host", func(e *agent.EndpointBinding) { e.Host = agent.HostAzure }},
		{"endpoint id", func(e *agent.EndpointBinding) { e.EndpointID = "endpoint-other" }},
		{"endpoint operator", func(e *agent.EndpointBinding) { e.EndpointOperator = "operator-other" }},
		{"endpoint revision", func(e *agent.EndpointBinding) { e.EndpointRevision = "revision-other" }},
		{"model author", func(e *agent.EndpointBinding) { e.ModelAuthor = "author-other" }},
		{"auth id", func(e *agent.EndpointBinding) { e.AuthBindingID = "auth_other" }},
		{"auth authority", func(e *agent.EndpointBinding) { e.AuthAuthority = "authority-other" }},
		{"auth commercial", func(e *agent.EndpointBinding) { e.AuthCommercialMode = string(executioncell.CommercialSubscription) }},
		{"auth scope", func(e *agent.EndpointBinding) { e.AuthBindingScope = string(executioncell.ScopeSession) }},
		{"auth portability", func(e *agent.EndpointBinding) { e.AuthPortability = string(executioncell.EndpointBound) }},
		{"auth delivery", func(e *agent.EndpointBinding) { e.AuthDelivery = string(executioncell.DeliveryBrokeredToken) }},
		{"auth mechanism", func(e *agent.EndpointBinding) { e.Mechanism = agent.AuthOAuth }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
			registry := selectorRegistry(t, provider)
			qw := exactReceiptQueuedWork("session_endpoint_mutation")
			test.mutate(qw.ResolvedProfile.Endpoint)
			qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
				t.Fatalf("endpoint mismatch = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
			}
		})
	}
}

func TestReceiptRequestIDIsBoundBeforeDecision(t *testing.T) {
	for _, decision := range []executioncell.AdmissionDecision{executioncell.AdmissionAdmitted, executioncell.AdmissionDenied} {
		t.Run(string(decision), func(t *testing.T) {
			provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
			registry := selectorRegistry(t, provider)
			qw := exactReceiptQueuedWork("session_request_target")
			if decision == executioncell.AdmissionAdmitted {
				qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
			} else {
				payloadDigest, _ := DigestOperationalPayload(qw)
				receipt := executioncell.AdmissionReceipt{
					ContractVersion: executioncell.ContractVersion, ReceiptID: "admission_wrong_request", RequestID: "session_other",
					Decision: decision, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: payloadDigest,
					DenialCode: executioncell.DenialUnknownEndpoint, DenialDetail: "denied elsewhere", ResolverDecisions: []executioncell.ResolverDecision{},
					RecordedAt: "2026-08-06T12:00:00Z",
				}
				raw, _ := json.Marshal(receipt)
				immutable, err := executioncell.DecodeAdmissionReceipt(raw)
				if err != nil {
					t.Fatal(err)
				}
				qw.AdmissionReceipt = immutable.Bytes()
			}
			if decision == executioncell.AdmissionAdmitted {
				immutable, _ := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
				receipt := immutable.Value()
				receipt.RequestID = "session_other"
				raw, _ := json.Marshal(receipt)
				immutable, _ = executioncell.DecodeAdmissionReceipt(raw)
				qw.AdmissionReceipt = immutable.Bytes()
			}
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) || denial.Code != executioncell.DenialFallbackNotAllowed || provider.spawnCalls.Load() != 0 {
				t.Fatalf("request binding = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
			}
		})
	}
}

func TestAdmissionReceiptProjectsLifecycleRequirementsAndReceiptLink(t *testing.T) {
	t.Parallel()
	parametersDigest := strings.Repeat("a", 64)
	qw := exactReceiptQueuedWork("session_lifecycle")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{
		{Name: "watch", ParametersDigest: parametersDigest}, {Name: "replay"}, {Name: "cancel"},
	}))
	immutable, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bindAdmissionToolLifecyclePlan(agent.Spec{Autonomous: true}, immutable, executioncell.ImmutableClaimReceipt{})
	if err != nil {
		t.Fatalf("bindAdmissionToolLifecyclePlan: %v", err)
	}
	plan := spec.ToolLifecyclePlan
	if plan == nil || plan.AdmissionReceiptID != "admission_session_lifecycle" || len(plan.Lifecycle) != 6 || plan.Replay == nil || !plan.RequireCleanup {
		t.Fatalf("projected plan = %+v", plan)
	}
	if plan.Lifecycle[0].ParametersDigest != parametersDigest {
		t.Fatalf("watch parameters digest = %q", plan.Lifecycle[0].ParametersDigest)
	}
	manifest := (&claude.Provider{}).Manifest()
	profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("claude autonomous profile missing")
	}
	_, receipt, err := agent.AdaptToolLifecycle(spec, profile)
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Channel != agent.ToolChannelLifecycle {
		t.Fatalf("AdaptToolLifecycle error = %v, want typed lifecycle denial", err)
	}
	if receipt.AdmissionReceiptID != plan.AdmissionReceiptID || receipt.OperationalPayloadDigest != plan.OperationalPayloadDigest || receipt.Entries[0].InputDigest != parametersDigest {
		t.Fatalf("linked denial receipt = %+v", receipt)
	}
}

func TestHostProviderViewCompilesAdmissionLifecycleBeforeChild(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	// Supply the real closed profiles while retaining a spawn counter.
	providerWithManifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest()}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("host-preflight-watch")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "watch"}}))
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	detail := map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
		"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding,
		"operationalPayload": qw.OperationalPayload,
	}
	raw := rawJSONForRunner(t, detail)
	receipt, err := NewProviderView(registry).PreflightExecution(raw)
	if err == nil {
		t.Fatal("watch adaptation unexpectedly ready")
	}
	var decoded struct {
		Decision string                      `json:"decision"`
		Tool     *agent.ToolLifecycleReceipt `json:"toolLifecycleReceipt"`
	}
	if json.Unmarshal(receipt, &decoded) != nil || decoded.Decision != "denied" || decoded.Tool == nil || decoded.Tool.Decision != "denied" {
		t.Fatalf("host denial receipt = %s err=%v", receipt, err)
	}
	if provider.spawnCalls.Load() != 0 {
		t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
	}
}

type manifestSelectorProvider struct {
	*selectorFakeProvider
	manifest agent.HarnessManifest
}

func (p *manifestSelectorProvider) Manifest() agent.HarnessManifest { return p.manifest }

func codexManifestForTest() agent.HarnessManifest {
	return (&codex.Provider{}).Manifest()
}
