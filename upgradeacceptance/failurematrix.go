package upgradeacceptance

// failurematrix.go — the D6 failure matrix as table-driven cases.
//
// Each case names a failure the upgrade harness must handle and the required
// outcome. The matrix drives two runners:
//
//   - The container driver (container/upgrade-acceptance.sh) executes the
//     live-seat cases across the N→N+1 restart.
//   - The in-repo test (accept_test.go) executes the receiver-semantics
//     cases against the stub receiver and the local queue's exact-replay
//     commit, which is the production entry point for the standalone lane.
//
// A case is either live (needs the container) or local (runs in-repo). The
// in-repo suite runs every local case; the container job runs every case.
// Cases that need adoption behaviour the current build lacks are marked
// requiresAdoption: they fail on current main (the seat dies on upgrade)
// and pass once the adoption slices land. The red-run record names the
// failing case and its cause.

import (
	"testing"
)

// failureCase is one row of the D6 failure matrix.
type failureCase struct {
	// name is the stable case id used by both the Go test and the
	// container driver.
	name string
	// want is the required outcome, one sentence.
	want string
	// local reports whether the case runs in-repo (true) or needs the
	// container with a systemd user manager (false).
	local bool
	// requiresAdoption reports whether the case needs headless shim
	// adoption (slices after this one). Such cases fail on current main.
	requiresAdoption bool
}

// failureMatrix is the D6 failure matrix: every row the acceptance flow
// must demonstrate. It mirrors the failure-modes table of the decision
// record's test plan.
var failureMatrix = []failureCase{
	{
		name:  "seat-survives-upgrade",
		want:  "the worker PID and start identity are unchanged across the N to N+1 restart and the controller generation advances by exactly one",
		local: false, requiresAdoption: true,
	},
	{
		name:  "scope-survives-upgrade",
		want:  "the seat scope still exists after the restart with the same limits and confinement still denies writes outside the workarea",
		local: false, requiresAdoption: true,
	},
	{
		name:  "credential-pushed-after-adoption",
		want:  "N+1 pushes a credential update the runner applies and the next lease refresh succeeds with the rotated worker id",
		local: false, requiresAdoption: true,
	},
	{
		name:  "exactly-one-terminal-commit",
		want:  "touching the trigger yields exactly one terminal commit, the outbox record reads delivered, and the scope is collected",
		local: false, requiresAdoption: true,
	},
	{
		name:  "worker-killed-mid-run",
		want:  "the daemon janitor proves the harness group gone then writes one worker-exited-without-result record and no second record when the runner already wrote one",
		local: false, requiresAdoption: true,
	},
	{
		name:  "harness-finishes-while-daemon-down-post-succeeds",
		want:  "outbox delivered and the new daemon adopts the tombstone and sends nothing",
		local: false, requiresAdoption: true,
	},
	{
		name:  "harness-finishes-while-daemon-down-post-fails",
		want:  "outbox pending and the new daemon replays the exact bytes once and a forced second replay returns the original receipt",
		local: true, requiresAdoption: true,
	},
	{
		name:  "old-daemon-meets-headless-shim",
		want:  "protocol-mismatch quarantine with capacity charged and the seat never killed, reaching the orphan deadline with its outbox record and tombstone",
		local: false, requiresAdoption: true,
	},
	{
		name:  "new-daemon-meets-older-profile-shim",
		want:  "highest overlap selected, or quarantine when none, and never a kill",
		local: false, requiresAdoption: true,
	},
	{
		name:  "adoption-refused",
		want:  "identity mismatch, duplicate identity or peer-credential failure quarantines while the runner keeps refreshing its lease until the orphan deadline ends it",
		local: false, requiresAdoption: true,
	},
	{
		name:  "replacement-crashes-between-welcome-and-batch-commit",
		want:  "the next daemon adopts at the next generation and frames from the crashed controller are rejected",
		local: false, requiresAdoption: true,
	},
	{
		name:  "old-controller-resurfaces-after-adoption",
		want:  "its stop and credential update are rejected as stale",
		local: false, requiresAdoption: true,
	},
	{
		name:  "daemon-killed-without-preflight",
		want:  "the seat survives and adoption proceeds with no fence",
		local: false, requiresAdoption: true,
	},
	{
		name:  "bearer-expires-while-no-daemon-runs",
		want:  "the lease fuse ends the seat as lost-ownership with its outbox record and tombstone and nothing released without terminal evidence",
		local: true, requiresAdoption: true,
	},
	{
		name:  "daemon-returns-before-bearer-expires",
		want:  "the pushed credential update arrives before the old bearer lapses and the lease never misses a tick",
		local: true, requiresAdoption: true,
	},
	{
		name:  "host-reboot",
		want:  "every process dies and boot recovery classifies stale records and tombstones before advertising capacity",
		local: false, requiresAdoption: true,
	},
	{
		name:  "direct-owned-plus-shim-owned-preflight",
		want:  "the preflight refuses with the direct-owned count and returns prepared once the direct-owned seat ends",
		local: true, requiresAdoption: false,
	},
	{
		name:  "scope-creation-refused",
		want:  "the seat is refused before spawn with a typed reason and never falls back to an unscoped shim",
		local: true, requiresAdoption: false,
	},
	{
		name:  "receiver-exact-replay",
		want:  "an exact terminal-status replay returns the original receipt with its revision unchanged and a changed body is a conflict",
		local: true, requiresAdoption: false,
	},
	{
		name:  "receiver-worker-rotation",
		want:  "re-registration rotates the worker id, the old bearer lapses, and the lease refresh succeeds with the rotated pair",
		local: true, requiresAdoption: false,
	},
	{
		name:  "receiver-lease-across-gap",
		want:  "lease refresh keeps succeeding across the restart gap and step heartbeats are recorded without releasing the session",
		local: true, requiresAdoption: false,
	},
	{
		name:  "harness-resume-reports-history",
		want:  "the resumed fake harness reports the full transcript history it loaded from the session-owned state dir",
		local: true, requiresAdoption: false,
	},
}

// matrixLiveNames returns the stable names of the matrix rows that need
// the container. The driver asserts the same set, so a case cannot silently
// stop running in either runner.
func matrixLiveNames() []string {
	var out []string
	for _, c := range failureMatrix {
		if !c.local {
			out = append(out, c.name)
		}
	}
	return out
}

// TestFailureMatrixRegistered pins the matrix shape: every row the
// container driver executes must be listed here, so a case cannot silently
// stop running. The driver asserts the same count.
func TestFailureMatrixRegistered(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range failureMatrix {
		if c.name == "" || c.want == "" {
			t.Fatalf("matrix row %+v needs a stable name and a required outcome", c)
		}
		if seen[c.name] {
			t.Fatalf("duplicate matrix case %q", c.name)
		}
		seen[c.name] = true
	}
	const wantCases = 22
	if len(failureMatrix) != wantCases {
		t.Fatalf("matrix has %d cases, want %d; the container driver asserts the same count", len(failureMatrix), wantCases)
	}
	local := 0
	liveNames := matrixLiveNames()
	for _, c := range failureMatrix {
		if c.local {
			local++
		}
	}
	if len(liveNames) == 0 {
		t.Fatal("matrix lists no live-seat cases; the container driver would run nothing")
	}
	const wantLocal = 8
	if local != wantLocal {
		t.Fatalf("matrix has %d local cases, want %d", local, wantLocal)
	}
}
