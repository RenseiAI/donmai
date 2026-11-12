package agent

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool-output bound shared by the runner and every harness.
//
// A seat's tool can print a very large output (for example a multi-megabyte
// CI log). The harness passes that output to the model; the runner retains
// its own copy. Both sides bound the copy they keep with the SAME limit so
// one oversized result cannot wedge the turn on either leg.
//
// The limit is configurable per session and visible: ProviderConfig
// "maxToolOutputBytes" (positive number) wins, then the
// DONMAI_TOOL_OUTPUT_LIMIT environment value the spawner places in the
// harness child environment, then DefaultMaxToolOutputBytes. There is no
// unbounded setting — the cap is protective, never advisory.
const (
	// DefaultMaxToolOutputBytes bounds one tool result's content (in bytes)
	// when no per-session limit is configured. 256 KiB keeps legitimate
	// outputs (test logs, diffs) intact while bounding pathological ones.
	DefaultMaxToolOutputBytes = 256 * 1024

	// ToolOutputLimitEnvVar carries the per-session tool-output byte bound
	// into the harness child environment at spawn time. The runner resolves
	// the bound and every harness exports it so the child-side enforcement
	// (gateway, wrapper, or harness subprocess) applies the same limit the
	// Go-side truncation uses.
	ToolOutputLimitEnvVar = "DONMAI_TOOL_OUTPUT_LIMIT"

	// ToolOutputLimitConfigKey is the ProviderConfig key that overrides the
	// tool-output byte bound for one session. A positive number wins over
	// the environment value and the default.
	ToolOutputLimitConfigKey = "maxToolOutputBytes"

	// ToolOutputTruncatedInfix is the stable infix every truncated tool
	// result carries, so operators and tests can recognise a bounded payload
	// without scraping byte counts.
	ToolOutputTruncatedInfix = "[tool output truncated"
)

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
	marker := fmt.Sprintf("…%s: %d-byte output exceeds the %d-byte limit; showing head and tail]…",
		ToolOutputTruncatedInfix, len(content), limit)
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

// ResolveToolOutputLimit returns the per-tool-result byte bound for one
// session: ProviderConfig[ToolOutputLimitConfigKey] when positive, else the
// ToolOutputLimitEnvVar entry in env when positive, else
// DefaultMaxToolOutputBytes. Malformed or non-positive values fall through
// to the next source, never to unbounded.
func ResolveToolOutputLimit(providerConfig map[string]any, env map[string]string) int {
	if n, ok := toolOutputLimitFromConfig(providerConfig); ok {
		return n
	}
	if n, ok := toolOutputLimitFromEnv(env); ok {
		return n
	}
	return DefaultMaxToolOutputBytes
}

// ToolOutputLimitEnv renders the spawn-time environment entry that carries
// the resolved bound into the harness child.
func ToolOutputLimitEnv(limit int) string {
	if limit <= 0 {
		limit = DefaultMaxToolOutputBytes
	}
	return ToolOutputLimitEnvVar + "=" + strconv.Itoa(limit)
}

// WithToolOutputLimitEnv returns a copy of env carrying the resolved bound
// under ToolOutputLimitEnvVar. A caller-supplied positive value is
// preserved; otherwise the resolved limit is exported so the child-side
// enforcement applies the same bound the Go-side truncation uses.
func WithToolOutputLimitEnv(env map[string]string, limit int) map[string]string {
	if limit <= 0 {
		limit = DefaultMaxToolOutputBytes
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	if existing, ok := out[ToolOutputLimitEnvVar]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(existing)); err == nil && n > 0 {
			return out
		}
	}
	out[ToolOutputLimitEnvVar] = strconv.Itoa(limit)
	return out
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
		if n > 0 {
			return int(n), true
		}
	case float64:
		if n > 0 {
			return int(n), true
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}

func toolOutputLimitFromEnv(env map[string]string) (int, bool) {
	if env == nil {
		return 0, false
	}
	raw, ok := env[ToolOutputLimitEnvVar]
	if !ok {
		return 0, false
	}
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
		return n, true
	}
	return 0, false
}
