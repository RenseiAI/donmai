package codex

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexAppSocketPrefix names the Unix-socket directory
// startNamedInteractiveAppServer creates for one bootstrap app-server (see
// interactive_name.go). Shared here so the sweep's donmai-owned fence and
// the directory's own creation use the exact same literal.
const codexAppSocketPrefix = "donmai-codex-app-"

// donmaiOwnerManifestName is the one file this package ever writes inside a
// donmai-owned artifact directory purely for the sweep's benefit. It is
// metadata only (a PID and a timestamp) — never a credential, never
// session content — and its removal happens for free whenever the
// directory it lives in is reclaimed or normally cleaned up.
const donmaiOwnerManifestName = ".donmai-owner.json"

// donmaiOwnerManifest records who is responsible for a donmai-owned
// artifact directory, so a LATER process (a sweep running in a fresh daemon
// after a restart) can tell a live session apart from an orphan without
// guessing from age alone.
//
//   - OwnerPID is the donmai process that created the directory (the one
//     whose in-memory Handle/*codexConfigBoundary owns cleaning it up). If
//     that process is gone, nothing will EVER call remove()/close() for
//     this directory again — it is now the sweep's sole responsibility.
//   - ChildPID is the codex subprocess PID this directory's owner started
//     under it, when one is knowable at write time (the headless app-server
//     in startLocked, the named bootstrap app-server in
//     startNamedInteractiveAppServer). Zero when not applicable — ptycli's
//     PTY driver deliberately exposes no child PID at all (see
//     ptycli.Spawn's doc comment), so a plain interactive PTY session's home
//     directory carries OwnerPID only.
type donmaiOwnerManifest struct {
	OwnerPID  int       `json:"ownerPid"`
	ChildPID  int       `json:"childPid,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	Kind      string    `json:"kind"`
}

// writeDonmaiOwnerManifest records this process as dir's owner. Best-effort:
// a write failure just means a later sweep falls back to the pure age
// heuristic for this one directory (see sweepOne) — it never fails
// construction of the directory it describes.
func writeDonmaiOwnerManifest(dir, kind string) {
	writeDonmaiOwnerManifestManifest(dir, donmaiOwnerManifest{
		OwnerPID:  os.Getpid(),
		StartedAt: time.Now().UTC(),
		Kind:      kind,
	})
}

// updateDonmaiOwnerManifestChildPID re-reads dir's manifest (falling back to
// a fresh one for this process if none exists yet) and records childPID —
// called once the caller's own codex subprocess has actually started, so
// the sweep can later distinguish "still owned by a live donmai process"
// from "the live process is gone but its own codex child kept running."
func updateDonmaiOwnerManifestChildPID(dir string, childPID int) {
	manifest, ok := readDonmaiOwnerManifest(dir)
	if !ok {
		manifest = donmaiOwnerManifest{OwnerPID: os.Getpid(), StartedAt: time.Now().UTC()}
	}
	manifest.ChildPID = childPID
	writeDonmaiOwnerManifestManifest(dir, manifest)
}

func writeDonmaiOwnerManifestManifest(dir string, manifest donmaiOwnerManifest) {
	body, err := json.Marshal(manifest)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, donmaiOwnerManifestName), body, 0o600)
}

// readDonmaiOwnerManifest reads dir's owner manifest. ok is false for a
// missing, unreadable, or malformed manifest — every one of those is
// treated identically by the sweep (fall back to the pure age heuristic),
// so callers never need to distinguish the reasons.
func readDonmaiOwnerManifest(dir string) (donmaiOwnerManifest, bool) {
	body, err := os.ReadFile(filepath.Join(dir, donmaiOwnerManifestName)) //nolint:gosec // G304: dir is always a donmai-owned artifact directory this package itself created.
	if err != nil {
		return donmaiOwnerManifest{}, false
	}
	var manifest donmaiOwnerManifest
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.OwnerPID <= 0 {
		return donmaiOwnerManifest{}, false
	}
	return manifest, true
}

// SweepOptions configures SweepOrphans. The zero value is a usable, fully
// production-shaped configuration.
type SweepOptions struct {
	// Root is the directory to scan for donmai-owned artifacts. Empty means
	// os.TempDir() — the same parent newCodexConfigBoundaryWithAuthMode and
	// startNamedInteractiveAppServer use by default.
	Root string
	// MinAge is how old (by directory mtime) an entry must be before the
	// sweep will even consider it — the first, unconditional line of
	// defense against touching a session that only just started. Empty
	// means 1 hour.
	MinAge time.Duration
	// MaxEntries bounds how many donmai-named entries one sweep call will
	// examine, so a pathologically large temp directory cannot turn daemon
	// startup into unbounded filesystem/process work. Empty means 500.
	MaxEntries int
	// TerminationGrace bounds how long the sweep waits after SIGTERM before
	// escalating to SIGKILL for a live orphaned app-server process. Empty
	// means 5s, matching namedInteractiveAppServer.close's own escalation
	// window.
	TerminationGrace time.Duration
	// BinaryHint names the codex binary the sweep expects a live orphaned
	// process to be running, for the process-identity check before it will
	// ever terminate anything. Empty means "codex".
	BinaryHint string
	// Logger receives one structured line per entry examined plus one
	// summary line. Empty means slog.Default().
	Logger *slog.Logger

	// processAlive / processLooksLikeCodex / now are test seams; production
	// leaves them nil and gets the real, platform-specific implementations.
	processAlive          func(pid int) bool
	processLooksLikeCodex func(pid int, binaryHint string) bool
	now                   func() time.Time
}

// SweepReport summarizes one SweepOrphans call for the caller's own
// logging/metrics; every field is also emitted as a structured field on the
// summary log line SweepOrphans itself writes.
type SweepReport struct {
	Scanned          int
	Reclaimed        int
	Terminated       int
	SkippedLive      int
	SkippedAmbiguous int
	Errors           int
}

func (opts SweepOptions) withDefaults() SweepOptions {
	if opts.Root == "" {
		opts.Root = os.TempDir()
	}
	if opts.MinAge <= 0 {
		opts.MinAge = time.Hour
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
// Within that fence, three independent gates must ALL agree before this
// function ever terminates a process, and any one of them failing leaves
// the entry untouched:
//
//  1. Age: an entry younger than opts.MinAge is always skipped outright — a
//     session that only just started must never race the sweep.
//  2. Ownership: the entry's own .donmai-owner.json (written at creation —
//     see writeDonmaiOwnerManifest) must name a live child PID. No
//     manifest, or a manifest whose owner process is STILL running (it may
//     yet clean this up itself), or whose child PID is not alive, never
//     reaches the termination path — those fall back to (or stop at) plain
//     directory reclamation once independently proven safe.
//  3. Identity: the live child PID must independently look like the
//     configured codex binary (opts.BinaryHint) — a PID that survived long
//     enough to be reused by an unrelated process fails this check and is
//     left alone, along with its directory, rather than guessed at.
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
		"terminated", report.Terminated,
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
	if age := opts.now().Sub(info.ModTime()); age < opts.MinAge {
		report.SkippedLive++
		return
	}
	manifest, hasManifest := readDonmaiOwnerManifest(path)
	if !hasManifest {
		// No PID to check either way. The age gate above already proves
		// nothing has touched this directory recently, so age alone is
		// enough to reclaim it safely.
		opts.reclaim(path, kind, report)
		return
	}
	if opts.processAlive(manifest.OwnerPID) {
		// The donmai process that owns this directory is still running and
		// may yet clean it up itself. Never touch it.
		report.SkippedLive++
		return
	}
	if manifest.ChildPID == 0 || !opts.processAlive(manifest.ChildPID) {
		// Owner is gone and there is no live child to worry about (or none
		// was ever tracked for this directory shape — see
		// donmaiOwnerManifest's doc comment). Safe to reclaim.
		opts.reclaim(path, kind, report)
		return
	}
	if !opts.processLooksLikeCodex(manifest.ChildPID, opts.BinaryHint) {
		// The recorded PID is alive but does not look like our own child —
		// almost certainly PID reuse after the real one exited. Ambiguous:
		// touch neither the process nor the directory.
		report.SkippedAmbiguous++
		opts.Logger.Warn("codex: orphan sweep found a live but unidentifiable PID; skipping",
			"path", path, "kind", kind, "pid", manifest.ChildPID)
		return
	}
	opts.terminate(manifest.ChildPID, path, report)
	opts.reclaim(path, kind, report)
}

func (opts SweepOptions) reclaim(path, kind string, report *SweepReport) {
	if err := os.RemoveAll(path); err != nil {
		report.Errors++
		opts.Logger.Warn("codex: orphan sweep failed to reclaim", "path", path, "kind", kind, "err", err)
		return
	}
	report.Reclaimed++
	opts.Logger.Info("codex: orphan sweep reclaimed", "path", path, "kind", kind)
}

// terminate sends the escalating SIGTERM→SIGKILL sequence this package uses
// everywhere else (see namedInteractiveAppServer.close) to a proven-orphaned
// codex child before its directory is reclaimed, so it stops retrying
// whatever network call has kept it alive instead of leaking a live process
// under a directory that is about to disappear out from under it.
func (opts SweepOptions) terminate(pid int, path string, report *SweepReport) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Signal(syscallSIGTERM())
	deadline := opts.now().Add(opts.TerminationGrace)
	for opts.now().Before(deadline) {
		if !opts.processAlive(pid) {
			report.Terminated++
			opts.Logger.Info("codex: orphan sweep terminated an orphaned app-server", "pid", pid, "path", path)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = process.Kill()
	report.Terminated++
	opts.Logger.Warn("codex: orphan sweep force-killed an orphaned app-server after its grace window expired",
		"pid", pid, "path", path)
}
