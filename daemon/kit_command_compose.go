package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/RenseiAI/donmai/internal/kit"
)

const (
	kitCompositionLockName  = ".composition.lock.json"
	maxCompositionLockBytes = 1 << 20
)

// ComposeForRepo performs command ownership preflight only once the runner
// knows the exact repository, OS, work type, and path scope. Package
// installation remains target-neutral; sessions fail before any kit-controlled
// provisioning or command executes when ownership is ambiguous.
func (r *KitRegistry) ComposeForRepo(repoRoot string, target kit.CompositionTarget, selected []kit.Selection) (*kit.ToolchainDemand, error) {
	var (
		views []kit.ManifestView
		err   error
	)
	if len(selected) > 0 {
		views, err = r.activeViewsForExactSelections(target.OS, selected)
	} else {
		views, err = r.DetectForRepo(repoRoot, target.OS)
	}
	if err != nil {
		return nil, err
	}
	lock, err := r.loadCompositionLock()
	if err != nil {
		return nil, err
	}
	return kit.ComposeForTarget(views, target, lock, nil)
}

// activeViewsForExactSelections loads platform-selected active manifests
// without applying repository detection predicates. Explicit platform
// lifecycle selection is authoritative over detection, while local active
// state, exact version, target OS, and package integrity remain fail-closed.
func (r *KitRegistry) activeViewsForExactSelections(targetOS string, selected []kit.Selection) ([]kit.ManifestView, error) {
	wanted, err := exactSelectionMap(selected)
	if err != nil {
		return nil, err
	}
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
	}
	manifests, manifestPaths := r.scanWithPaths()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, id := range state.DisabledIDs {
		disabled[id] = struct{}{}
	}

	views := make([]kit.ManifestView, 0, len(selected))
	for i, manifest := range manifests {
		key := manifest.Kit.ID + "\x00" + manifest.Kit.Version
		if _, selected := wanted[key]; !selected {
			continue
		}
		if _, off := disabled[manifest.Kit.ID]; off || !osSupported(manifest.Supports.OS, targetOS) {
			continue
		}
		packageDigest, legacyDigest, err := commandOwnerDigests(manifestPaths[i])
		if err != nil {
			return nil, fmt.Errorf("kit compose: command owner digest for %s: %w", manifest.Kit.ID, err)
		}
		views = append(views, manifestToView(manifest, packageDigest, legacyDigest))
	}
	return selectExactKitViews(kit.SortManifests(views), selected)
}

func selectExactKitViews(views []kit.ManifestView, selected []kit.Selection) ([]kit.ManifestView, error) {
	wanted, err := exactSelectionMap(selected)
	if err != nil {
		return nil, err
	}
	filtered := make([]kit.ManifestView, 0, len(selected))
	for _, view := range views {
		key := view.ID + "\x00" + view.Version
		if _, ok := wanted[key]; ok {
			filtered = append(filtered, view)
			delete(wanted, key)
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for _, selection := range wanted {
			missing = append(missing, selection.ID+"@"+selection.Version)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("selected kits unavailable for exact command preflight: %v", missing)
	}
	return filtered, nil
}

func exactSelectionMap(selected []kit.Selection) (map[string]kit.Selection, error) {
	wanted := make(map[string]kit.Selection, len(selected))
	for _, selection := range selected {
		if selection.ID == "" || selection.Version == "" {
			return nil, errors.New("platform kit selection must include exact id and version")
		}
		key := selection.ID + "\x00" + selection.Version
		if _, duplicate := wanted[key]; duplicate {
			return nil, fmt.Errorf("duplicate platform kit selection %s@%s", selection.ID, selection.Version)
		}
		wanted[key] = selection
	}
	return wanted, nil
}

func (r *KitRegistry) loadCompositionLock() (*kit.CompositionLock, error) {
	if len(r.scanPaths) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(r.scanPaths[0])
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open operator kit store: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(kitCompositionLockName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kitCompositionLockName, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", kitCompositionLockName, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCompositionLockBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", kitCompositionLockName, maxCompositionLockBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxCompositionLockBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kitCompositionLockName, err)
	}
	if len(raw) > maxCompositionLockBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", kitCompositionLockName, maxCompositionLockBytes)
	}
	lock, err := kit.ParseCompositionLock(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", kitCompositionLockName, err)
	}
	return lock, nil
}
