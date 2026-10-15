package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the crash-safety half of the workspace-trust seed
// (trust.go): a per-config-home ledger of seeded trust entries.
//
// The problem: removeSeededTrust runs in the spawning process at session
// end, so a crash between seed and cleanup — the worker dies, the host
// reboots, the daemon restarts — leaves the seed in the CLI's trust record
// with nobody left to remove it. That entry then trusts the worktree for
// every later session on the host, silently. A crashed session's leftovers
// must be found and undone at the next start, while a live session's
// entries must never be touched.
//
// The ledger records, under the same trust lock that guards the record, one
// entry per (path, owning session): every directory the seed created or
// flipped, the prior value a flipped entry carried (absent means the entry
// had no trust key), and the owning session. The daemon sweep at start
// (daemon/trust_ledger_sweep.go, fed by this package's exported Sweep
// entry) drops every entry whose session is no longer live: entries the
// ledger's own seed created are removed whole, flipped entries are put back
// to their prior value — the same ownership rule removeSeededTrust applies
// on the live path.
//
// Best-effort throughout, like the seed itself: a ledger that cannot be
// read, parsed, or written leaves the session to the modal rather than
// failing the spawn, and a sweep that cannot run leaves the record as
// found. The ledger is harness state, never trust state: the CLI never
// reads it, and the seed never consults it when deciding what to trust.

// trustLedgerFileName is the ledger basename beside the trust record under
// the same config home.
const trustLedgerFileName = ".claude.json.seeded-trust-ledger"

// TrustRecordFileName is the CLI trust-record basename, exported so the
// daemon sweep can resolve a visited home to the ledger beside it.
const TrustRecordFileName = claudeTrustFileName

// TrustLedgerFileName is the ledger basename beside the trust record,
// exported alongside TrustRecordFileName for the same sweep path.
const TrustLedgerFileName = trustLedgerFileName

// trustLedgerMaxEntries bounds the ledger file: each entry is one seeded
// directory of one session, so this is generous by construction. Past the
// bound the ledger stops recording new seeds (the seed itself still lands —
// the operator-visible behaviour never degrades) and the sweep still drains
// what is recorded.
const trustLedgerMaxEntries = 4096

// trustLedgerMaxForeignRetries bounds how many sweep passes keep dead
// sessions' rows beside a record the sweep cannot rewrite (unparseable, not
// ours to replace). Such a record can never change — the owning session is
// dead, so nothing will ever rewrite it into a shape the sweep can apply —
// and an unbounded keep would pile inert rows retried every boot. Past the
// bound the rows drain: the sweep never rewrites the foreign record itself
// (byte-for-byte as found, fail-closed for the CLI too), it only stops
// carrying the ledger rows that could never apply. Malformed records are
// fail-closed for the CLI, and never rewriting foreign records is
// deliberate; this bound only retires the sweep's own retry residue.
const trustLedgerMaxForeignRetries = 10

// trustLedgerEntry is one seeded directory's ledger row.
type trustLedgerEntry struct {
	// Dir is the trust-record key the seed wrote.
	Dir string `json:"dir"`
	// Created is true when the seed created the entry (absent before);
	// false when it flipped an existing entry's trust key.
	Created bool `json:"created,omitempty"`
	// Prior carries the flipped entry's previous trust-key value. HadKey is
	// false when the entry had no trust key at all (so the sweep deletes
	// the key rather than restoring it).
	Prior  any  `json:"prior,omitempty"`
	HadKey bool `json:"hadKey,omitempty"`
	// Session identifies the owning session (see seededTrust.session).
	// Empty on a discovery row: the row names an override home for the
	// sweep to VISIT (see recordSeededTrustLedger), not trust to undo.
	Session string `json:"session,omitempty"`
	// ConfigHome is the absolute config home the seed wrote this row
	// beside, recorded so the daemon sweep can visit exactly the homes
	// that carry rows — including a session-scoped CLAUDE_CONFIG_DIR
	// override the daemon process itself never ran with (see
	// daemon/trust_ledger_sweep.go). Empty on rows that predate the
	// field; those rows sweep under the process home as before.
	ConfigHome string `json:"configHome,omitempty"`
	// ForeignRetries counts how many sweep passes have kept this row beside
	// a record the sweep could not rewrite (unparseable, not ours to
	// replace). Zero on rows the seed writes; incremented by each sweep
	// that retries them against the still-foreign record, so the rows drain
	// past trustLedgerMaxForeignRetries instead of accumulating as inert
	// residue retried every boot.
	ForeignRetries int `json:"foreignRetries,omitempty"`
	// SeededAt is when the seed landed, for operator diagnosis.
	SeededAt time.Time `json:"seededAt,omitempty"`
}

// trustLedgerOwner returns the ledger's owning-session identity for a
// spawn. The sweep's liveness set (daemon/liveTrustLedgerSessions) keys on
// the daemon's adopted lifecycle identities — sessionshim.Identity SessionIDs,
// the platform UUIDs daemon/launchSessionShim adopts — so the owner MUST be
// that same SessionID, threaded through agent.Spec.SessionID by the runner
// (runner/spec_translation.go). It MUST NOT be Spec.SessionName: that is the
// user-facing display handle (prompt/queued_work.go: "never derived from
// the provider-native session id"), and keying rows on it would let the
// sweep undo a still-running session's trust at the next daemon restart —
// the live rows would never match the SessionID-keyed live set. An empty
// SessionID falls back to a process-scoped pid identity; pid rows name the
// process that spawned them (os.Getpid at seed time), and the sweep reads
// the row as LIVE while that process still runs (so an id-less standalone
// or legacy session keeps its trust across a daemon restart mid-run) and
// as dead once the pid is gone (see daemon/trust_ledger_sweep.go) — the
// crash-debris half. A recycled pid keeps a live read only until the
// sweep's liveness probe fails; fail-closed on lookup error.
func trustLedgerOwner(spec agent.Spec) string {
	if id := strings.TrimSpace(spec.SessionID); id != "" {
		return "session:" + id
	}
	return fmt.Sprintf("pid:%d", os.Getpid())
}

// TrustLedgerSessionLive reports whether a ledger session identity still
// owns a live session. "session:<id>" reads as live when the daemon
// adopted that lifecycle identity and has not observed its exit (the live
// set daemon/liveTrustLedgerSessions builds). "pid:<pid>" reads as live
// while that process still runs — the seed names its own spawner, so a
// live id-less session's rows survive a daemon restart — and dead once the
// pid is gone. Anything else (empty, unparsable, non-positive pid) reads
// as dead. A liveness probe that errors reads as live: failing closed
// keeps the sweep from undoing a live session's trust on an uninspectable
// pid. Exported so the daemon sweep and the provider sweep consult the
// same verdict and can never disagree about one identity.
func TrustLedgerSessionLive(session string, adopted map[string]struct{}) bool {
	if session == "" {
		return false
	}
	if _, ok := adopted[session]; ok {
		return true
	}
	pid, ok := parseTrustLedgerPidOwner(session)
	if !ok {
		return false
	}
	if pid <= 0 {
		return false
	}
	alive, err := trustLedgerPidAlive(pid)
	if err != nil {
		return true
	}
	return alive
}

// parseTrustLedgerPidOwner splits a "pid:<pid>" ledger identity.
func parseTrustLedgerPidOwner(session string) (int, bool) {
	pidText, ok := strings.CutPrefix(session, "pid:")
	if !ok || strings.TrimSpace(pidText) == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// recordSeededTrustLedger appends seed's entries to the per-config-home
// ledger under the trust lock. Silent and best-effort: any failure leaves
// the seed in place (the live-path cleanup still knows exactly what to
// remove) and the session proceeds.
//
// When the seed lands under a config-home override the daemon process
// itself never ran with (spec.Env carries its own CLAUDE_CONFIG_DIR), the
// daemon-start sweep would never enumerate that home — isolation would
// hide the crash debris forever. So the seed ALSO leaves a discovery
// pointer behind the daemon's own process home: a ledger row with no
// directory carrying only the owning session and the override home it
// names. A live session's pointer survives that home's own sweep passes
// (see sweepTrustLedger: directory-less rows drain only with their own
// dead session) while still naming the isolated home for discovery, so a
// restart mid-run keeps the pointer and a later crash still drains. The
// process-home sweep then follows that pointer to the isolated home (see
// daemon/trust_ledger_sweep.go). Skipped exactly when there is no override
// to name — the seed wrote beside the process home itself — or when the
// process home is not absolute; discovery-link failure never disturbs the
// seed itself.
func recordSeededTrustLedger(seed *seededTrust) {
	if seed == nil || seed.path == "" || (len(seed.dirs) == 0 && len(seed.flipped) == 0) {
		return
	}
	release, ok := acquireTrustLock(seed.path)
	if !ok {
		return
	}
	defer release()
	entries, _ := readTrustLedgerLocked(seed.path, false)
	if len(entries) >= trustLedgerMaxEntries {
		return
	}
	now := time.Now().UTC()
	configHome := filepath.Dir(seed.path)
	for _, dir := range seed.dirs {
		entries = append(entries, trustLedgerEntry{Dir: dir, Created: true, Session: seed.session, ConfigHome: configHome, SeededAt: now})
	}
	for _, dir := range seed.flipped {
		var prior any
		hadKey := false
		if seed.prior != nil {
			prior, hadKey = seed.prior[dir]
		}
		entries = append(entries, trustLedgerEntry{Dir: dir, Prior: prior, HadKey: hadKey, Session: seed.session, ConfigHome: configHome, SeededAt: now})
	}
	_ = writeTrustLedger(seed.path, entries)
	recordTrustLedgerDiscoveryLink(seed, configHome)
}

// recordTrustLedgerDiscoveryLink leaves the crash-sweep discovery pointer
// behind the daemon process's own config home: a ledger row with no
// directory naming overrideHome — the session-scoped config home seed
// wrote beside — so the daemon-start sweep visits the isolated home even
// though the process enumeration can never name it (see
// daemon/trust_ledger_sweep.go). The pointer row carries the seed's owning
// session and home: sweepTrustLedger keeps a live session's directory-less
// row (it never applies to the record) while still following it to the
// isolated home, and forgetSeededTrustLedger drops the link on the live
// path exactly like any other row the seed wrote — so a live session's
// link survives restarts without stranding the override home open for a
// later sweep to misread, and a crashed session's link drains with its
// own sweep pass.
//
// Runs only when there is actually an override to name: overrideHome must
// be absolute and must differ from the daemon process's own config home
// (os.Getenv("CLAUDE_CONFIG_DIR"), else the operator home). Same-home
// seeds are discovered by the process enumeration itself. Best-effort and
// silent throughout: a link that cannot land leaves the seed in place.
func recordTrustLedgerDiscoveryLink(seed *seededTrust, overrideHome string) {
	if overrideHome == "" || !filepath.IsAbs(overrideHome) {
		return
	}
	processHome := ""
	if override := os.Getenv(claudeConfigDirEnv); override != "" {
		if !filepath.IsAbs(override) {
			return
		}
		processHome = override
	} else if home, err := os.UserHomeDir(); err == nil {
		processHome = home
	}
	if processHome == "" || !filepath.IsAbs(processHome) || processHome == overrideHome {
		return
	}
	link, err := processTrustRecordPath()
	if err != nil || link == "" || filepath.Dir(link) == overrideHome {
		return
	}
	release, ok := acquireTrustLock(link)
	if !ok {
		return
	}
	defer release()
	entries, _ := readTrustLedgerLocked(link, false)
	for _, e := range entries {
		if e.Dir == "" && e.ConfigHome == overrideHome && e.Session == seed.session {
			return // link already present for this session's home
		}
	}
	if len(entries) >= trustLedgerMaxEntries {
		return
	}
	entries = append(entries, trustLedgerEntry{
		Session:    seed.session,
		ConfigHome: overrideHome,
		SeededAt:   time.Now().UTC(),
	})
	_ = writeTrustLedger(link, entries)
}

// processTrustRecordPath resolves the CLI trust record path the daemon
// process itself runs with — the process CLAUDE_CONFIG_DIR override when
// absolute, else the operator home — or an error when neither resolves to
// an absolute home the sweep would enumerate.
func processTrustRecordPath() (string, error) {
	if override := os.Getenv(claudeConfigDirEnv); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("relative %s %q: no process trust record to link", claudeConfigDirEnv, override)
		}
		return filepath.Join(override, claudeTrustFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", fmt.Errorf("no process trust record to link: %w", err)
	}
	return filepath.Join(home, claudeTrustFileName), nil
}

// forgetSeededTrustLedger drops seed's entries from the ledger under the
// trust lock — both beside the seed's own home AND the discovery link the
// seed left behind the daemon process home (see
// recordTrustLedgerDiscoveryLink). It runs deferred at the top of
// removeSeededTrust — before the lock for the record itself is taken — so
// the ledger cannot retain an entry the live cleanup already removed, even
// if that cleanup then fails to take the record lock. Silent and
// best-effort throughout.
func forgetSeededTrustLedger(seed *seededTrust) {
	if seed == nil || seed.path == "" || (len(seed.dirs) == 0 && len(seed.flipped) == 0) {
		return
	}
	release, ok := acquireTrustLock(seed.path)
	if !ok {
		return
	}
	defer release()
	entries, ok := readTrustLedgerLocked(seed.path, false)
	if !ok {
		return
	}
	keep := entries[:0]
	for _, e := range entries {
		if ledgerEntryOwnedBy(e, seed) {
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) != len(entries) {
		_ = writeTrustLedger(seed.path, keep)
	}
	// The discovery link names the seeded home but lives beside the
	// process home: drop it by session+home, since a directory-less row
	// is never "owned" by ledgerEntryOwnedBy's directory match. Without
	// this the live path would strand the link and the next sweep would
	// visit the override home for nothing.
	forgetTrustLedgerDiscoveryLink(seed)
}

// forgetTrustLedgerDiscoveryLink drops the discovery link
// recordTrustLedgerDiscoveryLink left behind the daemon process home for
// seed's session and home: directory-less rows naming the home seed wrote
// beside, owned by seed's session. Best-effort and silent; a no-op when
// the link path resolves beside the seed itself (same-home seeds leave no
// link) or cannot be taken under the lock.
func forgetTrustLedgerDiscoveryLink(seed *seededTrust) {
	if seed == nil || seed.path == "" {
		return
	}
	overrideHome := filepath.Dir(seed.path)
	link, err := processTrustRecordPath()
	if err != nil || link == "" || filepath.Dir(link) == overrideHome {
		return
	}
	release, ok := acquireTrustLock(link)
	if !ok {
		return
	}
	defer release()
	entries, ok := readTrustLedgerLocked(link, false)
	if !ok {
		return
	}
	keep := entries[:0]
	for _, e := range entries {
		if e.Dir == "" && e.Session == seed.session && e.ConfigHome == overrideHome {
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) != len(entries) {
		_ = writeTrustLedger(link, keep)
	}
}

// ledgerEntryOwnedBy reports whether ledger entry e is one seed wrote: same
// directory, same creation kind, same owning session. The session match is
// what keeps one live session's cleanup from dropping another live
// session's row for the same directory.
func ledgerEntryOwnedBy(e trustLedgerEntry, seed *seededTrust) bool {
	if e.Session != seed.session {
		return false
	}
	if e.Created {
		for _, dir := range seed.dirs {
			if e.Dir == dir {
				return true
			}
		}
		return false
	}
	for _, dir := range seed.flipped {
		if e.Dir == dir {
			return true
		}
	}
	return false
}

// trustLedgerPath returns the ledger path beside the trust record.
func trustLedgerPath(trustRecordPath string) string {
	return filepath.Join(filepath.Dir(trustRecordPath), trustLedgerFileName)
}

// readTrustLedger returns the ledger entries beside the trust record at
// trustRecordPath. ok is false when the ledger is absent, unreadable, or
// unparseable — all three read as "no ledger", never as an error, so a
// missing or foreign ledger never disturbs the seed.
//
// The read takes the trust lock: seeds, live cleanups, discovery links,
// and the sweep all write through it, and an unlocked read at startup
// could tear against one of them — listing no homes requires no lock to
// lose, only one torn read to skip (see daemon/trust_ledger_sweep.go).
func readTrustLedger(trustRecordPath string) (entries []trustLedgerEntry, ok bool) {
	return readTrustLedgerLocked(trustRecordPath, true)
}

// readTrustLedgerLocked is readTrustLedger with the locking made explicit.
// Callers that already hold the trust lock for trustRecordPath (every
// in-package writer and the sweep) pass locked=false; the daemon sweep's
// discovery pass — no lock held — passes locked=true so the discovery read
// cannot tear against a concurrent writer.
func readTrustLedgerLocked(trustRecordPath string, locked bool) (entries []trustLedgerEntry, ok bool) {
	if locked {
		release, ok := acquireTrustLock(trustRecordPath)
		if !ok {
			return nil, false
		}
		defer release()
	}
	data, err := os.ReadFile(trustLedgerPath(trustRecordPath)) //nolint:gosec // harness-owned ledger beside the trust record
	if err != nil {
		return nil, false
	}
	var decoded []trustLedgerEntry
	if json.Unmarshal(data, &decoded) != nil {
		return nil, false
	}
	return decoded, true
}

// writeTrustLedger replaces the ledger beside the trust record at
// trustRecordPath with entries, riding a temp-file rename so a concurrent
// reader never sees a half-written ledger. The caller holds the trust lock.
func writeTrustLedger(trustRecordPath string, entries []trustLedgerEntry) error {
	dir := filepath.Dir(trustRecordPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".seeded-trust-ledger.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, trustLedgerPath(trustRecordPath)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// SweepTrustLedgerOptions configures one crash-sweep pass over the ledger
// beside a trust record.
type SweepTrustLedgerOptions struct {
	// TrustRecordPath is the CLI trust record the ledger sits beside.
	TrustRecordPath string
	// Live reports whether a ledger session identity still owns a live
	// session. Entries whose session reads as live are kept; every other
	// entry is undone. Nil reads every session as dead — the caller that
	// cannot enumerate live sessions must say so explicitly.
	Live func(session string) bool
}

// sweepTrustLedger undoes every ledger entry whose session is no longer
// live: entries the dead session's seed created are removed whole, flipped
// entries are put back to their prior value — the same ownership rule the
// live-path cleanup applies. A directory-less discovery row drains only
// with its own owning session: while that session is still live the row is
// this sweep's pointer to the isolated home, so dropping it early would
// strand that home's debris at the next start — a live restart must keep
// the pointer and a dead session's pointer drains with its own pass. The
// ledger rows themselves are dropped only for entries the sweep actually
// undid; an entry the record no longer honours (already cleaned, already
// re-trusted by its owner) still drops its row, but a row the sweep could
// not apply — the record is unparseable, the lock is held — stays for the
// next start to retry.
//
// It returns the number of ledger rows dropped. Every failure is silent:
// the sweep must never fail a start.
func sweepTrustLedger(opts SweepTrustLedgerOptions) (dropped int) {
	path := opts.TrustRecordPath
	if path == "" {
		return 0
	}
	live := opts.Live
	if live == nil {
		live = func(string) bool { return false }
	}
	release, ok := acquireTrustLock(path)
	if !ok {
		return 0
	}
	defer release()
	entries, ok := readTrustLedgerLocked(path, false)
	if !ok || len(entries) == 0 {
		return 0
	}
	var dead []trustLedgerEntry
	for _, e := range entries {
		if e.Dir == "" {
			// A directory-less row is a discovery pointer, not trust
			// to undo: it names the isolated home for the daemon's
			// discovery pass. A live session's pointer must survive
			// this pass — the sweep still follows it to the isolated
			// home (see LedgerConfigHomes) while the rows beside that
			// home stay protected — so that a restart mid-run never
			// strands the debris a later crash leaves behind.
			if live(e.Session) {
				continue
			}
			dropped++
			continue
		}
		if live(e.Session) {
			continue
		}
		dead = append(dead, e)
	}
	if len(dead) == 0 && dropped == 0 {
		return 0
	}
	if len(dead) > 0 {
		// A created entry whose directory the record no longer carries, or a
		// flipped entry whose record entry is gone, is already undone — but
		// the row still drains. Only an unparseable or foreign record keeps
		// rows for the next start.
		if !applyTrustLedgerSweep(path, dead) {
			// The record could not be rewritten (unparseable, not ours to
			// replace, write failure): keep every row so the next start
			// retries, but still drop dead sessions' directory-less
			// discovery rows — they name no trust to apply. A live
			// session's discovery row stays: it is still that
			// session's pointer to its isolated home. A structurally
			// foreign record (unparseable, unexpected projects shape)
			// can never change — the owning session is dead — so the
			// retry is bounded: each kept row's ForeignRetries
			// increments and rows past trustLedgerMaxForeignRetries
			// drain instead of accumulating as inert residue retried
			// every boot. The foreign record itself is never rewritten
			// (byte-for-byte as found, fail-closed for the CLI too).
			// A lost lock is transient — the next start may hold it —
			// so rows kept for that reason do not count against the
			// bound; they retry until the record resolves.
			if recordStructurallyForeign(path) {
				return dropForeignBounded(path, entries, live)
			}
			// A mid-pass write failure (rename race, disk error) is transient
			// — the next start may succeed — so rows kept for that reason
			// do not count against the foreign-retry bound; they retry
			// until the record resolves.
			if dropped > 0 {
				var keep []trustLedgerEntry
				for _, e := range entries {
					if e.Dir == "" {
						// Dead sessions' discovery rows drained with
						// this pass; a live session's row stays.
						if live(e.Session) {
							keep = append(keep, e)
						}
						continue
					}
					keep = append(keep, e)
				}
				_ = writeTrustLedger(path, keep)
			}
			return dropped
		}
		for range dead {
			dropped++
		}
	}
	keep := make([]trustLedgerEntry, 0, len(entries)-len(dead))
deadLoop:
	for _, e := range entries {
		if e.Dir == "" {
			// A live session's discovery row survived the liveness
			// gate above; keep it so the next start still finds the
			// isolated home. Dead sessions' discovery rows drained
			// with this pass.
			if live(e.Session) {
				keep = append(keep, e)
			}
			continue
		}
		for _, d := range dead {
			if d.Dir == e.Dir && d.Session == e.Session && d.Created == e.Created {
				continue deadLoop
			}
		}
		keep = append(keep, e)
	}
	_ = writeTrustLedger(path, keep)
	return dropped
}

// recordStructurallyForeign reports whether the trust record at path is
// one the sweep can never rewrite no matter how many starts retry it:
// absent-but-accounted (handled by the caller), unparseable, or carrying
// an unexpected projects shape. A lost lock reads as NOT foreign — the
// next start may hold the lock, so those rows retry unbounded until the
// record resolves. The caller holds the trust lock.
func recordStructurallyForeign(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // config path, not session input
	if err != nil {
		return false
	}
	var record map[string]any
	if json.Unmarshal(data, &record) != nil {
		return true
	}
	raw := record[claudeTrustProjectsKey]
	if raw == nil {
		return false
	}
	_, ok := raw.(map[string]any)
	return !ok
}

// dropForeignBounded retires dead sessions' rows beside a structurally
// foreign record (see recordStructurallyForeign): each kept row's
// ForeignRetries increments, rows past trustLedgerMaxForeignRetries drain,
// and dead sessions' directory-less discovery rows drain with the pass
// (they name no trust to apply). A live session's discovery row stays — it
// is still that session's pointer to its isolated home. The foreign record
// itself is never rewritten. It returns the number of ledger rows dropped.
// The caller holds the trust lock.
func dropForeignBounded(path string, entries []trustLedgerEntry, live func(session string) bool) (dropped int) {
	keep := make([]trustLedgerEntry, 0, len(entries))
	for _, e := range entries {
		if e.Dir == "" {
			// Dead sessions' discovery rows drained with this pass; a
			// live session's row stays.
			if live(e.Session) {
				keep = append(keep, e)
				continue
			}
			dropped++
			continue
		}
		if live(e.Session) {
			keep = append(keep, e)
			continue
		}
		if e.ForeignRetries+1 > trustLedgerMaxForeignRetries {
			dropped++
			continue
		}
		e.ForeignRetries++
		keep = append(keep, e)
	}
	_ = writeTrustLedger(path, keep)
	return dropped
}

// applyTrustLedgerSweep rewrites the trust record at path, undoing dead's
// entries: created entries are removed whole, flipped entries get only
// their trust key put back to its prior value. It reports false when the
// record is not ours to rewrite — absent (nothing to undo), unparseable, or
// carrying an unexpected projects shape — so the caller keeps the rows for
// the next start. Unknown record fields are preserved; entries the record
// no longer carries are no-ops. The caller holds the trust lock.
func applyTrustLedgerSweep(path string, dead []trustLedgerEntry) bool {
	data, err := os.ReadFile(path) //nolint:gosec // config path, not session input
	if err != nil {
		// Absent: a dead session's entries have nowhere to live. Report
		// success so the rows drain — there is nothing to undo.
		return errors.Is(err, os.ErrNotExist)
	}
	var record map[string]any
	if json.Unmarshal(data, &record) != nil {
		return false
	}
	raw := record[claudeTrustProjectsKey]
	if raw == nil {
		return true // no projects table: nothing to undo, rows drain
	}
	projects, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	for _, e := range dead {
		if e.Created {
			delete(projects, e.Dir)
			continue
		}
		entry, ok := projects[e.Dir].(map[string]any)
		if !ok {
			continue
		}
		if e.HadKey {
			entry[claudeTrustAcceptedKey] = e.Prior
		} else {
			delete(entry, claudeTrustAcceptedKey)
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return false
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.tmp-*")
	if err != nil {
		return false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return false
	}
	return true
}

// LedgerConfigHomes lists every absolute config home named by the ledger
// beside the trust record at trustRecordPath — even when that ledger's own
// rows are still live, or its trust record needs no rewrite. Discovery must
// not depend on the sweep draining the naming home first: a live session's
// rows name its isolated home while staying protected themselves, and the
// sweep still has to visit that home for OTHER (dead) sessions' rows.
// Rows that predate the per-row config-home field name no home; the daemon
// sweep covers those under the home it is already visiting (the caller
// passes visited=true for that home). It returns the homes the sweep has
// not visited yet, so the daemon can drain session-scoped override homes
// the process home enumeration would never reach. Best-effort: an absent
// or unparseable ledger lists nothing.
func LedgerConfigHomes(trustRecordPath string, visited map[string]struct{}) []string {
	// Locked: this is the daemon sweep's discovery pass — no lock held —
	// and a torn read here would skip an override home for the whole boot
	// (see readTrustLedgerLocked).
	entries, ok := readTrustLedgerLocked(trustRecordPath, true)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range entries {
		home := strings.TrimSpace(e.ConfigHome)
		if home == "" || !filepath.IsAbs(home) {
			continue
		}
		if _, seen := visited[home]; seen {
			continue
		}
		visited[home] = struct{}{}
		out = append(out, home)
	}
	return out
}

// SeededTrustForSweepTest is the observable half of one production seed:
// the trust-record path it wrote. The seed record itself stays private —
// the daemon sweep test never touches it; a crashed session has nobody
// left to clean up, which is the shape under test. Exported for the daemon
// sweep test: it builds an isolated-home crash through the real seed —
// including its process-home discovery link — instead of a hand-shaped
// ledger. The caller sets CLAUDE_CONFIG_DIR (process home) and HOME
// before calling; workdir must be an absolute directory the seed may
// trust. Empty when the seed wrote nothing.
type SeededTrustForSweepTest struct {
	// TrustRecordPath is the trust record the production seed wrote.
	TrustRecordPath string
}

// SeedWorkspaceTrustForSweepTest runs the PRODUCTION seed with a
// session-scoped config-home override: the session env layer names
// overrideHome while the process env (set by the caller) names the
// daemon's own process home. A test that wants the crashed shape simply
// leaves the seed in place — nobody calls the live-path cleanup.
func SeedWorkspaceTrustForSweepTest(workdir, overrideHome, sessionID string) SeededTrustForSweepTest {
	spec := agent.Spec{
		SessionID:   sessionID,
		SessionName: "seed-for-test",
		Cwd:         workdir,
		Env:         map[string]string{claudeConfigDirEnv: overrideHome},
	}
	seed := seedWorkspaceTrust(spec)
	if seed == nil {
		return SeededTrustForSweepTest{}
	}
	return SeededTrustForSweepTest{TrustRecordPath: seed.path}
}

// SweepSeededTrustForConfigHome sweeps the ledger beside the trust record
// under configHome, undoing entries whose session is no longer live. It is
// the daemon startup entry point (see daemon/trust_ledger_sweep.go): config
// homes the daemon never seeds — a relative override, an unset home —
// resolve to nothing and sweep nothing. A home whose ledger is absent or
// foreign is a no-op. It returns the number of ledger rows dropped.
func SweepSeededTrustForConfigHome(configHome string, live func(session string) bool) int {
	if strings.TrimSpace(configHome) == "" || !filepath.IsAbs(configHome) {
		return 0
	}
	path := filepath.Join(configHome, claudeTrustFileName)
	if _, err := os.Stat(trustLedgerPath(path)); err != nil {
		return 0
	}
	return sweepTrustLedger(SweepTrustLedgerOptions{TrustRecordPath: path, Live: live})
}

// trustLedgerSortForTest orders ledger rows for deterministic test
// comparison.
func trustLedgerSortForTest(entries []trustLedgerEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir < entries[j].Dir
		}
		if entries[i].Session != entries[j].Session {
			return entries[i].Session < entries[j].Session
		}
		return !entries[i].Created && entries[j].Created
	})
}
