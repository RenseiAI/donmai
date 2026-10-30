package hostwatch

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// AC-1: ? opens help in list, detail and Unicode filter contexts;
// repeated ?/esc closes only the overlay; detail/filter/selection survive.
// Routing ? after filter/detail handling keeps the query and fails.
func TestHelpOverlay_OpenClosePreservesState(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "日本語")
	if got := visibleIDs(m); len(got) != 1 {
		t.Fatalf("unicode filter visible = %v, want 1", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.detail {
		t.Fatal("enter did not open detail for the filtered match")
	}
	keepID, keepQuery := m.selectedVisibleID(), m.filterQuery
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if !m.helpOpen {
		t.Fatal("? did not open help from detail+filter context")
	}
	if m.filterQuery != keepQuery || m.selectedVisibleID() != keepID || !m.detail {
		t.Fatalf("opening help mutated state: query %q detail %v sel %q", m.filterQuery, m.detail, m.selectedVisibleID())
	}
	view := ansi.Strip(m.render())
	for _, want := range []string{"keyboard help", "filter: /" + keepQuery, "mouse is not used"} {
		if !strings.Contains(view, want) {
			t.Errorf("help frame missing %q:\n%s", want, view)
		}
	}
	// Deterministic toggle: close with ? and reopen renders identically.
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if m.helpOpen {
		t.Fatal("repeated ? did not close help")
	}
	if m.filterQuery != keepQuery || !m.detail || m.selectedVisibleID() != keepID {
		t.Fatal("closing help mutated underlying state")
	}
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	second := ansi.Strip(m.render())
	if second != view {
		t.Error("repeated ? is not deterministic")
	}
	// esc closes help before detail: first esc keeps detail+filter.
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.helpOpen || !m.detail || !m.filterActive || m.filterQuery != keepQuery {
		t.Fatalf("esc must close only help: open=%v detail=%v filter=%v query=%q", m.helpOpen, m.detail, m.filterActive, m.filterQuery)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.detail || m.filterActive {
		t.Fatal("second esc must restore the stream and exit filtering")
	}
}

// AC-2: while help is open, selection/follow/split/filter and lifecycle
// keys are consumed, mouse sends no command, ctrl+c quits, confirmations
// ignore ?, and resize/snapshot/tail/action results still apply. Removing
// overlay key consumption lets p reach the action handler and fails.
func TestHelpOverlay_ConsumesInputAndSurvivesUpdates(t *testing.T) {
	acts := newFakeHostActions()
	m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if !m.helpOpen {
		t.Fatal("? did not open help")
	}
	cursor, following, split := m.cursor, m.stream.Following(), m.split
	for _, key := range []string{"j", "k", "f", "g", "p", "r", "x", "d", "/", "enter"} {
		var msg tea.KeyPressMsg
		switch key {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		if _, cmd := m.Update(msg); cmd != nil {
			t.Fatalf("%s while help open returned a command", key)
		}
	}
	if m.cursor != cursor || m.stream.Following() != following || m.split != split || m.filterActive {
		t.Fatal("help consumed keys mutated underlying state")
	}
	if pauses, resumes, drains, stops := acts.counts(); pauses != 0 || resumes != 0 || drains != 0 || len(stops) != 0 {
		t.Fatalf("help leaked lifecycle requests: %d %d %d %v", pauses, resumes, drains, stops)
	}
	if _, cmd := m.Update(tea.MouseClickMsg{X: 1, Y: 1}); cmd != nil {
		t.Fatal("mouse message must send no command")
	}
	if m.cursor != cursor {
		t.Fatal("mouse message moved selection")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown}); cmd != nil {
		t.Fatal("down while help open must scroll without a command")
	}
	// Pending confirmations ignore ? and stay armed.
	m.helpOpen = false
	if _, cmd := m.Update(actionKey("x")); cmd != nil {
		t.Fatal("x must arm without dispatch")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"}); cmd != nil {
		t.Fatal("? during confirm must not dispatch")
	}
	if m.confirmKind != "stop" || m.helpOpen {
		t.Fatalf("? resolved the gate or opened help: kind=%q open=%v", m.confirmKind, m.helpOpen)
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"}); cmd != nil {
		t.Fatal("n must cancel without dispatch")
	}
	// Non-key updates still apply while help is open.
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if m.width != 80 || m.height != 20 || !m.helpOpen {
		t.Fatal("resize must apply and keep help open")
	}
	before := len(m.cards)
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != before || !m.helpOpen {
		t.Fatal("snapshot must apply without closing help")
	}
	m.applyTailBatch(nil)
	if !m.helpOpen {
		t.Fatal("tail batch closed help")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	_ = cmd
	m.helpOpen = true
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd != nil {
		t.Fatal("q while help open is consumed, not quit (ctrl+c quits)")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c while help open must quit")
	}
}

// AC-3: 40/80/120/200 plain/styled frames with Unicode and overflow fit
// their dimensions, scrolling a short help reaches every row, and footer
// keeps ? at 40 columns with long filters/notices. Removing width/scroll
// bounds fails.
func TestHelpOverlay_FramesBoundedAndFooterKeepsHelp(t *testing.T) {
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{false, true} {
			m := newFixtureModel(t, width, 40, plain, 1)
			feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
			raw := m.render()
			lines := strings.Split(raw, "\n")
			if len(lines) != 40 {
				t.Fatalf("width=%d plain=%v help frame has %d rows, want 40", width, plain, len(lines))
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > width {
					t.Fatalf("width=%d plain=%v row %d %d cells: %q", width, plain, i, w, ansi.Strip(line))
				}
			}
			if plain && strings.Contains(raw, "\x1b") {
				t.Fatalf("width=%d plain help frame carries escapes", width)
			}
			stripped := ansi.Strip(raw)
			if !strings.Contains(stripped, "keyboard help") || !strings.Contains(stripped, "mouse is not used") {
				t.Fatalf("width=%d plain=%v help frame lacks title/mouse row:\n%s", width, plain, stripped)
			}
		}
	}
	// Short help scrolls so every row stays reachable.
	m := newFixtureModel(t, 40, 10, true, 0)
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	first := ansi.Strip(m.render())
	for i := 0; i < 30; i++ {
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	scrolled := ansi.Strip(m.render())
	if first == scrolled && m.helpMaxScroll() == 0 {
		t.Fatal("short help never scrolls; last rows unreachable")
	}
	lastRow := hostHelpRows[len(hostHelpRows)-1].key
	found := false
	for i := 0; i <= m.helpMaxScroll()+1; i++ {
		if strings.Contains(ansi.Strip(m.render()), lastRow) {
			found = true
			break
		}
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if !found {
		t.Fatalf("scrolling never reaches final help row %q", lastRow)
	}
	// Footer keeps ? at 40 columns with overflowing filter and notice.
	narrow := newFixtureModel(t, 40, 40, true, 0)
	narrow.notice = strings.Repeat("notice-outcome ", 20)
	foot := narrow.renderHelp()
	if !strings.Contains(foot, "?") {
		t.Fatalf("40-col footer with long notice lost ?: %q", foot)
	}
	feedKey(narrow, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(narrow, strings.Repeat("query-overflow-", 10))
	foot = narrow.renderHelp()
	if w := lipgloss.Width(foot); w > 40 {
		t.Fatalf("40-col filter footer is %d cells: %q", w, foot)
	}
	if !strings.Contains(foot, "?") || !strings.Contains(foot, "filter: /") {
		t.Fatalf("40-col filter footer lost ? or indicator: %q", foot)
	}
}

// AC-4 overlay half: every shipped control renders in the open overlay.
// Expected keys are hardcoded so deleting a hostHelpRows entry fails.
func TestHelpOverlay_ListsEveryShippedControl(t *testing.T) {
	m := newFixtureModel(t, 200, 40, true, 0)
	feedKey(m, tea.KeyPressMsg{Code: '?', Text: "?"})
	view := ansi.Strip(m.render())
	for _, want := range []string{"↑↓ / j k", "enter", "esc", "?", "/ + type", "backspace", "[ ] / 0", "f / g", "x then y", "p / r", "d then y", "y / n", "q / ctrl+c", "mouse", "mouse is not used", "filter"} {
		if !strings.Contains(view, want) {
			t.Errorf("help overlay missing shipped control %q", want)
		}
	}
}
