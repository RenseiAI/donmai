package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func permanentDenialDetail(t *testing.T, item PollWorkItem) (*SessionDetail, executioncell.RuntimeBinding) {
	t.Helper()
	detail := PollItemToSessionDetail(item, nil, "https://example.test", "runtime", "wkr-test")
	binding, err := executioncell.DecodeRuntimeBinding(detail.ExecutionRuntimeBinding)
	if err != nil {
		t.Fatal(err)
	}
	return detail, binding
}

func mutateDeniedHostReceipt(
	t *testing.T,
	receipt json.RawMessage,
	mutatePlan func(*agent.PreparedHarness),
	mutateOuterTool func(*agent.ToolLifecycleReceipt),
) json.RawMessage {
	t.Helper()
	var host executioncell.HostAdaptationReceipt
	if err := json.Unmarshal(receipt, &host); err != nil {
		t.Fatal(err)
	}
	var plan agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if mutatePlan != nil {
		mutatePlan(&plan)
	}
	planRaw := rawJSON(t, plan)
	planSum := sha256.Sum256(planRaw)
	host.Plan = planRaw
	host.PlanDigest = hex.EncodeToString(planSum[:])
	outer := plan.ToolLifecycleReceipt
	if mutateOuterTool != nil {
		mutateOuterTool(&outer)
	}
	host.ToolLifecycleReceipt = rawJSON(t, outer)
	return rawJSON(t, host)
}

func TestPermanentDenialProjection_CanonicalBindingAndRawHostIdentity(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-hash", "wkr-test")
	detail, binding := permanentDenialDetail(t, item)
	candidate := freshPermanentDenialCandidate(detail, binding, hostReceipt, "", typed)
	if candidate == nil || candidate.report == nil {
		t.Fatal("fresh typed denial did not produce a correlated candidate")
	}
	if durablePermanentDenialReport(candidate) != nil {
		t.Fatal("unpersisted candidate gained wire authority")
	}
	candidate.markDurable()
	report := durablePermanentDenialReport(candidate)
	if report == nil {
		t.Fatal("persisted candidate omitted durable report")
	}
	canonical, err := executioncell.CanonicalJSON(json.RawMessage(detail.ExecutionRuntimeBinding))
	if err != nil {
		t.Fatal(err)
	}
	wantBinding := sha256.Sum256(canonical)
	wantHost := sha256.Sum256(hostReceipt)
	if report.RuntimeBindingCanonicalSHA256 != hex.EncodeToString(wantBinding[:]) ||
		report.HostReceiptSHA256 != hex.EncodeToString(wantHost[:]) {
		t.Fatalf("report hashes = %+v", report)
	}

	var bindingObject map[string]json.RawMessage
	if err := json.Unmarshal(detail.ExecutionRuntimeBinding, &bindingObject); err != nil {
		t.Fatal(err)
	}
	reordered := json.RawMessage(fmt.Sprintf(
		" { \n \"workerId\":%s,\"placementId\":%s,\"requestId\":%s,\"contractVersion\":%s } ",
		bindingObject["workerId"], bindingObject["placementId"],
		bindingObject["requestId"], bindingObject["contractVersion"],
	))
	detailReordered := *detail
	detailReordered.ExecutionRuntimeBinding = reordered
	reorderedBinding, err := executioncell.DecodeRuntimeBinding(reordered)
	if err != nil {
		t.Fatal(err)
	}
	reorderedCandidate := freshPermanentDenialCandidate(&detailReordered, reorderedBinding, hostReceipt, "", typed)
	if reorderedCandidate == nil || reorderedCandidate.report == nil ||
		reorderedCandidate.report.RuntimeBindingCanonicalSHA256 != report.RuntimeBindingCanonicalSHA256 {
		t.Fatalf("reordered binding report = %+v, want canonical hash %s", reorderedCandidate, report.RuntimeBindingCanonicalSHA256)
	}

	spacedHost := append(json.RawMessage(" \n"), hostReceipt...)
	spacedHost = append(spacedHost, '\n')
	spacedCandidate := freshPermanentDenialCandidate(detail, binding, spacedHost, "", typed)
	if spacedCandidate == nil || spacedCandidate.report == nil {
		t.Fatal("strictly valid raw host variant was refused")
	}
	if spacedCandidate.report.HostReceiptSHA256 == report.HostReceiptSHA256 {
		t.Fatal("host receipt hash was canonicalized instead of hashing exact raw bytes")
	}
}

func TestPermanentDenialProjection_PreservesTypedCauseWithoutRenderingDetail(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-safe-error", "wkr-test")
	detail, binding := permanentDenialDetail(t, item)
	wrappedInput := fmt.Errorf("compiler context: %w", typed)
	candidate := freshPermanentDenialCandidate(detail, binding, hostReceipt, "", wrappedInput)
	if candidate == nil || candidate.Error() != preSpawnPermanentDenialReason {
		t.Fatalf("candidate = %#v", candidate)
	}
	var got *agent.ToolAdaptationError
	if !errors.As(candidate, &got) || got != typed {
		t.Fatalf("errors.As cause = %#v, want original typed error", got)
	}
	joined := errors.Join(errors.New("persist receipt unavailable"), candidate)
	if !errors.As(joined, &got) {
		t.Fatal("joined error lost typed cause")
	}
	for _, rendered := range []string{candidate.Error(), joined.Error()} {
		if strings.Contains(rendered, "PROFILE_SECRET_DO_NOT_LEAK") ||
			strings.Contains(rendered, "MCP_SECRET_DO_NOT_LEAK") {
			t.Fatalf("error rendering leaked typed detail: %s", rendered)
		}
	}
}

func TestPermanentDenialProjection_StrictNegativeMatrix(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-negative", "wkr-test")
	detail, binding := permanentDenialDetail(t, item)
	tests := []struct {
		name    string
		receipt json.RawMessage
		err     error
	}{
		{name: "generic prose", receipt: hostReceipt, err: errors.New(typed.Error())},
		{name: "application failed", receipt: hostReceipt, err: &agent.ToolAdaptationError{Code: agent.ToolDenialApplicationFailed, Channel: typed.Channel, Detail: typed.Detail}},
		{name: "empty channel", receipt: hostReceipt, err: &agent.ToolAdaptationError{Code: agent.ToolDenialDeliveryUnsupported, Detail: typed.Detail}},
		{name: "optional denied entry", receipt: mutateDeniedHostReceipt(t, hostReceipt, func(plan *agent.PreparedHarness) {
			plan.ToolLifecycleReceipt.Entries[0].Required = false
		}, nil), err: typed},
		{name: "duplicate matching entry", receipt: mutateDeniedHostReceipt(t, hostReceipt, func(plan *agent.PreparedHarness) {
			duplicate := plan.ToolLifecycleReceipt.Entries[0]
			duplicate.ID = "mcp-servers-duplicate"
			plan.ToolLifecycleReceipt.Entries = append(plan.ToolLifecycleReceipt.Entries, duplicate)
		}, nil), err: typed},
		{name: "second required denial on another channel", receipt: mutateDeniedHostReceipt(t, hostReceipt, func(plan *agent.PreparedHarness) {
			other := plan.ToolLifecycleReceipt.Entries[0]
			other.ID = "cleanup"
			other.Channel = agent.ToolChannelCleanup
			plan.ToolLifecycleReceipt.Entries = append(plan.ToolLifecycleReceipt.Entries, other)
		}, nil), err: typed},
		{name: "wrong admission", receipt: mutateDeniedHostReceipt(t, hostReceipt, func(plan *agent.PreparedHarness) {
			plan.ToolLifecycleReceipt.AdmissionReceiptID = "other-admission"
		}, nil), err: typed},
		{name: "outer projection mismatch", receipt: mutateDeniedHostReceipt(t, hostReceipt, nil, func(tools *agent.ToolLifecycleReceipt) {
			tools.ProfileID = "different-profile"
		}), err: typed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := freshPermanentDenialCandidate(detail, binding, tc.receipt, "", tc.err)
			if candidate != nil && candidate.report != nil {
				t.Fatalf("invalid evidence produced report: %+v", candidate.report)
			}
		})
	}
}

func TestPermanentDenialProjection_ClaimReceiptCorrelation(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-claim", "wkr-test")
	var admissionValue executioncell.AdmissionReceipt
	if err := json.Unmarshal(item.AdmissionReceipt, &admissionValue); err != nil {
		t.Fatal(err)
	}
	admissionValue.Cell.Placement = executioncell.PlacementRef{
		ID: "pool-claim", Kind: executioncell.PlacementPool,
		Resolution: executioncell.PlacementClaimBound,
	}
	item.AdmissionReceipt = rawJSON(t, admissionValue)
	admitted, err := executioncell.DecodeAdmissionReceipt(item.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	claimID := "claim-session-claim"
	claim, err := executioncell.EvaluateClaim(admitted, claimID, executioncell.ClaimLocalReality{
		PlacementID: "host-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable: true, AvailableCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:           admissionValue.Cell.EvidenceTier,
		RuntimeInventoryDigest: strings.Repeat("e", 64),
	}, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	claimValue := claim.Value()
	item.ClaimReceipt = claim.Bytes()
	item.EffectiveCell = rawJSON(t, claimValue.EffectiveCell)
	item.ExecutionRuntimeBinding = rawJSON(t, executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       item.SessionID, WorkerID: "wkr-test",
		PlacementID: claimValue.EffectiveCell.Placement.ID, ClaimID: claimID,
	})
	hostReceipt = mutateDeniedHostReceipt(t, hostReceipt, func(plan *agent.PreparedHarness) {
		plan.ToolLifecycleReceipt.ClaimReceiptID = claimValue.ClaimReceiptID
	}, nil)
	var host executioncell.HostAdaptationReceipt
	if err := json.Unmarshal(hostReceipt, &host); err != nil {
		t.Fatal(err)
	}
	host.PlacementID = claimValue.EffectiveCell.Placement.ID
	host.ClaimID = claimID
	hostReceipt = rawJSON(t, host)

	detail, binding := permanentDenialDetail(t, item)
	candidate := freshPermanentDenialCandidate(detail, binding, hostReceipt, "", typed)
	if candidate == nil || candidate.report == nil || candidate.report.ClaimReceiptID != claimValue.ClaimReceiptID {
		t.Fatalf("claim-bound candidate = %+v, want claimReceiptId %q", candidate, claimValue.ClaimReceiptID)
	}
}

func TestPermanentDenialClosedDecoderRejectsUnknownDuplicateAndTrailing(t *testing.T) {
	tests := []string{
		`{"contractVersion":"tool-lifecycle/v1alpha1","unexpected":true}`,
		`{"contractVersion":"tool-lifecycle/v1alpha1","contractVersion":"tool-lifecycle/v1alpha1"}`,
		`{"contractVersion":"tool-lifecycle/v1alpha1"}{}`,
	}
	for _, raw := range tests {
		var receipt agent.ToolLifecycleReceipt
		if err := decodeClosedPermanentDenialJSON(json.RawMessage(raw), &receipt); err == nil {
			t.Fatalf("closed decoder accepted %s", raw)
		}
	}
}

func TestPermanentDenialProjection_RefusesDetailIdentityAndPlacementDrift(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-drift", "wkr-test")
	detail, binding := permanentDenialDetail(t, item)
	for _, mutate := range []func(*SessionDetail){
		func(candidate *SessionDetail) { candidate.SessionID = "other-session" },
		func(candidate *SessionDetail) { candidate.WorkerID = "other-worker" },
		func(candidate *SessionDetail) {
			var effective executioncell.ResolvedExecutionCell
			if err := json.Unmarshal(candidate.EffectiveCell, &effective); err != nil {
				t.Fatal(err)
			}
			effective.Placement.ID = "other-placement"
			candidate.EffectiveCell = rawJSON(t, effective)
		},
	} {
		candidateDetail := *detail
		mutate(&candidateDetail)
		candidate := freshPermanentDenialCandidate(&candidateDetail, binding, hostReceipt, "", typed)
		if candidate != nil && candidate.report != nil {
			t.Fatalf("drifted detail produced report: %+v", candidate.report)
		}
	}
}

func TestPermanentDenialPersistenceFailurePreservesCauseWithoutReport(t *testing.T) {
	item, hostReceipt, typed := deniedToolDeliveryFixture(t, "session-persist-failure", "wkr-test")
	provider := &countingExecutionPreflight{receipt: hostReceipt, err: typed}
	persistFailure := errors.New("durable store unavailable")
	store := &failingExecutionPreflightStore{err: persistFailure}
	d := newRunningTestDaemon(t, Options{
		ProviderRegistry: provider, ExecutionPreflightStore: store,
	}, []ProjectConfig{{ID: "project", Repository: "github.com/acme/repo"}}, nil)
	detail := PollItemToSessionDetail(item, nil, "https://example.test", "runtime", "wkr-test")
	_, err := d.AcceptWorkWithDetail(PollItemToSessionSpec(item, d.spawner.AllProjects()), detail)
	if err == nil || !errors.Is(err, persistFailure) {
		t.Fatalf("persistence failure = %v", err)
	}
	var got *agent.ToolAdaptationError
	if !errors.As(err, &got) || got != typed {
		t.Fatalf("persistence error lost typed cause: %v", err)
	}
	if durablePermanentDenialReport(err) != nil {
		t.Fatal("failed persistence produced terminal report")
	}
	if strings.Contains(err.Error(), "PROFILE_SECRET_DO_NOT_LEAK") ||
		strings.Contains(err.Error(), "MCP_SECRET_DO_NOT_LEAK") {
		t.Fatalf("joined persistence error leaked typed detail: %v", err)
	}
}

func TestPermanentDenialNackResponseContract(t *testing.T) {
	report := &preSpawnPermanentDenialReport{
		ContractVersion: preSpawnPermanentDenialContractVersion,
		Code:            agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
		AdmissionReceiptID: "admission", OperationalPayloadDigest: strings.Repeat("a", 64),
		RuntimeBindingCanonicalSHA256: strings.Repeat("b", 64), HostReceiptSHA256: strings.Repeat("c", 64),
	}
	item := &PollWorkItem{
		SessionID: "session-response", IssueID: "issue", IssueIdentifier: "OPS-1", Priority: 1, QueuedAt: 1,
		claimAttempt: &workClaimAttemptProof{
			ContractVersion: workClaimAttemptContractVersion,
			SessionID:       "session-response", AttemptToken: claimAttemptTokenOne,
		},
	}
	tests := []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{name: "exact terminal", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"session-response","attemptToken":"%s","hostReceiptSha256":"%s"}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenOne, report.HostReceiptSHA256)},
		{name: "legacy no content", status: http.StatusNoContent, wantError: true},
		{name: "ordinary requeue 200", status: http.StatusOK, body: `{"nacked":true,"requeued":true}`, wantError: true},
		{name: "mismatched attempt", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"session-response","attemptToken":"%s","hostReceiptSha256":"%s"}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenTwo, report.HostReceiptSHA256), wantError: true},
		{name: "mismatched session", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"other","attemptToken":"%s","hostReceiptSha256":"%s"}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenOne, report.HostReceiptSHA256), wantError: true},
		{name: "mismatched host hash", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"session-response","attemptToken":"%s","hostReceiptSha256":"%s"}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenOne, strings.Repeat("d", 64)), wantError: true},
		{name: "unknown member", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"session-response","attemptToken":"%s","hostReceiptSha256":"%s","extra":true}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenOne, report.HostReceiptSHA256), wantError: true},
		{name: "duplicate member", status: http.StatusOK, body: fmt.Sprintf(`{"contractVersion":"%s","outcome":"terminal","sessionId":"session-response","sessionId":"session-response","attemptToken":"%s","hostReceiptSha256":"%s"}`, preSpawnPermanentDenialContractVersion, claimAttemptTokenOne, report.HostReceiptSHA256), wantError: true},
		{name: "ambiguous", status: http.StatusConflict, body: `{"outcome":"ambiguous"}`, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			err := callNackEndpoint(
				context.Background(), srv.Client(), srv.URL,
				item.SessionID, "worker", "runtime", preSpawnPermanentDenialReason,
				nil, report, item,
			)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError=%v", err, tc.wantError)
			}
			if calls.Load() != 1 {
				t.Fatalf("HTTP calls = %d, want exactly one with no legacy retry", calls.Load())
			}
		})
	}
}

func TestPermanentDenialDirectSenderRefusesInvalidProofBeforeHTTP(t *testing.T) {
	report := &preSpawnPermanentDenialReport{
		ContractVersion: preSpawnPermanentDenialContractVersion,
		Code:            agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
		AdmissionReceiptID: "admission", OperationalPayloadDigest: strings.Repeat("a", 64),
		RuntimeBindingCanonicalSHA256: strings.Repeat("b", 64), HostReceiptSHA256: strings.Repeat("c", 64),
	}
	for _, proof := range []*workClaimAttemptProof{
		nil,
		{ContractVersion: workClaimAttemptContractVersion, SessionID: "other", AttemptToken: claimAttemptTokenOne},
		{ContractVersion: workClaimAttemptContractVersion, SessionID: "s", AttemptToken: "not-a-uuid"},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		item := &PollWorkItem{
			SessionID: "s", IssueID: "i", IssueIdentifier: "OPS-1", Priority: 1, QueuedAt: 1,
			claimAttempt: proof,
		}
		err := callNackEndpoint(
			context.Background(), srv.Client(), srv.URL, "s", "worker", "runtime",
			preSpawnPermanentDenialReason, nil, report, item,
		)
		srv.Close()
		if err == nil || calls.Load() != 0 {
			t.Fatalf("proof=%+v error=%v HTTP calls=%d, want pre-request refusal", proof, err, calls.Load())
		}
	}
}

func TestPermanentDenialReportRequiresNegotiationAndClaimProof(t *testing.T) {
	typed := &agent.ToolAdaptationError{
		Code: agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
		Detail: "do not render",
	}
	wrapped := &preSpawnPermanentDenialError{
		cause: typed,
		report: &preSpawnPermanentDenialReport{
			ContractVersion: preSpawnPermanentDenialContractVersion,
			Code:            agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
			AdmissionReceiptID: "admission", OperationalPayloadDigest: strings.Repeat("a", 64),
			RuntimeBindingCanonicalSHA256: strings.Repeat("b", 64), HostReceiptSHA256: strings.Repeat("c", 64),
		},
		durable: true,
	}
	for _, tc := range []struct {
		name       string
		negotiated bool
		proof      *workClaimAttemptProof
		wantReport bool
	}{
		{name: "missing negotiation", proof: &workClaimAttemptProof{ContractVersion: workClaimAttemptContractVersion, SessionID: "s", AttemptToken: claimAttemptTokenOne}},
		{name: "missing proof", negotiated: true},
		{name: "both", negotiated: true, proof: &workClaimAttemptProof{ContractVersion: workClaimAttemptContractVersion, SessionID: "s", AttemptToken: claimAttemptTokenOne}, wantReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if tc.wantReport {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"contractVersion": preSpawnPermanentDenialContractVersion,
						"outcome":         "terminal", "sessionId": "s",
						"attemptToken":      claimAttemptTokenOne,
						"hostReceiptSha256": wrapped.report.HostReceiptSHA256,
					})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			item := &PollWorkItem{
				SessionID: "s", IssueID: "i", IssueIdentifier: "OPS-1", Priority: 1, QueuedAt: 1,
				claimAttempt: tc.proof, preSpawnPermanentDenialV1: tc.negotiated,
			}
			if err := NackRejectedWork(context.Background(), srv.Client(), srv.URL, "worker", "runtime", item, wrapped); err != nil {
				t.Fatal(err)
			}
			_, present := body["preSpawnPermanentDenial"]
			if present != tc.wantReport {
				t.Fatalf("report present=%v, want=%v body=%s", present, tc.wantReport, rawJSON(t, body))
			}
		})
	}
}

func TestPermanentDenialActualPollLogsJoinedErrorWithoutCauseProse(t *testing.T) {
	typed := &agent.ToolAdaptationError{
		Code: agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
		Detail: "JOINED_SECRET_DO_NOT_LEAK",
	}
	report := &preSpawnPermanentDenialReport{
		ContractVersion: preSpawnPermanentDenialContractVersion,
		Code:            agent.ToolDenialDeliveryUnsupported, Channel: agent.ToolChannelMCPServer,
		AdmissionReceiptID: "admission", OperationalPayloadDigest: strings.Repeat("a", 64),
		RuntimeBindingCanonicalSHA256: strings.Repeat("b", 64), HostReceiptSHA256: strings.Repeat("c", 64),
	}
	wrapped := &preSpawnPermanentDenialError{cause: typed, report: report, durable: true}
	joined := errors.Join(errors.New("registration acknowledgement unavailable"), wrapped)
	pollBody := fmt.Sprintf(
		`{"work":[{"sessionId":"joined","issueId":"i","issueIdentifier":"OPS-1","priority":1,"queuedAt":1}],"claimAttempts":{"joined":{"contractVersion":"work-claim-attempt/v1","sessionId":"joined","attemptToken":%q}},"nackContracts":["pre-spawn-permanent-denial/v1"]}`,
		claimAttemptTokenOne,
	)
	var requestBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(pollBody))
		case http.MethodPost:
			var err error
			requestBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contractVersion": preSpawnPermanentDenialContractVersion,
				"outcome":         "terminal", "sessionId": "joined",
				"attemptToken":      claimAttemptTokenOne,
				"hostReceiptSha256": report.HostReceiptSHA256,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	var logs strings.Builder
	poller := NewPollService(PollOptions{
		WorkerID: "worker", RuntimeJWT: "runtime", OrchestratorURL: srv.URL,
		HTTPClient: srv.Client(),
		LogWarn:    func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) },
		OnWork: func(item PollWorkItem) error {
			if err := NackRejectedWork(context.Background(), srv.Client(), srv.URL, "worker", "runtime", &item, joined); err != nil {
				return err
			}
			return joined
		},
	})
	poller.pollOnce(context.Background())
	combined := logs.String() + string(requestBody)
	if strings.Contains(combined, typed.Detail) {
		t.Fatalf("actual poll path leaked joined typed detail: %s", combined)
	}
	if !strings.Contains(logs.String(), preSpawnPermanentDenialReason) ||
		!bytes.Contains(requestBody, []byte(`"preSpawnPermanentDenial"`)) {
		t.Fatalf("poll logs/body missing fixed denial projection: logs=%s body=%s", logs.String(), requestBody)
	}
	var got *agent.ToolAdaptationError
	if !errors.As(joined, &got) || got != typed {
		t.Fatal("joined safe wrapper lost errors.As cause")
	}
}

func TestPermanentDenialReportStructHasFrozenJSONFields(t *testing.T) {
	typeOfReport := reflect.TypeOf(preSpawnPermanentDenialReport{})
	want := []string{
		"contractVersion", "code", "channel", "admissionReceiptId", "claimReceiptId,omitempty",
		"operationalPayloadDigest", "runtimeBindingCanonicalSha256", "hostReceiptSha256",
	}
	got := make([]string, 0, typeOfReport.NumField())
	for index := 0; index < typeOfReport.NumField(); index++ {
		got = append(got, typeOfReport.Field(index).Tag.Get("json"))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report JSON fields = %v, want %v", got, want)
	}
}

func TestPermanentDenialRawHostHashIsNotCanonical(t *testing.T) {
	left := json.RawMessage(`{"contractVersion":"host-adaptation/v1","requestId":"s","workerId":"w","placementId":"p","decision":"denied"}`)
	right := append(json.RawMessage(" \n"), left...)
	right = append(right, '\n')
	leftSum := sha256.Sum256(left)
	rightSum := sha256.Sum256(right)
	if bytes.Equal(leftSum[:], rightSum[:]) {
		t.Fatal("raw receipt fixtures unexpectedly share digest")
	}
}
