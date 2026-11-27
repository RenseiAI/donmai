package muse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

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
// It fails closed against every other spelling of a denied path. Each deny is
// anchored here as an absolute glob, both as written and with its literal
// directory (everything before its first glob character, root or home
// included) resolved on disk. An approval path is matched both as written and
// as resolved. On a case-insensitive platform it matches at several fold
// levels and denies when any matches (see foldLevels). A path that cannot be
// resolved, holds or resolves to a control character, or is relative with no
// root to resolve it against, is denied; so is every path while a deny's own
// directory cannot be resolved.
type pathDenies struct {
	levels     []foldedRules
	root       string
	unresolved []string
}

// foldedRules are the deny rules compiled with every glob folded one way;
// an approval path is folded the same way before it is matched.
type foldedRules struct {
	fold  func(string) string
	rules toolpolicy.Paths
}

// foldLevels are the foldings a deny is matched at. Full case folding between
// decompositions meets every spelling a case-insensitive volume equates, but
// it changes how many characters a letter spans, so a "?" or a one-character
// class stops matching "é", "ß" or "ﬁ". Matching also unfolded, and with the
// simple per-letter fold, keeps those denies. Without folding, only the
// unfolded level applies.
func foldLevels(fold bool) []func(string) string {
	unfolded := func(s string) string { return s }
	if !fold {
		return []func(string) string{unfolded}
	}
	return []func(string) string{
		unfolded,
		func(s string) string { return strings.ToLower(strings.ToUpper(s)) },
		func(s string) string { return norm.NFD.String(cases.Fold().String(norm.NFD.String(s))) },
	}
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
	d := pathDenies{root: root}

	// A rule is a tool name with an absolute glob to fold, or, when anchored
	// is false, a pattern passed to toolpolicy as written.
	type rule struct {
		name, glob string
		anchored   bool
	}
	var rules []rule
	var errs []error
	add := func(name, glob string) { rules = append(rules, rule{name, glob, true}) }
	for _, raw := range patterns {
		name, arg, scoped := toolpolicy.ParseToolPattern(strings.TrimSpace(raw))
		if !isPathToolName(name) {
			continue
		}
		if !scoped {
			rules = append(rules, rule{name: name})
			continue
		}
		if filepath.Clean(arg) == "/" {
			// A deny on the filesystem root covers every absolute path.
			arg = "/**"
		}
		abs, ok := anchoredGlob(arg, root, home)
		if !ok {
			// No root or home to anchor it at: toolpolicy rejects the
			// pattern, which denies every path.
			rules = append(rules, rule{name: name, glob: "(" + arg + ")"})
			continue
		}
		add(name, abs)
		canonical, ok := canonicalGlob(abs)
		switch {
		case !ok:
			// The deny's own directory cannot be resolved, so nothing can be
			// shown to lie outside it: deny every path.
			d.unresolved = append(d.unresolved, raw)
			errs = append(errs, fmt.Errorf("%q: directory cannot be resolved", raw))
		case canonical != abs:
			add(name, canonical)
		}
	}
	if len(rules) == 0 {
		return d, nil
	}
	for i, f := range foldLevels(fold) {
		expanded := make([]string, 0, len(rules))
		for _, r := range rules {
			if r.anchored {
				expanded = append(expanded, r.name+"("+asciiSafeGlob(f(r.glob))+")")
			} else {
				expanded = append(expanded, r.name+r.glob)
			}
		}
		compiled, compileErrs := toolpolicy.CompilePaths(expanded, isPathToolName, toolpolicy.Deny, "", "")
		if i == 0 {
			errs = append(errs, compileErrs...)
		}
		d.levels = append(d.levels, foldedRules{fold: f, rules: compiled})
	}
	return d, errs
}

// active reports whether any path deny is configured.
func (d pathDenies) active() bool {
	return len(d.levels) > 0 && d.levels[0].rules.Len() > 0
}

// denies reports whether a file access to path is denied, with the reason.
func (d pathDenies) denies(path string) (bool, string) {
	if !d.active() {
		return false, ""
	}
	if len(d.unresolved) > 0 {
		return true, "a path deny's directory cannot be resolved, so every file access is denied: " + strings.Join(d.unresolved, ", ")
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
	if strings.ContainsFunc(path, unicode.IsControl) || strings.ContainsFunc(physical, unicode.IsControl) {
		// A newline would end a deny's match early, so no deny can vouch for
		// a name holding one, whether written in the path or reached
		// through a symlink.
		return true, "path holds or resolves to a control character"
	}
	for _, form := range []string{filepath.Clean(path), physical} {
		for _, l := range d.levels {
			if l.rules.Match(l.fold(form), "") {
				return true, "path matches a disallow pattern"
			}
		}
	}
	return false, ""
}

// anchoredGlob returns the absolute glob a path-deny argument names,
// mirroring toolpolicy's anchoring: a relative deny without a "/" matches at
// any depth under the root. It reports false when there is no root or home to
// anchor the argument at, or the argument names another user's home.
func anchoredGlob(arg, root, home string) (string, bool) {
	switch {
	case arg == "~" || strings.HasPrefix(arg, "~/"):
		return escapeGlob(home) + arg[1:], home != ""
	case strings.HasPrefix(arg, "//"):
		return arg[1:], true
	case strings.HasPrefix(arg, "/"):
		return arg, true
	case strings.HasPrefix(arg, "~"):
		return "", false
	case !strings.Contains(arg, "/"):
		return escapeGlob(root) + "/**/" + arg, root != ""
	default:
		return escapeGlob(root) + "/" + arg, root != ""
	}
}

// asciiSafeGlob replaces every non-ASCII letter of an absolute glob with a
// single-character wildcard, and any character class holding one with a
// within-segment wildcard. toolpolicy
// builds its expressions byte by byte and never matches a non-ASCII literal,
// so this keeps such a deny matching, at the cost of also covering names that
// differ only in that letter.
func asciiSafeGlob(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); {
		r, size := utf8.DecodeRuneInString(glob[i:])
		switch {
		case r == '\\' && i+size < len(glob):
			next, nsize := utf8.DecodeRuneInString(glob[i+size:])
			if next >= utf8.RuneSelf {
				b.WriteByte('?')
			} else {
				b.WriteString(glob[i : i+size+nsize])
			}
			i += size + nsize
			continue
		case r == '[':
			if end := strings.IndexByte(glob[i+1:], ']'); end >= 0 {
				class := glob[i : i+end+2]
				if strings.IndexFunc(class, func(c rune) bool { return c >= utf8.RuneSelf }) >= 0 {
					// A decomposed member can span several characters.
					b.WriteByte('*')
				} else {
					b.WriteString(class)
				}
				i += len(class)
				continue
			}
		case r >= utf8.RuneSelf:
			b.WriteByte('?')
			i += size
			continue
		}
		b.WriteString(glob[i : i+size])
		i += size
	}
	return b.String()
}

// canonicalGlob resolves the literal directory an absolute glob starts with
// (everything before its first unescaped glob character), so a deny whose
// literal directory passes through a symlink also covers the directory it
// points to. A symlink after the first glob character is not resolved. It
// reports false when that directory cannot be resolved.
func canonicalGlob(glob string) (string, bool) {
	literal := glob
	if i := firstUnescapedMeta(glob); i >= 0 {
		literal = glob[:strings.LastIndexByte(glob[:i], '/')+1]
	}
	if literal == "" || literal == "/" {
		return glob, true
	}
	resolved, err := resolvePhysical(unescapeGlob(literal), 0)
	if err != nil {
		return "", false
	}
	tail := glob[len(literal):]
	if strings.HasSuffix(literal, "/") && tail != "" {
		tail = "/" + tail
	}
	return escapeGlob(resolved) + tail, true
}

// firstUnescapedMeta returns the index of the first glob character a
// backslash does not escape, or -1.
func firstUnescapedMeta(glob string) int {
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			i++
		case '*', '?', '[', '{':
			return i
		}
	}
	return -1
}

// unescapeGlob drops the backslashes that make glob characters literal.
func unescapeGlob(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		if glob[i] == '\\' && i+1 < len(glob) {
			i++
		}
		b.WriteByte(glob[i])
	}
	return b.String()
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
