package matrix

// Smoke-lane registry: which black-box smoke test proves each harness cell.
//
// A harness cell reads as smoked or stable only when a smoke lane actually
// proved it, never because someone typed it. The per-cell Stability/Smoked
// literals used to live beside the routing data in cells.go; nothing tied
// them to a lane that passed, so some were wrong (a retired harness once
// read stable/smoked on a registration-only check, and a gateway cell cited
// a lane that actually drives a different harness). This file replaces those
// literals with a registry of proof pointers plus a derivation rule:
//
//   - Each cell names the smoke test(s) that prove it, by test name. Naming
//     a test is not a claim; it is a pointer to evidence.
//   - Whether those tests passed in the last release gate lives in
//     smoke_gate.go (the observed outcome, refreshed from the gate log).
//   - deriveCellSmokeEvidence combines the two: a cell with no proving lane
//     reads as not smoked, and a named lane that did not pass un-smokes its
//     cell. Stability is derived from the depth of the passing proof:
//     experimental until a lane passes, beta once the spawn pipeline is
//     proven, stable only once a completed model turn is proven.
//
// The registry is emitted per cell (provingLanes) into the generated JSON so
// the smoke suite can assert every named test exists and ran in the last
// gate without importing this module, and so downstream matrix consumers see
// the same proof mapping the generator used.
//
// What counts as a proving lane: the lane must drive the cell's own
// (harness x endpoint x host) integration end to end through the shipped
// binary. Registration-only checks, config-shape unit tests, and lanes that
// drive a different harness do not prove a cell and are not registered —
// they are supporting evidence at most, named in the comments below where
// the distinction matters.

import (
	"fmt"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// SmokeLaneName is the name of one black-box smoke test in the sibling
// smoke suite (e.g. "TestOpenCodeHarnessSmoke_RealBinary_Teardown"). Stored
// as a string, never imported: the smoke suite stays free of any dependency
// on this module, and this module stays free of any dependency on it. The
// names meet in the generated JSON.
type SmokeLaneName string

// SmokeLaneProof is one lane's proof for a cell: which lane, and whether
// that lane drives a completed model turn (the depth that confers stable).
// A lane that proves the spawn/event-stream/teardown pipeline without a
// completed turn confers beta once it passes.
type SmokeLaneProof struct {
	Lane SmokeLaneName
	// CompletedTurn is true only when the lane asserts a full prompt to
	// terminal-result round trip against the pinned binary. Lanes that fail
	// fast on a bogus model, stop at the handshake, or teardown mid-run
	// leave this false: they prove the pipeline, not the turn.
	CompletedTurn bool
}

// CellSmokeProof maps one cell to the smoke lanes that prove it. A cell
// with no entry here has no proving lane and reads as not smoked.
type CellSmokeProof struct {
	Cell  CellKey
	Lanes []SmokeLaneProof
}

// UnprovenStability is the stability a cell reads when no smoke lane has
// proven it. It matches the tier unproven cells already carried, so the
// derivation only ever promotes from here on passing proof.
const UnprovenStability = "experimental"

// cellSmokeProofs is the smoke-lane registry: the only place that says
// which lane proves which cell. Entries name lanes that drive the cell's
// own integration; the comments on the ABSENT cells are load-bearing too —
// each says which lane was considered and why it does not prove that cell.
var cellSmokeProofs = []CellSmokeProof{
	{
		// The stub provider session runs to Status="completed" end to end,
		// so this lane proves the full turn and confers stable.
		Cell: CellKey{Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: agent.HostLocal},
		Lanes: []SmokeLaneProof{
			{Lane: "TestSiblingContextReposSmoke", CompletedTurn: true},
		},
	},
	{
		// The Lane-A real-binary lanes drive a genuine spawn of the pinned
		// binary through the NDJSON event-stream to a well-formed terminal
		// Result, plus deliberate teardown with no orphan process. The
		// model is deliberately unresolvable so the turn fails fast without
		// network; that proves the spawn/prompt/event-stream/teardown
		// pipeline, not a completed turn, so these confer beta.
		Cell: CellKey{Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect},
		Lanes: []SmokeLaneProof{
			{Lane: "TestOpenCodeHarnessSmoke_RealBinary_SpawnEventStreamTerminal"},
			{Lane: "TestOpenCodeHarnessSmoke_RealBinary_Teardown"},
		},
	},
	// Deliberately absent:
	//
	// - opencode x openai x gateway: the gateway completed-turn lane drives
	//   the pi harness (provider "pi", servingHost "gateway"), not opencode.
	//   Citing it here would repeat the misattribution this registry
	//   exists to end. No lane drives opencode through the gateway, so the
	//   cell reads as not smoked until one does.
	// - pi x anthropic/openai x direct: the pi completed-turn lane is
	//   gateway-served (servingHost "gateway" threads a worker-local
	//   loopback endpoint), so it proves no direct-host cell; the
	//   handshake-only lane stops before any model turn and, like any
	//   registration-only check, confers nothing. Both cells stay
	//   experimental/unsmoked until a direct-host turn lane exists.
	// - claude-code, codex, gemini-direct, ollama cells: no real-binary
	//   lane drives these harnesses end to end (the codex MCP lane asserts
	//   config wire shape, not a spawn). Their former smoked/stable values
	//   were ungrounded and now correctly read as unproven.
	// - opencode x google x local (the north-star cell): no lane drives it
	//   yet; it stays experimental/unsmoked as before, now computably so.
}

// SmokeProofsByCell indexes the registry by cell for the derivation.
// Exported for the parity test's lockstep cross-check; the generator path
// reads the list form through applySmokeEvidence.
func SmokeProofsByCell() map[CellKey][]SmokeLaneProof {
	out := make(map[CellKey][]SmokeLaneProof, len(cellSmokeProofs))
	for _, p := range cellSmokeProofs {
		out[p.Cell] = append([]SmokeLaneProof(nil), p.Lanes...)
	}
	return out
}

// deriveCellSmokeEvidence computes the (stability, smoked, provingLanes)
// triple for one cell from the registry and the last gate outcomes.
//
//   - No proving lane: experimental, not smoked.
//   - Any named lane missing from the gate outcomes, or recorded as not
//     passed: experimental, not smoked (fail closed — a lane that did not
//     provably pass proves nothing).
//   - Every named lane passed: smoked, with stable iff at least one passing
//     lane drove a completed turn, else beta.
//
// An unknown lane name is never silently accepted: derive returns an error
// naming it, so a typo'd lane fails the build instead of quietly un-smoking
// (or, worse, quietly smoking) a cell.
func deriveCellSmokeEvidence(key CellKey, lanes []SmokeLaneProof, gate SmokeGateOutcomes) (stability string, smoked bool, proving []SmokeLaneName, err error) {
	if len(lanes) == 0 {
		return UnprovenStability, false, nil, nil
	}
	proving = make([]SmokeLaneName, 0, len(lanes))
	completed := false
	for _, lane := range lanes {
		if lane.Lane == "" {
			return "", false, nil, fmt.Errorf("cell %+v: proving lane with empty name", key)
		}
		passed, ok := gate[lane.Lane]
		if !ok {
			return "", false, nil, fmt.Errorf("cell %+v: proving lane %q has no recorded gate outcome", key, lane.Lane)
		}
		proving = append(proving, lane.Lane)
		if !passed {
			return UnprovenStability, false, proving, nil
		}
		completed = completed || lane.CompletedTurn
	}
	sort.Slice(proving, func(i, j int) bool { return proving[i] < proving[j] })
	if completed {
		return "stable", true, proving, nil
	}
	return "beta", true, proving, nil
}

// applySmokeEvidence returns a copy of cells with every cell's ProvingLanes,
// Stability, and Smoked fields derived from the registry and gate outcomes.
// The hand-authored input must carry none of those (the parity test asserts
// it); derivation is the only writer. Unknown registry cells and unknown
// lane names fail loudly, mirroring the generator's posture everywhere else.
func applySmokeEvidence(cells []HarnessEndpointCell, proofs []CellSmokeProof, gate SmokeGateOutcomes) ([]HarnessEndpointCell, error) {
	byCell := make(map[CellKey][]SmokeLaneProof, len(proofs))
	for _, p := range proofs {
		if _, dup := byCell[p.Cell]; dup {
			return nil, fmt.Errorf("smoke registry: duplicate proof entry for cell %+v", p.Cell)
		}
		seen := map[SmokeLaneName]bool{}
		for _, lane := range p.Lanes {
			if seen[lane.Lane] {
				return nil, fmt.Errorf("smoke registry: cell %+v names lane %q twice", p.Cell, lane.Lane)
			}
			seen[lane.Lane] = true
		}
		byCell[p.Cell] = p.Lanes
	}

	known := make(map[CellKey]bool, len(cells))
	for _, c := range cells {
		known[c.Key()] = true
	}
	for _, p := range proofs {
		if !known[p.Cell] {
			return nil, fmt.Errorf("smoke registry: proof entry for unknown cell %+v", p.Cell)
		}
	}
	for lane := range gate {
		registered := false
		for _, p := range proofs {
			for _, l := range p.Lanes {
				if l.Lane == lane {
					registered = true
					break
				}
			}
			if registered {
				break
			}
		}
		if !registered {
			return nil, fmt.Errorf("smoke registry: gate outcome for unregistered lane %q", lane)
		}
	}

	out := make([]HarnessEndpointCell, 0, len(cells))
	for _, c := range cells {
		stability, smoked, proving, err := deriveCellSmokeEvidence(c.Key(), byCell[c.Key()], gate)
		if err != nil {
			return nil, err
		}
		c.ProvingLanes = proving
		c.Stability = stability
		c.Smoked = smoked
		out = append(out, c)
	}
	return out, nil
}
