package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/theme"
)

func at() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) }

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
