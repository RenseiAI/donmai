package confinement

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// guards are host paths no writable root may cover.
type guards struct {
	home       string
	stateHome  string
	profileDir string
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are the bindings the confinement owns; a cache may not rebind
// them.
var reservedEnv = map[string]bool{"TMPDIR": true, "TMP": true, "TEMP": true}

// resolveSpec validates spec and returns it in canonical spelling. Every
// refusal is writable_set_unrepresentable: the set cannot be confined as
// declared, and it is never widened or narrowed to make it fit.
func resolveSpec(spec Spec, g guards, canonical func(string) (string, error)) (*Resolved, error) {
	if strings.TrimSpace(spec.SessionID) == "" || strings.TrimSpace(spec.HarnessID) == "" {
		return nil, refuse(ReasonWritableSetUnrepresentable, "session id and harness id are required")
	}
	if spec.SessionTmp == "" {
		return nil, refuse(ReasonWritableSetUnrepresentable, "a per-session temporary directory is required")
	}

	root, err := resolveDir(spec.WorkareaRoot, "workarea root", canonical, false)
	if err != nil {
		return nil, err
	}

	// Pass 1: the writable roots, so pass 2 can tell whether a symbolic link
	// on any declared path sits inside the writable set, where the harness
	// could have planted it.
	type declared struct {
		path  string
		class WritableClass
	}
	var writableDecl []declared
	for _, leaf := range spec.MutableLeaves {
		writableDecl = append(writableDecl, declared{leaf, ClassMutableLeaf})
	}
	for _, dir := range spec.HarnessState {
		writableDecl = append(writableDecl, declared{dir, ClassHarnessState})
	}
	writableDecl = append(writableDecl, declared{spec.SessionTmp, ClassSessionTmp})
	seenEnv := map[string]bool{}
	for _, cache := range spec.Caches {
		if !envNamePattern.MatchString(cache.Env) || reservedEnv[cache.Env] || seenEnv[cache.Env] {
			return nil, refuse(ReasonWritableSetUnrepresentable, "cache variable %q is invalid, reserved or repeated", cache.Env)
		}
		seenEnv[cache.Env] = true
		writableDecl = append(writableDecl, declared{cache.Dir, ClassSessionCache})
	}

	resolved := &Resolved{
		SessionID:    spec.SessionID,
		HarnessID:    spec.HarnessID,
		WorkareaRoot: root,
		MetadataDir:  filepath.Join(root, workarea.DeclarationDirName),
	}
	var writablePaths []string
	for _, entry := range writableDecl {
		path, err := resolveDir(entry.path, string(entry.class), canonical, true)
		if err != nil {
			return nil, err
		}
		writablePaths = append(writablePaths, path)
		resolved.Writable = append(resolved.Writable, WritableRoot{Path: path, Class: entry.class})
	}

	// Pass 2: no declared path may traverse a symbolic link that lives inside
	// the writable set.
	allDeclared := []string{spec.WorkareaRoot}
	for _, entry := range writableDecl {
		allDeclared = append(allDeclared, entry.path)
	}
	allDeclared = append(allDeclared, spec.ReadOnlyLeaves...)
	allDeclared = append(allDeclared, spec.Protected...)
	for _, path := range allDeclared {
		if err := checkLinkPlants(path, writablePaths, canonical); err != nil {
			return nil, err
		}
	}

	for _, leaf := range spec.ReadOnlyLeaves {
		path, err := resolveDir(leaf, "read-only leaf", canonical, true)
		if err != nil {
			return nil, err
		}
		if !strictlyInside(path, root) {
			return nil, refuse(ReasonWritableSetUnrepresentable, "read-only leaf %q is not inside the workarea root", filepath.Base(path))
		}
		resolved.ReadOnly = append(resolved.ReadOnly, path)
		resolved.ReadOnlyLeafNames = append(resolved.ReadOnlyLeafNames, filepath.Base(path))
	}
	for _, protected := range spec.Protected {
		path, err := resolveExisting(protected, "protected path", canonical)
		if err != nil {
			return nil, err
		}
		resolved.Protected = append(resolved.Protected, path)
	}
	for _, socket := range spec.Sockets {
		if !filepath.IsAbs(socket) {
			return nil, refuse(ReasonWritableSetUnrepresentable, "declared socket must be an absolute path")
		}
		if info, err := os.Lstat(socket); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, refuse(ReasonWritableSetUnrepresentable, "declared socket is a symbolic link")
		}
		path, err := canonicalLoose(socket, canonical)
		if err != nil {
			return nil, refuse(ReasonWritableSetUnrepresentable, "declared socket: %v", errnoText(err))
		}
		resolved.Sockets = append(resolved.Sockets, path)
	}

	guardPaths := map[string]string{"workarea root": root}
	for name, raw := range map[string]string{"operator home": g.home, "host state home": g.stateHome, "profile directory": g.profileDir} {
		if raw == "" {
			continue
		}
		path, err := canonicalLoose(raw, canonical)
		if err != nil {
			return nil, refuse(ReasonWritableSetUnrepresentable, "%s: %v", name, err)
		}
		guardPaths[name] = path
	}
	if g.profileDir != "" {
		profileDir := guardPaths["profile directory"]
		if insideOrEqual(profileDir, root) {
			return nil, refuse(ReasonWritableSetUnrepresentable, "the profile directory is inside the workarea root")
		}
	}

	for _, entry := range resolved.Writable {
		if entry.Path == string(filepath.Separator) {
			return nil, refuse(ReasonWritableSetUnrepresentable, "%s is the filesystem root", entry.Class)
		}
		for name, guard := range guardPaths {
			if insideOrEqual(guard, entry.Path) {
				return nil, refuse(ReasonWritableSetUnrepresentable, "%s covers the %s", entry.Class, name)
			}
		}
		if entry.Class == ClassMutableLeaf {
			if !strictlyInside(entry.Path, root) {
				return nil, refuse(ReasonWritableSetUnrepresentable, "mutable leaf %q is not inside the workarea root", filepath.Base(entry.Path))
			}
			if err := checkOwnGitDir(entry.Path); err != nil {
				return nil, err
			}
		}
		for _, denied := range append(append([]string{}, resolved.ReadOnly...), resolved.Protected...) {
			if insideOrEqual(entry.Path, denied) {
				return nil, refuse(ReasonWritableSetUnrepresentable, "%s lies inside a read-only or protected path", entry.Class)
			}
		}
		if insideOrEqual(entry.Path, resolved.MetadataDir) {
			return nil, refuse(ReasonWritableSetUnrepresentable, "%s lies inside the workarea metadata", entry.Class)
		}
	}
	if err := checkHardLinks(writablePaths); err != nil {
		return nil, err
	}

	resolved.SessionTmp = resolved.Writable[len(spec.MutableLeaves)+len(spec.HarnessState)].Path
	for i, cache := range spec.Caches {
		resolved.Caches = append(resolved.Caches, Cache{Env: cache.Env, Dir: resolved.Writable[len(spec.MutableLeaves)+len(spec.HarnessState)+1+i].Path})
	}
	denied := append(append(append([]string{}, resolved.ReadOnly...), resolved.Protected...), resolved.MetadataDir)
	resolved.Pins = ancestorPins(writablePaths, denied)
	sort.SliceStable(resolved.Writable, func(i, j int) bool {
		if classOrder[resolved.Writable[i].Class] != classOrder[resolved.Writable[j].Class] {
			return classOrder[resolved.Writable[i].Class] < classOrder[resolved.Writable[j].Class]
		}
		return resolved.Writable[i].Path < resolved.Writable[j].Path
	})
	return resolved, nil
}

// resolveDir resolves an existing directory. When noLink is set the last
// component itself may not be a symbolic link: a harness that can remove its
// own leaf could otherwise replace it with a link to the home directory and
// widen the next rendering.
func resolveDir(raw, what string, canonical func(string) (string, error), noLink bool) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s must be an absolute path", what)
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s: %v", what, errnoText(err))
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if noLink {
			return "", refuse(ReasonWritableSetUnrepresentable, "%s is a symbolic link", what)
		}
	} else if !info.IsDir() {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s is not a directory", what)
	}
	path, err := canonical(raw)
	if err != nil {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s: %v", what, errnoText(err))
	}
	info, err = os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s is not a directory", what)
	}
	return path, nil
}

// resolveExisting resolves an existing path of any type whose last component
// is not a symbolic link.
func resolveExisting(raw, what string, canonical func(string) (string, error)) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s must be an absolute path", what)
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s: %v", what, errnoText(err))
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s is a symbolic link", what)
	}
	path, err := canonical(raw)
	if err != nil {
		return "", refuse(ReasonWritableSetUnrepresentable, "%s: %v", what, errnoText(err))
	}
	return path, nil
}

// canonicalLoose canonicalizes a path that may not exist yet: the longest
// existing ancestor is resolved and the rest is appended.
func canonicalLoose(raw string, canonical func(string) (string, error)) (string, error) {
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%q is not absolute", raw)
	}
	clean := filepath.Clean(raw)
	var rest []string
	for current := clean; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := canonical(current)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return clean, nil
		}
		rest = append(rest, filepath.Base(current))
	}
}

// checkLinkPlants walks raw component by component and refuses when any
// component is a symbolic link located inside a writable root: the harness
// can create links there, and a later rendering must not follow one.
func checkLinkPlants(raw string, writable []string, canonical func(string) (string, error)) error {
	clean := filepath.Clean(raw)
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if err != nil {
			return nil // a missing component is reported by the caller
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			parent, err := canonical(current)
			if err != nil {
				return refuse(ReasonWritableSetUnrepresentable, "resolve %q: %v", filepath.Base(current), errnoText(err))
			}
			for _, root := range writable {
				if insideOrEqual(parent, root) {
					return refuse(ReasonWritableSetUnrepresentable, "a declared path traverses a symbolic link inside the writable set")
				}
			}
		}
		current = next
	}
	return nil
}

// checkOwnGitDir refuses a mutable leaf whose .git is not its own directory:
// a worktree-add leaf points into a shared common directory (D2.4).
func checkOwnGitDir(leaf string) error {
	info, err := os.Lstat(filepath.Join(leaf, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return refuse(ReasonWritableSetUnrepresentable, "mutable leaf %q: .git: %v", filepath.Base(leaf), errnoText(err))
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return refuse(ReasonWritableSetUnrepresentable, "mutable leaf %q has no .git directory of its own (shared git common directory)", filepath.Base(leaf))
	}
	return nil
}

// checkHardLinks refuses a writable set holding a file with a hard link
// outside the set (D2.3): the kernel judges a write by the path it arrives
// through, so a write through the in-set name would land in the shared inode.
// Every link inside the set is counted; a file whose link count exceeds the
// links found inside is reachable from outside.
func checkHardLinks(roots []string) error {
	type tally struct {
		nlink uint64
		found uint64
	}
	counts := map[[2]any]*tally{}
	for _, root := range outermost(roots) {
		err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(st.Nlink) <= 1 {
				return nil
			}
			key := [2]any{st.Dev, st.Ino} // the field types differ across platforms
			entry := counts[key]
			if entry == nil {
				entry = &tally{nlink: uint64(st.Nlink)}
				counts[key] = entry
			}
			entry.found++
			return nil
		})
		if err != nil {
			return refuse(ReasonWritableSetUnrepresentable, "walk the writable set: %v", errnoText(err))
		}
	}
	for _, entry := range counts {
		if entry.found < entry.nlink {
			return refuse(ReasonWritableSetUnrepresentable, "a file in the writable set is hard-linked to a path outside it")
		}
	}
	return nil
}

// outermost drops every root nested inside another, so each file in the set
// is visited exactly once.
func outermost(roots []string) []string {
	var kept []string
	for i, root := range roots {
		nested := false
		for j, other := range roots {
			if i == j {
				continue
			}
			if strictlyInside(root, other) || (strings.EqualFold(root, other) && j < i) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, root)
		}
	}
	return kept
}

// ancestorPins returns, for every denied path strictly inside a writable
// root, that root and each directory between it and the denied path. Denying
// writes to those literals stops a rename of an ancestor from carrying the
// denied path out from under its rule.
func ancestorPins(writable, denied []string) []string {
	seen := map[string]bool{}
	var pins []string
	for _, deny := range denied {
		outer := ""
		for _, root := range writable {
			if strictlyInside(deny, root) && (outer == "" || len(root) < len(outer)) {
				outer = root
			}
		}
		if outer == "" {
			continue
		}
		for dir := filepath.Dir(deny); ; dir = filepath.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				pins = append(pins, dir)
			}
			if dir == outer || dir == filepath.Dir(dir) {
				break
			}
		}
	}
	sort.Strings(pins)
	return pins
}

// insideOrEqual reports whether path is base or below it. Comparison folds
// case: the macOS boundary matches case-insensitively on the default volume,
// so a case variant must be treated as the same path here too.
func insideOrEqual(path, base string) bool {
	p, b := strings.ToLower(filepath.Clean(path)), strings.ToLower(filepath.Clean(base))
	if p == b {
		return true
	}
	if b == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(p, b+string(filepath.Separator))
}

func strictlyInside(path, base string) bool {
	return insideOrEqual(path, base) && !strings.EqualFold(filepath.Clean(path), filepath.Clean(base))
}

// errnoText strips paths from an error so refusal details carry none.
func errnoText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error()
	}
	return err.Error()
}
