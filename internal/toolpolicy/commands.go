// Package toolpolicy matches tool-permission patterns in the Claude grammar
// against shell commands and file paths, for harness adapters that enforce a
// session's allow and deny lists themselves.
//
// Denies and allows read some forms differently, so that no reading can widen
// an allow:
//   - a deny matches the whole command line or any command it chains or wraps
//     (cd x && git push, env FOO=1 git push, bash -c 'git push');
//   - an allow matches only when the whole line and every chained command
//     match, keeping each command's wrappers;
//   - "*" inside a Bash pattern is a wildcard in a deny and literal in an allow;
//   - a bare "Bash" deny matches every command, while a bare "Bash" allow and a
//     raw regex keep their legacy raw-regex meaning;
//   - a tool-name allow ("Read", "WebFetch", "mcp__x__y") authorizes no command.
package toolpolicy

import (
	"regexp"
	"strings"
)

// Side says which list a pattern came from.
type Side int

// Side values.
const (
	Deny Side = iota
	Allow
)

// Commands is one side's compiled shell-command rules.
type Commands struct {
	side  Side
	rules []*regexp.Regexp
}

// CompileCommands compiles the command rules of one side. File-tool names and
// path-scoped Read/Edit/Write patterns are not command text and are skipped
// (see CompilePaths); a malformed raw regex is dropped.
func CompileCommands(patterns []string, side Side) Commands {
	c := Commands{side: side}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		name, arg, scoped := ParseToolPattern(p)
		switch {
		case IsFileChangeTool(p):
		case scoped && (name == "Read" || IsFileChangeTool(name)):
		case name == "Bash" && (scoped || side == Deny):
			c.rules = append(c.rules, regexp.MustCompile(bashCommandPattern(arg, scoped, side)))
		case side == Allow && p != "Bash" && toolNamePattern.MatchString(p):
		default:
			if re, err := regexp.Compile(p); err == nil {
				c.rules = append(c.rules, re)
			}
		}
	}
	return c
}

// Len reports how many command rules compiled.
func (c Commands) Len() int { return len(c.rules) }

// Match reports whether cmd matches this side's rules.
func (c Commands) Match(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if c.side == Allow {
		if !matchesAny(c.rules, cmd) {
			return false
		}
		for _, chained := range chainedCommands(cmd) {
			if !matchesAny(c.rules, chained) {
				return false
			}
		}
		return true
	}
	for _, candidate := range append([]string{cmd}, commandSegments(cmd)...) {
		if matchesAny(c.rules, candidate) {
			return true
		}
	}
	return false
}

// ParseToolPattern splits "Name(arg)" into its parts; a bare name is unscoped.
func ParseToolPattern(pattern string) (name, arg string, scoped bool) {
	open := strings.IndexByte(pattern, '(')
	if open <= 0 || !strings.HasSuffix(pattern, ")") {
		return pattern, "", false
	}
	return pattern[:open], strings.TrimSpace(pattern[open+1 : len(pattern)-1]), true
}

// IsFileChangeTool reports a file-change tool name.
func IsFileChangeTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// toolNamePattern matches a harness tool name, bare or scoped ("Read",
// "WebFetch(domain:x)", "mcp__server__tool"), as opposed to a raw regex.
var toolNamePattern = regexp.MustCompile(`^(?:[A-Z][A-Za-z0-9]*|mcp__[A-Za-z0-9_*-]+)(?:\(.*\))?$`)

// bashCommandPattern renders a Bash pattern: bare (a deny), empty or "*" match
// everything; "prefix:*" and "prefix *" stop at a word boundary; "prefix*" is
// a string prefix; anything else is exact.
func bashCommandPattern(inner string, scoped bool, side Side) string {
	if !scoped || inner == "" || inner == "*" {
		return `(?s)^.*$`
	}
	body, tail := inner, `$`
	if prefix, ok := strings.CutSuffix(inner, ":*"); ok {
		body, tail = strings.TrimSpace(prefix), `(?:\s|$)`
	} else if prefix, ok := strings.CutSuffix(inner, "*"); ok {
		body, tail = prefix, ""
		if trimmed := strings.TrimSpace(prefix); trimmed != prefix {
			body, tail = trimmed, `(?:\s|$)`
		}
	}
	quoted := regexp.QuoteMeta(body)
	if side == Deny {
		quoted = strings.ReplaceAll(quoted, `\*`, `.*`)
	}
	return `^` + quoted + tail
}

// commandWrappers match leading words that run the rest of the line as a
// command: VAR=value, env, command, exec, nohup, time, nice, timeout, xargs,
// eval, and a shell's -c flag, each optionally by absolute path and with flags.
var commandWrappers = regexp.MustCompile(`^(?:(?:[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|\S*)|(?:\S*/)?(?:env|command|exec|nohup|time|nice|xargs|eval)(?:\s+-\S+(?:\s+\d+)?)*|(?:\S*/)?timeout(?:\s+-\S+)*\s+\S+|(?:\S*/)?(?:ba|z|da|k)?sh(?:\s+-\S+)*\s+-[A-Za-z]*c)\s+)+`)

// chainedCommands splits a shell line into the commands it runs, untouched.
// It splits at unquoted &&, ||, ;, |, &, newlines and parentheses, and at
// $( and backticks even inside double quotes, where a substitution still
// runs. Quoted text is never split, so 'one;two' stays one argument.
func chainedCommands(cmd string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if part := strings.TrimSpace(cur.String()); part != "" {
			out = append(out, part)
		}
		cur.Reset()
	}
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == '\'':
			cur.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(cmd):
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
		case c == '`' || (c == '$' && i+1 < len(cmd) && cmd[i+1] == '('):
			flush()
			if c == '$' {
				i++
			}
		case quote == '"':
			cur.WriteByte(c)
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case strings.IndexByte(";&|()\n", c) >= 0:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// commandSegments returns, for deny matching only, each command a shell line
// chains or wraps: wrappers stripped, a quoted -c/eval program parsed as its
// own shell line, and each command also with runs of whitespace collapsed.
func commandSegments(cmd string) []string {
	return unwrapCommands(cmd, 0)
}

func unwrapCommands(line string, depth int) []string {
	var out []string
	for _, part := range chainedCommands(line) {
		for next := strings.TrimSpace(commandWrappers.ReplaceAllString(part, "")); next != part; next = strings.TrimSpace(commandWrappers.ReplaceAllString(part, "")) {
			part = next
		}
		out = append(out, part)
		if collapsed := strings.Join(strings.Fields(part), " "); collapsed != part {
			out = append(out, collapsed)
		}
		if program, ok := leadingQuotedWord(part); ok && depth < 4 {
			out = append(out, unwrapCommands(program, depth+1)...)
		}
	}
	return out
}

// leadingQuotedWord returns the content of a leading '...' or "..." word, the
// form a shell -c or eval program takes once its wrapper is stripped.
func leadingQuotedWord(s string) (string, bool) {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') {
		return "", false
	}
	if s[0] == '\'' {
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", false
		}
		return s[1 : 1+end], true
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0:
			b.WriteByte(s[i+1])
			i++
		case s[i] == '"':
			return b.String(), true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", false
}
