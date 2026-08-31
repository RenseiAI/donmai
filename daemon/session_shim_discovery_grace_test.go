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
// These tests pin the fix directly against awaitShimRecord rather than the
// full spawn-a-real-process launch path: the behavior under test is a pure
// function of (ctx deadline, registry contents, launch identity), and driving
// it through a real subprocess would only add flakiness without adding
// coverage.

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
// the given process epoch/pid, encodable and satisfying Record.Validate.
func validShimDiscoveryRecord(dir string, id sessionshim.Identity, processEpoch uint64, pid int, shimID string) sessionshim.Record {
	return sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: processEpoch,
		PID: pid, ProcessStartedAt: time.Now().UnixNano(),
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
	const pid = 4242
	rec := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch, pid, "shim-late")

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
	got, err := awaitShimRecord(ctx, registry, id, launch, pid)
	if putErr := <-errCh; putErr != nil {
		t.Fatalf("Put late record: %v", putErr)
	}
	if err != nil {
		t.Fatalf("awaitShimRecord did not adopt the late-arriving matching record: %v", err)
	}
	if got.ShimID != rec.ShimID || got.PID != pid || got.ProcessEpoch != launch.ProcessEpoch {
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
	if _, err := awaitShimRecord(ctx, registry, id, launch, 4242); err == nil {
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
	const wantPID = 4242

	// A record for the SAME identity, but a different process epoch — a stale
	// or unrelated incarnation, not a slow arrival of this exact launch.
	stale := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch+1, wantPID+1, "shim-stale")
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		errCh <- registry.Put(stale)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = awaitShimRecord(ctx, registry, id, launch, wantPID)
	if putErr := <-errCh; putErr != nil {
		t.Fatalf("Put stale record: %v", putErr)
	}
	if err == nil {
		t.Fatal("awaitShimRecord adopted a record that did not identity-match this launch")
	}
}

// TestShimDiscoveryRecordMatchesLaunch pins the identity-match predicate
// directly: same org/session, same process epoch, same PID.
func TestShimDiscoveryRecordMatchesLaunch(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org", SessionID: "sess"}
	launch := sessionshim.Launch{Identity: id, ProcessEpoch: 3}
	base := sessionshim.Record{OrgID: id.OrgID, SessionID: id.SessionID, ProcessEpoch: 3, PID: 99}

	if !shimDiscoveryRecordMatchesLaunch(base, id, launch, 99) {
		t.Fatal("an exact identity/epoch/pid match was refused")
	}
	wrongEpoch := base
	wrongEpoch.ProcessEpoch = 4
	if shimDiscoveryRecordMatchesLaunch(wrongEpoch, id, launch, 99) {
		t.Fatal("a mismatched process epoch was accepted")
	}
	wrongPID := base
	wrongPID.PID = 100
	if shimDiscoveryRecordMatchesLaunch(wrongPID, id, launch, 99) {
		t.Fatal("a mismatched pid was accepted")
	}
	wrongSession := base
	wrongSession.SessionID = "other"
	if shimDiscoveryRecordMatchesLaunch(wrongSession, id, launch, 99) {
		t.Fatal("a mismatched session id was accepted")
	}
}
