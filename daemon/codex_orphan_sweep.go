package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/shimwire"
)

// codexOrphanSweepTimeout bounds one daemon-driven orphan sweep pass: the
// sweep's own MaxDuration enforces the same budget internally, but the
// caller must not block past it waiting for the call to return either.
// Short enough that a pathological root cannot stall startup (or a
// periodic tick); the sweep itself also stops early on context
// cancellation.
const codexOrphanSweepTimeout = 45 * time.Second

// codexOrphanSweepInterval is how often the daemon re-runs the orphan sweep
// after the startup pass: crash/SIGKILL leftovers that land mid-uptime (or
// that a budget-capped startup pass never reached) are reclaimed within
// about an hour instead of surviving until the next restart. Long enough
// that a pathological root cannot keep the sweeper hot — each pass is
// independently bounded by codexOrphanSweepTimeout — and short enough that
// accumulation stays flat day over day.
const codexOrphanSweepInterval = time.Hour

// CodexOrphanSweepFunc is the shape of a sweep pass: the one the real
// providercodex.SweepOrphans has, and the one Options.CodexOrphanSweeper
// is typed as. Keeping it a named type (rather than inlining the signature
// at both the field and the real function) is what lets a test assign
// providercodex.SweepOrphans itself to it without a wrapper closure.
type CodexOrphanSweepFunc func(context.Context, providercodex.SweepOptions) providercodex.SweepReport

// sweepCodexOrphans runs one codex-harness orphan sweep pass over every
// root providercodex.DefaultCodexHomeRoots names — the system temp
// directory and the operator's persistent Codex home — with the homes
// still referenced by live adopted sessions protected. At startup it runs
// after shim adoption (so every live session is known) and before the
// daemon registers or claims work; afterwards runCodexOrphanSweepLoop
// re-runs it hourly. Crash/SIGKILL/restart leftovers — donmai-named homes
// and socket dirs whose owning process is gone, plus the
// identity-verified orphaned app-server processes pinned to them — are
// reclaimed here; lifecycle teardown still owns the live-session path, this
// only covers what that path can never reach again.
//
// The sweep degrades, never destroys: it only ever acts inside the
// donmai-codex-home-*/donmai-codex-app-* naming fence, never ambient user
// state; it never signals a process it cannot prove this fleet started; and
// homes still referenced by a live adopted session's resume key are passed
// in ProtectedHomes so a previous generation's dead owner manifest cannot
// condemn them. A sweep failure never fails startup — it is logged and the
// next pass retries.
//
// d.opts.CodexOrphanSweeper is nil unless a caller explicitly configures it,
// which makes this a no-op — no root read, no delete, no signal — for
// every Start call that does not ask for it. Only the production entry
// point (afcli/daemon_run.go) sets it, to providercodex.SweepOrphans, so
// operator behaviour is unchanged; a test that wants to observe or exercise
// the sweep sets Options.CodexOrphanSweeper itself instead of relying on (or
// fighting) a package-wide default.
func (d *Daemon) sweepCodexOrphans(ctx context.Context) providercodex.SweepReport {
	sweep := d.opts.CodexOrphanSweeper
	if sweep == nil {
		return providercodex.SweepReport{}
	}
	protected := d.liveAdoptedCodexHomes()
	var merged providercodex.SweepReport
	for _, root := range providercodex.DedupeCodexHomeRoots(providercodex.DefaultCodexHomeRoots()) {
		sweepCtx, cancel := context.WithTimeout(ctx, codexOrphanSweepTimeout)
		report := sweep(sweepCtx, providercodex.SweepOptions{Root: root, ProtectedHomes: protected})
		cancel()
		merged.Scanned += report.Scanned
		merged.Reclaimed += report.Reclaimed
		merged.PartiallyReclaimed += report.PartiallyReclaimed
		merged.Acted += report.Acted
		merged.SkippedUnowned += report.SkippedUnowned
		merged.SkippedIrreducible += report.SkippedIrreducible
		merged.ExpiredRetained += report.ExpiredRetained
		merged.SkippedProtected += report.SkippedProtected
		merged.Terminated += report.Terminated
		merged.SkippedYoung += report.SkippedYoung
		merged.SkippedLive += report.SkippedLive
		merged.SkippedAmbiguous += report.SkippedAmbiguous
		merged.Errors += report.Errors
	}
	report := merged
	slog.Info("daemon: codex orphan sweep complete",
		"scanned", report.Scanned,
		"reclaimed", report.Reclaimed,
		"partiallyReclaimed", report.PartiallyReclaimed,
		"expiredRetained", report.ExpiredRetained,
		"terminated", report.Terminated,
		"skippedProtected", report.SkippedProtected,
		"skippedIrreducible", report.SkippedIrreducible,
		"errors", report.Errors,
	)
	return report
}

// startCodexOrphanSweepLoop re-runs sweepCodexOrphans hourly until ctx ends.
// The startup pass alone cannot hold accumulation flat: leftovers that
// land mid-uptime — and entries a budget-capped startup pass never
// reached — would otherwise survive until the next restart, which on a
// long-lived host is never. Each pass is independently bounded by
// codexOrphanSweepTimeout, re-reads the live adopted homes (so a session
// adopted after startup is protected on the next pass, not just the
// first), and never fails anything — a pass that errors just logs and the
// next tick retries. Nil sweeper (tests, embedders that never set it) is a
// no-op: no goroutine, no timer, nothing to stop.
func (d *Daemon) startCodexOrphanSweepLoop(ctx context.Context) {
	if d.opts.CodexOrphanSweeper == nil {
		return
	}
	go d.runCodexOrphanSweepLoop(ctx, codexOrphanSweepInterval)
}

// runCodexOrphanSweepLoop is the blocking body of startCodexOrphanSweepLoop:
// one sweep pass per interval tick, passes strictly sequential (a pass
// still running when the next tick lands delays it rather than overlapping
// it), returning as soon as ctx ends. The interval is a parameter so a test
// can observe ticks and shutdown without waiting an hour.
func (d *Daemon) runCodexOrphanSweepLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sweepCodexOrphans(ctx)
		}
	}
}

// liveAdoptedCodexHomes collects the absolute codex-home paths still
// referenced by live adopted sessions' resume keys. Best-effort: any failure
// (no registry, unreadable entry) protects nothing rather than blocking
// startup — the sweep's own owner-liveness and age gates still apply to
// every entry.
func (d *Daemon) liveAdoptedCodexHomes() map[string]struct{} {
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return nil
	}
	entries, err := registry.Scan()
	if err != nil {
		return nil
	}
	var out map[string]struct{}
	for _, entry := range entries {
		if entry.Err != nil || entry.Record.ResumeKey == nil {
			continue
		}
		home := filepath.Clean(entry.Record.ResumeKey.CodexHome)
		if home == "" || !filepath.IsAbs(home) {
			continue
		}
		// Only live (non-terminal) records protect their home: a tombstoned
		// session's home is exactly what the TTL path exists to reap.
		if terminalSessionShimPhase(entry.Record.Phase) {
			continue
		}
		if out == nil {
			out = map[string]struct{}{}
		}
		out[home] = struct{}{}
	}
	return out
}

// terminalSessionShimPhase reports whether a registry record's phase is a
// terminal one whose home no longer needs adoption protection. Only the
// closed v1 phase registry's terminal value (PhaseExited) counts: an
// unknown or future phase fails open (treated as live, home protected)
// rather than silently widening reaping.
func terminalSessionShimPhase(phase shimwire.Phase) bool {
	return phase == shimwire.PhaseExited
}
