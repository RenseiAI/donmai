package upgradeacceptance

// failurematrix.go — the D6 failure matrix as table-driven cases.
//
// Each case names a failure the upgrade harness must handle and the required
// outcome. A case is either local (it runs in-repo) or live (it needs the
// container with a systemd user manager):
//
//   - Every local case names its driver: the in-repo test that drives it
//     through the production entry points (stub receiver, heartbeat and
//     step-heartbeat beaters, restart preflight route, pi provider).
//     TestFailureMatrixRegistered fails a local row with no driver, and
//     TestSuiteDefinesEveryNamedDriver fails a driver name the package's
//     test files do not define.
//   - Every live case is recorded by the container driver
//     (container/upgrade-acceptance.sh), which reads this list from the
//     record probe built from the same revision, so the two cannot drift.
//
// Cases that need adoption behaviour the current build lacks are marked
// requiresAdoption. Every live case is one: on current main the container
// records seat-survives-upgrade red with its cause and the other live
// cases blocked behind it, until the adoption slices land and the driver
// gains the live-seat assertions.

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
	// requiresAdoption reports whether the row's full outcome needs headless
	// shim adoption (slices after this one). A live row so marked fails on
	// current main; a local row so marked is driven in-repo for the half
	// that does not (receiver, lease and heartbeat semantics), and its live
	// half waits on the adoption slices.
	requiresAdoption bool
	// driver names the in-repo test that drives a local case. Empty for a
	// live case, which only the container driver records.
	driver string
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
		driver: "TestReceiverPendingReplay",
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
		driver: "TestBearerExpiresWhileNoDaemonRuns",
	},
	{
		name:  "daemon-returns-before-bearer-expires",
		want:  "the pushed credential update arrives before the old bearer lapses and the lease never misses a tick",
		local: true, requiresAdoption: true,
		driver: "TestDaemonReturnsBeforeBearerExpires",
	},
	{
		name:  "host-reboot",
		want:  "every process dies and boot recovery classifies stale records and tombstones before advertising capacity",
		local: false, requiresAdoption: true,
	},
	{
		name:  "direct-owned-plus-shim-owned-preflight",
		want:  "with one direct-owned and one shim-owned seat live, the preflight refuses with the direct-owned count and returns prepared once the direct-owned seat ends",
		local: false, requiresAdoption: true,
	},
	{
		name:  "scope-creation-refused",
		want:  "on a user install where creating the seat's transient scope is refused, the seat is refused before spawn with a typed reason and never falls back to an unscoped shim",
		local: false, requiresAdoption: true,
	},
	{
		// The direct-owned half of direct-owned-plus-shim-owned-preflight:
		// no shim-owned seat is live, so the preflight ends not_required
		// rather than prepared.
		name:  "direct-owned-preflight",
		want:  "while a direct-owned seat is live the preflight refuses with the direct-owned count, and once it ends the preflight returns not_required",
		local: true, requiresAdoption: false,
		driver: "TestDirectOwnedPreflight",
	},
	{
		// The execution-security scope, not the seat's transient scope that
		// scope-creation-refused is about.
		name:  "unrenderable-execution-scope-refused",
		want:  "a session stamping an execution-security level the harness cannot render is refused before spawn with the typed unrenderable reason and no harness process starts",
		local: true, requiresAdoption: false,
		driver: "TestUnrenderableExecutionScopeRefused",
	},
	{
		name:  "receiver-exact-replay",
		want:  "an exact terminal-status replay returns the original receipt with its revision unchanged and a changed body is a conflict",
		local: true, requiresAdoption: false,
		driver: "TestReceiverExactReplay",
	},
	{
		name:  "receiver-worker-rotation",
		want:  "re-registration rotates the worker id, the old bearer lapses, and the lease refresh succeeds with the rotated pair",
		local: true, requiresAdoption: false,
		driver: "TestReceiverWorkerRotation",
	},
	{
		name:  "receiver-lease-across-gap",
		want:  "lease refresh keeps succeeding across the restart gap and step heartbeats are recorded without releasing the session",
		local: true, requiresAdoption: false,
		driver: "TestReceiverLeaseAcrossGap",
	},
	{
		name:  "harness-resume-reports-history",
		want:  "the resumed fake harness reports the full transcript history it loaded from the session-owned state dir",
		local: true, requiresAdoption: false,
		driver: "TestFakeHarnessResumeReportsHistory",
	},
}

// matrixLiveNames returns the stable names of the matrix rows that need
// the container. The record probe hands the same set to the container
// driver, so a live case cannot silently drop out of the record.
func matrixLiveNames() []string {
	var out []string
	for _, c := range failureMatrix {
		if !c.local {
			out = append(out, c.name)
		}
	}
	return out
}
