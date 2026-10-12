package matrix

// SmokeGateOutcomes is the observed release-gate result for each named
// smoke lane: lane name -> whether that lane passed in the last release
// gate. The generator does not fetch this from anywhere — it is a committed
// literal, refreshed by hand from the release gate log each time the gate
// runs (and whenever a proving lane is added, removed, or renamed). The
// registry (smoke_lanes.go) names the lanes; this file records what the gate
// last said about them. applySmokeEvidence refuses a lane with no outcome
// here and refuses an outcome for a lane no cell names, so the two stay in
// lockstep: every named lane must have run, and every recorded lane must be
// claimed by a cell.
//
// Every value asserted here must be true of the last release gate: the
// smoke suite (sibling repository) is platform-free, and its release-gate
// CI log is the provenance. Refresh protocol: read the gate's per-test
// PASS/SKIP tally (Test outcome tally step), set true for each lane that
// reported PASS with no SKIP, false otherwise, and cite the run in the
// commit message.

// SmokeGateOutcomes maps each registered smoke lane to its last gate result.
type SmokeGateOutcomes map[SmokeLaneName]bool

// lastSmokeGateOutcomes is the observed outcome of the latest release gate
// covering the registered lanes. Sourced from the harness-smoke job's
// per-test tally in the release workflow (same discipline as the pi-lane
// and daemon-lifecycle canary guards there): every registered lane reported
// PASS with no SKIP, so every entry is true. Any future gate where a
// registered lane skips or fails flips its entry to false (or removes the
// cell's proof), un-smoking that cell until the gate is green again.
var lastSmokeGateOutcomes = SmokeGateOutcomes{
	"TestSiblingContextReposSmoke":                                 true,
	"TestOpenCodeHarnessSmoke_RealBinary_SpawnEventStreamTerminal": true,
	"TestOpenCodeHarnessSmoke_RealBinary_Teardown":                 true,
}
