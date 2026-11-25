package tempown

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// DefaultGrace is how long an artefact must have been idle before the janitor
// may remove it, even once its owner is proven dead. It keeps a window for a
// child process the owner started (a probe CLI, a harness) that outlived it.
const DefaultGrace = time.Hour

// pidReuseMargin absorbs clock granularity when comparing a live process's
// start time with an artefact's change time (see ownerAliveOS). It only ever
// widens "maybe the owner", never "dead".
const pidReuseMargin = time.Minute

// idleWalkMaxEntries bounds the per-artefact walk that proves a directory
// idle. A walk that stops early proves nothing, so the directory counts as in
// use (fail closed).
const idleWalkMaxEntries = 20000

// Verdict is the janitor's decision for one artefact.
type Verdict string

const (
	// VerdictReclaimable means the artefact is recorded, owner proven dead, idle past the grace
	// period. The only verdict Sweep acts on.
	VerdictReclaimable Verdict = "reclaimable"
	// VerdictLive means the recorded owner may still be running.
	VerdictLive Verdict = "live"
	// VerdictYoung means the owner is dead but the artefact changed within the
	// grace period (or could not be proven idle).
	VerdictYoung Verdict = "young"
	// VerdictLegacy means a registered prefix without an owner tag. No record,
	// never removed.
	VerdictLegacy Verdict = "legacy"
	// VerdictUnowned means the entry is not a plain entry of the expected shape owned by this
	// user and closed to group and other writes. Never removed.
	VerdictUnowned Verdict = "unowned"
	// VerdictAmbiguous means owner liveness could not be determined. Never removed.
	VerdictAmbiguous Verdict = "ambiguous"
)

// SweepOptions configures Sweep. The zero value sweeps os.TempDir() with the
// production defaults.
type SweepOptions struct {
	// Root is the directory scanned. Empty means os.TempDir().
	Root string
	// Grace is the idle floor; zero means DefaultGrace.
	Grace time.Duration
	// MaxRemovals bounds how many artefacts one call removes. Zero means 2000.
	MaxRemovals int
	// MaxDuration bounds the wall clock of one call. Zero means 30s.
	MaxDuration time.Duration
	// DryRun classifies without removing anything; Reclaimed then counts
	// what a real sweep would remove.
	DryRun bool
	// Logger receives one line per removal and a summary. Nil means
	// slog.Default().
	Logger *slog.Logger

	// ownerAlive and now are test seams.
	ownerAlive func(pid int, changed time.Time) (bool, error)
	now        func() time.Time
}

// ProducerCounts is the per-producer breakdown of one sweep.
type ProducerCounts struct {
	Producer  Producer        `json:"producer"`
	Count     int             `json:"count"`
	ByVerdict map[Verdict]int `json:"byVerdict"`
}

// SweepReport summarises one Sweep call.
type SweepReport struct {
	Root string `json:"root"`
	// Scanned counts entries carrying a registered prefix. Everything else
	// in Root is outside the fence and never examined.
	Scanned          int `json:"scanned"`
	Reclaimed        int `json:"reclaimed"`
	SkippedLive      int `json:"skippedLive"`
	SkippedYoung     int `json:"skippedYoung"`
	SkippedLegacy    int `json:"skippedLegacy"`
	SkippedUnowned   int `json:"skippedUnowned"`
	SkippedAmbiguous int `json:"skippedAmbiguous"`
	Errors           int `json:"errors"`
	// Partial is true when the call stopped at its time or removal budget.
	Partial bool `json:"partial"`
	// ByProducer is keyed by producer prefix.
	ByProducer map[string]*ProducerCounts `json:"byProducer"`
}

func (opts SweepOptions) withDefaults() SweepOptions {
	if opts.Root == "" {
		opts.Root = os.TempDir()
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	if opts.MaxRemovals <= 0 {
		opts.MaxRemovals = 2000
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ownerAlive == nil {
		opts.ownerAlive = ownerAliveOS
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return opts
}

// Sweep removes the artefacts in Root that this package recorded and whose
// owner is gone. An entry is removed only when every one of these holds:
//
//  1. Its name carries a registered prefix AND an owner tag (Parse). An
//     untagged name is legacy: nothing recorded its owner, so it stays.
//  2. It is a plain entry of the producer's kind (no symlink), owned by this
//     user, and not writable by group or other.
//  3. The recorded owner PID is not running, or is running but started after
//     the artefact last changed, which proves the PID was reused.
//  4. Nothing in it changed within the grace period. For a directory the
//     whole tree is walked; a walk that cannot finish counts as in use.
//
// Shared tool caches (tsx, the Node compile cache, Go build dirs, the Codex
// CLI's own temp files) never carry a registered prefix, so they are never
// candidates. Codex homes are reclaimed by the codex harness's own sweep.
func Sweep(ctx context.Context, opts SweepOptions) SweepReport {
	opts = opts.withDefaults()
	report := SweepReport{Root: opts.Root, ByProducer: map[string]*ProducerCounts{}}
	ctx, cancel := context.WithTimeout(ctx, opts.MaxDuration)
	defer cancel()

	entries, err := os.ReadDir(opts.Root)
	if err != nil {
		report.Errors++
		opts.Logger.Warn("tempown: janitor could not list root", "root", opts.Root, "err", err)
		return report
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			report.Partial = true
			break
		}
		name := entry.Name()
		producer, verdict, ok := opts.classify(filepath.Join(opts.Root, name), name)
		if !ok {
			continue
		}
		report.Scanned++
		counts := report.ByProducer[producer.Prefix]
		if counts == nil {
			counts = &ProducerCounts{Producer: producer, ByVerdict: map[Verdict]int{}}
			report.ByProducer[producer.Prefix] = counts
		}
		counts.Count++
		counts.ByVerdict[verdict]++
		switch verdict {
		case VerdictLive:
			report.SkippedLive++
		case VerdictYoung:
			report.SkippedYoung++
		case VerdictLegacy:
			report.SkippedLegacy++
		case VerdictUnowned:
			report.SkippedUnowned++
		case VerdictAmbiguous:
			report.SkippedAmbiguous++
		case VerdictReclaimable:
			if report.Reclaimed >= opts.MaxRemovals {
				report.Partial = true
				continue
			}
			if opts.DryRun {
				report.Reclaimed++
				continue
			}
			path := filepath.Join(opts.Root, name)
			if err := os.RemoveAll(path); err != nil {
				report.Errors++
				opts.Logger.Warn("tempown: janitor failed to remove a dead artefact", "path", path, "err", err)
				continue
			}
			report.Reclaimed++
			opts.Logger.Debug("tempown: janitor removed a dead artefact", "path", path, "producer", producer.Label)
		}
	}
	if report.Scanned > 0 || report.Errors > 0 {
		opts.Logger.Info("tempown: janitor sweep complete",
			"root", report.Root,
			"dryRun", opts.DryRun,
			"scanned", report.Scanned,
			"reclaimed", report.Reclaimed,
			"skippedLive", report.SkippedLive,
			"skippedYoung", report.SkippedYoung,
			"skippedLegacy", report.SkippedLegacy,
			"skippedUnowned", report.SkippedUnowned,
			"skippedAmbiguous", report.SkippedAmbiguous,
			"errors", report.Errors,
			"partial", report.Partial,
		)
	}
	return report
}

// Producers returns the report's per-producer counts, ordered by prefix.
func (r SweepReport) Producers() []ProducerCounts {
	out := make([]ProducerCounts, 0, len(r.ByProducer))
	for _, c := range r.ByProducer {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Producer.Prefix < out[j].Producer.Prefix })
	return out
}

// classify decides one entry. ok is false for a name outside the fence.
func (opts SweepOptions) classify(path, name string) (Producer, Verdict, bool) {
	producer, pid, recorded := Parse(name)
	if !recorded {
		if legacy, isLegacy := Legacy(name); isLegacy {
			return legacy, VerdictLegacy, true
		}
		return Producer{}, "", false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return producer, VerdictUnowned, true
	}
	if !shapeMatches(info, producer.Kind) || !ownedExclusively(info) {
		return producer, VerdictUnowned, true
	}
	alive, err := opts.ownerAlive(pid, changedAt(info))
	if err != nil {
		return producer, VerdictAmbiguous, true
	}
	if alive {
		return producer, VerdictLive, true
	}
	if !opts.idleLongerThan(path, info) {
		return producer, VerdictYoung, true
	}
	return producer, VerdictReclaimable, true
}

func shapeMatches(info os.FileInfo, kind Kind) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	switch kind {
	case KindDir:
		return info.IsDir()
	case KindFile:
		return info.Mode().IsRegular()
	default:
		return false
	}
}

// idleLongerThan reports whether nothing at or under path changed within the
// grace period. It fails closed: an unreadable entry, or a walk that hits its
// ceiling, reports not idle.
func (opts SweepOptions) idleLongerThan(path string, info os.FileInfo) bool {
	cutoff := opts.now().Add(-opts.Grace)
	if changedAt(info).After(cutoff) {
		return false
	}
	if !info.IsDir() {
		return true
	}
	idle := true
	seen := 0
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			idle = false
			return fs.SkipAll
		}
		seen++
		if seen > idleWalkMaxEntries {
			idle = false
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil || changedAt(fi).After(cutoff) {
			idle = false
			return fs.SkipAll
		}
		return nil
	})
	return err == nil && idle
}

// ownerAliveOS reports whether pid may still be the process that created an
// artefact last changed at changed. The current process is always alive. A
// PID with no process is dead. A live PID whose process started after the
// artefact last changed cannot be its creator (the creator existed when the
// artefact was made), so the PID was reused and the owner is dead. Any other
// live PID is treated as the owner. An unreadable process table is an error,
// which the caller treats as "leave it alone".
func ownerAliveOS(pid int, changed time.Time) (bool, error) {
	if pid == os.Getpid() {
		return true, nil
	}
	live, err := sessionshim.ProcessIdentityFor(pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	started := time.Unix(0, live.StartedAt)
	if started.After(changed.Add(pidReuseMargin)) {
		return false, nil
	}
	return true, nil
}
