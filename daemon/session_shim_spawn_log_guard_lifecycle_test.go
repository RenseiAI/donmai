package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestFailedShimLaunchKeepsTheChildLogAndStopsTheGuard is the inverted F1.
//
// It USED to assert the opposite — that a launch which never reaches
// trackLaunchedShim leaves no log file behind — and that assertion pinned the
// defect: the one artifact that explains a spawn-failed seat was deleted at
// the exact moment it became the answer, so the failure was only ever
// diagnosable by racing a copy loop against the daemon.
//
// The two properties the original test was really protecting are unchanged and
// still asserted here: the LIVE digest-named path is gone (so nothing
// accumulates under the name a future launch would reuse), and the guard
// goroutine self-terminates instead of ticking for the rest of the daemon's
// life. What changed is where the bytes went: to a `.failed` sibling, and out
// on the launch error itself.
func TestFailedShimLaunchKeepsTheChildLogAndStopsTheGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registryDir := dir + "/registry"
	const sessionID = "sess-failed-launch-keeps-log"
	// Short, space-separated words on purpose: shimChildLogSecretPatterns
	// carries a deliberately broad catch-all for any 32+ character opaque run,
	// so a long hyphenated marker would be masked as a suspected credential —
	// which is correct behaviour, and is pinned by
	// TestPreservedShimChildLogTailIsRedacted below.
	const childOutput = "the child said why it died"

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     registryDir,
			LaunchTimeout:   750 * time.Millisecond,
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that writes some output and exits immediately without
		// ever publishing a shim discovery record — awaitShimRecord times
		// out and launchSessionShim returns an error without ever calling
		// trackLaunchedShim. This is the shape the live stack fails in.
		WorkerCommand:     []string{"/bin/sh", "-c", "echo " + childOutput + "; exit 0"},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	// Plant an expired failure log from an imaginary earlier session. The
	// retention sweep is wired into the LAUNCH path (a preserved log outlives
	// the launch that produced it, so nothing in that session's own lifecycle
	// can reclaim it) — asserting it here is what pins the wiring rather than
	// only the function.
	stale := shimFailedChildLogPath(registryDir, sessionshim.Identity{OrgID: "test-org", SessionID: "an-earlier-failure"})
	if err := os.MkdirAll(registryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("older than the window\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-shimFailedChildLogRetention - time.Hour)
	if err := os.Chtimes(stale, expired, expired); err != nil {
		t.Fatal(err)
	}

	_, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: sessionID, ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	})
	if err == nil {
		t.Fatal("AcceptWork = nil error; a shim launch that never announced itself must fail the accept (see TestLaunchFailureFailsTheAcceptClosed)")
	}

	// The whole point: the reason reaches a caller who cannot read this
	// host's filesystem. Without the tail on the error, all the operator
	// ever sees is "never published a discovery record".
	if !strings.Contains(err.Error(), childOutput) {
		t.Errorf("accept error does not carry the child's own output:\n%v", err)
	}
	if !strings.Contains(err.Error(), "child log") {
		t.Errorf("accept error does not say where the retained log is:\n%v", err)
	}

	// launchSessionShim's cleanup defer runs synchronously as part of its
	// own return, before AcceptWork above ever returns to this test — no
	// polling or sleeping needed to observe it.
	id := sessionshim.Identity{OrgID: "test-org", SessionID: sessionID}
	logPath := shimChildLogPath(registryDir, id)
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Fatalf("the live log path %s still exists; the failed log must be renamed to its .failed sibling", logPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stat'ing %s: %v", logPath, statErr)
	}

	failedPath := shimFailedChildLogPath(registryDir, id)
	kept, readErr := os.ReadFile(failedPath) //nolint:gosec // test-owned temp path
	if readErr != nil {
		t.Fatalf("the failed launch's child log was not kept at %s: %v", failedPath, readErr)
	}
	if !strings.Contains(string(kept), childOutput) {
		t.Fatalf("retained log does not hold the child's output:\n%s", kept)
	}

	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Errorf("the launch did not sweep an expired failure log (stat err %v); retained logs would accumulate forever", statErr)
	}

	// Unchanged from the original F1 assertion: the guard goroutine started
	// in launchSessionShim self-terminates the FIRST time it finds the live
	// path gone (guardShimChildLogOnce returns false). A rename satisfies
	// that exactly as a removal did.
	if guardShimChildLogOnce(logPath) {
		t.Fatal("guardShimChildLogOnce = true after a failed launch; the guard goroutine started in launchSessionShim would keep ticking for the rest of the daemon's life")
	}
}

// TestStartupAdoptionStartsTheLogGuardForEachReAdoptedShim pins F2: a daemon
// restart that re-adopts a still-live shim (adoptSessionShims) must also
// resume the redaction+cap guard for it — without this, a shim that
// outlives its launching daemon (the exact case startShimProcess's own doc
// comment cites as the reason this log is captured to a plain file at all)
// silently stops having its log redacted and capped the moment the
// original daemon that launched it goes away.
//
// This drives a REAL launch through a REAL re-exec worker
// (newShimSpawnFixture — the same harness TestRestartFenceRetainsTheAdoptionResumeCursor
// uses). The launch itself already starts its OWN guard (launchSessionShim,
// F1) — a real daemon restart would kill that goroutine along with the
// whole old process, but THIS test's "restart" is just a second in-process
// *Daemon object in the same test binary, so that original goroutine is
// still very much alive and would silently mask a broken F2 (it would keep
// guarding the file regardless of what adoptSessionShims does). This test
// neutralizes it deliberately, in order: remove the log file and wait past
// shimChildLogGuardInterval so the launch's own guard observes the file is
// gone and self-terminates (exactly TestFailedShimLaunchKeepsTheChildLogAndStopsTheGuard's
// mechanism), THEN recreate a fresh file at the same digest-named path —
// simulating "a new capture file exists post-restart" — before ever
// touching adoption. Only then does re-adoption run; only a guard STARTED
// BY adoptSessionShims can be the one that later redacts an injected
// secret in that fresh file.
func TestStartupAdoptionStartsTheLogGuardForEachReAdoptedShim(t *testing.T) {
	f := newShimSpawnFixture(t)
	first := f.daemon

	spec := f.interactiveSpec("sess-guard-resume")
	if _, err := first.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	logPath := shimChildLogPath(f.registry, id)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file missing after launch: %v", err)
	}

	// Neutralize the launch's own guard (F1) so it cannot mask a broken F2.
	// removeShimChildLog only removes the FILE; the goroutine itself is
	// opaque to this test (no cancellation channel, by design — see
	// runShimChildLogGuard's doc comment) and only notices on its OWN next
	// tick, so the only reliable way to know it has actually observed the
	// removal and returned is to wait at least one full
	// shimChildLogGuardInterval of real wall-clock time.
	removeShimChildLog(f.registry, id)
	time.Sleep(shimChildLogGuardInterval + 500*time.Millisecond)
	if err := os.WriteFile(logPath, []byte("post-restart capture begins\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first.ReleaseAdoptedSessionShims()

	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			OrgID:          f.orgID,
			RegistryDir:    f.registry,
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoptSessionShims: %v", err)
	}
	if len(replacement.AdoptedSessionShims()) == 0 {
		t.Fatal("replacement did not re-adopt the live shim launched above; test setup problem")
	}

	const secret = "sk-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	appendFD, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFD.Write([]byte("token=" + secret + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = appendFD.Close()

	waitFor(t, 5*time.Second, "the re-adopted session's log guard to redact the injected secret", func() bool {
		content, readErr := os.ReadFile(logPath)
		if readErr != nil {
			return false
		}
		return strings.Contains(string(content), "token=") && !strings.Contains(string(content), secret)
	})
}
