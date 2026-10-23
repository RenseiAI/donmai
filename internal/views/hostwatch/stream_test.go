package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/theme"
)

func at() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) }

func f64p(v float64) *float64 { return &v }

func i64p(v int64) *int64 { return &v }

func TestFormatStreamLine_Plain(t *testing.T) {
	tm := theme.DefaultTheme()
	tests := []struct {
		name     string
		ev       agent.Event
		label    string
		want     string // exact for plain mode
		wantSkip bool
	}{
		{
			name:  "tool_use bash",
			ev:    agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "pnpm test"}},
			label: "ENG-1284",
			want:  "14:02:11 [ENG-1284] tool_use  Bash: pnpm test",
		},
		{
			name:  "tool_result ok",
			ev:    agent.ToolResultEvent{ToolName: "Read", Content: "x"},
			label: "ENG-1290",
			want:  "14:02:11 [ENG-1290] tool_result  Read → ok",
		},
		{
			name:  "assistant text",
			ev:    agent.AssistantTextEvent{Text: "Reviewing\nthe diff"},
			label: "ENG-1290",
			want:  "14:02:11 [ENG-1290] thought  Reviewing the diff",
		},
		{
			name:     "empty assistant text skipped",
			ev:       agent.AssistantTextEvent{Text: "   "},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:  "result success",
			ev:    agent.ResultEvent{Success: true},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] result  ✓ completed",
		},
		{
			name:  "init",
			ev:    agent.InitEvent{SessionID: "p"},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] init",
		},
		{
			name:  "llm_call prefers response model",
			ev:    agent.LlmCallEvent{Model: "requested", ResponseModel: "served", InputTokens: 100, OutputTokens: 50},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  served in 100 out 50",
		},
		{
			name:  "llm_call falls back to requested model",
			ev:    agent.LlmCallEvent{Model: "requested", InputTokens: 10, OutputTokens: 5},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  requested in 10 out 5",
		},
		{
			name:  "llm_call cost present",
			ev:    agent.LlmCallEvent{Model: "m", InputTokens: 1, OutputTokens: 2, ObservedCostUsd: f64p(0.5)},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 1 out 2 $0.5000",
		},
		{
			name:  "llm_call cost nil omits price",
			ev:    agent.LlmCallEvent{Model: "m", InputTokens: 1, OutputTokens: 2},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 1 out 2",
		},
		{
			name:  "llm_call cached and turn",
			ev:    agent.LlmCallEvent{Model: "m", InputTokens: 100, OutputTokens: 50, CachedInputTokens: 10, TurnCompleted: true},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 100 out 50 cached 10 turn",
		},
		{
			name:  "llm_call with only cached tokens still renders",
			ev:    agent.LlmCallEvent{CachedInputTokens: 5},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  cached 5",
		},
		{
			name:  "llm_call with only a zero observed cost still renders",
			ev:    agent.LlmCallEvent{ObservedCostUsd: f64p(0)},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  $0.0000",
		},
		{
			name:  "llm_call synthetic aggregate ignores response model",
			ev:    agent.LlmCallEvent{Synthetic: true, Model: "requested", ResponseModel: "served", InputTokens: 10, OutputTokens: 5},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  requested in 10 out 5 (aggregate)",
		},
		{
			name:  "llm_call synthetic cost and turn",
			ev:    agent.LlmCallEvent{Synthetic: true, Model: "m", InputTokens: 1, OutputTokens: 2, ObservedCostUsd: f64p(0.01), TurnCompleted: true},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 1 out 2 $0.0100 turn (aggregate)",
		},
		{
			name:     "empty llm_call skipped",
			ev:       agent.LlmCallEvent{},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:     "llm_call turn marker alone skipped",
			ev:       agent.LlmCallEvent{TurnCompleted: true},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:  "llm_call input-only renders token part",
			ev:    agent.LlmCallEvent{Model: "m", InputTokens: 1},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 1 out 0",
		},
		{
			name:  "llm_call output-only renders token part",
			ev:    agent.LlmCallEvent{Model: "m", OutputTokens: 1},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m in 0 out 1",
		},
		{
			name:  "llm_call model-only omits token part",
			ev:    agent.LlmCallEvent{Model: "m"},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] llm_call  m",
		},
		{
			name:  "context_usage threshold with measurements",
			ev:    agent.ContextUsageEvent{NoticeKind: agent.ContextUsageThreshold, UsedPercent: f64p(62.5), InputTokens: i64p(125000)},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] context_usage  threshold 62.5% in 125000",
		},
		{
			name:  "context_usage compaction marker alone",
			ev:    agent.ContextUsageEvent{NoticeKind: agent.ContextUsageCompaction},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] context_usage  compaction",
		},
		{
			name:     "empty context_usage skipped",
			ev:       agent.ContextUsageEvent{},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:  "usage message",
			ev:    agent.UsageEvent{Message: "allowed_warning"},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] usage  allowed_warning",
		},
		{
			name:     "empty usage skipped",
			ev:       agent.UsageEvent{},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:  "subagent completed",
			ev:    agent.SubagentEvent{ToolName: "Task", Phase: agent.SubagentCompleted},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] subagent  Task completed",
		},
		{
			name:     "empty subagent skipped",
			ev:       agent.SubagentEvent{},
			label:    "ENG-1",
			wantSkip: true,
		},
		{
			name:  "subagent tool name only",
			ev:    agent.SubagentEvent{ToolName: "Task"},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] subagent  Task",
		},
		{
			name:  "subagent phase only",
			ev:    agent.SubagentEvent{Phase: agent.SubagentCompleted},
			label: "ENG-1",
			want:  "14:02:11 [ENG-1] subagent  completed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := TailEvent{SessionID: "s1", At: at(), Event: tc.ev}
			got := formatStreamLine(tm, te, tc.label, 0, true)
			if tc.wantSkip {
				if got != "" {
					t.Fatalf("want skip (empty), got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestFormatStreamLine_DecodeError(t *testing.T) {
	tm := theme.DefaultTheme()
	te := TailEvent{SessionID: "s1", At: at(), Err: errFake("bad json")}
	got := formatStreamLine(tm, te, "ENG-1", 0, true)
	if !strings.Contains(got, "decode_error") {
		t.Fatalf("want decode_error in line, got %q", got)
	}
}

func TestFormatStreamLine_LabelFallback(t *testing.T) {
	tm := theme.DefaultTheme()
	te := TailEvent{SessionID: "abcdef0123456789", At: at(), Event: agent.InitEvent{}}
	got := formatStreamLine(tm, te, "", 0, true)
	if !strings.Contains(got, "[abcdef01]") {
		t.Fatalf("want short-id label fallback, got %q", got)
	}
}

func TestToolUseSummary(t *testing.T) {
	tests := []struct {
		ev   agent.ToolUseEvent
		want string
	}{
		{agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go  test\n./..."}}, "Bash: go test ./..."},
		{agent.ToolUseEvent{ToolName: "Read", Input: map[string]any{"file_path": "a.go"}}, "Read: a.go"},
		{agent.ToolUseEvent{ToolName: "Grep", Input: map[string]any{"pattern": "foo"}}, "Grep: foo"},
		{agent.ToolUseEvent{ToolName: "Task", Input: map[string]any{"description": "do x"}}, "Task: do x"},
		{agent.ToolUseEvent{ToolName: "Unknown"}, "Unknown"},
		{agent.ToolUseEvent{}, "tool"},
	}
	for _, tc := range tests {
		if got := toolUseSummary(tc.ev); got != tc.want {
			t.Errorf("toolUseSummary(%q)=%q want %q", tc.ev.ToolName, got, tc.want)
		}
	}
}

func TestPrefixIndex_Stable(t *testing.T) {
	p := newPrefixIndex()
	a1 := p.get("sess-a")
	b1 := p.get("sess-b")
	a2 := p.get("sess-a")
	if a1 != a2 {
		t.Errorf("prefix index not stable: %d != %d", a1, a2)
	}
	if a1 == b1 {
		t.Errorf("distinct sessions share prefix index %d", a1)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
