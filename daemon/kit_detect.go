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
	"errors"
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
	// [supports].os short-circuit (005:296) — never run detect on an
	// incompatible platform.
	return r.detectForRepo(repoRoot, func(supported []string) bool {
		return osSupported(supported, targetOS)
	})
}

// DetectForRepoAnyOS mirrors DetectForRepo but returns every kit whose
// declarative [detect] matchers pass, regardless of [supports].os.
//
// DetectForRepo's short-circuit is a placement-time correctness check: it
// answers "does this kit apply to the OS I already chose". Deriving the
// pre-placement PlacementDemand (kit_demand.go) needs the opposite —
// EVERY candidate kit's declared support, so DeriveDemand can intersect
// them itself. Pre-filtering by one OS before detection would bias the
// derived demand toward whatever OS happened to be passed in (typically
// the local host's), which is exactly the "gates run after placement"
// defect this signal exists to fix (005 § "Platform compatibility").
func (r *KitRegistry) DetectForRepoAnyOS(repoRoot string) ([]kit.ManifestView, error) {
	return r.detectForRepo(repoRoot, func([]string) bool { return true })
}

// detectForRepo is the shared implementation behind DetectForRepo and
// DetectForRepoAnyOS. osFilter receives each candidate manifest's
// [supports].os slice and decides whether it's even considered for
// declarative detection.
func (r *KitRegistry) detectForRepo(repoRoot string, osFilter func(supported []string) bool) ([]kit.ManifestView, error) {
	manifests, manifestPaths := r.scanWithPaths()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, id := range state.DisabledIDs {
		disabled[id] = struct{}{}
	}

	var matched []kit.ManifestView
	for i, m := range manifests {
		if _, off := disabled[m.Kit.ID]; off {
			continue
		}
		if !osFilter(m.Supports.OS) {
			continue
		}
		if !detectMatches(m, repoRoot) {
			continue
		}
		packageDigest, legacyDigest, err := commandOwnerDigests(manifestPaths[i])
		if err != nil {
			return nil, fmt.Errorf("kit detect: command owner digest for %s: %w", m.Kit.ID, err)
		}
		matched = append(matched, manifestToView(m, packageDigest, legacyDigest))
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
func manifestToView(m kitManifestTOML, packageDigest, legacyDigest string) kit.ManifestView {
	v := kit.ManifestView{
		ID:                   m.Kit.ID,
		Version:              m.Kit.Version,
		Priority:             m.Kit.Priority,
		Order:                m.Composition.Order,
		SupportedOS:          copyStrings(m.Supports.OS),
		SupportedArch:        copyStrings(m.Supports.Arch),
		PackageDigest:        packageDigest,
		LegacyManifestDigest: legacyDigest,
		PathScope:            ".",
		Commands:             copyStringMap(m.Provide.Commands),
		CommandsOverride:     copyStringMapMap(m.Provide.CommandsOverride),
		ToolchainInstall:     copyStringMapMap(m.Provide.ToolchainInstall),
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
	// Project [provide.prompt_fragments] into the view so the runner can
	// inject workType-filtered fragment bodies into the system prompt (step 5a).
	if len(m.Provide.PromptFragments) > 0 {
		v.PromptFragments = make([]kit.PromptFragmentEntry, 0, len(m.Provide.PromptFragments))
		for _, pf := range m.Provide.PromptFragments {
			v.PromptFragments = append(v.PromptFragments, kit.PromptFragmentEntry{
				Partial: pf.Partial,
				When:    copyStrings(pf.When),
				File:    pf.File,
			})
		}
	}
	if len(m.Provide.Lanes) > 0 {
		v.Lanes = make([]kit.LaneView, 0, len(m.Provide.Lanes))
		for _, lane := range m.Provide.Lanes {
			v.Lanes = append(v.Lanes, kit.LaneView{
				Name: lane.Name,
				OS:   copyStrings(lane.OS),
				Arch: copyStrings(lane.Arch),
			})
		}
	}
	return v
}

// commandOwnerDigest returns the immutable package digest when a manifest is
// generation-bound. Legacy manifests use the exact manifest-content SHA-256 so
// command ownership remains deterministic without upgrading their trust state.
func commandOwnerDigests(manifestPath string) (packageDigest, legacyDigest string, err error) {
	if isPackageManifestPath(manifestPath) {
		digest := filepath.Base(filepath.Dir(manifestPath))
		if !isCanonicalSHA256(digest) {
			return "", "", errors.New("invalid package digest path")
		}
		return digest, "", nil
	}
	raw, err := os.ReadFile(manifestPath) //nolint:gosec // path came from the registry's validated scan result
	if err != nil {
		return "", "", err
	}
	return "", sha256Hex(raw), nil
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
