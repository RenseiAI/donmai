package detail

import (
	"fmt"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/agentfactory-tui/internal/api"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/RenseiAI/tui-components/widget"
)

// activityIcon returns the icon for an activity type.
func activityIcon(t api.ActivityType) string {
	switch t {
	case api.ActivityThought:
		return "\U0001f4ad"
	case api.ActivityAction:
		return "\u26a1"
	case api.ActivityResponse:
		return "\U0001f4ac"
	case api.ActivityError:
		return "\u2717"
	case api.ActivityProgress:
		return "\u2713"
	default:
		return "\u00b7"
	}
}

// activityColor returns the lipgloss style for an activity type.
func activityColor(t api.ActivityType) lipgloss.Style {
	switch t {
	case api.ActivityThought:
		return lipgloss.NewStyle().Foreground(theme.TextSecondary)
	case api.ActivityAction:
		return lipgloss.NewStyle().Foreground(theme.Teal)
	case api.ActivityResponse:
		return lipgloss.NewStyle().Foreground(theme.TextPrimary)
	case api.ActivityError:
		return lipgloss.NewStyle().Foreground(theme.StatusError)
	case api.ActivityProgress:
		return lipgloss.NewStyle().Foreground(theme.StatusSuccess)
	default:
		return lipgloss.NewStyle().Foreground(theme.TextTertiary)
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
func renderActivityLine(a api.ActivityEvent, width int) string {
	ts := formatActivityTimestamp(a.Timestamp)
	tsRendered := theme.Dimmed().Render("[" + ts + "]")

	icon := activityIcon(a.Type)
	colorStyle := activityColor(a.Type)

	content := a.Content
	if a.ToolName != nil && a.Type == api.ActivityAction {
		badge := lipgloss.NewStyle().
			Foreground(theme.BgPrimary).
			Background(theme.Teal).
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
