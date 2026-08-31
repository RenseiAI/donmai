package codex

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// codexAppSocketPrefix names the Unix-socket directory
// startNamedInteractiveAppServer creates for one bootstrap app-server (see
// interactive_name.go). Shared here so the sweep's donmai-owned fence and
// the directory's own creation use the exact same literal.
const codexAppSocketPrefix = "donmai-codex-app-"

// donmaiOwnerManifestName is the one file this package ever writes inside a
// donmai-owned artifact directory purely for the sweep's benefit. It is
// metadata only (a PID, an OS-reported start time, and a timestamp) — never
// a credential, never session content — and its removal happens for free
// whenever the directory it lives in is reclaimed or normally cleaned up.
const donmaiOwnerManifestName = ".donmai-owner.json"

// donmaiOwnerManifest records who is responsible for a donmai-owned
// artifact directory, so a LATER process (a sweep running in a fresh daemon
// after a restart) can tell a live session apart from an orphan without
// guessing from age alone.
//
//   - OwnerIdentity pins the donmai process that created the directory (the
//     one whose in-memory Handle/*codexConfigBoundary owns cleaning it up).
//     If that process is gone, nothing will EVER call remove()/close() for
//     this directory again — it is now the sweep's sole responsibility.
//     Pairing the PID with its OS-reported start time (sessionshim's own
//     anti-reuse primitive — see sessionshim/procid.go) is what lets the
//     sweep tell "the owner is still running" apart from "a PID it reused
//     belongs to something else now"; a bare PID cannot make that
//     distinction. OwnerPID is a same-information fallback for a platform
//     where identity pinning is unavailable (see sessionshim's
//     procid_other.go) — liveness read from a bare PID can still be WRONG
//     in the reused-PID direction, but the consequence here is only ever a
//     missed reclaim, never a wrongful kill (see ownerAlive).
//   - ChildIdentity pins the codex subprocess this directory's owner
//     started under it, when one is knowable at write time (the headless
//     app-server in startLocked, the named bootstrap app-server in
//     startNamedInteractiveAppServer) — pinned via pinDonmaiChildIdentity
//     immediately after the child's own cmd.Start(), the only moment its
//     start time is still readable. Zero (PID<=0) when no child was ever
//     tracked for this directory shape (ptycli's PTY driver exposes no
//     child PID at all — see its Spawn doc comment) or identity pinning
//     failed/is unavailable on this platform. There is deliberately NO
//     bare-PID fallback for a child: unlike the owner, a wrong answer here
//     gates an actual SIGTERM/SIGKILL, so SweepOrphans never signals a
//     process it cannot prove, via a verified identity match, that donmai
//     itself started under this exact directory.
type donmaiOwnerManifest struct {
	OwnerIdentity sessionshim.ProcessIdentity `json:"ownerIdentity,omitzero"`
	OwnerPID      int                         `json:"ownerPid,omitempty"`
	ChildIdentity sessionshim.ProcessIdentity `json:"childIdentity,omitzero"`
	StartedAt     time.Time                   `json:"startedAt"`
	Kind          string                      `json:"kind"`
}

// writeDonmaiOwnerManifest records this process as dir's owner. Best-effort:
// a write failure just means a later sweep falls back to the pure age
// heuristic for this one directory (see sweepOne) — it never fails
// construction of the directory it describes.
func writeDonmaiOwnerManifest(dir, kind string) {
	manifest := donmaiOwnerManifest{StartedAt: time.Now().UTC(), Kind: kind}
	if identity, err := sessionshim.Self(); err == nil {
		manifest.OwnerIdentity = identity
	} else {
		manifest.OwnerPID = os.Getpid()
	}
	persistDonmaiOwnerManifest(dir, manifest)
}

// pinDonmaiChildIdentity is the single call site codex.go's startLocked and
// interactive_name.go's startNamedInteractiveAppServer use immediately
// after their own cmd.Start() succeeds — the only moment a child's OS-
// reported start time is still readable (see sessionshim.ProcessIdentityFor's
// doc comment). Best-effort: a pinning failure (identity unavailable on this
// platform, or the vanishingly unlikely race where the child has already
// exited by the time this runs) just means SweepOrphans can never verify —
// and therefore never terminates — this directory's child. That is the safe
// direction: see donmaiOwnerManifest's doc comment on why ChildIdentity has
// no bare-PID fallback the way OwnerIdentity does.
func pinDonmaiChildIdentity(dir string, pid int) {
	identity, err := sessionshim.ProcessIdentityFor(pid)
	if err != nil {
		return
	}
	manifest, ok := readDonmaiOwnerManifestUnchecked(dir)
	if !ok {
		manifest = donmaiOwnerManifest{StartedAt: time.Now().UTC()}
		if self, selfErr := sessionshim.Self(); selfErr == nil {
			manifest.OwnerIdentity = self
		} else {
			manifest.OwnerPID = os.Getpid()
		}
	}
	manifest.ChildIdentity = identity
	persistDonmaiOwnerManifest(dir, manifest)
}

func persistDonmaiOwnerManifest(dir string, manifest donmaiOwnerManifest) {
	body, err := json.Marshal(manifest)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, donmaiOwnerManifestName), body, 0o600)
}

// readDonmaiOwnerManifestUnchecked reads dir's manifest WITHOUT the
// directory-ownership verification readDonmaiOwnerManifest applies (see
// verifyManifestDirectoryOwnership) — safe here ONLY because
// pinDonmaiChildIdentity's caller is reading back a manifest it just wrote,
// under a directory donmai itself created moments ago in the same process,
// never an artifact discovered by a sweep. SweepOrphans itself must NEVER
// call this directly; see readDonmaiOwnerManifest.
func readDonmaiOwnerManifestUnchecked(dir string) (donmaiOwnerManifest, bool) {
	body, err := os.ReadFile(filepath.Join(dir, donmaiOwnerManifestName)) //nolint:gosec // G304: dir is always a donmai-owned artifact directory this package itself just created.
	if err != nil {
		return donmaiOwnerManifest{}, false
	}
	var manifest donmaiOwnerManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return donmaiOwnerManifest{}, false
	}
	if manifest.OwnerIdentity.PID <= 0 && manifest.OwnerPID <= 0 {
		return donmaiOwnerManifest{}, false
	}
	return manifest, true
}

// verifyManifestDirectoryOwnership requires dir to be owned by this process's
// own user and not writable by group or other — the same rigor
// config_boundary.go already applies to the isolated session home itself
// (rejectSymlink + a pinned parent + 0700 — see newCodexConfigBoundaryWithAuthMode).
//
// Without this, os.TempDir() being a shared, world-writable directory on a
// typical unix host (ordinary /tmp) makes an unverified manifest an
// unprivileged local kill primitive: any user could mkdir a
// donmai-codex-app-*/donmai-codex-home-* directory, drop a manifest naming
// any PID they want signalled, backdate it, and wait for the next sweep.
// info is the caller's own Lstat of dir, reused rather than re-stat'd so
// there is exactly one stat between "this is the directory the sweep is
// examining" and "this is the directory whose ownership was verified".
func verifyManifestDirectoryOwnership(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("not a plain directory")
	}
	return verifyManifestDirectoryOwnershipOS(info)
}

// readDonmaiOwnerManifest is the ONLY entry point SweepOrphans itself may use
// to read a manifest: it refuses to trust one at all unless
// verifyManifestDirectoryOwnership passes first. ok is false for a
// non-donmai-owned directory, a missing file, an unreadable/malformed one,
// or one whose PID fields are all non-positive — sweepOne treats every one
// of those identically (fall back to the unverified-age heuristic), so
// callers never need to distinguish the reasons.
func readDonmaiOwnerManifest(dir string, info os.FileInfo) (donmaiOwnerManifest, bool) {
	if err := verifyManifestDirectoryOwnership(info); err != nil {
		slog.Warn("codex: orphan sweep found an artifact directory it does not own; treating its manifest as unverifiable",
			"path", dir, "err", err)
		return donmaiOwnerManifest{}, false
	}
	return readDonmaiOwnerManifestUnchecked(dir)
}

// SweepOptions configures SweepOrphans. The zero value is a usable, fully
// production-shaped configuration.
type SweepOptions struct {
	// Root is the directory to scan for donmai-owned artifacts. Empty means
	// os.TempDir() — the same parent newCodexConfigBoundaryWithAuthMode and
	// startNamedInteractiveAppServer use by default.
	Root string
	// MinAge is how old (by directory mtime) an entry with a verified,
	// dead owner must be before the sweep will reclaim it. Empty means 1
	// hour.
	MinAge time.Duration
	// UnverifiedMinAge is the SEPARATE, much larger age floor required
	// before the sweep will reclaim a directory it cannot fully verify:
	// no manifest at all (every pre-upgrade rollout's still-running
	// sessions look like this the moment a new daemon starts, and a
	// manifest write is best-effort and can fail), an unowned manifest (see
	// verifyManifestDirectoryOwnership), or a dead owner with no tracked
	// child identity (the PTY session shape — a live orphaned codex process
	// could still be using it, and there is no PID to check at all). A live
	// session's top-level CODEX_HOME mtime stops moving minutes in (writes
	// land in subdirectories), so MinAge alone is not a safe bar for any of
	// these; only reclaiming the directory (never a process) and only past
	// this much longer floor is. Empty means 24 hours.
	UnverifiedMinAge time.Duration
	// MaxEntries bounds how many donmai-named entries one sweep call will
	// examine, so a pathologically large temp directory cannot turn daemon
	// startup into unbounded filesystem/process work. Empty means 500.
	MaxEntries int
	// TerminationGrace bounds how long the sweep waits after SIGTERM (and
	// again after SIGKILL) before giving up on confirming a live orphaned
	// app-server process actually died. Empty means 5s, matching
	// namedInteractiveAppServer.close's own escalation window.
	TerminationGrace time.Duration
	// BinaryHint names the codex binary the sweep expects a live orphaned
	// process to be running — an extra, independent check (on top of the
	// verified process identity) before it will ever terminate anything.
	// Empty means "codex".
	BinaryHint string
	// Logger receives one structured line per entry examined plus one
	// summary line. Empty means slog.Default().
	Logger *slog.Logger
	// PluginCacheDir is the host-level warm cache reclaim/harvestOrphanedPluginCache harvests
	// an orphaned session's own cache/ subtree into (see plugin_cache.go)
	// before its scratch is removed — the fetch that session paid for is
	// otherwise simply discarded, for exactly the sessions most likely to
	// need the cache-reuse mechanism (ones that crashed before they could
	// call remove()/harvestPluginCache() themselves). Empty means the same
	// default resolveCodexPluginCacheDir("") gives every boundary.
	PluginCacheDir string

	// processAlive / processLooksLikeCodex / identityAlive / now are test
	// seams; production leaves them nil and gets the real implementations.
	processAlive          func(pid int) bool
	processLooksLikeCodex func(pid int, binaryHint string) bool
	identityAlive         func(identity sessionshim.ProcessIdentity) (bool, error)
	now                   func() time.Time
}

// SweepReport summarizes one SweepOrphans call for the caller's own
// logging/metrics; every field is also emitted as a structured field on the
// summary log line SweepOrphans itself writes.
type SweepReport struct {
	Scanned   int
	Reclaimed int
	// PartiallyReclaimed counts a "codex-home" directory whose scratch was
	// removed but whose sessions/ subdirectory was deliberately left in
	// place — see codexSessionStateSubdir's doc comment. Not a failure: an
	// operator watching this climb over time is the intended signal that
	// retained session state is accumulating and its own lifecycle policy
	// (not this sweep) needs to exist.
	PartiallyReclaimed int
	Terminated         int
	SkippedYoung       int // MinAge or UnverifiedMinAge gate — "too young to judge yet", never a liveness signal.
	SkippedLive        int // a verified-or-fallback-live owner (or, historically, child) — "still owned", not age.
	SkippedAmbiguous   int // a live PID that failed identity or binary-identity verification — never touched.
	Errors             int
}

func (opts SweepOptions) withDefaults() SweepOptions {
	if opts.Root == "" {
		opts.Root = os.TempDir()
	}
	if opts.MinAge <= 0 {
		opts.MinAge = time.Hour
	}
	if opts.UnverifiedMinAge <= 0 {
		opts.UnverifiedMinAge = 24 * time.Hour
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 500
	}
	if opts.TerminationGrace <= 0 {
		opts.TerminationGrace = 5 * time.Second
	}
	if opts.BinaryHint == "" {
		opts.BinaryHint = "codex"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.processAlive == nil {
		opts.processAlive = processAliveOS
	}
	if opts.processLooksLikeCodex == nil {
		opts.processLooksLikeCodex = processLooksLikeCodexOS
	}
	if opts.identityAlive == nil {
		opts.identityAlive = sessionshim.ProcessIdentity.Alive
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return opts
}

// SweepOrphans reclaims donmai-created Codex artifacts a prior process left
// behind without cleaning up — a crash, a SIGKILL, a daemon restart mid-
// session. Intended to run once, early, on daemon start.
//
// Safety fence, load-bearing: this function NEVER inspects, removes, or
// signals anything outside opts.Root whose name does not start with
// codexHomePrefix ("donmai-codex-home-") or codexAppSocketPrefix
// ("donmai-codex-app-") — the real ~/.codex (or any other ambient Codex
// state) is never a directory with either prefix, so it is never a
// candidate at all, let alone examined.
//
// Within that fence, every decision funnels through independent gates, and
// any one of them failing leaves the entry (and any process it might name)
// untouched:
//
//  1. Ownership (verifyManifestDirectoryOwnership): a manifest is trusted
//     only when its directory is owned by this process's own user and not
//     writable by group or other. Anything else is treated exactly like
//     "no manifest at all" — never read, let alone acted on.
//  2. Age: MinAge for a verified-dead owner; the much larger
//     UnverifiedMinAge for anything the sweep cannot fully verify (no
//     manifest, an unowned one, or a dead owner with no tracked child) —
//     see UnverifiedMinAge's doc comment for why a short bar is unsafe
//     there.
//  3. Owner liveness: a manifest naming a still-running owner (by verified
//     identity, or by bare PID where identity pinning was unavailable) is
//     never touched — that owner's own in-memory Handle/boundary may yet
//     clean it up itself.
//  4. Child identity (sessionshim.ProcessIdentity, PID + OS-reported start
//     time): termination requires a LIVE, MATCHING identity — a PID that
//     merely exists is not enough, because PID reuse on a host churning
//     thousands of codex spawns is not a corner case. A manifest whose
//     child was never pinned, or whose pinned identity is no longer alive
//     under that exact PID+start-time pairing, is never signalled.
//  5. Binary identity: even a live, identity-matched child must
//     independently look like the configured codex binary
//     (opts.BinaryHint, via `ps`) before the sweep will ever terminate it.
//  6. Confirmed termination: SIGTERM, then SIGKILL, each followed by a
//     liveness re-probe — reclaiming a directory (or reporting a kill) is
//     never claimed on an unconfirmed signal.
//  7. Resumable session state (codexSessionStateSubdir): even once a
//     "codex-home" directory clears every gate above, a non-empty
//     sessions/ subdirectory is NEVER deleted — see that constant's doc
//     comment. Only scratch around it is removed, reported as
//     PartiallyReclaimed rather than Reclaimed.
//
// Work is bounded by opts.MaxEntries so a pathological temp directory
// cannot turn daemon startup into unbounded filesystem or process work.
func SweepOrphans(ctx context.Context, opts SweepOptions) SweepReport {
	opts = opts.withDefaults()
	var report SweepReport

	entries, err := os.ReadDir(opts.Root)
	if err != nil {
		opts.Logger.Warn("codex: orphan sweep could not list root", "root", opts.Root, "err", err)
		return report
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			opts.Logger.Warn("codex: orphan sweep stopped early", "reason", ctx.Err())
			break
		}
		name := entry.Name()
		var kind string
		switch {
		case strings.HasPrefix(name, codexHomePrefix):
			kind = "codex-home"
		case strings.HasPrefix(name, codexAppSocketPrefix):
			kind = "codex-app-socket"
		default:
			continue // outside the fence: never even looked at further.
		}
		if report.Scanned >= opts.MaxEntries {
			opts.Logger.Warn("codex: orphan sweep hit its bounded-work ceiling",
				"root", opts.Root, "maxEntries", opts.MaxEntries)
			break
		}
		report.Scanned++
		opts.sweepOne(filepath.Join(opts.Root, name), kind, &report)
	}

	opts.Logger.Info("codex: orphan sweep complete",
		"root", opts.Root,
		"scanned", report.Scanned,
		"reclaimed", report.Reclaimed,
		"partiallyReclaimed", report.PartiallyReclaimed,
		"terminated", report.Terminated,
		"skippedYoung", report.SkippedYoung,
		"skippedLive", report.SkippedLive,
		"skippedAmbiguous", report.SkippedAmbiguous,
		"errors", report.Errors,
	)
	return report
}

func (opts SweepOptions) sweepOne(path, kind string, report *SweepReport) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// Not a plain directory (or already gone) — never our shape.
		return
	}
	if opts.now().Sub(info.ModTime()) < opts.MinAge {
		report.SkippedYoung++
		return
	}
	manifest, hasManifest := readDonmaiOwnerManifest(path, info)
	if !hasManifest {
		opts.reclaimUnverified(path, kind, info, report)
		return
	}
	if opts.ownerAlive(manifest) {
		report.SkippedLive++
		return
	}
	if manifest.ChildIdentity.PID <= 0 {
		// Owner is gone, but no child was ever tracked for this directory
		// shape (the PTY session case — ptycli exposes no child PID at
		// all). A live orphaned codex process could still be using it;
		// there is no PID to check. Reclaiming the directory ALONE (never
		// a process) still requires the much larger age floor.
		opts.reclaimUnverified(path, kind, info, report)
		return
	}
	alive, err := opts.identityAlive(manifest.ChildIdentity)
	if err != nil {
		report.SkippedAmbiguous++
		opts.Logger.Warn("codex: orphan sweep could not verify a recorded child identity; leaving it alone",
			"path", path, "kind", kind, "pid", manifest.ChildIdentity.PID, "err", err)
		return
	}
	if !alive {
		// Confirmed: the owner is dead AND the exact child incarnation it
		// started is dead too (identity-verified, not just "PID absent").
		opts.reclaim(path, kind, report)
		return
	}
	if !opts.processLooksLikeCodex(manifest.ChildIdentity.PID, opts.BinaryHint) {
		// The recorded identity is alive and IS the same incarnation this
		// directory's owner started — but it no longer looks like the
		// configured binary at all. Ambiguous: touch neither the process
		// nor the directory rather than guess.
		report.SkippedAmbiguous++
		opts.Logger.Warn("codex: orphan sweep found a live, identity-matched child that no longer looks like the configured binary; skipping",
			"path", path, "kind", kind, "pid", manifest.ChildIdentity.PID)
		return
	}
	if !opts.terminate(manifest.ChildIdentity, path, report) {
		return // termination could not be confirmed; its directory stays too.
	}
	opts.reclaim(path, kind, report)
}

// reclaimUnverified applies UnverifiedMinAge — the separate, much larger age
// floor — before reclaiming a directory the sweep could not fully verify
// (see UnverifiedMinAge's doc comment). Never touches a process.
func (opts SweepOptions) reclaimUnverified(path, kind string, info os.FileInfo, report *SweepReport) {
	if opts.now().Sub(info.ModTime()) < opts.UnverifiedMinAge {
		report.SkippedYoung++
		return
	}
	opts.reclaim(path, kind, report)
}

// ownerAlive reports whether manifest's owner is still running. A pinned
// identity is checked for an exact live match (PID reuse reported as NOT
// alive — the safe direction for a read that only ever gates a SKIP, never
// a kill); a bare-PID fallback (identity pinning unavailable on this
// platform) accepts the same reuse risk bare-PID checks always have, which
// is acceptable ONLY because the consequence here is a missed reclaim, never
// a wrongful termination.
func (opts SweepOptions) ownerAlive(manifest donmaiOwnerManifest) bool {
	if manifest.OwnerIdentity.PID > 0 {
		if alive, err := opts.identityAlive(manifest.OwnerIdentity); err == nil {
			return alive
		}
		// Identity check errored (not "confirmed dead" — Alive() reports
		// that as false, nil): fall through to the bare-PID probe rather
		// than treat an error as proof of death.
	}
	if manifest.OwnerPID > 0 {
		return opts.processAlive(manifest.OwnerPID)
	}
	return false
}

// codexSessionStateSubdir is the ONE CODEX_HOME subdirectory SweepOrphans
// will NEVER delete as part of directory reclamation, no matter how old the
// surrounding directory is or how confidently its owner and child are
// proven dead.
//
// LOAD-BEARING CONSTRAINT — read before changing this:
//
// A real codex-cli persists a named thread's rollout-*.jsonl file under
// CODEX_HOME/sessions/<date>/ (see interactive_name.go's
// isRolloutFlushRaceError / codexRolloutFlushRaceMessage fixture for the
// exact shape observed against a real codex-cli release, and that file's
// package doc comment on why a thread that never took a turn cannot be
// reattached at all). This is what codex's OWN native `resume` is keyed
// on: once a thread has taken a turn, that file — not the process, not the
// donmai session that spawned it — is what makes the session durable, for
// as long as a user or a future resume feature might want to resume it.
// Deleting it out from under a dead-but-resumable session is not an orphan
// cleanup, it is silently destroying product data, hours after the session
// ended, which is precisely when someone would go looking for it.
//
// This is a deliberately conservative bridge, not a final design: a
// separate, already-planned initiative to move every harness's session
// state out of these ephemeral, process-scoped directories into a
// purpose-built, lifecycle-bound location (owned by session/worktree
// teardown, not by this process-liveness-based sweep) will eventually give
// this content a proper home outside os.TempDir() entirely. Any future
// change to this retention policy must land consistent with wherever that
// work puts session state — check for it before changing this constant.
// Until it lands, the sweep's only safe move for a home directory that
// still holds session state is reclaimSweepScratch below: strip everything
// ELSE (config.toml, the linked auth.json, the plugin-cache staging copy,
// the owner manifest itself) and leave sessions/ — and therefore the
// directory itself — in place, indefinitely, rather than guess at a
// retention window no timer could honestly justify (a resume request has
// no natural expiry). Do not "simplify" this back to an unconditional
// RemoveAll without confirming that relocation work has landed and this is
// where it wants the cleanup enforced.
const codexSessionStateSubdir = "sessions"

// reclaim removes path, UNLESS it still holds resumable session state (see
// codexSessionStateSubdir), in which case it strips everything else and
// preserves that subdirectory instead of deleting path outright. Only
// codex-home entries can ever hold session state at all — a
// codex-app-socket bootstrap directory contains nothing but a Unix socket
// and is always removed outright. Either way, whatever the directory's own
// cache/ subtree holds is harvested into the host-level plugin cache first
// (see harvestOrphanedPluginCache) — the fetch that session paid for is
// otherwise simply lost, for exactly the sessions most likely to need the
// cache-reuse mechanism (ones that crashed before they could call
// remove()/harvestPluginCache() themselves).
func (opts SweepOptions) reclaim(path, kind string, report *SweepReport) {
	opts.harvestOrphanedPluginCache(path)
	hasSessionState, err := dirHasEntries(filepath.Join(path, codexSessionStateSubdir))
	if err != nil {
		// Could not even determine whether resumable state exists. The
		// conservative answer is "assume yes" rather than risk deleting it.
		hasSessionState = true
	}
	if hasSessionState {
		if err := reclaimSweepScratch(path, codexSessionStateSubdir); err != nil {
			report.Errors++
			opts.Logger.Warn("codex: orphan sweep failed to reclaim scratch around preserved session state",
				"path", path, "kind", kind, "err", err)
			return
		}
		report.PartiallyReclaimed++
		opts.Logger.Info("codex: orphan sweep reclaimed scratch but PRESERVED resumable session state",
			"path", path, "kind", kind, "preserved", filepath.Join(path, codexSessionStateSubdir))
		return
	}
	if err := os.RemoveAll(path); err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep failed to reclaim", "path", path, "kind", kind, "err", err)
		return
	}
	report.Reclaimed++
	opts.Logger.Info("codex: orphan sweep reclaimed", "path", path, "kind", kind)
}

// harvestOrphanedPluginCache copies back whatever an orphaned directory's
// own cache/ subtree holds into the host-level warm cache (see
// plugin_cache.go's reuseCacheTree) before that directory is reclaimed —
// mirrors codexConfigBoundary.harvestPluginCache, but reachable from a
// SEPARATE sweep process that never held the original in-memory boundary
// object at all. A no-op (via reuseCacheTree's own missing-source handling)
// for a codex-app-socket directory, which never has a cache/ subtree.
func (opts SweepOptions) harvestOrphanedPluginCache(path string) {
	if codexPluginCacheDisabled() {
		return
	}
	src := filepath.Join(path, codexPluginCacheSubdir)
	dst := resolveCodexPluginCacheDir(opts.PluginCacheDir)
	if err := reuseCacheTree(src, dst); err != nil {
		opts.Logger.Debug("codex: orphan sweep plugin-cache harvest skipped", "path", path, "err", err)
	}
}

// dirHasEntries reports whether dir exists and contains at least one entry.
// A missing dir is "no entries", not an error — the ordinary case for a
// codex-app-socket directory (which never has a sessions/ subdirectory at
// all) and for a codex-home directory whose session never got as far as a
// named, turned thread.
func dirHasEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

// reclaimSweepScratch removes every top-level entry of dir except preserve.
func reclaimSweepScratch(dir, preserve string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.Name() == preserve {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// terminate sends the escalating SIGTERM→SIGKILL sequence this package uses
// everywhere else (see namedInteractiveAppServer.close) to a proven-orphaned,
// identity-verified codex child, re-probing liveness after EACH signal
// before reporting anything: a bare Kill() call can itself fail silently
// (EPERM, a process the OS will not let this one signal), and reporting a
// kill that was never confirmed would make the summary log lie. Returns
// whether termination was confirmed — sweepOne only reclaims the directory
// when this is true, since an unconfirmed-dead process could still be using
// it.
func (opts SweepOptions) terminate(identity sessionshim.ProcessIdentity, path string, report *SweepReport) bool {
	process, err := os.FindProcess(identity.PID)
	if err != nil {
		report.Errors++
		return false
	}
	_ = process.Signal(syscallSIGTERM())
	if opts.awaitDeath(identity, opts.TerminationGrace) {
		report.Terminated++
		opts.Logger.Info("codex: orphan sweep terminated an orphaned app-server", "pid", identity.PID, "path", path)
		return true
	}
	_ = process.Kill()
	if opts.awaitDeath(identity, opts.TerminationGrace) {
		report.Terminated++
		opts.Logger.Warn("codex: orphan sweep force-killed an orphaned app-server after its grace window expired",
			"pid", identity.PID, "path", path)
		return true
	}
	report.Errors++
	opts.Logger.Warn("codex: orphan sweep could not confirm an orphaned app-server was terminated; leaving its directory in place",
		"pid", identity.PID, "path", path)
	return false
}

// awaitDeath polls identity's liveness until it reports dead or grace
// elapses.
func (opts SweepOptions) awaitDeath(identity sessionshim.ProcessIdentity, grace time.Duration) bool {
	deadline := opts.now().Add(grace)
	for {
		if alive, err := opts.identityAlive(identity); err == nil && !alive {
			return true
		}
		if !opts.now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
