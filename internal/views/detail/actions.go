// Package detail implements the session detail TUI view.
package detail

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func (m *Model) stopAgentCmd() tea.Cmd {
	id := m.sessionID
	return func() tea.Msg {
		_, err := m.dataSource.StopSession(id)
		return stopAgentMsg{err: err}
	}
}

func (m *Model) sendPromptCmd(text string) tea.Cmd {
	id := m.sessionID
	return func() tea.Msg {
		_, err := m.dataSource.ChatSession(id, afclient.ChatSessionRequest{Prompt: text})
		return sendPromptMsg{text: text, err: err}
	}
}

// addInlineActivity appends a synthetic activity to the log viewer.
func (m *Model) addInlineActivity(actType afclient.ActivityType, content string) {
	a := afclient.ActivityEvent{
		ID:        "local",
		Type:      actType,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	m.logViewer.Append(renderActivityLine(a, m.width))
}
