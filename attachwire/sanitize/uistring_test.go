package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUIString(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"plain", "hello", 32, "hello"},
		{"strip_c0", "a\x00b\x07c\x1bd", 32, "abcd"},
		{"strip_del", "a\x7fb", 32, "ab"},
		{"strip_c1", "a\x9bb\x84c", 32, "abc"},
		{"strip_tab_newline", "line1\nline2\ttab", 64, "line1line2tab"},
		{"truncate", "abcdefghij", 5, "abcde"},
		{"truncate_zero", "abc", 0, ""},
		{"truncate_negative", "abc", -1, ""},
		{"unicode_kept", "café ☕", 32, "café ☕"},
		{"unicode_truncate_boundary", "áéíóú", 3, "áéí"},
		{"invalid_utf8_dropped", "a\xffb\xc3c", 32, "abc"},
		{"empty", "", 32, ""},
		{"only_controls", "\x00\x01\x1f\x7f\x80\x9f", 32, ""},
		{"mixed_escape_label", "Deploy \x1b[31mprod\x1b[0m", 64, "Deploy [31mprod[0m"},
		{"c1_continuation_not_stripped_as_control", "xٛy", 32, "xٛy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := UIString(tc.in, tc.maxLen)
			if got != tc.want {
				t.Fatalf("UIString(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("UIString produced invalid UTF-8: %q", got)
			}
			if utf8.RuneCountInString(got) > tc.maxLen && tc.maxLen > 0 {
				t.Fatalf("UIString exceeded rune cap: %d > %d", utf8.RuneCountInString(got), tc.maxLen)
			}
		})
	}
}

// TestUIStringNeverContainsControls fuzzes the invariant across arbitrary input.
func TestUIStringNeverContainsControls(t *testing.T) {
	inputs := []string{
		"\x1b]0;title\x07",
		strings.Repeat("a\x07", 100),
		"\x00\x1b\x9b\x9d\x7f mixed \U0001F600",
		"bidi\u202eoverride", // U+202E RIGHT-TO-LEFT OVERRIDE is a printable format char, kept by UIString
	}
	for _, in := range inputs {
		got := UIString(in, 1000)
		for _, r := range got {
			if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
				t.Fatalf("UIString(%q) leaked control rune %U", in, r)
			}
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8 from %q", in)
		}
	}
}

func TestUIStringRuneBoundarySafe(t *testing.T) {
	// A 4-byte rune (emoji) must never be split by truncation.
	in := "👍👍👍👍"
	for n := 0; n <= 4; n++ {
		got := UIString(in, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncation to %d runes split a rune: %q", n, got)
		}
		if utf8.RuneCountInString(got) != n {
			t.Fatalf("expected %d runes, got %d", n, utf8.RuneCountInString(got))
		}
	}
}
