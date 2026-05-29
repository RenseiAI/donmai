// Package daemon — kit_detect.go: declarative kit detection + foundation-first
// ordering (K1.2). Bridges the daemon's KitRegistry into the runner's compose layer
// by producing []kit.ManifestView from the kits whose [detect] matchers
// pass against a repo root.
//
// Scope (005 § "Detection lifecycle" Phase 1): declarative matchers only —
// files (any exists), files_all (all exist), not_files (none exist).
// content_matches and [detect].exec (Phase 2) are deferred to a later wave;
// a kit relying solely on those will not match here yet. Respects
// [supports].os short-circuit (005:284-298) and the registry's persisted
// disabled-state filter (mirrors List).
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/RenseiAI/donmai/internal/kit"
)

// ErrKitFoundationConflict is returned by DetectForRepo when more than one
// foundation kit matches unmediated (005 § "Confidence and selection"
// step 1 — conflicting kits both apply). Two foundation kits cannot both
// own the base toolchain layer; the operator must disable one.
var ErrKitFoundationConflict = fmt.Errorf("kit detect: multiple foundation kits matched")

// DetectForRepo runs the declarative [detect] matchers of every active
// (non-disabled, OS-compatible) kit against repoRoot and returns the
// matching manifests as kit.ManifestView, ordered foundation → framework →
// project (by [composition].order, priority, then id). The result feeds
// kit.Compose to build a ToolchainDemand.
//
// targetOS gates [supports].os: pass the SANDBOX OS (linux for cloud) — for
// the local runner path use kit.MustResolveOS(). An empty targetOS falls
// back to the host OS.
//
// Returns ErrKitFoundationConflict when >1 foundation kit matches. A repo
// with no matching kits returns (nil, nil) — the caller skips provisioning.
func (r *KitRegistry) DetectForRepo(repoRoot, targetOS string) ([]kit.ManifestView, error) {
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
	}

	manifests := r.scan()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, id := range state.DisabledIDs {
		disabled[id] = struct{}{}
	}

	var matched []kit.ManifestView
	for _, m := range manifests {
		if _, off := disabled[m.Kit.ID]; off {
			continue
		}
		// [supports].os short-circuit (005:296) — never run detect on an
		// incompatible platform.
		if !osSupported(m.Supports.OS, targetOS) {
			continue
		}
		if !detectMatches(m, repoRoot) {
			continue
		}
		matched = append(matched, manifestToView(m))
	}

	// Order foundation → framework → project (deterministic within group).
	ordered := kit.SortManifests(matched)

	// Reject >1 unmediated foundation match (005 § selection step 1).
	foundations := 0
	for _, v := range ordered {
		if v.Order == "foundation" {
			foundations++
		}
	}
	if foundations > 1 {
		ids := make([]string, 0, foundations)
		for _, v := range ordered {
			if v.Order == "foundation" {
				ids = append(ids, v.ID)
			}
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("%w: %v", ErrKitFoundationConflict, ids)
	}

	return ordered, nil
}

// detectMatches evaluates the declarative [detect] matchers for one
// manifest against repoRoot. Semantics (005 § "Detection lifecycle"):
//   - files: match if ANY listed file exists.
//   - files_all: match only if ALL listed files exist.
//   - not_files: exclusion — match fails if ANY listed file exists.
//
// A manifest with no positive matcher (files / files_all both empty) does
// NOT match in Phase-1-only detection — it would need [detect].exec or
// content_matches (deferred). not_files alone never selects a kit.
func detectMatches(m kitManifestTOML, repoRoot string) bool {
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, rel))
		return err == nil
	}

	// not_files exclusion runs first — any present excluder fails the match.
	for _, f := range m.Detect.NotFiles {
		if exists(f) {
			return false
		}
	}

	hasPositive := len(m.Detect.Files) > 0 || len(m.Detect.FilesAll) > 0
	if !hasPositive {
		return false
	}

	if len(m.Detect.FilesAll) > 0 {
		for _, f := range m.Detect.FilesAll {
			if !exists(f) {
				return false
			}
		}
	}

	if len(m.Detect.Files) > 0 {
		anyMatch := false
		for _, f := range m.Detect.Files {
			if exists(f) {
				anyMatch = true
				break
			}
		}
		if !anyMatch {
			return false
		}
	}

	return true
}

// manifestToView projects a parsed manifest into the exported view the
// compose layer consumes (K1.2 bridge — keeps kitManifestTOML
// daemon-private while the runner imports only internal/kit).
func manifestToView(m kitManifestTOML) kit.ManifestView {
	v := kit.ManifestView{
		ID:               m.Kit.ID,
		Version:          m.Kit.Version,
		Priority:         m.Kit.Priority,
		Order:            m.Composition.Order,
		SupportedOS:      copyStrings(m.Supports.OS),
		ToolchainInstall: copyStringMapMap(m.Provide.ToolchainInstall),
		Hooks: kit.HooksView{
			PostAcquire: m.Provide.Hooks.PostAcquire,
			PreRelease:  m.Provide.Hooks.PreRelease,
		},
	}
	if len(m.Provide.Hooks.OS) > 0 {
		v.Hooks.OS = make(map[string]kit.HookOSView, len(m.Provide.Hooks.OS))
		for osKey, e := range m.Provide.Hooks.OS {
			v.Hooks.OS[osKey] = kit.HookOSView{
				PostAcquire: e.PostAcquire,
				PreRelease:  e.PreRelease,
			}
		}
	}
	return v
}

// osSupported reports whether supported (from [supports].os) admits
// targetOS. Empty supported = any OS (permissive).
func osSupported(supported []string, targetOS string) bool {
	if len(supported) == 0 {
		return true
	}
	for _, s := range supported {
		if s == targetOS {
			return true
		}
	}
	return false
}
