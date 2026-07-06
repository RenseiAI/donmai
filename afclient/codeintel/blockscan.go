package codeintel

import "strings"

// Block-extent scanning for the regex extractors (Go + TS/JS).
//
// The extractors need one structural fact regex cannot give them: the line of
// the '}' that closes a declaration's body. A naive per-character brace count
// is fooled by braces inside string literals, rune/char literals,
// template/raw strings, and //-or-/**/ comments — truncating the extent and
// making the persisted symbol dedup fingerprint hash the wrong span (or drop
// it entirely below symbolHashMinLines). scanBlockExtent is a small state
// machine that skips those regions before counting braces.
//
// Dialect differences handled per language:
//   - Go: "..." and '...' honor backslash escapes and cannot span lines;
//     `...` raw strings span lines and have NO escapes (a trailing backslash
//     is literal).
//   - TS/JS: "..." and '...' honor escapes and are treated as ending at EOL
//     (line-spanning via trailing backslash is ignored — conservative);
//     `...` template literals span lines, honor \` escapes, and their whole
//     body — including ${...} interpolations — is skipped conservatively (a
//     backtick nested inside an interpolation would end the skip early; the
//     failure mode is a missing extent, never a wrong one... the extent is
//     simply not recorded when the brace count never closes).

// scanLang selects the string-literal dialect for scanBlockExtent.
type scanLang int

const (
	scanGo scanLang = iota
	scanTS
)

// maxBraceLookahead bounds how many lines past the declaration line the
// scanner searches for the body's opening '{' (wrapped multi-line signatures).
// Beyond this the declaration is treated as bodyless.
const maxBraceLookahead = 10

// blockScanOpts tunes scanBlockExtent per declaration form.
type blockScanOpts struct {
	// stopAtSemi aborts (no extent) when a code-context ';' occurs at paren
	// depth 0 before the opening brace — TS bodyless overload signatures and
	// declare stubs terminate with ';'.
	stopAtSemi bool
	// stopAtTopLevelDecl aborts when a line after startLine begins a new
	// top-level Go declaration before the opening brace was seen, so a
	// bodyless decl (assembly stub / external linkname) never borrows the
	// next declaration's brace.
	stopAtTopLevelDecl bool
	// noLookaheadCap disables the maxBraceLookahead bound (legacy TS
	// class/interface behavior: the first code '{' on or after the decl line
	// opens the body, however far away).
	noLookaheadCap bool
	// startCol is the byte offset on startLine at which scanning begins
	// (used to skip past an arrow function's '=>' so a destructured
	// parameter's '{' is never mistaken for the body).
	startCol int
}

// scanBlockExtent scans lines from startLine looking for a declaration body:
// the first code-context '{' at paren depth 0 (a '{' inside parentheses is a
// destructured parameter or inline argument, not the body). It then counts
// code-context braces until the body closes and returns the 0-based line
// index of the closing '}'. ok=false when no body opens (bodyless decl per
// the opts) or the count never returns to zero (unterminated / pathological
// source) — the caller records no extent, which downstream skips, rather than
// a wrong one.
func scanBlockExtent(lines []string, startLine int, lang scanLang, o blockScanOpts) (int, bool) {
	const (
		stCode = iota
		stBlockComment
		stBacktick
	)
	st := stCode
	braceDepth := 0
	parenDepth := 0
	bodyFound := false

	for i := startLine; i < len(lines); i++ {
		if !bodyFound {
			if !o.noLookaheadCap && i-startLine > maxBraceLookahead {
				return 0, false
			}
			if o.stopAtTopLevelDecl && i > startLine && st == stCode && startsTopLevelGoDecl(lines[i]) {
				return 0, false
			}
		}
		line := lines[i]
		j := 0
		if i == startLine {
			j = o.startCol
		}
		for j < len(line) {
			ch := line[j]
			switch st {
			case stBlockComment:
				if ch == '*' && j+1 < len(line) && line[j+1] == '/' {
					st = stCode
					j += 2
					continue
				}
				j++
			case stBacktick:
				if lang == scanTS && ch == '\\' {
					j += 2 // template-literal escape (\` etc.); Go raw strings have none
					continue
				}
				if ch == '`' {
					st = stCode
				}
				j++
			default: // stCode
				switch ch {
				case '/':
					if j+1 < len(line) {
						if line[j+1] == '/' {
							j = len(line) // line comment: rest of line is not code
							continue
						}
						if line[j+1] == '*' {
							st = stBlockComment
							j += 2
							continue
						}
					}
					j++
				case '"', '\'':
					j = skipQuoted(line, j)
				case '`':
					st = stBacktick
					j++
				case '(':
					parenDepth++
					j++
				case ')':
					if parenDepth > 0 {
						parenDepth--
					}
					j++
				case ';':
					if !bodyFound && o.stopAtSemi && parenDepth == 0 {
						return 0, false // bodyless: overload signature / declare stub
					}
					j++
				case '{':
					if bodyFound {
						braceDepth++
					} else if parenDepth == 0 {
						bodyFound = true
						braceDepth = 1
					}
					j++
				case '}':
					if bodyFound {
						braceDepth--
						if braceDepth == 0 {
							return i, true
						}
					} else if parenDepth == 0 {
						// The enclosing block closed before any body opened
						// (e.g. a call-like line at the end of a class body):
						// there is no body.
						return 0, false
					}
					j++
				default:
					j++
				}
			}
		}
	}
	return 0, false
}

// skipQuoted returns the index just past the closing quote of the
// string/rune/char literal opening at line[start] (a double or single
// quote), honoring backslash escapes. An unterminated literal consumes the
// rest of the line
// (Go interpreted/rune literals and TS non-template strings cannot span
// lines; treating EOL as the terminator is the conservative reading).
func skipQuoted(line string, start int) int {
	q := line[start]
	j := start + 1
	for j < len(line) {
		switch line[j] {
		case '\\':
			j += 2
		case q:
			return j + 1
		default:
			j++
		}
	}
	return j
}

// startsTopLevelGoDecl reports whether the raw (unindented) line begins a new
// top-level Go declaration — the wrapped-signature lookahead stops there so a
// bodyless declaration never borrows the next declaration's brace.
func startsTopLevelGoDecl(line string) bool {
	for _, kw := range [...]string{"func ", "type ", "var ", "const ", "package "} {
		if strings.HasPrefix(line, kw) {
			return true
		}
	}
	return false
}
