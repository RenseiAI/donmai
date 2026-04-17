package detail

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/agentfactory-tui/internal/api"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/RenseiAI/tui-components/widget"
	"github.com/RenseiAI/tui-components/widget/notification"
)

// NavigateBackMsg is sent when the user wants to go back to the dashboard.
type NavigateBackMsg struct{}

// Model is the Agent Detail view model.
type Model struct {
	dataSource api.DataSource
	session    *api.SessionDetail
	sessionID  string
	width      int
	height     int
	focused    bool
	loading    bool
	err        error
	frame      int
	// Activity streaming
	logViewer      *widget.LogViewer
	activityCursor *string
	// UI state
	showHelp    bool
	stopDialog  *widget.Dialog
	promptInput *widget.TextInput
	notifStack  notification.Stack
}

// New creates a new detail model.
func New(ds api.DataSource) *Model {
	return &Model{
		dataSource: ds,
		logViewer:  newActivityLogViewer(),
		notifStack: notification.NewStack(notification.WithMax(3), notification.WithNewestFirst()),
	}
}

// SetSession sets the session ID to display and starts loading.
func (m *Model) SetSession(id string) {
	m.sessionID = id
	m.loading = true
	m.session = nil
	m.activityCursor = nil
	m.logViewer = newActivityLogViewer()
	m.showHelp = false
	m.stopDialog = nil
	m.promptInput = nil
}

// SetSize updates the available render size.
func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// Focus marks the detail view as focused.
func (m *Model) Focus() {
	m.focused = true
	m.logViewer.Focus()
}

// Blur marks the detail view as unfocused.
func (m *Model) Blur() {
	m.focused = false
	m.logViewer.Blur()
}

// Init starts the data fetch and activity streaming.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchDetail(),
		m.fetchActivitiesCmd(),
		m.tickCmd(),
		m.activityTickCmd(),
	)
}

func (m *Model) fetchDetail() tea.Cmd {
	id := m.sessionID
	return func() tea.Msg {
		detail, err := m.dataSource.GetSessionDetail(id)
		return detailDataMsg{detail: detail, err: err}
	}
}

func (m *Model) tickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(_ time.Time) tea.Msg {
		return detailTickMsg{}
	})
}

func (m *Model) fetchActivitiesCmd() tea.Cmd {
	id := m.sessionID
	cursor := m.activityCursor
	return func() tea.Msg {
		resp, err := m.dataSource.GetActivities(id, cursor)
		if err != nil {
			return activityMsg{err: err}
		}
		return activityMsg{
			activities: resp.Activities,
			cursor:     resp.Cursor,
		}
	}
}

func (m *Model) activityTickCmd() tea.Cmd {
	return tea.Tick(1*time.Second, func(_ time.Time) tea.Msg {
		return activityTickMsg{}
	})
}

// isTerminal returns true if the session is in a terminal state.
func (m *Model) isTerminal() bool {
	if m.session == nil {
		return false
	}
	switch m.session.Status {
	case api.StatusCompleted, api.StatusFailed, api.StatusStopped:
		return true
	}
	return false
}

// Update handles messages for the detail view.
func (m *Model) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case detailDataMsg:
		m.loading = false
		if msg.detail != nil {
			m.session = &msg.detail.Session
		}
		if msg.err != nil {
			m.err = msg.err
		}

	case detailTickMsg:
		m.frame++
		return tea.Batch(m.fetchDetail(), m.tickCmd())

	case activityMsg:
		if msg.err == nil && len(msg.activities) > 0 {
			lines := make([]string, 0, len(msg.activities))
			for _, a := range msg.activities {
				lines = append(lines, renderActivityLine(a, m.width))
			}
			m.logViewer.Append(lines...)
			m.activityCursor = msg.cursor
		}

	case activityTickMsg:
		if !m.isTerminal() {
			return tea.Batch(m.fetchActivitiesCmd(), m.activityTickCmd())
		}
		// Terminal state: do one final fetch, then stop
		return m.fetchActivitiesCmd()

	case stopAgentMsg:
		if msg.err != nil {
			m.addInlineActivity(api.ActivityError, "Failed to stop agent: "+msg.err.Error())
			return m.pushNotification(notification.VariantError, "Failed to stop agent")
		}
		return m.pushNotification(notification.VariantSuccess, "Agent stop requested")

	case sendPromptMsg:
		if msg.err != nil {
			m.addInlineActivity(api.ActivityError, "Failed to send prompt: "+msg.err.Error())
			return m.pushNotification(notification.VariantError, "Failed to send prompt")
		}
		m.addInlineActivity(api.ActivityResponse, "Prompt sent: "+msg.text)
		return m.pushNotification(notification.VariantSuccess, "Prompt sent")

	case widget.DialogDoneMsg:
		m.logViewer.Focus()
		if msg.Result == widget.ResultYes {
			m.stopDialog = nil
			return m.stopAgentCmd()
		}
		m.stopDialog = nil
		return nil

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	}

	// Fan remaining messages to children.
	var cmds []tea.Cmd
	_, cmd := m.logViewer.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	next, cmd := m.notifStack.Update(msg)
	m.notifStack = next.(notification.Stack)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *Model) handleKeyPress(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	// Help overlay takes priority
	if m.showHelp {
		if key == "?" || key == "esc" {
			m.showHelp = false
		}
		return nil
	}

	// Stop dialog active — delegate keys to it
	if m.stopDialog != nil {
		_, cmd := m.stopDialog.Update(msg)
		return cmd
	}

	// Prompt input mode
	if m.promptInput != nil {
		switch key {
		case "esc":
			m.promptInput = nil
			m.logViewer.Focus()
		case "enter":
			if text := m.promptInput.Value(); text != "" {
				m.promptInput = nil
				m.logViewer.Focus()
				return m.sendPromptCmd(text)
			}
		default:
			next, cmd := m.promptInput.Update(msg)
			if ti, ok := next.(widget.TextInput); ok {
				m.promptInput = &ti
			}
			return cmd
		}
		return nil
	}

	// Normal mode keybindings
	switch key {
	case "esc":
		return func() tea.Msg { return NavigateBackMsg{} }
	case "r":
		return m.fetchDetail()
	case "s":
		if !m.isTerminal() {
			m.stopDialog = widget.New(
				widget.WithTitle("Stop Agent"),
				widget.WithBody("Stop agent "+m.sessionID+"? This cannot be undone."),
				widget.WithButtons(
					widget.Button{Label: "Stop", Result: widget.ResultYes},
					widget.Button{Label: "Cancel", Result: widget.ResultCancel},
				),
			)
			m.stopDialog.SetSize(m.width, m.height)
			m.stopDialog.Focus()
			m.logViewer.Blur()
		}
	case "p":
		if !m.isTerminal() {
			ti := widget.NewTextInput(
				widget.WithPlaceholder("Send prompt to agent..."),
			)
			ti.SetSize(m.width-4, 0)
			ti.Focus()
			m.promptInput = &ti
			m.logViewer.Blur()
		}
	case "?":
		m.showHelp = true
	default:
		// Delegate scroll/follow keys to the log viewer.
		_, cmd := m.logViewer.Update(msg)
		return cmd
	}
	return nil
}

// Render returns the detail view as a rendered string.
func (m *Model) Render() string {
	if m.width == 0 {
		return ""
	}

	var sections []string

	// Title bar: identifier + work type + provider + status
	sections = append(sections, m.renderTitleBar())

	if m.loading || m.session == nil {
		sections = append(sections, theme.Muted().Padding(1, 2).Render("Loading session detail..."))
		return lipgloss.JoinVertical(lipgloss.Left, sections...)
	}

	s := *m.session

	// Metadata header
	sections = append(sections, renderMetadata(s, m.width))

	// Timeline
	sections = append(sections, renderTimeline(s, m.width, m.frame))

	// Activity section header
	activityTitle := lipgloss.NewStyle().
		Foreground(theme.TextPrimary).
		Bold(true).
		Padding(0, 1).
		Width(m.width).
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(theme.SurfaceBorder).
		Render("ACTIVITY")
	sections = append(sections, activityTitle)

	header := lipgloss.JoinVertical(lipgloss.Left, sections...)

	// Calculate viewport height
	headerHeight := strings.Count(header, "\n") + 1
	helpHeight := 1
	if m.promptInput != nil {
		helpHeight = 3 // TextInput with border takes 3 rows
	}
	viewportHeight := m.height - headerHeight - helpHeight
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.logViewer.SetSize(m.width, viewportHeight)

	// Activity viewport
	activityContent := m.logViewer.View().Content

	// Help bar (or prompt input or confirm stop)
	help := m.renderBottomBar()

	content := header + "\n" + activityContent + "\n" + help

	// Stop dialog overlay
	if m.stopDialog != nil {
		content = m.stopDialog.Overlay(content)
	}

	// Help overlay
	if m.showHelp {
		content = renderHelpOverlay(content, m.width, m.height)
	}

	// Notification toasts (top-right corner)
	if m.notifStack.Len() > 0 {
		toasts := m.notifStack.View().Content
		placed := lipgloss.Place(m.width, 0, lipgloss.Right, lipgloss.Top, toasts)
		// Overlay on first few lines of content
		contentLines := strings.Split(content, "\n")
		toastLines := strings.Split(placed, "\n")
		for i, tl := range toastLines {
			if i+1 < len(contentLines) {
				contentLines[i+1] = tl
			}
		}
		content = strings.Join(contentLines, "\n")
	}

	return content
}

func (m *Model) renderTitleBar() string {
	if m.session == nil {
		return lipgloss.NewStyle().
			Foreground(theme.TextSecondary).
			Background(theme.Surface).
			Padding(0, 1).
			Width(m.width).
			Render("\u2190 Back (esc)")
	}

	s := *m.session
	ss := theme.GetStatusStyle(string(s.Status))

	symbol := lipgloss.NewStyle().Foreground(ss.Color).Render(ss.Symbol)
	identifier := lipgloss.NewStyle().Foreground(theme.TextPrimary).Bold(true).Render(s.Identifier)

	wtColor := theme.GetWorkTypeColor(s.WorkType)
	wtLabel := theme.GetWorkTypeLabel(s.WorkType)
	workType := lipgloss.NewStyle().Foreground(wtColor).Render(wtLabel)

	providerText := "--"
	if s.Provider != nil {
		providerText = *s.Provider
	}
	provider := theme.Muted().Render(providerText)

	statusLabel := lipgloss.NewStyle().Foreground(ss.Color).Render(ss.Label)

	// Auto-follow indicator
	followIndicator := ""
	if m.logViewer.Following() {
		followIndicator = lipgloss.NewStyle().Foreground(theme.Teal).Bold(true).Render(" \u27f1")
	}

	titleContent := symbol + " " + identifier + "  " + workType + "  " + provider + "  " + statusLabel + followIndicator

	return lipgloss.NewStyle().
		Background(theme.Surface).
		Padding(0, 1).
		Width(m.width).
		Render(titleContent)
}

func (m *Model) renderBottomBar() string {
	if m.promptInput != nil {
		return m.promptInput.View().Content
	}

	pairs := []struct{ key, desc string }{
		{"esc", "back"},
		{"s", "stop"},
		{"p", "prompt"},
		{"f", "follow"},
		{"\u2191\u2193", "scroll"},
		{"?", "help"},
	}

	var parts []string
	for _, p := range pairs {
		k := theme.HelpKey().Render(p.key)
		d := theme.HelpDesc().Render(p.desc)
		parts = append(parts, k+" "+d)
	}

	return theme.HelpBar().Width(m.width).Render(strings.Join(parts, "  "))
}

// pushNotification adds a toast to the notification stack and returns its
// auto-dismiss command.
func (m *Model) pushNotification(variant notification.Variant, message string) tea.Cmd {
	n := notification.New(variant, message, notification.WithWidth(30))
	var cmd tea.Cmd
	m.notifStack, cmd = m.notifStack.Push(n)
	return cmd
}
