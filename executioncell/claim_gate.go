package executioncell

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ClaimLocalReality is the claiming host's own, non-secret account of what it
// can actually deliver for a claim-bound admission. EvaluateClaim re-runs the
// admission predicates against these facts and only ever narrows: on success
// it may bind the pool placement to this host and swap the pool's
// runtime-inventory digest for the host's own, but it may not change harness,
// model, endpoint, auth binding, session mode, granted capabilities, evidence
// tier, or compatibility digest (ADR-2026-08-05 D4/D5). This package has no
// opinion on how a caller gathers these facts — from an installed-harness
// registry, endpoint reachability probe, credential store, capability matrix,
// or evidence-tier attestation; it only enforces that the gate never widens
// what admission already granted.
type ClaimLocalReality struct {
	// PlacementID is this host's own placement identity, published as the
	// exact host on a successful claim.
	PlacementID string
	// HarnessAvailable reports whether the admitted HarnessRef (exact id and
	// version) is actually installed and runnable on this host.
	HarnessAvailable bool
	// EndpointReachable reports whether the admitted ServingEndpointRef is
	// reachable from this host.
	EndpointReachable bool
	// AuthBindingAvailable reports whether the admitted AuthBindingRef can be
	// proven available (materialized) on this host.
	AuthBindingAvailable bool
	// AvailableCapabilities is the full set of capabilities this host can
	// actually deliver right now. A granted capability absent here is a
	// regression and denies the claim; this field is never used to widen a
	// capability the admission receipt did not already grant.
	AvailableCapabilities []CapabilityRequirement
	// EvidenceTier is the strongest evidence tier this host can currently back
	// for the admitted cell.
	EvidenceTier EvidenceTier
	// RuntimeInventoryDigest is this host's own live runtime-inventory digest
	// (lower-case hex SHA-256), which replaces the pool-level digest on a
	// successful claim.
	RuntimeInventoryDigest string
	// ConflictingClaimID, when non-empty and different from the claim id being
	// evaluated, names an already-active claim on this placement. The caller
	// supplies this from its own bookkeeping — EvaluateClaim keeps no claim
	// registry of its own.
	ConflictingClaimID string
}

var runtimeDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EvaluateClaim re-runs the narrow-only claim gate (ADR-2026-08-05 D4) for a
// claim-bound admission against a claiming host's local reality and returns
// the typed claim result the platform persists as the immutable ClaimReceipt
// (D5). It never widens the admitted cell and never assembles an alternative:
// an unsatisfied predicate is a typed refusal, not a substitution — fallback
// stays denied by default at claim time exactly as it is at admission time
// (D3).
//
// admission must already be an admitted, claim-bound receipt; callers gate on
// that shape (e.g. PlacementRef.Resolution == PlacementClaimBound) before
// invoking the gate — passing an exact or non-admitted receipt is a caller
// error, reported as a *ContractError rather than a denied claim, because no
// claim was ever eligible to run.
func EvaluateClaim(admission ImmutableAdmissionReceipt, claimID string, local ClaimLocalReality, recordedAt time.Time) (ImmutableClaimReceipt, error) {
	receipt := admission.Value()
	if receipt.Decision != AdmissionAdmitted || receipt.Cell == nil {
		return ImmutableClaimReceipt{}, contractError(ErrorInvalidReference, nil, "a claim gate requires an admitted receipt")
	}
	cell := *receipt.Cell
	if cell.Placement.Kind != PlacementPool || cell.Placement.Resolution != PlacementClaimBound {
		return ImmutableClaimReceipt{}, contractError(ErrorInvalidReference, []string{"placement"}, "claim gate requires a claim-bound pool admission")
	}
	if strings.TrimSpace(claimID) == "" {
		return ImmutableClaimReceipt{}, contractError(ErrorMissingRequiredField, []string{"claimId"}, "claim id is required")
	}

	claimReceiptID, err := claimReceiptDigestID(receipt.ReceiptID, claimID)
	if err != nil {
		return ImmutableClaimReceipt{}, err
	}

	out := ClaimReceipt{
		ContractVersion:    ContractVersion,
		ClaimReceiptID:     claimReceiptID,
		AdmissionReceiptID: receipt.ReceiptID,
		ClaimID:            claimID,
		RecordedAt:         recordedAt.UTC().Format(time.RFC3339Nano),
	}

	if code, detail, ok := narrowOnlyClaimDenial(cell, claimID, local); !ok {
		out.Decision = ClaimDenied
		out.DenialCode = code
		out.DenialDetail = detail
	} else {
		effective := cell
		effective.Placement = PlacementRef{ID: local.PlacementID, Kind: PlacementHost, Resolution: PlacementExact}
		effective.RuntimeInventoryDigest = local.RuntimeInventoryDigest
		out.Decision = ClaimClaimed
		out.EffectiveCell = &effective
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return ImmutableClaimReceipt{}, fmt.Errorf("executioncell: marshal claim receipt: %w", err)
	}
	claim, err := DecodeClaimReceipt(raw)
	if err != nil {
		return ImmutableClaimReceipt{}, fmt.Errorf("executioncell: validate claim receipt: %w", err)
	}
	// A locally-computed claim must satisfy the same narrow-only invariant
	// enforced on any externally-supplied claim receipt — belt and
	// suspenders: a bug in narrowOnlyClaimDenial or the effective-cell copy
	// above must fail closed here rather than escape as a bad receipt.
	if err := AssertNarrowClaim(admission, claim); err != nil {
		return ImmutableClaimReceipt{}, fmt.Errorf("executioncell: computed claim does not narrow admission: %w", err)
	}
	return claim, nil
}

// narrowOnlyClaimDenial evaluates the admission predicates against local
// reality and returns the first unsatisfied predicate's typed denial code and
// detail. ok is true only when every predicate is satisfied. Checks run in a
// fixed, documented order so two hosts presented with the same conflicting
// facts report the same code; none of them may substitute or widen — a
// failure here is always a straight denial, never an attempt to fall back to
// a different axis.
func narrowOnlyClaimDenial(cell ResolvedExecutionCell, claimID string, local ClaimLocalReality) (code ClaimDenialCode, detail string, ok bool) {
	switch {
	case local.ConflictingClaimID != "" && local.ConflictingClaimID != claimID:
		return ClaimConflict, fmt.Sprintf("placement is already held by claim %q", local.ConflictingClaimID), false
	case strings.TrimSpace(local.PlacementID) == "":
		return ClaimHostIneligible, "no local placement identity was supplied", false
	case !local.HarnessAvailable:
		return ClaimHostIneligible, fmt.Sprintf("admitted harness %s@%s is not available on this host", cell.Harness.ID, cell.Harness.Version), false
	case !local.EndpointReachable:
		return ClaimHostIneligible, fmt.Sprintf("admitted serving endpoint %s is not reachable from this host", cell.Endpoint.ID), false
	case !local.AuthBindingAvailable:
		return ClaimAuthUnavailable, fmt.Sprintf("admitted auth binding %s is not available on this host", cell.AuthBinding.ID), false
	case !capabilitiesSatisfied(cell.GrantedCapabilities, local.AvailableCapabilities):
		return ClaimCapabilityRegressed, "a granted capability is not available on this host", false
	case evidenceTierRank(local.EvidenceTier) < evidenceTierRank(cell.EvidenceTier):
		return ClaimEvidenceRegressed, fmt.Sprintf("this host can only back evidence tier %q, admission requires %q", local.EvidenceTier, cell.EvidenceTier), false
	case !runtimeDigestPattern.MatchString(local.RuntimeInventoryDigest):
		return ClaimInventoryChanged, "no live runtime inventory digest is available for this host", false
	}
	return "", "", true
}

// capabilitiesSatisfied reports whether every granted capability is present,
// name and parameter digest, in the host's available set. It is a pure
// subset check: available may be a superset of granted, but never the other
// way — this is the only shape a narrow-only gate may compare capabilities
// with.
func capabilitiesSatisfied(granted, available []CapabilityRequirement) bool {
	for _, want := range granted {
		found := false
		for _, have := range available {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// evidenceTierRank orders evidence tiers from weakest to strongest. An
// unrecognized tier ranks below every known tier so an unknown value fails
// closed rather than silently satisfying the gate.
var evidenceTierOrder = map[EvidenceTier]int{
	EvidenceDeclared:            0,
	EvidenceImplemented:         1,
	EvidenceUnitVerified:        2,
	EvidenceIntegrationVerified: 3,
	EvidenceSmoked:              4,
	EvidenceProductionEligible:  5,
}

func evidenceTierRank(tier EvidenceTier) int {
	if rank, ok := evidenceTierOrder[tier]; ok {
		return rank
	}
	return -1
}

func claimReceiptDigestID(admissionReceiptID, claimID string) (string, error) {
	digest, err := DigestContractValue(struct {
		AdmissionReceiptID string `json:"admissionReceiptId"`
		ClaimID            string `json:"claimId"`
	}{admissionReceiptID, claimID})
	if err != nil {
		return "", err
	}
	return "claim_" + digest[:24], nil
}
