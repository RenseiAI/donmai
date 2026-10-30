package hostwatch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/charmbracelet/x/ansi"
)

// mouse helpers ground coordinates in the rendered frame: y comes from the
// stripped view row holding the identity, x from the card column geometry.
func mouseClick(m *Model, x, y int, btn tea.MouseButton) tea.Cmd {
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: btn})
	return cmd
}

func mouseXYForVis(t *testing.T, m *Model, visIdx int) (int, int) {
	t.Helper()
	vis := m.visibleCards()
	if visIdx < 0 || visIdx >= len(vis) {
		t.Fatalf("visIdx %d out of %d", visIdx, len(vis))
	}
	id := vis[visIdx].displayID()
	gridH, _ := splitPaneHeights(m.height-2, m.split)
	lines := strings.Split(ansi.Strip(m.render()), "\n")
	y := -1
	for i := 1; i < 1+gridH && i < len(lines); i++ {
		if strings.Contains(lines[i], id) {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatalf("id %q not in grid window:\n%s", id, strings.Join(lines, "\n"))
	}
	perRow, cardW := gridGeometry(m.width, m.opts.Plain)
	if perRow == 0 {
		return 2, y
	}
	return (visIdx%perRow)*(cardW+cardGap) + 1, y
}

func mouseRowWith(t *testing.T, m *Model, sub string) int {
	t.Helper()
	for i, ln := range strings.Split(ansi.Strip(m.render()), "\n") {
		if strings.Contains(ln, sub) {
			return i
		}
	}
	t.Fatalf("row with %q not rendered", sub)
	return -1
}

func newMouseModel(t *testing.T, w, h, cursor int) *Model {
	t.Helper()
	m := newFixtureModel(t, w, h, false, cursor)
	return m
}

// AC-1: clicks select the rendered identity across layouts, filtering,
// resize/split and snapshot reorder. Removing dispatch leaves the cursor.
func TestModel_MouseSelectsRenderedIdentity(t *testing.T) {
	// Multi-column styled grid.
	m := newMouseModel(t, 120, 40, 0)
	x, y := mouseXYForVis(t, m, 1)
	if cmd := mouseClick(m, x, y, tea.MouseLeft); cmd != nil {
		t.Fatal("selection click must not dispatch")
	}
	if got := m.selectedVisibleID(); got != m.visibleCards()[1].SessionID {
		t.Fatalf("multi-column click selected %q, want %q", got, m.visibleCards()[1].SessionID)
	}
	// Single column styled (width 40 -> one card per row).
	m = newMouseModel(t, 40, 40, 0)
	x, y = mouseXYForVis(t, m, 1)
	mouseClick(m, x, y, tea.MouseLeft)
	if got := m.selectedVisibleID(); got != m.visibleCards()[1].SessionID {
		t.Fatalf("single-column click selected %q", got)
	}
	// Compact list (width below card minimum).
	m = newMouseModel(t, 30, 30, 0)
	x, y = mouseXYForVis(t, m, 3)
	mouseClick(m, x, y, tea.MouseLeft)
	if got := m.selectedVisibleID(); got != m.visibleCards()[3].SessionID {
		t.Fatalf("compact click selected %q", got)
	}
	// Overflow window: short terminal, selection deep keeps window on it.
	m = newMouseModel(t, 80, 12, 9)
	vis := m.visibleCards()
	x, y = mouseXYForVis(t, m, 9)
	mouseClick(m, x, y, tea.MouseLeft)
	if got := m.selectedVisibleID(); got != vis[9].SessionID {
		t.Fatalf("overflow click selected %q, want %q", got, vis[9].SessionID)
	}
	// Clicking another visible card in the same window selects it.
	lines := strings.Split(ansi.Strip(m.render()), "\n")
	found := ""
	for _, ln := range lines {
		for _, c := range m.visibleCards() {
			if c.SessionID != m.selectedVisibleID() && strings.Contains(ln, c.displayID()) {
				found = c.SessionID
				break
			}
		}
		if found != "" {
			break
		}
	}
	if found == "" {
		t.Fatal("no second visible card to click")
	}
	idx := -1
	for i, c := range m.visibleCards() {
		if c.SessionID == found {
			idx = i
		}
	}
	x, y = mouseXYForVis(t, m, idx)
	mouseClick(m, x, y, tea.MouseLeft)
	if got := m.selectedVisibleID(); got != found {
		t.Fatalf("overflow reselect = %q, want %q", got, found)
	}
	// Filtering: mapping follows the visible projection, not the source.
	m = New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	_, _ = m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "development")
	if got := len(m.visibleCards()); got != 2 {
		t.Fatalf("filtered visible = %d, want 2", got)
	}
	x, y = mouseXYForVis(t, m, 1)
	mouseClick(m, x, y, tea.MouseLeft)
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("filtered click selected %q, want delta (source index would be beta)", got)
	}
	mouseClick(m, x, y, tea.MouseLeft)
	if !m.detail {
		t.Fatal("filtered selected click must toggle detail")
	}
	if view := ansi.Strip(m.render()); !strings.Contains(view, "sess-delta-0004") || strings.Contains(view, "sess-beta-0002") {
		t.Fatalf("filtered detail must show delta, not source beta:\n%s", view)
	}
	mouseClick(m, x, y, tea.MouseLeft) // close again for later steps
	// Resize/split/snapshot reorder keep hits on the rendered identity.
	mr := newMouseModel(t, 120, 40, 0)
	_, _ = mr.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	mr.handleKey(keyMsg("["))
	x, y = mouseXYForVis(t, mr, 1)
	mouseClick(mr, x, y, tea.MouseLeft)
	if got := mr.selectedVisibleID(); got != mr.visibleCards()[1].SessionID {
		t.Fatalf("after resize/split click = %q", got)
	}
	rev := []SessionCard{mr.cards[3], mr.cards[2], mr.cards[1], mr.cards[0]}
	mr.applySnapshot(Snapshot{Cards: rev, Counters: fixtureCounters()})
	x, y = mouseXYForVis(t, mr, 3)
	mouseClick(mr, x, y, tea.MouseLeft)
	if got := mr.selectedVisibleID(); got != mr.visibleCards()[3].SessionID {
		t.Fatalf("after reorder click = %q, want %q", got, mr.visibleCards()[3].SessionID)
	}
}

// AC-2: selected-card clicks toggle like Enter, right clicks match Escape;
// gaps, markers, chrome, stream and empty/error views do nothing.
func TestModel_MouseDetailAndInertRegions(t *testing.T) {
	m := newMouseModel(t, 120, 40, 1)
	x, y := mouseXYForVis(t, m, 1)
	if cmd := mouseClick(m, x, y, tea.MouseLeft); cmd != nil {
		t.Fatal("detail toggle must not dispatch")
	}
	if !m.detail {
		t.Fatal("clicking the selected card must toggle detail like Enter")
	}
	if view := ansi.Strip(m.render()); !strings.Contains(view, m.visibleCards()[1].displayID()) {
		t.Fatalf("detail frame missing selected identity:\n%s", view)
	}
	mouseClick(m, x, y, tea.MouseLeft)
	if m.detail {
		t.Fatal("second click on the selected card must close detail")
	}
	// Right click matches Escape (closes detail).
	mouseClick(m, x, y, tea.MouseLeft)
	if !m.detail {
		t.Fatal("setup: detail should be open")
	}
	mouseClick(m, 5, 5, tea.MouseRight)
	if m.detail {
		t.Fatal("right click must match Escape and close detail")
	}
	// Right click while filtering clears it like Escape.
	mf := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }})
	_, _ = mf.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mf.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	_, _ = mf.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(mf, "development")
	mouseClick(mf, 5, 5, tea.MouseRight)
	if mf.filterActive || mf.filterQuery != "" || len(mf.visibleCards()) != 4 {
		t.Fatalf("right click must clear filter like Escape: active=%v query=%q", mf.filterActive, mf.filterQuery)
	}
	// Inert regions: header, help, stream, gaps, markers, bounds, empty.
	m2 := newMouseModel(t, 120, 40, 0)
	keep, keepDetail := m2.cursor, m2.detail
	mouseClick(m2, 2, 0, tea.MouseLeft)           // header
	mouseClick(m2, 2, m2.height-1, tea.MouseLeft) // help
	gridH, _ := splitPaneHeights(m2.height-2, m2.split)
	mouseClick(m2, 2, 1+gridH, tea.MouseLeft) // stream title
	if m2.cursor != keep || m2.detail != keepDetail {
		t.Fatal("header/help/stream clicks must do nothing")
	}
	perRow, cardW := gridGeometry(m2.width, false)
	if perRow > 1 {
		xg := cardW // inter-card gap column
		yg := mouseRowWith(t, m2, m2.visibleCards()[0].displayID())
		mouseClick(m2, xg, yg, tea.MouseLeft)
		if m2.cursor != keep {
			t.Fatal("gap click must do nothing")
		}
		// Gap in a row that does not contain the cursor must also do
		// nothing. The same-row gap above pins the column but not the
		// row: without the gap guard a gap click here selects that
		// row's card (cursor 0 -> perRow).
		if len(m2.visibleCards()) > perRow {
			ygOther := mouseRowWith(t, m2, m2.visibleCards()[perRow].displayID())
			if cmd := mouseClick(m2, xg, ygOther, tea.MouseLeft); cmd != nil {
				t.Fatal("other-row gap click must not dispatch")
			}
			if m2.cursor != keep || m2.detail != keepDetail {
				t.Fatalf("gap click in another row must do nothing (cursor=%d detail=%v)", m2.cursor, m2.detail)
			}
		}
	}
	mo := newMouseModel(t, 80, 12, 9)
	keep = mo.cursor
	if view := ansi.Strip(mo.render()); strings.Contains(view, "more") {
		my := mouseRowWith(t, mo, "more")
		mouseClick(mo, 1, my, tea.MouseLeft)
		if mo.cursor != keep || mo.detail {
			t.Fatal("marker click must do nothing")
		}
	}
	if cmd := mouseClick(mo, -1, -1, tea.MouseLeft); cmd != nil {
		t.Fatal("out-of-bounds click must not dispatch")
	}
	if mo.cursor != keep {
		t.Fatal("out-of-bounds click must do nothing")
	}
	me := newMouseModel(t, 120, 40, 0)
	me.applySnapshot(Snapshot{Cards: nil})
	if cmd := mouseClick(me, 10, 5, tea.MouseLeft); cmd != nil {
		t.Fatal("empty view click must not dispatch")
	}
	if me.detail {
		t.Fatal("empty view click must do nothing")
	}
	me.snapErr = fmt.Errorf("down")
	if cmd := mouseClick(me, 10, 5, tea.MouseLeft); cmd != nil {
		t.Fatal("error view click must not dispatch")
	}
}

// AC-2 expanded window: a click on the top overflow marker in an expanded
// window must do nothing (cursor and detail unchanged, no command). This
// pins the expanded-branch marker guard in sessionIndexAt, which the
// partial-window marker test cannot reach.
func TestModel_MouseExpandedMarkerIsInert(t *testing.T) {
	m := newMouseModel(t, 120, 40, 7)
	view := ansi.Strip(m.render())
	lines := strings.Split(view, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "more") {
		t.Fatalf("expected expanded top marker at row 1:\n%s", view)
	}
	keep := m.cursor
	if cmd := mouseClick(m, 1, 1, tea.MouseLeft); cmd != nil {
		t.Fatal("expanded marker click must not dispatch")
	}
	if m.cursor != keep || m.detail {
		t.Fatalf("expanded marker click must leave cursor=%d detail=%v (got cursor=%d detail=%v)", keep, false, m.cursor, m.detail)
	}
}

// AC-3: only interactive bounded views request reporting and react.
func TestModel_MouseViewPolicy(t *testing.T) {
	m := newMouseModel(t, 120, 40, 0)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("interactive MouseMode = %v, want CellMotion", got)
	}
	mp := newFixtureModel(t, 120, 40, true, 0)
	if got := mp.View().MouseMode; got == tea.MouseModeCellMotion {
		t.Fatalf("plain MouseMode = %v, must not request reporting", got)
	}
	mu := New(Options{Plain: true, UnboundedCards: true, HostLabel: "h", Now: func() time.Time { return fixtureNow }})
	_, _ = mu.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mu.applySnapshot(Snapshot{Cards: fixtureCards(), Counters: fixtureCounters()})
	if got := mu.View().MouseMode; got == tea.MouseModeCellMotion {
		t.Fatalf("unbounded MouseMode = %v, must not request reporting", got)
	}
	keep := mp.cursor
	_, cmd := mp.Update(tea.MouseClickMsg{X: 10, Y: 3, Button: tea.MouseLeft})
	if cmd != nil || mp.cursor != keep || mp.detail {
		t.Fatal("plain views must ignore mouse input")
	}
	_, cmd = mu.Update(tea.MouseClickMsg{X: 10, Y: 3, Button: tea.MouseLeft})
	if cmd != nil || mu.detail {
		t.Fatal("unbounded views must ignore mouse input")
	}
	// Right-clicks must also be inert in plain/unbounded views, including
	// with an active filter. Left-clicks are shadowed by sessionIndexAt's
	// own plain guard, so only right-clicks pin handleMouseClick's guard:
	// without it a right-click clears the filter like Escape.
	for _, plainOpts := range []Options{
		{Plain: true, HostLabel: "h", Now: func() time.Time { return fixtureNow }},
		{Plain: true, UnboundedCards: true, HostLabel: "h", Now: func() time.Time { return fixtureNow }},
	} {
		pf := New(plainOpts)
		_, _ = pf.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		pf.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
		_, _ = pf.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
		feedQuery(pf, "development")
		if !pf.filterActive || len(pf.visibleCards()) != 2 {
			t.Fatalf("setup: filtered visible = %d, want 2", len(pf.visibleCards()))
		}
		pf.detail = true
		keepQ, keepCursor := pf.filterQuery, pf.cursor
		if _, cmd := pf.Update(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseRight}); cmd != nil {
			t.Fatal("plain right-click must not dispatch")
		}
		if !pf.filterActive || pf.filterQuery != keepQ || pf.cursor != keepCursor || !pf.detail {
			t.Fatalf("plain right-click must leave filter/cursor/detail unchanged (active=%v query=%q cursor=%d detail=%v)", pf.filterActive, pf.filterQuery, pf.cursor, pf.detail)
		}
	}
	// Out-of-bounds right-clicks on the interactive view must do nothing,
	// even with a filter and detail set. Without the bounds guard a
	// right-click at (-1,-1) or x==width clears the filter/closes detail.
	moob := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }})
	_, _ = moob.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	moob.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	_, _ = moob.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(moob, "development")
	moob.detail = true
	keepQ, keepCursor := moob.filterQuery, moob.cursor
	for _, pos := range [][2]int{{-1, -1}, {120, 5}, {5, 40}} {
		if _, cmd := moob.Update(tea.MouseClickMsg{X: pos[0], Y: pos[1], Button: tea.MouseRight}); cmd != nil {
			t.Fatalf("oob right-click at %v must not dispatch", pos)
		}
		if !moob.filterActive || moob.filterQuery != keepQ || moob.cursor != keepCursor || !moob.detail {
			t.Fatalf("oob right-click at %v must leave filter/cursor/detail unchanged", pos)
		}
	}
}

// AC-4: selection clicks cancel stop/drain confirmation without toggling
// or dispatching; right clicks cancel; y afterwards sends nothing.
func TestModel_MouseCancelsConfirmation(t *testing.T) {
	for _, kind := range []string{"stop", "drain"} {
		acts := newFakeHostActions()
		m := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }, Actions: acts})
		_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
		if kind == "stop" {
			if cmd := m.requestStopConfirm(); cmd != nil {
				t.Fatal("arm must not dispatch")
			}
		} else {
			if cmd := m.requestDrainConfirm(); cmd != nil {
				t.Fatal("arm must not dispatch")
			}
		}
		x, y := mouseXYForVis(t, m, 1)
		if cmd := mouseClick(m, x, y, tea.MouseLeft); cmd != nil {
			t.Fatalf("%s: selection click must not dispatch", kind)
		}
		if m.confirmKind != "" || m.detail {
			t.Fatalf("%s: selection click must cancel without toggling detail", kind)
		}
		if got := m.selectedVisibleID(); got != m.visibleCards()[1].SessionID {
			t.Fatalf("%s: selection click must select, got %q", kind, got)
		}
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if cmd != nil {
			t.Fatalf("%s: y after cancel must not dispatch", kind)
		}
		if _, _, drains, stops := acts.counts(); drains != 0 || len(stops) != 0 {
			t.Fatalf("%s: canceled action dispatched drains=%d stops=%v", kind, drains, stops)
		}
		// Right click cancels the same way.
		if kind == "stop" {
			m.requestStopConfirm()
		} else {
			m.requestDrainConfirm()
		}
		if cmd := mouseClick(m, 5, 5, tea.MouseRight); cmd != nil {
			t.Fatalf("%s: right cancel must not dispatch", kind)
		}
		if m.confirmKind != "" {
			t.Fatalf("%s: right click must cancel confirmation", kind)
		}
	}
}

// Pins the visible-projection mapping in sessionIndexAt: with the grid
// filtered to 2 of 4, a click in the grid pane below the rendered rows
// (where source-layout rows would be) must do nothing. Mapping against
// the source slice selects a hidden row and loses the selection.
func TestModel_MouseFilteredOutOfWindowIsInert(t *testing.T) {
	m := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	_, _ = m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "development")
	if got := len(m.visibleCards()); got != 2 {
		t.Fatalf("setup: filtered visible = %d, want 2", got)
	}
	if m.cursor != 0 {
		t.Fatalf("setup: cursor = %d, want 0", m.cursor)
	}
	keepID := m.selectedVisibleID()
	if keepID != "sess-alpha-0001" {
		t.Fatalf("setup: selected = %q, want alpha", keepID)
	}
	gridH, _ := splitPaneHeights(m.height-2, m.split)
	y := 1 + cardHeight(false) + 1
	if y-1 < 0 || y-1 >= gridH {
		t.Fatalf("setup: probe y=%d outside gridH=%d", y, gridH)
	}
	x := 2
	if cmd := mouseClick(m, x, y, tea.MouseLeft); cmd != nil {
		t.Fatal("below-window click must not dispatch")
	}
	if m.cursor != 0 || m.detail {
		t.Fatalf("below-window click must leave cursor=0 detail=false (got cursor=%d detail=%v)", m.cursor, m.detail)
	}
	if got := m.selectedVisibleID(); got != keepID {
		t.Fatalf("below-window click selected %q, want %q", got, keepID)
	}
	if !m.filterActive || len(m.visibleCards()) != 2 {
		t.Fatal("below-window click must not disturb the filter")
	}
}

// Pins the middle/unknown-button guard in handleMouseClick: middle and
// extended buttons are inert, including on the selected card and while a
// confirmation gate is armed. Without the guard they act as left-click.
func TestModel_MouseMiddleButtonIsInert(t *testing.T) {
	m := newMouseModel(t, 120, 40, 0)
	x, y := mouseXYForVis(t, m, 1)
	keep := m.cursor
	for _, btn := range []tea.MouseButton{tea.MouseMiddle, tea.MouseButton10} {
		if cmd := mouseClick(m, x, y, btn); cmd != nil {
			t.Fatalf("button %v must not dispatch", btn)
		}
		if m.cursor != keep || m.detail {
			t.Fatalf("button %v must leave cursor=%d detail=false (got cursor=%d detail=%v)", btn, keep, m.cursor, m.detail)
		}
	}
	if got := m.selectedVisibleID(); got != m.visibleCards()[keep].SessionID {
		t.Fatalf("middle click moved selection to %q", got)
	}
	// Middle-click on the selected card must not toggle detail like Enter.
	ms := newMouseModel(t, 120, 40, 1)
	xs, ys := mouseXYForVis(t, ms, 1)
	for _, btn := range []tea.MouseButton{tea.MouseMiddle, tea.MouseButton10} {
		if cmd := mouseClick(ms, xs, ys, btn); cmd != nil {
			t.Fatalf("selected-card button %v must not dispatch", btn)
		}
		if ms.detail {
			t.Fatalf("selected-card button %v must not open detail", btn)
		}
	}
	// An armed confirmation gate survives middle/extended clicks.
	for _, btn := range []tea.MouseButton{tea.MouseMiddle, tea.MouseButton10} {
		acts := newFakeHostActions()
		mc := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }, Actions: acts})
		_, _ = mc.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		mc.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
		if cmd := mc.requestStopConfirm(); cmd != nil {
			t.Fatal("arm must not dispatch")
		}
		xc, yc := mouseXYForVis(t, mc, 1)
		if cmd := mouseClick(mc, xc, yc, btn); cmd != nil {
			t.Fatalf("confirm-armed button %v must not dispatch", btn)
		}
		if mc.confirmKind != "stop" || mc.cursor != 0 {
			t.Fatalf("confirm-armed button %v must keep gate armed at cursor 0 (got kind=%q cursor=%d)", btn, mc.confirmKind, mc.cursor)
		}
		if _, _, drains, stops := acts.counts(); drains != 0 || len(stops) != 0 {
			t.Fatalf("confirm-armed button %v dispatched drains=%d stops=%v", btn, drains, stops)
		}
	}
}

// Pins the confirmation-gate invalid-click guard in handleMouseClick: a
// header or gap click while stop/drain is armed keeps the gate armed with
// the cursor unchanged and dispatches nothing. Without the guard the gate
// silently disarms.
func TestModel_MouseInvalidClickKeepsConfirmArmed(t *testing.T) {
	for _, kind := range []string{"stop", "drain"} {
		acts := newFakeHostActions()
		m := New(Options{Plain: false, HostLabel: "h", Now: func() time.Time { return fixtureNow }, Actions: acts})
		_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.applySnapshot(Snapshot{Cards: fixtureCards(), Counters: fixtureCounters()})
		m.cursor = 1
		if kind == "stop" {
			if cmd := m.requestStopConfirm(); cmd != nil {
				t.Fatal("arm must not dispatch")
			}
		} else {
			if cmd := m.requestDrainConfirm(); cmd != nil {
				t.Fatal("arm must not dispatch")
			}
		}
		if m.confirmKind != kind {
			t.Fatalf("%s: setup gate = %q, want %q", kind, m.confirmKind, kind)
		}
		// Header click is outside every card window.
		if cmd := mouseClick(m, 2, 0, tea.MouseLeft); cmd != nil {
			t.Fatalf("%s: header click must not dispatch", kind)
		}
		if m.confirmKind != kind || m.cursor != 1 || m.detail {
			t.Fatalf("%s: header click must keep gate armed at cursor 1 (got kind=%q cursor=%d detail=%v)", kind, m.confirmKind, m.cursor, m.detail)
		}
		// Inter-card gap click in the selected row also misses.
		_, cardW := gridGeometry(m.width, false)
		yg := mouseRowWith(t, m, m.visibleCards()[1].displayID())
		if cmd := mouseClick(m, cardW, yg, tea.MouseLeft); cmd != nil {
			t.Fatalf("%s: gap click must not dispatch", kind)
		}
		if m.confirmKind != kind || m.cursor != 1 || m.detail {
			t.Fatalf("%s: gap click must keep gate armed at cursor 1 (got kind=%q cursor=%d detail=%v)", kind, m.confirmKind, m.cursor, m.detail)
		}
		if _, _, drains, stops := acts.counts(); drains != 0 || len(stops) != 0 {
			t.Fatalf("%s: invalid clicks dispatched drains=%d stops=%v", kind, drains, stops)
		}
	}
}

// newTestModel builds a plain-mode model over a fake daemon + on-disk
// worktree fixtures, sized for rendering.
func newTestModel(t *testing.T, fd *fakeDaemon, scope string) *Model {
	t.Helper()
	src := NewSource(fd, state.NewStore(), scope)
	m := New(Options{
		Source:        src,
		Plain:         true,
		IndexInterval: time.Millisecond,
		TailInterval:  time.Millisecond,
		Now:           func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return mm.(*Model)
}

func TestModel_FullFlow_PlainRender(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-1")
	writeState(t, wt, state.State{
		IssueIdentifier: "ENG-1284",
		ProviderName:    "claude",
		WorkType:        "development",
		StartedAt:       time.Date(2026, 6, 13, 13, 58, 0, 0, time.UTC).UnixMilli(),
	})
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID:    "sess-1",
			State:        "running",
			Repository:   "o/acme",
			ProjectName:  "acme",
			WorktreePath: wt,
		}},
		status: &afclient.DaemonStatusResponse{ActiveSessions: 1, MaxSessions: 8, UptimeSeconds: 60, Version: "0.39.0"},
		stats:  &afclient.DaemonStatsResponse{QueueDepth: 0},
	}

	m := newTestModel(t, fd, "o/acme")

	// 1. Drive an index poll → cards + tailer registration.
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 1 {
		t.Fatalf("want 1 card after snapshot, got %d", len(m.cards))
	}
	if _, ok := m.tailers["sess-1"]; !ok {
		t.Fatal("tailer should be registered for sess-1 (worktree path known)")
	}

	// 2. Append events to the session's events.jsonl, then drive a tail tick.
	writeEvents(t, evPath,
		agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "pnpm test"}},
		agent.ToolResultEvent{ToolName: "Bash", Content: "ok"},
	)
	drainTails(t, m)

	writeEvents(t, evPath, agent.AssistantTextEvent{Text: "running the suite"})
	drainTails(t, m)

	out := m.render()
	for _, want := range []string{"ENG-1284", "session stream", "development", "running the suite"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestModel_TerminalAggregateReplacesWithoutEndingLiveTail(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-x")
	writeState(t, wt, state.State{IssueIdentifier: "ENG-9", StartedAt: 1})
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-x", State: "running", Repository: "o/a", WorktreePath: wt,
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	drainTails(t, m)

	cost := 2.0
	turns := 3
	writeEvents(t, evPath, agent.ResultEvent{Success: true, Cost: &agent.CostData{TotalCostUsd: cost, NumTurns: turns}, ObservedCostUsd: &cost, ObservedTurns: &turns})
	drainTails(t, m)

	if _, ok := m.tailers["sess-x"]; !ok {
		t.Error("live tailer must remain after a provider turn result")
	}
	if c := findCard(m, "sess-x"); c == nil || c.CostUsd != 2.0 || c.NumTurns != 3 || !c.CostReported || !c.TurnsReported {
		t.Errorf("terminal cost/turns not folded onto card: %#v", c)
	}
}

func TestModel_DaemonUnreachableShowsWarning(t *testing.T) {
	fd := &fakeDaemon{sessErr: errFake("connection refused")}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	out := m.render()
	if !strings.Contains(out, "daemon unreachable") {
		t.Errorf("want unreachable warning, got:\n%s", out)
	}
}

func TestModel_PreEnrichmentNoWorktreeNoTailer(t *testing.T) {
	// A pre-enrichment daemon returns no WorktreePath; the model still shows
	// a card but cannot tail it.
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "old", State: "running", Repository: "o/a", // no WorktreePath
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(m.cards))
	}
	if len(m.tailers) != 0 {
		t.Errorf("want 0 tailers (no worktree path), got %d", len(m.tailers))
	}
}

func TestModel_HeldSessionHasNoTailerActivityOrRunningClaim(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "held-123", State: "unknown", Repository: "o/a", ProjectName: "a"},
			{SessionID: "live-123", State: "running", Repository: "o/a"},
		},
		status: &afclient.DaemonStatusResponse{ActiveSessions: 1, MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	// Plain cards stack; reserve enough rows for both complete cards.
	resized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 80})
	m = resized.(*Model)
	// Model an earlier projection of the same identity as live: when the new
	// held snapshot arrives, any stale event tailer must be dropped.
	m.tailers["held-123"] = NewTailer("held-123", "/fixture/old/events.jsonl", true, time.Now)
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 2 {
		t.Fatalf("want both visible sessions, got %d", len(m.cards))
	}
	if _, ok := m.tailers["held-123"]; ok {
		t.Error("held session must not retain or start an event tailer")
	}

	m.applyTailBatch([]TailEvent{{
		SessionID: "held-123", At: time.Now(), Event: agent.AssistantTextEvent{Text: "fabricated live activity"},
	}})
	out := m.render()
	for _, want := range []string{"2 sessions", "held-123", "live-123", "held", "awaiting local execution"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if strings.Contains(header, "running") || !strings.Contains(header, "2 sessions") {
		t.Errorf("header should count visible sessions without claiming all are running: %q", header)
	}
	for _, absent := range []string{"fabricated live activity"} {
		if strings.Contains(out, absent) {
			t.Errorf("render should not claim/show %q:\n%s", absent, out)
		}
	}
	if c := findCard(m, "held-123"); c == nil || c.LastActivity != "" || c.ToolCalls != 0 {
		t.Errorf("held card acquired live metrics: %#v", c)
	}
}

func TestHeldSnapshotDropsPreviousLiveObservations(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{
		SessionID: "held", DaemonState: "running", Observed: true, MetricsReported: true,
		ActualModel: "prior-response", ActualModelProvider: "prior-vendor", ActualModelVersion: "prior-version",
		ToolCalls: 3, CostUsd: 1, LastActivity: "prior activity", LastWorkAt: time.Now(), LastOutputAt: time.Now(),
	}}
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "held", DaemonState: "unknown", AgentCardID: "card-id", AgentCardName: "Review"}}})
	c := m.cards[0]
	if c.Observed || c.MetricsReported || c.ActualModel != "" || c.ActualModelProvider != "" || c.ActualModelVersion != "" ||
		c.ToolCalls != 0 || c.CostUsd != 0 || c.LastActivity != "" || !c.LastWorkAt.IsZero() || !c.LastOutputAt.IsZero() {
		t.Fatalf("held projection inherited previous live data: %+v", c)
	}
	if c.AgentCardID != "card-id" || c.AgentCardName != "Review" {
		t.Fatalf("held projection lost explicit card annotations: %+v", c)
	}
}

func TestModel_KeyNavAndFollow(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "a", State: "running", Repository: "o/a"},
			{SessionID: "b", State: "running", Repository: "o/a"},
		},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	if m.cursor != 0 {
		t.Fatalf("initial cursor: want 0, got %d", m.cursor)
	}
	m.handleKey(keyMsg("down"))
	if m.cursor != 1 {
		t.Errorf("after down: want cursor 1, got %d", m.cursor)
	}
	m.handleKey(keyMsg("down")) // clamp at len-1
	if m.cursor != 1 {
		t.Errorf("cursor should clamp at 1, got %d", m.cursor)
	}
	m.handleKey(keyMsg("up"))
	if m.cursor != 0 {
		t.Errorf("after up: want 0, got %d", m.cursor)
	}
	wasFollowing := m.stream.Following()
	m.handleKey(keyMsg("f"))
	if m.stream.Following() == wasFollowing {
		t.Error("f should toggle follow")
	}
}

func TestModel_QuitKey(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{status: &afclient.DaemonStatusResponse{}}, "")
	_, cmd := m.handleKey(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// drainTails runs one synchronous tail poll through the model's update path.
func drainTails(t *testing.T, m *Model) {
	t.Helper()
	cmd := m.pollTails()
	if cmd == nil {
		return
	}
	msg := cmd()
	batch, ok := msg.(tailBatchMsg)
	if !ok {
		t.Fatalf("pollTails: want tailBatchMsg, got %T", msg)
	}
	m.applyTailBatch(batch.events)
}

func findCard(m *Model, id string) *SessionCard {
	for i := range m.cards {
		if m.cards[i].SessionID == id {
			return &m.cards[i]
		}
	}
	return nil
}

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	default:
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
}

// ── host-watch card filter ───────────────────────────────────────────────────

// filterTestCards returns four cards with disjoint sanitized surfaces so
// each of the four allowed match fields can be pinned independently.
func filterTestCards() []SessionCard {
	return []SessionCard{
		{SessionID: "sess-alpha-0001", IssueIdentifier: "ENG-1001", IssueTitle: "Fix flaky upload retry", Repository: "https://github.com/acme/web.git", WorkType: "development", DaemonState: "running"},
		{SessionID: "sess-beta-0002", IssueIdentifier: "ENG-2002", IssueTitle: "Billing ledger migration", Repository: "https://github.com/acme/billing.git", WorkType: "qa", DaemonState: "running"},
		{SessionID: "sess-gamma-0003", IssueIdentifier: "ENG-3003", IssueTitle: "日本語の更新", Repository: "https://github.com/acme/docs.git", WorkType: "research", DaemonState: "running"},
		{SessionID: "sess-delta-0004", IssueIdentifier: "ENG-4004", IssueTitle: "Tighten session cache", Repository: "https://github.com/acme/api.git", WorkType: "development", DaemonState: "running", Handle: "ops-lead"},
	}
}

// newFilterModel builds a sized plain model over filterTestCards without a
// daemon, so the tests drive the production Update + render path only.
func newFilterModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := New(Options{Plain: true, Now: func() time.Time { return fixtureNow }, Handles: mapHandleEnricher{"sess-delta-0004": "ops-lead"}})
	_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	return m
}

// newUnboundedFilterModel builds the same filter fixture with the piped
// plain unbounded card path enabled, so grid assertions cover both the
// bounded and the unbounded renderGrid call sites.
func newUnboundedFilterModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := New(Options{Plain: true, UnboundedCards: true, Now: func() time.Time { return fixtureNow }, Handles: mapHandleEnricher{"sess-delta-0004": "ops-lead"}})
	_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	return m
}

// feedQuery feeds one production Update per rune.
func feedQuery(m *Model, query string) {
	for _, r := range query {
		_, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func feedKey(m *Model, msg tea.KeyPressMsg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

func visibleIDs(m *Model) []string {
	vis := m.visibleCards()
	out := make([]string, len(vis))
	for i, c := range vis {
		out[i] = c.SessionID
	}
	return out
}

func equalIDs(a, b []string) bool {
	return slices.Equal(a, b)
}

// TestModel_FilterNarrowsAndRestores proves AC-1 through production Update:
// "/" enters filtering, typed runes live-narrow across display identity,
// title, repository, and work type, backspace deletes one rune, and escape
// restores every card in snapshot order. Bypassing the match predicate
// (always true) keeps every non-match visible and fails this test.
func TestModel_FilterNarrowsAndRestores(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"display identity issue id", "ENG-2002", []string{"sess-beta-0002"}},
		{"display identity handle", "ops-lead", []string{"sess-delta-0004"}},
		{"title match", "ledger", []string{"sess-beta-0002"}},
		{"repository match", "web", []string{"sess-alpha-0001"}},
		{"work type match", "research", []string{"sess-gamma-0003"}},
		{"case insensitive", "LEDGER", []string{"sess-beta-0002"}},
		{"unicode title", "日本語", []string{"sess-gamma-0003"}},
		{"snapshot order preserved", "ENG-", []string{"sess-alpha-0001", "sess-beta-0002", "sess-gamma-0003"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newFilterModel(t, 120, 40)
			if got := visibleIDs(m); len(got) != 4 {
				t.Fatalf("unfiltered visible = %v, want 4 cards", got)
			}
			feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
			if !m.filterActive {
				t.Fatal("\"/\" did not enter filter mode")
			}
			feedQuery(m, tc.query)
			if got := visibleIDs(m); !equalIDs(got, tc.want) {
				t.Fatalf("query %q visible = %v, want %v", tc.query, got, tc.want)
			}
			view := m.render()
			wantSet := map[string]bool{}
			for _, w := range tc.want {
				wantSet[w] = true
			}
			for _, id := range tc.want {
				c := findCard(m, id)
				if c == nil {
					t.Fatalf("source card %s missing", id)
				}
				if !strings.Contains(view, c.displayID()) {
					t.Errorf("bounded render missing visible %q for query %q:\n%s", c.displayID(), tc.query, view)
				}
			}
			// B1 (bounded): no non-matching card may render in the grid.
			for _, c := range m.cards {
				if !wantSet[c.SessionID] && strings.Contains(view, c.displayID()) {
					t.Errorf("bounded render shows non-match %q (%s) for query %q:\n%s", c.SessionID, c.displayID(), tc.query, view)
				}
			}
			// B4: the active query is visible in the help row.
			if !strings.Contains(view, "filter: /"+tc.query) {
				t.Errorf("bounded render missing filter indicator %q for query %q:\n%s", "filter: /"+tc.query, tc.query, view)
			}
			// B1 (unbounded): the same query on the piped unbounded path
			// must also hide every non-match.
			mu := newUnboundedFilterModel(t, 120, 40)
			feedKey(mu, tea.KeyPressMsg{Code: '/', Text: "/"})
			feedQuery(mu, tc.query)
			uview := mu.render()
			for _, id := range tc.want {
				c := findCard(mu, id)
				if c == nil {
					t.Fatalf("unbounded source card %s missing", id)
				}
				if !strings.Contains(uview, c.displayID()) {
					t.Errorf("unbounded render missing visible %q for query %q:\n%s", c.displayID(), tc.query, uview)
				}
			}
			for _, c := range mu.cards {
				if !wantSet[c.SessionID] && strings.Contains(uview, c.displayID()) {
					t.Errorf("unbounded render shows non-match %q (%s) for query %q:\n%s", c.SessionID, c.displayID(), tc.query, uview)
				}
			}
			if !strings.Contains(uview, "filter: /"+tc.query) {
				t.Errorf("unbounded render missing filter indicator %q for query %q:\n%s", "filter: /"+tc.query, tc.query, uview)
			}
			for _, c := range m.cards {
				isWant := false
				for _, w := range tc.want {
					if c.SessionID == w {
						isWant = true
					}
				}
				if !isWant {
					for _, v := range m.visibleCards() {
						if v.SessionID == c.SessionID {
							t.Errorf("non-match %s remains visible for query %q", c.SessionID, tc.query)
						}
					}
				}
			}
			feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.filterActive || m.filterQuery != "" {
				t.Fatalf("escape left filter active=%v query=%q", m.filterActive, m.filterQuery)
			}
			if got := visibleIDs(m); !equalIDs(got, []string{"sess-alpha-0001", "sess-beta-0002", "sess-gamma-0003", "sess-delta-0004"}) {
				t.Fatalf("after escape visible = %v, want snapshot order", got)
			}
			// B4 (exit): the indicator must clear with the query.
			escapedView := m.render()
			if strings.Contains(escapedView, "filter: /") {
				t.Errorf("render still shows filter indicator after esc for query %q:\n%s", tc.query, escapedView)
			}
			if len(m.cards) != 4 {
				t.Fatalf("source cards mutated by filter: %d", len(m.cards))
			}
		})
	}
}

// TestModel_FilterBackspaceDeletesOneRune proves rune-safe backspace
// through production Update.
func TestModel_FilterBackspaceDeletesOneRune(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "日本語")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("cjk query visible = %v", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.filterQuery != "日本" {
		t.Fatalf("backspace query = %q, want one rune deleted", m.filterQuery)
	}
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("after one backspace visible = %v, want gamma only", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.filterQuery != "" {
		t.Fatalf("query = %q, want empty", m.filterQuery)
	}
	if got := visibleIDs(m); len(got) != 4 {
		t.Fatalf("empty query visible = %v, want all 4", got)
	}
}

// TestModel_FilterConsumesPrintableShortcuts proves filter mode consumes
// printable keys (q, f, navigation letters) as query text instead of
// running their global shortcuts.
func TestModel_FilterConsumesPrintableShortcuts(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	following := m.stream.Following()
	cmd := feedKey(m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd != nil {
		t.Error("\"q\" in filter mode returned a command (must not quit)")
	}
	if m.filterQuery != "q" {
		t.Fatalf("filter query = %q, want q", m.filterQuery)
	}
	if m.stream.Following() != following {
		t.Error("follow toggled while typing q in filter mode")
	}
	feedKey(m, tea.KeyPressMsg{Code: 'f', Text: "f"})
	if m.stream.Following() != following {
		t.Error("\"f\" toggled follow while filtering; it must be filter text")
	}
	if m.filterQuery != "qf" {
		t.Fatalf("filter query = %q, want qf", m.filterQuery)
	}
	feedKey(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.filterQuery != "qfj" {
		t.Fatalf("filter query = %q, want qfj", m.filterQuery)
	}
	if len(m.visibleCards()) != 0 {
		t.Fatalf("qfj should match nothing, visible = %v", visibleIDs(m))
	}
}

// TestModel_FilterSelectionPreservedAcrossEditsAndSnapshots proves AC-2
// through production Update: the same SessionID stays selected while
// visible across query edits and a reordered snapshot; exclusion selects
// the first match; no match selects none; enter/detail never indexes the
// unfiltered slice. Retaining the old numeric cursor selects the wrong
// session and fails this test.
func TestModel_FilterSelectionPreservedAcrossEditsAndSnapshots(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, keyMsg("down"))
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("selected = %s, want sess-beta-0002", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after / selected = %s, want beta", got)
	}
	feedQuery(m, "ENG-")
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("query ENG- selected = %s, want beta by id", got)
	}
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (beta index in full list)", m.cursor)
	}
	reordered := []SessionCard{filterTestCards()[3], filterTestCards()[2], filterTestCards()[1], filterTestCards()[0]}
	m.applySnapshot(Snapshot{Cards: reordered, Counters: Counters{Sessions: 4, Running: 4}})
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after reorder selected = %s, want beta by id", got)
	}
	if m.cursor != 1 {
		t.Fatalf("after reorder cursor = %d, want 1 (beta by id in ENG- visible)", m.cursor)
	}
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	for m.filterQuery != "" {
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after clear selected = %s, want beta", got)
	}
	feedQuery(m, "research")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("research visible = %v", got)
	}
	if got := m.selectedVisibleID(); got != "sess-gamma-0003" {
		t.Fatalf("excluded beta must fall back to first match gamma, got %s", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.detail {
		t.Fatal("enter did not open detail for the visible match")
	}
	view := m.render()
	if !strings.Contains(view, "sess-gamma-0003") && !strings.Contains(view, "日本") {
		t.Fatalf("detail shows wrong session (must be gamma):\n%s", view)
	}
	if strings.Contains(view, "sess-alpha-0001") {
		t.Fatalf("detail indexed the unfiltered slice (alpha leaks):\n%s", view)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "development")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-alpha-0001", "sess-delta-0004"}) {
		t.Fatalf("development visible = %v", got)
	}
	if got := m.selectedVisibleID(); got != "sess-alpha-0001" {
		t.Fatalf("development first match = %s, want alpha", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("after down selected = %s, want delta", got)
	}
	// B2: pressing down past the end of a narrowed filter must clamp to
	// the last visible card instead of overshooting the visible slice
	// (overshoot leaves selection "" and breaks enter/highlight).
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("after overshoot down selected = %q, want last visible sess-delta-0004", got)
	}
	if m.cursor != 1 {
		t.Fatalf("after overshoot down cursor = %d, want 1 (last visible index)", m.cursor)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.selectedVisibleID(); got != "sess-alpha-0001" {
		t.Fatalf("after up selected = %s, want alpha", got)
	}
	// B2 (unfiltered): down past the end without a filter clamps to the
	// last card as well.
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	for i := 0; i < 6; i++ {
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("unfiltered overshoot down selected = %q, want last sess-delta-0004", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "zzz-no-such-session")
	if len(m.visibleCards()) != 0 {
		t.Fatalf("no-match visible = %v, want empty", visibleIDs(m))
	}
	m.detail = false
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.detail {
		t.Error("enter with no match must not open detail")
	}
	_ = m.render()
}

// TestModel_FilteredStopTargetsVisibleCard proves the merge-forward with
// the lifecycle-confirm gate: x/d stop selection follows the visible
// (filtered) card at the cursor, not m.cards[m.cursor]. While filtering,
// printable keys are filter text, so the test arms the stop through the
// production requestStopConfirm handler (what x calls) after narrowing
// to "development" and moving to the second visible card; y then goes
// through the production confirm gate (which outranks filtering) and must
// stop the visible delta, not the unfiltered beta at the same numeric
// cursor. Indexing m.cards directly stops the wrong session and fails.
func TestModel_FilteredStopTargetsVisibleCard(t *testing.T) {
	acts := newFakeHostActions()
	m := New(Options{
		Plain:   true,
		Now:     func() time.Time { return fixtureNow },
		Handles: mapHandleEnricher{"sess-delta-0004": "ops-lead"},
		Actions: acts,
	})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "development")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-alpha-0001", "sess-delta-0004"}) {
		t.Fatalf("development visible = %v, want [alpha delta]", got)
	}
	// Second visible card is delta (unfiltered index 3); the same numeric
	// cursor in the unfiltered slice is beta.
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("filtered selected = %s, want sess-delta-0004", got)
	}
	if m.cards[m.cursor].SessionID == "sess-delta-0004" {
		t.Fatalf("test setup broken: unfiltered m.cards[%d] is already delta, cannot pin visible indexing", m.cursor)
	}
	// x while filtering is filter text, so arm through the production x
	// handler directly; y then flows through the production Update gate.
	if cmd := m.requestStopConfirm(); cmd != nil {
		t.Fatalf("requestStopConfirm must arm without dispatch, got a command")
	}
	if m.confirmStopID != "sess-delta-0004" {
		t.Fatalf("confirmStopID = %q, want visible sess-delta-0004 (unfiltered m.cards[%d] is %s)", m.confirmStopID, m.cursor, m.cards[m.cursor].SessionID)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("y must dispatch the confirmed stop")
	}
	msg := cmd()
	_, follow := m.Update(msg)
	_ = follow
	_, _, _, stops := acts.counts()
	if len(stops) != 1 || stops[0] != "sess-delta-0004" {
		t.Fatalf("stops = %v, want exactly [sess-delta-0004]", stops)
	}
}

// TestModel_FilterViewWidthBoundedAndSourceUnchanged proves AC-3 through
// production View: empty and no-match filters stay width-bounded at 40,
// 80, 120, and 200 columns in plain and styled modes while m.cards,
// counters, tailers, and stream content stay unchanged; clearing restores
// all cards. Filtering m.cards in place breaks the restore and fails.
func TestModel_FilterViewWidthBoundedAndSourceUnchanged(t *testing.T) {
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx-plain=%v", width, plain), func(t *testing.T) {
				m := newFixtureModel(t, width, 40, plain, 0)
				wantCards := len(m.cards)
				wantCounters := m.counters
				wantTailers := len(m.tailers)
				wantStream := strings.Join(m.stream.View(width, 10), "\n")
				wantIDs := make([]string, len(m.cards))
				for i, c := range m.cards {
					wantIDs[i] = c.SessionID
				}
				checkBounded := func(when string) {
					t.Helper()
					view := m.render()
					for i, line := range strings.Split(view, "\n") {
						if w := lipgloss.Width(line); w > width {
							t.Fatalf("%s width %d row %d is %d cells: %q", when, width, i, w, line)
						}
					}
					if plain && strings.Contains(view, "\x1b") {
						t.Fatalf("%s plain frame carries escapes", when)
					}
				}
				feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
				if len(m.visibleCards()) != wantCards {
					t.Fatalf("empty filter visible = %d, want %d", len(m.visibleCards()), wantCards)
				}
				checkBounded("empty filter")
				for _, r := range "zzz-no-such-xyz" {
					feedKey(m, tea.KeyPressMsg{Code: r, Text: string(r)})
				}
				if len(m.visibleCards()) != 0 {
					t.Fatalf("no-match visible = %d, want 0", len(m.visibleCards()))
				}
				checkBounded("no-match filter")
				if len(m.cards) != wantCards {
					t.Fatalf("m.cards mutated: %d, want %d", len(m.cards), wantCards)
				}
				for i, c := range m.cards {
					if c.SessionID != wantIDs[i] {
						t.Fatalf("m.cards order mutated at %d: %s, want %s", i, c.SessionID, wantIDs[i])
					}
				}
				if m.counters != wantCounters {
					t.Fatalf("counters mutated: %+v, want %+v", m.counters, wantCounters)
				}
				if len(m.tailers) != wantTailers {
					t.Fatalf("tailers mutated: %d, want %d", len(m.tailers), wantTailers)
				}
				if got := strings.Join(m.stream.View(width, 10), "\n"); got != wantStream {
					t.Fatalf("stream mutated:\n%s\nwant:\n%s", got, wantStream)
				}
				feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
				if len(m.visibleCards()) != wantCards {
					t.Fatalf("after escape visible = %d, want %d", len(m.visibleCards()), wantCards)
				}
				for i, c := range m.visibleCards() {
					if c.SessionID != wantIDs[i] {
						t.Fatalf("restore order mutated at %d: %s", i, c.SessionID)
					}
				}
				checkBounded("restored")
			})
		}
	}
}
