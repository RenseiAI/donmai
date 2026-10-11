package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// trustLedgerSweepTimeout bounds the whole daemon-startup trust-ledger
// sweep: every config home swept holds the trust lock only for one small
// read plus one small write, so this is generous. Short enough that a
// pathological config home cannot stall startup; the sweep never fails
// startup either way — see sweepSeededTrustLedger.
const trustLedgerSweepTimeout = 30 * time.Second

// TrustLedgerSweepFunc is the shape of the startup sweep call: the one the
// real providerclaude.SweepSeededTrustForConfigHome has, and the one
// Options.TrustLedgerSweeper is typed as. Keeping it a named type (rather
// than inlining the signature at both the field and the real function) is
// what lets a test assign providerclaude.SweepSeededTrustForConfigHome
// itself to it without a wrapper closure.
type TrustLedgerSweepFunc func(configHome string, live func(session string) bool) int

// sweepSeededTrustLedger runs the seeded-trust crash sweep once per daemon
// start, after shim adoption (so every live session is known) and before
// the daemon registers or claims work — beside sweepCodexOrphans, which
// owns the same slot for the codex lane. A session that crashed between
// seeding workspace trust and removing it leaves that trust in the CLI's
// record with nobody left to clean it up; the sweep undoes every ledger
// entry whose session is no longer live (see
// provider/harness/claude/trust_ledger.go) while a live session's entries
// are never touched.
//
// The sweep degrades, never destroys: a config home with no ledger, an
// unparseable ledger, or a trust record the sweep must not rewrite all
// sweep nothing, and a sweep failure never fails startup — it is logged
// and the next start retries.
//
// d.opts.TrustLedgerSweeper is nil unless a caller explicitly configures
// it, which makes this a no-op for every Start call that does not ask for
// it. Only the production entry point (afcli/daemon_run.go) sets it, to
// providerclaude.SweepSeededTrustForConfigHome, so operator behaviour is
// unchanged; a test that wants to observe or exercise the sweep sets
// Options.TrustLedgerSweeper itself instead of relying on (or fighting) a
// package-wide default.
func (d *Daemon) sweepSeededTrustLedger(ctx context.Context) {
	sweep := d.opts.TrustLedgerSweeper
	if sweep == nil {
		return
	}
	homes := d.trustLedgerConfigHomes()
	live := d.liveTrustLedgerSessions()
	sweepCtx, cancel := context.WithTimeout(ctx, trustLedgerSweepTimeout)
	defer cancel()
	total := 0
	for _, home := range homes {
		select {
		case <-sweepCtx.Done():
			slog.Info("daemon: seeded-trust sweep timed out", "swept", total)
			return
		default:
		}
		total += sweep(home, live)
	}
	slog.Info("daemon: seeded-trust sweep complete", "configHomes", len(homes), "rowsDropped", total)
}

// trustLedgerConfigHomes returns the config homes that may carry a seeded-
// trust ledger: the process config home plus every live adopted session's
// own session-scoped override. Best-effort: any failure sweeps just the
// process home rather than blocking startup.
func (d *Daemon) trustLedgerConfigHomes() []string {
	seen := map[string]struct{}{}
	var homes []string
	add := func(home string) {
		if home == "" || !isAbsPath(home) {
			return
		}
		if _, dup := seen[home]; dup {
			return
		}
		seen[home] = struct{}{}
		homes = append(homes, home)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	if override := os.Getenv("CLAUDE_CONFIG_DIR"); override != "" {
		add(override)
	}
	return homes
}

// liveTrustLedgerSessions returns the liveness predicate the sweep consults:
// a ledger session identity reads as live when this daemon adopted it and
// has not observed its exit. Ledger rows carry "session:<name>" for named
// sessions and "pid:<pid>" for unnamed ones (see
// provider/harness/claude.trustLedgerOwner): the pid form names a process
// that died with the crash, so it never reads as live here — only an
// adopted session identity does.
func (d *Daemon) liveTrustLedgerSessions() func(session string) bool {
	live := map[string]struct{}{}
	if d.shims != nil {
		d.shims.mu.RLock()
		for id, entry := range d.shims.adopted {
			if entry.terminal {
				continue
			}
			live["session:"+id.SessionID] = struct{}{}
		}
		d.shims.mu.RUnlock()
	}
	return func(session string) bool {
		_, ok := live[session]
		return ok
	}
}

// isAbsPath reports whether p is absolute (filepath, so Windows volume
// names count).
func isAbsPath(p string) bool {
	return filepath.IsAbs(p)
}
