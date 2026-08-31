package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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

// sweepKindCodexHome / sweepKindAppSocket name the two artifact shapes this
// package creates under os.TempDir(). Each has its OWN set of entries donmai
// declares it created — see declaredDeletableEntries.
const (
	sweepKindCodexHome = "codex-home"
	sweepKindAppSocket = "codex-app-socket"
)

// codexAppSocketFileName is the single file startNamedInteractiveAppServer
// creates inside a codexAppSocketPrefix directory. Shared here for the same
// reason as the prefix above: the sweep's declared-deletable set and the
// file's own creation must use the exact same literal.
const codexAppSocketFileName = "app.sock"

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
//
// There is deliberately no "kind" field: which artifact shape a directory is
// follows from its name prefix, which the sweep must read anyway to decide
// whether the directory is in scope at all. A second, writable copy of that
// fact could only ever disagree with the first.
type donmaiOwnerManifest struct {
	OwnerIdentity sessionshim.ProcessIdentity `json:"ownerIdentity,omitzero"`
	OwnerPID      int                         `json:"ownerPid,omitempty"`
	ChildIdentity sessionshim.ProcessIdentity `json:"childIdentity,omitzero"`
	StartedAt     time.Time                   `json:"startedAt"`
}

// writeDonmaiOwnerManifest records this process as dir's owner. Best-effort:
// a write failure just means a later sweep falls back to the pure age
// heuristic for this one directory (see sweepOne) — it never fails
// construction of the directory it describes.
func writeDonmaiOwnerManifest(dir string) {
	manifest := donmaiOwnerManifest{StartedAt: time.Now().UTC()}
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
// ownership proof readVerifiedDonmaiOwnerManifest requires — safe here ONLY
// because pinDonmaiChildIdentity's caller is reading back a manifest it just
// wrote, under a directory donmai itself created moments ago in the same
// process, never an artifact discovered by a sweep. SweepOrphans itself must
// NEVER call this directly; see readVerifiedDonmaiOwnerManifest.
func readDonmaiOwnerManifestUnchecked(dir string) (donmaiOwnerManifest, bool) {
	body, err := os.ReadFile(filepath.Join(dir, donmaiOwnerManifestName)) //nolint:gosec // G304: dir is always a donmai-owned artifact directory this package itself just created.
	if err != nil {
		return donmaiOwnerManifest{}, false
	}
	return decodeDonmaiOwnerManifest(body)
}

// donmaiOwnerManifestMaxBytes bounds how much of a manifest file the sweep
// will read. A real manifest is a few hundred bytes; the cap only exists so
// a corrupted or adversarial file cannot make the read itself unbounded.
const donmaiOwnerManifestMaxBytes = 64 << 10

// decodeDonmaiOwnerManifest parses manifest bytes, rejecting one that names
// no owner at all — such a manifest can neither prove a directory is still
// in use nor authorize reclaiming it, so it is worth exactly as much as no
// manifest.
func decodeDonmaiOwnerManifest(body []byte) (donmaiOwnerManifest, bool) {
	var manifest donmaiOwnerManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return donmaiOwnerManifest{}, false
	}
	if manifest.OwnerIdentity.PID <= 0 && manifest.OwnerPID <= 0 {
		return donmaiOwnerManifest{}, false
	}
	return manifest, true
}

// readVerifiedDonmaiOwnerManifest is the ONLY entry point SweepOrphans
// itself may use to read a manifest. It returns ok=true only for a manifest
// read through readOwnedManifestBytes, which proves — on inodes it holds
// open, not on names — that both the directory and the manifest inside it
// are owned by this process's own user and grant no group or other access,
// the same rigor config_boundary.go already applies to a live session home
// (rejectSymlink + a pinned parent + 0700 — see
// newCodexConfigBoundaryWithAuthMode).
//
// Without that proof, os.TempDir() being a shared, world-writable directory
// on a typical unix host (ordinary /tmp) would make a manifest an
// unprivileged local kill primitive: any user could mkdir a
// donmai-codex-app-*/donmai-codex-home-* directory, drop a manifest naming
// any PID they want signalled, backdate it, and wait for the next sweep.
//
// ok is false for a directory this process cannot prove it owns, a missing
// manifest, an unreadable/malformed one, or one whose PID fields are all
// non-positive. sweepOne treats every one of those identically — the
// directory is "undeclared": nothing inside it may be read, and nothing
// inside it may be deleted — so callers never need to distinguish the
// reasons.
func (opts SweepOptions) readVerifiedDonmaiOwnerManifest(dir string) (donmaiOwnerManifest, bool) {
	body, err := readOwnedManifestBytes(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			opts.Logger.Warn("codex: orphan sweep will not trust an artifact directory it cannot prove it owns; treating it as having no manifest at all",
				"path", dir, "err", err)
		}
		return donmaiOwnerManifest{}, false
	}
	return decodeDonmaiOwnerManifest(body)
}

// SweepOptions configures SweepOrphans. The zero value is a usable, fully
// production-shaped configuration.
type SweepOptions struct {
	// Root is the directory to scan for donmai-owned artifacts. Empty means
	// os.TempDir() — the same parent newCodexConfigBoundaryWithAuthMode and
	// startNamedInteractiveAppServer use by default.
	Root string
	// MinAge is how old (by directory mtime) an entry must be before the
	// sweep will act on the STRONGEST evidence it can have: a verified
	// manifest whose owner is dead and whose pinned child identity it can
	// check directly. It is deliberately the only floor short enough to
	// matter within one daemon's uptime, and it applies only on that path —
	// every weaker verdict goes through UnverifiedMinAge instead. Empty
	// means 1 hour.
	MinAge time.Duration
	// UnverifiedMinAge is the SEPARATE, much larger age floor required
	// before the sweep will act at all on a directory it cannot fully
	// verify: no manifest at all (every pre-upgrade rollout's still-running
	// sessions look like this the moment a new daemon starts, and a
	// manifest write is best-effort and can fail), a manifest this process
	// cannot prove it owns (see readVerifiedDonmaiOwnerManifest), or a dead
	// owner with no tracked child identity (the PTY session shape — a live
	// orphaned codex process could still be using it, and there is no PID
	// to check at all). A live session's top-level CODEX_HOME mtime stops
	// moving minutes in (writes land in subdirectories), so MinAge alone is
	// not a safe bar for any of these; only reclaiming the directory (never
	// a process) and only past this much longer floor is. Empty means 24
	// hours.
	UnverifiedMinAge time.Duration
	// MaxEntries bounds how many entries one sweep call will ACT on — read
	// from, delete inside, or signal a process for. It deliberately does not
	// count an entry the sweep resolves and then leaves alone.
	//
	// Counting every entry looked at is what made this bound
	// counter-productive in the field. os.ReadDir returns names in sorted
	// order, so on a host carrying thousands of directories the sweep can
	// never change — legacy homes, or ones reduced to state it may not touch
	// — the same lexically-first MaxEntries of them consumed the whole
	// budget on every daemon start, forever, and a reclaimable orphan
	// behind them was never reached. A ceiling that the unreclaimable
	// population monopolises protects nothing. Empty means 500.
	MaxEntries int
	// MaxScan bounds how many in-scope entries one call will look at at all,
	// including the ones it leaves alone. Resolving an entry costs a handful
	// of syscalls, so this can be far larger than MaxEntries and still bound
	// the walk; MaxDuration backstops it. Empty means 20000.
	MaxScan int
	// MaxDuration bounds the TOTAL wall clock one sweep call may spend.
	// MaxEntries bounds how MANY entries are examined and says nothing about
	// how LONG each takes: an entry that reaches terminate costs up to
	// 2 x TerminationGrace, so the entry ceiling alone still admits well over
	// an hour of work on a path documented as "run once, early, on daemon
	// start". Both bounds are required; neither implies the other. Empty
	// means 30s.
	MaxDuration time.Duration
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
	// PartiallyReclaimed counts a directory whose declared-deletable entries
	// were removed but which still holds something donmai never declared it
	// created — resumable session state, or any codex artifact this package
	// has not been taught about. Not a failure: an operator watching this
	// climb over time is the intended signal that retained state is
	// accumulating and its own lifecycle policy (not this sweep) needs to
	// exist. See declaredDeletableEntries.
	PartiallyReclaimed int
	// Acted counts entries that consumed the MaxEntries budget — ones the
	// sweep read from, deleted inside, or signalled a process for. Every
	// other outcome resolves an entry and leaves it alone, and is free.
	Acted int
	// SkippedUnowned counts a non-empty directory this process could not
	// prove it exclusively owns, so NOTHING inside it was read or deleted.
	// On a shared os.TempDir() this is the expected count for another user's
	// donmai-named directory.
	SkippedUnowned int
	// SkippedIrreducible counts a directory of ours holding nothing this
	// sweep may delete — either state donmai never created, or resume keys
	// it is deliberately preserving. Nothing is left to do for it, now or on
	// any future sweep while it stays in that shape: it is free, and it is
	// the count an operator watches to see retained state accumulating.
	SkippedIrreducible int
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
	if opts.MaxScan <= 0 {
		opts.MaxScan = 20000
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = 30 * time.Second
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
//  1. Provenance (verifyOwnedDirectory): the directory must be proven — on
//     a descriptor this process holds open — to be a real directory owned by
//     its own user granting no group or other access. Together with the
//     naming fence that means donmai created it and no one else could have
//     written into it. A directory that fails is read from NEVER (its
//     plugin cache included) and deleted inside NEVER; the most that can
//     happen to it is removal if it is already empty.
//     1a. Liveness (readVerifiedDonmaiOwnerManifest): a manifest, read through
//     the same fd-pinned proof extended to the file itself, says who owns
//     the directory NOW. That is a different question from provenance and it
//     gates a different thing: with a manifest the sweep may act on the
//     short MinAge floor and may signal a process; without one it may still
//     act, but only on the far larger idle floor and never on a process.
//  2. Age: MinAge for a verified-dead owner with a pinned child; the much
//     larger UnverifiedMinAge for anything without that liveness proof (no
//     manifest, or a dead owner with no tracked child). The larger floor is
//     measured across the WHOLE tree, not the top-level mtime — a live
//     session's CODEX_HOME stops moving minutes in, so top-level mtime is
//     the one signal that cannot answer "is this still in use".
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
//  7. Declared deletability (declaredDeletableEntries): even once an entry
//     clears every gate above, the sweep removes only the specific
//     top-level entries donmai's own manifest declares donmai created.
//     Anything else stays, and the directory itself is removed only if
//     nothing was left behind. A cleanup path that decided what to delete
//     by walking the filesystem could not know whether it was deleting a
//     resume key.
//  8. Resume keys (codexSessionStateSubdir): a "codex-home" that still
//     holds session state keeps its config.toml and auth link too — see
//     that constant's doc comment. Process death is the PRECONDITION for
//     resume, so it cannot also be the trigger for deleting what resume
//     needs.
//
// Work is bounded on three axes, because no one of them implies the others:
// opts.MaxEntries caps how many entries one call ACTS on, opts.MaxScan caps
// how many it resolves at all, and opts.MaxDuration caps total wall clock —
// an entry that reaches termination costs real time regardless of how few
// entries there are, and an entry the sweep can never change must not
// consume the action budget at all (see MaxEntries).
func SweepOrphans(ctx context.Context, opts SweepOptions) SweepReport {
	opts = opts.withDefaults()
	var report SweepReport

	// The wall-clock budget is enforced through ctx so that every blocking
	// step below — the per-entry loop AND the termination waits inside it —
	// observes the same deadline. Bounding only the loop would leave a
	// single entry free to burn 2 x TerminationGrace with nothing watching.
	ctx, cancel := context.WithTimeout(ctx, opts.MaxDuration)
	defer cancel()

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
			kind = sweepKindCodexHome
		case strings.HasPrefix(name, codexAppSocketPrefix):
			kind = sweepKindAppSocket
		default:
			continue // outside the fence: never even looked at further.
		}
		if report.Scanned >= opts.MaxScan {
			opts.Logger.Warn("codex: orphan sweep hit its scan ceiling",
				"root", opts.Root, "maxScan", opts.MaxScan)
			break
		}
		if report.Acted >= opts.MaxEntries {
			opts.Logger.Warn("codex: orphan sweep hit its bounded-work ceiling",
				"root", opts.Root, "maxEntries", opts.MaxEntries)
			break
		}
		report.Scanned++
		if opts.sweepOne(ctx, filepath.Join(opts.Root, name), kind, &report) {
			report.Acted++
		}
	}

	opts.Logger.Info("codex: orphan sweep complete",
		"root", opts.Root,
		"scanned", report.Scanned,
		"reclaimed", report.Reclaimed,
		"partiallyReclaimed", report.PartiallyReclaimed,
		"acted", report.Acted,
		"skippedUnowned", report.SkippedUnowned,
		"skippedIrreducible", report.SkippedIrreducible,
		"terminated", report.Terminated,
		"skippedYoung", report.SkippedYoung,
		"skippedLive", report.SkippedLive,
		"skippedAmbiguous", report.SkippedAmbiguous,
		"errors", report.Errors,
	)
	return report
}

// sweepOne resolves one in-scope entry. It reports whether the entry
// consumed the MaxEntries budget — true only when the sweep actually read
// from it, deleted inside it, or signalled a process for it. Every "resolve
// and leave alone" outcome returns false, because a bound the unreclaimable
// population monopolises is not a bound on anything (see MaxEntries).
func (opts SweepOptions) sweepOne(ctx context.Context, path, kind string, report *SweepReport) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// Not a plain directory (or already gone) — never our shape.
		return false
	}
	// PROVENANCE first, and separately from any manifest: a donmai-codex-*
	// name inside a directory this uid exclusively owns at 0700 is proof
	// donmai created it and nothing else could have written into it. Without
	// that proof the sweep reads nothing from it and deletes nothing inside
	// it, no matter what a manifest there might claim.
	if err := verifyOwnedDirectory(path); err != nil {
		opts.Logger.Warn("codex: orphan sweep will not read from or delete inside an artifact directory it cannot prove it exclusively owns",
			"path", path, "kind", kind, "err", err)
		opts.reclaimEmptyDirectoryOnly(path, kind, report)
		return false
	}
	// LIVENESS second, and only from a manifest: who owns this directory
	// NOW. Its presence is what lets the sweep act on the short floor.
	if manifest, ok := opts.readVerifiedDonmaiOwnerManifest(path); ok {
		return opts.sweepManifested(ctx, path, kind, manifest, info, report)
	}
	// No manifest: every session predating it looks like this, and the write
	// is best-effort so it can simply have failed. There is no liveness
	// proof at all, so the much larger idle floor stands in for one — and it
	// is measured across the whole tree, because a live session's top-level
	// mtime stops moving the moment its writes land in subdirectories.
	if !opts.idleLongerThan(path, info, opts.UnverifiedMinAge) {
		report.SkippedYoung++
		return false
	}
	return opts.reclaim(path, kind, report)
}

// sweepManifested handles an entry whose owner manifest this process proved
// it owns — the only path that may ever signal a process.
func (opts SweepOptions) sweepManifested(ctx context.Context, path, kind string, manifest donmaiOwnerManifest, info os.FileInfo, report *SweepReport) bool {
	if opts.ownerAlive(manifest) {
		report.SkippedLive++
		return false
	}
	if manifest.ChildIdentity.PID <= 0 {
		// Owner is gone, but no child was ever tracked for this directory
		// shape (the PTY session case — ptycli exposes no child PID at
		// all). A live orphaned codex process could still be using it;
		// there is no PID to check. Acting on the directory ALONE (never a
		// process) still requires the much larger idle floor.
		if !opts.idleLongerThan(path, info, opts.UnverifiedMinAge) {
			report.SkippedYoung++
			return false
		}
		return opts.reclaim(path, kind, report)
	}
	if opts.now().Sub(info.ModTime()) < opts.MinAge {
		// The strongest-evidence path (a verified manifest with a pinned
		// child identity) is the only one MinAge governs — see its doc
		// comment.
		report.SkippedYoung++
		return false
	}
	alive, err := opts.identityAlive(manifest.ChildIdentity)
	if err != nil {
		report.SkippedAmbiguous++
		opts.Logger.Warn("codex: orphan sweep could not verify a recorded child identity; leaving it alone",
			"path", path, "kind", kind, "pid", manifest.ChildIdentity.PID, "err", err)
		return false
	}
	if !alive {
		// Confirmed: the owner is dead AND the exact child incarnation it
		// started is dead too (identity-verified, not just "PID absent").
		return opts.reclaim(path, kind, report)
	}
	if !opts.processLooksLikeCodex(manifest.ChildIdentity.PID, opts.BinaryHint) {
		// The recorded identity is alive and IS the same incarnation this
		// directory's owner started — but it no longer looks like the
		// configured binary at all. Ambiguous: touch neither the process
		// nor the directory rather than guess.
		report.SkippedAmbiguous++
		opts.Logger.Warn("codex: orphan sweep found a live, identity-matched child that no longer looks like the configured binary; skipping",
			"path", path, "kind", kind, "pid", manifest.ChildIdentity.PID)
		return false
	}
	if !opts.terminate(ctx, manifest.ChildIdentity, path, report) {
		return true // it did signal a process; the directory stays.
	}
	opts.reclaim(path, kind, report)
	return true
}

// sweepDeepScanMaxEntries bounds the per-directory walk idleLongerThan does.
// A real CODEX_HOME holds a few dozen entries; the ceiling only exists so one
// pathological directory cannot make a single sweep entry unbounded.
const sweepDeepScanMaxEntries = 4096

// idleLongerThan reports whether NOTHING anywhere under path has been
// written within floor.
//
// The top-level mtime alone cannot answer this. A live session's CODEX_HOME
// stops moving minutes in, because its writes land in sessions/ and other
// subdirectories — which is exactly why a directory that looks a month stale
// at the top can be in active use. Looking at the whole tree is what makes
// it safe to act on a directory with no manifest to prove liveness for it.
//
// Returns as soon as it finds anything newer than the floor, so the case
// that matters most — a live session — is also the cheapest to resolve.
func (opts SweepOptions) idleLongerThan(path string, info os.FileInfo, floor time.Duration) bool {
	cutoff := opts.now().Add(-floor)
	if info.ModTime().After(cutoff) {
		return false
	}
	idle := true
	seen := 0
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort: an unreadable entry proves nothing about activity.
		}
		seen++
		if seen > sweepDeepScanMaxEntries {
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // same: skip what cannot be stat'd.
		}
		if fi.ModTime().After(cutoff) {
			idle = false
			return fs.SkipAll
		}
		return nil
	})
	return idle
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
// still holds session state is to strip the one thing a resume provably
// does not need — the plugin-cache staging copy, which is re-derivable
// network data and is harvested into the host cache first — and leave
// everything else in place, indefinitely, rather than guess at a retention
// window no timer could honestly justify (a resume request has no natural
// expiry). Do not "simplify" this back to an unconditional RemoveAll
// without confirming that relocation work has landed and this is where it
// wants the cleanup enforced.
//
// "Everything else" deliberately includes config.toml and the linked
// auth.json. An earlier shape of this sweep deleted exactly those two and
// kept only the rollout file, which has the causality backwards: a daemon
// restart makes the previous daemon dead, and "owner dead" is the very
// condition that admits a directory to reclamation — so process death was
// simultaneously the precondition for a resume and the trigger for deleting
// what the resume needs. The retained auth link is a real, honest cost of
// that choice (a credential hard link living in os.TempDir() for as long as
// the session state beside it does); it is retained on purpose, for the
// same reason and with the same expiry as the session state itself, and the
// relocation work above is what removes both.
const codexSessionStateSubdir = "sessions"

// declaredDeletableEntries returns the exact top-level entries donmai itself
// creates inside an artifact directory of this kind — the ONLY things a
// sweep may ever delete from it.
//
// This is an allowlist on purpose. Its predecessor was a preserve-list: it
// deleted every top-level entry except one hardcoded name, which means any
// codex state this package has not been taught about (archived_sessions/,
// history.jsonl, whatever a future codex release adds) was deleted by
// discovery. A cleanup path that decides what to delete by walking the
// filesystem cannot know whether it is deleting a resume key; one that
// deletes only what a manifest declares donmai created can. New state is
// therefore preserved by default and only becomes deletable when this
// function is deliberately taught about it.
//
// cache/ is removed recursively. That is not deletion by discovery: the
// whole subtree is donmai's own staging copy, created and populated by
// plugin_cache.go, and it is harvested into the host-level cache before
// anything is removed.
func declaredDeletableEntries(kind string, hasSessionState bool) map[string]bool {
	switch kind {
	case sweepKindCodexHome:
		deletable := map[string]bool{codexPluginCacheSubdir: true}
		if hasSessionState {
			// A resume key is present: keep config.toml, the auth link, and
			// the manifest that proves who owns this directory. See
			// codexSessionStateSubdir above.
			return deletable
		}
		deletable[codexConfigFileName] = true
		deletable[codexAuthFileName] = true
		deletable[donmaiOwnerManifestName] = true
		return deletable
	case sweepKindAppSocket:
		// A bootstrap app-server directory holds exactly one socket plus
		// the manifest; it never holds session state.
		return map[string]bool{codexAppSocketFileName: true, donmaiOwnerManifestName: true}
	default:
		return nil
	}
}

// reclaim removes the entries declaredDeletableEntries says donmai created
// inside path, and then path itself only if nothing was left behind. Its
// caller has already established provenance (verifyOwnedDirectory) — reading
// from or deleting inside a directory that failed that proof is what B1
// closed, and this function is unreachable for one.
//
// It reports whether the entry consumed the MaxEntries budget. A directory
// already reduced to entries this package never created has nothing left to
// do and is free; see MaxEntries for why that distinction is load-bearing
// rather than a micro-optimisation.
//
// The plugin-cache harvest runs here, before anything is removed: the fetch
// that session paid for is otherwise simply lost, for exactly the sessions
// most likely to need the cache-reuse mechanism (ones that crashed before
// they could call remove()/harvestPluginCache() themselves).
func (opts SweepOptions) reclaim(path, kind string, report *SweepReport) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep could not list an artifact directory", "path", path, "kind", kind, "err", err)
		return true
	}
	hasSessionState, err := dirHasEntries(filepath.Join(path, codexSessionStateSubdir))
	if err != nil {
		// Could not even determine whether resumable state exists. The
		// conservative answer is "assume yes" rather than risk deleting the
		// keys a resume needs.
		hasSessionState = true
	}
	deletable := declaredDeletableEntries(kind, hasSessionState)
	present := 0
	for _, entry := range entries {
		if deletable[entry.Name()] {
			present++
		}
	}
	if present == 0 && len(entries) > 0 {
		// Already reduced to entries this package never created and may not
		// delete. Nothing is left to do for it — now, or on any future
		// sweep. Charging the budget for re-deciding that every time is how
		// a host's retained state crowds out its reclaimable orphans.
		report.SkippedIrreducible++
		opts.Logger.Debug("codex: orphan sweep left an artifact directory holding only state donmai never created",
			"path", path, "kind", kind, "preserved", entryNames(entries))
		return false
	}
	opts.harvestOrphanedPluginCache(path)
	if err := removeDeclaredDeletables(path, deletable); err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep failed to remove the entries donmai declares it created",
			"path", path, "kind", kind, "err", err)
		return true
	}
	remaining, err := os.ReadDir(path)
	if err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep could not list an artifact directory after removing its declared entries",
			"path", path, "kind", kind, "err", err)
		return true
	}
	if len(remaining) > 0 {
		report.PartiallyReclaimed++
		opts.Logger.Info("codex: orphan sweep removed only the entries donmai declares it created and PRESERVED everything else",
			"path", path, "kind", kind, "preserved", entryNames(remaining))
		return true
	}
	if err := os.Remove(path); err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep failed to reclaim", "path", path, "kind", kind, "err", err)
		return true
	}
	report.Reclaimed++
	opts.Logger.Info("codex: orphan sweep reclaimed", "path", path, "kind", kind)
	return true
}

// reclaimEmptyDirectoryOnly is everything the sweep is permitted to do to a
// directory it cannot prove it exclusively owns: a NON-recursive remove,
// which succeeds exactly when the directory is already empty and fails
// harmlessly otherwise. An empty directory is the one case where "delete
// only what donmai can prove donmai created" and "delete the directory"
// cannot conflict — there is nothing inside to lose.
//
// A non-empty one is left completely alone, forever. That is the correct
// answer for a directory belonging to another user (or one this user has
// opened up to others) sitting in a shared os.TempDir(): reclaiming disk is
// never worth deleting contents this process cannot attribute to donmai.
// It is also free — see reclaim on why an outcome nothing will ever come of
// must not consume the scan budget.
func (opts SweepOptions) reclaimEmptyDirectoryOnly(path, kind string, report *SweepReport) {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		report.SkippedUnowned++
		return
	}
	report.Reclaimed++
	opts.Logger.Info("codex: orphan sweep reclaimed an empty artifact directory", "path", path, "kind", kind)
}

// entryNames renders a directory listing for a log line.
func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
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

// removeDeclaredDeletables removes exactly those top-level entries of dir
// whose names appear in deletable, and nothing else. An entry named in
// deletable but absent from dir is simply not there; an entry present in
// dir but absent from deletable is left completely alone.
func removeDeclaredDeletables(dir string, deletable map[string]bool) error {
	if len(deletable) == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !deletable[entry.Name()] {
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
//
// ctx is the sweep's wall-clock budget and is honoured at every step. It has
// to be: sessionshim's identity check reports a ZOMBIE as alive (the process
// table entry survives until someone reaps it, and its start time still
// answers), so a signalled child whose real parent is gone can never be
// confirmed dead here and burns the entire grace window twice. Multiply that
// by MaxEntries and a "run once, early, on daemon start" path becomes over
// an hour of blocking work. Giving up on the budget is the right answer:
// nothing is reclaimed and nothing is claimed to have been killed.
func (opts SweepOptions) terminate(ctx context.Context, identity sessionshim.ProcessIdentity, path string, report *SweepReport) bool {
	if ctx.Err() != nil {
		return false
	}
	process, err := os.FindProcess(identity.PID)
	if err != nil {
		report.Errors++
		return false
	}
	_ = process.Signal(syscallSIGTERM())
	if opts.awaitDeath(ctx, identity, opts.TerminationGrace) {
		report.Terminated++
		opts.Logger.Info("codex: orphan sweep terminated an orphaned app-server", "pid", identity.PID, "path", path)
		return true
	}
	if ctx.Err() != nil {
		opts.Logger.Warn("codex: orphan sweep ran out of its wall-clock budget while waiting for an orphaned app-server to exit; leaving it and its directory alone",
			"pid", identity.PID, "path", path, "reason", ctx.Err())
		return false
	}
	_ = process.Kill()
	if opts.awaitDeath(ctx, identity, opts.TerminationGrace) {
		report.Terminated++
		opts.Logger.Warn("codex: orphan sweep force-killed an orphaned app-server after its grace window expired",
			"pid", identity.PID, "path", path)
		return true
	}
	if ctx.Err() != nil {
		opts.Logger.Warn("codex: orphan sweep ran out of its wall-clock budget while confirming an orphaned app-server's death; leaving its directory alone",
			"pid", identity.PID, "path", path, "reason", ctx.Err())
		return false
	}
	report.Errors++
	opts.Logger.Warn("codex: orphan sweep could not confirm an orphaned app-server was terminated; leaving its directory in place",
		"pid", identity.PID, "path", path)
	return false
}

// awaitDeath polls identity's liveness until it reports dead, grace elapses,
// or the sweep's wall-clock budget runs out — whichever comes first.
func (opts SweepOptions) awaitDeath(ctx context.Context, identity sessionshim.ProcessIdentity, grace time.Duration) bool {
	deadline := opts.now().Add(grace)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if alive, err := opts.identityAlive(identity); err == nil && !alive {
			return true
		}
		if !opts.now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
