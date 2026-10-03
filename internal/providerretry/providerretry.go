// Package providerretry is the one retry policy for a model provider error a
// harness reports at the end of a turn: which failures a new attempt cannot
// fix. The harness mapper (provider/harness/pi) applies it to the structured
// fields of a failed message; the runner applies it to the error text when the
// harness gave no structured verdict (runner/turn_continuation.go).
//
// The policy errs toward retrying. Before it existed every provider error was
// retried within a bound, so a failure is treated as deterministic only on a
// positive signal: an explicit not-retryable flag, or an HTTP status read from
// a structured field or from a strictly anchored text form. A number in prose
// ("timeout after 400 seconds") or a port ("10.0.0.12:443") is never a status.
package providerretry

import (
	"regexp"
	"strconv"
)

// StatusRetryable reports whether a request that failed with the HTTP status
// may succeed on a later attempt, mirroring the provider SDK retry policy:
// 408, 409, 429 and any 5xx retry; any other 4xx is deterministic and does
// not. A status outside 4xx and 5xx says nothing about the failure and is
// treated as retryable.
func StatusRetryable(status int) bool {
	switch {
	case status == 408, status == 409, status == 429:
		return true
	case status >= 400 && status <= 499:
		return false
	default:
		return true
	}
}

// contextOverflowPattern matches the context-length failures models report:
// "prompt is too long", "input is too long", "context_length_exceeded",
// "maximum context length", "exceeds the context window", "too many tokens".
var contextOverflowPattern = regexp.MustCompile(`(?i)prompt is too long|input is too long|context[_ ]length|context[_ ]window|maximum context|too many tokens`)

// ContextOverflow reports whether text says the request did not fit the
// model's context window. Such a failure is a 400 to the provider SDK, but it
// stays retryable for the runner: a harness that compacts its context on the
// next user message (pi re-arms its compaction on each one) gets another
// attempt from the runner's retry prompt with a smaller context.
func ContextOverflow(text string) bool {
	return contextOverflowPattern.MatchString(text)
}

// The anchored text forms a status is read from, and nothing else.
var (
	// leadingStatusPattern: the text starts with the status, followed by
	// whitespace, the end, a ": " separator or the start of a JSON body
	// ("400 Bad Request", "429: rate limited", "400 {...}"). An address
	// such as "104.18.32.47:443" cannot match: the status must not be
	// followed by "." or by ":" and a digit.
	leadingStatusPattern = regexp.MustCompile(`^\s*([1-5]\d\d)(?:$|\s|:(?:\s|$)|[{(\[])`)
	// explicitStatusPattern: a status named as one — "status 400",
	// "status=400", "status: 400", "statusCode: 400", "status code 400",
	// "HTTP 400", "HTTP/1.1 400".
	explicitStatusPattern = regexp.MustCompile(`(?i)\b(?:status(?:[ _]?code)?\s*[=:]?\s*|HTTP(?:/\d(?:\.\d)?)?\s+)([1-5]\d\d)\b`)
)

// TextStatus reads an HTTP status from a provider's error text, but only from
// a strictly anchored form: a leading status, or one named explicitly as a
// status or an HTTP status line. A port, an address or a number in prose is
// never read as a status.
func TextStatus(text string) (int, bool) {
	for _, pattern := range []*regexp.Regexp{leadingStatusPattern, explicitStatusPattern} {
		if m := pattern.FindStringSubmatch(text); m != nil {
			status, err := strconv.Atoi(m[1])
			if err == nil {
				return status, true
			}
		}
	}
	return 0, false
}

// TextRetryable reports whether a provider error, known only by its text, may
// succeed on a later attempt: a context overflow retries, a status read from
// an anchored form follows StatusRetryable, and anything else retries.
func TextRetryable(text string) bool {
	if ContextOverflow(text) {
		return true
	}
	if status, ok := TextStatus(text); ok {
		return StatusRetryable(status)
	}
	return true
}
