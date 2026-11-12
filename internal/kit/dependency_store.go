// Package kit — dependency_store.go: kit-declared dependency stores (D1).
//
// A kit declares each package manager it supports as one
// [[provide.dependency_store]] entry, or as several entries with disjoint
// `versions` ranges when one manager changed its store layout between
// majors. This file is the slice-1 engine: validation of a single entry,
// composition of entries across kits (union keyed by manager and version
// range, conflict on overlapping ranges unless the composition lock selects
// a kit), and selection of the applicable entry for a mutable leaf (by
// lockfile presence, then the leaf's own declaration, then the version its
// pin names).
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
	"path/filepath"
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
	// ErrDependencyStoreVersions marks a `versions` value that is not a
	// valid range. It wraps ErrDependencyStoreInvalid.
	ErrDependencyStoreVersions = errors.New("kit dependency store versions invalid")
	// ErrDependencyStoreRangeOverlap marks two entries of one manager in
	// one kit whose `versions` ranges overlap (a missing `versions`
	// overlaps every range). It wraps ErrDependencyStoreInvalid.
	ErrDependencyStoreRangeOverlap = errors.New("kit dependency store version ranges overlap")
	// ErrDependencyStoreToolchainMismatch marks a leaf pinning a manager
	// version that no entry's `versions` range covers. The message carries
	// the closed miss-reason token toolchain_mismatch.
	ErrDependencyStoreToolchainMismatch = errors.New("kit dependency store toolchain mismatch")
	// ErrDependencyStoreVersionRequired marks several entries of one
	// manager (disjoint ranges) with no version to choose between them:
	// the leaf has no pin naming one. It wraps
	// ErrDependencyStoreAmbiguous. The caller reads the version from the
	// manager (the entry's `version` command) and selects with
	// SelectDependencyStoreForVersion.
	ErrDependencyStoreVersionRequired = fmt.Errorf("%w: version required", ErrDependencyStoreAmbiguous)
	// ErrPackagePathInvalid marks a package-relative path that is not
	// contained in the kit's package root, or a composed entry that
	// carries no verified package root.
	ErrPackagePathInvalid = errors.New("kit package path invalid")
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

// DependencyStoreNoCode is the [provide.dependency_store.no_code] block: the
// settings that make every phase run no package code and no
// repository-supplied manager hooks. A declared block with an empty Env is
// valid (a manager whose fetch and install run no package code needs none);
// an entry that declares no block cannot run where no confiner exists.
type DependencyStoreNoCode struct {
	Env map[string]string `json:"env,omitempty"`
}

// DependencyStoreCredentials is the [provide.dependency_store.credentials]
// block. ConfigEnv names the variable that points the manager at the per-run
// credential file, delivered to the fetch phase only.
type DependencyStoreCredentials struct {
	ConfigEnv string `json:"configEnv"`
}

// DependencyStoreView is one [[provide.dependency_store]] entry projected
// into the daemon-independent view type. Commands holds the generic
// command per name; CommandsOverride holds OS specializations that win
// over the generic form for the same store owned by the same kit.
type DependencyStoreView struct {
	Manager   string   `json:"manager"`
	Ecosystem string   `json:"ecosystem,omitempty"`
	Lockfiles []string `json:"lockfiles,omitempty"`
	Inputs    []string `json:"inputs,omitempty"`
	Version   string   `json:"version,omitempty"`
	// Versions is the semver range of manager versions this entry covers.
	// Empty covers every version.
	Versions         string                       `json:"versions,omitempty"`
	Store            DependencyStoreLayout        `json:"store"`
	ImportEnv        map[string]string            `json:"importEnv,omitempty"`
	NoCode           *DependencyStoreNoCode       `json:"noCode,omitempty"`
	Credentials      *DependencyStoreCredentials  `json:"credentials,omitempty"`
	Commands         map[string]string            `json:"commands,omitempty"`
	CommandsOverride map[string]map[string]string `json:"commandsOverride,omitempty"`
	Snapshot         DependencyStoreSnapshot      `json:"snapshot"`
	ProxyEnv         map[string]string            `json:"proxyEnv,omitempty"`
}

// ComposedDependencyStore is one resolved store entry: the winning kit's
// entry for a (manager, version range) with commands specialized for the
// composition target OS. A manager appears once per disjoint range.
type ComposedDependencyStore struct {
	Manager    string `json:"manager"`
	KitID      string `json:"kitId"`
	DigestKind string `json:"digestKind"`
	Digest     string `json:"digest"`
	// PackageRoot is the verified root directory of the owning kit's
	// package, empty for a kit with no verified package (a legacy
	// manifest). A package-owned path in the entry (relocate) resolves only
	// against it, through ResolvePackagePath. It is host-specific, so the
	// composition digest excludes it.
	PackageRoot string `json:"packageRoot,omitempty"`
	// Versions is the entry's range of covered manager versions; empty
	// covers every version.
	Versions    string                      `json:"versions,omitempty"`
	NoCode      *DependencyStoreNoCode      `json:"noCode,omitempty"`
	Credentials *DependencyStoreCredentials `json:"credentials,omitempty"`
	Ecosystem   string                      `json:"ecosystem,omitempty"`
	Lockfiles   []string                    `json:"lockfiles,omitempty"`
	Inputs      []string                    `json:"inputs,omitempty"`
	Version     string                      `json:"version,omitempty"`
	Store       DependencyStoreLayout       `json:"store"`
	ImportEnv   map[string]string           `json:"importEnv,omitempty"`
	Commands    map[string]string           `json:"commands"`
	Snapshot    DependencyStoreSnapshot     `json:"snapshot"`
	ProxyEnv    map[string]string           `json:"proxyEnv,omitempty"`
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
// the enclosing lock target, manager resolves to the named kit's entry at
// the pinned digest. The digest pin matches command bindings: the lock
// reader refuses a binding whose kit digest moved.
type LockedDependencyBinding struct {
	Manager string `json:"manager"`
	// SelectedKitID names the kit whose entry resolves the manager.
	SelectedKitID string `json:"selectedKitId"`
	// SelectedDigestKind and SelectedDigest pin the kit digest the
	// operator approved, matching command bindings. The lock reader
	// refuses a binding whose kit digest moved.
	SelectedDigestKind string `json:"selectedDigestKind"`
	SelectedDigest     string `json:"selectedDigest"`
}

// LeafDependencyFacts are the mutable-leaf facts selection needs: which
// repo-relative paths the leaf holds and the leaf's own manager
// declaration (Node packageManager, "" when the ecosystem has none).
type LeafDependencyFacts struct {
	Files          []string
	PackageManager string
}

// ValidateDependencyStore validates one entry. manager, lockfiles, fetch
// and install are required, and manager is a path-safe identifier;
// integrity is on-use, on-fetch or none with none forcing session-only;
// store paths are literal, canonical, relative and contained, and never
// the store root; lockfiles, inputs and snapshot.installed cannot leave
// the leaf; env names are POSIX names; secrets and never_share may not
// overlap content; relocate is a typed package path; override commands
// must specialize a same-entry base command on a known OS.
func ValidateDependencyStore(store DependencyStoreView) error {
	if store.Manager == "" {
		return fmt.Errorf("%w: manager is required", ErrDependencyStoreInvalid)
	}
	if !validManagerID(store.Manager) {
		// The manager names a directory in the keeper's storage layout
		// (stores/<scope>/<manager>/...), so it must be one safe segment.
		return fmt.Errorf("%w: manager %q is not an identifier", ErrDependencyStoreInvalid, store.Manager)
	}
	if len(store.Lockfiles) == 0 {
		return fmt.Errorf("%w: %s lockfiles are required", ErrDependencyStoreInvalid, store.Manager)
	}
	for _, leafField := range []struct {
		name     string
		patterns []string
	}{
		{"lockfile", store.Lockfiles},
		{"input", store.Inputs},
		{"snapshot.installed", store.Snapshot.Installed},
	} {
		for _, p := range leafField.patterns {
			if err := checkLeafPattern(store.Manager, leafField.name, p); err != nil {
				return err
			}
		}
	}
	for _, name := range store.Store.Env {
		if !validEnvName(name) {
			return fmt.Errorf("%w: %s store.env %q is not an environment variable name", ErrDependencyStoreInvalid, store.Manager, name)
		}
	}
	for _, env := range []struct {
		field string
		vars  map[string]string
	}{
		{"import.env", store.ImportEnv},
		{"proxy.env", store.ProxyEnv},
	} {
		for name := range env.vars {
			if !validEnvName(name) {
				return fmt.Errorf("%w: %s %s key %q is not an environment variable name", ErrDependencyStoreInvalid, store.Manager, env.field, name)
			}
		}
	}
	for osKey := range store.Store.Default {
		if _, ok := dependencyStoreOSKeys[osKey]; !ok {
			return fmt.Errorf("%w: %s store.default names unknown OS %q", ErrDependencyStoreInvalid, store.Manager, osKey)
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
	// relocate is package-owned by field (D1), never inline shell text: it
	// must be a typed package path the signed inventory can pin
	// (ADR-2026-07-10 reference closure), so arguments, absolute paths and
	// escapes are rejected rather than silently left uninventoried.
	if relocate, ok := store.Commands[DependencyStoreCommandRelocate]; ok {
		if err := checkPackagePath(store.Manager, "", relocate); err != nil {
			return err
		}
	}
	for osKey, overlay := range store.CommandsOverride {
		if relocate, ok := overlay[DependencyStoreCommandRelocate]; ok {
			if err := checkPackagePath(store.Manager, osKey, relocate); err != nil {
				return err
			}
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
	if _, err := ParseVersionRange(store.Versions); err != nil {
		return fmt.Errorf("%w: %w: %s versions: %w", ErrDependencyStoreInvalid, ErrDependencyStoreVersions, store.Manager, err)
	}
	return validateNoCodeAndCredentials(store)
}

// forbiddenProcessEnv reports a variable the preparer never lets a manifest
// set: dynamic loader variables and node's option channel load code the
// manager never asked for.
func forbiddenProcessEnv(name string) bool {
	return strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "LD_") || name == "NODE_OPTIONS"
}

// validateNoCodeAndCredentials checks the no_code and credentials blocks: env
// names are POSIX names, no loader or option-channel variable, and the
// credential variable collides with no other variable the entry sets.
func validateNoCodeAndCredentials(store DependencyStoreView) error {
	if store.NoCode != nil {
		for name, value := range store.NoCode.Env {
			if !validEnvName(name) {
				return fmt.Errorf("%w: %s no_code.env key %q is not an environment variable name", ErrDependencyStoreInvalid, store.Manager, name)
			}
			if forbiddenProcessEnv(name) {
				return fmt.Errorf("%w: %s no_code.env may not set %q", ErrDependencyStoreInvalid, store.Manager, name)
			}
			if strings.ContainsRune(value, 0) {
				return fmt.Errorf("%w: %s no_code.env %q has a NUL in its value", ErrDependencyStoreInvalid, store.Manager, name)
			}
		}
	}
	if store.Credentials == nil {
		return nil
	}
	name := store.Credentials.ConfigEnv
	if name == "" {
		return fmt.Errorf("%w: %s credentials.config_env is required", ErrDependencyStoreInvalid, store.Manager)
	}
	if !validEnvName(name) {
		return fmt.Errorf("%w: %s credentials.config_env %q is not an environment variable name", ErrDependencyStoreInvalid, store.Manager, name)
	}
	if forbiddenProcessEnv(name) {
		return fmt.Errorf("%w: %s credentials.config_env may not be %q", ErrDependencyStoreInvalid, store.Manager, name)
	}
	taken := map[string]string{}
	for _, v := range store.Store.Env {
		taken[v] = "store.env"
	}
	for v := range store.ImportEnv {
		taken[v] = "import.env"
	}
	for v := range store.ProxyEnv {
		taken[v] = "proxy.env"
	}
	if store.NoCode != nil {
		for v := range store.NoCode.Env {
			taken[v] = "no_code.env"
		}
	}
	if where, clash := taken[name]; clash {
		return fmt.Errorf("%w: %s credentials.config_env %q is also set by %s", ErrDependencyStoreInvalid, store.Manager, name, where)
	}
	return nil
}

// ValidateDependencyStores validates each entry of one kit. A kit may declare
// one manager more than once only with non-overlapping `versions` ranges; an
// entry with no `versions` covers every version, so it overlaps every other
// entry of its manager. Cross-kit duplicates are not a validation error;
// they are composition conflicts resolved by the lock.
func ValidateDependencyStores(stores []DependencyStoreView) error {
	byManager := make(map[string][]int, len(stores))
	for i := range stores {
		if err := ValidateDependencyStore(stores[i]); err != nil {
			return err
		}
		byManager[stores[i].Manager] = append(byManager[stores[i].Manager], i)
	}
	for i := range stores {
		for _, j := range byManager[stores[i].Manager] {
			if j <= i {
				continue
			}
			a, _ := ParseVersionRange(stores[i].Versions) // validated above
			b, _ := ParseVersionRange(stores[j].Versions)
			if a.Overlaps(b) {
				return fmt.Errorf("%w: %w: manager %q declares overlapping versions %q and %q in one kit",
					ErrDependencyStoreInvalid, ErrDependencyStoreRangeOverlap, stores[i].Manager, a.String(), b.String())
			}
		}
	}
	return nil
}

// checkStorePath requires a relative, contained, forward-slash, canonical
// store path naming a proper subtree. Store paths are literal: a spelling
// that cleans to the store root ("./", "a/..") or a glob would make
// content cover every secrets and never_share path while the overlap check
// compares strings that never meet.
func checkStorePath(manager, field, p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("%w: %s has an empty %s path", ErrDependencyStoreInvalid, manager, field)
	}
	if path.IsAbs(p) || strings.Contains(p, "\\") {
		return fmt.Errorf("%w: %s %s path %q is not relative forward-slash form",
			ErrDependencyStoreInvalid, manager, field, p)
	}
	if strings.ContainsAny(p, "*?[\x00") {
		return fmt.Errorf("%w: %s %s path %q is not a literal path", ErrDependencyStoreInvalid, manager, field, p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: %s %s path %q escapes the store", ErrDependencyStoreInvalid, manager, field, p)
	}
	if clean != p {
		return fmt.Errorf("%w: %s %s path %q is not in canonical form", ErrDependencyStoreInvalid, manager, field, p)
	}
	return nil
}

// checkLeafPattern requires a leaf-relative path or glob (lockfiles,
// inputs, snapshot.installed) that cannot leave the leaf: relative,
// forward-slash, canonical, and free of "." and ".." segments. These
// patterns later select files the filler copies out of the leaf and paths
// retention deletes from it.
func checkLeafPattern(manager, field, p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("%w: %s has an empty %s entry", ErrDependencyStoreInvalid, manager, field)
	}
	if path.IsAbs(p) || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return fmt.Errorf("%w: %s %s %q is not a canonical relative forward-slash path",
			ErrDependencyStoreInvalid, manager, field, p)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return fmt.Errorf("%w: %s %s %q escapes the leaf", ErrDependencyStoreInvalid, manager, field, p)
		}
	}
	return nil
}

// checkPackagePath requires a package-owned command (relocate) to be a
// typed package path: one canonical, contained, relative path with no
// arguments, variables, flags or URLs.
func checkPackagePath(manager, osKey, p string) error {
	where := "relocate"
	if osKey != "" {
		where = "relocate for OS " + osKey
	}
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p ||
		strings.ContainsAny(p, " \t\r\n\\\x00$") || strings.HasPrefix(p, "-") || strings.Contains(p, "://") {
		return fmt.Errorf("%w: %s %s %q must be a package path with no arguments",
			ErrDependencyStoreInvalid, manager, where, p)
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%w: %s %s %q escapes the package", ErrDependencyStoreInvalid, manager, where, p)
	}
	return nil
}

// validManagerID accepts one path-safe identifier segment.
func validManagerID(manager string) bool {
	if len(manager) > 64 {
		return false
	}
	for i := 0; i < len(manager); i++ {
		c := manager[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 && !alnum {
			return false
		}
		if !alnum && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return manager != ""
}

// validEnvName accepts a POSIX environment variable name.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
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
// into a union keyed by (manager, version range). One kit may declare a
// manager several times with disjoint ranges. Two kits declaring the same
// manager conflict when any of their ranges overlap, unless the composition
// lock selects one kit for the target, in which case only that kit's
// entries of the manager remain. A lock selection naming a kit that does
// not declare the manager, or a lock row for a manager no kit declares,
// fails closed.
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
		// Per-entry rules plus the same-kit range rule: a view need not
		// have come through the manifest parser.
		if err := ValidateDependencyStores(view.DependencyStores); err != nil {
			return nil, fmt.Errorf("kit dependency store compose: kit %s: %w", view.ID, err)
		}
		for i := range view.DependencyStores {
			store := view.DependencyStores[i]
			identity := commandIdentityForView(view, "dependency-store")
			byManager[store.Manager] = append(byManager[store.Manager], storeClaimant{
				kitID:       view.ID,
				digestKind:  identity.DigestKind,
				digest:      identity.Digest,
				packageRoot: view.PackageRoot,
				store:       store,
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
		selected, err := resolveManagerClaimants(manager, byManager[manager], target, lock)
		if err != nil {
			return nil, err
		}
		for _, claimant := range selected {
			plan.Stores = append(plan.Stores, composeClaimant(manager, claimant, target))
		}
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

func composeClaimant(manager string, selected storeClaimant, target CompositionTarget) ComposedDependencyStore {
	return ComposedDependencyStore{
		Manager:     manager,
		KitID:       selected.kitID,
		DigestKind:  selected.digestKind,
		Digest:      selected.digest,
		PackageRoot: selected.packageRoot,
		Ecosystem:   selected.store.Ecosystem,
		Lockfiles:   append([]string{}, selected.store.Lockfiles...),
		Inputs:      append([]string{}, selected.store.Inputs...),
		Version:     selected.store.Version,
		Versions:    strings.TrimSpace(selected.store.Versions),
		Store:       selected.store.Store,
		ImportEnv:   copyStringMap(selected.store.ImportEnv),
		NoCode:      copyNoCode(selected.store.NoCode),
		Credentials: copyCredentials(selected.store.Credentials),
		Commands:    ResolveDependencyStoreCommands(selected.store, target.OS),
		Snapshot:    selected.store.Snapshot,
		ProxyEnv:    copyStringMap(selected.store.ProxyEnv),
	}
}

func copyNoCode(in *DependencyStoreNoCode) *DependencyStoreNoCode {
	if in == nil {
		return nil
	}
	return &DependencyStoreNoCode{Env: copyStringMap(in.Env)}
}

func copyCredentials(in *DependencyStoreCredentials) *DependencyStoreCredentials {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// storeClaimant is one kit's validated entry for a manager.
type storeClaimant struct {
	kitID       string
	digestKind  string
	digest      string
	packageRoot string
	store       DependencyStoreView
}

// resolveManagerClaimants returns the claimants that compose for a manager.
// The lock-selected kit's entries win outright when a binding exists. The
// lock selection pins the kit digest, matching command bindings: a binding
// whose kit digest moved is refused, even when the kit still declares the
// manager. With no binding, claimants from one kit all compose (validation
// already made its ranges disjoint), and claimants from different kits
// compose only when every cross-kit pair of ranges is disjoint.
func resolveManagerClaimants(manager string, claimants []storeClaimant, target CompositionTarget, lock *CompositionLock) ([]storeClaimant, error) {
	if locked, ok := lockedDependencySelection(lock, target, manager); ok {
		var kept []storeClaimant
		for _, claimant := range claimants {
			if claimant.kitID != locked.SelectedKitID {
				continue
			}
			if claimant.digestKind != locked.SelectedDigestKind || claimant.digest != locked.SelectedDigest {
				return nil, fmt.Errorf("%w: binding for manager %q pins kit %q at digest %s:%s which no longer matches",
					ErrCompositionLockInvalid, manager, locked.SelectedKitID, locked.SelectedDigestKind, locked.SelectedDigest)
			}
			kept = append(kept, claimant)
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("%w: binding for manager %q selects kit %q which does not declare it",
				ErrCompositionLockInvalid, manager, locked.SelectedKitID)
		}
		return kept, nil
	}
	conflicting := map[string]struct{}{}
	for i := range claimants {
		for j := i + 1; j < len(claimants); j++ {
			if claimants[i].kitID == claimants[j].kitID {
				continue
			}
			a, _ := ParseVersionRange(claimants[i].store.Versions) // validated above
			b, _ := ParseVersionRange(claimants[j].store.Versions)
			if a.Overlaps(b) {
				conflicting[claimants[i].kitID] = struct{}{}
				conflicting[claimants[j].kitID] = struct{}{}
			}
		}
	}
	if len(conflicting) > 0 {
		owners := make([]string, 0, len(conflicting))
		for owner := range conflicting {
			owners = append(owners, owner)
		}
		sort.Strings(owners)
		return nil, fmt.Errorf("%w: manager %q declared by [%s] with overlapping version ranges; add an exact operator lock binding for target os=%q workType=%q pathScope=%q",
			ErrDependencyStoreConflict, manager, strings.Join(owners, ", "), target.OS, target.WorkType, target.PathScope)
	}
	sort.SliceStable(claimants, func(i, j int) bool {
		if claimants[i].store.Versions != claimants[j].store.Versions {
			return claimants[i].store.Versions < claimants[j].store.Versions
		}
		return claimants[i].kitID < claimants[j].kitID
	})
	return claimants, nil
}

// lockedDependencySelection returns the lock-selected kit pin for a
// manager under the exact composition target.
func lockedDependencySelection(lock *CompositionLock, target CompositionTarget, manager string) (LockedDependencyBinding, bool) {
	if lock == nil {
		return LockedDependencyBinding{}, false
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Dependencies {
			if binding.Manager == manager {
				return binding, true
			}
		}
	}
	return LockedDependencyBinding{}, false
}

// validateAppliedDependencyLock rejects lock rows for the composition
// target that selected nothing: a stale or unused manager binding, or a
// binding whose pinned kit digest moved.
func validateAppliedDependencyLock(stores []ComposedDependencyStore, target CompositionTarget, lock *CompositionLock) error {
	if lock == nil {
		return nil
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Dependencies {
			found := false
			for _, store := range stores {
				if store.Manager == binding.Manager && store.KitID == binding.SelectedKitID &&
					store.DigestKind == binding.SelectedDigestKind && store.Digest == binding.SelectedDigest {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: stale or unused binding for manager %q",
					ErrCompositionLockInvalid, binding.Manager)
			}
		}
	}
	return nil
}

// SelectDependencyStores selects the applicable composed store per
// ecosystem for a mutable leaf. An entry applies when the leaf holds one
// of its lockfiles. When entries of two managers in one ecosystem match, the
// leaf's own declaration decides where the ecosystem has one; otherwise no
// entry applies and selection reports manager_ambiguous.
//
// When the chosen manager has several entries (disjoint `versions` ranges),
// the version in the leaf's declaration (Node packageManager, for example
// "pnpm@9.15.9") selects the entry whose range covers it, or the result is
// toolchain_mismatch, whether the manager has one entry or several. With a
// single entry and no usable pin, that entry is selected (the caller checks
// the version the manager reports). With no usable pin and several entries,
// the result is ErrDependencyStoreVersionRequired, and the caller selects
// with SelectDependencyStoreForVersion once the manager reports its version.
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

	token, pinned := leafManagerToken(leaf.PackageManager), leafManagerVersion(leaf.PackageManager)
	var selected []ComposedDependencyStore
	for _, group := range ecosystems {
		candidates := byEcosystem[group]
		managers := map[string][]ComposedDependencyStore{}
		names := []string{}
		for _, candidate := range candidates {
			if _, ok := managers[candidate.Manager]; !ok {
				names = append(names, candidate.Manager)
			}
			managers[candidate.Manager] = append(managers[candidate.Manager], candidate)
		}
		sort.Strings(names)
		manager := names[0]
		if len(names) > 1 {
			if _, ok := managers[token]; !ok {
				return nil, fmt.Errorf("%w: manager_ambiguous: ecosystem %q matched by [%s] with no leaf declaration selecting one",
					ErrDependencyStoreAmbiguous, candidates[0].Ecosystem, strings.Join(names, ", "))
			}
			manager = token
		}
		entries := managers[manager]
		// The pin's version describes the pinned manager only.
		version := pinned
		if token != manager {
			version = ""
		}
		if version == "" && len(entries) > 1 {
			return nil, fmt.Errorf("%w: manager %q has %d entries with disjoint versions and the leaf pins no version",
				ErrDependencyStoreVersionRequired, manager, len(entries))
		}
		if version == "" {
			selected = append(selected, entries[0])
			continue
		}
		entry, err := SelectDependencyStoreForVersion(entries, manager, version)
		if err != nil {
			return nil, err
		}
		selected = append(selected, entry)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Manager < selected[j].Manager })
	return selected, nil
}

// SelectDependencyStoreForVersion returns the entry of manager whose
// `versions` range covers version (for example "9.15.9"). An entry with no
// `versions` covers every version. When no entry covers it the error wraps
// ErrDependencyStoreToolchainMismatch. Ranges are disjoint after composition
// and validation, so at most one entry covers a version, apart from an
// unranged entry the lock selected beside ranged ones, which loses to the
// ranged entry covering the version.
func SelectDependencyStoreForVersion(stores []ComposedDependencyStore, manager, version string) (ComposedDependencyStore, error) {
	var unranged *ComposedDependencyStore
	for i := range stores {
		if stores[i].Manager != manager {
			continue
		}
		rng, err := ParseVersionRange(stores[i].Versions)
		if err != nil {
			return ComposedDependencyStore{}, fmt.Errorf("%w: %s versions: %w", ErrDependencyStoreInvalid, manager, err)
		}
		if rng.Unbounded() {
			unranged = &stores[i]
			continue
		}
		covered, err := rng.Covers(version)
		if err != nil {
			return ComposedDependencyStore{}, fmt.Errorf("%w: toolchain_mismatch: %s version %q: %w",
				ErrDependencyStoreToolchainMismatch, manager, version, err)
		}
		if covered {
			return stores[i], nil
		}
	}
	if unranged != nil {
		if _, err := parseManagerVersion(version); err != nil {
			return ComposedDependencyStore{}, fmt.Errorf("%w: toolchain_mismatch: %s version %q: %w",
				ErrDependencyStoreToolchainMismatch, manager, version, err)
		}
		return *unranged, nil
	}
	return ComposedDependencyStore{}, fmt.Errorf("%w: toolchain_mismatch: no %s entry covers version %q",
		ErrDependencyStoreToolchainMismatch, manager, version)
}

// ResolvePackagePath resolves a package-relative path (relocate) against the
// entry's verified package root and returns the absolute path. It refuses an
// entry with no package root, and a path that is not one canonical relative
// path contained in the root. The check is lexical; the caller opens the
// result under the package's own verified inventory.
func (c ComposedDependencyStore) ResolvePackagePath(rel string) (string, error) {
	if c.PackageRoot == "" {
		return "", fmt.Errorf("%w: %s entry from kit %s has no verified package root", ErrPackagePathInvalid, c.Manager, c.KitID)
	}
	if err := checkPackagePath(c.Manager, "", rel); err != nil {
		return "", fmt.Errorf("%w: %w", ErrPackagePathInvalid, err)
	}
	return filepath.Join(c.PackageRoot, filepath.FromSlash(rel)), nil
}

// ResolveRelocate returns the absolute path of the entry's relocate command
// for the composed target (already OS-specialized in Commands), resolved
// against the package root. ok is false when the entry declares none.
func (c ComposedDependencyStore) ResolveRelocate() (resolved string, ok bool, err error) {
	rel, ok := c.Commands[DependencyStoreCommandRelocate]
	if !ok || rel == "" {
		return "", false, nil
	}
	resolved, err = c.ResolvePackagePath(rel)
	return resolved, true, err
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

// leafManagerVersion returns the version part of a leaf declaration such as
// "pnpm@9.15.9+sha512.abc", without the hash, or "" when there is none or it
// is not major.minor.patch (a dist-tag such as "latest" names no version).
func leafManagerVersion(declaration string) string {
	declaration = strings.TrimSpace(declaration)
	at := strings.Index(declaration, "@")
	if at < 0 {
		return ""
	}
	version := declaration[at+1:]
	if plus := strings.Index(version, "+"); plus >= 0 {
		version = version[:plus]
	}
	if _, err := parseManagerVersion(version); err != nil {
		return ""
	}
	return version
}

// dependencyStoreDigest binds the resolved managers, owners and commands. The
// package root is a host path, so it never enters the digest.
func dependencyStoreDigest(plan DependencyStoreComposition) (string, error) {
	plan.Digest = ""
	plan.Stores = append([]ComposedDependencyStore(nil), plan.Stores...)
	for i := range plan.Stores {
		plan.Stores[i].PackageRoot = ""
	}
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
