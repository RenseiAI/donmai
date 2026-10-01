package daemon

import (
	"context"
	"testing"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
)

// TestDaemonStartRunsCodexOrphanSweep pins the missing-caller half of the
// fix: daemon Start actually invokes the orphan sweep once, so
// crash/SIGKILL/restart leftovers are reclaimed instead of accumulating in
// the temp dir forever. The seam replaces the real sweep (no temp-dir walk,
// no signals) and Start runs with registration skipped.
func TestDaemonStartRunsCodexOrphanSweep(t *testing.T) {
	calls := 0
	prev := codexOrphanSweepFunc
	codexOrphanSweepFunc = func(context.Context, providercodex.SweepOptions) providercodex.SweepReport {
		calls++
		return providercodex.SweepReport{}
	}
	t.Cleanup(func() { codexOrphanSweepFunc = prev })

	tmp := t.TempDir()
	d := New(Options{
		ConfigPath: tmp + "/daemon.yaml", JWTPath: tmp + "/daemon.jwt",
		SkipWizard: true, SkipRegistration: true,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/true"},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	if calls != 1 {
		t.Fatalf("sweep calls = %d, want exactly 1 per Start", calls)
	}
}

// TestDaemonStartSweepFailureNeverFailsStartup pins the degrade-don't-destroy
// contract at the call site: even if the sweep itself errors on every entry,
// Start still succeeds — a best-effort disk reclamation must never take down
// the daemon.
func TestDaemonStartSweepFailureNeverFailsStartup(t *testing.T) {
	prev := codexOrphanSweepFunc
	codexOrphanSweepFunc = func(context.Context, providercodex.SweepOptions) providercodex.SweepReport {
		return providercodex.SweepReport{Errors: 500}
	}
	t.Cleanup(func() { codexOrphanSweepFunc = prev })

	tmp := t.TempDir()
	d := New(Options{
		ConfigPath: tmp + "/daemon.yaml", JWTPath: tmp + "/daemon.jwt",
		SkipWizard: true, SkipRegistration: true,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/true"},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start with a failing sweep: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
}
