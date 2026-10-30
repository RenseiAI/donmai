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

// windowPlan is the placement windowBlocks renders and mouse hit-testing
// maps clicks through, so a click only lands on an on-screen session.
type windowPlan struct {
	lo, hi         int
	topMarker      bool
	topCount       int
	bottomMarker   bool
	bottomCount    int
	bottomPartial  bool // bare "↓ more": the shown block itself is cut
	partial        bool // only the selected block shows, possibly cut
	partialVisible int  // lines of the selected block shown when partial
	dropFrame      bool // partial view drops the block's top frame row
}

// planWindow computes the window over blocks of the given heights/counts
// that keeps selBlock visible within maxLines (see windowBlocks).
func planWindow(heights, counts []int, selBlock, maxLines int, framed bool) windowPlan {
	selBlock = max(0, min(selBlock, len(heights)-1))
	total := 0
	for _, h := range heights {
		total += h
	}
	if maxLines <= 0 || total <= maxLines {
		return windowPlan{lo: 0, hi: len(heights) - 1}
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
		if hi < len(heights)-1 {
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
			if hi < len(heights)-1 && cost(lo, hi+1) <= maxLines {
				hi++
				progress = true
			}
			if !progress {
				break
			}
		}
		p := windowPlan{lo: lo, hi: hi}
		if n := hidden(0, lo); n > 0 {
			p.topMarker, p.topCount = true, n
		}
		if n := hidden(hi+1, len(heights)); n > 0 {
			p.bottomMarker, p.bottomCount = true, n
		}
		return p
	}
	// The selected block alone exceeds the pane. Keep its identity row
	// visible first, then spend the remaining rows on content and markers.
	// A one-line pane cannot fit both a card row and an overflow marker.
	lines := heights[selBlock]
	dropFrame := framed && lines > 1
	if dropFrame {
		lines-- // the row's top frame carries no content
	}
	above := hidden(0, selBlock)
	below := hidden(selBlock+1, len(heights))
	markers := 0
	if above > 0 && maxLines > 1 {
		markers++
	}
	if below > 0 && maxLines-markers > 1 {
		markers++
	}
	visible := maxLines - markers
	cut := lines > visible
	if cut && below == 0 && maxLines-markers > 1 {
		markers++
		visible--
	}
	p := windowPlan{lo: selBlock, hi: selBlock, partial: true, dropFrame: dropFrame}
	p.partialVisible = min(visible, lines)
	if above > 0 && markers > 0 {
		p.topMarker, p.topCount = true, above
		markers--
	}
	if markers > 0 {
		switch {
		case below > 0:
			p.bottomMarker, p.bottomCount = true, below
		case cut:
			p.bottomMarker, p.bottomPartial = true, true
		}
	}
	return p
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
	heights := make([]int, len(blocks))
	for i, b := range blocks {
		heights[i] = displayLines(b)
	}
	p := planWindow(heights, counts, selBlock, maxLines, framed)
	var out []string
	if p.topMarker {
		out = append(out, overflowMarker(t, true, p.topCount, plain))
	}
	if p.partial {
		lines := strings.Split(blocks[p.lo], "\n")
		if p.dropFrame {
			lines = lines[1:]
		}
		out = append(out, lines[:p.partialVisible]...)
	} else {
		out = append(out, blocks[p.lo:p.hi+1]...)
	}
	if p.bottomMarker {
		n := p.bottomCount
		if p.bottomPartial {
			n = -1
		}
		out = append(out, overflowMarker(t, false, n, plain))
	}
	return strings.Join(out, "\n")
}

// hitRegion is one visible session's rectangle (x1/y1 exclusive); idx is
// its index in the visible card slice.
type hitRegion struct {
	idx    int
	x0, x1 int
	y0, y1 int
}

// gridHitRegions projects the cards renderGrid shows in a pane of
// maxLines rows from row topY into hit regions, through the same geometry
// and window plan: markers, gaps and partial rows produce no region.
func gridHitRegions(cards []SessionCard, selectedIdx, width, maxLines, topY int, plain bool) []hitRegion {
	if len(cards) == 0 || width <= 0 || maxLines <= 0 {
		return nil
	}
	perRow, cardW := gridGeometry(width, plain)
	if perRow == 0 { // compact list: one line per session, full width
		heights := make([]int, len(cards))
		counts := make([]int, len(cards))
		for i := range cards {
			heights[i], counts[i] = 1, 1
		}
		sel := selectedIdx
		if sel < 0 || sel >= len(cards) {
			sel = 0
		}
		p := planWindow(heights, counts, sel, maxLines, false)
		y := topY
		if p.topMarker {
			y++
		}
		if p.partial {
			if p.partialVisible <= 0 {
				return nil
			}
			return []hitRegion{{idx: p.lo, x0: 0, x1: width, y0: y, y1: y + p.partialVisible}}
		}
		var out []hitRegion
		for b := p.lo; b <= p.hi; b++ {
			out = append(out, hitRegion{idx: b, x0: 0, x1: width, y0: y, y1: y + 1})
			y++
		}
		return out
	}
	rows := (len(cards) + perRow - 1) / perRow
	h := cardHeight(plain)
	heights := make([]int, rows)
	counts := make([]int, rows)
	selRow := 0
	for r := 0; r < rows; r++ {
		start := r * perRow
		counts[r] = min(perRow, len(cards)-start)
		heights[r] = h
		if selectedIdx >= start && selectedIdx < start+counts[r] {
			selRow = r
		}
	}
	p := planWindow(heights, counts, selRow, maxLines, !plain)
	y := topY
	if p.topMarker {
		y++
	}
	var out []hitRegion
	for r := p.lo; r <= p.hi; r++ {
		rowH := h
		if p.partial {
			rowH = p.partialVisible
		}
		for c := 0; c < counts[r]; c++ {
			x0 := c * (cardW + cardGap)
			out = append(out, hitRegion{idx: r*perRow + c, x0: x0, x1: x0 + cardW, y0: y, y1: y + rowH})
		}
		y += rowH
		if p.partial {
			break
		}
	}
	return out
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
// elapsed, title and the attach hint when selected and live — truncated to
// the terminal width and windowed around the selection, so sessions stay
// reachable where no full card fits.
func renderCompactList(t theme.Theme, cards []SessionCard, selectedIdx, frame, width, maxLines int, plain bool, now time.Time) string {
	width = max(width, 1)
	blocks := make([]string, len(cards))
	counts := make([]int, len(cards))
	for i, c := range cards {
		marker := selectionMarker(i == selectedIdx, plain)
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
		if c.attachHintEligible(i == selectedIdx) {
			segs = append(segs, segment{" · " + attachHint, lipgloss.NewStyle().Foreground(t.TextTertiary)})
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
