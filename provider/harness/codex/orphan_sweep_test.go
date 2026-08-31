package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeDeadPID returns a PID that is provably not alive: it starts and
// fully reaps a real short-lived child process, then hands back its PID.
// More honest than a hardcoded large integer, which could — in principle —
// collide with something actually running.
func fakeDeadPID(t *testing.T) int {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", "exit", "0")
	} else {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short-lived fixture: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived fixture: %v", err)
	}
	return cmd.Process.Pid
}

// ageEntry backdates path's mtime by age relative to asOf.
func ageEntry(t *testing.T, path string, asOf time.Time, age time.Duration) {
	t.Helper()
	stamp := asOf.Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// spawnSweepFixtureProcess starts a short-lived real child process (`sleep`,
// present on every unix test runner) so tests can exercise the sweep's
// liveness checks against a genuinely running PID rather than a mocked one.
// The caller is responsible for waiting on or otherwise reaping it.
func spawnSweepFixtureProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only fixture (uses /bin/sleep); the sweep's own process-identity gate is unix-only too")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestSweepOrphans_IgnoresAnythingOutsideTheDonmaiNamingFence pins the hard
// safety fence: an entry under Root that does NOT start with
// codexHomePrefix or codexAppSocketPrefix — ambient user Codex state, or
// literally anything else — is never scanned, reclaimed, or even Lstat'd
// for age. Reusing ~/.codex's own real shape (a directory literally named
// ".codex") as the fixture makes the point concretely: this must survive
// completely untouched.
func TestSweepOrphans_IgnoresAnythingOutsideTheDonmaiNamingFence(t *testing.T) {
	root := t.TempDir()
	ambient := filepath.Join(root, ".codex")
	if err := os.Mkdir(ambient, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(ambient, old, old); err != nil {
		t.Fatal(err)
	}

	report := SweepOrphans(context.Background(), SweepOptions{Root: root})
	if report.Scanned != 0 {
		t.Fatalf("Scanned = %d, want 0 — an ambient, non-donmai-named entry was examined at all", report.Scanned)
	}
	if _, err := os.Stat(ambient); err != nil {
		t.Fatalf("ambient Codex state was touched: %v", err)
	}
}

// TestSweepOrphans_SkipsYoungEntriesRegardlessOfManifest pins the
// unconditional age gate: an entry newer than MinAge is skipped even when
// it has no manifest at all (which, absent the age gate, this
// implementation would otherwise reclaim on sight) — a session must never
// race the sweep just because it started a moment before the daemon did.
func TestSweepOrphans_SkipsYoungEntriesRegardlessOfManifest(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"young")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.SkippedLive != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want one skipped-live and zero reclaimed", report)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("young entry was removed: %v", err)
	}
}

// TestSweepOrphans_ReclaimsOldEntryWithNoManifest pins the fallback path:
// an old, donmai-named directory with no manifest at all (a pre-upgrade
// leftover, or a failed manifest write) is still reclaimed on age alone —
// the age gate having already run is what makes this safe.
func TestSweepOrphans_ReclaimsOldEntryWithNoManifest(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"noage")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageEntry(t, home, now, 2*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want Reclaimed=1", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("orphaned entry survived the sweep: err=%v", err)
	}
}

// TestSweepOrphans_NeverTouchesADirectoryWhoseOwnerIsStillAlive pins the
// core ownership gate: even a very old directory is left completely alone
// when its manifest names a still-running owner PID — that owner's own
// in-memory Handle/boundary may yet clean it up itself, and only IT is
// allowed to.
//
// RED proof: in sweepOne, delete the `if opts.processAlive(manifest.OwnerPID)`
// branch (falling through to the dead-owner reclaim path unconditionally)
// and this test fails — the live-owned directory is reclaimed out from under
// its owner. Verified: FAILED ("live-owned entry was reclaimed"), then
// PASSED again after restoring — see the completion report.
func TestSweepOrphans_NeverTouchesADirectoryWhoseOwnerIsStillAlive(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"live-owner")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDonmaiOwnerManifest(home, "codex-home") // records THIS test process's own PID.
	// Backdate mtime AFTER writing the manifest: writing the manifest file
	// itself touches the directory's mtime, exactly as it would in
	// production (the manifest is written once, right at creation).
	ageEntry(t, home, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.SkippedLive != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want one skipped-live and zero reclaimed", report)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("live-owned entry was reclaimed: %v", err)
	}
}

// TestSweepOrphans_ReclaimsDirectoryWhoseOwnerIsGone pins the dominant
// production case (the 4591-directory leak this deliverable exists to
// close): the owning donmai process is gone (a PID this test never spawned,
// picked from a range Go test binaries do not reuse mid-run), so nothing
// will ever call remove() for this directory again — the sweep must
// reclaim it.
func TestSweepOrphans_ReclaimsDirectoryWhoseOwnerIsGone(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"dead-owner")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDonmaiOwnerManifestManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want Reclaimed=1", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("dead-owner entry survived the sweep: err=%v", err)
	}
}

// TestSweepOrphans_TerminatesAndReclaimsWhenChildLooksLikeTheConfiguredBinary
// pins the "~20 orphaned live app-server processes" case end to end: the
// owning donmai process is gone, but it started a real child (the sleep
// fixture, matched here via BinaryHint="sleep" standing in for "codex") that
// is still alive. The sweep must terminate it AND reclaim its socket
// directory.
func TestSweepOrphans_TerminatesAndReclaimsWhenChildLooksLikeTheConfiguredBinary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"live-child")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := spawnSweepFixtureProcess(t)
	writeDonmaiOwnerManifestManifest(dir, donmaiOwnerManifest{
		OwnerPID: fakeDeadPID(t), ChildPID: child.Process.Pid, StartedAt: time.Now(),
	})
	ageEntry(t, dir, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, BinaryHint: "sleep",
		TerminationGrace: 200 * time.Millisecond, // keep the suite fast; production default is 5s.
	})
	if report.Terminated != 1 || report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want Terminated=1 Reclaimed=1", report)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("socket directory survived the sweep: err=%v", err)
	}
	// This test process is child's real OS parent, so a signal-0 liveness
	// probe alone cannot distinguish "terminated" from "terminated but not
	// yet reaped" (a zombie still answers signal 0). Wait() blocks until the
	// kernel actually reaps it, which only happens once it has genuinely
	// exited — proof the sweep's SIGTERM/SIGKILL escalation really worked,
	// not an artifact of this test being the parent (which a fresh daemon
	// process, in production, never is for an orphan it did not spawn).
	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("child process was not actually terminated")
	}
}

// TestSweepOrphans_SkipsAmbiguousPIDReuseWithoutTouchingProcessOrDirectory
// pins the third gate: the manifest's child PID is alive, but that live
// process does not look like the configured binary at all (BinaryHint
// deliberately does not match "sleep") — the sweep must prove this is
// almost certainly PID reuse and leave BOTH the process and its directory
// alone, never guessing.
//
// RED proof: in sweepOne, delete the `if !opts.processLooksLikeCodex(...)`
// branch (falling through to unconditional termination once a PID is merely
// alive) and this test fails — an unrelated live process gets SIGTERM'd and
// its directory removed. Verified: FAILED (process was killed / directory
// removed), then PASSED again after restoring — see the completion report.
func TestSweepOrphans_SkipsAmbiguousPIDReuseWithoutTouchingProcessOrDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"ambiguous")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := spawnSweepFixtureProcess(t)
	writeDonmaiOwnerManifestManifest(dir, donmaiOwnerManifest{
		OwnerPID: fakeDeadPID(t), ChildPID: child.Process.Pid, StartedAt: time.Now(),
	})
	ageEntry(t, dir, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, BinaryHint: "definitely-not-this-process",
	})
	if report.SkippedAmbiguous != 1 || report.Terminated != 0 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want one skipped-ambiguous and nothing else", report)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("ambiguous entry's directory was removed: %v", err)
	}
	if !processAliveOS(child.Process.Pid) {
		t.Fatal("ambiguous entry's live process was killed")
	}
}

// TestSweepOrphans_BoundsWorkPerCall pins the bounded-work requirement: with
// MaxEntries set below the number of donmai-named orphans present, the
// sweep must stop early rather than examine every one of them.
func TestSweepOrphans_BoundsWorkPerCall(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for i := 0; i < 5; i++ {
		home := filepath.Join(root, codexHomePrefix+string(rune('a'+i)))
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		ageEntry(t, home, now, 2*time.Hour)
	}

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour, MaxEntries: 2})
	if report.Scanned > 2 {
		t.Fatalf("Scanned = %d, want <= MaxEntries (2)", report.Scanned)
	}
}

// TestSweepOrphans_StopsOnContextCancellation pins the other bounded-work
// knob: a caller (daemon startup with its own timeout) can cut a sweep
// short via ctx, and it must stop examining further entries rather than
// ignore cancellation.
func TestSweepOrphans_StopsOnContextCancellation(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for i := 0; i < 5; i++ {
		home := filepath.Join(root, codexHomePrefix+string(rune('a'+i)))
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		ageEntry(t, home, now, 2*time.Hour)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := SweepOrphans(ctx, SweepOptions{Root: root, MinAge: time.Hour})
	if report.Scanned != 0 {
		t.Fatalf("Scanned = %d, want 0 with an already-cancelled ctx", report.Scanned)
	}
}

// TestReadDonmaiOwnerManifest_MissingOrMalformedIsNotOK pins the fallback
// contract every caller in sweepOne relies on: a missing file, and a file
// that fails to parse as the expected shape, are indistinguishable to the
// caller (both report ok=false) — sweepOne then falls back to pure age,
// exactly as if no manifest had ever been written.
func TestReadDonmaiOwnerManifest_MissingOrMalformedIsNotOK(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readDonmaiOwnerManifest(dir); ok {
		t.Fatal("expected ok=false for a directory with no manifest at all")
	}
	if err := os.WriteFile(filepath.Join(dir, donmaiOwnerManifestName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDonmaiOwnerManifest(dir); ok {
		t.Fatal("expected ok=false for a malformed manifest")
	}
}

// TestUpdateDonmaiOwnerManifestChildPID_PreservesOwnerPID pins the update
// path startLocked and startNamedInteractiveAppServer both rely on: adding
// a child PID once the codex subprocess actually starts must not disturb
// the owner PID recorded at directory-creation time.
func TestUpdateDonmaiOwnerManifestChildPID_PreservesOwnerPID(t *testing.T) {
	dir := t.TempDir()
	writeDonmaiOwnerManifest(dir, "codex-home")
	before, ok := readDonmaiOwnerManifest(dir)
	if !ok {
		t.Fatal("expected a manifest immediately after writeDonmaiOwnerManifest")
	}
	updateDonmaiOwnerManifestChildPID(dir, 424242)
	after, ok := readDonmaiOwnerManifest(dir)
	if !ok {
		t.Fatal("expected a manifest after updateDonmaiOwnerManifestChildPID")
	}
	if after.OwnerPID != before.OwnerPID {
		t.Fatalf("OwnerPID changed from %d to %d", before.OwnerPID, after.OwnerPID)
	}
	if after.ChildPID != 424242 {
		t.Fatalf("ChildPID = %d, want 424242", after.ChildPID)
	}
}
