// Package diagnostic redacts child-process output before it becomes failure evidence.
package diagnostic

import (
	"regexp"
	"strings"
)

// The existing child-stderr rules are shared by app-server and PTY diagnostics.
// Values end at a closing quote or an unquoted whitespace/query boundary.
const diagnosticValuePattern = `(?:"[^"\r\n]*"?|'[^'\r\n]*'?|[^\s"'&\r\n]+)`

type diagnosticRedaction struct {
	pattern     *regexp.Regexp
	replacement string
}

var diagnosticRedactions = []diagnosticRedaction{
	{
		regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*)[^\r\n]+`),
		"${1}[REDACTED]",
	},
	{
		regexp.MustCompile(`(?i)(bearer[\s:=]+)\S+`),
		"${1}[REDACTED]",
	},
	{
		regexp.MustCompile(`(?i)((?:api[_-]?keys?|access[_-]?tokens?|refresh[_-]?tokens?|auth[_-]?tokens?|client[_-]?secrets?|private[_-]?keys?|secrets?|tokens?|passwords?|passwds?|credentials?|cookies?|sessions?)["']?\s*[:=]\s*)` + diagnosticValuePattern),
		"${1}[REDACTED]",
	},
	{
		regexp.MustCompile(`(://[^/\s:@'"]*:)[^/\s@'"]+(@)`),
		"${1}[REDACTED]${2}",
	},
	{
		regexp.MustCompile(`(://)[^/\s:@'"]+(@)`),
		"${1}[REDACTED]${2}",
	},
	{
		regexp.MustCompile(`\b(?:sk|pk|gh[oprsu]|xox[baprs])[-_][A-Za-z0-9](?:[A-Za-z0-9-]{8,})[A-Za-z0-9]\b`),
		"[REDACTED]",
	},
	{
		regexp.MustCompile(`\bdmk_[0-9a-f]{16,}\b`),
		"[REDACTED]",
	},
}

// Redact masks secret-shaped values before diagnostics are truncated or logged.
func Redact(s string) string {
	for _, r := range diagnosticRedactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// Unredacted reports whether s still carries a value one of Redact's rules
// would mask: a rule match whose value is not already the [REDACTED] marker
// (optionally quoted), or any match of a rule that masks its whole match.
// Unlike comparing Redact(s) with s it is exact on already-redacted text,
// where a rule's value pattern may re-match the marker plus trailing
// punctuation without exposing anything.
func Unredacted(s string) bool {
	return len(UnredactedValues(s)) > 0
}

// UnredactedValues returns the values in s that one of Redact's rules would
// mask and that are not already the [REDACTED] marker: the value part of a
// key-prefixed rule (surrounding quotes trimmed) or the whole match of a rule
// that masks its whole match.
func UnredactedValues(s string) []string {
	var out []string
	for _, r := range diagnosticRedactions {
		keepsPrefix := strings.HasPrefix(r.replacement, "${1}")
		for _, m := range r.pattern.FindAllStringSubmatchIndex(s, -1) {
			if !keepsPrefix || len(m) < 4 || m[3] < 0 {
				out = append(out, s[m[0]:m[1]])
				continue
			}
			end := m[1]
			if len(m) >= 6 && m[4] >= 0 {
				end = m[4]
			}
			value := strings.Trim(s[m[3]:end], "\"'")
			if !strings.HasPrefix(value, redactedMarker) {
				out = append(out, value)
			}
		}
	}
	return out
}

const redactedMarker = "[REDACTED]"
