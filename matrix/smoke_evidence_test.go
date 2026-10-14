package matrix

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSmokeEvidence_NoHandTypedValues is the Done-when guard: the generator
// derives every cell's ProvingLanes/Stability/Smoked from the smoke-lane
// registry plus the last gate outcomes, and the hand-authored literals carry
// none of them. buildCells already rejects a literal that sets any of the
// three, so this test pins that rejection at the boundary: a literal with
// typed evidence must fail the build loudly rather than silently certify
// itself.
func TestSmokeEvidence_NoHandTypedValues(t *testing.T) {
	t.Parallel()
	harnesses, harnessByName, err := buildHarnesses()
	if err != nil {
		t.Fatalf("buildHarnesses(): %v", err)
	}
	if len(harnesses) == 0 {
		t.Fatal("buildHarnesses(): no harnesses")
	}
	_, endpointByCompany := buildEndpoints()

	typed := HarnessEndpointCell{
		Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: agent.HostLocal,
		Protocol: agent.ProtoStub, Transport: agent.TransportDirectAPI,
		AuthModes:      []agent.AuthMode{agent.AuthLocal},
		ProvingLanes:   []SmokeLaneName{"TestSiblingContextReposSmoke"},
		Stability:      "stable",
		Smoked:         true,
		StructuredVia:  "stub",
		BringsOwnAuth:  true,
		CostModel:      agent.CostLocalFree,
		OneShot:        true,
		NativeJSONMode: true,
	}
	if _, err := buildCells([]HarnessEndpointCell{typed}, harnessByName, endpointByCompany); err == nil {
		t.Fatal("buildCells() accepted a hand-typed ProvingLanes/Stability/Smoked literal; want a derivation-only error")
	} else if !strings.Contains(err.Error(), "smoke evidence is derived, not hand-authored") {
		t.Fatalf("buildCells() error = %q; want the derivation-only refusal", err)
	}
}

// TestSmokeEvidence_RemovingLaneUnsmokesCell is the second Done-when guard:
// the registry named a lane, the gate stopped recording it (it was removed
// or renamed without updating the registry), the cell must read not smoked
// AND the build must fail loudly. The loud failure is the point — a quiet
// un-smoke would be as unactionable as a quiet smoke.
func TestSmokeEvidence_RemovingLaneUnsmokesCell(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	stubKey := CellKey{Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: agent.HostLocal}
	found := false
	for _, c := range built.Matrix.Cells {
		if c.Key() != stubKey {
			continue
		}
		found = true
		if !c.Smoked || c.Stability != "stable" || len(c.ProvingLanes) == 0 {
			t.Fatalf("stub cell = stability %q smoked %v lanes %v; want stable/smoked with lanes (gate is green)", c.Stability, c.Smoked, c.ProvingLanes)
		}
	}
	if !found {
		t.Fatal("stub cell missing from built matrix")
	}

	// Drop one lane from the gate outcomes (as if the lane was removed from
	// the suite without updating the registry): derivation must fail loudly.
	gate := make(SmokeGateOutcomes, len(lastSmokeGateOutcomes))
	for lane, passed := range lastSmokeGateOutcomes {
		gate[lane] = passed
	}
	delete(gate, "TestSiblingContextReposSmoke")
	if _, err := applySmokeEvidence(validCells, cellSmokeProofs, gate); err == nil {
		t.Fatal("applySmokeEvidence() accepted a registry lane with no gate outcome; want a loud failure")
	} else if !strings.Contains(err.Error(), "no recorded gate outcome") {
		t.Fatalf("applySmokeEvidence() error = %q; want the missing-outcome refusal", err)
	}

	// Flip one lane to failed: the cell reads not smoked (fail closed).
	gate = make(SmokeGateOutcomes, len(lastSmokeGateOutcomes))
	for lane, passed := range lastSmokeGateOutcomes {
		gate[lane] = passed
	}
	gate["TestSiblingContextReposSmoke"] = false
	derived, err := applySmokeEvidence(validCells, cellSmokeProofs, gate)
	if err != nil {
		t.Fatalf("applySmokeEvidence(): %v", err)
	}
	for _, c := range derived {
		if c.Key() != stubKey {
			continue
		}
		if c.Smoked || c.Stability != UnprovenStability {
			t.Errorf("stub cell with failed lane = stability %q smoked %v; want %q/not-smoked", c.Stability, c.Smoked, UnprovenStability)
		}
	}
}

// TestSmokeEvidence_UnprovenCellsReadExperimental pins the derivation for
// every cell with no proving lane: experimental and not smoked. This is the
// tier the unproven cells already carried, now computed instead of typed —
// including the two cells whose old typed values were actively wrong (the
// gateway cell that cited a lane driving a different harness, and the
// legacy default cells whose smoked/stable values named no lane at all).
func TestSmokeEvidence_UnprovenCellsReadExperimental(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	proven := map[CellKey]bool{}
	for _, p := range cellSmokeProofs {
		proven[p.Cell] = true
	}
	if len(proven) == 0 {
		t.Fatal("smoke registry is empty; nothing proves anything")
	}
	unproven := 0
	for _, c := range built.Matrix.Cells {
		if proven[c.Key()] {
			continue
		}
		unproven++
		if c.Smoked || c.Stability != UnprovenStability || len(c.ProvingLanes) != 0 {
			t.Errorf("cell %+v = stability %q smoked %v lanes %v; want %q/not-smoked/no-lanes", c.Key(), c.Stability, c.Smoked, c.ProvingLanes, UnprovenStability)
		}
	}
	if unproven == 0 {
		t.Fatal("every cell names a proving lane; the unproven default is untested")
	}
}

// TestSmokeEvidence_GatewayCellNotSmokedByForeignLane pins the
// misattribution fix directly: the gateway cell's proof must come from a
// lane driving its own harness, and today no such lane exists, so the cell
// reads not smoked even though the gateway completed-turn lane is green.
func TestSmokeEvidence_GatewayCellNotSmokedByForeignLane(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	gatewayKey := CellKey{Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostGateway}
	for _, c := range built.Matrix.Cells {
		if c.Key() != gatewayKey {
			continue
		}
		if c.Smoked {
			t.Errorf("gateway cell reads smoked with proving lanes %v; the only gateway turn lane drives pi, not opencode", c.ProvingLanes)
		}
		return
	}
	t.Fatal("gateway cell missing from built matrix")
}

// TestSmokeEvidence_RegistryAndGateInLockstep pins that the two inputs meet:
// every registered lane has a gate outcome, every gate outcome is claimed by
// a cell, no cell names a lane twice, and no proof entry names a cell that
// does not exist. applySmokeEvidence enforces all four; this test pins the
// committed literals satisfy them (a red here means the literals drifted,
// not the check).
func TestSmokeEvidence_RegistryAndGateInLockstep(t *testing.T) {
	t.Parallel()
	if _, err := applySmokeEvidence(validCells, cellSmokeProofs, lastSmokeGateOutcomes); err != nil {
		t.Fatalf("applySmokeEvidence(): committed registry/gate literals do not meet: %v", err)
	}
}

// TestSmokeEvidence_ProvenCellsDeriveBetaOrStable pins the depth rule: the
// direct opencode cell (spawn/event-stream/teardown lanes, no completed
// turn) derives beta+smoked; the stub cell (a full completed-turn lane)
// derives stable+smoked.
func TestSmokeEvidence_ProvenCellsDeriveBetaOrStable(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	want := map[CellKey]struct {
		stability string
		smoked    bool
	}{
		{Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect}: {stability: "beta", smoked: true},
		{Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: agent.HostLocal}:        {stability: "stable", smoked: true},
	}
	for _, c := range built.Matrix.Cells {
		expected, ok := want[c.Key()]
		if !ok {
			continue
		}
		if c.Stability != expected.stability || c.Smoked != expected.smoked || len(c.ProvingLanes) == 0 {
			t.Errorf("cell %+v = stability %q smoked %v lanes %v; want %q/%v with lanes", c.Key(), c.Stability, c.Smoked, c.ProvingLanes, expected.stability, expected.smoked)
		}
		delete(want, c.Key())
	}
	if len(want) != 0 {
		t.Fatalf("missing proven cells: %+v", want)
	}
}
