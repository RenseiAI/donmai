package muse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/RenseiAI/donmai/internal/toolpolicy"
)

// foldPathCase reports a platform whose default volumes ignore letter case,
// so "DOCS/a.md" and "docs/a.md" name the same file.
var foldPathCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// maxSymlinkHops bounds how many dangling symlinks resolvePhysical follows.
const maxSymlinkHops = 40

// pathDenies enforces the scoped path denies of every read and write tool
// ("Read(~/.ssh/**)", "Edit(docs/**)") on file-access approvals. A
// file-access approval does not say whether it reads or writes, so either
// kind of deny applies.
//
// It fails closed against every other spelling of a denied path: the rules
// are compiled once per spelling of their anchors (the root and home as
// given and as resolved, and each pattern's literal directory resolved), an
// approval path is matched both as written and as resolved on disk, and on a
// case-insensitive platform both sides are case-folded. A path that cannot be
// resolved, or a relative path with no root to resolve it against, is denied.
type pathDenies struct {
	rules []toolpolicy.Paths
	root  string
	fold  bool
}

// compilePathDenies compiles the path denies among patterns with relative
// patterns anchored at root. A root that is not absolute is no root, so a
// relative deny then denies every file access.
func compilePathDenies(patterns []string, root, home string, fold bool) (pathDenies, []error) {
	if root != "" && !filepath.IsAbs(root) {
		root = ""
	}
	if root != "" {
		root = filepath.Clean(root)
	}
	d := pathDenies{fold: fold}
	roots := spellings(root)
	if len(roots) > 0 {
		d.root = roots[len(roots)-1]
	}
	homes := spellings(home)

	var expanded []string
	for _, raw := range patterns {
		name, arg, scoped := toolpolicy.ParseToolPattern(strings.TrimSpace(raw))
		if !isPathToolName(name) {
			continue
		}
		if !scoped {
			expanded = append(expanded, name)
			continue
		}
		if filepath.Clean(arg) == "/" {
			// A deny on the filesystem root covers every absolute path.
			arg = "/**"
		}
		expanded = append(expanded, name+"("+arg+")")
		for _, abs := range anchoredGlobs(arg, roots, homes) {
			canonical, ok := canonicalGlob(abs)
			switch {
			case !ok:
				// The deny's own directory cannot be resolved: deny every path.
				expanded = append(expanded, name)
			case canonical != abs:
				expanded = append(expanded, name+"("+canonical+")")
			}
		}
	}
	if len(expanded) == 0 {
		return d, nil
	}

	var errs []error
	if len(roots) == 0 {
		roots = []string{""}
	}
	if len(homes) == 0 {
		homes = []string{""}
	}
	for i, r := range roots {
		for j, h := range homes {
			compiled, compileErrs := toolpolicy.CompilePaths(foldAll(expanded, fold), isPathToolName, toolpolicy.Deny, foldCase(r, fold), foldCase(h, fold))
			if i == 0 && j == 0 {
				errs = compileErrs
			}
			d.rules = append(d.rules, compiled)
		}
	}
	return d, errs
}

// active reports whether any path deny is configured.
func (d pathDenies) active() bool {
	for _, r := range d.rules {
		if r.Len() > 0 {
			return true
		}
	}
	return false
}

// denies reports whether a file access to path is denied, with the reason.
func (d pathDenies) denies(path string) (bool, string) {
	if !d.active() {
		return false, ""
	}
	if !filepath.IsAbs(path) {
		if d.root == "" {
			return true, "relative path without a workspace root cannot be checked against path denies"
		}
		path = d.root + string(filepath.Separator) + path
	}
	physical, err := resolvePhysical(path, 0)
	if err != nil {
		return true, "path cannot be resolved to check it against path denies"
	}
	for _, form := range []string{filepath.Clean(path), physical} {
		form = foldCase(form, d.fold)
		for _, r := range d.rules {
			if r.Match(form, "") {
				return true, "path matches a disallow pattern"
			}
		}
	}
	return false, ""
}

// spellings returns dir as given and, when it differs, as resolved on disk;
// the resolved spelling is last. An empty dir has none.
func spellings(dir string) []string {
	if dir == "" {
		return nil
	}
	out := []string{dir}
	if resolved, err := resolvePhysical(dir, 0); err == nil && resolved != dir {
		out = append(out, resolved)
	}
	return out
}

// anchoredGlobs returns the absolute globs a path-deny argument names, for
// each spelling of the root and home it is anchored at. A relative argument
// without a "/" matches at any depth, so its literal directory is the root
// itself and the root spellings already cover it.
func anchoredGlobs(arg string, roots, homes []string) []string {
	switch {
	case arg == "~" || strings.HasPrefix(arg, "~/"):
		out := make([]string, 0, len(homes))
		for _, h := range homes {
			out = append(out, escapeGlob(h)+arg[1:])
		}
		return out
	case strings.HasPrefix(arg, "//"):
		return []string{arg[1:]}
	case strings.HasPrefix(arg, "/"):
		return []string{arg}
	case strings.HasPrefix(arg, "~") || !strings.Contains(arg, "/"):
		return nil
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, escapeGlob(r)+"/"+arg)
	}
	return out
}

// canonicalGlob resolves the literal directory an absolute glob starts with,
// so a deny naming a symlink also covers the directory it points to. It
// reports false when that directory cannot be resolved.
func canonicalGlob(glob string) (string, bool) {
	literal := glob
	if i := strings.IndexAny(glob, `*?[{\`); i >= 0 {
		literal = glob[:strings.LastIndexByte(glob[:i], '/')+1]
	}
	if literal == "" || literal == "/" {
		return glob, true
	}
	resolved, err := resolvePhysical(literal, 0)
	if err != nil {
		return "", false
	}
	tail := glob[len(literal):]
	if strings.HasSuffix(literal, "/") && tail != "" {
		tail = "/" + tail
	}
	return escapeGlob(resolved) + tail, true
}

// resolvePhysical resolves every symlink in path the way the kernel would,
// including a dangling symlink a write would create the target of. A
// missing tail is kept as written; any other failure (a symlink loop, an
// unreadable directory) is an error.
func resolvePhysical(path string, hops int) (string, error) {
	if hops > maxSymlinkHops {
		return "", errors.New("too many symlinks")
	}
	cur, rest := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", cur, err)
		}
		trimmed := strings.TrimRight(cur, string(filepath.Separator))
		i := strings.LastIndexByte(trimmed, filepath.Separator)
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			target, rerr := os.Readlink(cur)
			if rerr != nil {
				return "", fmt.Errorf("read link %s: %w", cur, rerr)
			}
			if !filepath.IsAbs(target) && i >= 0 {
				target = trimmed[:i+1] + target
			}
			return resolvePhysical(target+string(filepath.Separator)+rest, hops+1)
		}
		if i < 0 {
			return filepath.Clean(path), nil
		}
		rest = filepath.Join(trimmed[i+1:], rest)
		cur = trimmed[:i]
		if cur == "" {
			cur = string(filepath.Separator)
		}
	}
}

// escapeGlob makes a literal directory safe to prefix to a glob.
func escapeGlob(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if strings.ContainsRune(`*?[]{}\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func foldCase(s string, fold bool) string {
	if fold {
		return strings.ToLower(s)
	}
	return s
}

// foldAll folds the arguments of patterns, leaving the tool names alone.
func foldAll(patterns []string, fold bool) []string {
	if !fold {
		return patterns
	}
	out := make([]string, len(patterns))
	for i, p := range patterns {
		name, arg, scoped := toolpolicy.ParseToolPattern(p)
		if scoped {
			p = name + "(" + strings.ToLower(arg) + ")"
		}
		out[i] = p
	}
	return out
}
