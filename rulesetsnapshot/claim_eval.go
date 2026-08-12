// Package rulesetsnapshot claim_eval.go — projects a cached, verified
// Snapshot onto the two questions the daemon's claim path
// (executioncell.EvaluateClaim, via
// daemon.ClaimGateProvider) needs answered when a live control plane cannot
// answer them itself:
//
//  1. EvaluatePermission — is this claim's target pool STILL granted, per
//     the freshest cached snapshot? An admission receipt may be older than
//     the snapshot; a pool an org has since disabled or dropped from every
//     capacity profile must not go on being claimed just because a stale
//     admission still names it. This is a NEW check — nothing in the
//     existing narrow-only claim gate (executioncell/claim_gate.go) re-asks
//     "is this grant still current", only "does local reality still match
//     what was admitted".
//  2. BuildClaimLocalReality — a conservative, OSS-shipped default answer
//     to executioncell.ClaimLocalReality when no other (host-local) source
//     is wired at all. See its doc comment for exactly how bounded this is.
package rulesetsnapshot

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
)

// eligiblePoolStatus / eligibleHostStatus are the contract's "healthy"
// values for pool.status / host.status (see PoolHostInventorySection's
// field docs). Anything else — paused/draining/disabled for a pool,
// draining/unhealthy/offline for a host — is NOT eligible; this fails
// closed on an unrecognized status too, deliberately: a status this
// package has never seen is not assumed healthy.
const (
	eligiblePoolStatus = "active"
	eligibleHostStatus = "ready"
)

// EvaluatePermission re-checks that cell's target pool is still eligible
// per snap: present in the pool/host inventory, in an eligible status, and
// named by at least one capacity profile's ordered pool list (the
// "profile candidates" input — a pool no capacity profile names anymore is
// exactly what an org revoking a grant looks like in this wire shape).
// Returns nil when cell's placement is not pool-kind (nothing to
// re-check) or when the pool is eligible; otherwise a *PermissionRefusedError
// wrapping ErrPermissionRefused.
func EvaluatePermission(cell executioncell.ResolvedExecutionCell, snap Snapshot) error {
	if cell.Placement.Kind != executioncell.PlacementPool {
		return nil
	}
	poolID := cell.Placement.ID
	pool := findPool(snap.Sections.PoolHostInventory.Pools, poolID)
	if pool == nil {
		return &PermissionRefusedError{PoolID: poolID, Reason: "pool is absent from the cached ruleset snapshot's pool/host inventory"}
	}
	if pool.Status != eligiblePoolStatus {
		return &PermissionRefusedError{PoolID: poolID, Reason: fmt.Sprintf("pool status %q is not eligible to claim", pool.Status)}
	}
	if !poolNamedByAnyProfile(snap.Sections.CapacityProfiles, poolID) {
		return &PermissionRefusedError{PoolID: poolID, Reason: "pool is not named by any capacity profile's pool list in the cached ruleset snapshot"}
	}
	return nil
}

func findPool(pools []Pool, id string) *Pool {
	for i := range pools {
		if pools[i].ID == id {
			return &pools[i]
		}
	}
	return nil
}

func poolNamedByAnyProfile(section CapacityProfilesSection, poolID string) bool {
	for _, profile := range section.Profiles {
		for _, id := range profile.PoolIDs {
			if id == poolID {
				return true
			}
		}
	}
	return false
}

// findHost returns the SnapshotHost row for hostID within poolID, or nil
// when the snapshot's inventory has no such row (e.g. this host registered
// after the cached snapshot was compiled — not itself disqualifying; see
// BuildClaimLocalReality).
func findHost(hosts []Host, poolID, hostID string) *Host {
	for i := range hosts {
		if hosts[i].ExecutionPoolID == poolID && hosts[i].ID == hostID {
			return &hosts[i]
		}
	}
	return nil
}

// harnessKnownToMatrix reports whether harnessID appears as ANY provider's
// harness for ANY auth mode in the execution-cell matrix section.
//
// Deliberately coarse: the matrix's auth-mode vocabulary (an org's
// credential-ownership axis — "byok"/"metered"/… in the publisher's own
// contract) is a different axis from this repo's own
// executioncell.AuthMechanism ("api_key"/"oauth"/…, an endpoint
// authentication axis — see agent/endpoint.go's doc comment on the
// distinction). Cross-walking the two would assert a mapping this package
// has no authority to define, so the OSS default asks only the question
// both vocabularies agree on: does the org's cached inventory know this
// harness id at all.
func harnessKnownToMatrix(matrix ExecutionCellMatrixSection, harnessID string) bool {
	for _, provider := range matrix.Providers {
		for _, id := range provider.HarnessByAuthMode {
			if id == harnessID {
				return true
			}
		}
	}
	return false
}

// BuildClaimLocalReality derives a conservative executioncell.ClaimLocalReality
// from snap for cell, reporting as this host: hostID.
//
// This is the OSS-shipped DEFAULT fail-static evaluator, used only when the
// daemon has no other (genuinely host-local) source wired at all — see
// daemon.FailStaticClaimGateProvider, which prefers a real Live provider's
// answer whenever one is wired and only falls back to this. Three of the
// six fields it must answer are honestly NOT derivable from an org-wide
// snapshot at all (whether a harness binary is actually installed, whether
// a credential is actually materialized, and this host's exact capability
// set are all facts about ONE machine, not the org) — those are the fields
// a real host-local ClaimGateProvider exists to answer. This default is
// therefore intentionally bounded and documented per field rather than
// guessing:
//
//   - HarnessAvailable / EndpointReachable / AuthBindingAvailable: proxied
//     by harnessKnownToMatrix — "the org's cached inventory still lists this
//     harness at all" is the only signal an org-wide snapshot can honestly
//     offer for a host-local availability question.
//   - AvailableCapabilities / EvidenceTier / RuntimeInventoryDigest: passed
//     through UNCHANGED from the already-admitted cell — the bounded
//     assumption that what was true when the platform admitted this cell is
//     still true within the snapshot's own TTL window. This is exactly the
//     kind of assumption fail-static accepts deliberately (ADR-2026-08-12
//     §D6: "an evaluator holding a stale snapshot is a designed state") and
//     exactly why RefuseAfter exists: past that bound, the daemon refuses
//     loudly instead of extending the assumption indefinitely.
func BuildClaimLocalReality(cell executioncell.ResolvedExecutionCell, snap Snapshot, hostID string) (executioncell.ClaimLocalReality, error) {
	if strings.TrimSpace(hostID) == "" {
		return executioncell.ClaimLocalReality{}, fmt.Errorf("rulesetsnapshot: BuildClaimLocalReality: hostID is required")
	}
	known := harnessKnownToMatrix(snap.Sections.ExecutionCellMatrix, cell.Harness.ID)

	// A pool-bound cell additionally checks whether the snapshot has a host
	// row for THIS host in THIS pool and, when it does, requires that row to
	// be in an eligible status — a coarser, always-available fallback layered
	// under the newer per-host signal (see findHost's doc comment: an
	// unknown host row is not itself disqualifying, since the daemon may have
	// registered after the cached snapshot compiled).
	if cell.Placement.Kind == executioncell.PlacementPool {
		if host := findHost(snap.Sections.PoolHostInventory.Hosts, cell.Placement.ID, hostID); host != nil {
			known = known && host.Status == eligibleHostStatus
		}
	}

	return executioncell.ClaimLocalReality{
		PlacementID:            hostID,
		HarnessAvailable:       known,
		EndpointReachable:      known,
		AuthBindingAvailable:   known,
		AvailableCapabilities:  cell.GrantedCapabilities,
		EvidenceTier:           cell.EvidenceTier,
		RuntimeInventoryDigest: cell.RuntimeInventoryDigest,
	}, nil
}
