// Package daemon — kit_dependency_entries.go: the kit registry as the
// dependency preparer's entry source.
//
// runtime/depprep asks an EntrySource for the composed dependency-store
// entries of a leaf. The registry answers from the installed kits: every
// enabled kit that supports the leaf's OS contributes each entry whose
// lockfiles the leaf holds, whether or not the kit's [detect] rules match
// the leaf. Dependency stores are selected by lockfile, so a plain
// JavaScript repository with a pnpm lockfile and no tsconfig.json still gets
// the TypeScript kit's pnpm entries. Detection, foundation conflicts and
// command composition play no part here.
//
// The registry does not select among the entries it returns: the preparer
// picks the manager (the leaf's packageManager decides between managers of
// one ecosystem) and then the entry whose versions range covers the leaf's
// pinned or reported manager version.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/depprep"
)

// The kit registry is the preparer's entry source.
var _ depprep.EntrySource = (*KitRegistry)(nil)

// Entries implements depprep.EntrySource. It composes the dependency-store
// entries of every enabled kit that supports leaf.OS (the host OS when
// empty) and declares an entry whose lockfiles the leaf holds. Kit
// detection is not consulted. Entries of one manager from several kits
// compose when their versions ranges are disjoint; overlapping ranges
// conflict unless the operator's composition lock selects one kit for the
// target {OS: leaf.OS, PathScope: "."} (no work type: the preparer serves
// callers that have none). A conflict, an unreadable lock or an invalid
// eligible entry is an error, which the preparer reports as
// entries_unavailable. A leaf no kit's entry matches gets no entries and no
// error.
func (r *KitRegistry) Entries(ctx context.Context, leaf depprep.Leaf) ([]depprep.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if leaf.Root == "" || !filepath.IsAbs(leaf.Root) {
		return nil, fmt.Errorf("kit dependency entries: leaf root %q is not an absolute path", leaf.Root)
	}
	targetOS := leaf.OS
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
	}
	switch targetOS {
	case kit.OSLinux, kit.OSMacOS, kit.OSWindows:
	default:
		return nil, fmt.Errorf("kit dependency entries: unknown OS %q", targetOS)
	}

	views, err := r.eligibleDependencyViews(leaf.Root, targetOS)
	if err != nil {
		return nil, fmt.Errorf("kit dependency entries: %w", err)
	}
	if len(views) == 0 {
		return nil, nil
	}
	target := kit.CompositionTarget{OS: targetOS, PathScope: "."}
	lock, err := r.loadCompositionLock()
	if err != nil {
		return nil, fmt.Errorf("kit dependency entries: %w", err)
	}
	plan, err := kit.ComposeDependencyStores(views, target, dependencyLockFor(lock, target, views))
	if err != nil {
		return nil, fmt.Errorf("kit dependency entries: %w", err)
	}
	entries := make([]depprep.Entry, 0, len(plan.Stores))
	for _, store := range plan.Stores {
		entries = append(entries, depprepEntry(store))
	}
	return entries, nil
}

// eligibleDependencyViews returns, for every enabled kit that supports
// targetOS, a view carrying only the dependency-store entries whose
// lockfiles the leaf holds. Kits with none are left out. An eligible entry
// that fails validation is an error; an ineligible one is ignored, as are
// kits whose manifests the scan already excluded.
func (r *KitRegistry) eligibleDependencyViews(root, targetOS string) ([]kit.ManifestView, error) {
	manifests, manifestPaths := r.scanWithPaths()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, id := range state.DisabledIDs {
		disabled[id] = struct{}{}
	}
	var views []kit.ManifestView
	for i, m := range manifests {
		if len(m.Provide.DependencyStores) == 0 {
			continue
		}
		if _, off := disabled[m.Kit.ID]; off || !osSupported(m.Supports.OS, targetOS) {
			continue
		}
		var eligible []kit.DependencyStoreView
		for j := range m.Provide.DependencyStores {
			store := projectDependencyStore(m.Provide.DependencyStores[j])
			if leafHoldsAny(root, store.Lockfiles) {
				eligible = append(eligible, store)
			}
		}
		if len(eligible) == 0 {
			continue
		}
		if err := kit.ValidateDependencyStores(eligible); err != nil {
			return nil, fmt.Errorf("kit %s: %w", m.Kit.ID, err)
		}
		packageDigest, legacyDigest, err := commandOwnerDigests(manifestPaths[i])
		if err != nil {
			return nil, fmt.Errorf("command owner digest for %s: %w", m.Kit.ID, err)
		}
		view := manifestToView(m, packageDigest, legacyDigest, commandOwnerPackageRoot(manifestPaths[i], packageDigest))
		view.DependencyStores = eligible
		views = append(views, view)
	}
	return kit.SortManifests(views), nil
}

// leafHoldsAny reports whether the leaf holds one of the lockfile patterns:
// a literal path that exists, or a single-level glob that matches. A "**"
// pattern is not a lockfile shape and never matches. The preparer applies
// the same rule again before it selects.
func leafHoldsAny(root string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(p, "**") {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.ContainsAny(p, "*?[") {
			if matches, err := filepath.Glob(full); err == nil && len(matches) > 0 {
				return true
			}
			continue
		}
		if _, err := os.Lstat(full); err == nil {
			return true
		}
	}
	return false
}

// dependencyLockFor narrows the operator's lock to the dependency bindings
// that can apply to this leaf: the target's bindings for managers some
// eligible kit declares. A binding for a manager the leaf does not use is
// not stale for this leaf, so it must not fail its composition. Command
// bindings are kept as written; dependency composition does not read them.
func dependencyLockFor(lock *kit.CompositionLock, target kit.CompositionTarget, views []kit.ManifestView) *kit.CompositionLock {
	if lock == nil {
		return nil
	}
	declared := map[string]struct{}{}
	for _, v := range views {
		for _, s := range v.DependencyStores {
			declared[s.Manager] = struct{}{}
		}
	}
	out := &kit.CompositionLock{Schema: lock.Schema, Targets: make([]kit.LockedCompositionTarget, 0, len(lock.Targets))}
	for _, entry := range lock.Targets {
		if entry.Target.OS == target.OS && entry.Target.WorkType == target.WorkType && entry.Target.PathScope == target.PathScope {
			var kept []kit.LockedDependencyBinding
			for _, b := range entry.Dependencies {
				if _, ok := declared[b.Manager]; ok {
					kept = append(kept, b)
				}
			}
			entry.Dependencies = kept
		}
		out.Targets = append(out.Targets, entry)
	}
	return out
}

// depprepEntry converts one composed store into the preparer's own entry
// type. A declared [no_code] block stays non-nil even when it sets nothing;
// an absent one stays nil.
func depprepEntry(s kit.ComposedDependencyStore) depprep.Entry {
	e := depprep.Entry{
		Manager:        s.Manager,
		Ecosystem:      s.Ecosystem,
		Versions:       s.Versions,
		KitID:          s.KitID,
		KitDigestKind:  s.DigestKind,
		KitDigest:      s.Digest,
		PackageRoot:    s.PackageRoot,
		Lockfiles:      copyStrings(s.Lockfiles),
		Inputs:         copyStrings(s.Inputs),
		VersionCommand: s.Version,
		Commands: depprep.Commands{
			Fetch:     s.Commands[kit.DependencyStoreCommandFetch],
			Install:   s.Commands[kit.DependencyStoreCommandInstall],
			Online:    s.Commands[kit.DependencyStoreCommandOnline],
			Reconcile: s.Commands[kit.DependencyStoreCommandReconcile],
			Verify:    s.Commands[kit.DependencyStoreCommandVerify],
			Relocate:  s.Commands[kit.DependencyStoreCommandRelocate],
		},
		Store: depprep.StoreLayout{
			Env:         copyStrings(s.Store.Env),
			Sharing:     s.Store.Sharing,
			Integrity:   s.Store.Integrity,
			Content:     copyStrings(s.Store.Content),
			Bookkeeping: copyStrings(s.Store.Bookkeeping),
			Secrets:     copyStrings(s.Store.Secrets),
			NeverShare:  copyStrings(s.Store.NeverShare),
			RecordsPath: s.Store.RecordsPath,
		},
		ImportEnv:       copyStringMap(s.ImportEnv),
		Installed:       copyStrings(s.Snapshot.Installed),
		RunsPackageCode: s.Snapshot.RunsPackageCode,
	}
	if s.NoCode != nil {
		env := copyStringMap(s.NoCode.Env)
		if env == nil {
			env = map[string]string{}
		}
		e.NoCode = &depprep.NoCode{Env: env}
	}
	if s.Credentials != nil {
		e.Credentials = &depprep.CredentialsSpec{ConfigEnv: s.Credentials.ConfigEnv}
	}
	return e
}
