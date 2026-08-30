package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestFailedShimLaunchLeavesNoLogFileOrGuardGoroutine pins F1: a launch that
// never reaches trackLaunchedShim (never entering d.shims.adopted) must not
// leak its digest-named log file or leave runShimChildLogGuard ticking for
// the rest of the daemon's life. launchSessionShim's launchAdopted defer
// runs removeShimChildLog on every such early-return path — this drives the
// exact same "launch never announced itself" shape
// TestShimChildStdoutStderrLandInThePerSessionLogFile drives (proving
// capture works), but this test's concern is the opposite: that nothing
// survives once the accept has definitively failed.
func TestFailedShimLaunchLeavesNoLogFileOrGuardGoroutine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registryDir := dir + "/registry"
	const sessionID = "sess-failed-launch-no-leak"

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
		// trackLaunchedShim.
		WorkerCommand:     []string{"/bin/sh", "-c", "echo some-output; exit 0"},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	if _, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: sessionID, ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	}); err == nil {
		t.Fatal("AcceptWork = nil error; a shim launch that never announced itself must fail the accept (see TestLaunchFailureFailsTheAcceptClosed)")
	}

	// launchSessionShim's cleanup defer runs synchronously as part of its
	// own return, before AcceptWork above ever returns to this test — no
	// polling or sleeping needed to observe it.
	logPath := shimChildLogPath(registryDir, sessionshim.Identity{OrgID: "test-org", SessionID: sessionID})
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("log file %s survived a failed launch; launchSessionShim's launchAdopted defer must remove it", logPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error stat'ing %s: %v", logPath, err)
	}

	// The guard goroutine started in launchSessionShim self-terminates the
	// FIRST time it finds the file gone (guardShimChildLogOnce returns
	// false) — proving that directly, rather than sleeping past
	// shimChildLogGuardInterval and hoping the real goroutine already
	// ticked, is what actually pins "no ticking goroutine": if the file is
	// gone, EVERY goroutine racing to check it — including the one this
	// launch actually started — observes the same false and returns.
	if guardShimChildLogOnce(logPath) {
		t.Fatal("guardShimChildLogOnce = true after a failed launch removed the log; the guard goroutine started in launchSessionShim would keep ticking for the rest of the daemon's life")
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
// gone and self-terminates (exactly TestFailedShimLaunchLeavesNoLogFileOrGuardGoroutine's
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
