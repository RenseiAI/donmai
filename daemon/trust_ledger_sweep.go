package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
)

// trustLedgerSweepTimeout bounds the whole daemon-startup trust-ledger
// sweep: every config home swept holds the trust lock only for one small
// read plus one small write, so this is generous. Short enough that a
// pathological config home cannot stall startup; the sweep never fails
// startup either way — see sweepSeededTrustLedger.
const trustLedgerSweepTimeout = 30 * time.Second

// trustLedgerMaxDiscoveredHomes caps how many ledger-named override homes
// one sweep pass visits beyond the process-enumerated homes. The ledger
// names its own debris — each row records the home its seed wrote beside
// — so discovery follows rows wherever they point; without a cap a ledger
// naming many distinct homes would walk and lock every one of them on
// every daemon start, stalling startup up to the sweep-wide timeout. Past
// the cap the sweep stops visiting new homes and logs how many it left
// undiscovered; those homes wait for the next start. The cap is generous
// by construction: one override home per isolated session, and isolated
// sessions are the rare shape, not the common one.
const trustLedgerMaxDiscoveredHomes = 64

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
// Concurrency: two daemon generations can sweep at once — the old one
// draining while the new one starts. That interleaving is benign: every
// pass takes the trust lock per home (one small read plus one small write),
// ledger rows are idempotent to drop (re-dropping an already-drained row
// is a no-op against a record that no longer carries it), and a row the
// sweep could not apply stays for the next start to retry.
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
	perHome := make([]int, 0, len(homes))
	discovered := 0
	discoveryCapped := false
	visited := make(map[string]struct{}, len(homes))
	for _, home := range homes {
		visited[home] = struct{}{}
	}
	// The ledger names its own debris: each row records the absolute
	// config home the seed wrote it beside, so one pass over the process
	// homes discovers session-scoped override homes the process
	// enumeration above could never reach. The loop revisits until no
	// visited home names a new one; every iteration holds the trust lock
	// only for one small read plus one small write per home, so the
	// sweep-wide timeout still bounds the whole pass — and the discovery
	// cap below bounds how many ledger-named homes one start visits, so a
	// ledger naming hundreds of homes cannot stall startup.
	for i := 0; i < len(homes); i++ {
		home := homes[i]
		select {
		case <-sweepCtx.Done():
			slog.Info("daemon: seeded-trust sweep timed out", "swept", total)
			return
		default:
		}
		// Discover override homes BEFORE sweeping this home: the ledger
		// names its own debris even when its rows are live (a live
		// session's rows name its isolated home while staying protected
		// themselves), so discovery must not wait for the naming home's
		// own sweep to drain first. A live session's discovery pointer
		// survives its home's own sweep for exactly this reason (see
		// sweepTrustLedger), so a restart mid-run still finds the
		// isolated home at the next start.
		if !discoveryCapped {
			for _, next := range providerclaude.LedgerConfigHomes(
				filepath.Join(home, providerclaude.TrustRecordFileName),
				visited,
			) {
				if discovered >= trustLedgerMaxDiscoveredHomes {
					discoveryCapped = true
					break
				}
				discovered++
				homes = append(homes, next)
			}
		}
		dropped := sweep(home, live)
		total += dropped
		perHome = append(perHome, dropped)
	}
	if discoveryCapped {
		slog.Info("daemon: seeded-trust sweep complete",
			"configHomes", len(homes), "rowsDropped", total, "perHome", perHome,
			"discoveryCapped", true, "discoveryCap", trustLedgerMaxDiscoveredHomes)
		return
	}
	slog.Info("daemon: seeded-trust sweep complete",
		"configHomes", len(homes), "rowsDropped", total, "perHome", perHome)
}

// trustLedgerConfigHomes returns the config homes that may carry a seeded-
// trust ledger: the process config home (the operator home, plus the
// daemon-process CLAUDE_CONFIG_DIR override when set). Best-effort: any
// failure sweeps just the process home rather than blocking startup.
//
// Session-scoped override homes — a session whose own env layer sets
// CLAUDE_CONFIG_DIR to a home the daemon process never ran with — are NOT
// enumerated here. Each ledger row records the absolute home its seed wrote
// beside (see provider/harness/claude.trustLedgerEntry.ConfigHome), and
// the sweep loop above follows those rows: visiting the process homes
// discovers every override home that carries rows. A home whose ledger is
// gone (or that holds only rows predating the per-row field) needs no
// visit — there is nothing recorded there to drain.
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

// liveTrustLedgerSessions returns the liveness predicate the sweep consults.
// A ledger session identity reads as live when this daemon adopted it and
// has not observed its exit. Ledger rows carry "session:<SessionID>" — the
// platform lifecycle UUID the runner threads through agent.Spec.SessionID
// (see provider/harness/claude.trustLedgerOwner) — and "pid:<pid>" for
// sessions that carried no lifecycle id (standalone/legacy dispatch).
// The pid form names the spawner process, so it reads as live while that
// process still runs — a live id-less session keeps its trust across a
// daemon restart mid-run — and as dead once the pid is gone. Anything else
// (empty, unparsable) reads as dead. The probe consults the SHARED verdict
// (providerclaude.TrustLedgerSessionLive), so daemon and provider can
// never disagree about one identity.
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
		return providerclaude.TrustLedgerSessionLive(session, live)
	}
}

// isAbsPath reports whether p is absolute (filepath, so Windows volume
// names count).
func isAbsPath(p string) bool {
	return filepath.IsAbs(p)
}
