package agent_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestNormalizeUpstreamError(t *testing.T) {
	t.Parallel()

	u := agent.NormalizeUpstreamError(429, "usage_limit", "Rate limited", "1777673400")
	if u == nil {
		t.Fatal("NormalizeUpstreamError returned nil for a structured error")
	}
	if u.HTTPStatus != 429 || u.ProviderCode != "usage_limit" || u.ProviderMessage != "Rate limited" || u.ResetAt != "1777673400" {
		t.Errorf("NormalizeUpstreamError = %+v", u)
	}

	// A bare message with no status, code, or reset time is not an
	// upstream error — the text already rides the event's Message.
	if got := agent.NormalizeUpstreamError(0, "", "just text", ""); got != nil {
		t.Errorf("NormalizeUpstreamError(message only) = %+v, want nil", got)
	}

	// Out-of-range statuses are dropped, not carried.
	if got := agent.NormalizeUpstreamError(99, "", "x", ""); got != nil {
		t.Errorf("NormalizeUpstreamError(status 99) = %+v, want nil", got)
	}

	// Bounds hold: the code, message, and reset time are truncated.
	long := strings.Repeat("c", agent.MaxUpstreamProviderCodeLen+10)
	msg := strings.Repeat("m", agent.MaxUpstreamProviderMessageLen+10)
	reset := strings.Repeat("r", agent.MaxUpstreamResetAtLen+10)
	bounded := agent.NormalizeUpstreamError(503, long, msg, reset)
	if bounded == nil {
		t.Fatal("NormalizeUpstreamError returned nil for a bounded error")
	}
	if len([]rune(bounded.ProviderCode)) != agent.MaxUpstreamProviderCodeLen {
		t.Errorf("ProviderCode runes = %d, want %d", len([]rune(bounded.ProviderCode)), agent.MaxUpstreamProviderCodeLen)
	}
	if len([]rune(bounded.ProviderMessage)) != agent.MaxUpstreamProviderMessageLen {
		t.Errorf("ProviderMessage runes = %d, want %d", len([]rune(bounded.ProviderMessage)), agent.MaxUpstreamProviderMessageLen)
	}
	if len([]rune(bounded.ResetAt)) != agent.MaxUpstreamResetAtLen {
		t.Errorf("ResetAt runes = %d, want %d", len([]rune(bounded.ResetAt)), agent.MaxUpstreamResetAtLen)
	}

	// Nil and empty canonicalize to absent so the wire field stays omitted.
	if agent.CanonicalUpstreamError(nil) != nil {
		t.Errorf("CanonicalUpstreamError(nil) != nil")
	}
	if agent.CanonicalUpstreamError(&agent.UpstreamError{}) != nil {
		t.Errorf("CanonicalUpstreamError(empty) != nil")
	}
	if got := agent.CanonicalUpstreamError(&agent.UpstreamError{HTTPStatus: 429}); got == nil || got.HTTPStatus != 429 {
		t.Errorf("CanonicalUpstreamError(429) = %+v, want status 429", got)
	}
}

func TestParseUpstreamError(t *testing.T) {
	t.Parallel()

	got := agent.ParseUpstreamError(map[string]any{
		"errorMessage": "Rate limited",
		"error":        map[string]any{"code": "usage_limit", "status": float64(429), "resetsAt": float64(1777673400)},
	})
	if got == nil {
		t.Fatal("ParseUpstreamError returned nil")
	}
	if got.HTTPStatus != 429 || got.ProviderCode != "usage_limit" || got.ProviderMessage != "Rate limited" || got.ResetAt != "1777673400" {
		t.Errorf("ParseUpstreamError = %+v", got)
	}

	// Only allowlisted keys are read: request bodies, headers, and
	// credentials never become an upstream error.
	if got := agent.ParseUpstreamError(map[string]any{
		"errorMessage": "Rate limited",
		"headers":      map[string]any{"authorization": "Bearer x"},
		"body":         "secret",
	}); got != nil {
		t.Errorf("ParseUpstreamError(allowlisted miss) = %+v, want nil", got)
	}

	// An envelope type is not a provider code: a result line without a
	// structured error stays nil.
	if got := agent.ParseUpstreamError(map[string]any{"type": "result", "subtype": "error_max_turns"}); got != nil {
		t.Errorf("ParseUpstreamError(type only) = %+v, want nil", got)
	}

	if agent.ParseUpstreamError(nil) != nil {
		t.Errorf("ParseUpstreamError(nil) != nil")
	}
}
