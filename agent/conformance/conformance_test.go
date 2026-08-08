package conformance

import (
	"strings"
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

func TestCheckSingleInit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		events  []agent.Event
		wantErr string // substring; empty means want nil
	}{
		{
			name: "conforming: one init, first",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.AssistantTextEvent{Text: "hi"},
				agent.ResultEvent{Success: true},
			},
		},
		{
			name:    "violation: no init at all",
			events:  []agent.Event{agent.AssistantTextEvent{Text: "hi"}, agent.ResultEvent{Success: true}},
			wantErr: "no InitEvent",
		},
		{
			name: "violation: two inits re-anchor the session identity",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.InitEvent{SessionID: "ses_2"},
				agent.ResultEvent{Success: true},
			},
			wantErr: "2 InitEvents",
		},
		{
			name: "violation: init is not first",
			events: []agent.Event{
				agent.SystemEvent{Subtype: "startup"},
				agent.InitEvent{SessionID: "ses_1"},
				agent.ResultEvent{Success: true},
			},
			wantErr: "is not first",
		},
		{
			name:    "violation: empty sequence",
			events:  nil,
			wantErr: "no InitEvent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckSingleInit(tc.events)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("CheckSingleInit() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("CheckSingleInit() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("CheckSingleInit() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckCompleteAssistantTexts(t *testing.T) {
	t.Parallel()

	repeat := func(text string, n int) []agent.Event {
		out := make([]agent.Event, 0, n)
		for range n {
			out = append(out, agent.AssistantTextEvent{Text: text})
		}
		return out
	}

	cases := []struct {
		name    string
		events  []agent.Event
		wantErr bool
	}{
		{
			name: "conforming: one complete message",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.AssistantTextEvent{Text: "I have finished the requested change and opened a pull request."},
				agent.ResultEvent{Success: true},
			},
		},
		{
			name:   "conforming: no assistant text at all",
			events: []agent.Event{agent.InitEvent{SessionID: "ses_1"}, agent.ResultEvent{Success: true}},
		},
		{
			name:   "conforming: a short run below the run-length threshold",
			events: repeat("ok", perTokenRunLength-1),
		},
		{
			name:   "conforming: a long run of complete (large) messages",
			events: repeat("this message is comfortably longer than the per-token threshold", perTokenRunLength*3),
		},
		{
			name:   "conforming: small events broken up by a tool call",
			events: append(append(repeat("ok", perTokenRunLength-1), agent.ToolUseEvent{ToolName: "Bash"}), repeat("ok", perTokenRunLength-1)...),
		},
		{
			name:    "violation: a run of tiny events is per-token streaming",
			events:  repeat("tok", perTokenRunLength),
			wantErr: true,
		},
		{
			name:    "violation: per-token run at the tail of a real session",
			events:  append(append([]agent.Event{agent.InitEvent{SessionID: "ses_1"}}, repeat(" the", perTokenRunLength+4)...), agent.ResultEvent{Success: true}),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckCompleteAssistantTexts(tc.events)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckCompleteAssistantTexts() = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckEventContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		events   []agent.Event
		wantErrs []string
	}{
		{
			name: "conforming sequence",
			events: []agent.Event{
				agent.InitEvent{SessionID: "ses_1"},
				agent.AssistantTextEvent{Text: "a complete assistant message, comfortably long"},
				agent.ResultEvent{Success: true},
			},
		},
		{
			name: "reports every violation at once, not just the first",
			events: append(
				[]agent.Event{agent.AssistantTextEvent{Text: "tok"}},
				append(
					[]agent.Event{
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
						agent.AssistantTextEvent{Text: "tok"},
					},
					agent.ResultEvent{Success: true}, agent.ErrorEvent{Code: "spawn_no_result"},
				)...,
			),
			wantErrs: []string{"no InitEvent", "2 terminal events", "per-token streaming"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckEventContract(tc.events)
			if len(tc.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("CheckEventContract() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckEventContract() = nil, want errors containing %v", tc.wantErrs)
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("CheckEventContract() = %v, missing %q", err, want)
				}
			}
		})
	}
}
