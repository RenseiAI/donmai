// Package diagnostic redacts child-process output before it becomes failure evidence.
package diagnostic

import "regexp"

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
