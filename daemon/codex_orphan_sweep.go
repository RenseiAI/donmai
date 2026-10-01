package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/shimwire"
)

// codexOrphanSweepTimeout bounds the whole daemon-startup orphan sweep: the
// sweep's own MaxDuration enforces the same budget internally, but Start must
// not block past it waiting for the call to return either. Short enough that
// a pathological temp dir cannot stall startup; the sweep itself also stops
// early on context cancellation.
const codexOrphanSweepTimeout = 45 * time.Second

// codexOrphanSweepFunc is the package seam the startup sweep runs through.
// Production points at providercodex.SweepOrphans; tests replace it to
// observe the call without touching the real temp dir or signalling real
// processes. Assign-only in tests; never nil in production Start.
var codexOrphanSweepFunc = providercodex.SweepOrphans

// sweepCodexOrphans runs the codex-harness orphan sweep once per daemon
// start, after shim adoption (so every live session is known) and before the
// daemon registers or claims work. Crash/SIGKILL/restart leftovers —
// donmai-named homes and socket dirs whose owning process is gone, plus the
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
// next start retries.
func (d *Daemon) sweepCodexOrphans(ctx context.Context) {
	protected := d.liveAdoptedCodexHomes()
	sweepCtx, cancel := context.WithTimeout(ctx, codexOrphanSweepTimeout)
	defer cancel()
	report := codexOrphanSweepFunc(sweepCtx, providercodex.SweepOptions{ProtectedHomes: protected})
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
