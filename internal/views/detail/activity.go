package detail

import (
	"fmt"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/RenseiAI/tui-components/widget"
	"github.com/mattn/go-runewidth"
)

// activityIcon returns the icon for an activity type.
func activityIcon(t afclient.ActivityType) string {
	switch t {
	case afclient.ActivityThought:
		return "\U0001f4ad"
	case afclient.ActivityAction:
		return "\u26a1"
	case afclient.ActivityResponse:
		return "\U0001f4ac"
	case afclient.ActivityError:
		return "\u2717"
	case afclient.ActivityProgress:
		return "\u2713"
	default:
		return "\u00b7"
	}
}

// activityColor returns the lipgloss style for an activity type.
func activityColor(t afclient.ActivityType) lipgloss.Style {
	switch t {
	case afclient.ActivityThought:
		return lipgloss.NewStyle().Foreground(theme.Default().TextSecondary)
	case afclient.ActivityAction:
		return lipgloss.NewStyle().Foreground(theme.Default().Teal)
	case afclient.ActivityResponse:
		return lipgloss.NewStyle().Foreground(theme.Default().TextPrimary)
	case afclient.ActivityError:
		return lipgloss.NewStyle().Foreground(theme.Default().StatusError)
	case afclient.ActivityProgress:
		return lipgloss.NewStyle().Foreground(theme.Default().StatusSuccess)
	default:
		return lipgloss.NewStyle().Foreground(theme.Default().TextTertiary)
	}
}

// activityKeyMap returns a LogViewer KeyMap customized for the activity
// viewport. Clear and ToggleWrap are disabled since the parent view
// manages the activity buffer lifecycle.
func activityKeyMap() widget.KeyMap {
	km := widget.DefaultKeyMap()
	km.Clear.SetEnabled(false)
	km.ToggleWrap.SetEnabled(false)
	return km
}

// newActivityLogViewer creates a LogViewer configured for activity display.
func newActivityLogViewer() *widget.LogViewer {
	return widget.NewLogViewer(
		widget.WithLogViewerKeyMap(activityKeyMap()),
	)
}

// renderActivityLine formats a single activity event as a styled string
// suitable for LogViewer.Append.
func renderActivityLine(a afclient.ActivityEvent, width int) string {
	ts := formatActivityTimestamp(a.Timestamp)
	tsRendered := theme.Dimmed().Render("[" + ts + "]")

	icon := activityIcon(a.Type)
	colorStyle := activityColor(a.Type)

	// Budget for the content, in terminal display cells (width minus the
	// indent + timestamp + icon prefix).
	budget := width - 18
	if budget < 20 {
		budget = 20
	}

	var badge string
	if a.ToolName != nil && a.Type == afclient.ActivityAction {
		badge = lipgloss.NewStyle().
			Foreground(theme.Default().BgPrimary).
			Background(theme.Default().Teal).
			Padding(0, 1).
			Render(*a.ToolName) + " "
		// The badge spends part of the content budget; measure it in
		// display cells (ANSI-aware) so the clipped row still fits.
		if bw := lipgloss.Width(badge); bw < budget {
			budget -= bw
		} else {
			budget = 1
		}
	}

	// Clip by display cells, never runes or bytes: byte slicing splits a
	// multi-byte UTF-8 sequence into mojibake, and rune counting lets
	// double-width CJK/emoji runes overflow the row to twice the budget.
	// runewidth.Truncate measures terminal cells and appends the ellipsis
	// only when it actually clips.
	content := runewidth.Truncate(a.Content, budget, "...")

	rendered := badge + colorStyle.Render(content)
	return fmt.Sprintf("  %s %s %s", tsRendered, icon, rendered)
}

func formatActivityTimestamp(isoString string) string {
	t, err := time.Parse(time.RFC3339, isoString)
	if err != nil {
		return isoString
	}
	return t.Local().Format("15:04:05")
}
