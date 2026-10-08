package hostwatch

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

// gridGeometry returns how many cards share a grid row and each card's
// outer width for a terminal width. The card width depends on the terminal
// width alone — never on how many sessions exist or what they contain — so
// every card is the same size and columns stay put as sessions come and go.
// perRow is 0 below minCardWidth (the compact list takes over). Plain
// output stacks one card per row at the full width.
func gridGeometry(width int, plain bool) (perRow, cardW int) {
	if width < minCardWidth {
		return 0, 0
	}
	if plain {
		return 1, width
	}
	perRow = (width + cardGap) / (minCardWidth + cardGap)
	if perRow < 1 {
		perRow = 1
	}
	return perRow, (width - cardGap*(perRow-1)) / perRow
}

// renderGrid lays the session cards out as a responsive grid: sessions
// flow left-to-right across the full terminal width in snapshot order
// regardless of issue grouping, and issue and project context live inside
// each card, so no grid row is spent on group headings.
//
// selectedIdx is the flat card index of the focused card; -1 selects
// nothing. frame drives the status-dot pulse. plain drops color and frames
// for CI/pipe output. maxLines bounds the returned height in display lines
// (the caller passes its grid pane height); the window keeps the selected
// card visible and collapses hidden rows into "N more" markers.
// maxLines <= 0 renders everything. Every returned line is at most width
// cells wide.
func renderGrid(t theme.Theme, cards []SessionCard, selectedIdx, frame, width, maxLines int, plain bool, now time.Time) string {
	if len(cards) == 0 {
		empty := truncateWidth("No active sessions for this scope.", width)
		if plain {
			return empty
		}
		return lipgloss.NewStyle().Foreground(t.TextTertiary).Render(empty)
	}

	perRow, cardW := gridGeometry(width, plain)
	if perRow == 0 {
		return renderCompactList(t, cards, selectedIdx, frame, width, maxLines, plain, now)
	}

	var blocks []string
	var counts []int
	selRow := 0
	gap := strings.Repeat(" ", cardGap)
	for start := 0; start < len(cards); start += perRow {
		end := min(start+perRow, len(cards))
		if selectedIdx >= start && selectedIdx < end {
			selRow = len(blocks)
		}
		rendered := make([][]string, 0, end-start)
		for i := start; i < end; i++ {
			rendered = append(rendered, strings.Split(renderCard(t, cards[i], frame, i == selectedIdx, plain, now, cardW), "\n"))
		}
		// Every card has the same height, so line i of the row is line i
		// of each card side by side.
		lines := make([]string, len(rendered[0]))
		for li := range lines {
			parts := make([]string, len(rendered))
			for ci, card := range rendered {
				parts[ci] = card[li]
			}
			lines[li] = strings.Join(parts, gap)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
		counts = append(counts, end-start)
	}
	return windowBlocks(t, blocks, counts, selRow, maxLines, plain, !plain)
}

// windowBlocks trims rendered blocks to at most maxLines display lines,
// keeping the selected block visible: it expands outward from the selection
// and collapses hidden blocks above/below into one marker line each
// ("↑ N more" / "↓ N more", counting hidden sessions) so overflow selection
// stays visible and truncation is explicit. maxLines <= 0 renders
// everything. A selected block taller than the budget shows its head —
// after its frame row when framed, so the identity row comes first — with
// a "↓ more" marker.
func windowBlocks(t theme.Theme, blocks []string, counts []int, selBlock, maxLines int, plain, framed bool) string {
	if len(blocks) == 0 {
		return ""
	}
	selBlock = max(0, min(selBlock, len(blocks)-1))
	heights := make([]int, len(blocks))
	total := 0
	for i, b := range blocks {
		heights[i] = displayLines(b)
		total += heights[i]
	}
	if maxLines <= 0 || total <= maxLines {
		return strings.Join(blocks, "\n")
	}

	hidden := func(from, to int) int {
		n := 0
		for i := from; i < to; i++ {
			n += counts[i]
		}
		return n
	}
	cost := func(lo, hi int) int {
		n := 0
		for i := lo; i <= hi; i++ {
			n += heights[i]
		}
		if lo > 0 {
			n++
		}
		if hi < len(blocks)-1 {
			n++
		}
		return n
	}
	lo, hi := selBlock, selBlock
	if cost(lo, hi) <= maxLines {
		for {
			progress := false
			if lo > 0 && cost(lo-1, hi) <= maxLines {
				lo--
				progress = true
			}
			if hi < len(blocks)-1 && cost(lo, hi+1) <= maxLines {
				hi++
				progress = true
			}
			if !progress {
				break
			}
		}
		var out []string
		if n := hidden(0, lo); n > 0 {
			out = append(out, overflowMarker(t, true, n, plain))
		}
		out = append(out, blocks[lo:hi+1]...)
		if n := hidden(hi+1, len(blocks)); n > 0 {
			out = append(out, overflowMarker(t, false, n, plain))
		}
		return strings.Join(out, "\n")
	}

	// The selected block alone exceeds the pane. Keep its identity row
	// visible first, then spend the remaining rows on content and markers.
	// A one-line pane cannot fit both a card row and an overflow marker.
	lines := strings.Split(blocks[selBlock], "\n")
	if framed && len(lines) > 1 {
		lines = lines[1:] // the row's top frame carries no content
	}
	above := hidden(0, selBlock)
	below := hidden(selBlock+1, len(blocks))
	markers := 0
	if above > 0 && maxLines > 1 {
		markers++
	}
	if below > 0 && maxLines-markers > 1 {
		markers++
	}
	visible := maxLines - markers
	partial := len(lines) > visible
	if partial && below == 0 && maxLines-markers > 1 {
		markers++
		visible--
	}
	visible = min(visible, len(lines))
	var out []string
	if above > 0 && markers > 0 {
		out = append(out, overflowMarker(t, true, above, plain))
		markers--
	}
	out = append(out, lines[:visible]...)
	if markers > 0 {
		if below > 0 {
			out = append(out, overflowMarker(t, false, below, plain))
		} else if partial {
			out = append(out, overflowMarker(t, false, -1, plain))
		}
	}
	return strings.Join(out, "\n")
}

// overflowMarker renders a truncation marker. n < 0 (unknown count, e.g. a
// partially shown block) renders a bare arrow marker.
func overflowMarker(t theme.Theme, above bool, n int, plain bool) string {
	var s string
	switch {
	case above && n >= 0:
		s = fmt.Sprintf("↑ %d more", n)
	case above:
		s = "↑ more"
	case n >= 0:
		s = fmt.Sprintf("↓ %d more", n)
	default:
		s = "↓ more"
	}
	if plain {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.TextTertiary).Render(s)
}

// renderCompactList is the tiny-terminal fallback (width < minCardWidth):
// one line per session — selection marker, status dot, issue id, state,
// elapsed and title — truncated to the terminal width and windowed around
// the selection, so sessions stay reachable where no full card fits.
func renderCompactList(t theme.Theme, cards []SessionCard, selectedIdx, frame, width, maxLines int, plain bool, now time.Time) string {
	width = max(width, 1)
	blocks := make([]string, len(cards))
	counts := make([]int, len(cards))
	for i, c := range cards {
		marker := " "
		if i == selectedIdx {
			marker = ">"
			if !plain {
				marker = "▸"
			}
		}
		state := c.displayState() + " " + c.elapsedText(now)
		segs := []segment{
			{marker + " ", lipgloss.NewStyle().Bold(true).Foreground(t.Accent)},
			{c.statusDot(frame) + " ", lipgloss.NewStyle().Foreground(statusColor(t, c))},
			{c.displayID(), lipgloss.NewStyle().Bold(true).Foreground(t.TextPrimary)},
			{" " + state, lipgloss.NewStyle().Foreground(t.TextSecondary)},
		}
		if title := c.title(); title != "" {
			segs = append(segs, segment{" · " + title, lipgloss.NewStyle().Foreground(t.TextTertiary)})
		}
		blocks[i] = truncateWidth(joinSegments(segs, plain), width)
		counts[i] = 1
	}
	sel := selectedIdx
	if sel < 0 || sel >= len(blocks) {
		sel = 0
	}
	return windowBlocks(t, blocks, counts, sel, maxLines, plain, false)
}

// displayLines returns the number of display lines in a rendered block.
func displayLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// fitLines returns exactly n lines: s's lines, truncated to n or padded
// with empty lines. A pane allocated n rows always occupies n rows, so the
// panes below it never move when it is sparse.
func fitLines(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	var lines []string
	if s != "" {
		lines = strings.Split(s, "\n")
	}
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}
