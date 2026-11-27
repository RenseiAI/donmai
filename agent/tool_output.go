package agent

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool-output bound shared by the runner and the in-process tool loop.
//
// A seat's tool can print a very large output (for example a multi-megabyte
// CI log). The runner bounds the copy it retains or forwards (its audit log,
// activity stream and pull request scan) with this helper. A harness whose
// tool loop runs in Go (the harness itself executes the tool and folds the
// result back into the model's context) can bound the copy it hands the
// model with the same helper and marker. A harness whose child CLI runs the
// tool hands the result to the model itself; nothing here bounds that copy.
//
// There is no unbounded setting — the cap is protective, never advisory.
const (
	// DefaultMaxToolOutputBytes bounds one tool result's content (in bytes)
	// when no limit is configured. 256 KiB keeps legitimate outputs (test
	// logs, diffs) intact while bounding pathological ones.
	DefaultMaxToolOutputBytes = 256 * 1024

	// ToolOutputLimitConfigKey is the ProviderConfig key that overrides the
	// model-side tool-output byte bound for one session, for a harness that
	// runs its tool loop in Go. A positive whole number wins over the
	// default.
	ToolOutputLimitConfigKey = "maxToolOutputBytes"

	// ToolOutputTruncatedInfix is the stable infix every truncated tool
	// result carries, so operators and tests can recognise a bounded payload
	// without scraping byte counts.
	ToolOutputTruncatedInfix = "[tool output truncated"
)

// toolOutputMarkerFormat renders the marker TruncateToolOutput places
// between the head and the tail. ParseToolOutputTruncation reads it back.
const toolOutputMarkerFormat = "…" + ToolOutputTruncatedInfix + ": %d-byte output exceeds the %d-byte limit; showing head and tail]…"

// TruncateToolOutput bounds one tool result's content to limit bytes,
// preserving the head (the command echo) and the tail (the result — e.g.
// the pull request URL a verifier scans for) around a marker that names the
// truncation with the original and limit sizes. Content within the limit is
// returned unchanged. The bound is protective, never a failure: the caller
// keeps going on the truncated copy.
//
// The cut is rune-safe on both sides: the head shrinks and the tail's start
// grows forward to rune boundaries, so neither slice splits a multi-byte
// rune (escape-laden output included). A limit smaller than the marker
// itself still names the truncation: the marker alone, cut rune-safe to the
// limit.
func TruncateToolOutput(content string, limit int) (string, bool) {
	if limit <= 0 || len(content) <= limit {
		return content, false
	}
	marker := fmt.Sprintf(toolOutputMarkerFormat, len(content), limit)
	if len(marker) >= limit {
		return truncateOnRuneBoundary(marker, limit), true
	}
	keep := limit - len(marker)
	head := keep / 2
	tail := keep - head
	// Snap to rune boundaries: shrink the head, then grow the tail's start
	// forward, so neither slice splits a multi-byte rune.
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	tailStart := len(content) - tail
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	return content[:head] + marker + content[tailStart:], true
}

// truncateOnRuneBoundary cuts s to at most limit bytes without splitting a
// multi-byte rune.
func truncateOnRuneBoundary(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ParseToolOutputTruncation reports whether content carries the marker
// TruncateToolOutput places in a truncated result, and if so the original
// byte size and the limit it names. A consumer that receives an
// already-bounded copy (for example the runner, from a harness that bounded
// its tool output before emitting it) uses it to keep recording that a
// truncation happened. Content that merely mentions the infix without the
// full numeric marker does not match.
func ParseToolOutputTruncation(content string) (original, limit int, ok bool) {
	idx := strings.Index(content, "…"+ToolOutputTruncatedInfix+": ")
	if idx < 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(content[idx:], toolOutputMarkerFormat, &original, &limit); err != nil {
		return 0, 0, false
	}
	if original <= 0 || limit <= 0 || original <= limit {
		return 0, 0, false
	}
	return original, limit, true
}

// ResolveToolOutputLimit returns the per-tool-result byte bound for one
// session: ProviderConfig[ToolOutputLimitConfigKey] when it is a whole
// number of at least one byte, else DefaultMaxToolOutputBytes. Malformed,
// fractional-below-one or non-positive values fall through to the default,
// never to unbounded.
func ResolveToolOutputLimit(providerConfig map[string]any) int {
	if n, ok := toolOutputLimitFromConfig(providerConfig); ok {
		return n
	}
	return DefaultMaxToolOutputBytes
}

func toolOutputLimitFromConfig(providerConfig map[string]any) (int, bool) {
	if providerConfig == nil {
		return 0, false
	}
	v, ok := providerConfig[ToolOutputLimitConfigKey]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		if n > 0 {
			return n, true
		}
	case int8:
		if n > 0 {
			return int(n), true
		}
	case int16:
		if n > 0 {
			return int(n), true
		}
	case int32:
		if n > 0 {
			return int(n), true
		}
	case int64:
		if n > 0 {
			return int(n), true
		}
	case uint:
		if n > 0 {
			return int(n), true
		}
	case uint32:
		if n > 0 {
			return int(n), true
		}
	case uint64:
		if n > 0 && n <= uint64(^uint(0)>>1) {
			return int(n), true
		}
	case float32:
		if n >= 1 {
			return int(n), true
		}
	case float64:
		if n >= 1 {
			return int(n), true
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}
