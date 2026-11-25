package depprep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/internal/semverrange"
)

// EntrySource supplies the composed kit dependency-store entries for a leaf
// (ADR D7.1). A composing binary's kit registry implements it: for a leaf it
// composes the entries of every enabled kit whose lockfiles the leaf holds,
// whether or not the kit's detect rules matched, and fails on an invalid kit
// or a composition conflict the lock does not resolve.
//
// Prepare itself re-checks lockfile eligibility and then selects one entry
// per ecosystem (the leaf's packageManager decides between managers) and,
// within a manager, the entry whose versions range covers the effective
// version. A source may return eligible candidates or an already-selected
// set; both select the same entries.
type EntrySource interface {
	Entries(ctx context.Context, leaf Leaf) ([]Entry, error)
}

// Leaf identifies the leaf an entry source composes for.
type Leaf struct {
	// Root is the absolute path of the mutable leaf.
	Root string
	// OS is the kit platform key of the host: "macos", "linux" or "windows".
	OS string
}

// Entry is one composed [[provide.dependency_store]] entry, in this package's
// own types so a caller outside the module can build one.
type Entry struct {
	Manager   string
	Ecosystem string
	// Versions is a semver range over the effective manager version. Empty
	// covers every version.
	Versions string
	// KitID, KitDigestKind and KitDigest identify the owning kit.
	KitID         string
	KitDigestKind string
	KitDigest     string
	// PackageRoot is the owning kit's verified package root. A package path
	// in the entry (Commands.Relocate) resolves only against it.
	PackageRoot string
	// Lockfiles are leaf-relative paths whose presence selects the entry.
	Lockfiles []string
	// Inputs are further committed files that change what an install
	// produces.
	Inputs []string
	// VersionCommand prints the manager version.
	VersionCommand string
	Commands       Commands
	Store          StoreLayout
	// ImportEnv makes the manager import by clone or copy, never by hard
	// link.
	ImportEnv map[string]string
	// NoCode is the [no_code] block. Nil: the entry declares none, and it
	// cannot be used without a Confiner. A non-nil block with an empty Env is
	// a declaration that the manager runs no package code by default.
	NoCode *NoCode
	// Credentials is the [credentials] block. Nil: the entry takes no
	// registry credentials.
	Credentials *CredentialsSpec
	// Installed are the leaf paths an install creates (snapshot.installed).
	Installed []string
	// RunsPackageCode reports whether the manager's install runs lifecycle
	// scripts or builds by default (snapshot.runs_package_code).
	RunsPackageCode bool
}

// Commands are an entry's commands, already specialized for the host OS.
// Each is a command line of plain words: an optional run of NAME=value
// assignments, then the program and its arguments. Shell syntax is refused.
type Commands struct {
	Fetch     string
	Install   string
	Online    string
	Reconcile string
	Verify    string
	Relocate  string
}

// StoreLayout is the [store] block.
type StoreLayout struct {
	// Env names the variables bound to the store exposure. Without a keeper
	// each is bound to an empty per-run directory under Scratch.
	Env         []string
	Sharing     string
	Integrity   string
	Content     []string
	Bookkeeping []string
	Secrets     []string
	NeverShare  []string
	RecordsPath bool
}

// NoCode is the [no_code] block: settings that make every phase run no
// package code and no repository-supplied manager hooks.
type NoCode struct {
	Env map[string]string
}

// CredentialsSpec is the [credentials] block.
type CredentialsSpec struct {
	// ConfigEnv names the variable that points the manager at the per-run
	// credential file.
	ConfigEnv string
}

// StaticEntries is an EntrySource that returns fixed entries, for tests and
// for embedders that compose entries themselves.
type StaticEntries []Entry

// Entries returns a copy of the fixed entries.
func (s StaticEntries) Entries(context.Context, Leaf) ([]Entry, error) {
	return append([]Entry(nil), s...), nil
}

// reservedEnv are names Prepare constructs itself. No entry setting and no
// command assignment may replace them.
var reservedEnv = map[string]struct{}{
	"PATH": {}, "HOME": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {}, "LANG": {}, "CI": {},
	"XDG_CONFIG_HOME": {}, "XDG_CACHE_HOME": {}, "XDG_DATA_HOME": {}, "XDG_STATE_HOME": {},
	"XDG_RUNTIME_DIR": {}, "NODE_OPTIONS": {},
}

// forbiddenEnv reports a name no entry may set: one Prepare constructs, a
// loader variable, or a proxy variable (only the caller supplies those).
func forbiddenEnv(name string) bool {
	if _, ok := reservedEnv[name]; ok {
		return true
	}
	if strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "XDG_") {
		return true
	}
	_, proxy := proxyEnvNames[strings.ToUpper(name)]
	return proxy
}

// validateEntry checks the fields this build uses. The entry source has
// validated the manifest; this guards the boundary a library caller crosses.
func validateEntry(e Entry) error {
	if e.Manager == "" {
		return errors.New("entry has no manager")
	}
	if len(e.Lockfiles) == 0 {
		return fmt.Errorf("%s entry has no lockfiles", e.Manager)
	}
	for _, p := range append(append(append([]string{}, e.Lockfiles...), e.Inputs...), e.Installed...) {
		if !leafRelative(p) {
			return fmt.Errorf("%s entry path %q is not a canonical leaf-relative path", e.Manager, p)
		}
	}
	if e.Versions != "" {
		if _, err := semverrange.ParseRange(e.Versions); err != nil {
			return fmt.Errorf("%s entry versions: %w", e.Manager, err)
		}
	}
	for _, c := range []struct{ name, line string }{
		{kit.DependencyStoreCommandFetch, e.Commands.Fetch},
		{kit.DependencyStoreCommandInstall, e.Commands.Install},
	} {
		if strings.TrimSpace(c.line) == "" {
			return fmt.Errorf("%s entry has no %s command", e.Manager, c.name)
		}
		if _, err := parseCommand(c.line); err != nil {
			return fmt.Errorf("%s %s command: %w", e.Manager, c.name, err)
		}
	}
	if e.VersionCommand != "" {
		if _, err := parseCommand(e.VersionCommand); err != nil {
			return fmt.Errorf("%s version command: %w", e.Manager, err)
		}
	}
	for _, name := range e.Store.Env {
		if !validEnvName(name) || forbiddenEnv(name) {
			return fmt.Errorf("%s store.env %q may not be set by an entry", e.Manager, name)
		}
	}
	envs := []map[string]string{e.ImportEnv}
	if e.NoCode != nil {
		envs = append(envs, e.NoCode.Env)
	}
	for _, env := range envs {
		for name, value := range env {
			if !validEnvName(name) || forbiddenEnv(name) || strings.ContainsAny(value, "\x00\n") {
				return fmt.Errorf("%s entry setting %q may not be set by an entry", e.Manager, name)
			}
		}
	}
	if e.Credentials != nil && (!validEnvName(e.Credentials.ConfigEnv) || forbiddenEnv(e.Credentials.ConfigEnv)) {
		return fmt.Errorf("%s credentials.config_env is not a usable variable name", e.Manager)
	}
	return nil
}

func leafRelative(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

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

// leafHolds reports whether the leaf holds one of the patterns: a literal
// path that exists, or a single-level glob that matches. "**" patterns are
// not lockfile shapes and never match here.
func leafHolds(root string, patterns []string) []string {
	var held []string
	for _, p := range patterns {
		if strings.Contains(p, "**") {
			continue
		}
		if strings.ContainsAny(p, "*?[") {
			matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				continue
			}
			for _, m := range matches {
				if rel, err := filepath.Rel(root, m); err == nil {
					held = append(held, filepath.ToSlash(rel))
				}
			}
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			held = append(held, p)
		}
	}
	sort.Strings(held)
	return held
}

// leafPin is the leaf's own manager declaration (Node's packageManager).
type leafPin struct {
	manager  string
	version  string
	verified bool
	raw      string
}

const maxPackageJSONBytes = 1 << 20

// readLeafPin reads packageManager from the leaf's package.json. A missing
// or unreadable file declares nothing; a malformed declaration declares
// nothing too, and the manager's own version decides.
func readLeafPin(root string) leafPin {
	f, err := os.Open(filepath.Join(root, "package.json")) //nolint:gosec // fixed name inside the caller's leaf
	if err != nil {
		return leafPin{}
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxPackageJSONBytes))
	if err != nil {
		return leafPin{}
	}
	var doc struct {
		PackageManager string `json:"packageManager"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return leafPin{}
	}
	decl := strings.TrimSpace(doc.PackageManager)
	if decl == "" {
		return leafPin{}
	}
	at := strings.LastIndex(decl, "@")
	if at <= 0 {
		return leafPin{manager: decl, raw: decl}
	}
	pin := leafPin{manager: decl[:at], raw: decl}
	version := decl[at+1:]
	if plus := strings.IndexByte(version, '+'); plus >= 0 {
		pin.verified = plus < len(version)-1
		version = version[:plus]
	}
	if _, err := semverrange.ParseVersion(version); err == nil {
		pin.version = version
	}
	return pin
}

// lockfileDigest hashes the entry's present lockfiles, by path.
func lockfileDigest(root string, held []string) string {
	h := sha256.New()
	for _, p := range held {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
		if raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err == nil { //nolint:gosec // leaf-relative, validated
			sum := sha256.Sum256(raw)
			_, _ = h.Write(sum[:])
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// managerGroup is one ecosystem's chosen manager and its candidate entries
// (several only when they declare disjoint versions ranges).
type managerGroup struct {
	manager string
	entries []Entry
}

// groupEligible keeps the entries whose lockfiles the leaf holds and picks
// one manager per ecosystem. Two managers of one ecosystem are decided by
// the leaf's declaration, else manager_ambiguous.
func groupEligible(root string, entries []Entry, pin leafPin) ([]managerGroup, *Error) {
	byEcosystem := map[string]map[string][]Entry{}
	for _, e := range entries {
		if len(leafHolds(root, e.Lockfiles)) == 0 {
			continue
		}
		group := e.Ecosystem
		if group == "" {
			group = "\x00" + e.Manager
		}
		if byEcosystem[group] == nil {
			byEcosystem[group] = map[string][]Entry{}
		}
		byEcosystem[group][e.Manager] = append(byEcosystem[group][e.Manager], e)
	}
	ecosystems := make([]string, 0, len(byEcosystem))
	for g := range byEcosystem {
		ecosystems = append(ecosystems, g)
	}
	sort.Strings(ecosystems)
	var groups []managerGroup
	for _, g := range ecosystems {
		managers := byEcosystem[g]
		if len(managers) == 1 {
			for m, es := range managers {
				groups = append(groups, managerGroup{manager: m, entries: es})
			}
			continue
		}
		if es, ok := managers[pin.manager]; ok && pin.manager != "" {
			groups = append(groups, managerGroup{manager: pin.manager, entries: es})
			continue
		}
		names := make([]string, 0, len(managers))
		for m := range managers {
			names = append(names, m)
		}
		sort.Strings(names)
		return nil, &Error{
			Code:       CodeManagerAmbiguous,
			MissReason: MissManagerAmbiguous,
			Detail:     fmt.Sprintf("ecosystem matched by [%s] with no leaf declaration selecting one", strings.Join(names, ", ")),
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].manager < groups[j].manager })
	return groups, nil
}

// needsVersionToSelect reports whether a group's entry can only be chosen
// by the effective manager version.
func (g managerGroup) needsVersionToSelect() bool {
	return len(g.entries) > 1 || g.entries[0].Versions != ""
}

// selectByVersion picks the entry whose range covers v. A group whose
// entries overlap is an entry source defect.
func (g managerGroup) selectByVersion(v string) (Entry, *Error) {
	parsed, err := semverrange.ParseVersion(v)
	if err != nil {
		return Entry{}, &Error{
			Code: CodeToolchainUnavailable, Manager: g.manager, Step: stepVersion,
			Detail: "the manager reported no semantic version",
		}
	}
	var covering []Entry
	for _, e := range g.entries {
		if e.Versions == "" {
			covering = append(covering, e)
			continue
		}
		r, err := semverrange.ParseRange(e.Versions)
		if err == nil && r.Contains(parsed) {
			covering = append(covering, e)
		}
	}
	switch len(covering) {
	case 1:
		return covering[0], nil
	case 0:
		return Entry{}, &Error{
			Code: CodeToolchainMismatch, Manager: g.manager, MissReason: MissToolchainMismatch,
			Detail: fmt.Sprintf("no entry's versions range covers %s", parsed),
		}
	default:
		return Entry{}, &Error{
			Code: CodeEntriesUnavailable, Manager: g.manager,
			Detail: fmt.Sprintf("%d entries' versions ranges cover %s", len(covering), parsed),
		}
	}
}
