package hostwatch

import (
	"strconv"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

// detailField is one labeled row of the session detail view.
type detailField struct {
	label string
	value string
}

// detailFields lists everything the dashboard knows about one session, in
// display order, each value sanitized. Cards carry only the essentials; this
// view keeps every admitted and observed axis reachable for the selected
// session. Absent values read "unknown", "not reported" or "never" — the
// three ways a missing observation is stated — never a zero or a default.
func detailFields(c SessionCard, now time.Time) []detailField {
	title := c.title()
	if title == "" {
		title = "not reported"
	}
	pid := "unknown"
	if c.PID > 0 {
		pid = strconv.Itoa(c.PID)
	}
	state := c.displayState()
	if c.DaemonState == "" {
		state = "unknown"
	}
	activity := c.activityText()
	if activity == "" {
		activity = "not reported"
	}
	fields := []detailField{
		{"Issue", safeIdentityText(c.IssueIdentifier)},
		{"Title", title},
		{"Session", safeIdentityText(c.SessionID)},
		{"State", state},
		{"Elapsed", elapsedStr(c, now)},
		{"Repository", safeIdentityText(c.Repository)},
		{"Project", safeIdentityText(c.ProjectName)},
		{"Work type", safeIdentityText(c.WorkType)},
		{"Harness", safeIdentityText(c.Harness)},
		{"Model", safeIdentityText(c.Model)},
		{"Model identity", safeIdentityText(c.ActualModel)},
		{"Actual provider", safeAxisText(c.ActualModelProvider)},
		{"Model version", safeIdentityText(c.ActualModelVersion)},
		{"Model author", safeAxisText(c.ModelAuthor)},
		{"Endpoint surface", safeAxisText(c.modelProvider())},
		{"Endpoint operator", safeAxisText(c.EndpointOperator)},
		{"Protocol", safeAxisText(c.Protocol)},
		{"Agent card", safeIdentityText(c.AgentCardName)},
		{"Card ID", safeIdentityText(c.AgentCardID)},
		{"Turns", countStr(c.TurnsReported, c.NumTurns)},
		{"Cost", costStr(c)},
		{"Tools", countStr(c.Observed, c.ToolCalls)},
		{"Activity", activity},
		{"Output", freshStr(c.LastOutputAt, now)},
		{"Work", freshStr(c.LastWorkAt, now)},
		{"Heartbeat", freshStr(c.heartbeatTime(), now)},
		{"Step", safeIdentityText(c.CurrentStep)},
		{"PID", pid},
		{"Accepted", safeIdentityText(c.AcceptedAt)},
		{"Worktree", safeIdentityText(c.WorktreePath)},
	}
	if c.isHeld() {
		// A held row has no process, workarea or live observations yet.
		kept := fields[:0]
		for _, f := range fields {
			switch f.label {
			case "Turns", "Cost", "Tools", "Activity", "Output", "Work", "Heartbeat", "Step", "PID", "Worktree":
				continue
			}
			kept = append(kept, f)
		}
		fields = kept
	}
	return fields
}

// detailLabelWidth is the label column width of the detail view.
const detailLabelWidth = 18

// renderDetail renders the selected session's detail fields as rows of
// "label  value", each at most width cells. Wide terminals use two columns.
// It returns at most height rows; a clipped tail ends in a "↓ N more" row.
func renderDetail(t theme.Theme, c SessionCard, now time.Time, width, height int, plain bool) []string {
	fields := detailFields(c, now)
	columns := 1
	colW := width
	if width >= 96 {
		columns = 2
		colW = (width - 3) / 2
	}
	labelStyle := lipgloss.NewStyle().Foreground(t.TextTertiary)
	valueStyle := lipgloss.NewStyle().Foreground(t.TextPrimary)
	cell := func(f detailField) string {
		label := fitCells(f.label, detailLabelWidth)
		value := truncateWidth(f.value, colW-detailLabelWidth-1)
		if plain {
			return truncateWidth(label+" "+value, colW)
		}
		return truncateWidth(labelStyle.Render(label)+" "+valueStyle.Render(value), colW)
	}
	rowsNeeded := (len(fields) + columns - 1) / columns
	var rows []string
	for r := 0; r < rowsNeeded; r++ {
		left := cell(fields[r])
		if columns == 2 && r+rowsNeeded < len(fields) {
			left = fitCells(left, colW) + "   " + cell(fields[r+rowsNeeded])
		}
		rows = append(rows, left)
	}
	if height > 0 && len(rows) > height {
		hiddenFields := 0
		for r := height - 1; r < len(rows); r++ {
			hiddenFields++
			if columns == 2 && r+rowsNeeded < len(fields) {
				hiddenFields++
			}
		}
		rows = append(rows[:height-1], truncateWidth(overflowMarker(t, false, hiddenFields, plain)+" fields  [ ] resize", width))
	}
	return rows
}

// detailTitle is the pane title shown while the detail view replaces the
// stream.
func detailTitle(c SessionCard, plain bool, t theme.Theme, width int) string {
	text := "session detail · " + c.displayID()
	hint := "  esc close"
	if plain {
		return truncateWidth(text+hint, width)
	}
	return truncateWidth(theme.SectionTitle().Render(text)+lipgloss.NewStyle().Foreground(t.TextTertiary).Render(hint), width)
}
