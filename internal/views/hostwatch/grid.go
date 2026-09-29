package hostwatch

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

// minCardWidth is the minimum terminal width (columns) at which full
// session cards render. Below it the grid falls back to a compact
// one-line-per-session list so sessions stay reachable on tiny terminals.
const minCardWidth = cardWidth

// renderGrid lays the session cards out as a responsive grid: active
// sessions flow left-to-right across the full terminal width regardless of
// issue grouping — cards from different issues share rows. Issue and
// project context live inside each card, so no grid row is spent on group
// headings.
//
// selectedIdx is the flat card index of the focused card; -1 selects
// nothing. frame drives the status-dot pulse. plain drops color/boxes for
// CI/pipe output. maxLines bounds the returned height in display lines
// (the caller passes its grid pane height); the window keeps the selected
// card visible and collapses hidden rows into "N more" markers.
// maxLines <= 0 renders everything.
//
// The returned string is bounded to width columns.
func renderGrid(t theme.Theme, cards []SessionCard, selectedIdx, frame, width, maxLines int, plain bool, now time.Time) string {
	if len(cards) == 0 {
		empty := "No active sessions for this scope."
		if plain {
			return empty
		}
		return lipgloss.NewStyle().Foreground(t.TextTertiary).Render(empty)
	}

	if width < minCardWidth {
		return renderCompactList(t, cards, selectedIdx, width, maxLines, plain)
	}

	perRow := width / (cardWidth + 2)
	if perRow < 1 {
		perRow = 1
	}

	// Flat rows across the full width: every row packs the next perRow
	// sessions whatever their issue, so the grid fills the terminal
	// instead of starting a new row per issue group.
	var rows [][]int
	for start := 0; start < len(cards); start += perRow {
		end := start + perRow
		if end > len(cards) {
			end = len(cards)
		}
		var idx []int
		for i := start; i < end; i++ {
			idx = append(idx, i)
		}
		rows = append(rows, idx)
	}

	blocks := make([]string, len(rows))
	counts := make([]int, len(rows))
	for r, idx := range rows {
		var parts []string
		for _, i := range idx {
			parts = append(parts, renderCard(t, cards[i], frame, i == selectedIdx, plain, now))
		}
		if plain {
			blocks[r] = strings.Join(parts, "\n")
		} else {
			// Normalize the row to its tallest card so cards with and
			// without tickers (and selected vs unselected borders) share
			// one row height and JoinHorizontal keeps them aligned.
			h := 0
			for _, part := range parts {
				if n := displayLines(part); n > h {
					h = n
				}
			}
			for i, part := range parts {
				if n := displayLines(part); n < h {
					filler := strings.Repeat("\n", h-n)
					parts[i] = part + filler
				}
			}
			blocks[r] = lipgloss.JoinHorizontal(lipgloss.Top, parts...)
		}
		counts[r] = len(idx)
	}
	return windowBlocks(t, blocks, counts, rowOf(rows, selectedIdx), maxLines, plain)
}

// rowOf returns the row containing the selected flat card index, clamped
// into range. -1 (nothing selected) anchors the top.
func rowOf(rows [][]int, selectedIdx int) int {
	for r, idx := range rows {
		for _, i := range idx {
			if i == selectedIdx {
				return r
			}
		}
	}
	return 0
}

// windowBlocks trims rendered blocks to at most maxLines display lines,
// keeping the selected block visible: it expands outward from the selection
// and collapses hidden blocks above/below into one marker line each
// ("↑ N more" / "↓ N more", counting hidden sessions) so overflow selection
// stays visible and truncation is explicit. maxLines <= 0 renders
// everything. A selected block taller than the budget shows its head (never
// dropped entirely) with a "↓ more" marker.
func windowBlocks(t theme.Theme, blocks []string, counts []int, selBlock, maxLines int, plain bool) string {
	if len(blocks) == 0 {
		return ""
	}
	if selBlock < 0 {
		selBlock = 0
	}
	if selBlock >= len(blocks) {
		selBlock = len(blocks) - 1
	}
	heights := make([]int, len(blocks))
	total := 0
	for i, b := range blocks {
		heights[i] = displayLines(b)
		total += heights[i]
	}
	if maxLines <= 0 || total <= maxLines {
		return strings.Join(blocks, "\n")
	}

	hiddenAbove := func(lo int) int {
		n := 0
		for i := 0; i < lo; i++ {
			n += counts[i]
		}
		return n
	}
	hiddenBelow := func(hi int) int {
		n := 0
		for i := hi + 1; i < len(blocks); i++ {
			n += counts[i]
		}
		return n
	}

	// Reserve marker lines for sides that overflow; markers that end up
	// unneeded (all blocks on that side fit) are dropped when assembling.
	budget := maxLines
	if selBlock > 0 {
		budget--
	}
	if selBlock < len(blocks)-1 {
		budget--
	}
	if budget < 1 {
		budget = 1
	}

	lo, hi := selBlock, selBlock
	used := heights[selBlock]
	if used <= budget {
		for {
			progress := false
			if lo > 0 && used+heights[lo-1] <= budget {
				lo--
				used += heights[lo]
				progress = true
			}
			if hi < len(blocks)-1 && used+heights[hi+1] <= budget {
				hi++
				used += heights[hi]
				progress = true
			}
			if !progress {
				break
			}
		}
		var out []string
		if n := hiddenAbove(lo); n > 0 {
			out = append(out, overflowMarker(t, true, n, plain))
		}
		for i := lo; i <= hi; i++ {
			out = append(out, blocks[i])
		}
		if n := hiddenBelow(hi); n > 0 {
			out = append(out, overflowMarker(t, false, n, plain))
		}
		return strings.Join(out, "\n")
	}

	// The selected block alone exceeds the budget: show its head and mark
	// the truncation. Top marker only when blocks above are hidden.
	lines := strings.Split(blocks[selBlock], "\n")
	head := lines
	if len(head) > budget {
		head = head[:budget]
	}
	var out []string
	if n := hiddenAbove(selBlock); n > 0 {
		out = append(out, overflowMarker(t, true, n, plain))
	}
	out = append(out, strings.Join(head, "\n"))
	out = append(out, overflowMarker(t, false, -1, plain))
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
// one line per session — selection marker, status dot, issue id, work type
// — truncated to the terminal width and windowed to maxLines around the
// selection, so sessions stay reachable where no full card fits.
func renderCompactList(t theme.Theme, cards []SessionCard, selectedIdx, width, maxLines int, plain bool) string {
	if width < 1 {
		width = 1
	}
	blocks := make([]string, len(cards))
	counts := make([]int, len(cards))
	for i, c := range cards {
		marker := "  "
		if i == selectedIdx {
			marker = "> "
		}
		header := c.IssueIdentifier
		if header == "" {
			header = shortID(c.SessionID)
		}
		work := c.WorkType
		if work == "" {
			work = "—"
		}
		// Budget the content first (plain widths), then style: the fixed
		// prefix takes 4 cells (marker + dot + space), the "  " separator 2.
		fixed := 6
		avail := width - fixed
		if avail < 1 {
			avail = 1
		}
		header = truncateWidth(header, avail)
		work = truncateWidth(work, avail-lipgloss.Width(header)-2)
		switch {
		case work == "" && avail-lipgloss.Width(header) > 0:
			// Header alone fills the line; drop the separator.
			line := marker + animDot(c.DaemonState, 0) + " " + header
			blocks[i] = truncateWidth(line, width)
		case plain:
			blocks[i] = truncateWidth(marker+animDot(c.DaemonState, 0)+" "+header+"  "+work, width)
		default:
			head := lipgloss.NewStyle().Bold(true).Foreground(t.TextPrimary).Render(header)
			wk := lipgloss.NewStyle().Foreground(t.TextSecondary).Render(work)
			blocks[i] = marker + animDot(c.DaemonState, 0) + " " + head + "  " + wk
		}
		counts[i] = 1
	}
	sel := selectedIdx
	if sel < 0 || sel >= len(blocks) {
		sel = 0
	}
	return windowBlocks(t, blocks, counts, sel, maxLines, plain)
}

// animDot returns the status glyph for a daemon state at the given frame.
func animDot(state string, frame int) string {
	if isLiveState(state) {
		return animFrames[frame%len(animFrames)]
	}
	return animFrames[0]
}

// displayLines returns the number of display lines in a rendered block.
func displayLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
