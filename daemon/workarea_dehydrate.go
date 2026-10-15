// Package daemon workarea_dehydrate.go — retention-budget dehydration sweep
// wiring retained roots to the kit-declared dependency stores.
//
// The sweep classifies each retained root against the kit-declared
// dependency stores (slice 1's lockfile selection over the composed
// stores), writes the install record, and after 24 hours removes exactly
// the recorded installed paths. Each removal carries a receipt naming the
// policy and the measured free-space change.
//
// The pass never makes a session wait: leased, parked, adopted,
// quarantined and unprovable roots are untouched, and every keeper
// failure degrades to keeping the root — today's install behavior.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// DehydrateSweepInterval is the period between dehydration passes on a
// running daemon. Removal waits 24 hours after classification regardless,
// so a daily pass bounds the work while keeping the budget tight.
const DehydrateSweepInterval = 24 * time.Hour

// DehydratePassReport summarizes one dehydration sweep: the
// per-transition receipts and the measured free-space change.
type DehydratePassReport struct {
	// Dehydrated counts retained roots whose recorded installed paths
	// the pass removed.
	Dehydrated int
	// BytesReclaimed is the measured free-space change across the pass.
	BytesReclaimed int64
	// At is when the pass completed.
	At time.Time
}

// DehydrationStores resolves the kit-declared dependency stores applying
// to one retained leaf: detect, compose, and select exactly as the
// session path does, but read-only — nothing is installed, and a leaf
// whose selection is ambiguous selects nothing (the keeper failure
// degrades to today's install: the leaf keeps everything).
func (r *KitRegistry) DehydrationStores(leaf string, targetOS string) ([]kit.ComposedDependencyStore, error) {
	if r == nil {
		return nil, nil
	}
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
	}
	views, err := r.DetectForRepo(leaf, targetOS)
	if err != nil {
		return nil, fmt.Errorf("dehydration stores: detect: %w", err)
	}
	if len(views) == 0 {
		return nil, nil
	}
	lock, err := r.loadCompositionLock()
	if err != nil {
		return nil, fmt.Errorf("dehydration stores: lock: %w", err)
	}
	target := kit.CompositionTarget{OS: targetOS, PathScope: "."}
	composed, err := kit.ComposeDependencyStores(views, target, lock)
	if err != nil {
		return nil, fmt.Errorf("dehydration stores: compose: %w", err)
	}
	facts, err := leafDependencyFacts(leaf)
	if err != nil {
		return nil, fmt.Errorf("dehydration stores: leaf facts: %w", err)
	}
	selected, err := kit.SelectDependencyStores(composed.Stores, facts)
	if err != nil {
		// An ambiguous leaf (two stores of one ecosystem matching with
		// no leaf declaration deciding) selects nothing: the pass keeps
		// the root rather than guessing which installed tree is safe.
		return nil, nil
	}
	return selected, nil
}

// leafDependencyFacts reads the mutable-leaf facts selection needs: the
// leaf's top-level file names and its own package-manager declaration.
// Only the top level is listed — lockfiles live at the leaf root, and
// the pass must not walk whole trees just to select a store.
func leafDependencyFacts(leaf string) (kit.LeafDependencyFacts, error) {
	entries, err := os.ReadDir(leaf)
	if err != nil {
		return kit.LeafDependencyFacts{}, err
	}
	facts := kit.LeafDependencyFacts{}
	for _, entry := range entries {
		if !entry.IsDir() {
			facts.Files = append(facts.Files, entry.Name())
		}
	}
	sort.Strings(facts.Files)
	declaration, err := leafPackageManagerDeclaration(leaf, facts.Files)
	if err == nil {
		facts.PackageManager = declaration
	}
	return facts, nil
}

// leafPackageManagerDeclaration reads the leaf's own manager declaration
// (a package.json packageManager field) when present. Absence selects
// nothing — it is not an error. The body is size-bounded and decoded
// with unknown fields rejected: only a plain top-level string value
// counts.
func leafPackageManagerDeclaration(leaf string, files []string) (string, error) {
	for _, name := range files {
		if name != "package.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(leaf, name)) //nolint:gosec // leaf is the manager-owned retained root under classification
		if err != nil {
			return "", nil
		}
		if len(raw) > 1<<20 {
			return "", fmt.Errorf("dehydration stores: package.json exceeds 1MiB")
		}
		var doc struct {
			PackageManager string `json:"packageManager"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return "", nil
		}
		return doc.PackageManager, nil
	}
	return "", nil
}

// RunDehydratePass runs one bounded dehydration pass over the worktree
// parent: it classifies each retained root against the registry's
// kit-declared dependency stores and dehydrates roots whose install
// record exceeds the configured age. targetOS is retained for the
// per-leaf store resolution the sweep will thread through once the
// engine selects per leaf; today the union pass is OS-neutral. A nil
// registry or nil manager is a no-op returning a zero report. Keeper
// failures degrade to keeping the root; only pass-level authority
// failures surface as errors.
func RunDehydratePass(ctx context.Context, manager *worktree.Manager, registry *KitRegistry, _ string, settings RetentionSettings) (*DehydratePassReport, error) {
	report := &DehydratePassReport{}
	if manager == nil || registry == nil {
		return report, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stores, err := registry.dehydrationStoresForPass()
	if err != nil {
		return nil, err
	}
	age := time.Duration(settings.DehydrationAgeHours) * time.Hour
	result, err := manager.DehydratePreservedRoots(ctx, worktree.DehydratePolicy{
		Stores: stores, DehydrationAge: age,
	})
	if err != nil {
		return nil, err
	}
	for _, receipt := range result.Receipts {
		if receipt.Disposition == worktree.DehydrateDispositionDehydrated {
			report.Dehydrated++
		}
		report.BytesReclaimed += receipt.BytesReclaimed
	}
	report.At = time.Now().UTC()
	slog.Info("dehydration sweep complete",
		"dehydrated", report.Dehydrated,
		"bytesReclaimed", report.BytesReclaimed,
	)
	return report, nil
}

// dehydrationStoresForPass projects every validated kit store into the
// engine's minimal input. Selection still happens per leaf at
// classification time (lockfile presence decides); the union here only
// bounds which managers the pass may name on a record.
func (r *KitRegistry) dehydrationStoresForPass() ([]worktree.DehydrateStore, error) {
	manifests, _ := r.scanWithPaths()
	seen := make(map[string]worktree.DehydrateStore)
	for i := range manifests {
		views, err := dependencyStoreViews(manifests[i])
		if err != nil {
			// A manifest that fails validation is already excluded
			// from detection; skip it here rather than failing the
			// whole pass on one kit.
			continue
		}
		for _, view := range views {
			key := view.Manager
			merged, ok := seen[key]
			if !ok {
				merged = worktree.DehydrateStore{Manager: view.Manager}
			}
			merged.Lockfiles = unionStrings(merged.Lockfiles, view.Lockfiles)
			merged.Installed = unionStrings(merged.Installed, view.Snapshot.Installed)
			seen[key] = merged
		}
	}
	stores := make([]worktree.DehydrateStore, 0, len(seen))
	for _, store := range seen {
		if store.Manager == "" || len(store.Lockfiles) == 0 || len(store.Installed) == 0 {
			continue
		}
		stores = append(stores, store)
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].Manager < stores[j].Manager })
	return stores, nil
}

func unionStrings(base, add []string) []string {
	seen := make(map[string]struct{}, len(base)+len(add))
	out := append([]string(nil), base...)
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range add {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// RunDehydrateSweeper runs the dehydration sweep until ctx is cancelled,
// starting with one pass and repeating every interval. A sweep failure is
// reported through onError (when non-nil) and never stops the loop: the
// next interval retries.
func RunDehydrateSweeper(ctx context.Context, manager *worktree.Manager, registry *KitRegistry, targetOS string, settings RetentionSettings, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = DehydrateSweepInterval
	}
	sweep := func() {
		if _, err := RunDehydratePass(ctx, manager, registry, targetOS, settings); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
