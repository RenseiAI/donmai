package hostwatch

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// clickRendered clicks the first rendered occurrence of needle, grounded
// in the actual frame the model just drew.
func clickRendered(t *testing.T, m *Model, needle string, button tea.MouseButton) tea.Cmd {
	t.Helper()
	row, col := cellIndex(m.View().Content, needle)
	if row < 0 {
		t.Fatalf("%q not rendered:\n%s", needle, m.View().Content)
	}
	return clickXY(m, col, row, button)
}

func clickXY(m *Model, x, y int, button tea.MouseButton) tea.Cmd {
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: button})
	return cmd
}

func wantSelected(t *testing.T, m *Model, id string) {
	t.Helper()
	if got := m.selectedVisibleID(); got != id {
		t.Fatalf("selected = %q, want %q (cursor %d)", got, id, m.cursor)
	}
}

func TestMouse_ClickSelectsRenderedCardLayouts(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		cursor        int
		needle, want  string
	}{
		{"multi-column", 120, 40, 0, "ENG-1290", "1c2d3e4f-2222"},
		{"multi-column second row", 120, 40, 0, "ENG-1310", "5a6b7c8d-6666"},
		{"one-column", 40, 40, 0, "ENG-1290", "1c2d3e4f-2222"},
		{"compact", 30, 30, 0, "ENG-1301", "2d3e4f5a-3333"},
		{"wide", 200, 40, 0, "ENG-1311", "6b7c8d9e-7777"},
		{"overflow window", 120, 22, 6, "ENG-1312", "7c8d9e0f-8888"},
		{"compact overflow", 30, 12, 10, "ENG-1403", "8d9e0f1a-0003"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newFixtureModel(t, tc.width, tc.height, false, tc.cursor)
			if cmd := clickRendered(t, m, tc.needle, tea.MouseLeft); cmd != nil {
				t.Fatal("mouse click returned a command")
			}
			wantSelected(t, m, tc.want)
		})
	}
}

func TestMouse_ClickFollowsFilterResizeAndSnapshot(t *testing.T) {
	m := newFixtureModel(t, 40, 32, false, 0)
	m.filterActive, m.filterQuery = true, "ENG-14"
	m.cursor = 5
	clickRendered(t, m, "ENG-1404", tea.MouseLeft)
	wantSelected(t, m, "8d9e0f1a-0004")
	clickXY(m, 0, 0, tea.MouseRight)
	if m.filterActive || m.filterQuery != "" {
		t.Fatal("right click did not clear filtering")
	}
	wantSelected(t, m, "8d9e0f1a-0004")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	cards := fixtureCards()
	m.applySnapshot(Snapshot{Cards: []SessionCard{cards[2], cards[0]}, Counters: fixtureCounters()})
	clickRendered(t, m, "ENG-1284", tea.MouseLeft)
	wantSelected(t, m, "0b1e6c2a-1111")
	m.setSplit(maxSplitRatio)
	clickRendered(t, m, "ENG-1301", tea.MouseLeft)
	wantSelected(t, m, "2d3e4f5a-3333")
}

func TestMouse_DetailToggleAndRightClickEscape(t *testing.T) {
	m := newFixtureModel(t, 120, 40, false, 0)
	clickRendered(t, m, "ENG-1284", tea.MouseLeft) // already selected: enter
	if !m.detail {
		t.Fatal("clicking the selected card did not open detail")
	}
	if view := m.View().Content; !strings.Contains(view, "session detail") || !strings.Contains(view, "Fix flaky retry") {
		t.Fatalf("detail content missing:\n%s", view)
	}
	clickRendered(t, m, "ENG-1284", tea.MouseLeft)
	if m.detail {
		t.Fatal("clicking the selected card again did not close detail")
	}
	clickRendered(t, m, "ENG-1284", tea.MouseLeft)
	clickXY(m, 1, 1, tea.MouseRight)
	if m.detail {
		t.Fatal("right click did not close detail")
	}
	clickRendered(t, m, "ENG-1284", tea.MouseLeft)
	clickRendered(t, m, "ENG-1290", tea.MouseLeft)
	wantSelected(t, m, "1c2d3e4f-2222")
	if !m.detail || !strings.Contains(m.View().Content, "session detail") {
		t.Fatal("selecting another card should keep detail on the new card")
	}
}

func TestMouse_MissesAreInert(t *testing.T) {
	m := newFixtureModel(t, 120, 22, false, 6)
	before := m.selectedVisibleID()
	_, cardW := gridGeometry(120, false)
	for _, xy := range [][2]int{{0, 0}, {cardW, 3}, {0, m.height - 1}, {-1, 3}, {3, -1}, {500, 3}, {3, 500}} {
		if cmd := clickXY(m, xy[0], xy[1], tea.MouseLeft); cmd != nil {
			t.Fatalf("click %v returned a command", xy)
		}
	}
	if row, _ := cellIndex(m.View().Content, "more"); row >= 0 {
		clickXY(m, 0, row, tea.MouseLeft)
	}
	row, col := cellIndex(m.View().Content, "ENG-1312")
	_, _ = m.Update(tea.MouseReleaseMsg{X: col, Y: row, Button: tea.MouseLeft})
	_, _ = m.Update(tea.MouseMotionMsg{X: col, Y: row, Button: tea.MouseLeft})
	_, _ = m.Update(tea.MouseWheelMsg{X: col, Y: row, Button: tea.MouseWheelDown})
	clickXY(m, col, row, tea.MouseMiddle)
	wantSelected(t, m, before)
	if m.detail {
		t.Fatal("inert mouse input opened detail")
	}
	m.applySnapshot(Snapshot{Cards: nil, Counters: fixtureCounters()})
	clickXY(m, 1, 2, tea.MouseLeft)
	m.snapErr = errFake("down")
	clickXY(m, 1, 2, tea.MouseLeft)
	if m.detail {
		t.Fatal("click on empty/error view opened detail")
	}
}

func TestMouse_PartialCardAndPlainPolicy(t *testing.T) {
	m := newFixtureModel(t, 120, 12, false, 4)
	want := m.selectedVisibleID()
	clickRendered(t, m, "4f5a6b7c", tea.MouseLeft)
	if !m.detail {
		t.Fatal("click on partial selected card did not open detail")
	}
	wantSelected(t, m, want)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("interactive MouseMode = %v, want cell motion", got)
	}
	for name, opts := range map[string]Options{
		"plain":     {Plain: true},
		"unbounded": {Plain: true, UnboundedCards: true},
	} {
		pm := New(opts)
		_, _ = pm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		pm.applySnapshot(Snapshot{Cards: fixtureCards(), Counters: fixtureCounters()})
		if got := pm.View().MouseMode; got != tea.MouseModeNone {
			t.Fatalf("%s MouseMode = %v, want none", name, got)
		}
		row, col := cellIndex(pm.View().Content, "ENG-1290")
		clickXY(pm, col, row, tea.MouseLeft)
		clickXY(pm, col, row, tea.MouseRight)
		if pm.cursor != 0 || pm.detail {
			t.Fatalf("%s view reacted to mouse input", name)
		}
	}
}

func TestMouse_ErrorWithRetainedCardsIsInert(t *testing.T) {
	// The daemon-error view renders no cards. With cards retained in
	// state, a click where a card would be must still do nothing.
	m := newFixtureModel(t, 120, 40, false, 1)
	before := m.selectedVisibleID()
	if before != "1c2d3e4f-2222" {
		t.Fatalf("fixture cursor = %q, want selected card", before)
	}
	// Reachability: the same cell selects a different card while healthy.
	reach := newFixtureModel(t, 120, 40, false, 1)
	if cmd := clickXY(reach, 5, 2, tea.MouseLeft); cmd != nil {
		t.Fatal("healthy click returned a command")
	}
	wantSelected(t, reach, "0b1e6c2a-1111")
	// Now fail the daemon with cards retained and click the same cell.
	m.snapErr = errFake("down")
	if cmd := clickXY(m, 5, 2, tea.MouseLeft); cmd != nil {
		t.Fatal("error-view click returned a command")
	}
	wantSelected(t, m, before)
	if m.detail {
		t.Fatal("click on error view with retained cards opened detail")
	}
	if cmd := clickXY(m, 5, 2, tea.MouseRight); cmd != nil {
		t.Fatal("error-view right click returned a command")
	}
	wantSelected(t, m, before)
	if m.detail {
		t.Fatal("right click on error view opened detail")
	}
}

func TestMouse_RightClickOutsideFrameIsInert(t *testing.T) {
	m := newFixtureModel(t, 120, 40, false, 0)
	m.filterActive, m.filterQuery = true, "ENG-14"
	m.cursor = 0
	m.detail = true
	beforeQ := m.filterQuery
	for _, xy := range [][2]int{{-1, 3}, {500, 3}, {3, -1}, {3, 500}, {m.width, 3}, {3, m.height}} {
		if cmd := clickXY(m, xy[0], xy[1], tea.MouseRight); cmd != nil {
			t.Fatalf("right click %v returned a command", xy)
		}
		if !m.filterActive || m.filterQuery != beforeQ {
			t.Fatalf("outside-frame right click %v cleared filtering", xy)
		}
		if !m.detail {
			t.Fatalf("outside-frame right click %v closed detail", xy)
		}
	}
	wantSelected(t, m, m.visibleCards()[0].SessionID)
	// Inside-frame right click still follows escape (clears filter).
	if cmd := clickXY(m, 1, 1, tea.MouseRight); cmd != nil {
		t.Fatal("inside right click returned a command")
	}
	if m.filterActive {
		t.Fatal("inside-frame right click did not clear filtering")
	}
}

func TestMouse_ZeroHeightPaneIsInert(t *testing.T) {
	// Height 2 leaves no rows for the card pane (gridH == 0).
	m := newFixtureModel(t, 120, 2, false, 1)
	gridH, _ := splitPaneHeights(m.height-2, m.split)
	if gridH != 0 {
		t.Fatalf("gridH = %d, want 0 for height 2", gridH)
	}
	before := m.selectedVisibleID()
	if cmd := clickXY(m, 5, 1, tea.MouseLeft); cmd != nil {
		t.Fatal("zero-height click returned a command")
	}
	wantSelected(t, m, before)
	if m.detail {
		t.Fatal("zero-height pane click opened detail")
	}
	// Reachability: the same cell selects with a normal-height pane.
	reach := newFixtureModel(t, 120, 40, false, 1)
	if cmd := clickXY(reach, 5, 1, tea.MouseLeft); cmd != nil {
		t.Fatal("healthy click returned a command")
	}
	wantSelected(t, reach, "0b1e6c2a-1111")
}

func TestMouse_CompactPartialMarkerAndCard(t *testing.T) {
	// Narrow pane with a one-row grid window: the overflow marker shares
	// the pane with a single compact row.
	m := newFixtureModel(t, 30, 6, false, 7)
	view := m.View().Content
	markerRow, _ := cellIndex(view, "more")
	if markerRow < 0 {
		t.Fatalf("marker not rendered:\n%s", view)
	}
	cardRow, cardCol := cellIndex(view, "ENG-1312")
	if cardRow < 0 {
		t.Fatalf("selected compact card not rendered:\n%s", view)
	}
	if cmd := clickXY(m, 5, markerRow, tea.MouseLeft); cmd != nil {
		t.Fatal("marker click returned a command")
	}
	wantSelected(t, m, "7c8d9e0f-8888")
	if m.detail {
		t.Fatal("compact marker click opened detail")
	}
	if cmd := clickXY(m, cardCol, cardRow, tea.MouseLeft); cmd != nil {
		t.Fatal("compact card click returned a command")
	}
	if !m.detail {
		t.Fatal("click on compact partial selected card did not open detail")
	}
	wantSelected(t, m, "7c8d9e0f-8888")
}

func TestMouse_StreamChromeBeneathPartialIsInert(t *testing.T) {
	// Wide pane with a cut card row: only the head of the selected row
	// shows; the marker and stream rows beneath it are not cards.
	m := newFixtureModel(t, 120, 12, false, 4)
	view := m.View().Content
	topRow, _ := cellIndex(view, "more")
	if topRow < 0 {
		t.Fatalf("overflow marker not rendered:\n%s", view)
	}
	streamRow, streamCol := cellIndex(view, "session stream")
	if streamRow < 0 {
		t.Fatalf("stream title not rendered:\n%s", view)
	}
	before := m.selectedVisibleID()
	for _, xy := range [][2]int{{5, topRow}, {5, streamRow}, {streamCol, streamRow}} {
		if cmd := clickXY(m, xy[0], xy[1], tea.MouseLeft); cmd != nil {
			t.Fatalf("click %v returned a command", xy)
		}
		wantSelected(t, m, before)
		if m.detail {
			t.Fatalf("click %v beneath partial card opened detail", xy)
		}
	}
	// The visible head of the selected row still toggles detail.
	if cmd := clickRendered(t, m, "4f5a6b7c", tea.MouseLeft); cmd != nil {
		t.Fatal("partial card click returned a command")
	}
	if !m.detail {
		t.Fatal("click on visible partial head did not open detail")
	}
	wantSelected(t, m, before)
}

func TestMouse_ClickCancelsConfirmationWithoutAction(t *testing.T) {
	m := newFixtureModel(t, 120, 40, false, 0)
	acts := newFakeHostActions()
	m.actions = acts
	pressActionKey(m, "x")
	if m.confirmKind != "stop" {
		t.Fatalf("confirmKind = %q, want stop", m.confirmKind)
	}
	if cmd := clickRendered(t, m, "ENG-1290", tea.MouseLeft); cmd != nil {
		t.Fatal("selection click during confirmation returned a command")
	}
	if m.confirmKind != "" || m.detail {
		t.Fatalf("click left confirmKind=%q detail=%v, want cancelled without detail", m.confirmKind, m.detail)
	}
	wantSelected(t, m, "1c2d3e4f-2222")
	_, cmd := pressActionKey(m, "y")
	if cmd != nil {
		_ = cmd()
	}
	if _, _, drains, stops := acts.counts(); drains != 0 || len(stops) != 0 {
		t.Fatalf("mouse-cancelled confirmation still acted: drains=%d stops=%v", drains, stops)
	}
	pressActionKey(m, "d")
	if m.confirmKind != "drain" {
		t.Fatalf("confirmKind = %q, want drain", m.confirmKind)
	}
	clickXY(m, 0, 0, tea.MouseRight)
	if m.confirmKind != "" {
		t.Fatalf("right click left confirmKind=%q, want cancelled", m.confirmKind)
	}
}
