package hostwatch

import (
	"fmt"
	"image/color"
	"strconv"
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
	label = cleanText(label)
	if label == "" {
		label = shortID(cleanText(ev.SessionID))
	}
	kind, summary := summarizeEvent(ev)
	if kind == "" {
		return ""
	}
	// Event text is agent output: strip escapes and controls so a tool
	// argument can never drive the terminal or break the line.
	kind, summary = cleanText(kind), cleanText(summary)
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
		if contentFreeSubtype(e.Subtype) {
			return "", ""
		}
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
	case agent.LlmCallEvent:
		s := llmCallSummary(e)
		if s == "" {
			return "", ""
		}
		return string(agent.EventLlmCall), truncateRunes(collapseWS(s), maxStreamSummary)
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
	case agent.UsageEvent:
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			return "", ""
		}
		return string(agent.EventUsage), truncateRunes(collapseWS(msg), maxStreamSummary)
	case agent.ContextUsageEvent:
		s := contextUsageSummary(e)
		if s == "" {
			return "", ""
		}
		return string(agent.EventContextUsage), truncateRunes(collapseWS(s), maxStreamSummary)
	case agent.SubagentEvent:
		s := subagentSummary(e)
		if s == "" {
			return "", ""
		}
		return string(agent.EventSubagent), truncateRunes(collapseWS(s), maxStreamSummary)
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

// contentFreeSubtype reports whether a system subtype is a runner-internal
// signal with no text for a reader: the stream-progress liveness tick and
// the metadata-only serving observations. The merged stream never prints
// them; stream progress instead drives the card's streaming indicator.
func contentFreeSubtype(subtype string) bool {
	switch subtype {
	case agent.SystemSubtypeStreamProgress,
		agent.SystemSubtypeModelIdentity,
		agent.SystemSubtypeGenerationID:
		return true
	}
	return false
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

// llmCallSummary renders one model call as a compact token/cost line:
// the serving model, the input/output totals, the cached input share,
// the observed price and the turn marker. A synthetic aggregate event
// carries the rolled-up session total rather than a measured per-call
// value, so it is marked (aggregate). The model is the native response
// model when the call is provider-measured and reports one, else the
// requested model. An event with no model, no tokens and no cost carries
// no stream text and returns "" so no row prints.
func llmCallSummary(e agent.LlmCallEvent) string {
	model := ""
	if !e.Synthetic && e.ResponseModel != "" {
		model = e.ResponseModel
	} else if e.Model != "" {
		model = e.Model
	}
	if model == "" && e.InputTokens == 0 && e.OutputTokens == 0 &&
		e.CachedInputTokens == 0 && e.ObservedCostUsd == nil {
		return ""
	}
	var parts []string
	if model != "" {
		parts = append(parts, model)
	}
	if e.InputTokens > 0 || e.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("in %d out %d", e.InputTokens, e.OutputTokens))
	}
	if e.CachedInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("cached %d", e.CachedInputTokens))
	}
	if e.ObservedCostUsd != nil {
		parts = append(parts, fmt.Sprintf("$%.4f", *e.ObservedCostUsd))
	}
	if e.TurnCompleted {
		parts = append(parts, "turn")
	}
	if e.Synthetic {
		parts = append(parts, "(aggregate)")
	}
	return collapseWS(strings.Join(parts, " "))
}

// contextUsageSummary renders a harness context notice as its kind plus
// the reported measurements: the consumed window share and the input
// total. Unknown measurements stay absent, never zero. An event with no
// kind and no measurement carries no stream text and returns "".
func contextUsageSummary(e agent.ContextUsageEvent) string {
	var parts []string
	if kind := string(e.NoticeKind); kind != "" {
		parts = append(parts, kind)
	}
	if e.UsedPercent != nil {
		parts = append(parts, strconv.FormatFloat(*e.UsedPercent, 'f', -1, 64)+"%")
	}
	if e.InputTokens != nil {
		parts = append(parts, fmt.Sprintf("in %d", *e.InputTokens))
	}
	return collapseWS(strings.Join(parts, " "))
}

// subagentSummary renders a delegation lifecycle tick as the delegating
// tool and its phase. Either half absent still prints the other; both
// absent carries no stream text and returns "".
func subagentSummary(e agent.SubagentEvent) string {
	phase := string(e.Phase)
	switch {
	case e.ToolName != "" && phase != "":
		return e.ToolName + " " + phase
	case e.ToolName != "":
		return e.ToolName
	default:
		return phase
	}
}

// collapseWS collapses runs of whitespace (incl. newlines) to single
// spaces so a multi-line tool arg renders as one clean stream line.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// streamTickerText returns the short "last activity" string a card shows in
// its activity row, derived from the most recent meaningful event.
func streamTickerText(ev TailEvent) string {
	_, summary := summarizeEvent(ev)
	return cleanText(summary)
}

// defaultStreamLines is the merged stream's retained scroll-back.
const defaultStreamLines = 10_000

// streamPane is the merged session stream: a bounded ring of rendered
// lines shown newest-last. Following, the pane shows the newest lines;
// paused, it holds the window it showed when paused while new lines keep
// arriving (until the ring drops them). It renders exactly the rows it is
// given, each truncated to the pane width, and adds no chrome of its own —
// the model draws the title row — so plain output never carries color.
type streamPane struct {
	lines  []string
	max    int
	total  int // lines ever appended
	follow bool
	pinned int // total at pause time: the paused window ends here
}

func newStreamPane() *streamPane {
	return &streamPane{max: defaultStreamLines, follow: true}
}

// Append adds rendered lines, dropping the oldest beyond the ring size.
func (s *streamPane) Append(lines ...string) {
	s.lines = append(s.lines, lines...)
	s.total += len(lines)
	if over := len(s.lines) - s.max; over > 0 {
		s.lines = append(s.lines[:0], s.lines[over:]...)
	}
}

// Following reports whether the pane tracks the newest line.
func (s *streamPane) Following() bool { return s.follow }

// SetFollowing resumes following, or pauses on the current window.
func (s *streamPane) SetFollowing(follow bool) {
	if !follow && s.follow {
		s.pinned = s.total
	}
	s.follow = follow
}

// View returns exactly height rows (padded with blank rows), each at most
// width cells. height <= 0 returns every retained line, newest last.
func (s *streamPane) View(width, height int) []string {
	end := len(s.lines)
	if !s.follow {
		end -= s.total - s.pinned
		end = max(end, 0)
	}
	start := 0
	if height > 0 {
		start = max(end-height, 0)
	}
	out := make([]string, 0, max(height, end-start))
	for _, line := range s.lines[start:end] {
		out = append(out, truncateWidth(line, width))
	}
	for len(out) < height {
		out = append(out, "")
	}
	return out
}
