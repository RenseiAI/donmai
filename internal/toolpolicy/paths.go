package toolpolicy

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Paths is one side's compiled path rules for one tool family.
type Paths struct {
	rules []*regexp.Regexp
	// bases holds each rule's literal directory: the part of its glob before
	// the first wildcard ("/" for a rule that covers every path).
	bases []string
}

var matchEveryPath = regexp.MustCompile(`(?s)^.*$`)

// CompilePaths compiles the patterns of the tools the family predicate
// accepts ("Read(~/.ssh/**)", "Edit(docs/**)", a bare "Edit"). A bare name
// covers every path. Relative patterns are anchored at root and "~/" expands
// to home.
//
// Path patterns are gitignore-style globs: "**" spans directories, "*" and
// "?" stay within one, "[...]" is a class and "{a,b}" a choice. A deny also
// covers everything beneath the path it names, and a relative deny without a
// "/" matches at any depth under root; an allow matches exactly.
//
// A pattern that cannot be compiled is returned as an error and fails closed:
// on the deny side it denies every path, on the allow side it allows none.
func CompilePaths(patterns []string, family func(name string) bool, side Side, root, home string) (Paths, []error) {
	var p Paths
	var errs []error
	for _, raw := range patterns {
		name, arg, scoped := ParseToolPattern(strings.TrimSpace(raw))
		if !family(name) {
			continue
		}
		if !scoped {
			p.rules, p.bases = append(p.rules, matchEveryPath), append(p.bases, "/")
			continue
		}
		re, base, err := compilePathGlob(arg, side, root, home)
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", raw, err))
			if side == Deny {
				p.rules, p.bases = append(p.rules, matchEveryPath), append(p.bases, "/")
			}
			continue
		}
		p.rules, p.bases = append(p.rules, re), append(p.bases, base)
	}
	return p, errs
}

// Under reports whether dir is a rule's literal directory or one of its
// ancestors, so a tool that walks dir could reach paths the rule matches.
func (p Paths) Under(dir, root string) bool {
	if !filepath.IsAbs(dir) && root != "" {
		dir = filepath.Join(root, dir)
	}
	dir = filepath.Clean(dir)
	for _, base := range p.bases {
		if base == dir || dir == "/" || strings.HasPrefix(base, dir+"/") {
			return true
		}
	}
	return false
}

// Len reports how many path rules compiled.
func (p Paths) Len() int { return len(p.rules) }

// Match reports whether path matches; a relative path is resolved against root.
func (p Paths) Match(path, root string) bool {
	if !filepath.IsAbs(path) && root != "" {
		path = filepath.Join(root, path)
	}
	return matchesAny(p.rules, filepath.Clean(path))
}

func compilePathGlob(glob string, side Side, root, home string) (*regexp.Regexp, string, error) {
	switch {
	case glob == "" || strings.ContainsFunc(glob, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return nil, "", errors.New("empty path or control character")
	case glob == "~" || strings.HasPrefix(glob, "~/"):
		if home == "" {
			return nil, "", errors.New("no home directory to expand ~ against")
		}
		glob = escapeGlob(home) + glob[1:]
	case strings.HasPrefix(glob, "~"):
		return nil, "", errors.New("only ~/ home paths are supported")
	case strings.HasPrefix(glob, "//"):
		glob = glob[1:]
	case strings.HasPrefix(glob, "/"):
	case root == "":
		return nil, "", errors.New("relative path pattern without a root")
	case side == Deny && !strings.Contains(glob, "/"):
		glob = escapeGlob(root) + "/**/" + glob
	default:
		glob = escapeGlob(root) + "/" + glob
	}
	glob = filepath.Clean(glob)
	body, err := globRegexp(glob)
	if err != nil {
		return nil, "", err
	}
	tail := `$`
	if side == Deny {
		tail = `(?:/.*)?$`
	}
	re, err := regexp.Compile(`^` + body + tail)
	return re, literalDir(glob), err
}

// literalDir is the unescaped part of a glob before its first wildcard, cut
// back to a whole directory.
func literalDir(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		if glob[i] == '\\' && i+1 < len(glob) {
			i++
		} else if strings.IndexByte("*?[{", glob[i]) >= 0 {
			return filepath.Dir(b.String() + "x")
		}
		b.WriteByte(glob[i])
	}
	return b.String()
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

// globRegexp translates a cleaned absolute glob into a regexp body; a
// backslash makes the next character literal.
func globRegexp(glob string) (string, error) {
	var b strings.Builder
	braces := 0
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '\\' && i+1 < len(glob):
			b.WriteString(regexp.QuoteMeta(glob[i+1 : i+2]))
			i++
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString(`(?:.*/)?`)
			i += 2
		case strings.HasPrefix(glob[i:], "/**") && i+3 == len(glob):
			b.WriteString(`(?:/.*)?`)
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(`.*`)
			i++
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		case c == '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				return "", errors.New("unclosed character class")
			}
			class := glob[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case c == '{':
			braces++
			b.WriteString(`(?:`)
		case c == ',' && braces > 0:
			b.WriteString(`|`)
		case c == '}':
			if braces == 0 {
				return "", errors.New("unbalanced brace set")
			}
			braces--
			b.WriteString(`)`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if braces != 0 {
		return "", errors.New("unbalanced brace set")
	}
	return b.String(), nil
}
