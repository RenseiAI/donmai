package hostwatch

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

// renderGrid lays the session cards out grouped by issue identifier, with
// each group's cards flowing left-to-right across the available width — the
// FIG 2.0 "agent-session cards clustered around issues" vibe, TUI-formatted.
//
// selectedIdx is the flat index (into the same order returned by
// Source.Snapshot) of the focused card; -1 selects nothing. frame drives
// the status-dot pulse. plain drops color/boxes for CI/pipe output.
//
// The returned string is bounded to width columns; the caller allocates the
// vertical space (the grid is intentionally height-agnostic — the model
// clamps it).
func renderGrid(t theme.Theme, cards []SessionCard, selectedIdx, frame, width int, plain bool, now time.Time) string {
	if len(cards) == 0 {
		empty := "No active sessions for this scope."
		if plain {
			return empty
		}
		return lipgloss.NewStyle().Foreground(t.TextTertiary).Render(empty)
	}

	perRow := width / (cardWidth + 2)
	if perRow < 1 {
		perRow = 1
	}

	// Group cards by issue identifier, preserving the snapshot order
	// (already sorted by group key then session id).
	type group struct {
		key   string
		cards []int // flat indices into cards
	}
	var groups []group
	idxByKey := map[string]int{}
	for i, c := range cards {
		k := c.GroupKey()
		gi, ok := idxByKey[k]
		if !ok {
			idxByKey[k] = len(groups)
			groups = append(groups, group{key: k})
			gi = len(groups) - 1
		}
		groups[gi].cards = append(groups[gi].cards, i)
	}

	var sections []string
	for _, g := range groups {
		// Group heading: issue id + the work type of its first card.
		head := g.key
		if len(g.cards) > 0 {
			if wt := cards[g.cards[0]].WorkType; wt != "" {
				head = g.key + "  " + wt
			}
		}
		if plain {
			sections = append(sections, head)
		} else {
			sections = append(sections, theme.SectionTitle().Render(head))
		}

		// Flow this group's cards into rows of perRow.
		for start := 0; start < len(g.cards); start += perRow {
			end := start + perRow
			if end > len(g.cards) {
				end = len(g.cards)
			}
			var rowCards []string
			for _, flatIdx := range g.cards[start:end] {
				selected := flatIdx == selectedIdx
				rowCards = append(rowCards, renderCard(t, cards[flatIdx], frame, selected, plain, now))
			}
			if plain {
				sections = append(sections, strings.Join(rowCards, "\n"))
			} else {
				sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Top, rowCards...))
			}
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}
