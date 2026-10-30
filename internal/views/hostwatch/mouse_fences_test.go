package hostwatch

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The five fences below each failed to pin one mouse decision boundary:
// removing the fence left the suite green. Each test drives the production
// Update with a MouseClickMsg and fails when its fence is reverted.

// Styled partial window: the selected block alone exceeds the pane, so only
// its head shows and the bottom marker row must miss. Width 80 gives two
// cards per row; height 12 gives a 5-row grid; cursor 8 sits in a partial
// block with a bottom marker at terminal row 5. Deleting the visible-row
// fence (always hitting the selected block) selects here and fails.
func TestModel_MousePartialGridBottomMarkerIsInert(t *testing.T) {
	m := newMouseModel(t, 80, 12, 8)
	view := ansi.Strip(m.render())
	lines := strings.Split(view, "\n")
	if len(lines) <= 5 || !strings.Contains(lines[5], "more") {
		t.Fatalf("setup: want partial bottom marker at row 5:\n%s", view)
	}
	keep, keepDetail := m.cursor, m.detail
	keepID := m.selectedVisibleID()
	if cmd := mouseClick(m, 1, 5, tea.MouseLeft); cmd != nil {
		t.Fatal("partial bottom marker click must not dispatch")
	}
	if m.cursor != keep || m.detail != keepDetail {
		t.Fatalf("bottom marker click (1,5) must be inert: cursor=%d detail=%v (want cursor=%d detail=%v)", m.cursor, m.detail, keep, keepDetail)
	}
	if got := m.selectedVisibleID(); got != keepID {
		t.Fatalf("bottom marker click selected %q, want %q", got, keepID)
	}
}

// Compact expanded window: the last rendered row must still select. Width
// 30 forces the one-line-per-session list; height 12 keeps an expanded
// window with rows 7, 8, 9 visible and markers at rows 1 and 5. Clicking
// (2,4) is the last visible row (index 9). Tightening the fence to reject
// the end index leaves the cursor behind and fails.
func TestModel_MouseCompactLastRowSelects(t *testing.T) {
	m := newMouseModel(t, 30, 12, 8)
	view := ansi.Strip(m.render())
	lines := strings.Split(view, "\n")
	if len(lines) <= 5 || !strings.Contains(lines[1], "more") || !strings.Contains(lines[5], "more") {
		t.Fatalf("setup: want compact expanded markers at rows 1 and 5:\n%s", view)
	}
	wantID := m.visibleCards()[9].SessionID
	if cmd := mouseClick(m, 2, 4, tea.MouseLeft); cmd != nil {
		t.Fatal("last rendered compact row click must not dispatch")
	}
	if got := m.selectedVisibleID(); got != wantID {
		t.Fatalf("last rendered compact row must select 9 at (2,4), got %q (cursor=%d)", got, m.cursor)
	}
	if m.detail {
		t.Fatal("selecting a different row must not open detail")
	}
}

// Styled expanded window: the bottom overflow marker must miss. Width 120
// with height 34 keeps two blocks on screen with markers at rows 1 and 16;
// cursor 6 sits in the shown window. Accepting the marker row (loop
// falling through to a hit) selects here and fails.
func TestModel_MouseExpandedGridBottomMarkerIsInert(t *testing.T) {
	m := newMouseModel(t, 120, 34, 6)
	view := ansi.Strip(m.render())
	lines := strings.Split(view, "\n")
	if len(lines) <= 16 || !strings.Contains(lines[1], "more") || !strings.Contains(lines[16], "more") {
		t.Fatalf("setup: want expanded markers at rows 1 and 16:\n%s", view)
	}
	keep, keepDetail := m.cursor, m.detail
	keepID := m.selectedVisibleID()
	if cmd := mouseClick(m, 1, 16, tea.MouseLeft); cmd != nil {
		t.Fatal("expanded bottom marker click must not dispatch")
	}
	if m.cursor != keep || m.detail != keepDetail {
		t.Fatalf("expanded bottom marker (1,16) must be inert: cursor=%d detail=%v (want cursor=%d detail=%v)", m.cursor, m.detail, keep, keepDetail)
	}
	if got := m.selectedVisibleID(); got != keepID {
		t.Fatalf("expanded bottom marker selected %q, want %q", got, keepID)
	}
}

// Confirmation gate: a right click at column zero must still only cancel,
// preserving cursor, detail, filter and selection. Requiring a nonzero
// column keeps the gate armed and fails.
func TestModel_MouseConfirmRightAtColumnZeroCancels(t *testing.T) {
	acts := newFakeHostActions()
	m := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }, Actions: acts})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.applySnapshot(Snapshot{Cards: fixtureCards(), Counters: fixtureCounters()})
	m.cursor = 0
	m.detail = true
	m.filterActive = true
	m.filterQuery = "ENG-1284"
	keepCursor, keepDetail := m.cursor, m.detail
	keepID := m.selectedVisibleID()
	keepFilterActive, keepFilterQuery := m.filterActive, m.filterQuery
	if cmd := m.requestStopConfirm(); cmd != nil {
		t.Fatal("arm must not dispatch")
	}
	if m.confirmKind != "stop" {
		t.Fatalf("setup: gate = %q, want stop", m.confirmKind)
	}
	if cmd := mouseClick(m, 0, 0, tea.MouseRight); cmd != nil {
		t.Fatal("right confirmation at (0,0) must not dispatch")
	}
	if m.confirmKind != "" {
		t.Fatalf("right confirmation at (0,0) must only cancel: gate=%q detail=%v filter=%v cursor=%d", m.confirmKind, m.detail, m.filterActive, m.cursor)
	}
	if m.cursor != keepCursor || m.detail != keepDetail {
		t.Fatalf("right confirmation at (0,0) must only cancel: cursor=%d detail=%v (want cursor=%d detail=%v)", m.cursor, m.detail, keepCursor, keepDetail)
	}
	if got := m.selectedVisibleID(); got != keepID {
		t.Fatalf("right confirmation at (0,0) must keep selection %q (got %q)", keepID, got)
	}
	if m.filterActive != keepFilterActive || m.filterQuery != keepFilterQuery {
		t.Fatalf("right confirmation at (0,0) must keep filter active=%v query=%q", m.filterActive, m.filterQuery)
	}
	if _, _, drains, stops := acts.counts(); drains != 0 || len(stops) != 0 {
		t.Fatalf("right confirmation at (0,0) dispatched drains=%d stops=%v", drains, stops)
	}
}

// Help-open mouse clicks must preserve selection: while the contextual
// help overlay is open, a click that would otherwise select a different
// rendered card stays inert and the overlay stays open. Deleting the
// help guard in handleMouseClick selects here and fails.
func TestModel_MouseHelpOpenIsInert(t *testing.T) {
	m := newMouseModel(t, 120, 40, 0)
	x, y := mouseXYForVis(t, m, 1)
	wantID := m.selectedVisibleID()
	feedKey(m, questionKey())
	if !m.help {
		t.Fatal("setup: \"?\" did not open the help overlay")
	}
	if cmd := mouseClick(m, x, y, tea.MouseLeft); cmd != nil {
		t.Fatal("help-open click must not dispatch")
	}
	if !m.help {
		t.Fatal("help-open click closed the overlay")
	}
	if got := m.selectedVisibleID(); got != wantID {
		t.Fatalf("help-open click selected %q, want %q (cursor=%d)", got, wantID, m.cursor)
	}
	if m.detail {
		t.Fatal("help-open click must not open detail")
	}
	// Right clicks while help is open are equally inert.
	if cmd := mouseClick(m, x, y, tea.MouseRight); cmd != nil {
		t.Fatal("help-open right click must not dispatch")
	}
	if !m.help || m.selectedVisibleID() != wantID || m.detail {
		t.Fatalf("help-open right click mutated state: help=%v id=%q detail=%v", m.help, m.selectedVisibleID(), m.detail)
	}
}
