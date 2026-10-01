package hostwatch

import (
	"fmt"
	"image/color"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

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
	case card.isHeld():
		return t.TextTertiary
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
// header, a labeled harness/model/provider/state line, a labeled metric
// line (elapsed / tool-calls / cost / turns), a freshness line
// (heartbeat / output / work) and a current-tool ticker. Every field is
// named; missing data renders as "unknown" or "not reported", never an
// invented running state or a zero. Colors are read from the theme
// exclusively (no hardcoded hexes). frame drives the dot pulse; selected
// draws a bright border. When plain is true, color and box drawing are
// dropped for a pipe-/CI-friendly rendering. Layout only flows the lines;
// parallel layout work owns the grid/split geometry.
func renderCard(t theme.Theme, card SessionCard, frame int, selected, plain bool, now time.Time) string {
	dot := animFrames[0]
	if card.isHeld() {
		dot = "○"
	}
	if isLiveState(card.DaemonState) {
		dot = animFrames[frame%len(animFrames)]
	}

	header := card.IssueIdentifier
	if header == "" {
		header = shortID(card.SessionID)
	}
	work := card.WorkType
	if work == "" {
		work = "unknown"
	}

	// Dispatch composition and observed serving identity are independent of
	// the requested model and harness. Missing components stay explicit.
	agentCard := "Agent card " + safeIdentityText(card.AgentCardName)
	cardID := "Card ID " + safeIdentityText(card.AgentCardID)
	modelIdentity := "Model identity " + safeIdentityText(card.ActualModel)
	actualProvider := "Actual provider " + safeIdentityText(card.ActualModelProvider)
	modelVersion := "Model version " + safeIdentityText(card.ActualModelVersion)
	// Plain cards also reserve one physical row per identity component.
	agentCard = truncateWidth(agentCard, cardWidth-2)
	cardID = truncateWidth(cardID, cardWidth-2)
	modelIdentity = truncateWidth(modelIdentity, cardWidth-2)
	actualProvider = truncateWidth(actualProvider, cardWidth-2)
	modelVersion = truncateWidth(modelVersion, cardWidth-2)
	identity := fmt.Sprintf("harness %s · model %s",
		unknownIfEmpty(card.Harness),
		unknownIfEmpty(card.Model))
	state := fmt.Sprintf("provider %s · state %s",
		unknownIfEmpty(card.modelProvider()),
		card.displayState())
	// Scope stays visible inside the card. Appending it after the identity
	// fields would truncate it at ordinary card widths.
	scope := ""
	if card.ProjectName != "" {
		scope = "project " + card.ProjectName
	}
	if card.IssueIdentifier != "" {
		if scope != "" {
			scope += " · "
		}
		scope += "issue " + card.IssueIdentifier
	}

	metrics := fmt.Sprintf("elapsed %s · tools %s",
		elapsedStr(card, now),
		countStr(card.Observed, card.ToolCalls))
	cost := fmt.Sprintf("cost %s · turns %s",
		costStr(card),
		countStr(card.MetricsReported, card.NumTurns))

	fresh := fmt.Sprintf("heartbeat %s · output %s",
		freshStr(card.heartbeatTime(), now),
		freshStr(card.LastOutputAt, now))
	workFresh := "work " + freshStr(card.LastWorkAt, now)

	ticker := card.LastActivity
	if ticker == "" {
		ticker = card.LastTool
	}
	if card.isHeld() {
		ticker = ""
	} else if ticker == "" {
		ticker = card.CurrentStep
	}
	ticker = truncateRunes(ticker, cardWidth-2)

	if plain {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s  %s\n", dot, header, work)
		if scope != "" {
			fmt.Fprintf(&b, "  %s\n", scope)
		}
		for _, line := range []string{agentCard, cardID, modelIdentity, actualProvider, modelVersion} {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		fmt.Fprintf(&b, "  %s\n", identity)
		fmt.Fprintf(&b, "  %s\n", state)
		if card.isHeld() {
			for _, detail := range strings.Split(heldDetails(card), "\n") {
				fmt.Fprintf(&b, "  %s\n", detail)
			}
		} else {
			fmt.Fprintf(&b, "  %s\n", metrics)
			fmt.Fprintf(&b, "  %s\n", cost)
			fmt.Fprintf(&b, "  %s\n", fresh)
			fmt.Fprintf(&b, "  %s\n", workFresh)
		}
		if ticker != "" {
			fmt.Fprintf(&b, "  %s\n", ticker)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	// Budget the body to the inner content width (cardWidth minus the
	// left border + padding, or the full ring for selected cards) using
	// display widths, so CJK content truncates inside the border instead
	// of overflowing it. The card field list is unchanged — only the
	// widths are enforced.
	inner := cardWidth - 2
	if selected {
		inner = cardWidth - 4
	}
	header = truncateWidth(header, inner-4)
	work = truncateWidth(work, inner-4)
	agentCard = truncateWidth(agentCard, inner)
	cardID = truncateWidth(cardID, inner)
	modelIdentity = truncateWidth(modelIdentity, inner)
	actualProvider = truncateWidth(actualProvider, inner)
	modelVersion = truncateWidth(modelVersion, inner)
	identity = truncateWidth(identity, inner)
	state = truncateWidth(state, inner)
	scope = truncateWidth(scope, inner)
	metrics = truncateWidth(metrics, inner)
	cost = truncateWidth(cost, inner)
	fresh = truncateWidth(fresh, inner)
	workFresh = truncateWidth(workFresh, inner)
	ticker = truncateWidth(ticker, inner)

	sc := statusColor(t, card)
	dotStyle := lipgloss.NewStyle().Foreground(sc)
	headStyle := lipgloss.NewStyle().Bold(true).Foreground(t.TextPrimary)
	workStyle := lipgloss.NewStyle().Foreground(t.TextSecondary)
	chipStyle := lipgloss.NewStyle().Foreground(t.TextSecondary)
	metricStyle := lipgloss.NewStyle().Foreground(t.TextPrimary)
	tickStyle := lipgloss.NewStyle().Foreground(t.TextTertiary)

	lines := []string{
		dotStyle.Render(dot) + " " + headStyle.Render(header) + "  " + workStyle.Render(work),
	}
	if scope != "" {
		lines = append(lines, chipStyle.Render(scope))
	}
	lines = append(lines,
		chipStyle.Render(agentCard), chipStyle.Render(cardID),
		chipStyle.Render(modelIdentity), chipStyle.Render(actualProvider), chipStyle.Render(modelVersion),
		chipStyle.Render(identity), chipStyle.Render(state))
	if card.isHeld() {
		for _, detail := range strings.Split(heldDetails(card), "\n") {
			lines = append(lines, chipStyle.Render(truncateWidth(detail, inner)))
		}
	} else {
		lines = append(lines, metricStyle.Render(metrics), metricStyle.Render(cost),
			chipStyle.Render(fresh), chipStyle.Render(workFresh))
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

func heldDetails(card SessionCard) string {
	var parts []string
	if card.ProjectName != "" {
		parts = append(parts, "project "+safeIdentityText(card.ProjectName))
	}
	if card.Repository != "" {
		parts = append(parts, "repo "+safeIdentityText(card.Repository))
	}
	if card.AcceptedAt != "" {
		parts = append(parts, "accepted "+safeIdentityText(card.AcceptedAt))
	}
	if len(parts) == 0 {
		return "awaiting local execution"
	}
	return strings.Join(parts, "\n")
}

func (c SessionCard) isHeld() bool {
	return strings.EqualFold(c.DaemonState, "unknown")
}

func (c SessionCard) displayState() string {
	if c.isHeld() {
		return "held"
	}
	if c.DaemonState == "" {
		return "unknown"
	}
	return c.DaemonState
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

// modelProvider returns the model-serving vendor identity. Provider is
// the legacy conflated field: prefer the explicit ModelProvider axis and
// fall back only for state written before the split.
func (c SessionCard) modelProvider() string {
	if c.ModelProvider != "" {
		return c.ModelProvider
	}
	return c.Provider
}

// heartbeatTime returns the freshest heartbeat observation: live tail
// freshness never exists (heartbeats are not tailed), so the persisted
// runner heartbeat snapshot is the only heartbeat signal. Zero means the
// runner never reported one.
func (c SessionCard) heartbeatTime() time.Time {
	if c.LastHeartbeatUnixMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.LastHeartbeatUnixMs)
}

// unknownIfEmpty renders an absent display field as unknown instead of an
// empty chip or an invented default.
func unknownIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// elapsedStr renders the session age, or unknown when the start time was
// never reported (never a zero duration).
func elapsedStr(card SessionCard, now time.Time) string {
	if card.StartedAtUnixMs <= 0 {
		return "unknown"
	}
	start := time.UnixMilli(card.StartedAtUnixMs)
	d := now.Sub(start)
	if d < 0 {
		return "unknown"
	}
	return format.Duration(int(d.Seconds()))
}

// countStr renders a cumulative count, or "not reported" until its source
// has been observed: tool calls fold from the tail (Observed), cost/turns
// only arrive on the terminal result event (MetricsReported). A zero after
// that is a measured zero.
func countStr(reported bool, n int) string {
	if !reported {
		return "not reported"
	}
	return fmt.Sprintf("%d", n)
}

// costStr renders the terminal cost payload, or "not reported" until it
// arrives on the terminal result event (never incrementally).
func costStr(card SessionCard) string {
	if !card.MetricsReported {
		return "not reported"
	}
	v := card.CostUsd
	return format.Cost(&v)
}

// freshStr renders one freshness timestamp as a relative age, or "never"
// when that signal has never been observed live. Replayed history never
// advances these timestamps (see foldMetrics), so "never" stays honest.
func freshStr(ts time.Time, now time.Time) string {
	if ts.IsZero() {
		return "never"
	}
	d := now.Sub(ts)
	if d < 0 {
		d = 0
	}
	return format.Duration(int(d.Seconds())) + " ago"
}

// isLiveState reports whether a daemon state should animate (running-ish).
// An empty state is unknown, not running: it renders statically.
func isLiveState(s string) bool {
	switch strings.ToLower(s) {
	case "running", "starting":
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

// safeIdentityText removes native VT/OSC/CSI sequences and control characters
// before row-width budgeting. Preserve printable Unicode; identity is data,
// never terminal instructions. Plain rendering receives the same protection.
func safeIdentityText(s string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, ansi.Strip(s))
	return unknownIfEmpty(clean)
}
