package detail

import (
	"fmt"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/RenseiAI/tui-components/widget"
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

	content := a.Content
	if a.ToolName != nil && a.Type == afclient.ActivityAction {
		badge := lipgloss.NewStyle().
			Foreground(theme.Default().BgPrimary).
			Background(theme.Default().Teal).
			Padding(0, 1).
			Render(*a.ToolName)
		content = badge + " " + content
	}

	maxContentWidth := width - 18
	if maxContentWidth < 20 {
		maxContentWidth = 20
	}
	if len(content) > maxContentWidth {
		content = content[:maxContentWidth-3] + "..."
	}

	rendered := colorStyle.Render(content)
	return fmt.Sprintf("  %s %s %s", tsRendered, icon, rendered)
}

func formatActivityTimestamp(isoString string) string {
	t, err := time.Parse(time.RFC3339, isoString)
	if err != nil {
		return isoString
	}
	return t.Local().Format("15:04:05")
}
