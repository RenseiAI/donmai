package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
)

// newTestDaemonForSweep builds a daemon the way every other daemon_test.go
// helper does, with opts merged in. Both existing seam tests and the new
// default-construction test share this shape so the only difference between
// them is whether Options.CodexOrphanSweeper is set.
func newTestDaemonForSweep(t *testing.T, opts Options) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	opts.ConfigPath = tmp + "/daemon.yaml"
	opts.JWTPath = tmp + "/daemon.jwt"
	opts.SkipWizard = true
	opts.SkipRegistration = true
	opts.SpawnerOptions = SpawnerOptions{WorkerCommand: []string{"/bin/true"}}
	d := New(opts)
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

// TestDaemonStartRunsCodexOrphanSweep pins the missing-caller half of the
// fix: daemon Start actually invokes the configured orphan sweeper, so
// crash/SIGKILL/restart leftovers are reclaimed instead of accumulating
// forever. One invocation per default root (see
// TestDaemonSweepCodexOrphansCoversEveryDefaultRoot) — the count is
// asserted there; here Start must produce at least one. The fake sweeper
// is supplied through Options.CodexOrphanSweeper — the same seam the
// production entry point configures with the real providercodex.SweepOrphans
// — so this test never touches a real temp dir or signals a real process.
func TestDaemonStartRunsCodexOrphanSweep(t *testing.T) {
	calls := 0
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(context.Context, providercodex.SweepOptions) providercodex.SweepReport {
			calls++
			return providercodex.SweepReport{}
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls < 1 {
		t.Fatalf("sweep calls = %d, want at least 1 per Start", calls)
	}
}

// TestDaemonStartSweepFailureNeverFailsStartup pins the degrade-don't-destroy
// contract at the call site: even if the configured sweeper errors on every
// entry, Start still succeeds — a best-effort disk reclamation must never
// take down the daemon.
func TestDaemonStartSweepFailureNeverFailsStartup(t *testing.T) {
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(context.Context, providercodex.SweepOptions) providercodex.SweepReport {
			return providercodex.SweepReport{Errors: 500}
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start with a failing sweep: %v", err)
	}
}

// TestDaemonStartWithoutConfiguredSweeperNeverTouchesATempCodexRoot is the
// revert proof for the seam itself: every one of the roughly two dozen other
// daemon_test.go tests that call Start()/h.start() builds a Daemon the way
// newTestDaemonForSweep does here — WITHOUT ever mentioning
// Options.CodexOrphanSweeper — and must stay exactly that oblivious. This
// test proves the default (nil) construction's Start never reads from, nor
// deletes inside, a real codex-home directory: it redirects os.TempDir()
// (via TMPDIR, exactly what providercodex.SweepOrphans' own zero-value Root
// resolves through) at a directory planted with a fixture that the real
// sweep — with nothing but defaults, the production call shape — WOULD
// reclaim: a donmai-codex-home-* directory, owned 0700, carrying no owner
// manifest and aged well past the sweep's 24h UnverifiedMinAge default.
//
// Revert proof: if the nil-check in sweepCodexOrphans (or the Options field
// it reads) is removed so Start falls back to calling the real sweep
// unconditionally, this fixture satisfies every gate sweepOne checks on the
// no-manifest path (owned, in-fence, idle past UnverifiedMinAge) and gets
// deleted — os.Stat(canary) below starts reporting IsNotExist and
// `go test -run TestDaemonStartWithoutConfiguredSweeperNeverTouchesATempCodexRoot ./daemon/` fails.
func TestDaemonStartWithoutConfiguredSweeperNeverTouchesATempCodexRoot(t *testing.T) {
	fakeTemp := t.TempDir()
	t.Setenv("TMPDIR", fakeTemp)

	canary := filepath.Join(fakeTemp, "donmai-codex-home-canary")
	if err := os.Mkdir(canary, 0o700); err != nil {
		t.Fatalf("create canary codex-home directory: %v", err)
	}
	stale := time.Now().Add(-25 * time.Hour) // past the sweep's 24h UnverifiedMinAge default.
	if err := os.Chtimes(canary, stale, stale); err != nil {
		t.Fatalf("age canary codex-home directory: %v", err)
	}

	d := newTestDaemonForSweep(t, Options{}) // CodexOrphanSweeper deliberately left unset.
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("canary codex-home directory was touched by Start with no configured sweeper: %v", err)
	}
}

// TestDaemonSweepCodexOrphansCoversEveryDefaultRoot pins the multi-root fan
// out: one sweep pass calls the configured sweeper once per default root,
// each with that root set, so persistent-Codex-home orphans are swept —
// not just system-temp ones. The fixture HOME makes temp and persistent
// roots distinct (in this package's TestMain isolation the temp home sits
// under TMPDIR, which would otherwise collapse the two). The fake records
// the roots it was asked to sweep; the assertion is on that record, not
// on the merged report.
//
// RED proof: replace the fan-out loop with a single sweep call over the
// first deduped default root and this test fails — the second root is
// never presented to the sweeper.
func TestDaemonSweepCodexOrphansCoversEveryDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	want := providercodex.DedupeCodexHomeRoots(providercodex.DefaultCodexHomeRoots())
	if len(want) < 2 {
		t.Skipf("fixture HOME %q collapses the default roots to %q", home, want)
	}
	var roots []string
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(_ context.Context, opts providercodex.SweepOptions) providercodex.SweepReport {
			roots = append(roots, opts.Root)
			return providercodex.SweepReport{}
		},
	})
	_ = d.sweepCodexOrphans(context.Background())
	if len(roots) != len(want) {
		t.Fatalf("sweep roots = %q, want one call per default root %q", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("sweep roots = %q, want %q in order", roots, want)
		}
	}
}

// TestDaemonSweepCodexOrphansMergesPerRootReports pins the merged-report
// contract: per-root counts add, so the pass log line and any future
// metric consumer see the whole pass, not just the last root.
func TestDaemonSweepCodexOrphansMergesPerRootReports(t *testing.T) {
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(_ context.Context, _ providercodex.SweepOptions) providercodex.SweepReport {
			return providercodex.SweepReport{Scanned: 3, Reclaimed: 1, PartiallyReclaimed: 1, Acted: 1, Errors: 1}
		},
	})
	report := d.sweepCodexOrphans(context.Background())
	roots := len(providercodex.DedupeCodexHomeRoots(providercodex.DefaultCodexHomeRoots()))
	if report.Scanned != 3*roots || report.Reclaimed != roots || report.Errors != roots {
		t.Fatalf("merged report = %+v, want each per-root count times %d roots", report, roots)
	}
}

// TestDaemonSweepCodexOrphansProtectsLiveAdoptionOnEveryRoot pins that the
// adoption-protection set computed once per pass reaches every root's call:
// a home referenced by a live adopted session must be presented as
// protected on each root, not just the first.
func TestDaemonSweepCodexOrphansProtectsLiveAdoptionOnEveryRoot(t *testing.T) {
	var protected []map[string]struct{}
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(_ context.Context, opts providercodex.SweepOptions) providercodex.SweepReport {
			protected = append(protected, opts.ProtectedHomes)
			return providercodex.SweepReport{}
		},
	})
	_ = d.sweepCodexOrphans(context.Background())
	roots := len(providercodex.DedupeCodexHomeRoots(providercodex.DefaultCodexHomeRoots()))
	if len(protected) != roots {
		t.Fatalf("sweeper calls = %d, want one per deduped default root (%d)", len(protected), roots)
	}
	first := protected[0]
	for i, got := range protected[1:] {
		if len(got) != len(first) {
			t.Fatalf("call %d protected set differs from call 0: %v vs %v", i+1, got, first)
		}
		for home := range first {
			if _, ok := got[home]; !ok {
				t.Fatalf("call %d missing protected home %s present on call 0", i+1, home)
			}
		}
	}
}

// TestDaemonSweepCodexOrphansSkipsAnUnresolvablePersistentRoot pins the
// never-materialise rule at the fan-out: when the persistent root does not
// resolve, the pass still sweeps system temp exactly once and never asks
// the sweeper to look at an empty root.
func TestDaemonSweepCodexOrphansSkipsAnUnresolvablePersistentRoot(t *testing.T) {
	// CODEX_HOME pointed at a path that cannot resolve, and a HOME that
	// cannot resolve either, leave the fallback root empty — but this
	// package's TestMain isolation already pointed HOME at a temp dir,
	// so override both explicitly for this test.
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing", "codex"))
	var roots []string
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(_ context.Context, opts providercodex.SweepOptions) providercodex.SweepReport {
			roots = append(roots, opts.Root)
			return providercodex.SweepReport{}
		},
	})
	_ = d.sweepCodexOrphans(context.Background())
	for _, root := range roots {
		if root == "" {
			t.Fatalf("sweeper was called with an empty root: %q", roots)
		}
	}
	if len(roots) == 0 {
		t.Fatal("sweeper was never called")
	}
}

// TestDaemonCodexOrphanSweepLoopTicksAndStopsWithContext pins the periodic
// half: the loop re-runs a full multi-root pass on every tick, and returns
// once its context ends with no further passes. The production interval is
// an hour; the loop body takes the interval as a parameter so this runs in
// milliseconds.
//
// RED proof: make runCodexOrphanSweepLoop return without ticking and the
// pass count never grows; drop the ctx.Done case and the loop never returns.
func TestDaemonCodexOrphanSweepLoopTicksAndStopsWithContext(t *testing.T) {
	var calls atomic.Int64
	d := newTestDaemonForSweep(t, Options{
		CodexOrphanSweeper: func(_ context.Context, _ providercodex.SweepOptions) providercodex.SweepReport {
			calls.Add(1)
			return providercodex.SweepReport{}
		},
	})
	roots := int64(len(providercodex.DedupeCodexHomeRoots(providercodex.DefaultCodexHomeRoots())))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runCodexOrphanSweepLoop(ctx, 5*time.Millisecond)
	}()

	deadline := time.After(10 * time.Second)
	for calls.Load() < 2*roots {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("sweeper calls = %d after 10s, want at least two passes over %d roots", calls.Load(), roots)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sweep loop did not return after its context was cancelled")
	}
	settled := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != settled {
		t.Fatalf("sweeper calls grew from %d to %d after the loop returned", settled, got)
	}
}
