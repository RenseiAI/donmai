package daemon

// Provenance: shim-launch-discovery-grace-2026-08-31 — grep a build for this
// marker to prove it carries the post-deadline discovery re-read.
//
// THE STRAND THIS UNDOES
//
// sessionshim.Start deliberately spawns the harness under a PTY BEFORE
// publishing its discovery record, so defaultShimLaunchTimeout's budget has to
// cover the harness's entire cold start — and some harnesses' first run also
// does network-bound work that can occasionally push discovery a hair past
// even a generous budget. Measured live: the daemon gave up exactly at the
// deadline, and the record appeared on disk under the exact expected filename
// for the exact expected identity moments later, with the shim process still
// alive. Pre-fix, that launch failed forever: nothing was adopted, nothing was
// counted, and the still-running shim held a session open with no daemon ever
// willing to claim it — capacity consumed for a session that, from the
// daemon's point of view, did not exist.
//
// TWO LAYERS OF COVERAGE, DELIBERATELY
//
// The awaitShimRecord/shimDiscoveryRecordMatchesLaunch tests below pin the
// grace poll's own shape (bounded, backing off, identity-checked) as a pure
// function of (ctx deadline, registry contents, launch identity). That is
// necessary but was NOT sufficient: a first version of this fix adopted the
// late record correctly and then handed launchSessionShim's Dial call the
// SAME already-expired ctx the grace poll had just outlived, turning "never
// published a discovery record" into "could not adopt the shim it just
// launched" one statement later — a bug these pure-function tests could not
// see because they never touch the calling path. That is why
// TestLaunchSessionShimAdoptsThroughTheRealPathWhenDiscoveryArrivesLate below
// drives the real launch path (a real spawned worker process, a real
// on-disk registry, a real Dial) far enough to prove the daemon hands
// discovery's late finish a LIVE context, not just that awaitShimRecord
// itself returns a record.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// withShortShimDiscoveryGrace shrinks the post-deadline grace poll's wall-clock
// cost for the duration of one test, restoring the production defaults on
// cleanup. The behavior under test is the SHAPE of the grace window (bounded,
// backing off, identity-checked), not its production-sized duration.
func withShortShimDiscoveryGrace(t *testing.T, attempts int, base time.Duration) {
	t.Helper()
	origAttempts, origBase := shimRecordDiscoveryGraceAttempts, shimRecordDiscoveryGraceBaseDelay
	shimRecordDiscoveryGraceAttempts = attempts
	shimRecordDiscoveryGraceBaseDelay = base
	t.Cleanup(func() {
		shimRecordDiscoveryGraceAttempts = origAttempts
		shimRecordDiscoveryGraceBaseDelay = origBase
	})
}

// validShimDiscoveryRecord builds a minimally-valid discovery record for id at
// the given process epoch/pid/start-time, encodable and satisfying
// Record.Validate.
func validShimDiscoveryRecord(dir string, id sessionshim.Identity, processEpoch uint64, started sessionshim.ProcessIdentity, shimID string) sessionshim.Record {
	startedAt := started.StartedAt
	if startedAt == 0 {
		startedAt = time.Now().UnixNano()
	}
	return sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: processEpoch,
		PID: started.PID, ProcessStartedAt: startedAt,
		SocketPath:        filepath.Join(dir, shimID+".sock"),
		ProtocolMin:       1,
		ProtocolMax:       1,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
}

// TestAwaitShimRecordAdoptsALateArrivingMatchingRecord is the measured strand,
// undone: a discovery record that lands after the launch context's own
// deadline, but that identity-matches the exact launch awaitShimRecord was
// called for, is adopted rather than treated as a launch that never happened.
func TestAwaitShimRecordAdoptsALateArrivingMatchingRecord(t *testing.T) {
	withShortShimDiscoveryGrace(t, 5, 20*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-late", SessionID: "session-late"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
	started := sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111}
	rec := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch, started, "shim-late")

	// Publish the record only after awaitShimRecord's own ctx has already
	// expired — the exact condition measured live: the daemon's launch clock
	// ran out moments before the shim actually published.
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		errCh <- registry.Put(rec)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	got, err := awaitShimRecord(ctx, shimDiscoveryWait{registry: registry, id: id, launch: launch, started: started})
	if putErr := <-errCh; putErr != nil {
		t.Fatalf("Put late record: %v", putErr)
	}
	if err != nil {
		t.Fatalf("awaitShimRecord did not adopt the late-arriving matching record: %v", err)
	}
	if got.ShimID != rec.ShimID || got.PID != started.PID || got.ProcessEpoch != launch.ProcessEpoch {
		t.Fatalf("awaitShimRecord returned %+v, want the late record %+v", got, rec)
	}
}

// TestAwaitShimRecordStillFailsWhenNoRecordEverAppears is the negative that
// must keep holding: the launch timeout's own bound, plus the bounded grace
// poll, both expire, and a launch that genuinely produced nothing still fails
// — the give-up path §D10 requires stays intact.
func TestAwaitShimRecordStillFailsWhenNoRecordEverAppears(t *testing.T) {
	withShortShimDiscoveryGrace(t, 3, 5*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-never", SessionID: "session-never"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := awaitShimRecord(ctx, shimDiscoveryWait{
		registry: registry, id: id, launch: launch,
		started: sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111},
	}); err == nil {
		t.Fatal("awaitShimRecord succeeded for a launch that never published a record")
	}
}

// TestAwaitShimRecordRefusesALateRecordFromADifferentIncarnation guards the
// other side of the fix: a record that appears after the deadline but belongs
// to a DIFFERENT process epoch/pid than this exact launch is not this launch's
// record, and adopting it would be exactly the guess §D10 forbids. The grace
// window must still fail this launch.
func TestAwaitShimRecordRefusesALateRecordFromADifferentIncarnation(t *testing.T) {
	withShortShimDiscoveryGrace(t, 5, 10*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-mismatch", SessionID: "session-mismatch"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
	wantStarted := sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111}

	// A record for the SAME identity, but a different process epoch — a stale
	// or unrelated incarnation, not a slow arrival of this exact launch.
	stale := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch+1,
		sessionshim.ProcessIdentity{PID: wantStarted.PID + 1, StartedAt: wantStarted.StartedAt + 1}, "shim-stale")
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		errCh <- registry.Put(stale)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = awaitShimRecord(ctx, shimDiscoveryWait{registry: registry, id: id, launch: launch, started: wantStarted})
	if putErr := <-errCh; putErr != nil {
		t.Fatalf("Put stale record: %v", putErr)
	}
	if err == nil {
		t.Fatal("awaitShimRecord adopted a record that did not identity-match this launch")
	}
}

// TestAwaitShimRecordRefusesAReusedPIDWithADifferentStartTime is the
// production-realistic mismatch: launch.ProcessEpoch is hardcoded to 1 for
// every ordinary launch (launchSessionShim), so a same-epoch, same-PID record
// with a DIFFERENT OS-reported start time — the ordinary shape of PID reuse —
// is the case §D2/§D10 actually need guarded against, not a differing
// process epoch production never produces.
func TestAwaitShimRecordRefusesAReusedPIDWithADifferentStartTime(t *testing.T) {
	withShortShimDiscoveryGrace(t, 5, 10*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-pid-reuse", SessionID: "session-pid-reuse"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
	wantStarted := sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111}

	// Same identity, same process epoch (1, as every real launch uses), same
	// PID — but a different start time: the OS reused wantStarted.PID for an
	// unrelated process between this launch starting and the grace poll
	// running.
	reused := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch,
		sessionshim.ProcessIdentity{PID: wantStarted.PID, StartedAt: wantStarted.StartedAt + 1}, "shim-reused-pid")
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		errCh <- registry.Put(reused)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = awaitShimRecord(ctx, shimDiscoveryWait{registry: registry, id: id, launch: launch, started: wantStarted})
	if putErr := <-errCh; putErr != nil {
		t.Fatalf("Put reused-pid record: %v", putErr)
	}
	if err == nil {
		t.Fatal("awaitShimRecord adopted a same-PID record with a different OS-reported start time")
	}
}

// TestShimDiscoveryRecordMatchesLaunch pins the identity-match predicate
// directly: same org/session, same process epoch, same PID, and — when this
// launch could pin its own start time — the same OS-reported start time.
func TestShimDiscoveryRecordMatchesLaunch(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org", SessionID: "sess"}
	launch := sessionshim.Launch{Identity: id, ProcessEpoch: 3}
	started := sessionshim.ProcessIdentity{PID: 99, StartedAt: 555}
	base := sessionshim.Record{OrgID: id.OrgID, SessionID: id.SessionID, ProcessEpoch: 3, PID: 99, ProcessStartedAt: 555}

	if !shimDiscoveryRecordMatchesLaunch(base, id, launch, started) {
		t.Fatal("an exact identity/epoch/pid/startedAt match was refused")
	}
	wrongEpoch := base
	wrongEpoch.ProcessEpoch = 4
	if shimDiscoveryRecordMatchesLaunch(wrongEpoch, id, launch, started) {
		t.Fatal("a mismatched process epoch was accepted")
	}
	wrongPID := base
	wrongPID.PID = 100
	if shimDiscoveryRecordMatchesLaunch(wrongPID, id, launch, started) {
		t.Fatal("a mismatched pid was accepted")
	}
	wrongStart := base
	wrongStart.ProcessStartedAt = 556
	if shimDiscoveryRecordMatchesLaunch(wrongStart, id, launch, started) {
		t.Fatal("a same-pid, different-start-time record was accepted")
	}
	wrongSession := base
	wrongSession.SessionID = "other"
	if shimDiscoveryRecordMatchesLaunch(wrongSession, id, launch, started) {
		t.Fatal("a mismatched session id was accepted")
	}
	// A launch whose own start time could not be pinned (startShimProcess logs
	// a warning and proceeds rather than failing an otherwise-successful
	// spawn) must FAIL CLOSED here, never fall back to a bare-PID match:
	// launch.ProcessEpoch is a hardcoded constant in production, so
	// PID+StartedAt are the only two real discriminators this check has, and
	// this repo's own ProcessIdentity doc calls a bare-PID comparison unsafe
	// because PID reuse is ordinary.
	unpinnedLaunchStart := sessionshim.ProcessIdentity{PID: 99}
	if shimDiscoveryRecordMatchesLaunch(base, id, launch, unpinnedLaunchStart) {
		t.Fatal("an unpinned launch start time was accepted instead of refused")
	}
	// THE DISCRIMINATING CASE for the fail-closed guard: base.ProcessStartedAt
	// (555) already differs from unpinnedLaunchStart.StartedAt (0) above, so
	// that check alone passes even WITHOUT the explicit
	// "if started.StartedAt == 0 { return false }" guard — the ordinary
	// equality comparison already rejects it, for an unrelated reason. The
	// ONLY shape that actually exercises the guard is a record whose OWN
	// reported start time is ALSO zero: without the guard, 0 == 0 and every
	// other field already matches, so the equality check alone would accept
	// it. Deleting the guard turns this one case RED while leaving every
	// other case above GREEN.
	recordWithNoStartTime := base
	recordWithNoStartTime.ProcessStartedAt = 0
	if shimDiscoveryRecordMatchesLaunch(recordWithNoStartTime, id, launch, unpinnedLaunchStart) {
		t.Fatal("a record with no recorded start time matched an unpinned launch on 0 == 0 instead of being refused")
	}
}

// TestLaunchSessionShimAdoptsThroughTheRealPathWhenDiscoveryArrivesLate is the
// end-to-end proof the pure-function tests above cannot give: it drives a
// REAL launch (a real spawned worker process publishing a real discovery
// record under a real on-disk registry) through d.spawner.AcceptWork with the
// launch timeout deliberately shorter than the worker's own configured start
// delay, and asserts the session ends up DURABLY ADOPTED — not merely that a
// record was returned.
//
// This is the test that would have caught the ctx-reuse bug the
// pure-function tests above could not see: a first version of this fix
// adopted the late record but then handed sessionshim.Dial the SAME
// already-expired ctx awaitShimRecord had just outlived, so Dial's own
// DialTimeout was moot (context.WithTimeout never outlives an expired
// parent) and the dial failed immediately — converting "never published a
// discovery record" into "could not adopt the shim it just launched" one
// statement later. Only a test that reaches Dial on the real return path
// exercises that ctx at all.
func TestLaunchSessionShimAdoptsThroughTheRealPathWhenDiscoveryArrivesLate(t *testing.T) {
	withShortShimDiscoveryGrace(t, 8, 100*time.Millisecond)
	f := newShimSpawnFixture(t)
	// Shorter than the worker's own start delay below, so discovery can only
	// land through the post-deadline grace path — never within the ordinary
	// wait.
	f.daemon.opts.SessionShim.LaunchTimeout = 150 * time.Millisecond
	// TEST-ONLY: the spawned worker (runDaemonShimHelper) sleeps this long
	// before calling sessionshim.StartFromEnv, reproducing a harness cold
	// start that lands its discovery record after the launch deadline. See
	// envDaemonShimHelperStartDelayMS's doc comment — production
	// sessionshim.Start has no such delay and never reads this variable.
	f.daemon.spawner.opts.BaseEnv[envDaemonShimHelperStartDelayMS] = "500"

	spec := f.interactiveSpec("late-discovery-real-launch")
	handle, err := f.daemon.spawner.AcceptWork(spec)
	if err != nil {
		t.Fatalf("AcceptWork with discovery delayed past the launch deadline: %v", err)
	}
	if handle == nil || handle.SessionID != spec.SessionID || handle.State != SessionRunning {
		t.Fatalf("AcceptWork returned %+v, want a running handle for %s", handle, spec.SessionID)
	}
	if _, err := f.daemon.adoptedShimEntry(f.orgID, spec.SessionID); err != nil {
		t.Fatalf("session was not durably adopted after a late-arriving discovery record: %v", err)
	}
}
