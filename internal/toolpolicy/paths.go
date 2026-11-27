package toolpolicy

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Paths is one side's compiled path rules for one tool family.
type Paths struct {
	rules []*regexp.Regexp
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
			p.rules = append(p.rules, matchEveryPath)
			continue
		}
		re, err := compilePathGlob(arg, side, root, home)
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", raw, err))
			if side == Deny {
				p.rules = append(p.rules, matchEveryPath)
			}
			continue
		}
		p.rules = append(p.rules, re)
	}
	return p, errs
}

// Len reports how many path rules compiled.
func (p Paths) Len() int { return len(p.rules) }

// Match reports whether path matches; a relative path is resolved against root.
// Callers must pass the root and expand a leading "~" first: Match treats "~"
// as an ordinary directory name. Matching is byte-exact, so on a
// case-insensitive filesystem a differently cased path is not matched; like a
// symlink, that alias is a residual risk the caller owns.
func (p Paths) Match(path, root string) bool {
	if !filepath.IsAbs(path) && root != "" {
		path = filepath.Join(root, path)
	}
	return matchesAny(p.rules, filepath.Clean(path))
}

func compilePathGlob(glob string, side Side, root, home string) (*regexp.Regexp, error) {
	switch {
	case glob == "" || strings.ContainsFunc(glob, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return nil, errors.New("empty path or control character")
	case glob == "~" || strings.HasPrefix(glob, "~/"):
		if home == "" {
			return nil, errors.New("no home directory to expand ~ against")
		}
		glob = escapeGlob(home) + glob[1:]
	case strings.HasPrefix(glob, "~"):
		return nil, errors.New("only ~/ home paths are supported")
	case strings.HasPrefix(glob, "//"):
		glob = glob[1:]
	case strings.HasPrefix(glob, "/"):
	case root == "":
		return nil, errors.New("relative path pattern without a root")
	case side == Deny && !strings.Contains(glob, "/"):
		glob = escapeGlob(root) + "/**/" + glob
	default:
		glob = escapeGlob(root) + "/" + glob
	}
	body, err := globRegexp(filepath.Clean(glob))
	if err != nil {
		return nil, err
	}
	tail := `$`
	switch {
	case side == Deny && body == "/":
		// A deny of the filesystem root covers every path beneath it.
		tail = `.*$`
	case side == Deny:
		tail = `(?:/.*)?$`
	}
	// (?s): a newline in a file name must not stop "**" or the deny subtree.
	return regexp.Compile(`(?s)^` + body + tail)
}

// escapeGlob makes a literal directory safe to prefix to a glob. It copies
// bytes, not runes, so invalid UTF-8 reaches globRegexp and fails closed there
// instead of becoming U+FFFD.
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

// globRegexp translates a cleaned absolute glob into a regexp body; a
// backslash makes the next character literal. The glob is walked rune by rune
// so a multibyte character is quoted whole rather than as Latin-1 bytes.
func globRegexp(glob string) (string, error) {
	var b strings.Builder
	// segment: the next token starts a path segment, so a "**/" there spans
	// directories. Inside a brace set each alternative inherits the segment
	// state of its "{", as it would after brace expansion.
	segment := false
	var braces []bool
	for i := 0; i < len(glob); {
		c, size := utf8.DecodeRuneInString(glob[i:])
		if c == utf8.RuneError && size == 1 {
			return "", errors.New("invalid UTF-8")
		}
		rest := glob[i:]
		next := false
		switch {
		case c == '\\' && i+size < len(glob):
			_, n := utf8.DecodeRuneInString(glob[i+size:])
			b.WriteString(regexp.QuoteMeta(glob[i+size : i+size+n]))
			next = glob[i+size] == '/'
			size += n
		case segment && strings.HasPrefix(rest, "**/"):
			b.WriteString(`(?:.*/)?`)
			size, next = 3, true
		case strings.HasPrefix(rest, "/**") && i+3 == len(glob):
			b.WriteString(`(?:/.*)?`)
			size = 3
		case strings.HasPrefix(rest, "**"):
			// Not a whole segment: an ordinary "*", as in gitignore.
			b.WriteString(`[^/]*`)
			size = 2
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		case c == '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				return "", errors.New("unclosed character class")
			}
			class, err := globClass(glob[i+1 : i+1+end])
			if err != nil {
				return "", err
			}
			b.WriteString(class)
			size = end + 2
		case c == '{':
			braces = append(braces, segment)
			b.WriteString(`(?:`)
			next = segment
		case c == ',' && len(braces) > 0:
			b.WriteString(`|`)
			next = braces[len(braces)-1]
		case c == '}':
			if len(braces) == 0 {
				return "", errors.New("unbalanced brace set")
			}
			braces = braces[:len(braces)-1]
			b.WriteString(`)`)
		default:
			b.WriteString(regexp.QuoteMeta(rest[:size]))
			next = c == '/'
		}
		i += size
		segment = next
	}
	if len(braces) != 0 {
		return "", errors.New("unbalanced brace set")
	}
	return b.String(), nil
}

// globClass translates the inside of a "[...]" class. A class never matches
// the separator, as in gitignore: one that could is an error, as is any form
// the translation does not reproduce exactly (an empty class, an escape or a
// nested "[").
func globClass(inner string) (string, error) {
	if inner == "" || strings.ContainsAny(inner, `\[`) {
		return "", errors.New("unsupported character class")
	}
	class := inner
	negated, ok := strings.CutPrefix(inner, "!")
	if !ok {
		negated, ok = strings.CutPrefix(inner, "^")
	}
	if ok {
		if negated == "" {
			return "", errors.New("empty character class")
		}
		// After "^/" a leading "-" would start a range at "/".
		if r, dash := strings.CutPrefix(negated, "-"); dash {
			negated = `\-` + r
		}
		class = "^/" + negated
	}
	class = "[" + class + "]"
	re, err := regexp.Compile(`^` + class + `$`)
	if err != nil {
		return "", fmt.Errorf("character class: %w", err)
	}
	if re.MatchString("/") {
		return "", errors.New("character class can match /")
	}
	return class, nil
}
