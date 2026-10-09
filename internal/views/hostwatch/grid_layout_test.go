package hostwatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

// acceptanceWidths are the terminal widths every layout rule is checked at.
var acceptanceWidths = []int{80, 120, 160, 200, 250}

// TestGridGeometry_FillsWidthWithEqualCards: the card width is a function
// of the terminal width only; cards fill the row (no room for another card)
// and never overflow it.
func TestGridGeometry_FillsWidthWithEqualCards(t *testing.T) {
	for width := minCardWidth; width <= 320; width++ {
		perRow, cardW := gridGeometry(width, false)
		used := perRow*cardW + cardGap*(perRow-1)
		if perRow < 1 || cardW < minCardWidth || used > width {
			t.Fatalf("width %d: perRow=%d cardW=%d used=%d", width, perRow, cardW, used)
		}
		if (perRow+1)*minCardWidth+perRow*cardGap <= width {
			t.Fatalf("width %d: another card would fit beside %d", width, perRow)
		}
		if width-used >= perRow {
			t.Fatalf("width %d: %d cells unused by %d cards", width, width-used, perRow)
		}
	}
	if perRow, _ := gridGeometry(minCardWidth-1, false); perRow != 0 {
		t.Fatalf("below the minimum card width the compact list takes over, got perRow=%d", perRow)
	}
}

// TestRenderGrid_EqualCardWidthsAndHeights pins alignment: in every row of
// the styled grid, every card has the same width and height, and every
// line has its frame characters at exactly the same cell columns — however
// wide the content (CJK, emoji clusters) or long the title, and whichever
// card is selected.
func TestRenderGrid_EqualCardWidthsAndHeights(t *testing.T) {
	cards := fixtureCards()
	for _, width := range acceptanceWidths {
		perRow, cardW := gridGeometry(width, false)
		for _, selected := range []int{0, 2, 5} {
			out := renderGrid(theme.DefaultTheme(), cards, selected, 0, width, 0, false, fixtureNow)
			lines := strings.Split(out, "\n")
			if len(lines)%cardHeight(false) != 0 {
				t.Fatalf("width %d: %d lines is not a whole number of %d-row cards", width, len(lines), cardHeight(false))
			}
			for li, line := range lines {
				row := li / cardHeight(false)
				inRow := min(perRow, len(cards)-row*perRow)
				var want []int
				for k := 0; k < inRow; k++ {
					left := k * (cardW + cardGap)
					want = append(want, left, left+cardW-1)
				}
				if got := frameColumns(line); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("width %d selected %d line %d: frame columns %v, want %v:\n%s", width, selected, li, got, want, ansi.Strip(line))
				}
				if got, wantW := ansi.StringWidth(line), inRow*cardW+(inRow-1)*cardGap; got != wantW {
					t.Fatalf("width %d line %d: width %d, want %d: %q", width, li, got, wantW, ansi.Strip(line))
				}
			}
		}
	}
}

// TestModel_SelectionDoesNotMoveCards pins the selection geometry: moving
// the selection forward and back keeps every card's identity at the same
// terminal row and column, and peer cards share a baseline.
func TestModel_SelectionDoesNotMoveCards(t *testing.T) {
	ids := []string{"ENG-1284", "ENG-1290", "ENG-1301", "ENG-1310", "ENG-1311", "ENG-1312"}
	for _, width := range []int{120, 160, 250} {
		m := newFixtureModel(t, width, 70, false, 0)
		baseline := map[string][2]int{}
		for _, id := range ids {
			r, c := cellIndex(m.render(), id)
			baseline[id] = [2]int{r, c}
		}
		perRow, _ := gridGeometry(width, false)
		if baseline["ENG-1284"][0] != baseline["ENG-1290"][0] || (perRow >= 3 && baseline["ENG-1290"][0] != baseline["ENG-1301"][0]) {
			t.Fatalf("width %d: peer cards do not share a baseline: %v", width, baseline)
		}
		keys := []string{"j", "j", "j", "j", "j", "k", "k", "k", "k", "k"}
		for step, key := range keys {
			m.Update(keyMsg(key))
			view := m.render()
			for _, id := range ids {
				r, c := cellIndex(view, id)
				if got := [2]int{r, c}; got != baseline[id] {
					t.Fatalf("width %d after %d keys (cursor %d): %s moved from %v to %v", width, step+1, m.cursor, id, baseline[id], got)
				}
			}
		}
	}
}

// TestModel_OverflowSelectionStaysVisible: with more sessions than fit,
// walking the selection across every session keeps the selected card's
// identity on screen with the selection marker, at every acceptance width
// and at short heights, without the frame growing past the terminal.
func TestModel_OverflowSelectionStaysVisible(t *testing.T) {
	cards := fixtureCards()
	for _, plain := range []bool{false, true} {
		marker := "▸"
		if plain {
			marker = ">"
		}
		for _, width := range append([]int{40}, acceptanceWidths...) {
			for _, height := range []int{8, 12, 20, 40} {
				m := newFixtureModel(t, width, height, plain, 0)
				for i := range cards {
					view := m.render()
					if got := len(strings.Split(view, "\n")); got != height {
						t.Fatalf("plain=%v %dx%d: frame has %d rows", plain, width, height, got)
					}
					id := cards[i].displayID()
					found := false
					for _, line := range strings.Split(ansi.Strip(view), "\n") {
						if strings.Contains(line, marker) && strings.Contains(line, id) {
							found = true
						}
					}
					if !found {
						t.Fatalf("plain=%v %dx%d: selected %s (%d) not visible with its marker:\n%s", plain, width, height, id, i, ansi.Strip(view))
					}
					m.Update(keyMsg("j"))
				}
			}
		}
	}
}

// TestModel_PanesKeepAllocatedRows pins the split geometry with a sparse
// grid: the stream title sits right below the grid's allocated rows and the
// help row on the last terminal row, through keyboard adjust and reset and
// across a resize, which keeps the chosen ratio.
func TestModel_PanesKeepAllocatedRows(t *testing.T) {
	for _, plain := range []bool{false, true} {
		m := New(Options{Plain: plain, HostLabel: "host-a", Now: func() time.Time { return fixtureNow }})
		m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
		m.applySnapshot(Snapshot{Cards: fixtureCards()[:1], Counters: fixtureCounters()})
		check := func(when string, height, wantTitleRow int) {
			t.Helper()
			lines := strings.Split(ansi.Strip(m.render()), "\n")
			if len(lines) != height {
				t.Fatalf("plain=%v %s: %d rows, want %d", plain, when, len(lines), height)
			}
			if !strings.HasPrefix(lines[wantTitleRow], "session stream") {
				t.Fatalf("plain=%v %s: stream title not at row %d:\n%s", plain, when, wantTitleRow, strings.Join(lines, "\n"))
			}
			if !strings.HasPrefix(lines[height-1], "↑↓/jk select") {
				t.Fatalf("plain=%v %s: help not on the last row: %q", plain, when, lines[height-1])
			}
		}
		check("default 50/50", 40, 1+19) // header + round(0.5*38)
		m.Update(keyMsg("]"))
		m.Update(keyMsg("]"))
		check("after ] ]", 40, 1+27) // round(0.7*38)
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		check("resized keeps 0.7", 30, 1+20) // round(0.7*28)
		m.Update(keyMsg("0"))
		check("reset", 30, 1+14) // round(0.5*28)
		m.Update(keyMsg("["))
		m.Update(keyMsg("["))
		m.Update(keyMsg("["))
		m.Update(keyMsg("["))
		check("after [ x4 clamps at 0.2", 30, 1+6) // round(0.2*28)
	}
}

// TestModel_FrameFitsTerminal: at every acceptance width and at very short
// heights, the frame has exactly the terminal's rows (the header first) and
// no row is wider than the terminal.
func TestModel_FrameFitsTerminal(t *testing.T) {
	for _, plain := range []bool{false, true} {
		for _, width := range append([]int{20, 40}, acceptanceWidths...) {
			for _, height := range []int{1, 2, 3, 4, 6, 9, 14, 40} {
				for _, detail := range []bool{false, true} {
					m := newFixtureModel(t, width, height, plain, 1)
					m.detail = detail
					view := m.render()
					lines := strings.Split(view, "\n")
					if len(lines) != height {
						t.Fatalf("plain=%v detail=%v %dx%d: %d rows", plain, detail, width, height, len(lines))
					}
					if !strings.Contains(ansi.Strip(lines[0]), "build-host") {
						t.Fatalf("plain=%v %dx%d: header not first: %q", plain, width, height, lines[0])
					}
					for i, line := range lines {
						if w := lipgloss.Width(line); w > width {
							t.Fatalf("plain=%v detail=%v %dx%d row %d: width %d: %q", plain, detail, width, height, i, w, ansi.Strip(line))
						}
					}
				}
			}
		}
	}
}

// TestModel_PlainFrameCarriesNoEscapes: plain output (pipes, CI, --plain)
// never contains a terminal escape, including from the stream pane and from
// agent output that itself carries escapes.
func TestModel_PlainFrameCarriesNoEscapes(t *testing.T) {
	m := newFixtureModel(t, 120, 40, true, 0)
	m.stream.SetFollowing(false)
	for _, detail := range []bool{false, true} {
		m.detail = detail
		if view := m.render(); strings.Contains(view, "\x1b") {
			t.Fatalf("detail=%v: plain frame carries an escape: %q", detail, view)
		}
	}
}
