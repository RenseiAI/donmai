package detail

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/afclient"
)

// renderActivityLine must clip long content on rune boundaries — a byte
// slice can split a multi-byte UTF-8 sequence mid-character and render
// mojibake.
func TestRenderActivityLine_ClipIsRuneSafe(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("日本語テキスト", 40) // 3-byte runes, far past any clip width
	a := afclient.ActivityEvent{
		Type:      afclient.ActivityThought,
		Content:   long,
		Timestamp: "2026-06-09T01:02:03Z",
	}
	got := renderActivityLine(a, 80)
	if !utf8.ValidString(got) {
		t.Fatalf("rendered line contains invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Fatalf("expected clipped content to carry an ellipsis: %q", got)
	}
}

// The clip must be width-aware, not rune-count-aware: CJK (and emoji)
// runes occupy two terminal cells, so a rune-count clip leaves the
// rendered row up to twice the budget and overflows the viewport.
func TestRenderActivityLine_ClipIsWidthAware(t *testing.T) {
	t.Parallel()
	const width = 80
	cases := []struct {
		name    string
		content string
	}{
		{name: "cjk", content: strings.Repeat("日本語テキスト", 40)},
		{name: "emoji", content: strings.Repeat("\U0001f600\U0001f680", 120)},
		{name: "mixed", content: strings.Repeat("ascii日本語", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := afclient.ActivityEvent{
				Type:      afclient.ActivityThought,
				Content:   tc.content,
				Timestamp: "2026-06-09T01:02:03Z",
			}
			got := renderActivityLine(a, width)
			if w := lipgloss.Width(got); w > width {
				t.Fatalf("rendered line is %d cells wide, want <= %d: %q", w, width, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("rendered line contains invalid UTF-8: %q", got)
			}
			if !strings.Contains(got, "...") {
				t.Fatalf("expected clipped content to carry an ellipsis: %q", got)
			}
		})
	}
}

// The tool badge spends part of the content budget — a badged action row
// with wide content must still fit the viewport width.
func TestRenderActivityLine_BadgeCountsAgainstWidth(t *testing.T) {
	t.Parallel()
	const width = 80
	tool := "Bash"
	a := afclient.ActivityEvent{
		Type:      afclient.ActivityAction,
		ToolName:  &tool,
		Content:   strings.Repeat("日本語テキスト", 40),
		Timestamp: "2026-06-09T01:02:03Z",
	}
	got := renderActivityLine(a, width)
	if w := lipgloss.Width(got); w > width {
		t.Fatalf("badged line is %d cells wide, want <= %d: %q", w, width, got)
	}
	if !strings.Contains(got, "Bash") {
		t.Fatalf("expected tool badge preserved: %q", got)
	}
}

// Short content stays untouched by the clip.
func TestRenderActivityLine_ShortContentUnclipped(t *testing.T) {
	t.Parallel()
	a := afclient.ActivityEvent{
		Type:      afclient.ActivityThought,
		Content:   "short thought",
		Timestamp: "2026-06-09T01:02:03Z",
	}
	got := renderActivityLine(a, 80)
	if !strings.Contains(got, "short thought") {
		t.Fatalf("expected full content preserved: %q", got)
	}
	if strings.Contains(got, "...") {
		t.Fatalf("short content must not be clipped: %q", got)
	}
}
