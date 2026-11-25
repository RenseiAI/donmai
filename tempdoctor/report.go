// Package tempdoctor reports what fills the user temp folder, by producer,
// and removes what donmai itself left behind.
//
// Scan is read-only and bounded: it samples members of large groups, reads at
// most a fixed number of entries per directory (estimating the rest from the
// filesystem's own child count where it keeps one), and stops at a deadline,
// returning a partial report rather than hanging on a folder with millions of
// files. Fix removes daemon-owned dead artefacts (tempown's janitor, and codex
// homes no codex process ever used) and, only on an explicit, confirmed
// request with no seat running, the shared tool caches.
//
// An embedding product can wrap Scan, Report.WriteText and Fix as its own
// command; donmai's `host doctor temp` is one such wrapper.
package tempdoctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/tempown"
)

// Defaults for Options.
const (
	DefaultTimeout              = 20 * time.Second
	DefaultMemberSample         = 24
	DefaultDirSample            = 5000
	DefaultMemberBudget         = 20000
	DefaultFlatThreshold        = 100000
	DefaultBootEntriesPerSecond = 10000
)

// Options configures Scan. The zero value scans os.TempDir() with defaults.
type Options struct {
	// Root is the folder to report on. Empty means os.TempDir().
	Root string
	// Timeout bounds the whole scan; the report is partial past it.
	Timeout time.Duration
	// MemberSample is how many entries of one group are measured; the rest
	// are extrapolated from them.
	MemberSample int
	// DirSample is how many entries of one directory are read before its
	// remaining size is estimated.
	DirSample int
	// MemberBudget bounds the entries read while measuring one member.
	MemberBudget int
	// FlatThreshold is the direct entry count that marks a directory flat.
	FlatThreshold int64
	// BootEntriesPerSecond is the deletion rate the boot-cleanup estimate
	// assumes.
	BootEntriesPerSecond float64
	// Grace is the idle floor the daemon-artefact verdicts use; zero means
	// tempown.DefaultGrace.
	Grace time.Duration
}

func (o Options) withDefaults() Options {
	if o.Root == "" {
		o.Root = os.TempDir()
	}
	o.Root = filepath.Clean(o.Root)
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MemberSample <= 0 {
		o.MemberSample = DefaultMemberSample
	}
	if o.DirSample <= 0 {
		o.DirSample = DefaultDirSample
	}
	if o.MemberBudget <= 0 {
		o.MemberBudget = DefaultMemberBudget
	}
	if o.FlatThreshold <= 0 {
		o.FlatThreshold = DefaultFlatThreshold
	}
	if o.BootEntriesPerSecond <= 0 {
		o.BootEntriesPerSecond = DefaultBootEntriesPerSecond
	}
	return o
}

// Group is one producer's share of the temp folder.
type Group struct {
	Class   Class  `json:"class"`
	Label   string `json:"label"`
	Pattern string `json:"pattern"`
	// Items is the number of top-level entries in the group (exact).
	Items int `json:"items"`
	// Entries counts every file and directory in the group's items.
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
	// Estimated is true when Entries/Bytes were sampled or extrapolated.
	Estimated bool `json:"estimated"`
	// LowerBound is true when part of the group could not be counted or
	// estimated; the real figures are larger.
	LowerBound bool `json:"lowerBound"`
	// Measured is false when the deadline arrived before the group was
	// measured; Entries and Bytes are then zero.
	Measured bool `json:"measured"`
	// Verdicts breaks down a daemon group by janitor verdict.
	Verdicts map[tempown.Verdict]int `json:"verdicts,omitempty"`
	// Shapes breaks down the codex homes group.
	Shapes map[providercodex.HomeShape]int `json:"shapes,omitempty"`

	members []string
}

// FlatDir is a directory holding a very large number of direct entries.
type FlatDir struct {
	Path      string `json:"path"`
	Entries   int64  `json:"entries"`
	Estimated bool   `json:"estimated"`
}

// BootCleanup estimates what emptying this folder at boot costs.
type BootCleanup struct {
	// Applies is true where the OS empties this folder before the user
	// session starts (macOS per-user temp folders).
	Applies          bool    `json:"applies"`
	Entries          int64   `json:"entries"`
	EntriesPerSecond float64 `json:"entriesPerSecond"`
	Seconds          float64 `json:"seconds"`
}

// Report is the result of Scan.
type Report struct {
	Root          string      `json:"root"`
	ScannedAt     time.Time   `json:"scannedAt"`
	ElapsedMS     int64       `json:"elapsedMs"`
	Partial       bool        `json:"partial"`
	PartialReason string      `json:"partialReason,omitempty"`
	TopLevel      int         `json:"topLevelEntries"`
	Entries       int64       `json:"entries"`
	Bytes         int64       `json:"bytes"`
	Estimated     bool        `json:"estimated"`
	Groups        []Group     `json:"groups"`
	FlatDirs      []FlatDir   `json:"flatDirs"`
	BootCleanup   BootCleanup `json:"bootCleanup"`
}

// Scan reports on opts.Root. It never modifies anything. It returns an error
// only when the root itself cannot be read; a deadline yields a partial
// report.
func Scan(ctx context.Context, opts Options) (*Report, error) {
	opts = opts.withDefaults()
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	report := &Report{Root: opts.Root, ScannedAt: start.UTC(), FlatDirs: []FlatDir{}}
	defer func() { report.ElapsedMS = time.Since(start).Milliseconds() }()

	names, err := listNames(ctx, opts.Root)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		report.markPartial("deadline reached while listing the folder")
	case len(names) == 0:
		return nil, fmt.Errorf("tempdoctor: list %s: %w", opts.Root, err)
	default:
		report.markPartial("top-level listing stopped: " + err.Error())
	}
	report.TopLevel = len(names)
	hint := childCountHint(opts.Root)

	groups := map[string]*Group{}
	for _, name := range names {
		if ctx.Err() != nil {
			report.markPartial("deadline reached while classifying entries")
			break
		}
		class, label := classify(name)
		key := string(class) + "\x00" + label
		g := groups[key]
		if g == nil {
			g = &Group{Class: class, Label: label, Pattern: patternFor(class, label)}
			groups[key] = g
		}
		path := filepath.Join(opts.Root, name)
		g.Items++
		g.members = append(g.members, path)
		// A cheap whole-folder flat-directory check: where the filesystem
		// keeps a child count, every top-level directory is checked, not
		// just the sampled ones.
		if hint != nil {
			if info, err := os.Lstat(path); err == nil && info.IsDir() {
				if n, ok := hint(path, info); ok && n >= opts.FlatThreshold {
					report.FlatDirs = append(report.FlatDirs, FlatDir{Path: path, Entries: n})
				}
			}
		}
	}

	ordered := make([]*Group, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Class != ordered[j].Class {
			return classOrder(ordered[i].Class) < classOrder(ordered[j].Class)
		}
		return ordered[i].Label < ordered[j].Label
	})

	report.annotateDaemonVerdicts(ctx, ordered, opts.Grace)
	report.annotateCodexShapes(ctx, ordered)

	s := scanner{opts: opts, hint: hint, flats: map[string]bool{}}
	for _, f := range report.FlatDirs {
		s.flats[f.Path] = true
	}
	for _, g := range ordered {
		if ctx.Err() != nil {
			report.markPartial("deadline reached before every group was measured")
			break
		}
		s.measureGroup(ctx, g)
		if !g.Measured {
			report.markPartial("deadline reached while measuring " + g.Label)
		}
	}
	report.FlatDirs = append(report.FlatDirs, s.found...)
	sort.Slice(report.FlatDirs, func(i, j int) bool { return report.FlatDirs[i].Entries > report.FlatDirs[j].Entries })

	for _, g := range ordered {
		report.Entries += g.Entries
		report.Bytes += g.Bytes
		if g.Estimated || g.LowerBound || !g.Measured {
			report.Estimated = true
		}
		report.Groups = append(report.Groups, *g)
	}
	report.BootCleanup = BootCleanup{
		Applies:          bootCleansFolder(opts.Root),
		Entries:          report.Entries,
		EntriesPerSecond: opts.BootEntriesPerSecond,
		Seconds:          float64(report.Entries) / opts.BootEntriesPerSecond,
	}
	return report, nil
}

func (r *Report) markPartial(reason string) {
	if !r.Partial {
		r.Partial = true
		r.PartialReason = reason
	}
}

func classOrder(c Class) int {
	switch c {
	case ClassToolCache:
		return 0
	case ClassDaemonCache:
		return 1
	case ClassCodex:
		return 2
	case ClassDaemon:
		return 3
	default:
		return 4
	}
}

// annotateDaemonVerdicts attaches the janitor's dry-run verdicts to the
// daemon groups.
func (r *Report) annotateDaemonVerdicts(ctx context.Context, groups []*Group, grace time.Duration) {
	remaining := time.Until(deadlineOf(ctx))
	if remaining <= 0 {
		return
	}
	dry := tempown.Sweep(ctx, tempown.SweepOptions{
		Root: r.Root, DryRun: true, MaxDuration: remaining, Grace: grace,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	for _, counts := range dry.Producers() {
		for _, g := range groups {
			if g.Class == ClassDaemon && g.Label == counts.Producer.Label {
				g.Verdicts = counts.ByVerdict
			}
		}
	}
	if dry.Partial {
		r.markPartial("deadline reached while classifying daemon artefacts")
	}
}

// annotateCodexShapes classifies every codex home by shape.
func (r *Report) annotateCodexShapes(ctx context.Context, groups []*Group) {
	for _, g := range groups {
		if g.Class != ClassCodex || g.Label != "codex homes" {
			continue
		}
		g.Shapes = map[providercodex.HomeShape]int{}
		for _, path := range g.members {
			if ctx.Err() != nil {
				r.markPartial("deadline reached while classifying codex homes")
				return
			}
			g.Shapes[providercodex.ClassifyHome(path)]++
		}
	}
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(DefaultTimeout)
}

// listNames lists root's top-level names in batches so a deadline can
// interrupt a very large listing.
func listNames(ctx context.Context, root string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f, err := os.Open(root) //nolint:gosec // root is the operator's own temp folder.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var names []string
	for {
		if ctx.Err() != nil {
			return names, ctx.Err()
		}
		batch, err := f.ReadDir(4096)
		for _, d := range batch {
			names = append(names, d.Name())
		}
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return names, err
		}
	}
}

// scanner measures groups within the scan's budgets.
type scanner struct {
	opts  Options
	hint  func(string, os.FileInfo) (int64, bool)
	flats map[string]bool
	found []FlatDir
}

// measureGroup measures up to MemberSample evenly spaced members and
// extrapolates to the whole group.
func (s *scanner) measureGroup(ctx context.Context, g *Group) {
	n := len(g.members)
	if n == 0 {
		g.Measured = true
		return
	}
	k := min(n, s.opts.MemberSample)
	var entries, bytes int64
	measured := 0
	for i := range k {
		if ctx.Err() != nil {
			break
		}
		path := g.members[i*n/k]
		t := s.measure(ctx, path)
		entries += t.entries
		bytes += t.bytes
		g.Estimated = g.Estimated || t.estimated
		g.LowerBound = g.LowerBound || t.lowerBound
		measured++
	}
	if measured == 0 {
		return
	}
	g.Measured = measured == k
	if measured < n {
		g.Estimated = true
		entries = entries * int64(n) / int64(measured)
		bytes = bytes * int64(n) / int64(measured)
	}
	g.Entries, g.Bytes = entries, bytes
}

type tally struct {
	entries, bytes        int64
	estimated, lowerBound bool
}

// measure counts path and everything under it within MemberBudget entries.
func (s *scanner) measure(ctx context.Context, path string) tally {
	info, err := os.Lstat(path)
	if err != nil {
		return tally{lowerBound: true}
	}
	t := tally{entries: 1}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			t.bytes = info.Size()
		}
		return t
	}
	budget := s.opts.MemberBudget
	queue := []string{path}
	for len(queue) > 0 {
		if ctx.Err() != nil || budget <= 0 {
			t.lowerBound = true
			return t
		}
		dir := queue[0]
		queue = queue[1:]
		subdirs, read, sampleFiles, sampleBytes, more, ok := s.readDir(dir, min(s.opts.DirSample, budget))
		budget -= int(read)
		t.entries += read
		t.bytes += sampleBytes
		queue = append(queue, subdirs...)
		if !ok {
			t.lowerBound = true
			continue
		}
		count := read
		if more {
			total, known := int64(0), false
			if s.hint != nil {
				if di, err := os.Lstat(dir); err == nil {
					total, known = s.hint(dir, di)
				}
			}
			if known && total > read {
				extra := total - read
				t.entries += extra
				if sampleFiles > 0 {
					t.bytes += extra * (sampleBytes / sampleFiles)
				}
				t.estimated = true
				count = total
			} else {
				t.lowerBound = true
			}
		}
		if count >= s.opts.FlatThreshold && !s.flats[dir] {
			s.flats[dir] = true
			s.found = append(s.found, FlatDir{Path: dir, Entries: count, Estimated: more})
		}
	}
	return t
}

// readDir reads up to limit entries of dir. more reports entries left
// unread; ok is false when the directory could not be read.
func (s *scanner) readDir(dir string, limit int) (subdirs []string, read, files, bytes int64, more, ok bool) {
	f, err := os.Open(dir) //nolint:gosec // dir is under the operator's own temp folder.
	if err != nil {
		return nil, 0, 0, 0, false, false
	}
	defer func() { _ = f.Close() }()
	for int(read) < limit {
		batch, err := f.ReadDir(min(512, limit-int(read)))
		for _, d := range batch {
			read++
			if d.IsDir() {
				subdirs = append(subdirs, filepath.Join(dir, d.Name()))
				continue
			}
			if d.Type().IsRegular() {
				if fi, ierr := d.Info(); ierr == nil {
					files++
					bytes += fi.Size()
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return subdirs, read, files, bytes, false, true
		}
		if err != nil {
			return subdirs, read, files, bytes, false, false
		}
	}
	peek, err := f.ReadDir(1)
	more = len(peek) > 0 && err == nil
	return subdirs, read, files, bytes, more, true
}

// bootCleansFolder reports whether the OS empties root at boot: on macOS the
// per-user temp folder (/var/folders/<x>/<y>/T) is emptied before the user
// session starts.
func bootCleansFolder(root string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	return filepath.Base(resolved) == "T" && strings.Contains(resolved, "/var/folders/")
}
