package rulesetsnapshot

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

func testSnapshot(t *testing.T) Snapshot {
	t.Helper()
	sections, err := decodeTypedSections(testSections(t))
	if err != nil {
		t.Fatalf("decodeTypedSections: %v", err)
	}
	return Snapshot{OrgID: "org1", Revision: 1, RulesetRev: "org1@1", Sections: sections}
}

func poolBoundCell(poolID string) executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion:     executioncell.ContractVersion,
		Harness:             executioncell.HarnessRef{ID: "harness1", Version: "v1"},
		Model:               executioncell.ModelRef{ID: "model1", Author: "acme"},
		Endpoint:            executioncell.ServingEndpointRef{ID: "endpoint1", Protocol: "http", Operator: "acme", Revision: "r1"},
		AuthBinding:         executioncell.AuthBindingRef{ID: "auth1", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled, Authority: "acme", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment},
		Placement:           executioncell.PlacementRef{ID: poolID, Kind: executioncell.PlacementPool, Resolution: executioncell.PlacementClaimBound},
		SessionMode:         executioncell.SessionAutonomous,
		GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("a", 64), RuntimeInventoryDigest: strings.Repeat("b", 64),
	}
}

func TestEvaluatePermission_NonPoolPlacementIsNoOp(t *testing.T) {
	t.Parallel()
	cell := poolBoundCell("pool1")
	cell.Placement = executioncell.PlacementRef{ID: "host1", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact}
	if err := EvaluatePermission(cell, testSnapshot(t)); err != nil {
		t.Fatalf("EvaluatePermission on a non-pool placement returned %v, want nil", err)
	}
}

func TestEvaluatePermission_GrantedAndHealthy(t *testing.T) {
	t.Parallel()
	if err := EvaluatePermission(poolBoundCell("pool1"), testSnapshot(t)); err != nil {
		t.Fatalf("EvaluatePermission = %v, want nil for a granted, active pool", err)
	}
}

func TestEvaluatePermission_PoolAbsent(t *testing.T) {
	t.Parallel()
	err := EvaluatePermission(poolBoundCell("pool-does-not-exist"), testSnapshot(t))
	if !errors.Is(err, ErrPermissionRefused) {
		t.Fatalf("error = %v, want ErrPermissionRefused", err)
	}
}

func TestEvaluatePermission_PoolUnhealthy(t *testing.T) {
	t.Parallel()
	snap := testSnapshot(t)
	snap.Sections.PoolHostInventory.Pools[0].Status = "disabled"
	err := EvaluatePermission(poolBoundCell("pool1"), snap)
	if !errors.Is(err, ErrPermissionRefused) {
		t.Fatalf("error = %v, want ErrPermissionRefused for a disabled pool", err)
	}
}

func TestEvaluatePermission_PoolNotGrantedByAnyProfile(t *testing.T) {
	t.Parallel()
	snap := testSnapshot(t)
	snap.Sections.CapacityProfiles.Profiles[0].PoolIDs = []string{"some-other-pool"}
	err := EvaluatePermission(poolBoundCell("pool1"), snap)
	if !errors.Is(err, ErrPermissionRefused) {
		t.Fatalf("error = %v, want ErrPermissionRefused for a pool no profile names", err)
	}
}

func TestBuildClaimLocalReality_HarnessKnown(t *testing.T) {
	t.Parallel()
	cell := poolBoundCell("pool1")
	cell.Harness.ID = "harness1" // present in testSections' matrix
	reality, err := BuildClaimLocalReality(cell, testSnapshot(t), "host1")
	if err != nil {
		t.Fatalf("BuildClaimLocalReality: %v", err)
	}
	if !reality.HarnessAvailable || !reality.EndpointReachable || !reality.AuthBindingAvailable {
		t.Fatalf("reality = %+v, want all three availability flags true for a known harness on a ready host", reality)
	}
	if reality.PlacementID != "host1" {
		t.Fatalf("PlacementID = %q, want host1", reality.PlacementID)
	}
	if reality.RuntimeInventoryDigest != cell.RuntimeInventoryDigest {
		t.Fatal("RuntimeInventoryDigest was not passed through from the admitted cell")
	}
}

func TestBuildClaimLocalReality_HarnessUnknown(t *testing.T) {
	t.Parallel()
	cell := poolBoundCell("pool1")
	cell.Harness.ID = "harness-nobody-heard-of"
	reality, err := BuildClaimLocalReality(cell, testSnapshot(t), "host1")
	if err != nil {
		t.Fatalf("BuildClaimLocalReality: %v", err)
	}
	if reality.HarnessAvailable {
		t.Fatal("HarnessAvailable = true for a harness absent from the execution-cell matrix")
	}
}

func TestBuildClaimLocalReality_HostRowUnhealthyDenies(t *testing.T) {
	t.Parallel()
	snap := testSnapshot(t)
	snap.Sections.PoolHostInventory.Hosts[0].Status = "unhealthy"
	cell := poolBoundCell("pool1")
	cell.Harness.ID = "harness1"
	reality, err := BuildClaimLocalReality(cell, snap, "host1")
	if err != nil {
		t.Fatalf("BuildClaimLocalReality: %v", err)
	}
	if reality.HarnessAvailable {
		t.Fatal("HarnessAvailable = true despite this host's own inventory row being unhealthy")
	}
}

func TestBuildClaimLocalReality_UnknownHostRowNotDisqualifying(t *testing.T) {
	t.Parallel()
	// A host id absent from the snapshot's inventory (e.g. registered after
	// the snapshot compiled) must not, by itself, deny a claim the
	// pool-level and matrix-level signals otherwise support.
	cell := poolBoundCell("pool1")
	cell.Harness.ID = "harness1"
	reality, err := BuildClaimLocalReality(cell, testSnapshot(t), "host-not-in-snapshot")
	if err != nil {
		t.Fatalf("BuildClaimLocalReality: %v", err)
	}
	if !reality.HarnessAvailable {
		t.Fatal("an unknown host row incorrectly denied a claim the pool/matrix signals otherwise support")
	}
}

func TestBuildClaimLocalReality_RequiresHostID(t *testing.T) {
	t.Parallel()
	_, err := BuildClaimLocalReality(poolBoundCell("pool1"), testSnapshot(t), "")
	if err == nil {
		t.Fatal("BuildClaimLocalReality accepted an empty hostID")
	}
}
