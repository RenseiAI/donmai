package detail

import (
	"strings"
	"testing"
	"unicode/utf8"

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
