package executioncell

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func claimBoundFixtureAdmission(t *testing.T) (ImmutableAdmissionReceipt, ResolvedExecutionCell) {
	t.Helper()
	fixtures := loadFixtures(t)
	admission, err := DecodeAdmissionReceipt(fixtures.ClaimBound.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	value := admission.Value()
	if value.Cell == nil {
		t.Fatal("claim-bound fixture admission has no cell")
	}
	return admission, *value.Cell
}

// satisfyingReality returns the ClaimLocalReality that exactly matches what
// the fixture admission cell demands, so a single field can be perturbed per
// test case to isolate one denial predicate at a time.
func satisfyingReality(cell ResolvedExecutionCell) ClaimLocalReality {
	return ClaimLocalReality{
		PlacementID:            "host_mac_studio_claimed",
		HarnessAvailable:       true,
		EndpointReachable:      true,
		AuthBindingAvailable:   true,
		AvailableCapabilities:  append([]CapabilityRequirement(nil), cell.GrantedCapabilities...),
		EvidenceTier:           cell.EvidenceTier,
		RuntimeInventoryDigest: "da818c932697d8b3b52d3d4b8002e7a537e12a3d58e95f4f486adfa6f7cb1f13",
	}
}

func TestEvaluateClaim_ClaimedNarrowsAdmission(t *testing.T) {
	t.Parallel()
	admission, cell := claimBoundFixtureAdmission(t)
	local := satisfyingReality(cell)
	recordedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	claim, err := EvaluateClaim(admission, "claim_host_mac_studio", local, recordedAt)
	if err != nil {
		t.Fatalf("EvaluateClaim: %v", err)
	}
	value := claim.Value()
	if value.Decision != ClaimClaimed {
		t.Fatalf("decision = %q, want %q (denial=%s detail=%s)", value.Decision, ClaimClaimed, value.DenialCode, value.DenialDetail)
	}
	if value.AdmissionReceiptID != admission.Value().ReceiptID {
		t.Fatalf("admissionReceiptId = %q, want %q", value.AdmissionReceiptID, admission.Value().ReceiptID)
	}
	if value.ClaimID != "claim_host_mac_studio" {
		t.Fatalf("claimId = %q", value.ClaimID)
	}
	if value.EffectiveCell == nil {
		t.Fatal("claimed receipt has no effective cell")
	}
	effective := *value.EffectiveCell
	if effective.Placement.Kind != PlacementHost || effective.Placement.Resolution != PlacementExact {
		t.Fatalf("effective placement = %+v, want exact host", effective.Placement)
	}
	if effective.Placement.ID != local.PlacementID {
		t.Fatalf("effective placement id = %q, want %q", effective.Placement.ID, local.PlacementID)
	}
	if effective.RuntimeInventoryDigest != local.RuntimeInventoryDigest {
		t.Fatalf("effective runtime inventory digest = %q, want %q", effective.RuntimeInventoryDigest, local.RuntimeInventoryDigest)
	}
	// Every other axis must be byte-identical to the admitted cell — this is
	// the narrow-only invariant, re-asserted independently of the internal
	// self-check EvaluateClaim already performs.
	if err := AssertNarrowClaim(admission, claim); err != nil {
		t.Fatalf("AssertNarrowClaim: %v", err)
	}
	if !sameValue(effective.Harness, cell.Harness) || !sameValue(effective.Model, cell.Model) ||
		!sameValue(effective.Endpoint, cell.Endpoint) || !sameValue(effective.AuthBinding, cell.AuthBinding) ||
		effective.SessionMode != cell.SessionMode || !sameValue(effective.GrantedCapabilities, cell.GrantedCapabilities) ||
		effective.EvidenceTier != cell.EvidenceTier || effective.CompatibilityDigest != cell.CompatibilityDigest {
		t.Fatalf("effective cell diverged from admitted cell beyond placement/runtimeInventoryDigest: %+v vs %+v", effective, cell)
	}
}

func TestEvaluateClaim_DeniedTypedReasons(t *testing.T) {
	t.Parallel()
	recordedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		mutate func(cell ResolvedExecutionCell, local *ClaimLocalReality)
		want   ClaimDenialCode
	}{
		{
			name: "conflicting active claim",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.ConflictingClaimID = "claim_other_host"
			},
			want: ClaimConflict,
		},
		{
			name: "no local placement identity",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.PlacementID = ""
			},
			want: ClaimHostIneligible,
		},
		{
			name: "harness not available locally",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.HarnessAvailable = false
			},
			want: ClaimHostIneligible,
		},
		{
			name: "endpoint not reachable locally",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.EndpointReachable = false
			},
			want: ClaimHostIneligible,
		},
		{
			name: "auth binding not available locally",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.AuthBindingAvailable = false
			},
			want: ClaimAuthUnavailable,
		},
		{
			name: "granted capability missing locally",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.AvailableCapabilities = []CapabilityRequirement{{Name: "watch"}}
			},
			want: ClaimCapabilityRegressed,
		},
		{
			name: "evidence tier regressed",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.EvidenceTier = EvidenceUnitVerified
			},
			want: ClaimEvidenceRegressed,
		},
		{
			name: "no live runtime inventory digest",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.RuntimeInventoryDigest = ""
			},
			want: ClaimInventoryChanged,
		},
		{
			name: "malformed runtime inventory digest",
			mutate: func(_ ResolvedExecutionCell, local *ClaimLocalReality) {
				local.RuntimeInventoryDigest = "not-hex"
			},
			want: ClaimInventoryChanged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			admission, cell := claimBoundFixtureAdmission(t)
			local := satisfyingReality(cell)
			tt.mutate(cell, &local)

			claim, err := EvaluateClaim(admission, "claim_host_mac_studio", local, recordedAt)
			if err != nil {
				t.Fatalf("EvaluateClaim: %v", err)
			}
			value := claim.Value()
			if value.Decision != ClaimDenied {
				t.Fatalf("decision = %q, want %q", value.Decision, ClaimDenied)
			}
			if value.DenialCode != tt.want {
				t.Fatalf("denialCode = %q, want %q (detail=%s)", value.DenialCode, tt.want, value.DenialDetail)
			}
			if value.DenialDetail == "" {
				t.Fatal("denied claim has no denial detail")
			}
			if value.EffectiveCell != nil {
				t.Fatal("denied claim must not carry an effective cell")
			}
			// D5: a denied claim receives no credentials and spawns nothing —
			// at the contract level that means it must still satisfy the
			// narrow-only assertion (a denial is trivially narrow-only, but
			// this proves EvaluateClaim never produces a receipt that fails
			// the shared invariant checker).
			if err := AssertNarrowClaim(admission, claim); err != nil {
				t.Fatalf("AssertNarrowClaim on denied receipt: %v", err)
			}
		})
	}
}

func TestEvaluateClaim_ConflictWinsOverEveryOtherAxis(t *testing.T) {
	t.Parallel()
	// A conflicting active claim must deny even when every other axis of
	// local reality would otherwise satisfy the gate — no partial credit,
	// and no attempt to "fall back" past the conflict.
	admission, cell := claimBoundFixtureAdmission(t)
	local := satisfyingReality(cell)
	local.ConflictingClaimID = "claim_other_host"
	claim, err := EvaluateClaim(admission, "claim_host_mac_studio", local, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value().DenialCode != ClaimConflict {
		t.Fatalf("denialCode = %q, want %q", claim.Value().DenialCode, ClaimConflict)
	}
}

func TestEvaluateClaim_RejectsNonClaimBoundAdmission(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	// Any semantic fixture cell that is not itself claim-bound proves the
	// point; reuse one of the always-present fixture cells and wrap it in a
	// minimal admitted receipt.
	var exampleCellRaw json.RawMessage
	for _, raw := range fixtures.Cells {
		exampleCellRaw = raw
		break
	}
	cell, err := DecodeResolvedExecutionCell(exampleCellRaw)
	if err != nil {
		t.Fatal(err)
	}
	if cell.Placement.Kind == PlacementPool && cell.Placement.Resolution == PlacementClaimBound {
		t.Skip("fixture cell happens to be claim-bound; nothing to assert here")
	}
	receipt := AdmissionReceipt{
		ContractVersion: ContractVersion, ReceiptID: "admission_exact_example", RequestID: "request_exact_example",
		Decision: AdmissionAdmitted, IntentDigest: strings.Repeat("a", 64), OperationalPayloadDigest: strings.Repeat("b", 64),
		Cell: &cell, ResolverDecisions: []ResolverDecision{}, RecordedAt: "2026-08-12T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EvaluateClaim(admission, "claim_x", ClaimLocalReality{}, time.Now())
	requireContractError(t, err, ErrorInvalidReference)
}

func TestEvaluateClaim_RejectsEmptyClaimID(t *testing.T) {
	t.Parallel()
	admission, cell := claimBoundFixtureAdmission(t)
	local := satisfyingReality(cell)
	_, err := EvaluateClaim(admission, "  ", local, time.Now())
	requireContractError(t, err, ErrorMissingRequiredField)
}

func TestEvaluateClaim_RejectsDeniedAdmission(t *testing.T) {
	t.Parallel()
	receipt := AdmissionReceipt{
		ContractVersion: ContractVersion, ReceiptID: "admission_denied_example", RequestID: "request_denied_example",
		Decision: AdmissionDenied, IntentDigest: strings.Repeat("a", 64), OperationalPayloadDigest: strings.Repeat("b", 64),
		DenialCode: DenialUnknownHarness, DenialDetail: "unknown harness selector",
		ResolverDecisions: []ResolverDecision{}, RecordedAt: "2026-08-12T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EvaluateClaim(admission, "claim_x", ClaimLocalReality{}, time.Now())
	requireContractError(t, err, ErrorInvalidReference)
}
