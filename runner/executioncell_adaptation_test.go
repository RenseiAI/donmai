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
	mode := agent.PromptModeAutonomous
	if admission.Value().Cell != nil && admission.Value().Cell.SessionMode == executioncell.SessionHumanControlled {
		mode = agent.PromptModeHumanControlled
	}
	plan := &agent.PreparedHarness{
		ContractVersion: agent.HarnessAdaptationContractVersion, Harness: admission.Value().Cell.Harness.ID,
		Mode: mode, OperationalPayloadDigest: admission.Value().OperationalPayloadDigest, AuthorityDigest: "test-authority",
		PromptReceipt: promptReceipt, ToolLifecycleReceipt: toolReceipt,
	}
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		plan.Materializations = append(plan.Materializations, agent.HarnessMaterialization{
			Channel: channel, SourceDigest: admission.Value().OperationalPayloadDigest, Required: true,
		})
	}
	host := map[string]any{
		"contractVersion": executioncell.HostAdaptationContractVersion, "requestId": binding.RequestID,
		"workerId": binding.WorkerID, "placementId": binding.PlacementID, "decision": "ready",
		"plan": plan, "planDigest": agent.DigestPreparedHarness(plan),
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

// TestEveryEndpointBindingFieldMismatchDeniesBeforeSpawn covers the endpoint
// fields the admitted execution cell carries an axis for. Company is
// deliberately absent: the cell has no company axis, so a company mismatch has
// nothing in the cell to disagree with. Company is pinned by the
// operational-payload digest instead — see
// TestReceiptDeniesEndpointCompanySubstitutedAfterAdmission.
func TestEveryEndpointBindingFieldMismatchDeniesBeforeSpawn(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agent.EndpointBinding)
	}{
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
	// A cell that was admitted with watch, replay, and cancel must adapt
	// against a harness whose profile declares all three. This whole class of
	// admitted cell used to be unspawnable: the lifecycle, replay, and cleanup
	// lanes denied for every harness alike regardless of what its manifest
	// declared, so admission granted capabilities the executor always refused.
	_, receipt, err := agent.AdaptToolLifecycle(spec, profile)
	if err != nil {
		t.Fatalf("AdaptToolLifecycle: %v", err)
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready: %+v", receipt.Decision, receipt.Entries)
	}
	if receipt.AdmissionReceiptID != plan.AdmissionReceiptID || receipt.OperationalPayloadDigest != plan.OperationalPayloadDigest || receipt.Entries[0].InputDigest != parametersDigest {
		t.Fatalf("linked ready receipt = %+v", receipt)
	}
	lanes := map[agent.ToolLifecycleChannel]agent.ToolAdaptationOutcome{
		agent.ToolChannelLifecycle: agent.ToolOutcomePendingRuntime,
		agent.ToolChannelReplay:    agent.ToolOutcomePendingRuntime,
		agent.ToolChannelCleanup:   agent.ToolOutcomePendingCleanup,
	}
	seen := map[agent.ToolLifecycleChannel]int{}
	for _, entry := range receipt.Entries {
		want, ok := lanes[entry.Channel]
		if !ok {
			t.Fatalf("unexpected channel %q on entry %+v", entry.Channel, entry)
		}
		seen[entry.Channel]++
		if entry.Outcome != want || entry.Delivery == "" {
			t.Errorf("entry %q = outcome %q via %q, want %q with a declared delivery", entry.ID, entry.Outcome, entry.Delivery, want)
		}
	}
	if seen[agent.ToolChannelLifecycle] != len(plan.Lifecycle) || seen[agent.ToolChannelReplay] != 1 || seen[agent.ToolChannelCleanup] != 1 {
		t.Fatalf("receipt lane coverage = %+v, want every projected requirement", seen)
	}
}

// TestAdmissionWatchProjectionRequiresOnlyTheSessionBoundary pins which part of
// a watch grant every mode must evidence. The session boundary stays required
// so a mode that cannot report start and terminal result is not watchable at
// all; the richer per-turn and per-tool evidence is optional, because a PTY
// mode carries genuinely fewer event kinds and must record that gap on the
// receipt rather than lose the whole session to it.
func TestAdmissionWatchProjectionRequiresOnlyTheSessionBoundary(t *testing.T) {
	t.Parallel()
	qw := attachAdmittedExecutionCell(t, exactReceiptQueuedWork("session_watch_projection"),
		exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionHumanControlled, []executioncell.CapabilityRequirement{{Name: "watch"}}))
	immutable, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bindAdmissionToolLifecyclePlan(agent.Spec{Interactive: &agent.InteractiveSpec{}}, immutable, executioncell.ImmutableClaimReceipt{})
	if err != nil {
		t.Fatalf("bindAdmissionToolLifecyclePlan: %v", err)
	}
	required := map[agent.EventKind]bool{}
	for _, requirement := range spec.ToolLifecyclePlan.Lifecycle {
		required[requirement.Event] = requirement.Required
		if requirement.MinimumFidelity != agent.EvidenceCoarse {
			t.Errorf("human-controlled watch %s fidelity = %q, want coarse", requirement.Event, requirement.MinimumFidelity)
		}
	}
	want := map[agent.EventKind]bool{
		agent.EventInit: true, agent.EventResult: true,
		agent.EventAssistantText: false, agent.EventToolUse: false,
		agent.EventToolResult: false, agent.EventError: false,
	}
	if len(required) != len(want) {
		t.Fatalf("watch projected %d events, want %d", len(required), len(want))
	}
	for event, wantRequired := range want {
		got, ok := required[event]
		if !ok {
			t.Fatalf("watch projection omitted %s", event)
		}
		if got != wantRequired {
			t.Errorf("watch %s required = %v, want %v", event, got, wantRequired)
		}
	}
	// The interactive profile then adapts: the boundary is delivered as
	// terminal bytes and the undeliverable tool evidence is recorded, not fatal.
	_, receipt, err := agent.AdaptToolLifecycle(spec, mustClaudeProfile(t, agent.PromptModeHumanControlled))
	if err != nil {
		t.Fatalf("interactive watch adaptation: %v", err)
	}
	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready: %+v", receipt.Decision, receipt.Entries)
	}
	var pending, denied int
	for _, entry := range receipt.Entries {
		switch entry.Outcome {
		case agent.ToolOutcomePendingRuntime:
			pending++
		case agent.ToolOutcomeDenied:
			denied++
		default:
			t.Errorf("entry %q outcome = %q", entry.ID, entry.Outcome)
		}
	}
	if pending != 2 || denied != 4 {
		t.Fatalf("interactive watch receipt = %d pending / %d denied, want 2/4", pending, denied)
	}
}

func mustClaudeProfile(t *testing.T, mode agent.PromptSessionMode) agent.ToolLifecycleProfile {
	t.Helper()
	profile, ok := (&claude.Provider{}).Manifest().ToolLifecycleProfile(mode)
	if !ok {
		t.Fatalf("claude %s profile missing", mode)
	}
	return profile
}

func TestHostProviderViewCompilesAdmissionLifecycleBeforeChild(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	// Supply the real closed profiles while retaining a spawn counter.
	providerWithManifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("host-preflight-watch")
	qw.Body = "exercise lifecycle preflight"
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
	if err != nil {
		t.Fatalf("host compile of an admitted watch grant: %v receipt=%s", err, receipt)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var tool agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Decision != "ready" {
		t.Fatalf("host tool receipt = %+v, want ready", tool)
	}
	for _, entry := range tool.Entries {
		if entry.Channel != agent.ToolChannelLifecycle {
			continue
		}
		if entry.Required && entry.Outcome != agent.ToolOutcomePendingRuntime {
			t.Errorf("required watch entry %q = %+v, want pending runtime evidence", entry.ID, entry)
		}
	}
	// Compiling the adaptation is still a pre-spawn act: nothing runs yet.
	if provider.spawnCalls.Load() != 0 {
		t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
	}
}

func TestHostProviderViewCompilesActualHumanControlledInput(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	providerWithManifest := &manifestSelectorProvider{selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest()}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("host-preflight-human")
	qw.Mode = interactiveRunMode
	qw.InitialPrompt = "actual human-controlled initial turn"
	qw.McpServers = []agent.MCPServerConfig{{Name: "card-server", Command: "card-mcp", Args: []string{"--stdio"}}}
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionHumanControlled, nil))
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
	if plan.Mode != agent.PromptModeHumanControlled || plan.Harness != string(agent.HarnessCodex) {
		t.Fatalf("host compiled wrong identity: %+v", plan)
	}
	source, _, err := buildPreparedSourceSpec(qw, harnessSelection{
		Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, qw.AdmissionReceipt),
		effectiveCell: exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionHumanControlled, nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.Prompt != qw.InitialPrompt || source.Autonomous || source.Interactive == nil || len(source.MCPServers) != 2 {
		t.Fatalf("host source was not actual human input: prompt=%q autonomous=%v interactive=%v mcp=%+v", source.Prompt, source.Autonomous, source.Interactive != nil, source.MCPServers)
	}
	source.PreparedHarness = &plan
	if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
		t.Fatalf("host plan cannot be consumed as sole provider authority: %v", err)
	}
	if provider.spawnCalls.Load() != 0 {
		t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
	}
}

func mustAdmissionReceipt(t *testing.T, raw json.RawMessage) executioncell.ImmutableAdmissionReceipt {
	t.Helper()
	receipt, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

type manifestSelectorProvider struct {
	*selectorFakeProvider
	manifest     agent.HarnessManifest
	capabilities agent.Capabilities
}

func (p *manifestSelectorProvider) Manifest() agent.HarnessManifest { return p.manifest }

// Capabilities must come from the same harness the manifest describes: spec
// translation reads them to decide whether tool policy travels as a flat
// allow-list or through the approval bridge, and a mismatch denies on the tool
// channel before the lane under test is ever reached.
func (p *manifestSelectorProvider) Capabilities() agent.Capabilities { return p.capabilities }

func codexManifestForTest() agent.HarnessManifest {
	return (&codex.Provider{}).Manifest()
}

func codexCapabilitiesForTest() agent.Capabilities {
	return (&codex.Provider{}).Capabilities()
}

// TestReceiptAdmitsCellWhoseModelAuthorDiffersFromEndpointCompany pins the
// two-field vocabulary the receipt lane must honor. Company is the SPEAK-axis
// endpoint identity — which wire dialect and vendor surface the request is
// spoken to (anthropic/openai/google/local/stub). ModelAuthor is who authored
// the model. They coincide only for first-party direct cells; every
// OpenAI-compatible aggregator, gateway, or local serving cell diverges (the
// worker-local gateway always binds Company=openai regardless of the model it
// serves). A receipt lane that requires them to be equal denies legitimate
// work and asserts a falsehood about which vendor authored the model that ran.
func TestReceiptAdmitsCellWhoseModelAuthorDiffersFromEndpointCompany(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)

	// A gateway cell: spoken to an OpenAI-compatible surface, serving a model
	// authored by someone else entirely.
	qw := exactReceiptQueuedWork("session_gateway_authorship")
	qw.ResolvedProfile.Model = "llama-3.3-70b"
	qw.ResolvedProfile.Endpoint.Model = "llama-3.3-70b"
	qw.ResolvedProfile.Endpoint.Company = agent.CompanyOpenAI
	qw.ResolvedProfile.Endpoint.ModelAuthor = "meta"

	cell := exactReceiptCell("harness/v2", "llama-3.3-70b", executioncell.SessionAutonomous, nil)
	cell.Model.Author = "meta"
	qw = attachAdmittedExecutionCell(t, qw, cell)

	admission, err := registry.PreflightHarness(qw)
	if err != nil {
		t.Fatalf("PreflightHarness denied a coherent cell whose model author differs from the endpoint company: %v", err)
	}
	ref, ok := admission.CanonicalHarnessRef()
	if !ok || ref.ID != string(agent.HarnessCodex) {
		t.Fatalf("canonical harness = %+v, %t", ref, ok)
	}
}

// TestReceiptDeniesModelAuthorThatDisagreesWithAdmittedCell is the other half
// of the vocabulary: the endpoint's ModelAuthor — not its Company — is the
// field that must equal the admitted cell's model author.
func TestReceiptDeniesModelAuthorThatDisagreesWithAdmittedCell(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)

	qw := exactReceiptQueuedWork("session_author_disagreement")
	qw.ResolvedProfile.Endpoint.Company = agent.CompanyOpenAI
	qw.ResolvedProfile.Endpoint.ModelAuthor = "meta"

	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil)
	cell.Model.Author = "openai"
	qw = attachAdmittedExecutionCell(t, qw, cell)

	admission, err := registry.PreflightHarness(qw)
	var denial *HarnessAdmissionError
	if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
		t.Fatalf("model-author disagreement = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
	}
	if denial.Code != executioncell.DenialUnknownEndpoint {
		t.Fatalf("denial code = %q; want %q", denial.Code, executioncell.DenialUnknownEndpoint)
	}
}

// TestReceiptDeniesEndpointCompanySubstitutedAfterAdmission proves the
// endpoint Company stays pinned once the model-author conflation is gone. The
// admitted execution cell carries no company axis, so Company is anchored by
// the operational-payload digest the platform stamped pre-enqueue: substituting
// it in flight still denies before spawn.
func TestReceiptDeniesEndpointCompanySubstitutedAfterAdmission(t *testing.T) {
	t.Parallel()
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)

	qw := exactReceiptQueuedWork("session_company_substitution")
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
	// Substitute the endpoint company after the platform admitted the payload.
	qw.ResolvedProfile.Endpoint.Company = agent.CompanyAnthropic

	admission, err := registry.PreflightHarness(qw)
	var denial *HarnessAdmissionError
	if admission == nil || !errors.As(err, &denial) || provider.spawnCalls.Load() != 0 {
		t.Fatalf("post-admission company substitution = admission %+v, err %v, spawn %d", admission, err, provider.spawnCalls.Load())
	}
}
