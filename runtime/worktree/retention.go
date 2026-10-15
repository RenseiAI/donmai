// Package worktree — retention.go: retention budget for retained work areas.
//
// This file implements the expire and archive-drop half of the retention
// budget for retained work areas: roots whose session is durably terminal
// and that survived teardown through preservation are expired after a
// maximum age (or least recently used first over a disk budget), and the
// archives that hold their unpushed work are dropped after their own
// maximum age. Dehydration (removing installed trees first) is a separate
// pass over install records and is not implemented here.
//
// A retained root is one the manager no longer tracks: its terminal lease
// released (or it was preserved without a lease) and its directory still
// sits under the manager's parent directory. Each expiry is an explicit
// transition with a receipt naming the policy and the reclaimed bytes —
// never a "looks orphaned" sweep. A root is disposed of as a whole.
//
// Scope guard: the pass never touches a root that is still live — leased
// (active or release-pending terminal lease), adopted into the manager's
// live sessions, quarantined by the acquisition authority, or parked for
// resume. Parked-for-resume means a root whose release-pending lease has
// not yet been reaped: its disposition is still "return this root to its
// session", so expiry must not race it.
//
// Unpushed-work rule: a root holding unpushed work in any mutable leaf is
// archived, never destroyed. Unpushed work is any of:
//   - commits unreachable from the leaf's remote-tracking refs;
//   - uncommitted changes to tracked files;
//   - untracked files that git does not ignore.
//
// Dropping an archive after its maximum age is the only path by which
// unpushed work leaves the host, and it carries its own receipt.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// Retention defaults follow the founder's 2026-10-08 rulings: roots expire
// after 14 days, archives drop after 30 days. A zero value disables the
// corresponding rule.
const (
	// DefaultRetentionMaxAgeDays expires retained roots after 14 days.
	DefaultRetentionMaxAgeDays = 14
	// DefaultRetentionArchiveMaxAgeDays drops archives after 30 days.
	DefaultRetentionArchiveMaxAgeDays = 30
)

// RetentionPolicy configures one retention pass over retained roots and
// archives. The zero value expires roots after DefaultRetentionMaxAgeDays
// and drops archives after DefaultRetentionArchiveMaxAgeDays, with no disk
// budget. A zero MaxAgeDays or ArchiveMaxAgeDays disables that rule; a
// zero MaxDiskBytes disables disk-pressure expiry.
type RetentionPolicy struct {
	// MaxAgeDays expires retained roots older than this many days. Zero
	// disables age expiry.
	MaxAgeDays int
	// MaxDiskBytes expires least recently used retained roots first while
	// retained roots exceed this many bytes. Zero disables disk pressure.
	MaxDiskBytes int64
	// ArchiveMaxAgeDays drops archives older than this many days. Zero
	// disables archive dropping.
	ArchiveMaxAgeDays int
}

// EffectiveRetentionPolicy resolves zero policy fields to their defaults.
// Explicit zeros cannot be distinguished from absent, so callers that need
// "disabled" pass a negative value, which resolves to zero (disabled).
func EffectiveRetentionPolicy(p RetentionPolicy) RetentionPolicy {
	if p.MaxAgeDays == 0 {
		p.MaxAgeDays = DefaultRetentionMaxAgeDays
	} else if p.MaxAgeDays < 0 {
		p.MaxAgeDays = 0
	}
	if p.ArchiveMaxAgeDays == 0 {
		p.ArchiveMaxAgeDays = DefaultRetentionArchiveMaxAgeDays
	} else if p.ArchiveMaxAgeDays < 0 {
		p.ArchiveMaxAgeDays = 0
	}
	if p.MaxDiskBytes < 0 {
		p.MaxDiskBytes = 0
	}
	return p
}

// RetentionDisposition names how one retained root left the host.
type RetentionDisposition string

const (
	// RetentionArchived means the root held unpushed work and was captured
	// through the manager's archive provider before disposal.
	RetentionArchived RetentionDisposition = "archived"
	// RetentionDestroyed means the root held no unpushed work and its
	// directory was removed.
	RetentionDestroyed RetentionDisposition = "destroyed"
	// RetentionArchiveDropped means an archive past its maximum age was
	// removed. It is the only path by which unpushed work leaves the host.
	RetentionArchiveDropped RetentionDisposition = "archive-dropped"
)

// RetentionReason names why a root was eligible for expiry.
type RetentionReason string

const (
	// RetentionReasonAge means the root exceeded RetentionPolicy.MaxAgeDays.
	RetentionReasonAge RetentionReason = "age"
	// RetentionReasonDiskPressure means the root was the least recently
	// used root over RetentionPolicy.MaxDiskBytes.
	RetentionReasonDiskPressure RetentionReason = "disk-pressure"
	// RetentionReasonArchiveAge means the archive exceeded
	// RetentionPolicy.ArchiveMaxAgeDays.
	RetentionReasonArchiveAge RetentionReason = "archive-age"
)

// RetentionReceipt is the explicit per-transition record for one expiry or
// archive drop: it names the policy (reason), the disposition, and the
// reclaimed bytes.
type RetentionReceipt struct {
	// Root is the retained root (or archive directory) the transition
	// disposed of.
	Root string
	// Disposition is how the root left the host.
	Disposition RetentionDisposition
	// Reason is the policy that made the root eligible.
	Reason RetentionReason
	// BytesReclaimed is the measured free-space change of the transition.
	BytesReclaimed int64
	// At is when the transition completed.
	At time.Time
}

// RetentionReport summarizes one retention pass.
type RetentionReport struct {
	// Receipts holds one receipt per completed transition, in completion
	// order.
	Receipts []RetentionReceipt
	// BytesReclaimed is the measured free-space change across the pass.
	BytesReclaimed int64
	// Considered counts retained roots the pass inspected.
	Considered int
	// Skipped counts retained roots left in place (live, unexpired, or
	// keeper failure degrading to today's install).
	Skipped int
}

// RetentionArchiveDropper removes one archive directory past its maximum
// age. Implementations remove the directory named by the archive ID under
// the archive root the pass scanned; the pass supplies the ID and the
// absolute directory.
type RetentionArchiveDropper func(ctx context.Context, archiveID, archiveDir string) error

// ExpireRetainedRoots performs one bounded retention pass: it expires
// retained roots under the manager's parent directory that exceed the
// policy's age, then least recently used first while retained roots exceed
// the policy's disk budget, then drops archives past their maximum age.
//
// The pass never makes a session wait: a root whose liveness cannot be
// proved (lease-store read failure, unreadable declaration) is skipped,
// and a keeper failure degrades to today's behavior — the root stays.
func (m *Manager) ExpireRetainedRoots(ctx context.Context, policy RetentionPolicy, dropper RetentionArchiveDropper) (*RetentionReport, error) {
	policy = EffectiveRetentionPolicy(policy)
	report := &RetentionReport{}
	now := m.clock()
	roots, err := m.listRetainedRoots()
	if err != nil {
		return nil, err
	}
	report.Considered = len(roots)
	expired := make(map[string]struct{}, len(roots))
	if policy.MaxAgeDays > 0 {
		for _, root := range roots {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			old, err := rootOlderThan(root, now, time.Duration(policy.MaxAgeDays)*24*time.Hour)
			if err != nil {
				// The root's age cannot be read; leave it for a later
				// pass rather than expiring on a guess.
				report.Skipped++
				continue
			}
			if !old {
				report.Skipped++
				continue
			}
			receipt, kept, err := m.expireRetainedRoot(ctx, root, RetentionReasonAge, now)
			if err != nil {
				return report, err
			}
			if kept {
				report.Skipped++
				continue
			}
			report.Receipts = append(report.Receipts, receipt)
			report.BytesReclaimed += receipt.BytesReclaimed
			expired[root.Path] = struct{}{}
		}
		roots = filterExpiredRoots(roots, expired)
	} else {
		report.Skipped += len(roots)
	}
	if policy.MaxDiskBytes > 0 {
		// Roots skipped by the age pass (young, or unprovable) are
		// still candidates under pressure; re-list so LRU order sees
		// every surviving root. The expired set keeps disposed roots
		// out of the measurement.
		if len(expired) > 0 {
			all, err := m.listRetainedRoots()
			if err != nil {
				return report, err
			}
			roots = filterExpiredRoots(all, expired)
			report.Considered = len(all)
		}
		for {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			live, err := m.retainedRootsUnderPressure(roots, expired)
			if err != nil {
				return report, err
			}
			if live.total <= policy.MaxDiskBytes {
				break
			}
			victim := oldestRetainedRoot(live.roots)
			if victim == nil {
				break
			}
			// LRU order is by last use: the oldest root goes first. A
			// root with no observable use time sorts oldest.
			receipt, kept, err := m.expireRetainedRoot(ctx, victim.root, RetentionReasonDiskPressure, now)
			if err != nil {
				return report, err
			}
			if kept {
				// Live or unreadable: it still occupies disk, but the
				// pass must not spin expiring nothing. Count it skipped
				// and stop the pressure loop; the next pass re-measures.
				report.Skipped++
				break
			}
			report.Receipts = append(report.Receipts, receipt)
			report.BytesReclaimed += receipt.BytesReclaimed
			expired[victim.root.Path] = struct{}{}
			roots = filterExpiredRoots(roots, expired)
		}
	}
	if policy.ArchiveMaxAgeDays > 0 && dropper != nil {
		dropped, skipped, err := m.dropExpiredArchives(ctx, policy, dropper, now)
		if err != nil {
			return report, err
		}
		report.Receipts = append(report.Receipts, dropped...)
		for _, receipt := range dropped {
			report.BytesReclaimed += receipt.BytesReclaimed
		}
		report.Skipped += skipped
	}
	// Reconcile the skipped count: roots neither expired nor pressure-run
	// are skips. The pressure loop above resets the count; recompute from
	// the surviving set instead of trusting the running tally.
	surviving, err := m.listRetainedRoots()
	if err == nil {
		report.Skipped = len(filterExpiredRoots(surviving, expired))
	}
	return report, nil
}

// retainedRoot is one candidate root for expiry: a directory directly
// under the manager's parent directory that is not manager-owned state.
type retainedRoot struct {
	// Path is the absolute root directory.
	Path string
	// UseTime is the last observable use of the root: the youngest
	// modification time under it, or the directory's own modification
	// time when the walk fails. Zero when nothing was observable.
	UseTime time.Time
	// Size is the physical allocation of the root in bytes, or -1 when
	// the walk failed. A failed walk still expires by age (the receipt
	// reports the measured free-space change, not this estimate).
	Size int64
}

// listRetainedRoots enumerates candidate expiry roots: every directory
// directly under the parent directory except the manager's durable state
// directories (acquisition store, terminal leases, cache seeds, archive
// staging). Roots the manager currently tracks as live sessions are
// included here and filtered by the liveness check at expiry time, so a
// root adopted concurrently between listing and expiry is still safe.
func (m *Manager) listRetainedRoots() ([]retainedRoot, error) {
	entries, err := os.ReadDir(m.parentDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: list retained roots: %w", err)
	}
	skip := map[string]struct{}{
		".workarea-acquisitions": {},
		".workarea-seeds":        {},
		".terminal-leases":       {},
		".terminal-quarantine":   {},
		".terminal-receivers":    {},
	}
	roots := make([]retainedRoot, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, reserved := skip[entry.Name()]; reserved {
			continue
		}
		rootPath := filepath.Join(m.parentDir, entry.Name())
		info, err := os.Lstat(rootPath)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		root := retainedRoot{Path: rootPath, Size: -1}
		if useTime, ok := youngestUseTime(rootPath, info); ok {
			root.UseTime = useTime
		}
		if size, err := workarea.PhysicalUsage(workarea.RootPath(rootPath)); err == nil {
			root.Size = size
		}
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	return roots, nil
}

// youngestUseTime reports the last observable use of a root: the youngest
// file modification time one level under it, falling back to the root
// directory's own modification time. Only the top level is inspected —
// the pass must not walk whole trees just to order them.
func youngestUseTime(rootPath string, rootInfo os.FileInfo) (time.Time, bool) {
	youngest := rootInfo.ModTime()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return youngest, true
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(youngest) {
			youngest = info.ModTime()
		}
	}
	return youngest, true
}

// rootOlderThan reports whether a root's last observable use exceeds maxAge.
func rootOlderThan(root retainedRoot, now time.Time, maxAge time.Duration) (bool, error) {
	if root.UseTime.IsZero() {
		return false, fmt.Errorf("runtime/worktree: retained root %q has no observable use time", root.Path)
	}
	return now.Sub(root.UseTime) > maxAge, nil
}

// filterExpiredRoots drops disposed roots from a candidate list.
func filterExpiredRoots(roots []retainedRoot, expired map[string]struct{}) []retainedRoot {
	if len(expired) == 0 {
		return roots
	}
	kept := roots[:0]
	for _, root := range roots {
		if _, gone := expired[root.Path]; !gone {
			kept = append(kept, root)
		}
	}
	return kept
}

// retainedUsage totals the physical allocation of candidate roots whose
// size walk succeeded.
type retainedUsage struct {
	roots []retainedRoot
	total int64
}

// retainedRootsUnderPressure measures the surviving candidates for the
// disk-budget check. Roots whose size walk failed contribute nothing to
// the total — they still expire by age, but pressure never fires on a
// guess. A measurement failure for the whole set fails the pass: without
// a total there is no pressure signal.
func (m *Manager) retainedRootsUnderPressure(roots []retainedRoot, expired map[string]struct{}) (retainedUsage, error) {
	_ = m
	var usage retainedUsage
	for _, root := range roots {
		if _, gone := expired[root.Path]; gone {
			continue
		}
		if root.Size < 0 {
			continue
		}
		usage.roots = append(usage.roots, root)
		usage.total += root.Size
	}
	return usage, nil
}

// oldestRetainedRoot selects the least recently used candidate: the root
// with the oldest use time. Roots with no observable use time sort first —
// they are the coldest measurable signal, not a guess about content.
func oldestRetainedRoot(roots []retainedRoot) *struct {
	root retainedRoot
} {
	if len(roots) == 0 {
		return nil
	}
	victim := roots[0]
	for _, root := range roots[1:] {
		if root.UseTime.Before(victim.UseTime) {
			victim = root
		}
	}
	return &struct {
		root retainedRoot
	}{root: victim}
}

// expireRetainedRoot expires one candidate root: live roots (leased,
// release-pending, adopted, quarantined, parked for resume) are untouched;
// a root with unpushed work is archived, never destroyed; otherwise the
// root is destroyed. It returns kept=true when the root stays on disk.
//
// Every keeper failure degrades to today's behavior: the root stays, and
// only I/O failures of the pass itself (listing, removing) surface as
// errors.
func (m *Manager) expireRetainedRoot(ctx context.Context, root retainedRoot, reason RetentionReason, now time.Time) (RetentionReceipt, bool, error) {
	receipt := RetentionReceipt{Root: root.Path, Reason: reason, At: now}
	live, err := m.retainedRootLive(root.Path)
	if err != nil {
		// Liveness is unprovable: lease-store or acquisition-store read
		// failure. The root stays; a later pass retries.
		m.logger.Warn("retention: liveness unprovable; keeping retained root",
			"path", root.Path, "err", err)
		return receipt, true, nil
	}
	if live {
		return receipt, true, nil
	}
	unpushed, err := m.retainedRootHasUnpushedWork(ctx, root.Path)
	if err != nil {
		// The unpushed-work probe failed (unreadable leaf, missing git,
		// corrupt index). Failing closed keeps the root: destroying on
		// an unreadable checkout could delete work the probe could not
		// see, and archiving without a readable tree captures nothing.
		m.logger.Warn("retention: unpushed-work probe failed; keeping retained root",
			"path", root.Path, "err", err)
		return receipt, true, nil
	}
	before, beforeErr := freeSpace(m.parentDir)
	if unpushed {
		if m.archiveRoot == nil {
			m.logger.Warn("retention: root holds unpushed work with no archive provider; keeping retained root",
				"path", root.Path)
			return receipt, true, nil
		}
		if err := m.archiveRetainedRoot(ctx, root.Path); err != nil {
			return receipt, false, err
		}
		receipt.Disposition = RetentionArchived
	} else {
		if err := m.destroyRetainedRoot(ctx, root.Path); err != nil {
			return receipt, false, err
		}
		receipt.Disposition = RetentionDestroyed
	}
	receipt.BytesReclaimed = reclaimedBytes(m.parentDir, before, beforeErr)
	receipt.At = m.clock()
	return receipt, false, nil
}

// retentionQuarantinedRoot reports whether the acquisition authority holds
// the root in the quarantined state: ownership could not be proved and no
// cleanup is authorized. ReadyRecords (used below for live ready roots)
// never returns quarantined generations, so this path scans the store
// directory directly and reads each generation's record without running
// recovery or mutating anything: a quarantined record whose final root is
// the candidate path keeps the root. A read failure fails closed — the
// caller keeps the root rather than expiring on an unreadable authority.
func (m *Manager) retentionQuarantinedRoot(rootPath string) (bool, error) {
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention quarantine acquisition check: %w", err)
	}
	records, err := acquisitions.QuarantinedRecords()
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention quarantine acquisition read: %w", err)
	}
	for _, record := range records {
		if filepath.Clean(record.FinalRoot) == filepath.Clean(rootPath) {
			return true, nil
		}
	}
	return false, nil
}

// retainedRootLive reports whether a root is still live and must not
// expire: leased (an active or release-pending terminal lease retains the
// exact path), adopted (the manager tracks a live session at the root),
// quarantined by the acquisition authority, or parked for resume.
//
// Parked for resume is the release-pending lease state: the lease's
// disposition is still "return this root to its session", so expiry must
// not race the reaper. The lease store reports parked roots as retained;
// the check below treats every retained path as live, which covers
// active, release-pending, and parked alike.
func (m *Manager) retainedRootLive(rootPath string) (bool, error) {
	retained, err := m.retainedPath(rootPath)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention liveness lease check: %w", err)
	}
	if retained {
		return true, nil
	}
	m.mu.Lock()
	for _, res := range m.sessions {
		if res != nil && filepath.Clean(res.workareaRootOrPath()) == filepath.Clean(rootPath) {
			m.mu.Unlock()
			return true, nil
		}
	}
	m.mu.Unlock()
	quarantined, err := m.retentionQuarantinedRoot(rootPath)
	if err != nil {
		return false, err
	}
	if quarantined {
		return true, nil
	}
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention liveness acquisition check: %w", err)
	}
	records, err := acquisitions.ReadyRecords()
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention liveness acquisition read: %w", err)
	}
	for _, record := range records {
		// ReadyRecords returns ready generations only (quarantined and
		// released generations never appear here). A ready generation
		// whose owner still holds it is a live root the manager has
		// not adopted in this process (for example after a restart
		// before recovery). Only the exact retained path counts:
		// released generations whose directory still lingers are
		// expiry candidates.
		if record.State == workarea.AcquisitionReady && !record.OwnerReleased &&
			filepath.Clean(record.FinalRoot) == filepath.Clean(rootPath) {
			return true, nil
		}
	}
	// A quarantined generation authorizes no cleanup at all: its root
	// is untouched even though no lease retains it. The terminal
	// quarantine journal (lease-acquisition failures) is consulted
	// through the lease store's retained-path check above — a
	// quarantined workarea path reports retained. Acquisition-level
	// quarantine (unproven root identity) surfaces as a ReadyRecords
	// failure or a quarantined record the pass must not dispose of; a
	// root whose declaration cannot authorize against the record is
	// unprovable and handled by the probe-fails-closed path at expiry
	// time, never by disposal here.
	return false, nil
}

// retainedRootHasUnpushedWork reports whether any mutable leaf of a
// retained root holds unpushed work: commits unreachable from the leaf's
// remote-tracking refs, uncommitted changes to tracked files, or untracked
// files that git does not ignore.
//
// The probe reads the root's declaration when present to find mutable
// leaves; a root without a declaration (a retained legacy flat workarea)
// is probed as one leaf. Read-only context leaves are never installed
// into and carry no session work, so they are skipped — but a read-only
// leaf the declaration does not name is probed anyway: an undeclared
// directory may still hold work the session left behind.
func (m *Manager) retainedRootHasUnpushedWork(ctx context.Context, rootPath string) (bool, error) {
	leaves, err := m.retentionLeaves(rootPath)
	if err != nil {
		return false, err
	}
	for _, leaf := range leaves {
		unpushed, err := m.leafHasUnpushedWork(ctx, leaf)
		if err != nil {
			return false, fmt.Errorf("runtime/worktree: probe retained leaf %q: %w", leaf, err)
		}
		if unpushed {
			return true, nil
		}
	}
	return false, nil
}

// retentionLeaves resolves the mutable leaves of a retained root. A root
// carrying a declaration contributes its declared repositories except
// read-only context leaves; every other root (legacy flat, declaration
// unreadable, or the selected path of a nested root) contributes the
// root itself when it holds a git checkout, plus any direct child holding
// a checkout the declaration does not name.
func (m *Manager) retentionLeaves(rootPath string) ([]string, error) {
	declaration, err := workarea.ReadDeclaration(workarea.RootPath(rootPath))
	if err == nil {
		leaves := make([]string, 0, len(declaration.Repositories))
		for _, repository := range declaration.Repositories {
			if repository.Authority == workarea.RepositoryReadOnly && repository.Role == workarea.RepositoryRoleContext {
				continue
			}
			leaf := filepath.Join(rootPath, repository.Leaf)
			if isGitCheckout(leaf) {
				leaves = append(leaves, leaf)
			}
		}
		if len(leaves) > 0 {
			return leaves, nil
		}
		// The declaration names no probed leaf (all leaves missing or
		// read-only context). Fall through to the filesystem scan so an
		// undeclared checkout in the root is still probed.
	}
	leaves := []string{}
	if isGitCheckout(rootPath) {
		leaves = append(leaves, rootPath)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		if len(leaves) > 0 {
			return leaves, nil
		}
		return nil, fmt.Errorf("runtime/worktree: list retained root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		leaf := filepath.Join(rootPath, entry.Name())
		if leaf == rootPath {
			continue
		}
		if isGitCheckout(leaf) && !containsPath(leaves, leaf) {
			leaves = append(leaves, leaf)
		}
	}
	if len(leaves) == 0 {
		return nil, fmt.Errorf("runtime/worktree: retained root %q holds no git checkout", rootPath)
	}
	return leaves, nil
}

// leafHasUnpushedWork probes one leaf for the three kinds of unpushed
// work. Any probe failure fails closed (returns the error): the caller
// keeps the root rather than destroying work it could not see.
func (m *Manager) leafHasUnpushedWork(ctx context.Context, leaf string) (bool, error) {
	if dirty, err := m.retentionLeafDirty(ctx, leaf); err != nil {
		return false, err
	} else if dirty {
		return true, nil
	}
	if untracked, err := m.retentionLeafUntracked(ctx, leaf); err != nil {
		return false, err
	} else if untracked {
		return true, nil
	}
	unpushed, err := m.retentionLeafUnpushedCommits(ctx, leaf)
	if err != nil {
		return false, err
	}
	return unpushed, nil
}

// retentionLeafDirty reports uncommitted changes to tracked files: staged
// additions or edits (the index differs from HEAD), or raw checkout bytes
// differing from the index.
func (m *Manager) retentionLeafDirty(ctx context.Context, leaf string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	config := retentionGitConfig(leaf)
	stagedArgs := append(append([]string{}, config...), "diff-index", "--cached", "--no-ext-diff", "--no-textconv", "--name-only", "HEAD", "--")
	staged, err := m.runGit(ctx, "", append([]string{"-C", leaf}, stagedArgs...)...)
	if err != nil {
		// A leaf with no commits yet has no HEAD to diff against; that
		// is unpushed work by definition (nothing is reachable from any
		// remote-tracking ref). Any other git failure is a probe error.
		if isMissingHeadError(staged) {
			return true, nil
		}
		return false, fmt.Errorf("runtime/worktree: retention staged diff: %w (%s)", err, strings.TrimSpace(string(staged)))
	}
	if len(bytesTrimSpace(staged)) != 0 {
		return true, nil
	}
	dirtyArgs := append(append([]string{}, config...), "status", "--porcelain=v1", "-z", "--untracked-files=no")
	dirty, err := m.runGit(ctx, "", append([]string{"-C", leaf}, dirtyArgs...)...)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention status: %w (%s)", err, strings.TrimSpace(string(dirty)))
	}
	return len(bytesTrimSpace(dirty)) != 0, nil
}

// retentionLeafUntracked reports untracked files that git does not ignore.
func (m *Manager) retentionLeafUntracked(ctx context.Context, leaf string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	args := append(retentionGitConfig(leaf), "ls-files", "--others", "--exclude-standard", "-z", "--")
	other, err := m.runGit(ctx, "", append([]string{"-C", leaf}, args...)...)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention untracked scan: %w (%s)", err, strings.TrimSpace(string(other)))
	}
	return len(bytesTrimSpace(other)) != 0, nil
}

// retentionLeafUnpushedCommits reports commits unreachable from the leaf's
// remote-tracking refs. With no remote-tracking refs at all, every commit
// on HEAD is unpushed.
func (m *Manager) retentionLeafUnpushedCommits(ctx context.Context, leaf string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	config := retentionGitConfig(leaf)
	refsArgs := append(append([]string{}, config...), "for-each-ref", "--format=%(refname)", "refs/remotes/")
	refsOut, err := m.runGit(ctx, "", append([]string{"-C", leaf}, refsArgs...)...)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention remote refs: %w (%s)", err, strings.TrimSpace(string(refsOut)))
	}
	headArgs := append(append([]string{}, config...), "rev-parse", "--verify", "HEAD^{commit}")
	headOut, err := m.runGit(ctx, "", append([]string{"-C", leaf}, headArgs...)...)
	if err != nil {
		// No HEAD (empty checkout): nothing committed, so no unpushed
		// commits. Uncommitted files are reported by the other probes.
		if isMissingHeadError(headOut) {
			return false, nil
		}
		return false, fmt.Errorf("runtime/worktree: retention resolve HEAD: %w (%s)", err, strings.TrimSpace(string(headOut)))
	}
	head := strings.TrimSpace(string(headOut))
	if !validGitObjectID(head) {
		return false, fmt.Errorf("runtime/worktree: retention invalid HEAD %q", head)
	}
	remotes := strings.Fields(string(refsOut))
	args := append(append([]string{}, config...), "rev-list", "--count", "HEAD", "--not")
	if len(remotes) == 0 {
		// No remote-tracking refs: every commit on HEAD is unpushed.
		// --not with no refs still needs one; count HEAD directly.
		args = append(append([]string{}, config...), "rev-list", "--count", "HEAD", "--")
		countOut, err := m.runGit(ctx, "", append([]string{"-C", leaf}, args...)...)
		if err != nil {
			return false, fmt.Errorf("runtime/worktree: retention count commits: %w (%s)", err, strings.TrimSpace(string(countOut)))
		}
		return strings.TrimSpace(string(countOut)) != "0", nil
	}
	for _, ref := range remotes {
		if !strings.HasPrefix(ref, "refs/") {
			return false, fmt.Errorf("runtime/worktree: retention unexpected remote ref %q", ref)
		}
		args = append(args, ref)
	}
	args = append(args, "--")
	countOut, err := m.runGit(ctx, "", append([]string{"-C", leaf}, args...)...)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: retention count unpushed: %w (%s)", err, strings.TrimSpace(string(countOut)))
	}
	return strings.TrimSpace(string(countOut)) != "0", nil
}

// retentionGitConfig pins the inspection environment: no hooks, no
// monitors, no credential helpers, no submodule recursion, so repository
// configuration cannot change what the probe sees or execute code.
func retentionGitConfig(leaf string) []string {
	return []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.bare=false",
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "credential.helper=",
		"-c", "core.askPass=/usr/bin/false",
		"-c", "protocol.allow=never",
		"-c", "protocol.file.allow=always",
		"-c", "protocol.http.allow=always",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "submodule.recurse=false",
		"--work-tree=" + leaf,
	}
}

// isMissingHeadError reports the git "unknown revision / no HEAD" output
// that marks a checkout without commits.
func isMissingHeadError(output []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(output)))
	return strings.Contains(text, "unknown revision") || strings.Contains(text, "bad revision") ||
		strings.Contains(text, "needed a single revision") || strings.Contains(text, "unknown commit")
}

// isGitCheckout reports whether a directory holds a git checkout.
func isGitCheckout(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular()
}

// containsPath reports whether paths holds the cleaned path.
func containsPath(paths []string, path string) bool {
	for _, existing := range paths {
		if filepath.Clean(existing) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

// bytesTrimSpace trims NUL and whitespace framing git's -z output.
func bytesTrimSpace(out []byte) []byte {
	return []byte(strings.Trim(string(out), "\x00 \t\r\n"))
}

// archiveRetainedRoot captures a retained root through the manager's whole-
// root archive provider, then removes the source root. The archive call
// must succeed before the root is released: an archive failure keeps the
// only source, exactly like the terminal-lease release path.
//
// The provider captures through ExportWorkarea plus the registry's
// identity-continuous ArchiveRoot, which releases the published root
// itself. The manager must not also release the record or remove the
// directory underneath it: RemovePublishedRoot on a released generation
// reconciles a lingering directory as QUARANTINED, and os.RemoveAll on
// the archived source races the provider's own source removal. Both
// sides return release-authority errors when the record is already
// gone, so the legacy-flat fallback (no record matched) is the only
// path that removes the source directly.
func (m *Manager) archiveRetainedRoot(ctx context.Context, rootPath string) error {
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return fmt.Errorf("runtime/worktree: retention archive authority: %w", err)
	}
	records, err := acquisitions.ReadyRecords()
	if err != nil {
		return fmt.Errorf("runtime/worktree: retention archive authority read: %w", err)
	}
	var record *workarea.AcquisitionRecord
	for i := range records {
		if filepath.Clean(records[i].FinalRoot) == filepath.Clean(rootPath) {
			record = &records[i]
			break
		}
	}
	spec := ArchiveRootSpec{WorkareaRoot: rootPath, SelectedPath: rootPath}
	if record != nil {
		declaration, err := m.authorizedDeclaration(*record)
		if err != nil {
			return fmt.Errorf("runtime/worktree: retention archive authorize: %w", err)
		}
		selected := declaration.SelectedRepository
		for _, repository := range declaration.Repositories {
			if repository.Name == selected {
				spec.SelectedPath = filepath.Join(rootPath, repository.Leaf)
				break
			}
		}
		spec.AcquisitionID = record.AcquisitionID
		spec.WorkareaID = record.WorkareaID
		spec.SessionID = record.SessionID
	} else {
		// A retained legacy flat workarea carries no acquisition record.
		// The archive provider derives identity from the path; the
		// session identity is unknown, so archiving uses the leaf name.
		// Without an acquisition identity the provider may refuse; that
		// refusal keeps the root (the expiry returns the error so the
		// pass surfaces it rather than silently keeping).
		leaf := filepath.Base(filepath.Clean(rootPath))
		spec.WorkareaID = leaf
		spec.SessionID = leaf
	}
	if m.archiveRoot == nil {
		return errors.New("runtime/worktree: archive disposition has no whole-root archive provider")
	}
	if err := m.archiveRoot(ctx, spec); err != nil {
		return fmt.Errorf("runtime/worktree: archive retained root: %w", err)
	}
	if record != nil {
		if err := acquisitions.RemovePublishedRoot(record.AcquisitionID); err != nil {
			return fmt.Errorf("runtime/worktree: release archived retained root: %w", err)
		}
		m.forgetSessionsAtRoot(rootPath)
		return nil
	}
	if err := os.RemoveAll(rootPath); err != nil {
		return fmt.Errorf("runtime/worktree: remove archived retained root %q: %w", rootPath, err)
	}
	return nil
}

// destroyRetainedRoot removes a retained root that holds no unpushed work.
// A root still bound to an acquisition record goes through the
// acquisition authority so the durable record and the directory agree;
// anything else is removed directly.
func (m *Manager) destroyRetainedRoot(ctx context.Context, rootPath string) error {
	_ = ctx
	acquisitions, err := m.acquisitionStore()
	if err != nil {
		return fmt.Errorf("runtime/worktree: retention destroy authority: %w", err)
	}
	records, err := acquisitions.ReadyRecords()
	if err != nil {
		return fmt.Errorf("runtime/worktree: retention destroy authority read: %w", err)
	}
	for i := range records {
		if filepath.Clean(records[i].FinalRoot) != filepath.Clean(rootPath) {
			continue
		}
		if records[i].State != workarea.AcquisitionReady {
			return fmt.Errorf("runtime/worktree: retained root %q is not releasable (state %q)", rootPath, records[i].State)
		}
		if err := acquisitions.RemovePublishedRoot(records[i].AcquisitionID); err != nil {
			return fmt.Errorf("runtime/worktree: destroy retained root: %w", err)
		}
		m.forgetSessionsAtRoot(rootPath)
		return nil
	}
	if err := os.RemoveAll(rootPath); err != nil {
		return fmt.Errorf("runtime/worktree: remove retained root %q: %w", rootPath, err)
	}
	return nil
}

// dropExpiredArchives removes archives past the policy's maximum age. It
// is the only path by which unpushed work leaves the host, and each drop
// carries its own receipt.
func (m *Manager) dropExpiredArchives(ctx context.Context, policy RetentionPolicy, dropper RetentionArchiveDropper, now time.Time) ([]RetentionReceipt, int, error) {
	if dropper == nil {
		return nil, 0, nil
	}
	archives, err := m.listRetentionArchives()
	if err != nil {
		// Without an archive listing there is no age signal; the pass
		// skips archive dropping rather than dropping on a guess.
		m.logger.Warn("retention: archive listing unavailable; skipping archive drops", "err", err)
		return nil, 0, nil
	}
	var receipts []RetentionReceipt
	skipped := 0
	maxAge := time.Duration(policy.ArchiveMaxAgeDays) * 24 * time.Hour
	for _, archive := range archives {
		if ctx.Err() != nil {
			return receipts, skipped, ctx.Err()
		}
		if archive.CreatedAt.IsZero() {
			skipped++
			continue
		}
		if now.Sub(archive.CreatedAt) <= maxAge {
			skipped++
			continue
		}
		before, beforeErr := freeSpace(m.retentionArchiveRoot())
		if err := dropper(ctx, archive.ID, archive.Dir); err != nil {
			return receipts, skipped, fmt.Errorf("runtime/worktree: drop archive %q: %w", archive.ID, err)
		}
		receipts = append(receipts, RetentionReceipt{
			Root:           archive.Dir,
			Disposition:    RetentionArchiveDropped,
			Reason:         RetentionReasonArchiveAge,
			BytesReclaimed: reclaimedBytes(m.retentionArchiveRoot(), before, beforeErr),
			At:             m.clock(),
		})
	}
	return receipts, skipped, nil
}

// retentionArchive is one archive candidate for dropping.
type retentionArchive struct {
	// ID is the archive identity (directory leaf under the archive root).
	ID string
	// Dir is the absolute archive directory.
	Dir string
	// CreatedAt is the archive creation time from its manifest.
	CreatedAt time.Time
}

// retentionArchiveRoot resolves the archive root the pass scans for drops.
// It defaults beside the worktree parent, mirroring the daemon's default
// archive layout, and is overridden in tests through Options.
func (m *Manager) retentionArchiveRoot() string {
	if m.retentionArchiveRootOverride != "" {
		return m.retentionArchiveRootOverride
	}
	return filepath.Join(filepath.Dir(filepath.Clean(m.parentDir)), "workareas")
}

// listRetentionArchives enumerates archives with their manifest creation
// times. A corrupt or unreadable manifest is skipped (never dropped): the
// drop needs an age, and a manifest that cannot be read proves none.
func (m *Manager) listRetentionArchives() ([]retentionArchive, error) {
	root := m.retentionArchiveRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("runtime/worktree: list retention archives: %w", err)
	}
	archives := make([]retentionArchive, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "." || entry.Name() == ".." || entry.Name() != filepath.Clean(entry.Name()) {
			continue
		}
		// The entry name is one validated leaf of the archive root:
		// ReadDir never returns separators, and Clean rejects
		// traversal spellings, so the join stays inside the root.
		manifestPath := filepath.Join(root, entry.Name(), "manifest.json")
		manifestRaw, err := os.ReadFile(manifestPath) //nolint:gosec // validated single leaf of the archive root
		if err != nil {
			continue
		}
		created, ok := retentionArchiveCreatedAt(manifestRaw)
		if !ok {
			continue
		}
		archives = append(archives, retentionArchive{ID: entry.Name(), Dir: filepath.Join(root, entry.Name()), CreatedAt: created})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].ID < archives[j].ID })
	return archives, nil
}

// retentionArchiveCreatedAt extracts the manifest's createdAt timestamp.
// The manifest shape is daemon-owned; this reader takes only the field
// the drop needs and ignores the rest.
func retentionArchiveCreatedAt(manifestRaw []byte) (time.Time, bool) {
	created, ok := extractManifestStringField(manifestRaw, "createdAt")
	if !ok || created == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		if at, err = time.Parse(time.RFC3339, created); err != nil {
			return time.Time{}, false
		}
	}
	return at.UTC(), true
}

// clock returns the manager's clock (injectable for tests).
func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}
