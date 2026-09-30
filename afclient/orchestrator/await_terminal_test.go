package orchestrator

import (
	"context"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestAwaitTerminal_ErrorTheSessionContinuesPast pins that an error event
// which says the session continues is not the dispatch's terminal: the
// dispatch completes on the session's own successful result. An error event
// that ends the session still fails it.
func TestAwaitTerminal_ErrorTheSessionContinuesPast(t *testing.T) {
	cases := []struct {
		name string
		evs  []agent.Event
		want DispatchStatus
	}{
		{
			name: "continuing error, then success",
			evs: []agent.Event{
				agent.ErrorEvent{Message: "one tool call unproven; the session continues", Code: "policy_adjudication_missing", SessionContinues: true},
				agent.ResultEvent{Success: true},
			},
			want: DispatchCompleted,
		},
		{
			name: "fatal error",
			evs:  []agent.Event{agent.ErrorEvent{Message: "provider crashed", Code: "crashed"}},
			want: DispatchFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad := &AgentDispatch{Status: DispatchRunning}
			err := awaitTerminal(context.Background(), &scriptedHandle{events: tc.evs}, ad)
			if ad.Status != tc.want {
				t.Fatalf("Status = %q (err %v); want %q", ad.Status, err, tc.want)
			}
			if (err == nil) != (tc.want == DispatchCompleted) {
				t.Fatalf("err = %v; want an error exactly when the dispatch fails", err)
			}
		})
	}
}
