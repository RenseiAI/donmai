package runner

import (
	"testing"
)

// TestParseWorkTypeRole pins the role validation boundary: the four wire
// values validate, everything else (including empty and wrong case)
// reports absent so callers fall back to the name-keyed legacy behaviour.
func TestParseWorkTypeRole(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"inflight", "review", "intake", "terminal"} {
		t.Run("valid/"+role, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseWorkTypeRole(role)
			if !ok || string(got) != role {
				t.Errorf("ParseWorkTypeRole(%q) = (%q, %v), want (%q, true)", role, got, ok, role)
			}
		})
	}
	for _, raw := range []string{"", "not-a-role", "Inflight", "REVIEW", "development", "qa"} {
		t.Run("absent/"+raw, func(t *testing.T) {
			t.Parallel()
			if got, ok := ParseWorkTypeRole(raw); ok || got != "" {
				t.Errorf("ParseWorkTypeRole(%q) = (%q, %v), want (\"\", false)", raw, got, ok)
			}
		})
	}
}
