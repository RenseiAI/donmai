package pi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/RenseiAI/donmai/internal/toolpolicy"
)

// foldPathCase reports a platform whose default volumes ignore letter case,
// so "SECRET/k" and "secret/k" name the same file.
var foldPathCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// maxSymlinkHops bounds how many dangling symlinks resolvePhysical follows.
const maxSymlinkHops = 40

// maxBraceExpansions bounds the alternatives one deny's brace sets expand to
// for the walk check; a deny with more reaches every walk.
const maxBraceExpansions = 256

// pathDenies enforces the scoped path denies of one tool family (every read
// tool, or every write tool).
//
// It fails closed against every other spelling of a denied path. Each deny is
// anchored as an absolute glob at the workspace root or home, both as written
// and with its literal directory (everything before its first glob
// character, root or home included) resolved on disk. A path is matched both
// as written and as resolved, and on a case-insensitive platform at several
// fold levels (see foldLevels); any match denies. A path that cannot be
// resolved, holds or resolves to a control character, or is relative with no
// root, is denied; so is every path while a deny's own directory cannot be
// resolved.
//
// grep, find and ls walk a directory, so for them a deny also applies when
// the walk could reach a path it names: the directory is the deny's literal
// directory, an ancestor of it, or a directory beneath a "**", a wildcard or
// a brace set of the deny's glob.
type pathDenies struct {
	levels     []foldedDenies
	root       string
	unresolved []string
}

// foldedDenies are the denies compiled with every glob folded one way; a
// path is folded the same way before it is matched.
type foldedDenies struct {
	fold  func(string) string
	rules toolpolicy.Paths
	walks []walkGlob
}

// foldLevels are the foldings a deny is matched at. Full case folding between
// decompositions meets every spelling a case-insensitive volume equates, but
// it changes how many characters a letter spans, so a "?" or a one-character
// class stops matching "é", "ß" or "ﬁ". Matching also unfolded, and with the
// simple per-letter fold of the composed form, keeps those denies. Without
// folding, only the unfolded level applies.
func foldLevels(fold bool) []func(string) string {
	unfolded := func(s string) string { return s }
	if !fold {
		return []func(string) string{unfolded}
	}
	return []func(string) string{
		unfolded,
		func(s string) string { return strings.ToLower(strings.ToUpper(norm.NFC.String(s))) },
		func(s string) string { return norm.NFD.String(cases.Fold().String(norm.NFD.String(s))) },
	}
}

// compilePathDenies compiles the denies of the tools family accepts, with
// relative denies anchored at root. A root that is not absolute is no root,
// so a relative deny then denies every path.
func compilePathDenies(patterns []string, family func(string) bool, root, home string, fold bool) (pathDenies, []error) {
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
		if !family(name) {
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
			d.unresolved = append(d.unresolved, raw)
			errs = append(errs, fmt.Errorf("%q: directory cannot be resolved", raw))
		case canonical != abs:
			add(name, canonical)
		}
	}
	if len(rules) == 0 {
		return d, errs
	}
	reported := map[string]bool{}
	for _, f := range foldLevels(fold) {
		expanded := make([]string, 0, len(rules))
		var walks []walkGlob
		for _, r := range rules {
			if !r.anchored {
				expanded = append(expanded, r.name+r.glob)
				continue
			}
			folded := f(r.glob)
			expanded = append(expanded, r.name+"("+folded+")")
			walks = append(walks, compileWalkGlob(folded))
		}
		compiled, compileErrs := toolpolicy.CompilePaths(expanded, family, toolpolicy.Deny, "", "")
		// A deny that only one fold level cannot compile still denies every
		// path; report each such failure once.
		for _, err := range compileErrs {
			if !reported[err.Error()] {
				reported[err.Error()] = true
				errs = append(errs, err)
			}
		}
		d.levels = append(d.levels, foldedDenies{fold: f, rules: compiled, walks: walks})
	}
	return d, errs
}

// active reports whether any path deny is configured.
func (d pathDenies) active() bool {
	return len(d.levels) > 0 && d.levels[0].rules.Len() > 0
}

// denies reports whether access to path is denied, with the reason. walk
// marks a tool that walks path; variants adds the alternate names pi's read
// tool falls back to (piReadVariants).
func (d pathDenies) denies(path string, walk, variants bool) (bool, string) {
	if !d.active() {
		return false, ""
	}
	if len(d.unresolved) > 0 {
		return true, "a path deny's directory cannot be resolved, so every path is denied: " + strings.Join(d.unresolved, ", ")
	}
	if !filepath.IsAbs(path) {
		if d.root == "" {
			return true, "relative path without a workspace root cannot be checked against path denies"
		}
		path = d.root + string(filepath.Separator) + path
	}
	written := []string{filepath.Clean(path)}
	if variants {
		written = append(written, piReadVariants(written[0])...)
	}
	for _, p := range written {
		physical, err := resolvePhysical(p, 0)
		if err != nil {
			return true, "path cannot be resolved to check it against path denies"
		}
		if strings.ContainsFunc(p, unicode.IsControl) || strings.ContainsFunc(physical, unicode.IsControl) {
			// A newline would end a deny's match early, so no deny can vouch
			// for a name holding one, whether written in the path or reached
			// through a symlink.
			return true, "path holds or resolves to a control character"
		}
		for _, form := range []string{p, physical} {
			for _, l := range d.levels {
				folded := l.fold(form)
				if l.rules.Match(folded, "") {
					return true, "path matches a disallow pattern"
				}
				if walk && reachesAny(l.walks, folded) {
					return true, "walking this directory reaches a path a disallow pattern names"
				}
			}
		}
	}
	return false, ""
}

// piReadVariants are the names pi's read tool tries, in its order, when the
// typed path does not exist: a macOS screenshot name with a narrow no-break
// space before AM/PM, the decomposed form, and a curly apostrophe.
func piReadVariants(path string) []string {
	amPM := screenshotTime.ReplaceAllString(path, " $1.")
	nfd := norm.NFD.String(path)
	curly := strings.ReplaceAll(path, "'", "’")
	nfdCurly := strings.ReplaceAll(nfd, "'", "’")
	var out []string
	for _, v := range []string{amPM, nfd, curly, nfdCurly} {
		if v != path {
			out = append(out, v)
		}
	}
	return out
}

var screenshotTime = regexp.MustCompile(`(?i) (AM|PM)\.`)

// walkGlob is one deny glob split into path segments per brace alternative,
// for the walk check. all marks a glob too large to expand: it reaches every
// walk.
type walkGlob struct {
	alts [][]walkSegment
	all  bool
}

// walkSegment is one segment of a glob: a whole "**", or a pattern for one
// path segment (nil when it cannot be compiled, which matches every name).
type walkSegment struct {
	globstar bool
	rules    *toolpolicy.Paths
}

func compileWalkGlob(glob string) walkGlob {
	alts, ok := expandBraces(filepath.Clean(glob), maxBraceExpansions)
	if !ok {
		return walkGlob{all: true}
	}
	var w walkGlob
	for _, alt := range alts {
		var segs []walkSegment
		for _, s := range splitGlob(filepath.Clean(alt)) {
			if s == "**" {
				segs = append(segs, walkSegment{globstar: true})
				continue
			}
			// The "/x" sentinel keeps the segment off the end of the
			// pattern, where its spaces would be trimmed.
			rules, errs := toolpolicy.CompilePaths([]string{"Read(/" + s + "/x)"}, isReadToolName, toolpolicy.Allow, "", "")
			if len(errs) > 0 || rules.Len() == 0 {
				segs = append(segs, walkSegment{})
				continue
			}
			segs = append(segs, walkSegment{rules: &rules})
		}
		w.alts = append(w.alts, segs)
	}
	return w
}

// reachesAny reports whether walking dir can reach a path any glob matches:
// dir and a match lie on one branch of the tree, so the match is dir, beneath
// it, or one of its ancestors (a deny also covers the tree beneath it).
func reachesAny(walks []walkGlob, dir string) bool {
	var names []string
	for _, n := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if n != "" {
			names = append(names, n)
		}
	}
	for _, w := range walks {
		if w.all {
			return true
		}
		for _, segs := range w.alts {
			if segmentsReach(segs, names) {
				return true
			}
		}
	}
	return false
}

func segmentsReach(segs []walkSegment, names []string) bool {
	memo := map[[2]int]bool{}
	var reach func(si, ni int) bool
	reach = func(si, ni int) bool {
		if si == len(segs) || ni == len(names) {
			return true
		}
		key := [2]int{si, ni}
		if v, ok := memo[key]; ok {
			return v
		}
		var r bool
		switch s := segs[si]; {
		case s.globstar:
			r = reach(si+1, ni) || reach(si, ni+1)
		case s.rules == nil || s.rules.Match("/"+names[ni]+"/x", ""):
			r = reach(si+1, ni+1)
		}
		memo[key] = r
		return r
	}
	return reach(0, 0)
}

// expandBraces expands a glob's brace sets the way toolpolicy reads them
// (an escape or a "[...]" class keeps its characters literal), reporting
// false past limit alternatives. An unbalanced set is left as written.
func expandBraces(glob string, limit int) ([]string, bool) {
	open, end, commas := braceSet(glob)
	if open < 0 {
		return []string{glob}, true
	}
	var out []string
	start := open + 1
	for _, c := range append(commas, end) {
		alts, ok := expandBraces(glob[:open]+glob[start:c]+glob[end+1:], limit-len(out))
		if !ok || len(out)+len(alts) > limit {
			return nil, false
		}
		out = append(out, alts...)
		start = c + 1
	}
	return out, true
}

// braceSet finds a glob's first balanced brace set: the index of its "{",
// its "}" and its top-level commas, or open < 0 when there is none.
func braceSet(glob string) (open, end int, commas []int) {
	open, depth := -1, 0
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			i++
		case '[':
			if j := strings.IndexByte(glob[i+1:], ']'); j >= 0 {
				i += j + 1
			}
		case '{':
			if depth == 0 {
				open = i
			}
			depth++
		case ',':
			if depth == 1 {
				commas = append(commas, i)
			}
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				return open, i, commas
			}
		}
	}
	return -1, -1, nil
}

// splitGlob splits an absolute glob into segments at each "/", including an
// escaped one (an escaped "/" is still the separator), and keeps a "[...]"
// class whole.
func splitGlob(glob string) []string {
	var segs []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			segs = append(segs, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case c == '\\' && i+1 < len(glob):
			if glob[i+1] == '/' {
				flush()
			} else {
				b.WriteString(glob[i : i+2])
			}
			i++
		case c == '[':
			j := strings.IndexByte(glob[i+1:], ']')
			if j < 0 {
				b.WriteString(glob[i:])
				i = len(glob)
				continue
			}
			b.WriteString(glob[i : i+j+2])
			i += j + 1
		case c == '/':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return segs
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

// escapeGlob makes a literal directory safe to prefix to a glob. It copies
// bytes, so invalid UTF-8 reaches toolpolicy and fails closed there.
func escapeGlob(dir string) string {
	var b strings.Builder
	for i := 0; i < len(dir); i++ {
		if strings.IndexByte(`*?[]{}\`, dir[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(dir[i])
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
