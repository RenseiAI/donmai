package conformance

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   agent.Event
		want bool
	}{
		{"result", agent.ResultEvent{Success: true}, true},
		{"error", agent.ErrorEvent{Code: "boom"}, true},
		{"init", agent.InitEvent{SessionID: "ses_1"}, false},
		{"assistant_text", agent.AssistantTextEvent{Text: "hi"}, false},
		{"llm_call", agent.LlmCallEvent{System: "opencode"}, false},
		{"tool_use", agent.ToolUseEvent{ToolName: "read"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTerminal(tc.ev); got != tc.want {
				t.Errorf("IsTerminal(%T) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func TestCheckTerminalContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		events  []agent.Event
		wantErr bool
	}{
		{
			name: "conforming: init, text, terminal result last",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.AssistantTextEvent{Text: "hi"},
				agent.LlmCallEvent{System: "opencode"},
				agent.ResultEvent{Success: true},
			},
			wantErr: false,
		},
		{
			name: "conforming: single terminal error",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.ErrorEvent{Code: "spawn_no_result"},
			},
			wantErr: false,
		},
		{
			name:    "violation: no terminal event",
			events:  []agent.Event{agent.InitEvent{SessionID: "ses_1"}, agent.AssistantTextEvent{Text: "hi"}},
			wantErr: true,
		},
		{
			name: "violation: two terminals (D-1 shape — result then spurious error)",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.ResultEvent{Success: true},
				agent.ErrorEvent{Code: "spawn_no_result"},
			},
			wantErr: true,
		},
		{
			name: "violation: event after terminal",
			events: []agent.Event{
				agent.ResultEvent{Success: true},
				agent.AssistantTextEvent{Text: "trailing"},
			},
			wantErr: true,
		},
		{
			name:    "violation: empty sequence has no terminal",
			events:  nil,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckTerminalContract(tc.events)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckTerminalContract() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
