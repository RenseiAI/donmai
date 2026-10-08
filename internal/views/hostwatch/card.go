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

// Card geometry. Every card in the grid shares one outer width (derived
// from the terminal width, never from its content) and one height, so the
// cards of a row align and the grid's columns line up.
const (
	// minCardWidth is the narrowest full card in terminal cells, borders
	// included. Below it the grid falls back to one line per session.
	minCardWidth = 36
	// cardGap is the blank column between neighbouring cards in a row.
	cardGap = 1
	// cardBodyRows is the fixed number of content rows in every card.
	cardBodyRows = 5
	// cardChrome is the horizontal cells a styled card spends on its frame:
	// one border cell and one padding cell on each side.
	cardChrome = 4
	// maxActivityRunes bounds the activity text kept per card. Rendering
	// truncates by terminal cells; this only caps memory per session.
	maxActivityRunes = 240
)

// animFrames is the status-dot pulse animation. A running session cycles
// these; a terminal or idle session shows a static glyph.
var animFrames = []string{"●", "◉", "○", "◉"} // ● ◉ ○ ◉

// statusColor maps a card's effective status to a theme status color.
// Colors are read from the theme exclusively.
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

// segment is one styled run of a card row. Rows are composed from
// segments, then fitted to the row's cell budget as a whole, so the
// lowest-priority text (always last) is what an ellipsis replaces.
type segment struct {
	text  string
	style lipgloss.Style
}

func joinSegments(segs []segment, plain bool) string {
	var b strings.Builder
	for _, s := range segs {
		if s.text == "" {
			continue
		}
		if plain {
			b.WriteString(s.text)
		} else {
			b.WriteString(s.style.Render(s.text))
		}
	}
	return b.String()
}

// renderCard renders one session card exactly width cells wide.
//
// Rows, in a fixed order every card shares:
//
//  1. selection marker, status dot, issue id and title
//  2. project (repository) and work type
//  3. model (with the observed serving model when it differs) and harness
//  4. state, elapsed time, and turns / cost when the harness reported them
//  5. age of the last activity and what it was
//
// Values are bare; a label appears only where a bare value would be
// ambiguous ("elapsed unknown", "harness unknown"). Missing data stays
// unknown — it is never rendered as zero or as an invented default.
// Selection never changes geometry: every card carries the same one-cell
// frame and reserves the padding cell left of its first row for a marker.
// The selected card switches to a heavy frame in the bright border color
// and shows ▸ in that cell, so selection reads without color too.
//
// Plain mode drops color and the frame; rows are bounded by width but not
// padded, and the selection marker is ">".
func renderCard(t theme.Theme, card SessionCard, frame int, selected, plain bool, now time.Time, width int) string {
	if plain {
		marker := "  "
		if selected {
			marker = "> "
		}
		rows := cardRows(t, card, frame, true, now, width-len(marker))
		for i := range rows {
			lead := "  "
			if i == 0 {
				lead = marker
			}
			rows[i] = truncateWidth(lead+rows[i], width)
		}
		return strings.Join(rows, "\n")
	}
	inner := width - cardChrome
	if inner < 1 {
		inner = 1
	}
	rows := cardRows(t, card, frame, false, now, inner)
	border := lipgloss.RoundedBorder()
	borderColor := t.SurfaceBorder
	marker := " "
	if selected {
		border = lipgloss.ThickBorder()
		borderColor = t.SurfaceBorderBright
		marker = lipgloss.NewStyle().Bold(true).Foreground(t.Accent).Render("▸")
	}
	bs := lipgloss.NewStyle().Foreground(borderColor)
	out := make([]string, 0, len(rows)+2)
	out = append(out, bs.Render(border.TopLeft+strings.Repeat(border.Top, inner+2)+border.TopRight))
	for i, row := range rows {
		lead := " "
		if i == 0 {
			lead = marker
		}
		out = append(out, bs.Render(border.Left)+lead+fitCells(row, inner)+" "+bs.Render(border.Right))
	}
	out = append(out, bs.Render(border.BottomLeft+strings.Repeat(border.Bottom, inner+2)+border.BottomRight))
	return strings.Join(out, "\n")
}

// cardHeight is the number of terminal rows every card occupies.
func cardHeight(plain bool) int {
	if plain {
		return cardBodyRows
	}
	return cardBodyRows + 2
}

// cardRows returns the card's content rows (always cardBodyRows of them),
// each composed and truncated to at most inner cells. Plain rows after the
// first are indented to line up under the issue id.
func cardRows(t theme.Theme, card SessionCard, frame int, plain bool, now time.Time, inner int) []string {
	sc := statusColor(t, card)
	primary := lipgloss.NewStyle().Foreground(t.TextPrimary)
	secondary := lipgloss.NewStyle().Foreground(t.TextSecondary)
	tertiary := lipgloss.NewStyle().Foreground(t.TextTertiary)
	state := lipgloss.NewStyle().Foreground(sc)

	indent := ""
	if plain {
		indent = "  "
	}

	identity := []segment{
		{card.statusDot(frame), lipgloss.NewStyle().Foreground(sc)},
		{" ", primary},
		{card.displayID(), lipgloss.NewStyle().Bold(true).Foreground(t.TextPrimary)},
	}
	if title := card.title(); title != "" {
		identity = append(identity, segment{"  " + title, secondary})
	}

	var context, model, status, activity []segment
	if card.isHeld() {
		context = []segment{{indent + card.repoName(), secondary}}
		model = []segment{{indent + "accepted " + safeIdentityText(card.AcceptedAt), tertiary}}
		status = []segment{{indent + "held", state}, {" " + card.elapsedText(now), primary}}
		activity = []segment{{indent + "awaiting local execution", tertiary}}
	} else {
		context = []segment{{indent + card.repoName() + " · " + labelIfUnknown(card.WorkType, "work type"), secondary}}
		model = []segment{{indent + card.modelText(inner-len(indent)), secondary}}
		status = []segment{{indent + card.displayState(), state}, {" " + card.elapsedText(now), primary}}
		if card.TurnsReported {
			status = append(status, segment{fmt.Sprintf(" · %d turns", card.NumTurns), primary})
		}
		if card.CostReported {
			status = append(status, segment{" · " + costStr(card), primary})
		}
		if card.Errored {
			status = append(status, segment{" · errored", lipgloss.NewStyle().Foreground(t.StatusError)})
		}
		activity = card.activitySegments(now, indent, primary, tertiary)
	}

	rows := [][]segment{identity, context, model, status, activity}
	out := make([]string, len(rows))
	for i, segs := range rows {
		out[i] = truncateWidth(joinSegments(segs, plain), inner)
	}
	return out
}

// activitySegments renders the last-activity row: how long ago the session
// last produced output or work, then what that was. Either part may be
// absent; with neither, the row says so instead of implying idleness.
func (c SessionCard) activitySegments(now time.Time, indent string, primary, tertiary lipgloss.Style) []segment {
	text := c.activityText()
	last := c.lastActivityAt()
	switch {
	case !last.IsZero() && text != "":
		return []segment{{indent + freshStr(last, now), primary}, {" · " + text, tertiary}}
	case !last.IsZero():
		return []segment{{indent + freshStr(last, now), primary}}
	case text != "":
		return []segment{{indent + text, tertiary}}
	default:
		return []segment{{indent + "activity not reported", tertiary}}
	}
}

// activityText is the most specific description of the session's latest
// activity: folded stream activity, else the last tool, else the runner's
// current step.
func (c SessionCard) activityText() string {
	for _, s := range []string{c.LastActivity, c.LastTool, c.CurrentStep} {
		if clean := cleanText(s); clean != "" {
			return clean
		}
	}
	return ""
}

// lastActivityAt is the freshest observed output or work time. Zero means
// neither has been observed.
func (c SessionCard) lastActivityAt() time.Time {
	if c.LastWorkAt.After(c.LastOutputAt) {
		return c.LastWorkAt
	}
	return c.LastOutputAt
}

// modelText renders "<model> · <harness>" within budget cells, keeping the
// harness whole and shortening the model first. When the observed serving
// model differs from the requested one, both show: "<requested> → <observed>".
func (c SessionCard) modelText(budget int) string {
	harness := labelIfUnknown(c.Harness, "harness")
	model := labelIfUnknown(c.Model, "model")
	// The observed serving model wins when the harness reported one. A
	// dated snapshot of the requested alias simply replaces it; a different
	// model (a fallback) shows as "<requested> → <observed>".
	if observed := cleanText(c.ActualModel); observed != "" {
		if requested := cleanText(c.Model); requested == "" || strings.HasPrefix(observed, requested) {
			model = observed
		} else {
			model = requested + " → " + observed
		}
	}
	tail := " · " + harness
	if room := budget - lipgloss.Width(tail); room > 0 && lipgloss.Width(model) > room {
		model = truncateWidth(model, room)
	}
	return model + tail
}

// displayID is the card's identifier: the issue identifier when known,
// else a short session id.
func (c SessionCard) displayID() string {
	if id := cleanText(c.IssueIdentifier); id != "" {
		return id
	}
	return shortID(cleanText(c.SessionID))
}

// title is the sanitized work-item title, or "" when none was reported.
func (c SessionCard) title() string {
	return cleanText(c.IssueTitle)
}

// repoName is the project context a card shows: the repository's short
// name (the scope the dashboard filters on), else the configured project
// identifier, else an explicit unknown.
func (c SessionCard) repoName() string {
	if name := repoShortName(c.Repository); name != "" {
		return name
	}
	return labelIfUnknown(c.ProjectName, "project")
}

// repoShortName returns the last path segment of a repository URL or slug
// without a trailing ".git", or "" when there is none.
func repoShortName(repo string) string {
	repo = strings.TrimRight(cleanText(repo), "/")
	if i := strings.LastIndexAny(repo, "/:"); i >= 0 {
		repo = repo[i+1:]
	}
	return strings.TrimSuffix(repo, ".git")
}

// statusDot is the pulsing status glyph: animated while live, a hollow
// glyph for held rows, static otherwise.
func (c SessionCard) statusDot(frame int) string {
	switch {
	case c.isHeld():
		return "○"
	case isLiveState(c.DaemonState):
		return animFrames[frame%len(animFrames)]
	default:
		return animFrames[0]
	}
}

// elapsedText is the session age, labeled only when unknown.
func (c SessionCard) elapsedText(now time.Time) string {
	e := elapsedStr(c, now)
	if e == "unknown" {
		return "elapsed unknown"
	}
	return e
}

// labelIfUnknown returns the cleaned value, or "<label> unknown" when it is
// absent: a bare "unknown" would not say which field is missing.
func labelIfUnknown(value, label string) string {
	if clean := cleanText(value); clean != "" {
		return clean
	}
	return label + " unknown"
}

func (c SessionCard) isHeld() bool {
	return strings.EqualFold(c.DaemonState, "unknown")
}

func (c SessionCard) displayState() string {
	if c.isHeld() {
		return "held"
	}
	if c.DaemonState == "" {
		return "state unknown"
	}
	return cleanText(c.DaemonState)
}

// modelProvider returns the configured endpoint surface only. The older
// Provider field conflates harness and endpoint, so it cannot fill this axis.
func (c SessionCard) modelProvider() string {
	return c.ModelProvider
}

// heartbeatTime returns the freshest heartbeat observation: the persisted
// runner heartbeat snapshot is the only heartbeat signal. Zero means the
// runner never reported one.
func (c SessionCard) heartbeatTime() time.Time {
	if c.LastHeartbeatUnixMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.LastHeartbeatUnixMs)
}

// startTime is when the session started: the runner's recorded start, else
// the daemon's admission time. Zero when neither is known.
func (c SessionCard) startTime() time.Time {
	if c.StartedAtUnixMs > 0 {
		return time.UnixMilli(c.StartedAtUnixMs)
	}
	if accepted, err := time.Parse(time.RFC3339, strings.TrimSpace(c.AcceptedAt)); err == nil {
		return accepted
	}
	return time.Time{}
}

// unknownIfEmpty renders an absent display field as unknown instead of an
// empty chip or an invented default.
func unknownIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// elapsedStr renders the session age, or unknown when neither the runner's
// start nor the daemon's admission time was reported (never a zero
// duration).
func elapsedStr(card SessionCard, now time.Time) string {
	start := card.startTime()
	if start.IsZero() {
		return "unknown"
	}
	d := now.Sub(start)
	if d < 0 {
		return "unknown"
	}
	return format.Duration(int(d.Seconds()))
}

// countStr renders a cumulative count, or "not reported" until its source
// has been observed. A reported zero is distinct from absent.
func countStr(reported bool, n int) string {
	if !reported {
		return "not reported"
	}
	return fmt.Sprintf("%d", n)
}

// costStr renders a native observed cost, including live call prices.
func costStr(card SessionCard) string {
	if !card.CostReported {
		return "not reported"
	}
	if card.CostUsd == 0 {
		return "$0.00"
	}
	v := card.CostUsd
	return format.Cost(&v)
}

// freshStr renders one freshness timestamp as a relative age, or "never"
// when that signal has never been observed.
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

// shortID returns the first 8 runes of an id for compact display.
func shortID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}

// truncateRunes shortens s to at most n runes, appending an ellipsis when
// truncated. n<=0 returns "". It bounds stored text; display budgets use
// truncateWidth, which counts terminal cells.
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

// truncateWidth shortens s to at most n terminal cells, appending an
// ellipsis when truncated. It measures grapheme clusters (wide CJK, emoji
// with modifiers or joiners) and skips ANSI sequences, so styled text keeps
// its escapes and the result always fits its budget. n<=0 returns "".
func truncateWidth(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

// fitCells truncates s to n cells and pads it with spaces to exactly n, so
// a row of fixed-width cells stays aligned whatever its content.
func fitCells(s string, n int) string {
	s = truncateWidth(s, n)
	if pad := n - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// cleanText strips terminal escape sequences and control characters and
// collapses whitespace. Display values are data, never terminal
// instructions; a newline or tab inside one would also break row geometry.
func cleanText(s string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(s))
	return collapseWS(clean)
}

// safeIdentityText sanitizes an identity value for display, rendering an
// absent one as unknown. Printable Unicode is preserved.
func safeIdentityText(s string) string {
	return unknownIfEmpty(cleanText(s))
}

// safeAxisText accepts only compact identifiers on the configured display
// axes. A malformed value cannot turn the view into a URL or credential
// display.
func safeAxisText(s string) string {
	s = safeIdentityText(s)
	if len(s) == 0 || len(s) > 64 {
		return "unknown"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return "unknown"
		}
	}
	return s
}
