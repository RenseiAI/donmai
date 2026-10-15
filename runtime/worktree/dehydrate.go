// Package worktree — dehydrate.go: retention budget for retained work areas,
// classify and dehydrate.
//
// This file implements the classify-and-dehydrate half of the retention
// budget for retained work areas: roots whose session is durably terminal
// and that survived teardown through preservation are classified against
// the kit-declared dependency stores, an install record is written, and
// after 24 hours the recorded installed trees are removed. Each removal
// carries a receipt naming the policy and the measured free-space change.
//
// A retained root here is a directory directly under the manager's parent
// directory that is none of: leased (an active or release-pending terminal
// lease retains the exact path, which also covers parked-for-resume roots
// whose disposition is still "return this root to its session"), adopted
// (the manager tracks a live session at the root), quarantined (the
// terminal quarantine journal names the path, or an acquisition record
// still binds the root), or unreconciled (a declaration or acquisition
// record describes the root, so its lifecycle is still owned elsewhere).
// Only legacy flat roots — every root on a host that never negotiated the
// versioned layout — are classified: anything carrying a declaration or an
// acquisition record is skipped, which keeps every excluded category
// untouched without enumerating them one by one.
//
// A leaf path qualifies when it matches a kit entry's snapshot.installed
// pattern, git ignores it, and it is not tracked: tracked source is never
// dehydrated, exactly like the archive capture never slims tracked source.
// The install record is written before anything is removed, and dehydration
// removes exactly the recorded paths. The free-space change of each
// transition is measured with the filesystem's free-space counter, not a
// directory walk: copy-on-write clones and reflinks make walks over-report
// what a removal actually frees.
//
// Keeper failures degrade to keeping the root: a store that matches
// nothing classifies nothing, an unreadable leaf is skipped, and a git
// probe failure keeps the root rather than removing on a guess. The pass
// never runs on the session path, so no session waits for it.
package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// DefaultDehydrationAge is how long a classified install record waits
// before its recorded paths are removed: 24 hours.
const DefaultDehydrationAge = 24 * time.Hour

// DehydrateRecordsDirName is the manager-owned state directory under the
// worktree parent holding one install record per classified root. It is a
// dot directory so root enumeration skips it like every other durable
// authority beside the session roots.
const DehydrateRecordsDirName = ".workarea-install-records"

// dehydrateInstallRecordSchema identifies the durable install record.
const dehydrateInstallRecordSchema = "donmai.workarea-install-record.v1"

// dehydrateMaxGitOutput bounds one classification probe's output. A leaf
// whose ignore/track listing exceeds it is skipped: without the complete
// listing there is no safe classification signal.
const dehydrateMaxGitOutput = 1 << 20

// dehydrateMaxPatternVisits bounds one installed-pattern expansion: a
// `**` glob must not walk an unbounded tree to classify it. The walk
// keeps what matched so far and stops, which under-classifies (keeps)
// rather than over-removes.
const dehydrateMaxPatternVisits = 20000

// DehydrateStore is the minimal kit-declared dependency-store input one
// dehydration pass classifies against: the manager name for the receipt,
// the leaf-relative lockfiles selecting the store, and the leaf-relative
// snapshot.installed patterns naming regenerable trees. The daemon
// projects validated kit views into this shape; the engine never parses
// manifests itself.
type DehydrateStore struct {
	// Manager names the package manager (for example "pnpm").
	Manager string
	// Lockfiles are leaf-relative paths or globs selecting the store.
	Lockfiles []string
	// Installed are leaf-relative paths or globs naming regenerable trees.
	Installed []string
}

// DehydratePolicy configures one classify-and-dehydrate pass.
type DehydratePolicy struct {
	// Stores are the kit-declared dependency stores to classify against.
	Stores []DehydrateStore
	// DehydrationAge waits this long after classification before removing
	// the recorded paths. Zero resolves to DefaultDehydrationAge; negative
	// classifies only and never removes.
	DehydrationAge time.Duration
	// Now fixes the pass clock. Zero reads the manager clock.
	Now time.Time
}

// DehydrateReason names the policy that made a root eligible.
type DehydrateReason string

const (
	// DehydrateReasonAge means the install record exceeded
	// DehydratePolicy.DehydrationAge.
	DehydrateReasonAge DehydrateReason = "age"
)

// DehydrateDisposition names how a dehydrated root left its installed trees.
type DehydrateDisposition string

const (
	// DehydrateDispositionDehydrated means the recorded installed paths
	// were removed and the receipt was written.
	DehydrateDispositionDehydrated DehydrateDisposition = "dehydrated"
)

// InstallRecordStore is one classifying store persisted on the record.
type InstallRecordStore struct {
	Manager   string   `json:"manager"`
	Lockfiles []string `json:"lockfiles"`
	Installed []string `json:"installed"`
}

// InstallDehydration is the receipt section added to the install record
// when its paths are removed: it names the policy (reason) and the
// measured free-space change (bytesReclaimed).
type InstallDehydration struct {
	At             time.Time            `json:"at"`
	Reason         DehydrateReason      `json:"reason"`
	Disposition    DehydrateDisposition `json:"disposition"`
	BytesReclaimed int64                `json:"bytesReclaimed"`
	Paths          []string             `json:"paths"`
}

// InstallRecord is the durable classification of one retained root: the
// leaf-relative installed paths that qualified (matched a store's
// snapshot.installed, ignored by git, untracked), written before anything
// is removed. Dehydration removes exactly these paths.
type InstallRecord struct {
	SchemaVersion string               `json:"schemaVersion"`
	Root          string               `json:"root"`
	ClassifiedAt  time.Time            `json:"classifiedAt"`
	Head          string               `json:"head,omitempty"`
	Stores        []InstallRecordStore `json:"stores"`
	Paths         []string             `json:"paths"`
	Dehydration   *InstallDehydration  `json:"dehydration,omitempty"`
}

// DehydrateReceipt is the per-transition record for one dehydration.
type DehydrateReceipt struct {
	// Root is the retained root whose installed trees were removed.
	Root string
	// Reason is the policy that made the root eligible.
	Reason DehydrateReason
	// Disposition is how the installed trees left the host.
	Disposition DehydrateDisposition
	// BytesReclaimed is the measured free-space change of the transition.
	BytesReclaimed int64
	// At is when the transition completed.
	At time.Time
	// Paths are the leaf-relative installed paths removed.
	Paths []string
}

// DehydrateReport summarizes one classify-and-dehydrate pass.
type DehydrateReport struct {
	// Receipts holds one receipt per completed dehydration, in completion
	// order.
	Receipts []DehydrateReceipt
	// BytesReclaimed is the measured free-space change across the pass.
	BytesReclaimed int64
	// Considered counts candidate roots the pass inspected.
	Considered int
	// Skipped counts candidate roots left in place (live, unexpired,
	// unclassifiable, or probe failure degrading to keep).
	Skipped int
	// Classified counts roots whose install record was written or
	// refreshed.
	Classified int
}

// DehydratePreservedRoots performs one bounded classify-and-dehydrate pass
// over the manager's parent directory: every candidate root is classified
// against the policy's stores (writing or refreshing its install record),
// and roots whose record exceeds the dehydration age are dehydrated.
//
// Live roots — leased, release-pending, parked, adopted, quarantined, or
// otherwise lifecycle-owned — are untouched, as is anything the pass
// cannot prove safe: an unreadable leaf, a failed git probe, or a keeper
// failure keeps the root. Only pass-level authority failures (the lease or
// quarantine journal cannot be read at all) surface as errors.
func (m *Manager) DehydratePreservedRoots(ctx context.Context, policy DehydratePolicy) (*DehydrateReport, error) {
	now := policy.Now
	if now.IsZero() {
		now = m.dehydrateClock()
	}
	age := policy.DehydrationAge
	if age == 0 {
		age = DefaultDehydrationAge
	}
	report := &DehydrateReport{}
	roots, err := m.enumeratePreservedRoots()
	if err != nil {
		return nil, err
	}
	report.Considered = len(roots)
	quarantined, err := m.dehydrateQuarantinedPaths()
	if err != nil {
		return nil, err
	}
	records, err := workarea.ReadExistingReadyRecords(m.parentDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: dehydration acquisition read: %w", err)
	}
	bound := make(map[string]struct{}, len(records))
	for i := range records {
		bound[filepath.Clean(records[i].FinalRoot)] = struct{}{}
	}
	for _, root := range roots {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		classified, receipt, kept := m.dehydratePreservedRoot(ctx, root, policy, now, age, quarantined, bound)
		if classified {
			report.Classified++
		}
		if receipt != nil {
			report.Receipts = append(report.Receipts, *receipt)
			report.BytesReclaimed += receipt.BytesReclaimed
		}
		if kept {
			report.Skipped++
		}
	}
	return report, nil
}

// DehydrateInstallRecord returns the durable install record for one root,
// or an error wrapping fs.ErrNotExist when the root was never classified.
func (m *Manager) DehydrateInstallRecord(root string) (*InstallRecord, error) {
	clean := filepath.Clean(root)
	raw, err := os.ReadFile(m.dehydrateRecordPath(clean)) //nolint:gosec // path is the manager-owned records dir joined with the validated root leaf
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: read install record for %q: %w", clean, err)
	}
	record, err := parseInstallRecord(raw)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: parse install record for %q: %w", clean, err)
	}
	if filepath.Clean(record.Root) != clean {
		return nil, fmt.Errorf("runtime/worktree: install record for %q names root %q", clean, record.Root)
	}
	return record, nil
}

// dehydratePreservedRoot classifies one candidate root and dehydrates it
// when its record is due. It returns classified=true when the record was
// written or refreshed, a non-nil receipt when paths were removed, and
// kept=true when the root stays on disk.
func (m *Manager) dehydratePreservedRoot(ctx context.Context, root string, policy DehydratePolicy, now time.Time, age time.Duration, quarantined map[string]struct{}, bound map[string]struct{}) (bool, *DehydrateReceipt, bool) {
	live, err := m.preservedRootLive(root, quarantined, bound)
	if err != nil {
		// Liveness is unprovable: the lease check failed. The root
		// stays; a later pass retries.
		m.logger.Warn("dehydration: liveness unprovable; keeping preserved root",
			"path", root, "err", err)
		return false, nil, true
	}
	if live {
		return false, nil, true
	}
	qualified, head, err := m.classifyPreservedRoot(ctx, root, policy.Stores)
	if err != nil {
		// The leaf cannot be classified (not a checkout, unreadable, or
		// a git probe failed). Keeping is the only safe direction.
		m.logger.Warn("dehydration: classification failed; keeping preserved root",
			"path", root, "err", err)
		return false, nil, true
	}
	if len(qualified) == 0 {
		return false, nil, true
	}
	record, err := m.refreshInstallRecord(root, qualified, head, policy.Stores, now)
	if err != nil {
		m.logger.Warn("dehydration: install record unwritable; keeping preserved root",
			"path", root, "err", err)
		return false, nil, true
	}
	if age < 0 || now.Sub(record.ClassifiedAt) < age {
		return true, nil, true
	}
	receipt, err := m.dehydrateRecordedRoot(ctx, root, record, now)
	if err != nil {
		m.logger.Warn("dehydration: removal failed; keeping preserved root",
			"path", root, "err", err)
		return true, nil, true
	}
	if receipt == nil {
		return true, nil, true
	}
	return true, receipt, false
}

// preservedRootLive reports whether a root is still lifecycle-owned and
// must not be classified or dehydrated: leased (an active or
// release-pending terminal lease retains the exact path, which covers
// parked-for-resume roots), adopted (the manager tracks a live session at
// the root), quarantined (the terminal quarantine journal names the path,
// or an acquisition record still binds the root), or unreconciled (a
// declaration or acquisition record describes the root).
func (m *Manager) preservedRootLive(root string, quarantined map[string]struct{}, bound map[string]struct{}) (bool, error) {
	clean := filepath.Clean(root)
	retained, err := m.retainedPath(clean)
	if err != nil {
		return false, fmt.Errorf("runtime/worktree: dehydration liveness lease check: %w", err)
	}
	if retained {
		return true, nil
	}
	if _, ok := quarantined[clean]; ok {
		return true, nil
	}
	m.mu.Lock()
	for _, res := range m.sessions {
		if res != nil && filepath.Clean(res.workareaRootOrPath()) == clean {
			m.mu.Unlock()
			return true, nil
		}
	}
	m.mu.Unlock()
	if _, ok := bound[clean]; ok {
		return true, nil
	}
	if _, err := os.Lstat(filepath.Join(clean, workarea.DeclarationDirName, workarea.DeclarationFileName)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("runtime/worktree: dehydration declaration check: %w", err)
	}
	return false, nil
}

// enumeratePreservedRoots lists candidate roots: real directories directly
// under the parent directory, excluding the manager's durable authorities
// (every dot directory — the acquisition, seed, lease, quarantine,
// receiver, and install-record stores — since no retained root is ever a
// dot directory). Roots adopted concurrently between listing and expiry
// are still safe: liveness is rechecked per root at classification time.
func (m *Manager) enumeratePreservedRoots() ([]string, error) {
	entries, err := os.ReadDir(m.parentDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: list preserved roots: %w", err)
	}
	roots := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !entry.IsDir() {
			continue
		}
		root := filepath.Join(m.parentDir, name)
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots, nil
}

// dehydrateQuarantinedPaths returns the set of workarea paths named by the
// terminal quarantine journal. A journal entry reports a workarea the
// lease authority proved unacquirable; dehydration must not touch it even
// though no lease retains it.
func (m *Manager) dehydrateQuarantinedPaths() (map[string]struct{}, error) {
	items, err := m.leases.Quarantines()
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: dehydration quarantine read: %w", err)
	}
	out := make(map[string]struct{}, len(items))
	for i := range items {
		if items[i].WorkareaPath != "" {
			out[filepath.Clean(items[i].WorkareaPath)] = struct{}{}
		}
	}
	return out, nil
}

// classifyPreservedRoot computes the leaf-relative installed paths that
// qualify for dehydration: they match an applicable store's
// snapshot.installed pattern, git ignores them, and they are not tracked.
// It returns the qualified paths (sorted) and the leaf's HEAD commit for
// the record's freshness signal.
func (m *Manager) classifyPreservedRoot(ctx context.Context, root string, stores []DehydrateStore) ([]string, string, error) {
	if !dehydrateIsGitCheckout(root) {
		return nil, "", fmt.Errorf("runtime/worktree: preserved root %q holds no git checkout", root)
	}
	applicable := make([]DehydrateStore, 0, len(stores))
	for _, store := range stores {
		matched, err := m.dehydrateStoreApplies(root, store)
		if err != nil {
			return nil, "", err
		}
		if matched {
			applicable = append(applicable, store)
		}
	}
	if len(applicable) == 0 {
		return nil, "", nil
	}
	candidates := make(map[string]struct{})
	for _, store := range applicable {
		for _, pattern := range store.Installed {
			matches, err := m.expandInstalledPattern(root, pattern)
			if err != nil {
				return nil, "", err
			}
			for _, match := range matches {
				rel, err := filepath.Rel(root, match)
				if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
					continue
				}
				candidates[filepath.ToSlash(rel)] = struct{}{}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, "", nil
	}
	rels := make([]string, 0, len(candidates))
	for rel := range candidates {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	ignored, err := m.dehydrateIgnoredPaths(ctx, root, rels)
	if err != nil {
		return nil, "", err
	}
	if len(ignored) == 0 {
		return nil, "", nil
	}
	tracked, err := m.dehydrateTrackedPaths(ctx, root, ignored)
	if err != nil {
		return nil, "", err
	}
	qualified := make([]string, 0, len(ignored))
	for _, rel := range ignored {
		if _, isTracked := tracked[rel]; !isTracked {
			qualified = append(qualified, rel)
		}
	}
	if len(qualified) == 0 {
		return nil, "", nil
	}
	head, _ := m.dehydrateLeafHEAD(ctx, root)
	return qualified, head, nil
}

// dehydrateStoreApplies reports whether a store applies to a leaf: one of
// its lockfiles expands to an existing path. A store with no lockfiles
// never applies; an unreadable leaf fails closed.
func (m *Manager) dehydrateStoreApplies(leaf string, store DehydrateStore) (bool, error) {
	if store.Manager == "" || len(store.Lockfiles) == 0 {
		return false, nil
	}
	for _, pattern := range store.Lockfiles {
		matches, err := m.expandInstalledPattern(leaf, pattern)
		if err != nil {
			return false, err
		}
		if len(matches) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// expandInstalledPattern expands one leaf-relative installed or lockfile
// pattern to absolute matches. Literal patterns resolve directly;
// patterns carrying glob metacharacters walk the leaf without following
// symlinks and without descending into .git. A pattern that could match
// the leaf itself is refused: the recorded paths must always stay inside
// the leaf. Matches covered by a matched ancestor are pruned: removing
// the ancestor removes them.
func (m *Manager) expandInstalledPattern(leaf, pattern string) ([]string, error) {
	if err := checkDehydratePattern(pattern); err != nil {
		return nil, err
	}
	abs := filepath.Join(leaf, filepath.FromSlash(pattern))
	if !strings.ContainsAny(pattern, "*?[\\") {
		_, lstatErr := os.Lstat(abs)
		if lstatErr == nil {
			return []string{abs}, nil
		}
		if errors.Is(lstatErr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("runtime/worktree: inspect installed path %q: %w", pattern, lstatErr)
	}
	segments := strings.Split(pattern, "/")
	expander := &dehydrateExpander{visits: 0}
	matches := expander.expand(leaf, segments)
	sort.Strings(matches)
	return pruneCoveredPaths(matches), expander.err
}

// checkDehydratePattern requires a leaf-relative, contained,
// forward-slash pattern. It mirrors the manifest validation the daemon
// applies before projecting stores, so a corrupt record or a hostile
// projection cannot carry an escape into removal.
func checkDehydratePattern(pattern string) error {
	if pattern == "" || pattern == "." {
		return fmt.Errorf("runtime/worktree: installed pattern is empty")
	}
	if filepath.IsAbs(pattern) || strings.Contains(pattern, "\\") {
		return fmt.Errorf("runtime/worktree: installed pattern %q is not a relative forward-slash path", pattern)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(pattern)))
	if clean != pattern {
		return fmt.Errorf("runtime/worktree: installed pattern %q is not in canonical form", pattern)
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == ".." {
			return fmt.Errorf("runtime/worktree: installed pattern %q escapes the leaf", pattern)
		}
	}
	if pattern == "**" {
		return fmt.Errorf("runtime/worktree: installed pattern %q matches the leaf itself", pattern)
	}
	return nil
}

// dehydrateExpander carries the visit budget for one glob expansion.
type dehydrateExpander struct {
	visits int
	err    error
}

// expand walks dir matching segments, collecting absolute matches.
func (e *dehydrateExpander) expand(dir string, segments []string) []string {
	if len(segments) == 0 {
		return []string{dir}
	}
	segment := segments[0]
	rest := segments[1:]
	if segment == "**" {
		out := e.expand(dir, rest)
		e.expandStarStar(dir, rest, &out)
		return out
	}
	if !strings.ContainsAny(segment, "*?[\\") {
		next := filepath.Join(dir, segment)
		info, err := os.Lstat(next)
		if err != nil {
			return nil
		}
		if len(rest) > 0 {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			return e.expand(next, rest)
		}
		return []string{next}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		ok, err := filepath.Match(segment, name)
		if err != nil || !ok {
			continue
		}
		next := filepath.Join(dir, name)
		if len(rest) == 0 {
			out = append(out, next)
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		out = append(out, e.expand(next, rest)...)
	}
	return out
}

// expandStarStar matches `**` against zero or more directories under dir,
// never following symlinks and never descending into .git. A directory
// that itself completes the pattern is not descended further: removing it
// removes everything beneath it.
func (e *dehydrateExpander) expandStarStar(dir string, rest []string, out *[]string) {
	e.visits++
	if e.visits > dehydrateMaxPatternVisits {
		if e.err == nil {
			e.err = fmt.Errorf("runtime/worktree: installed pattern expansion exceeded %d directory visits", dehydrateMaxPatternVisits)
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}
		next := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		before := len(*out)
		*out = append(*out, e.expand(next, rest)...)
		if len(*out) == before {
			e.expandStarStar(next, rest, out)
		}
	}
}

// pruneCoveredPaths drops matches that sit beneath another match:
// removing the ancestor removes them.
func pruneCoveredPaths(matches []string) []string {
	if len(matches) == 0 {
		return matches
	}
	kept := matches[:0]
	for _, match := range matches {
		clean := filepath.Clean(match)
		covered := false
		for _, parent := range kept {
			if clean != parent && strings.HasPrefix(clean, parent+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, clean)
		}
	}
	return kept
}

// dehydrateGitConfig pins the classification inspection environment: no
// hooks, no monitors, no credential helpers, no submodule recursion, so
// repository configuration cannot change what the probe sees or execute
// code.
func dehydrateGitConfig() []string {
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
	}
}

// dehydrateIgnoredPaths reports which of the leaf-relative candidates git
// ignores, in one check-ignore invocation over stdin. check-ignore exits 1
// when no input is ignored, which is not an error; it echoes only the
// ignored inputs, so the caller treats the output as the ignored set.
func (m *Manager) dehydrateIgnoredPaths(ctx context.Context, leaf string, rels []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	args := append(dehydrateGitConfig(), "-C", leaf, "check-ignore", "-z", "--stdin")
	var stdin []byte
	for _, rel := range rels {
		stdin = append(stdin, rel...)
		stdin = append(stdin, 0)
	}
	out, err := m.runPublicationGitInput(ctx, "", stdin, args...)
	if err != nil && len(dehydrateTrimNUL(out)) != 0 {
		return nil, fmt.Errorf("runtime/worktree: check ignored installed paths: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if len(out) > dehydrateMaxGitOutput {
		return nil, fmt.Errorf("runtime/worktree: ignored installed listing exceeded %d bytes", dehydrateMaxGitOutput)
	}
	if err != nil {
		return nil, nil
	}
	ignored := []string{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		ignored = append(ignored, filepath.ToSlash(name))
	}
	sort.Strings(ignored)
	return ignored, nil
}

// dehydrateTrackedPaths reports which of the leaf-relative candidates are
// tracked (present in the index). Tracked source is never dehydrated, even
// when an ignore rule also matches it.
func (m *Manager) dehydrateTrackedPaths(ctx context.Context, leaf string, rels []string) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	args := append(dehydrateGitConfig(), "-C", leaf, "ls-files", "-z", "--")
	args = append(args, rels...)
	out, err := m.runGit(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("runtime/worktree: list tracked installed paths: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if len(out) > dehydrateMaxGitOutput {
		return nil, fmt.Errorf("runtime/worktree: tracked installed listing exceeded %d bytes", dehydrateMaxGitOutput)
	}
	tracked := make(map[string]struct{}, len(rels))
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		tracked[filepath.ToSlash(name)] = struct{}{}
	}
	return tracked, nil
}

// dehydrateLeafHEAD resolves the leaf's HEAD commit for the record's
// freshness signal. An unresolvable HEAD (empty checkout, missing git) is
// not a classification failure: the record simply carries no HEAD.
func (m *Manager) dehydrateLeafHEAD(ctx context.Context, leaf string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	args := append(dehydrateGitConfig(), "-C", leaf, "rev-parse", "--verify", "HEAD^{commit}")
	out, err := m.runGit(ctx, "", args...)
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(out))
	if !validGitObjectID(head) {
		return "", fmt.Errorf("runtime/worktree: invalid HEAD %q", head)
	}
	return head, nil
}

// refreshInstallRecord writes the classification before anything is
// removed. A record with the same paths and HEAD keeps its original
// classification time so steady roots dehydrate on schedule; changed
// content restarts the 24-hour clock.
func (m *Manager) refreshInstallRecord(root string, qualified []string, head string, stores []DehydrateStore, now time.Time) (*InstallRecord, error) {
	clean := filepath.Clean(root)
	previous, err := m.DehydrateInstallRecord(clean)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	record := &InstallRecord{
		SchemaVersion: dehydrateInstallRecordSchema,
		Root:          clean,
		ClassifiedAt:  now.UTC(),
		Head:          head,
		Paths:         append([]string(nil), qualified...),
	}
	for _, store := range stores {
		matched, err := m.dehydrateStoreApplies(clean, store)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		record.Stores = append(record.Stores, InstallRecordStore{
			Manager:   store.Manager,
			Lockfiles: append([]string(nil), store.Lockfiles...),
			Installed: append([]string(nil), store.Installed...),
		})
	}
	if previous != nil && previous.SchemaVersion == record.SchemaVersion &&
		slicesEqualString(previous.Paths, record.Paths) && previous.Head == record.Head {
		record.ClassifiedAt = previous.ClassifiedAt
		record.Dehydration = previous.Dehydration
	}
	if err := m.writeInstallRecord(record); err != nil {
		return nil, err
	}
	return record, nil
}

// dehydrateRecordedRoot removes exactly the recorded installed paths and
// writes the receipt naming the policy and the measured free-space change.
// A path that no longer exists is already dehydrated; a path that became
// tracked since classification is kept. Removal is skipped entirely when
// nothing remains.
func (m *Manager) dehydrateRecordedRoot(ctx context.Context, root string, record *InstallRecord, now time.Time) (*DehydrateReceipt, error) {
	leaf := filepath.Clean(root)
	tracked, err := m.dehydrateTrackedPaths(ctx, leaf, record.Paths)
	if err != nil {
		return nil, err
	}
	removable := make([]string, 0, len(record.Paths))
	for _, rel := range record.Paths {
		if _, isTracked := tracked[rel]; isTracked {
			m.logger.Warn("dehydration: recorded path became tracked; keeping",
				"path", filepath.Join(leaf, rel))
			continue
		}
		removable = append(removable, rel)
	}
	targets := make([]string, 0, len(removable))
	for _, rel := range removable {
		abs, exists, err := resolveDehydratePath(leaf, rel)
		if err != nil {
			m.logger.Warn("dehydration: recorded path unsafe; keeping",
				"path", rel, "err", err)
			continue
		}
		if !exists {
			continue
		}
		targets = append(targets, abs)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	before, beforeErr := dehydrateFreeSpace(m.parentDir)
	removed := make([]string, 0, len(targets))
	for i, abs := range targets {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The path is confined inside the manager-owned retained root:
		// every segment was validated at classification and re-resolved
		// without following intermediate symlinks above.
		if err := os.RemoveAll(abs); err != nil { //nolint:gosec // confined removal, see above
			return nil, fmt.Errorf("runtime/worktree: remove installed path %q: %w", removable[i], err)
		}
		removed = append(removed, removable[i])
	}
	receipt := &DehydrateReceipt{
		Root:           leaf,
		Reason:         DehydrateReasonAge,
		Disposition:    DehydrateDispositionDehydrated,
		BytesReclaimed: dehydratedReclaimedBytes(m.parentDir, before, beforeErr),
		At:             now.UTC(),
		Paths:          removed,
	}
	record.Dehydration = &InstallDehydration{
		At:             receipt.At,
		Reason:         receipt.Reason,
		Disposition:    receipt.Disposition,
		BytesReclaimed: receipt.BytesReclaimed,
		Paths:          append([]string(nil), removed...),
	}
	if err := m.writeInstallRecord(record); err != nil {
		return nil, fmt.Errorf("runtime/worktree: write dehydration receipt: %w", err)
	}
	return receipt, nil
}

// resolveDehydratePath re-resolves one recorded leaf-relative path to an
// absolute removal target. Intermediate segments must be real directories:
// a symlinked ancestor would redirect removal outside the leaf, so it
// fails closed. A missing path reports exists=false: it is already
// dehydrated. The final segment may be any file type: removing a symlink
// removes the link, never its target.
func resolveDehydratePath(leaf, rel string) (abs string, exists bool, err error) {
	if rel == "" || rel == "." || filepath.IsAbs(rel) {
		return "", false, fmt.Errorf("installed path %q is not a leaf-relative path", rel)
	}
	current := leaf
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false, fmt.Errorf("installed path %q escapes the leaf", rel)
		}
		current = filepath.Join(current, segment)
	}
	current = filepath.Clean(current)
	if !strings.HasPrefix(current, leaf+string(filepath.Separator)) {
		return "", false, fmt.Errorf("installed path %q escapes the leaf", rel)
	}
	parent := filepath.Dir(current)
	relParent, err := filepath.Rel(leaf, parent)
	if err != nil {
		return "", false, fmt.Errorf("installed path %q escapes the leaf", rel)
	}
	currentCheck := leaf
	if relParent != "." {
		for _, segment := range strings.Split(relParent, string(filepath.Separator)) {
			currentCheck = filepath.Join(currentCheck, segment)
			info, err := os.Lstat(currentCheck)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return "", false, nil
				}
				return "", false, fmt.Errorf("inspect installed path %q: %w", rel, err)
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", false, fmt.Errorf("installed path %q crosses a non-directory", rel)
			}
		}
	}
	if _, err := os.Lstat(current); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect installed path %q: %w", rel, err)
	}
	return current, true, nil
}

// dehydrateRecordsDir ensures the manager-owned install-record directory.
func (m *Manager) dehydrateRecordsDir() (string, error) {
	dir := filepath.Join(m.parentDir, DehydrateRecordsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runtime/worktree: create install record directory: %w", err)
	}
	return dir, nil
}

// dehydrateRecordPath maps one root to its install record file. The leaf
// name is validated to a single path segment so the join cannot escape
// the records directory.
func (m *Manager) dehydrateRecordPath(root string) string {
	leaf := filepath.Base(filepath.Clean(root))
	if leaf == "." || leaf == ".." || leaf == "" || strings.ContainsAny(leaf, `/\`) {
		leaf = "invalid-root"
	}
	return filepath.Join(m.parentDir, DehydrateRecordsDirName, leaf+".json")
}

// writeInstallRecord persists one install record atomically (temp file
// plus rename) with mode 0600.
func (m *Manager) writeInstallRecord(record *InstallRecord) error {
	dir, err := m.dehydrateRecordsDir()
	if err != nil {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("runtime/worktree: encode install record: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".install-record-*.tmp")
	if err != nil {
		return fmt.Errorf("runtime/worktree: create install record temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime/worktree: chmod install record: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime/worktree: write install record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtime/worktree: sync install record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtime/worktree: close install record: %w", err)
	}
	if err := os.Rename(tmpPath, m.dehydrateRecordPath(record.Root)); err != nil {
		return fmt.Errorf("runtime/worktree: publish install record: %w", err)
	}
	return nil
}

// parseInstallRecord decodes and validates one install record: the schema
// is closed, the root is absolute, and every recorded path is a
// leaf-relative canonical path that cannot escape the leaf.
func parseInstallRecord(raw []byte) (*InstallRecord, error) {
	record, err := decodeInstallRecord(raw)
	if err != nil {
		return nil, err
	}
	if record.SchemaVersion != dehydrateInstallRecordSchema {
		return nil, fmt.Errorf("runtime/worktree: install record schema %q is not %q", record.SchemaVersion, dehydrateInstallRecordSchema)
	}
	if !filepath.IsAbs(record.Root) {
		return nil, fmt.Errorf("runtime/worktree: install record root is not absolute")
	}
	if record.ClassifiedAt.IsZero() {
		return nil, fmt.Errorf("runtime/worktree: install record has no classification time")
	}
	seen := make(map[string]struct{}, len(record.Paths))
	for _, rel := range record.Paths {
		if rel == "" || rel == "." || filepath.IsAbs(rel) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))) != rel {
			return nil, fmt.Errorf("runtime/worktree: install record path %q is not canonical leaf-relative form", rel)
		}
		for _, segment := range strings.Split(rel, "/") {
			if segment == ".." {
				return nil, fmt.Errorf("runtime/worktree: install record path %q escapes the leaf", rel)
			}
		}
		if _, duplicate := seen[rel]; duplicate {
			return nil, fmt.Errorf("runtime/worktree: install record path %q is duplicated", rel)
		}
		seen[rel] = struct{}{}
	}
	return record, nil
}

// decodeInstallRecord decodes one install record with unknown fields
// rejected: a record carrying a key the reader does not know may mean a
// newer writer, and dehydrating on a half-understood record could remove
// paths the writer classified under different rules.
func decodeInstallRecord(raw []byte) (*InstallRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record InstallRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("runtime/worktree: decode install record: %w", err)
	}
	if err := ensureInstallRecordEOF(decoder); err != nil {
		return nil, err
	}
	return &record, nil
}

func ensureInstallRecordEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("runtime/worktree: trailing install record value")
		}
		return fmt.Errorf("runtime/worktree: trailing install record data: %w", err)
	}
	return nil
}

// dehydrateIsGitCheckout reports whether a directory holds a git checkout.
func dehydrateIsGitCheckout(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular()
}

// dehydrateTrimNUL trims NUL and whitespace framing git's -z output.
func dehydrateTrimNUL(out []byte) []byte {
	return []byte(strings.Trim(string(out), "\x00 \t\r\n"))
}

// slicesEqualString reports whether two string slices hold the same
// elements in order.
func slicesEqualString(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dehydrateClock returns the manager's clock (injectable for tests).
func (m *Manager) dehydrateClock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// dehydrateFreeSpace reports the bytes available to unprivileged processes
// on the filesystem holding path. An error leaves the receipt at zero
// reclaimed rather than failing the transition: the disposal already
// happened, and a stat failure must not turn it into a pass failure.
func dehydrateFreeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// dehydratedReclaimedBytes returns the free-space change across a
// transition: the after sample minus the before sample. A failed before
// sample, a failed after sample, or a negative delta (concurrent writers
// filled the disk during the transition) reports zero rather than a
// guess.
func dehydratedReclaimedBytes(path string, before uint64, beforeErr error) int64 {
	if beforeErr != nil {
		return 0
	}
	after, err := dehydrateFreeSpace(path)
	if err != nil {
		return 0
	}
	if after < before {
		return 0
	}
	delta := after - before
	const maxInt64 = int64(^uint64(0) >> 1)
	if delta > uint64(maxInt64) {
		return maxInt64
	}
	return int64(delta)
}
