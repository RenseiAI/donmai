package hostwatch

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/theme"
)

// maxStreamSummary bounds the length of the per-event summary text in the
// merged stream so a chatty tool burst cannot blow out a line.
const maxStreamSummary = 160

// prefixIndex assigns a stable small integer to a session id so its stream
// prefix gets a consistent color across ticks.
type prefixIndex struct {
	order map[string]int
}

func newPrefixIndex() *prefixIndex { return &prefixIndex{order: map[string]int{}} }

func (p *prefixIndex) get(sessionID string) int {
	if i, ok := p.order[sessionID]; ok {
		return i
	}
	i := len(p.order)
	p.order[sessionID] = i
	return i
}

// formatStreamLine renders one TailEvent as a single merged-stream line:
//
//	HH:MM:SS [LABEL] kind  summary
//
// label is the issue identifier when known (else a short session id). The
// label is color-coded per session. When plain is true, color is dropped.
// Returns "" for events that carry no useful stream text (e.g. an empty
// assistant-text chunk).
func formatStreamLine(t theme.Theme, ev TailEvent, label string, colorIdx int, plain bool) string {
	if label == "" {
		label = shortID(ev.SessionID)
	}
	kind, summary := summarizeEvent(ev)
	if kind == "" {
		return ""
	}
	ts := ev.At.Format("15:04:05")

	if plain {
		if summary == "" {
			return fmt.Sprintf("%s [%s] %s", ts, label, kind)
		}
		return fmt.Sprintf("%s [%s] %s  %s", ts, label, kind, summary)
	}

	tsStyle := lipgloss.NewStyle().Foreground(t.TextTertiary)
	labelStyle := lipgloss.NewStyle().Foreground(prefixColor(t, colorIdx)).Bold(true)
	kindStyle := lipgloss.NewStyle().Foreground(kindHue(t, ev))
	sumStyle := lipgloss.NewStyle().Foreground(t.TextSecondary)

	out := tsStyle.Render(ts) + " " +
		labelStyle.Render("["+label+"]") + " " +
		kindStyle.Render(kind)
	if summary != "" {
		out += "  " + sumStyle.Render(summary)
	}
	return out
}

// prefixColor cycles a small set of theme accent colors for stream labels —
// the af-worker-fleet `[W##]` colored-tag idea, now keyed by session so
// concurrent sessions are visually separable. Colors are read from the theme
// (color.Color, which lipgloss.Foreground accepts directly).
func prefixColor(t theme.Theme, idx int) color.Color {
	palette := []color.Color{
		t.Accent,
		t.Teal,
		t.Blue,
		t.StatusWarning,
		t.StatusSuccess,
		t.AccentDim,
	}
	return palette[idx%len(palette)]
}

// kindHue colors the event-kind token by semantics.
func kindHue(t theme.Theme, ev TailEvent) color.Color {
	if ev.Err != nil {
		return t.StatusError
	}
	if ev.Event == nil {
		return t.TextSecondary
	}
	switch ev.Event.Kind() {
	case agent.EventError:
		return t.StatusError
	case agent.EventResult:
		return t.StatusSuccess
	case agent.EventToolUse, agent.EventToolResult, agent.EventToolProgress:
		return t.Teal
	case agent.EventAssistantText:
		return t.TextPrimary
	default:
		return t.TextSecondary
	}
}

// summarizeEvent maps a TailEvent to a (kind, summary) pair for the stream.
// The kind token is a short label; the summary is the human-readable detail.
func summarizeEvent(ev TailEvent) (kind, summary string) {
	if ev.Err != nil {
		return "decode_error", truncateRunes(ev.Err.Error(), maxStreamSummary)
	}
	if ev.Event == nil {
		return "", ""
	}
	switch e := ev.Event.(type) {
	case agent.InitEvent:
		return "init", ""
	case agent.SystemEvent:
		s := e.Subtype
		if e.Message != "" {
			s = strings.TrimSpace(e.Subtype + " " + e.Message)
		}
		return "system", truncateRunes(collapseWS(s), maxStreamSummary)
	case agent.AssistantTextEvent:
		txt := strings.TrimSpace(e.Text)
		if txt == "" {
			return "", ""
		}
		return "thought", truncateRunes(collapseWS(txt), maxStreamSummary)
	case agent.ToolUseEvent:
		return "tool_use", truncateRunes(toolUseSummary(e), maxStreamSummary)
	case agent.ToolResultEvent:
		status := "ok"
		if e.IsError {
			status = "error"
		}
		name := e.ToolName
		if name == "" {
			name = "tool"
		}
		return "tool_result", truncateRunes(collapseWS(name+" → "+status), maxStreamSummary)
	case agent.ToolProgressEvent:
		return "tool_progress", truncateRunes(fmt.Sprintf("%s (%.0fs)", e.ToolName, e.ElapsedSeconds), maxStreamSummary)
	case agent.ResultEvent:
		if e.Success {
			return "result", "✓ completed"
		}
		msg := "✗ failed"
		if len(e.Errors) > 0 {
			msg = "✗ " + e.Errors[0]
		}
		return "result", truncateRunes(collapseWS(msg), maxStreamSummary)
	case agent.ErrorEvent:
		return "error", truncateRunes(collapseWS(e.Message), maxStreamSummary)
	default:
		return string(ev.Event.Kind()), ""
	}
}

// toolUseSummary renders a tool-use event as "Tool: arg" — a local
// re-implementation of the activity poster's summarizeToolUse (which is
// package-private to runtime/activity), keeping hostwatch dependency-free of
// that package.
func toolUseSummary(e agent.ToolUseEvent) string {
	name := e.ToolName
	if name == "" {
		name = "tool"
	}
	var arg string
	switch {
	case strings.EqualFold(name, "Bash"):
		arg = strArg(e.Input, "command")
	case strings.EqualFold(name, "Read"), strings.EqualFold(name, "Edit"),
		strings.EqualFold(name, "Write"), strings.EqualFold(name, "NotebookEdit"):
		arg = strArg(e.Input, "file_path")
	case strings.EqualFold(name, "Grep"), strings.EqualFold(name, "Glob"):
		arg = strArg(e.Input, "pattern")
	case strings.EqualFold(name, "Agent"), strings.EqualFold(name, "Task"):
		arg = strArg(e.Input, "description")
		if arg == "" {
			arg = strArg(e.Input, "prompt")
		}
	}
	if arg == "" {
		return name
	}
	return name + ": " + collapseWS(arg)
}

// strArg returns the trimmed string value of input[key], or "".
func strArg(input map[string]any, key string) string {
	if input == nil {
		return ""
	}
	v, ok := input[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// collapseWS collapses runs of whitespace (incl. newlines) to single
// spaces so a multi-line tool arg renders as one clean stream line.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// streamTickerText returns the short "last activity" string a card shows in
// its tool-ticker line, derived from the most recent meaningful event.
func streamTickerText(ev TailEvent) string {
	_, summary := summarizeEvent(ev)
	return summary
}
