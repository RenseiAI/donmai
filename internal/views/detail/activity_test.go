package detail

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/afclient"
)

// renderActivityLine must retain the FULL content — clipping at append
// time would make the LogViewer's wrap toggle a no-op (a pre-clipped
// line has nothing left to expand). Presentation-time clipping is the
// LogViewer/viewport's job (ANSI-aware ansi.Cut), so no mojibake or
// double-width overflow can come from this layer.
func TestRenderActivityLine_FullContentRetained(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
	}{
		{name: "ascii", content: strings.Repeat("long ascii content ", 40)},
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
			got := renderActivityLine(a)
			if !strings.Contains(got, tc.content) {
				t.Fatalf("expected full content retained for wrap to expand, got %d bytes", len(got))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("rendered line contains invalid UTF-8: %q", got)
			}
			if strings.Contains(got, "...") {
				t.Fatalf("content must not be ellipsis-clipped at append time: %q", got)
			}
		})
	}
}

// A badged action row keeps both the tool badge and the full content.
func TestRenderActivityLine_BadgeAndContentRetained(t *testing.T) {
	t.Parallel()
	tool := "Bash"
	content := strings.Repeat("日本語テキスト", 40)
	a := afclient.ActivityEvent{
		Type:      afclient.ActivityAction,
		ToolName:  &tool,
		Content:   content,
		Timestamp: "2026-06-09T01:02:03Z",
	}
	got := renderActivityLine(a)
	if !strings.Contains(got, "Bash") {
		t.Fatalf("expected tool badge preserved: %q", got)
	}
	if !strings.Contains(got, content) {
		t.Fatalf("expected full content preserved alongside the badge")
	}
}

// Short content stays untouched.
func TestRenderActivityLine_ShortContent(t *testing.T) {
	t.Parallel()
	a := afclient.ActivityEvent{
		Type:      afclient.ActivityThought,
		Content:   "short thought",
		Timestamp: "2026-06-09T01:02:03Z",
	}
	got := renderActivityLine(a)
	if !strings.Contains(got, "short thought") {
		t.Fatalf("expected full content preserved: %q", got)
	}
}

// The activity keymap keeps ToggleWrap ("w") live — disabling it was the
// regression that left long lines permanently clipped — while Clear stays
// disabled because the parent view owns the buffer lifecycle.
func TestActivityKeyMap_WrapEnabledClearDisabled(t *testing.T) {
	t.Parallel()
	km := activityKeyMap()
	if !km.ToggleWrap.Enabled() {
		t.Error("ToggleWrap must be enabled so long activity lines can be expanded")
	}
	if km.Clear.Enabled() {
		t.Error("Clear must stay disabled; the detail view owns the activity buffer")
	}
}

// Pressing "w" on a focused detail view toggles the LogViewer's wrap mode
// end-to-end through Update → handleKeyPress → LogViewer key dispatch.
func TestDetailModel_WrapKeyTogglesLogViewer(t *testing.T) {
	t.Parallel()
	m := New(afclient.NewMockClient())
	m.SetSize(80, 24)
	m.Focus()

	if !m.logViewer.Wrap() {
		t.Fatal("wrap must default to on (expanded) for the activity viewport")
	}

	w := tea.KeyPressMsg{Code: 'w', Text: "w"}
	m.Update(w)
	if m.logViewer.Wrap() {
		t.Fatal("first 'w' press should collapse (wrap off)")
	}
	m.Update(w)
	if !m.logViewer.Wrap() {
		t.Fatal("second 'w' press should expand again (wrap on)")
	}
}

func TestDetailModelTimedOutIsTerminal(t *testing.T) {
	t.Parallel()

	m := New(afclient.NewMockClient())
	m.session = &afclient.SessionDetail{Status: afclient.StatusTimedOut}
	if !m.isTerminal() {
		t.Fatal("timed_out session must stop detail activity polling")
	}
}

func TestBuildTimelineDistinguishesStartingAndTimedOut(t *testing.T) {
	t.Parallel()

	started := buildTimeline(afclient.SessionDetail{
		Status: afclient.StatusStarting,
		Timeline: afclient.SessionTimeline{
			Created: "2026-08-26T00:00:00Z",
		},
	})
	if got := started[len(started)-1]; got.label != "Starting..." || !got.active {
		t.Errorf("starting timeline tail = %#v, want active Starting...", got)
	}

	completedAt := "2026-08-26T00:01:00Z"
	timedOut := buildTimeline(afclient.SessionDetail{
		Status: afclient.StatusTimedOut,
		Timeline: afclient.SessionTimeline{
			Created:   "2026-08-26T00:00:00Z",
			Completed: &completedAt,
		},
	})
	if got := timedOut[len(timedOut)-1]; got.label != "Timed out" || got.active {
		t.Errorf("timed_out timeline tail = %#v, want terminal Timed out", got)
	}
}
