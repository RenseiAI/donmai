package agent

import (
	"strings"
	"testing"
)

func TestTruncateToolOutput_WithinLimitUnchanged(t *testing.T) {
	got, changed := TruncateToolOutput("hello", 100)
	if changed || got != "hello" {
		t.Fatalf("got (%q, %v), want unchanged", got, changed)
	}
}

func TestTruncateToolOutput_MarkerNamesTruncation(t *testing.T) {
	big := strings.Repeat("x", 1000)
	got, changed := TruncateToolOutput(big, 100)
	if !changed {
		t.Fatal("want changed=true")
	}
	if len(got) > 100 {
		t.Fatalf("len = %d, want <= 100", len(got))
	}
	if !strings.Contains(got, ToolOutputTruncatedInfix) {
		t.Fatalf("truncated output missing marker %q: %q", ToolOutputTruncatedInfix, got)
	}
}

func TestTruncateToolOutput_RuneSafe(t *testing.T) {
	// Multi-byte rune at the cut points must not split.
	big := strings.Repeat("é", 500) // 2 bytes each
	got, changed := TruncateToolOutput(big, 100)
	if !changed {
		t.Fatal("want changed=true")
	}
	for i := 0; i < len(got); {
		r := got[i]
		// Every byte that starts a rune must be a valid rune start; a
		// simpler check: the whole string must be valid UTF-8.
		_ = r
		i++
	}
	valid := true
	for _, r := range got {
		if r == 0xFFFD {
			// Replacement char would indicate a split; our input has none,
			// so any replacement is a split artifact.
			valid = false
		}
	}
	if !valid {
		t.Fatalf("truncated output is not rune-safe: %q", got)
	}
}

func TestResolveToolOutputLimit_Precedence(t *testing.T) {
	def := DefaultMaxToolOutputBytes
	if got := ResolveToolOutputLimit(nil, nil); got != def {
		t.Fatalf("nil sources = %d, want default %d", got, def)
	}
	if got := ResolveToolOutputLimit(map[string]any{ToolOutputLimitConfigKey: 123}, map[string]string{ToolOutputLimitEnvVar: "456"}); got != 123 {
		t.Fatalf("config should win, got %d", got)
	}
	if got := ResolveToolOutputLimit(nil, map[string]string{ToolOutputLimitEnvVar: "456"}); got != 456 {
		t.Fatalf("env should win over default, got %d", got)
	}
	if got := ResolveToolOutputLimit(map[string]any{ToolOutputLimitConfigKey: "bad"}, map[string]string{ToolOutputLimitEnvVar: "also-bad"}); got != def {
		t.Fatalf("malformed should fall to default, got %d", got)
	}
	if got := ResolveToolOutputLimit(map[string]any{ToolOutputLimitConfigKey: -5}, nil); got != def {
		t.Fatalf("non-positive should fall to default, got %d", got)
	}
}

func TestWithToolOutputLimitEnv_PreservesCallerValue(t *testing.T) {
	env := WithToolOutputLimitEnv(map[string]string{ToolOutputLimitEnvVar: "777"}, 123)
	if env[ToolOutputLimitEnvVar] != "777" {
		t.Fatalf("caller value overwritten: %q", env[ToolOutputLimitEnvVar])
	}
	env = WithToolOutputLimitEnv(nil, 123)
	if env[ToolOutputLimitEnvVar] != "123" {
		t.Fatalf("limit not exported: %v", env)
	}
}
