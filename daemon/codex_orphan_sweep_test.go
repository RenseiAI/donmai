package daemon

import (
	"context"
	"os"
	"path/filepath"
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
// fix: daemon Start actually invokes the configured orphan sweeper exactly
// once, so crash/SIGKILL/restart leftovers are reclaimed instead of
// accumulating in the temp dir forever. The fake sweeper is supplied through
// Options.CodexOrphanSweeper — the same seam the production entry point
// configures with the real providercodex.SweepOrphans — so this test never
// touches a real temp dir or signals a real process.
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
	if calls != 1 {
		t.Fatalf("sweep calls = %d, want exactly 1 per Start", calls)
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
