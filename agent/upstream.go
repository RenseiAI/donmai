package agent

import (
	"strconv"
	"strings"
)

// Bounds for the structured upstream error a harness captures from a model
// endpoint's refusal or throttle. Only small, fixed-shape fields travel —
// never request bodies, headers, or credentials.
const (
	// MaxUpstreamProviderCodeLen bounds the provider's error code
	// (for example "overloaded_error").
	MaxUpstreamProviderCodeLen = 128
	// MaxUpstreamProviderMessageLen bounds the provider's error text.
	MaxUpstreamProviderMessageLen = 512
	// MaxUpstreamResetAtLen bounds the reset-time representation.
	MaxUpstreamResetAtLen = 64
)

// UpstreamError is the structured cause of a model endpoint refusal or
// throttle (for example HTTP 429 with a usage-limit code and a reset time),
// so the control plane can tell "quota exhausted" from "key rejected" from
// "server error". It rides the terminal status as an optional, omitempty
// field; it is absent on every run that did not end on an endpoint error.
type UpstreamError struct {
	// HTTPStatus is the endpoint's HTTP status (for example 429).
	HTTPStatus int `json:"httpStatus,omitempty"`
	// ProviderCode is the endpoint's error code (for example
	// "overloaded_error").
	ProviderCode string `json:"providerCode,omitempty"`
	// ProviderMessage is the endpoint's error text, truncated to
	// MaxUpstreamProviderMessageLen.
	ProviderMessage string `json:"providerMessage,omitempty"`
	// ResetAt is the endpoint's reset time as reported: a decimal unix
	// timestamp or an opaque timestamp string, truncated to
	// MaxUpstreamResetAtLen. Empty when the endpoint named none.
	ResetAt string `json:"resetAt,omitempty"`
}

// Empty reports whether the value carries no structured signal at all.
func (u *UpstreamError) Empty() bool {
	return u == nil || (u.HTTPStatus == 0 && u.ProviderCode == "" && u.ProviderMessage == "" && u.ResetAt == "")
}

// CanonicalUpstreamError returns nil for a nil or fully empty value so
// callers that construct the struct by hand still serialize the field as
// absent; otherwise it returns the normalized form.
func CanonicalUpstreamError(u *UpstreamError) *UpstreamError {
	if u.Empty() {
		return nil
	}
	return NormalizeUpstreamError(u.HTTPStatus, u.ProviderCode, u.ProviderMessage, u.ResetAt)
}

// NormalizeUpstreamError bounds and normalizes the parts. It returns nil
// when there is no structured signal — a zero status with no code and no
// reset time is not an upstream error even when a message is present, since
// that text already rides the event's own Message field.
func NormalizeUpstreamError(status int, code, message, resetAt string) *UpstreamError {
	if status < 100 || status > 599 {
		status = 0
	}
	code = truncateUpstream(strings.TrimSpace(code), MaxUpstreamProviderCodeLen)
	message = truncateUpstream(strings.TrimSpace(message), MaxUpstreamProviderMessageLen)
	resetAt = truncateUpstream(strings.TrimSpace(resetAt), MaxUpstreamResetAtLen)
	if status == 0 && code == "" && resetAt == "" {
		return nil
	}
	return &UpstreamError{
		HTTPStatus:      status,
		ProviderCode:    code,
		ProviderMessage: message,
		ResetAt:         resetAt,
	}
}

func truncateUpstream(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

// Tolerant key lists for reading an endpoint error out of a decoded event
// envelope. Shapes differ per harness, so every list accepts the common
// spellings; only these allowlisted keys are ever read — request bodies,
// headers, and credentials are never consulted.
var (
	upstreamStatusKeys = []string{"status", "statusCode", "status_code", "httpStatus", "http_status"}
	// NOTE: the bare "type" key is deliberately absent — stream-json
	// envelopes carry one ("result", "assistant", ...) and reading it as
	// a code would turn every event into a false-positive upstream error.
	upstreamCodeKeys  = []string{"code", "errorCode", "error_code", "providerCode", "provider_code"}
	upstreamResetKeys = []string{
		"resetsAt", "resets_at", "resetAt", "reset_at", "reset",
		"resetTime", "reset_time", "retryAfter", "retry_after",
		"retryAfterSeconds", "retry_after_seconds",
	}
	upstreamMessageKeys = []string{"errorMessage", "message"}
)

// ParseUpstreamError reads the structured endpoint error out of a decoded
// envelope map: the map itself, its "error" and "details" submaps, and any
// "diagnostics" entries (with their own "details"/"error" submaps). It
// returns nil when the envelope carries no status, code, or reset time.
func ParseUpstreamError(m map[string]any) *UpstreamError {
	if len(m) == 0 {
		return nil
	}
	maps := upstreamSearchMaps(m)
	var status int
	for _, cand := range maps {
		if s, ok := upstreamNumber(cand, upstreamStatusKeys...); ok && s >= 100 && s <= 599 {
			status = s
			break
		}
	}
	var code string
	for _, cand := range maps {
		if c := upstreamString(cand, upstreamCodeKeys...); c != "" {
			code = c
			break
		}
	}
	var reset string
	for _, cand := range maps {
		if r := upstreamResetValue(cand); r != "" {
			reset = r
			break
		}
	}
	var message string
	for _, cand := range maps {
		if msg := upstreamString(cand, upstreamMessageKeys...); msg != "" {
			message = msg
			break
		}
	}
	return NormalizeUpstreamError(status, code, message, reset)
}

// upstreamSearchMaps orders the maps a structured error is read from: the
// envelope first, then its "error"/"details" submaps, then each diagnostics
// entry with its own submaps.
func upstreamSearchMaps(m map[string]any) []map[string]any {
	out := []map[string]any{m}
	for _, key := range []string{"error", "details"} {
		if sub, ok := m[key].(map[string]any); ok && len(sub) > 0 {
			out = append(out, sub)
		}
	}
	if raw, ok := m["diagnostics"].([]any); ok {
		for _, item := range raw {
			d, ok := item.(map[string]any)
			if !ok || len(d) == 0 {
				continue
			}
			out = append(out, d)
			for _, key := range []string{"error", "details"} {
				if sub, ok := d[key].(map[string]any); ok && len(sub) > 0 {
					out = append(out, sub)
				}
			}
		}
	}
	return out
}

func upstreamString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func upstreamNumber(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case float32:
			return int(v), true
		case int:
			return v, true
		case int64:
			return int(v), true
		case int32:
			return int(v), true
		}
	}
	return 0, false
}

// upstreamResetValue reads a reset time as its opaque representation: a
// string verbatim, or a number rendered as decimal digits (a unix
// timestamp). Anything else is not a reset time.
func upstreamResetValue(m map[string]any) string {
	for _, k := range upstreamResetKeys {
		switch v := m[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case float32:
			return strconv.FormatFloat(float64(v), 'f', -1, 32)
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case int32:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}
