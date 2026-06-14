package hostwatch

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/format"
	"github.com/RenseiAI/tui-components/theme"
)

// cardWidth is the target render width of one session card. Cards lay out
// in a responsive grid: as many per row as the terminal width allows.
const cardWidth = 44

// animFrames is the status-dot pulse animation (the FIG 2.0 pulsing dot
// translated to a TUI frame swap). A running session cycles these; a
// terminal/idle session shows a static glyph.
var animFrames = []string{"●", "◉", "○", "◉"} // ● ◉ ○ ◉

// statusColor maps a card's effective status to a theme status color. The
// "health glow" of the marketing specimen becomes a colored status dot + a
// colored left border bar. Colors are read from the theme exclusively.
func statusColor(t theme.Theme, card SessionCard) color.Color {
	switch {
	case card.Errored:
		return t.StatusError
	case strings.EqualFold(card.DaemonState, "completed"):
		return t.StatusSuccess
	case strings.EqualFold(card.DaemonState, "failed"),
		strings.EqualFold(card.DaemonState, "terminated"):
		return t.StatusError
	case strings.EqualFold(card.DaemonState, "starting"):
		return t.StatusWarning
	default: // running
		return t.Accent
	}
}

// renderCard renders a single session card: a colored status dot + issue
// header, a role/provider/state chip line, a 4-tile metric line (duration /
// tool-calls / cost / turns), and a current-tool ticker. Colors are read
// from the theme exclusively (no hardcoded hexes). frame drives the dot
// pulse; selected draws a bright border. When plain is true, color and box
// drawing are dropped for a pipe-/CI-friendly rendering.
func renderCard(t theme.Theme, card SessionCard, frame int, selected, plain bool, now time.Time) string {
	dot := animFrames[0]
	if isLiveState(card.DaemonState) {
		dot = animFrames[frame%len(animFrames)]
	}

	header := card.IssueIdentifier
	if header == "" {
		header = shortID(card.SessionID)
	}
	work := card.WorkType
	if work == "" {
		work = "—"
	}

	provider := card.Provider
	if provider == "" {
		provider = "agent"
	}
	st := card.DaemonState
	if st == "" {
		st = "running"
	}
	chips := fmt.Sprintf("%s · %s · %s", card.roleBadge(), provider, st)

	metrics := fmt.Sprintf("⏱ %s   ⌨ %d   %s   ↻ %d",
		format.Duration(card.ageSeconds(now)),
		card.ToolCalls,
		costStr(card.CostUsd),
		card.NumTurns,
	)

	ticker := card.LastActivity
	if ticker == "" {
		ticker = card.LastTool
	}
	if ticker == "" {
		ticker = card.CurrentStep
	}
	ticker = truncateRunes(ticker, cardWidth-2)

	if plain {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s  %s\n", dot, header, work)
		fmt.Fprintf(&b, "  %s\n", chips)
		fmt.Fprintf(&b, "  %s\n", metrics)
		if ticker != "" {
			fmt.Fprintf(&b, "  %s\n", ticker)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	sc := statusColor(t, card)
	dotStyle := lipgloss.NewStyle().Foreground(sc)
	headStyle := lipgloss.NewStyle().Bold(true).Foreground(t.TextPrimary)
	workStyle := lipgloss.NewStyle().Foreground(t.TextSecondary)
	chipStyle := lipgloss.NewStyle().Foreground(t.TextSecondary)
	metricStyle := lipgloss.NewStyle().Foreground(t.TextPrimary)
	tickStyle := lipgloss.NewStyle().Foreground(t.TextTertiary)

	lines := []string{
		dotStyle.Render(dot) + " " + headStyle.Render(header) + "  " + workStyle.Render(work),
		chipStyle.Render(chips),
		metricStyle.Render(metrics),
	}
	if ticker != "" {
		lines = append(lines, tickStyle.Render(ticker))
	}
	body := lipgloss.JoinVertical(lipgloss.Left, lines...)

	borderColor := t.SurfaceBorder
	if selected {
		borderColor = t.SurfaceBorderBright
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), false, false, false, true).
		BorderForeground(sc).
		PaddingLeft(1).
		Width(cardWidth)
	// Selected cards get a full bright border ring for affordance.
	if selected {
		box = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(0, 1).
			Width(cardWidth)
	}
	return box.Render(body)
}

// roleBadge derives a short role label from the work type, falling back to
// the current step. Matches the specimen's role badge.
func (c SessionCard) roleBadge() string {
	switch strings.ToLower(c.WorkType) {
	case "development", "develop":
		return "impl"
	case "qa", "review", "acceptance":
		return "review"
	case "research", "backlog-writer":
		return "planner"
	case "kg-extraction":
		return "kg"
	case "":
		if c.CurrentStep != "" {
			return c.CurrentStep
		}
		return "agent"
	default:
		return c.WorkType
	}
}

// ageSeconds returns the session age in whole seconds from StartedAt.
func (c SessionCard) ageSeconds(now time.Time) int {
	if c.StartedAtUnixMs <= 0 {
		return 0
	}
	start := time.UnixMilli(c.StartedAtUnixMs)
	d := now.Sub(start)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// costStr formats the running cost using the format helper's *float64 API.
func costStr(usd float64) string {
	v := usd
	return format.Cost(&v)
}

// isLiveState reports whether a daemon state should animate (running-ish).
func isLiveState(s string) bool {
	switch strings.ToLower(s) {
	case "", "running", "starting":
		return true
	default:
		return false
	}
}

// shortID returns the first 8 chars of an id for compact display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncateRunes shortens s to at most n runes, appending an ellipsis when
// truncated. n<=0 returns "".
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
