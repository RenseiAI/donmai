package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
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

// fakeDeadIdentity pins a real, correctly-formed sessionshim.ProcessIdentity
// for a short-lived fixture process WHILE it is still running, then waits
// for it to exit. The returned identity's own .Alive() correctly reports
// false afterward — this is a genuinely dead, genuinely pinned identity,
// not a fabricated one, unix/linux+darwin only (sessionshim.ProcessIdentityFor
// has no portable implementation elsewhere — see sessionshim/procid_other.go).
func fakeDeadIdentity(t *testing.T) sessionshim.ProcessIdentity {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("sessionshim.ProcessIdentityFor has no implementation on this platform")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short-lived fixture: %v", err)
	}
	identity, err := sessionshim.ProcessIdentityFor(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("pin short-lived fixture identity: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived fixture: %v", err)
	}
	return identity
}

// requireManifestVerification skips a test that depends on the sweep being
// able to PROVE it owns an artifact directory. That proof is fd-based and
// unix-only; windows has no ACL implementation, so every manifest is treated
// as unverifiable there on purpose (see orphan_sweep_windows.go).
func requireManifestVerification(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("owner-manifest ownership verification is unimplemented on windows; manifests are never trusted there")
	}
}

// ownedFixtureDir creates a 0700 directory the sweep's ownership proof can
// pass. t.TempDir() itself is not usable for this: it inherits the runner's
// umask and is commonly 0755, which is exactly what that proof rejects.
func ownedFixtureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
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
	reapEagerlyAndOnce(cmd)
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// reapEagerlyAndOnce starts collecting cmd's exit CONCURRENTLY, the moment
// it happens, rather than whenever a test gets around to calling Wait.
// This test process is the fixture's real OS parent — nothing else will
// ever reap it — and a signalled-but-unreaped zombie still answers both a
// bare signal-0 probe and sessionshim's identity check as "alive". Without
// this, SweepOrphans's own post-signal liveness re-probes (see terminate's
// F8 fix) could never observe death within any grace window, since nothing
// would be collecting the exit while the sweep polls.
func reapEagerlyAndOnce(cmd *exec.Cmd) {
	go func() { _ = cmd.Wait() }()
}

// spawnSweepFixtureProcessNamedCodexSource is a trivial, dependency-free
// sleeper. Compiling it fresh (rather than copying a signed system binary
// like /bin/sleep to a new name) matters on macOS specifically: a bit-for-bit
// copy of a system binary to a new path is killed by the kernel's code-
// signing enforcement almost immediately (verified directly against this
// exact approach before switching to a compiled fixture) — a freshly built
// binary carries no such conflicting signature.
const spawnSweepFixtureProcessNamedCodexSource = `package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	seconds := 30
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil {
			seconds = n
		}
	}
	time.Sleep(time.Duration(seconds) * time.Second)
}
`

// buildSweepFixtureBinary compiles the trivial sleeper above to a binary
// named exactly name, so `ps -o comm=` reports name rather than "sleep" or
// the test binary's own name. Returns its path; a caller that needs several
// concurrent fixture processes builds once and starts many.
func buildSweepFixtureBinary(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only fixture; the sweep's own process-identity gate is unix-only too")
	}
	binDir := t.TempDir()
	srcPath := filepath.Join(binDir, "fixture.go")
	if err := os.WriteFile(srcPath, []byte(spawnSweepFixtureProcessNamedCodexSource), 0o600); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
	namedBin := filepath.Join(binDir, name)
	build := exec.Command("go", "build", "-o", namedBin, srcPath) //nolint:gosec // G204: fixed argv; srcPath/namedBin are this test's own temp paths.
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile named fixture binary: %v\n%s", err, out)
	}
	return namedBin
}

// startSweepFixtureBinary starts bin with the given duration argument in
// seconds and deliberately does NOT reap it: this test process is its real
// OS parent, so once the fixture exits (on its own or because the sweep
// signalled it) it stays a zombie — a process-table entry the kernel still
// answers start-time queries for, which is exactly what makes
// sessionshim's identity liveness check report a dead process as alive.
// Cleanup collects the exit so the zombie does not outlive the test.
func startSweepFixtureBinary(t *testing.T, bin, seconds string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, seconds) //nolint:gosec // G204: bin is a fixture this test just built.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start named fixture process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// spawnSweepFixtureProcessNamed builds and starts one fixture process whose
// exit is collected concurrently (see reapEagerlyAndOnce), so a sweep's
// post-signal liveness re-probe can observe a genuine exit.
func spawnSweepFixtureProcessNamed(t *testing.T, name string, seconds string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(buildSweepFixtureBinary(t, name), seconds) //nolint:gosec // G204: the binary is a fixture this test just built.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start named fixture process: %v", err)
	}
	reapEagerlyAndOnce(cmd)
	t.Cleanup(func() { _ = cmd.Process.Kill() })
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
	if report.SkippedYoung != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want one skipped-young and zero reclaimed", report)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("young entry was removed: %v", err)
	}
}

// TestSweepOrphans_ReclaimsOldEntryWithNoManifest pins the fallback path:
// a donmai-named directory with no manifest at all (a pre-upgrade leftover,
// or a failed manifest write) is reclaimed once it clears the SEPARATE,
// larger UnverifiedMinAge floor — never MinAge alone (see F3/
// UnverifiedMinAge's doc comment: a live session's own mtime is not a safe
// signal once writes have moved into subdirectories).
func TestSweepOrphans_ReclaimsOldEntryWithNoManifest(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"noage")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ageEntry(t, home, now, 2*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour, UnverifiedMinAge: time.Hour})
	if report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want Reclaimed=1", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("orphaned entry survived the sweep: err=%v", err)
	}
}

// TestSweepOrphans_ManifestLessLiveSessionSurvives is the reviewer's F3
// probe: a donmai-named directory with NO manifest — exactly what a
// pre-upgrade rollout's still-running session, or a failed best-effort
// manifest write, looks like — must survive a sweep whose UnverifiedMinAge
// it has not yet cleared, even though it is well past the ordinary MinAge.
// A live session's top-level CODEX_HOME mtime stops moving minutes in
// (writes land in subdirectories), so MinAge alone proves nothing about
// whether this is still in active use.
//
// RED proof: in sweepOne, replace the `opts.reclaimUnverified(path, kind, info, report)`
// call for the no-manifest branch with an unconditional `opts.reclaim(path, kind, report)`
// and this test fails — the manifest-less directory (standing in for a live
// session) is deleted out from under it. Verified: FAILED ("manifest-less
// entry was reclaimed while still within its unverified grace window"), then
// PASSED again after restoring — see the completion report.
func TestSweepOrphans_ManifestLessLiveSessionSurvives(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"live-no-manifest")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Well past MinAge (an hour), nowhere near the default 24h
	// UnverifiedMinAge — exactly the gap a live session's own idle mtime can
	// sit in.
	ageEntry(t, home, now, 2*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want Reclaimed=0 — manifest-less entry was reclaimed while still within its unverified grace window", report)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("manifest-less live-session stand-in was removed: %v", err)
	}
}

// TestSweepOrphans_ForeignlyWritableDirectoryIsTreatedAsUnverified is the
// reviewer's F2 probe: a directory this process does not exclusively own —
// here, made group/other-writable, the same half of
// verifyManifestDirectoryOwnershipOS a cross-uid attacker would also fail,
// and the only half a single-user CI runner can exercise without a second
// real system account — must have its manifest ignored entirely, exactly
// like a directory with no manifest. A world-writable os.TempDir() (ordinary
// unix /tmp) makes an unverified manifest an unprivileged local kill
// primitive otherwise: any user could plant one naming a PID they want
// signalled.
//
// RED proof: in readDonmaiOwnerManifest, delete the
// `if err := verifyManifestDirectoryOwnership(info); err != nil { ... return donmaiOwnerManifest{}, false }`
// guard (falling through to trust readDonmaiOwnerManifestUnchecked
// unconditionally) and this test fails — the foreign-writable manifest is
// trusted, and a live child recorded in it gets terminated. Verified: FAILED
// (process was killed / directory removed), then PASSED again after
// restoring — see the completion report.
func TestSweepOrphans_ForeignlyWritableDirectoryIsTreatedAsUnverified(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"foreign")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := spawnSweepFixtureProcess(t)
	persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{
		OwnerPID:      fakeDeadPID(t),
		ChildIdentity: mustProcessIdentity(t, child.Process.Pid),
		StartedAt:     time.Now(),
	})
	// Group/other-writable: the exact condition verifyManifestDirectoryOwnershipOS
	// rejects, alongside a mismatched uid (unreachable from a single-user
	// test runner without a second real account).
	if err := os.Chmod(dir, 0o707); err != nil { //nolint:gosec // G302: the test deliberately makes this world-writable to exercise the ownership-rejection path.
		t.Fatal(err)
	}
	ageEntry(t, dir, time.Now(), 2*time.Hour) // past MinAge, nowhere near UnverifiedMinAge.

	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, BinaryHint: "sleep",
	})
	if report.Terminated != 0 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want nothing terminated or reclaimed — the manifest must be treated as unverified", report)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("foreign-writable directory was removed: %v", err)
	}
	if !processAliveOS(child.Process.Pid) {
		t.Fatal("foreign-writable manifest's recorded child was signalled")
	}
}

// TestSweepOrphans_NeverTouchesADirectoryWhoseOwnerIsStillAlive pins the
// core ownership gate: even a very old directory is left completely alone
// when its manifest names a still-running owner — that owner's own
// in-memory Handle/boundary may yet clean it up itself, and only IT is
// allowed to.
func TestSweepOrphans_NeverTouchesADirectoryWhoseOwnerIsStillAlive(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"live-owner")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDonmaiOwnerManifest(home, sweepKindCodexHome) // records THIS test process's own identity.
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
// production leak this deliverable exists to close: the owning donmai
// process is gone, so nothing will ever call remove() for this directory
// again — the sweep must reclaim it.
func TestSweepOrphans_ReclaimsDirectoryWhoseOwnerIsGone(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"dead-owner")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	persistDonmaiOwnerManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want Reclaimed=1", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("dead-owner entry survived the sweep: err=%v", err)
	}
}

// mustProcessIdentity pins pid's real, live identity or fails the test.
func mustProcessIdentity(t *testing.T, pid int) sessionshim.ProcessIdentity {
	t.Helper()
	identity, err := sessionshim.ProcessIdentityFor(pid)
	if err != nil {
		t.Fatalf("pin process identity for pid %d: %v", pid, err)
	}
	return identity
}

// TestSweepOrphans_TerminatesAndReclaimsWhenIdentityAndBinaryMatch pins the
// full happy path for the "orphaned live app-server process" case end to
// end: the owning donmai process is gone, but it started a real child (the
// sleep fixture, matched here via BinaryHint="sleep" standing in for
// "codex") whose VERIFIED, LIVE identity is still recorded and still
// matches. The sweep must terminate it AND reclaim its socket directory.
func TestSweepOrphans_TerminatesAndReclaimsWhenIdentityAndBinaryMatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"live-child")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := spawnSweepFixtureProcess(t)
	persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{
		OwnerPID:      fakeDeadPID(t),
		ChildIdentity: mustProcessIdentity(t, child.Process.Pid),
		StartedAt:     time.Now(),
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
	// terminate() itself only ever reports Terminated++ after re-probing
	// liveness post-signal (the F8 fix) — reapEagerlyAndOnce (in
	// spawnSweepFixtureProcess) is what makes that re-probe able to observe
	// a genuine exit at all, rather than a signalled-but-unreaped zombie.
	if processAliveOS(child.Process.Pid) {
		t.Fatal("child process was reported terminated but is still alive")
	}
}

// TestSweepOrphans_NeverTerminatesOnPIDReuseEvenWhenBinaryNameMatches is the
// reviewer's F1 counterexample, reproduced and proven fixed: a REAL,
// independently-running process is deliberately named "codex" (so the
// binary-identity gate alone would have passed it), its manifest carries a
// deliberately WRONG start time — exactly what a manifest written for an
// EARLIER process now reused by this unrelated one would look like — and
// production zero-value SweepOptions (except Root) are used, matching
// exactly how a real daemon start would call this. The sweep must never
// signal it.
func TestSweepOrphans_NeverTerminatesOnPIDReuseEvenWhenBinaryNameMatches(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"pid-reuse")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := spawnSweepFixtureProcessNamed(t, "codex", "30")
	actual := mustProcessIdentity(t, unrelated.Process.Pid)
	stale := actual
	stale.StartedAt++ // a plausible-looking but WRONG start time: PID reuse, from the manifest's point of view.
	persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{
		OwnerPID:      fakeDeadPID(t),
		ChildIdentity: stale,
		StartedAt:     time.Now(),
	})
	ageEntry(t, dir, time.Now(), 25*time.Hour) // past even the production UnverifiedMinAge default.

	report := SweepOrphans(context.Background(), SweepOptions{Root: root})
	if report.Terminated != 0 {
		t.Fatalf("report = %+v, want Terminated=0 — a stale identity must never be signalled even when its binary name matches", report)
	}
	if alive, err := actual.Alive(); err != nil || !alive {
		t.Fatalf("the unrelated, independently-running process was touched: alive=%v err=%v", alive, err)
	}
}

// TestSweepOrphans_SkipsAmbiguousPIDReuseWithoutTouchingProcessOrDirectory
// pins the binary-identity gate: the manifest's child identity is live and
// IS the exact incarnation recorded, but that live process does not look
// like the configured binary at all (BinaryHint deliberately does not match
// "sleep") — the sweep must leave BOTH the process and its directory alone,
// never guessing.
func TestSweepOrphans_SkipsAmbiguousPIDReuseWithoutTouchingProcessOrDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, codexAppSocketPrefix+"ambiguous")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := spawnSweepFixtureProcess(t)
	persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{
		OwnerPID:      fakeDeadPID(t),
		ChildIdentity: mustProcessIdentity(t, child.Process.Pid),
		StartedAt:     time.Now(),
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

// TestReadVerifiedDonmaiOwnerManifest_MissingOrMalformedIsNotOK pins the
// fallback contract every caller in sweepOne relies on: a missing file, and
// a file that fails to parse as the expected shape, are indistinguishable to
// the caller (both report ok=false) — sweepOne then treats the directory as
// undeclared, exactly as if no manifest had ever been written.
func TestReadVerifiedDonmaiOwnerManifest_MissingOrMalformedIsNotOK(t *testing.T) {
	requireManifestVerification(t)
	dir := ownedFixtureDir(t)
	opts := SweepOptions{}.withDefaults()
	if _, ok := opts.readVerifiedDonmaiOwnerManifest(dir); ok {
		t.Fatal("expected ok=false for a directory with no manifest at all")
	}
	if err := os.WriteFile(filepath.Join(dir, donmaiOwnerManifestName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := opts.readVerifiedDonmaiOwnerManifest(dir); ok {
		t.Fatal("expected ok=false for a malformed manifest")
	}
}

// TestReadVerifiedDonmaiOwnerManifest_RefusesAWorldWritableManifestFile pins
// the half of the ownership proof that is about the FILE rather than the
// directory: a manifest whose own mode grants group or other write is not
// something this process can prove only it wrote, so it is refused even
// inside a directory whose mode is impeccable.
func TestReadVerifiedDonmaiOwnerManifest_RefusesAWorldWritableManifestFile(t *testing.T) {
	requireManifestVerification(t)
	dir := ownedFixtureDir(t)
	persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	if _, ok := (SweepOptions{}).withDefaults().readVerifiedDonmaiOwnerManifest(dir); !ok {
		t.Fatal("expected a freshly written 0600 manifest to verify")
	}
	if err := os.Chmod(filepath.Join(dir, donmaiOwnerManifestName), 0o666); err != nil { //nolint:gosec // G302: the test deliberately widens the mode to exercise the rejection path.
		t.Fatal(err)
	}
	if _, ok := (SweepOptions{}).withDefaults().readVerifiedDonmaiOwnerManifest(dir); ok {
		t.Fatal("a group/other-writable manifest file was trusted")
	}
}

// TestPinDonmaiChildIdentity_PreservesOwnerIdentity pins the update path
// startLocked and startNamedInteractiveAppServer both rely on: recording a
// child's identity once the codex subprocess actually starts must not
// disturb the owner identity recorded at directory-creation time.
func TestPinDonmaiChildIdentity_PreservesOwnerIdentity(t *testing.T) {
	dir := t.TempDir()
	writeDonmaiOwnerManifest(dir, "codex-home")
	before, ok := readDonmaiOwnerManifestUnchecked(dir)
	if !ok {
		t.Fatal("expected a manifest immediately after writeDonmaiOwnerManifest")
	}
	child := fakeDeadIdentity(t)
	pinDonmaiChildIdentity(dir, child.PID) // child has already exited — pinning must be a harmless no-op.
	afterExited, ok := readDonmaiOwnerManifestUnchecked(dir)
	if !ok {
		t.Fatal("expected a manifest to remain after a failed pin attempt")
	}
	if afterExited.ChildIdentity.PID != 0 {
		t.Fatalf("ChildIdentity = %+v, want zero value — pinning an already-exited pid must not fabricate an identity", afterExited.ChildIdentity)
	}

	live := spawnSweepFixtureProcess(t)
	pinDonmaiChildIdentity(dir, live.Process.Pid)
	after, ok := readDonmaiOwnerManifestUnchecked(dir)
	if !ok {
		t.Fatal("expected a manifest after pinDonmaiChildIdentity")
	}
	if after.OwnerIdentity != before.OwnerIdentity || after.OwnerPID != before.OwnerPID {
		t.Fatalf("owner identity changed: before=%+v/%d after=%+v/%d", before.OwnerIdentity, before.OwnerPID, after.OwnerIdentity, after.OwnerPID)
	}
	if after.ChildIdentity.PID != live.Process.Pid {
		t.Fatalf("ChildIdentity.PID = %d, want %d", after.ChildIdentity.PID, live.Process.Pid)
	}
}

// TestSweepOrphans_PreservesResumableSessionStateWhileReclaimingScratch is
// a probe for the resume-safety constraint: a dead-owner, dead-child
// "codex-home" directory that still holds a rollout file under sessions/
// must NEVER be fully deleted. The one declared-deletable entry a resume
// provably does not need — the plugin-cache staging copy — is removed;
// sessions/, config.toml and the auth link all survive, and the result is
// reported as PartiallyReclaimed rather than Reclaimed.
//
// RED proof: in declaredDeletableEntries, drop the `if hasSessionState`
// early return so a codex-home always declares config.toml, the auth link
// and the manifest deletable, and this test fails — the resume keys beside
// the rollout file are destroyed.
func TestSweepOrphans_PreservesResumableSessionStateWhileReclaimingScratch(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"resumable")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutDir := filepath.Join(home, "sessions", "2026", "08", "31")
	if err := os.MkdirAll(rolloutDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const rolloutBody = `{"type":"session_meta"}`
	rolloutPath := filepath.Join(rolloutDir, "rollout-2026-08-31T00-00-00-thread-live.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(rolloutBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("mcp_servers = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistDonmaiOwnerManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.PartiallyReclaimed != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want PartiallyReclaimed=1 Reclaimed=0", report)
	}
	body, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatalf("resumable session state was deleted: %v", err)
	}
	if string(body) != rolloutBody {
		t.Fatalf("rollout file content changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(home, codexConfigFileName)); err != nil {
		t.Fatalf("config.toml — a resume key — was destroyed beside the session state it belongs to: %v", err)
	}
}

// TestSweepOrphans_FullyReclaimsWhenSessionsDirIsEmptyOrAbsent pins the
// other half: a "codex-home" directory with no sessions/ subdirectory at
// all (never named/turned) — or one that exists but is empty — is fully
// removed exactly as before this constraint existed. Preservation is
// conditioned on there being something to preserve.
func TestSweepOrphans_FullyReclaimsWhenSessionsDirIsEmptyOrAbsent(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"never-named")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("mcp_servers = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistDonmaiOwnerManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 24*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})
	if report.Reclaimed != 1 || report.PartiallyReclaimed != 0 {
		t.Fatalf("report = %+v, want Reclaimed=1 PartiallyReclaimed=0", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("directory with no session state survived: err=%v", err)
	}
}

// --- Review-round-2 probes: B1 (never read from a foreign directory),
// --- B2 (delete only what a manifest declares deletable), A1 (bounded
// --- total wall clock). Each was written to FAIL against the code as it
// --- stood before the corresponding fix.

// TestSweepOrphans_NeverHarvestsFromADirectoryItDoesNotOwn pins the rule
// "read nothing from a directory you have declared foreign".
//
// os.TempDir() is a shared, world-writable directory on an ordinary unix
// host, so any local user can mkdir a donmai-named directory there and fill
// its cache/ subtree with whatever they like. Refusing to TRUST such a
// directory's manifest is not enough: harvesting its cache/ subtree copies
// the planted file into the host-level warm cache, where reuseCacheTree's
// never-overwrite rule makes it permanent and every future session's
// CODEX_HOME is seeded from it. The payload is the remote plugin/apps
// catalog — the data that drives a session's advertised tool surface.
func TestSweepOrphans_NeverHarvestsFromADirectoryItDoesNotOwn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: directory ownership verification is unix-only (see orphan_sweep_windows.go)")
	}
	root := t.TempDir()
	hostCache := t.TempDir()
	foreign := filepath.Join(root, codexHomePrefix+"attacker")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	const poison = `{"plugins":[{"name":"attacker-supplied"}]}`
	writeCacheFixture(t, foreign, filepath.Join(codexPluginCacheSubdir, "remote_plugin_catalog", "poison.json"), poison)
	persistDonmaiOwnerManifest(foreign, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	// World-writable: the exact condition verifyManifestDirectoryOwnershipOS
	// rejects, and the only half of it a single-account test runner can
	// exercise without a second real system user.
	if err := os.Chmod(foreign, 0o777); err != nil { //nolint:gosec // G302: the test deliberately makes this world-writable to exercise the ownership-rejection path.
		t.Fatal(err)
	}
	ageEntry(t, foreign, time.Now(), 48*time.Hour) // past every floor the sweep has.

	SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, UnverifiedMinAge: time.Hour, PluginCacheDir: hostCache,
	})

	if _, err := os.Lstat(filepath.Join(hostCache, "remote_plugin_catalog", "poison.json")); !os.IsNotExist(err) {
		t.Fatalf("the sweep harvested from a directory it had already declared foreign: err=%v", err)
	}
	entries, err := os.ReadDir(hostCache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("host plugin cache gained entries from an unowned directory: %v", entries)
	}
}

// TestSweepOrphans_PreservesRestartAdoptionKeysAlongsideSessionState pins
// the governing principle: process death is the PRECONDITION for resume, so
// it cannot also be the trigger for deleting what resume needs. After a
// daemon restart the previous daemon is dead — which is exactly the
// manifest's "owner dead" condition — so a sweep that treats "owner dead"
// as licence to strip a home down to its rollout file destroys the
// config.toml and auth link the resumed session needs, while carefully
// preserving the half it could rebuild.
func TestSweepOrphans_PreservesRestartAdoptionKeysAlongsideSessionState(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"restart-adoptable")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutDir := filepath.Join(home, codexSessionStateSubdir, "2026", "08", "31")
	if err := os.MkdirAll(rolloutDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(rolloutDir, "rollout-2026-08-31T00-00-00-thread-live.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const configBody = "mcp_servers = {}\n"
	if err := os.WriteFile(filepath.Join(home, codexConfigFileName), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, codexAuthFileName), []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache/ subtree IS re-derivable and IS declared deletable — it is the
	// one thing this directory should lose.
	writeCacheFixture(t, home, filepath.Join(codexPluginCacheSubdir, "remote_plugin_catalog", "abc123.json"), "warm")
	persistDonmaiOwnerManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 48*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, UnverifiedMinAge: time.Hour, PluginCacheDir: t.TempDir(),
	})

	if report.PartiallyReclaimed != 1 || report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want PartiallyReclaimed=1 Reclaimed=0", report)
	}
	body, err := os.ReadFile(filepath.Join(home, codexConfigFileName))
	if err != nil {
		t.Fatalf("config.toml — a restart-adoption key — was destroyed: %v", err)
	}
	if string(body) != configBody {
		t.Fatalf("config.toml content changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(home, codexAuthFileName)); err != nil {
		t.Fatalf("the auth link — a restart-adoption key — was destroyed: %v", err)
	}
	if _, err := os.Stat(rolloutPath); err != nil {
		t.Fatalf("resumable session state was destroyed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, codexPluginCacheSubdir)); !os.IsNotExist(err) {
		t.Fatalf("the re-derivable cache subtree survived: err=%v", err)
	}
}

// TestSweepOrphans_NeverDeletesStateNoManifestDeclares pins the other half
// of the inversion: a preserve-list is a denylist — it deletes every
// top-level entry except the names it happens to know, so any codex state
// this package has not been taught about is deleted by discovery. A sweep
// that discovers deletable state by walking the filesystem cannot know
// whether it is deleting a resume key.
func TestSweepOrphans_NeverDeletesStateNoManifestDeclares(t *testing.T) {
	requireManifestVerification(t)
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"undeclared-state")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Real codex state this package never declares it created. No sessions/
	// subdirectory at all, so the sweep's own preserve-list does not protect
	// any of it.
	archived := filepath.Join(home, "archived_sessions", "2026")
	if err := os.MkdirAll(archived, 0o700); err != nil {
		t.Fatal(err)
	}
	archivedFile := filepath.Join(archived, "rollout-archived.jsonl")
	if err := os.WriteFile(archivedFile, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(home, "history.jsonl")
	if err := os.WriteFile(historyPath, []byte(`{"prompt":"remember this"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, codexConfigFileName), []byte("mcp_servers = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistDonmaiOwnerManifest(home, donmaiOwnerManifest{OwnerPID: fakeDeadPID(t), StartedAt: time.Now()})
	ageEntry(t, home, time.Now(), 48*time.Hour)

	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, UnverifiedMinAge: time.Hour, PluginCacheDir: t.TempDir(),
	})

	if report.Reclaimed != 0 || report.PartiallyReclaimed != 1 {
		t.Fatalf("report = %+v, want PartiallyReclaimed=1 Reclaimed=0", report)
	}
	if _, err := os.Stat(archivedFile); err != nil {
		t.Fatalf("archived_sessions/ — state no manifest declares deletable — was deleted by discovery: %v", err)
	}
	if _, err := os.Stat(historyPath); err != nil {
		t.Fatalf("history.jsonl — state no manifest declares deletable — was deleted by discovery: %v", err)
	}
	// The declared entries still go: this is a narrowing, not a disabling.
	if _, err := os.Lstat(filepath.Join(home, codexConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("a declared-deletable entry survived: err=%v", err)
	}
}

// TestSweepOrphans_ManifestLessSessionNeverLosesItsConfig pins the raised
// floor for the shape every pre-upgrade session has and every failed
// best-effort manifest write produces: with no manifest there is nothing
// declaring ANY of this directory's contents deletable, so a manifest-less
// home keeps its config.toml and auth link no matter how old it looks —
// including a LIVE session whose top-level mtime stopped moving the moment
// its writes went into subdirectories.
func TestSweepOrphans_ManifestLessSessionNeverLosesItsConfig(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, codexHomePrefix+"manifest-less-live")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, codexConfigFileName), []byte("mcp_servers = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, codexAuthFileName), []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, home, time.Now(), 30*24*time.Hour) // a month old by mtime, and still live.

	report := SweepOrphans(context.Background(), SweepOptions{Root: root, MinAge: time.Hour})

	if report.Reclaimed != 0 {
		t.Fatalf("report = %+v, want Reclaimed=0", report)
	}
	if _, err := os.Stat(filepath.Join(home, codexConfigFileName)); err != nil {
		t.Fatalf("a manifest-less session lost its config.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, codexAuthFileName)); err != nil {
		t.Fatalf("a manifest-less session lost its auth link: %v", err)
	}
}

// TestSweepOrphans_BoundsTotalWallClockNotJustEntryCount pins the other
// bounded-work promise on a path documented as "run once, early, on daemon
// start". MaxEntries bounds how MANY entries are examined; it says nothing
// about how LONG each one takes, and an entry that reaches termination
// costs up to 2 x TerminationGrace.
//
// The fixtures here reproduce the real trigger rather than simulating it:
// each is a genuine live process named "codex" that this test process
// starts and deliberately never reaps, so when the sweep signals it, it
// becomes a zombie — a process-table entry the kernel still answers start-
// time queries for, which is what makes sessionshim's identity liveness
// check report a dead process as alive. awaitDeath can then never confirm
// death and burns the entire grace window, twice, per entry.
func TestSweepOrphans_BoundsTotalWallClockNotJustEntryCount(t *testing.T) {
	const (
		entries = 8
		grace   = 500 * time.Millisecond
	)
	bin := buildSweepFixtureBinary(t, "codex")
	root := t.TempDir()
	for i := range entries {
		dir := filepath.Join(root, codexAppSocketPrefix+"unreaped-"+strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		child := startSweepFixtureBinary(t, bin, "60")
		persistDonmaiOwnerManifest(dir, donmaiOwnerManifest{
			OwnerPID:      fakeDeadPID(t),
			ChildIdentity: mustProcessIdentity(t, child.Process.Pid),
			StartedAt:     time.Now(),
		})
		ageEntry(t, dir, time.Now(), 48*time.Hour)
	}

	start := time.Now()
	report := SweepOrphans(context.Background(), SweepOptions{
		Root: root, MinAge: time.Hour, UnverifiedMinAge: time.Hour,
		BinaryHint: "codex", TerminationGrace: grace, MaxDuration: 600 * time.Millisecond,
	})
	elapsed := time.Since(start)

	// Unbounded shape: entries * 2 * grace = 8s. Bounded shape: one entry's
	// grace window at most, then the budget check stops the sweep.
	if elapsed > 3*time.Second {
		t.Fatalf("sweep took %s for %d entries at a %s grace (report %+v) — total wall clock is bounded only by entry count, not by time",
			elapsed, entries, grace, report)
	}
}
