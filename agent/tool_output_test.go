package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestTruncateToolOutput_ExactlyAtLimitUnchanged(t *testing.T) {
	const limit = 200
	exact := strings.Repeat("x", limit)
	if got, changed := TruncateToolOutput(exact, limit); changed || got != exact {
		t.Fatalf("len == limit: got (len %d, %v), want unchanged", len(got), changed)
	}
	over := exact + "y"
	got, changed := TruncateToolOutput(over, limit)
	if !changed || len(got) > limit {
		t.Fatalf("len == limit+1: got (len %d, %v), want truncated to <= %d", len(got), changed, limit)
	}
}

// TestTruncateToolOutput_RuneSafe pins both rune snaps: the head shrinks to a
// rune start and the tail's start grows forward to one. Each case puts a
// multi-byte rune across exactly one cut so a deleted snap loop splits it.
func TestTruncateToolOutput_RuneSafe(t *testing.T) {
	const limit = 200
	// Probe the marker length for this input size so the cut points are
	// known: head = keep/2, tail = keep - head.
	probe, _ := TruncateToolOutput(strings.Repeat("a", 1000), limit)
	marker := probe[strings.Index(probe, "…") : strings.LastIndex(probe, "…")+len("…")]
	keep := limit - len(marker)
	head, tail := keep/2, keep-keep/2
	tests := []struct {
		name    string
		content string
	}{
		{
			// A 3-byte rune straddles the head cut at byte head.
			name:    "head cut",
			content: strings.Repeat("a", head-1) + "世" + strings.Repeat("a", 1000-head-2),
		},
		{
			// A 3-byte rune straddles the tail cut at len-tail.
			name:    "tail cut",
			content: strings.Repeat("a", 1000-tail-1) + "世" + strings.Repeat("a", tail-2),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.content) != 1000 {
				t.Fatalf("fixture len = %d, want 1000 (marker sizes depend on it)", len(tc.content))
			}
			got, changed := TruncateToolOutput(tc.content, limit)
			if !changed {
				t.Fatal("want changed=true")
			}
			if len(got) > limit {
				t.Fatalf("len = %d, want <= %d", len(got), limit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncated output splits a rune: %q", got)
			}
		})
	}
}

func TestParseToolOutputTruncation(t *testing.T) {
	got, changed := TruncateToolOutput(strings.Repeat("z", 5000), 300)
	if !changed {
		t.Fatal("want changed=true")
	}
	original, limit, ok := ParseToolOutputTruncation(got)
	if !ok || original != 5000 || limit != 300 {
		t.Fatalf("parse = (%d, %d, %v), want (5000, 300, true)", original, limit, ok)
	}
	for _, content := range []string{
		"",
		"plain output",
		"source mentions " + ToolOutputTruncatedInfix + " without numbers",
		"…" + ToolOutputTruncatedInfix + ": %d-byte output exceeds the %d-byte limit; showing head and tail]…",
	} {
		if _, _, ok := ParseToolOutputTruncation(content); ok {
			t.Fatalf("parse matched non-marker content %q", content)
		}
	}
}

func TestResolveToolOutputLimit(t *testing.T) {
	def := DefaultMaxToolOutputBytes
	tests := []struct {
		name string
		cfg  map[string]any
		want int
	}{
		{"nil config", nil, def},
		{"int", map[string]any{ToolOutputLimitConfigKey: 123}, 123},
		{"float64 whole", map[string]any{ToolOutputLimitConfigKey: float64(2048)}, 2048},
		{"string", map[string]any{ToolOutputLimitConfigKey: " 456 "}, 456},
		{"malformed", map[string]any{ToolOutputLimitConfigKey: "bad"}, def},
		{"negative", map[string]any{ToolOutputLimitConfigKey: -5}, def},
		{"float64 below one", map[string]any{ToolOutputLimitConfigKey: 0.5}, def},
		{"float32 below one", map[string]any{ToolOutputLimitConfigKey: float32(0.9)}, def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveToolOutputLimit(tc.cfg); got != tc.want {
				t.Fatalf("ResolveToolOutputLimit = %d, want %d", got, tc.want)
			}
		})
	}
}
