// Package kit — dependency_store.go: kit-declared dependency stores (D1).
//
// A kit declares each package manager it supports as one
// [[provide.dependency_store]] entry. This file is the slice-1 engine:
// validation of a single entry, composition of entries across kits (union
// keyed by manager, conflict unless the composition lock selects one), and
// selection of the applicable entry for a mutable leaf (by lockfile
// presence, falling back to the leaf's own declaration or manager_ambiguous).
//
// The keeper, filler, views, snapshots, budgets and events that consume the
// composed result are later slices; this file only parses, validates,
// composes and selects. Nothing here executes a manager command or touches
// a store.
package kit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// DependencyStoreCompositionSchema identifies the canonical resolved
// dependency-store plan digest input.
const DependencyStoreCompositionSchema = "donmai.dev/kit-dependency-store-composition/v1"

var (
	// ErrDependencyStoreInvalid marks a manifest entry that fails
	// validation (missing required fields, bad integrity/sharing, unsafe
	// store paths, overlapping secrets/content, unknown commands).
	ErrDependencyStoreInvalid = errors.New("kit dependency store invalid")
	// ErrDependencyStoreConflict marks two kits declaring the same
	// manager without a composition-lock selection.
	ErrDependencyStoreConflict = errors.New("kit dependency store conflict")
	// ErrDependencyStoreAmbiguous marks two entries of one ecosystem
	// matching a leaf with no leaf declaration to decide between them.
	// The message carries the closed miss-reason token manager_ambiguous.
	ErrDependencyStoreAmbiguous = errors.New("kit dependency store ambiguous")
)

// Dependency-store integrity modes. Only on-use and on-fetch may back a
// shared store; none forces session-only exposure.
const (
	DependencyStoreIntegrityOnUse   = "on-use"
	DependencyStoreIntegrityOnFetch = "on-fetch"
	DependencyStoreIntegrityNone    = "none"
)

// Dependency-store sharing modes.
const (
	DependencyStoreSharingContentAddressed = "content-addressed"
	DependencyStoreSharingSessionOnly      = "session-only"
)

// Dependency-store command names. fetch and install are required;
// reconcile defaults to install when absent.
const (
	DependencyStoreCommandFetch     = "fetch"
	DependencyStoreCommandInstall   = "install"
	DependencyStoreCommandOnline    = "online"
	DependencyStoreCommandReconcile = "reconcile"
	DependencyStoreCommandVerify    = "verify"
	DependencyStoreCommandRelocate  = "relocate"
)

// dependencyStoreCommands is the closed command vocabulary for a store
// entry. Unknown keys are authoring typos; fail closed rather than carry
// a dead key the keeper would silently ignore.
var dependencyStoreCommands = map[string]struct{}{
	DependencyStoreCommandFetch:     {},
	DependencyStoreCommandInstall:   {},
	DependencyStoreCommandOnline:    {},
	DependencyStoreCommandReconcile: {},
	DependencyStoreCommandVerify:    {},
	DependencyStoreCommandRelocate:  {},
}

// dependencyStoreOSKeys is the closed OS universe a commands_override
// overlay may specialize, mirroring the manifest's platform keys.
var dependencyStoreOSKeys = map[string]struct{}{
	OSLinux:   {},
	OSMacOS:   {},
	OSWindows: {},
}

// DependencyStoreLayout is the [provide.dependency_store.store] block:
// where the manager keeps its store and which subtrees are shareable,
// per-session, secret-bearing, or tainted by package code.
type DependencyStoreLayout struct {
	Env         []string          `json:"env,omitempty"`
	Default     map[string]string `json:"default,omitempty"`
	Sharing     string            `json:"sharing,omitempty"`
	Integrity   string            `json:"integrity,omitempty"`
	Content     []string          `json:"content,omitempty"`
	Bookkeeping []string          `json:"bookkeeping,omitempty"`
	Secrets     []string          `json:"secrets,omitempty"`
	NeverShare  []string          `json:"neverShare,omitempty"`
	RecordsPath bool              `json:"recordsPath,omitempty"`
}

// DependencyStoreSnapshot is the [provide.dependency_store.snapshot]
// block: which leaf paths an install creates and whether a pristine tree
// is snapshot-restorable.
type DependencyStoreSnapshot struct {
	Installed       []string `json:"installed,omitempty"`
	ABI             []string `json:"abi,omitempty"`
	Relocation      string   `json:"relocation,omitempty"`
	RunsPackageCode bool     `json:"runsPackageCode,omitempty"`
}

// DependencyStoreView is one [[provide.dependency_store]] entry projected
// into the daemon-independent view type. Commands holds the generic
// command per name; CommandsOverride holds OS specializations that win
// over the generic form for the same store owned by the same kit.
type DependencyStoreView struct {
	Manager          string                       `json:"manager"`
	Ecosystem        string                       `json:"ecosystem,omitempty"`
	Lockfiles        []string                     `json:"lockfiles,omitempty"`
	Inputs           []string                     `json:"inputs,omitempty"`
	Version          string                       `json:"version,omitempty"`
	Store            DependencyStoreLayout        `json:"store"`
	ImportEnv        map[string]string            `json:"importEnv,omitempty"`
	Commands         map[string]string            `json:"commands,omitempty"`
	CommandsOverride map[string]map[string]string `json:"commandsOverride,omitempty"`
	Snapshot         DependencyStoreSnapshot      `json:"snapshot"`
	ProxyEnv         map[string]string            `json:"proxyEnv,omitempty"`
}

// ComposedDependencyStore is one manager's resolved store: the winning
// kit's entry with commands specialized for the composition target OS.
type ComposedDependencyStore struct {
	Manager    string                  `json:"manager"`
	KitID      string                  `json:"kitId"`
	DigestKind string                  `json:"digestKind"`
	Digest     string                  `json:"digest"`
	Ecosystem  string                  `json:"ecosystem,omitempty"`
	Lockfiles  []string                `json:"lockfiles,omitempty"`
	Inputs     []string                `json:"inputs,omitempty"`
	Version    string                  `json:"version,omitempty"`
	Store      DependencyStoreLayout   `json:"store"`
	ImportEnv  map[string]string       `json:"importEnv,omitempty"`
	Commands   map[string]string       `json:"commands"`
	Snapshot   DependencyStoreSnapshot `json:"snapshot"`
	ProxyEnv   map[string]string       `json:"proxyEnv,omitempty"`
}

// DependencyStoreComposition is the canonical resolved store plan for one
// target. Digest binds the exact managers, owners and commands for
// diagnostics and audit evidence.
type DependencyStoreComposition struct {
	Schema string                    `json:"schema"`
	Target CompositionTarget         `json:"target"`
	Stores []ComposedDependencyStore `json:"stores"`
	Digest string                    `json:"digest"`
}

// LockedDependencyBinding is one operator-approved manager selection: for
// the enclosing lock target, manager resolves to the named kit's entry.
type LockedDependencyBinding struct {
	Manager       string `json:"manager"`
	SelectedKitID string `json:"selectedKitId"`
}

// LeafDependencyFacts are theMutable-leaf facts selection needs: which
// repo-relative paths the leaf holds and the leaf's own manager
// declaration (Node packageManager, "" when the ecosystem has none).
type LeafDependencyFacts struct {
	Files          []string
	PackageManager string
}

// ValidateDependencyStore validates one entry. manager, lockfiles, fetch
// and install are required; integrity is on-use, on-fetch or none with
// none forcing session-only; store paths are relative and contained;
// secrets and never_share may not overlap content; override commands must
// specialize a same-entry base command on a known OS.
func ValidateDependencyStore(store DependencyStoreView) error {
	if store.Manager == "" {
		return fmt.Errorf("%w: manager is required", ErrDependencyStoreInvalid)
	}
	if len(store.Lockfiles) == 0 {
		return fmt.Errorf("%w: %s lockfiles are required", ErrDependencyStoreInvalid, store.Manager)
	}
	for _, lockfile := range store.Lockfiles {
		if lockfile == "" {
			return fmt.Errorf("%w: %s has an empty lockfile entry", ErrDependencyStoreInvalid, store.Manager)
		}
	}
	fetch, fetchOK := store.Commands[DependencyStoreCommandFetch]
	if !fetchOK || fetch == "" {
		return fmt.Errorf("%w: %s fetch command is required", ErrDependencyStoreInvalid, store.Manager)
	}
	install, installOK := store.Commands[DependencyStoreCommandInstall]
	if !installOK || install == "" {
		return fmt.Errorf("%w: %s install command is required", ErrDependencyStoreInvalid, store.Manager)
	}
	switch store.Store.Integrity {
	case DependencyStoreIntegrityOnUse, DependencyStoreIntegrityOnFetch:
	case DependencyStoreIntegrityNone:
		if store.Store.Sharing != DependencyStoreSharingSessionOnly {
			return fmt.Errorf("%w: %s declares integrity none without session-only sharing",
				ErrDependencyStoreInvalid, store.Manager)
		}
	default:
		return fmt.Errorf("%w: %s has unknown integrity %q", ErrDependencyStoreInvalid, store.Manager, store.Store.Integrity)
	}
	switch store.Store.Sharing {
	case DependencyStoreSharingContentAddressed, DependencyStoreSharingSessionOnly:
	default:
		return fmt.Errorf("%w: %s has unknown sharing %q", ErrDependencyStoreInvalid, store.Manager, store.Store.Sharing)
	}
	for _, named := range []struct {
		name  string
		paths []string
	}{
		{"content", store.Store.Content},
		{"bookkeeping", store.Store.Bookkeeping},
		{"secrets", store.Store.Secrets},
		{"never_share", store.Store.NeverShare},
	} {
		for _, p := range named.paths {
			if err := checkStorePath(store.Manager, named.name, p); err != nil {
				return err
			}
		}
	}
	excluded := append(append([]string{}, store.Store.Secrets...), store.Store.NeverShare...)
	for _, guarded := range excluded {
		for _, shared := range store.Store.Content {
			if storePathsOverlap(guarded, shared) {
				return fmt.Errorf("%w: %s path %q overlaps shared content %q",
					ErrDependencyStoreInvalid, store.Manager, guarded, shared)
			}
		}
	}
	for name := range store.Commands {
		if _, ok := dependencyStoreCommands[name]; !ok {
			return fmt.Errorf("%w: %s has unknown command %q", ErrDependencyStoreInvalid, store.Manager, name)
		}
	}
	for osKey, overlay := range store.CommandsOverride {
		if _, ok := dependencyStoreOSKeys[osKey]; !ok {
			return fmt.Errorf("%w: %s has commands for unknown OS %q", ErrDependencyStoreInvalid, store.Manager, osKey)
		}
		for name := range overlay {
			if _, ok := dependencyStoreCommands[name]; !ok {
				return fmt.Errorf("%w: %s has unknown command %q for OS %q",
					ErrDependencyStoreInvalid, store.Manager, name, osKey)
			}
			if _, ok := store.Commands[name]; !ok {
				return fmt.Errorf("%w: %s OS %q override %q has no same-entry base command",
					ErrDependencyStoreInvalid, store.Manager, osKey, name)
			}
		}
	}
	if relocation := store.Snapshot.Relocation; relocation != "" && relocation != "reconcile" && relocation != "none" {
		return fmt.Errorf("%w: %s has unknown snapshot relocation %q", ErrDependencyStoreInvalid, store.Manager, relocation)
	}
	return nil
}

// ValidateDependencyStores validates each entry of one kit and rejects a
// kit declaring the same manager twice. Cross-kit duplicates are not a
// validation error; they are composition conflicts resolved by the lock.
func ValidateDependencyStores(stores []DependencyStoreView) error {
	seen := make(map[string]struct{}, len(stores))
	for i := range stores {
		if err := ValidateDependencyStore(stores[i]); err != nil {
			return err
		}
		if stores[i].Manager == "" {
			continue
		}
		if _, ok := seen[stores[i].Manager]; ok {
			return fmt.Errorf("%w: duplicate manager %q in one kit", ErrDependencyStoreInvalid, stores[i].Manager)
		}
		seen[stores[i].Manager] = struct{}{}
	}
	return nil
}

// checkStorePath requires a relative, contained, forward-slash store path.
func checkStorePath(manager, field, p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("%w: %s has an empty %s path", ErrDependencyStoreInvalid, manager, field)
	}
	if path.IsAbs(p) || strings.Contains(p, "\\") {
		return fmt.Errorf("%w: %s %s path %q is not relative forward-slash form",
			ErrDependencyStoreInvalid, manager, field, p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: %s %s path %q escapes the store", ErrDependencyStoreInvalid, manager, field, p)
	}
	return nil
}

// storePathsOverlap reports whether two normalized store-relative paths
// overlap: equal, or one containing the other on a segment boundary.
func storePathsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ResolveDependencyStoreCommands returns the entry's commands specialized
// for targetOS: the OS-keyed overlay wins over the generic form per
// command, and reconcile defaults to install when the entry omits it.
func ResolveDependencyStoreCommands(store DependencyStoreView, targetOS string) map[string]string {
	resolved := make(map[string]string, len(store.Commands))
	for name, cmd := range store.Commands {
		resolved[name] = cmd
	}
	for name, cmd := range store.CommandsOverride[targetOS] {
		resolved[name] = cmd
	}
	if _, ok := resolved[DependencyStoreCommandReconcile]; !ok {
		if install, ok := resolved[DependencyStoreCommandInstall]; ok && install != "" {
			resolved[DependencyStoreCommandReconcile] = install
		}
	}
	return resolved
}

// ComposeDependencyStores composes every OS-applicable entry across kits
// into a union keyed by manager. Two kits declaring the same manager
// conflict unless the composition lock selects one kit for the target;
// a lock selection naming a kit that does not declare the manager, or a
// lock row for a manager no kit declares, fails closed.
func ComposeDependencyStores(views []ManifestView, target CompositionTarget, lock *CompositionLock) (*DependencyStoreComposition, error) {
	if target.OS == "" {
		return nil, errors.New("kit dependency store compose: target OS is required")
	}
	if lock != nil {
		if err := validateCompositionLock(lock); err != nil {
			return nil, err
		}
	}
	byManager := map[string][]storeClaimant{}
	for _, view := range views {
		if !supportsOS(view.SupportedOS, target.OS) {
			continue
		}
		for i := range view.DependencyStores {
			store := view.DependencyStores[i]
			if err := ValidateDependencyStore(store); err != nil {
				return nil, fmt.Errorf("kit dependency store compose: kit %s: %w", view.ID, err)
			}
			identity := commandIdentityForView(view, "dependency-store")
			byManager[store.Manager] = append(byManager[store.Manager], storeClaimant{
				kitID:      view.ID,
				digestKind: identity.DigestKind,
				digest:     identity.Digest,
				store:      store,
			})
		}
	}
	managers := make([]string, 0, len(byManager))
	for manager := range byManager {
		managers = append(managers, manager)
	}
	sort.Strings(managers)

	plan := &DependencyStoreComposition{Schema: DependencyStoreCompositionSchema, Target: target}
	for _, manager := range managers {
		claimants := byManager[manager]
		selected, err := resolveManagerClaimants(manager, claimants, target, lock)
		if err != nil {
			return nil, err
		}
		plan.Stores = append(plan.Stores, ComposedDependencyStore{
			Manager:    manager,
			KitID:      selected.kitID,
			DigestKind: selected.digestKind,
			Digest:     selected.digest,
			Ecosystem:  selected.store.Ecosystem,
			Lockfiles:  append([]string{}, selected.store.Lockfiles...),
			Inputs:     append([]string{}, selected.store.Inputs...),
			Version:    selected.store.Version,
			Store:      selected.store.Store,
			ImportEnv:  copyStringMap(selected.store.ImportEnv),
			Commands:   ResolveDependencyStoreCommands(selected.store, target.OS),
			Snapshot:   selected.store.Snapshot,
			ProxyEnv:   copyStringMap(selected.store.ProxyEnv),
		})
	}
	if err := validateAppliedDependencyLock(plan.Stores, target, lock); err != nil {
		return nil, err
	}
	digest, err := dependencyStoreDigest(*plan)
	if err != nil {
		return nil, err
	}
	plan.Digest = digest
	return plan, nil
}

// storeClaimant is one kit's validated entry for a manager.
type storeClaimant struct {
	kitID      string
	digestKind string
	digest     string
	store      DependencyStoreView
}

// resolveManagerClaimants selects one claimant: the sole entry, or the
// lock-selected kit when several kits declare the manager.
func resolveManagerClaimants(manager string, claimants []storeClaimant, target CompositionTarget, lock *CompositionLock) (storeClaimant, error) {
	if len(claimants) == 1 {
		if selected, ok := lockedDependencySelection(lock, target, manager); ok && selected != claimants[0].kitID {
			return storeClaimant{}, fmt.Errorf("%w: binding for manager %q selects kit %q which does not declare it",
				ErrCompositionLockInvalid, manager, selected)
		}
		return claimants[0], nil
	}
	if selected, ok := lockedDependencySelection(lock, target, manager); ok {
		for _, claimant := range claimants {
			if claimant.kitID == selected {
				return claimant, nil
			}
		}
		return storeClaimant{}, fmt.Errorf("%w: binding for manager %q selects kit %q which does not declare it",
			ErrCompositionLockInvalid, manager, selected)
	}
	owners := make([]string, 0, len(claimants))
	for _, claimant := range claimants {
		owners = append(owners, claimant.kitID)
	}
	sort.Strings(owners)
	return storeClaimant{}, fmt.Errorf("%w: manager %q declared by [%s]; add an exact operator lock binding for target os=%q workType=%q pathScope=%q",
		ErrDependencyStoreConflict, manager, strings.Join(owners, ", "), target.OS, target.WorkType, target.PathScope)
}

// lockedDependencySelection returns the lock-selected kit for a manager
// under the exact composition target.
func lockedDependencySelection(lock *CompositionLock, target CompositionTarget, manager string) (string, bool) {
	if lock == nil {
		return "", false
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Dependencies {
			if binding.Manager == manager {
				return binding.SelectedKitID, true
			}
		}
	}
	return "", false
}

// validateAppliedDependencyLock rejects lock rows for the composition
// target that selected nothing: a stale or unused manager binding.
func validateAppliedDependencyLock(stores []ComposedDependencyStore, target CompositionTarget, lock *CompositionLock) error {
	if lock == nil {
		return nil
	}
	resolved := make(map[string]string, len(stores))
	for _, store := range stores {
		resolved[store.Manager] = store.KitID
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Dependencies {
			selected, ok := resolved[binding.Manager]
			if !ok || selected != binding.SelectedKitID {
				return fmt.Errorf("%w: stale or unused binding for manager %q",
					ErrCompositionLockInvalid, binding.Manager)
			}
		}
	}
	return nil
}

// SelectDependencyStores selects the applicable composed store per
// ecosystem for a mutable leaf. An entry applies when the leaf holds one
// of its lockfiles. When two entries of one ecosystem match, the leaf's
// own declaration decides where the ecosystem has one; otherwise no entry
// applies and selection reports manager_ambiguous.
func SelectDependencyStores(stores []ComposedDependencyStore, leaf LeafDependencyFacts) ([]ComposedDependencyStore, error) {
	present := make(map[string]struct{}, len(leaf.Files))
	for _, file := range leaf.Files {
		present[file] = struct{}{}
	}
	byEcosystem := map[string][]ComposedDependencyStore{}
	ecosystems := []string{}
	for _, store := range stores {
		matched := false
		for _, lockfile := range store.Lockfiles {
			if _, ok := present[lockfile]; ok {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		group := store.Ecosystem
		if group == "" {
			group = "\x00" + store.Manager
		}
		if _, ok := byEcosystem[group]; !ok {
			ecosystems = append(ecosystems, group)
		}
		byEcosystem[group] = append(byEcosystem[group], store)
	}
	sort.Strings(ecosystems)

	var selected []ComposedDependencyStore
	for _, group := range ecosystems {
		candidates := byEcosystem[group]
		if len(candidates) == 1 {
			selected = append(selected, candidates[0])
			continue
		}
		declared := leafManagerToken(leaf.PackageManager)
		matched := -1
		for i := range candidates {
			if candidates[i].Manager == declared {
				matched = i
				break
			}
		}
		if matched < 0 {
			managers := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				managers = append(managers, candidate.Manager)
			}
			sort.Strings(managers)
			return nil, fmt.Errorf("%w: manager_ambiguous: ecosystem %q matched by [%s] with no leaf declaration selecting one",
				ErrDependencyStoreAmbiguous, candidates[0].Ecosystem, strings.Join(managers, ", "))
		}
		selected = append(selected, candidates[matched])
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Manager < selected[j].Manager })
	return selected, nil
}

// leafManagerToken reduces a leaf declaration such as "pnpm@10.1.0" to
// its manager token "pnpm". Empty input selects nothing.
func leafManagerToken(declaration string) string {
	declaration = strings.TrimSpace(declaration)
	if declaration == "" {
		return ""
	}
	if at := strings.Index(declaration, "@"); at >= 0 {
		return declaration[:at]
	}
	return declaration
}

// dependencyStoreDigest binds the resolved managers, owners and commands.
func dependencyStoreDigest(plan DependencyStoreComposition) (string, error) {
	plan.Digest = ""
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	raw, err = jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
