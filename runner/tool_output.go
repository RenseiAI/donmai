package runner

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// toolOutputTruncatedInfix is the stable infix every truncated tool result
// carries, so operators and tests can recognise a bounded payload without
// scraping byte counts.
const toolOutputTruncatedInfix = "[tool output truncated"

// truncateToolOutput bounds one tool result's content to limit bytes,
// preserving the head (the command echo) and the tail (the result — e.g.
// the pull request URL the verifier scans for) around a marker that names
// the truncation with the original and limit sizes. Content within the
// limit is returned unchanged. The bound is protective, never a failure:
// the caller keeps the seat going on the truncated copy.
//
// The cut is rune-safe on both sides: the head shrinks and the tail's
// start grows forward to rune boundaries, so neither slice splits a
// multi-byte rune (escape-laden output included). A limit smaller than
// the marker itself still names the truncation: the marker alone, cut
// rune-safe to the limit.
func truncateToolOutput(content string, limit int) (string, bool) {
	if limit <= 0 || len(content) <= limit {
		return content, false
	}
	marker := fmt.Sprintf("…%s: %d-byte output exceeds the %d-byte limit; showing head and tail]…",
		toolOutputTruncatedInfix, len(content), limit)
	if len(marker) >= limit {
		return truncateOnRuneBoundary(marker, limit), true
	}
	keep := limit - len(marker)
	head := keep / 2
	tail := keep - head
	// Snap to rune boundaries: shrink the head, then grow the tail's
	// start forward, so neither slice splits a multi-byte rune.
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	tailStart := len(content) - tail
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	return content[:head] + marker + content[tailStart:], true
}

// toolOutputLimit returns the per-tool-result byte bound this Runner
// retains. Hand-built Runners that bypass New resolve to the default,
// so the cap holds on every construction path.
func (r *Runner) toolOutputLimit() int {
	if r == nil || r.maxToolOutputBytes <= 0 {
		return DefaultMaxToolOutputBytes
	}
	return r.maxToolOutputBytes
}

// toolOutputTruncationWarningPrefix prefixes the post-session warning
// recorded when a turn's tool output was truncated. The prefix dedups
// the warning across the several streams one session merges.
const toolOutputTruncationWarningPrefix = "tool output exceeded"

// noteToolOutputTruncation records an oversized tool result the runner
// truncated as a post-session warning — observability, never a failure
// — so the bound copy the seat kept going on stays visible wherever
// warnings surface. Deduped by prefix: applyTo runs once per stream and
// several streams merge onto one envelope.
func noteToolOutputTruncation(res *Result, o streamObservation) {
	if res == nil || !o.toolOutputTruncated {
		return
	}
	for _, w := range res.PostSessionWarnings {
		if strings.HasPrefix(w, toolOutputTruncationWarningPrefix) {
			return
		}
	}
	res.PostSessionWarnings = append(res.PostSessionWarnings,
		fmt.Sprintf("tool output exceeded the %d-byte limit and was truncated with a marker (largest result %d bytes); the seat kept going on the bounded copy", o.toolOutputLimit, o.toolOutputBytes))
}
