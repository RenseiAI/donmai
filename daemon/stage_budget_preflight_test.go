package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

func TestAcceptWorkForwardsSiblingStageBudgetToExecutionPreflight(t *testing.T) {
	admission, cell := claimBoundAdmission(t)
	reality := executioncell.ClaimLocalReality{
		PlacementID: "worker-local", HarnessAvailable: true, EndpointReachable: true,
		AuthBindingAvailable: true, AvailableCapabilities: cell.GrantedCapabilities,
		EvidenceTier: cell.EvidenceTier, RuntimeInventoryDigest: strings.Repeat("e", 64),
	}
	provider := &stubClaimGateProvider{realityJSON: mustMarshal(t, reality)}
	d := startClaimGateDaemon(t, provider)
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       "request-claim-gate-1", WorkerID: "worker-local",
		PlacementID: "worker-local", ClaimID: "claim-stage-budget",
	}
	detail := &SessionDetail{
		SessionID: "request-claim-gate-1", WorkerID: "worker-local",
		AdmissionReceipt: admission.Bytes(), EffectiveCell: mustMarshal(t, cell),
		ExecutionRuntimeBinding: mustMarshal(t, binding),
		StageBudget: &PollStageBudget{
			MaxDurationSeconds: 1800,
			MaxSubAgents:       3,
			MaxTokens:          24_000,
		},
	}

	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "stage-budget-project"}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	input := provider.lastPreflightInput.Load()
	if input == nil {
		t.Fatal("preflight compiler received no input")
	}
	var wire struct {
		StageBudget *PollStageBudget `json:"stageBudget"`
	}
	if err := json.Unmarshal(*input, &wire); err != nil {
		t.Fatalf("decode preflight input: %v", err)
	}
	if wire.StageBudget == nil {
		t.Fatal("preflight input omitted the sibling stage budget")
	}
	if got := *wire.StageBudget; got != *detail.StageBudget {
		t.Fatalf("preflight stage budget = %+v, want %+v", got, *detail.StageBudget)
	}
}
