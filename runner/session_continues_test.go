package runner

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// continuingError is the error a harness reports for the record while its
// session runs on: here, the pi harness's unproven-call record for a tool
// call that ended without a policy ruling.
var continuingError = agent.ErrorEvent{
	Message:          `policy adjudication missing: tool "edit" (call "c-1") ended without a recorded policy ruling — the call is unproven and the session continues`,
	Code:             "policy_adjudication_missing",
	SessionContinues: true,
}

// malformedEditThenValidEdit is a turn's tool traffic in which an edit call
// is rejected on its arguments, the harness reports an error the session
// continues past, and the agent's corrected edit then succeeds.
func malformedEditThenValidEdit() []agent.Event {
	return []agent.Event{
		agent.ToolUseEvent{ToolName: "edit", ToolUseID: "c-1", Input: map[string]any{"path": "README.md", "edits": []any{map[string]any{"newText": "orphan"}}}},
		continuingError,
		agent.ToolResultEvent{ToolName: "edit", ToolUseID: "c-1", IsError: true, Content: `Validation failed for tool "edit": edits.0.oldText`},
		agent.ToolUseEvent{ToolName: "edit", ToolUseID: "c-2", Input: map[string]any{"path": "README.md", "edits": []any{map[string]any{"oldText": "old", "newText": "new"}}}},
		agent.ToolResultEvent{ToolName: "edit", ToolUseID: "c-2", Content: "Successfully replaced 1 block in README.md."},
	}
}

// TestRun_ErrorTheSessionContinuesPastIsNotTheRunFailure pins that an error
// event which says the session continues never becomes the run's failure: a
// session that goes on to open its pull request with a passed verdict ends
// completed. A provider error that really ends the session still fails it,
// with its own message rather than the earlier continuing one.
func TestRun_ErrorTheSessionContinuesPastIsNotTheRunFailure(t *testing.T) {
	cases := []struct {
		name string
		turn verdictScriptTurn
		want verdictWant
	}{
		{
			name: "a rejected tool call mid-session, then a passed verdict with a pull request",
			turn: verdictScriptTurn{events: malformedEditThenValidEdit(), manifest: passedManifest, text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
			want: verdictWant{status: "completed", workResult: "passed", summary: "all done", pr: followUpPR, manifest: "passed"},
		},
		{
			name: "a fatal provider error after it still fails the session",
			turn: verdictScriptTurn{events: malformedEditThenValidEdit(), manifest: passedManifest, crash: true},
			want: verdictWant{status: "failed", failureMode: FailureProviderError, workResult: "passed", manifest: "passed", errContains: "provider crashed mid turn"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runScripted(t, "development", false, "", tc.turn)
			assertVerdict(t, res, tc.want)
		})
	}
}

// TestConsumeEvents_ErrorTheSessionContinuesPast pins the stream half: an
// error that says the session continues is not recorded as the stream's
// error, and a tool call still running past it keeps the idle watchdog
// re-arming. The control is an error that ends the stream.
func TestConsumeEvents_ErrorTheSessionContinuesPast(t *testing.T) {
	t.Parallel()
	t.Run("continuing error", func(t *testing.T) {
		t.Parallel()
		r := minimalRunner(t)
		r.idleTimeout = 30 * time.Millisecond
		events := make(chan agent.Event, 4)
		events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "long"}
		events <- continuingError
		go func() {
			// Silent past several idle windows while the call runs.
			time.Sleep(120 * time.Millisecond)
			events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "long", Content: "ok"}
			events <- agent.ResultEvent{Success: true}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		obs, err := r.consumeEvents(ctx, &fakeHandle{events: events}, t.TempDir(), QueuedWork{QueuedWork: queuedWorkBase("CONTINUES-1")}, nil, NewBudgetEnforcer(nil, time.Now()), noopSink{}, nil)
		if err != nil || obs.noProgress {
			t.Fatalf("err = %v, noProgress = %v; want the running call to keep the stream alive past the error", err, obs.noProgress)
		}
		if obs.errorEvent != nil {
			t.Fatalf("errorEvent = %+v; want nil for an error the session continues past", obs.errorEvent)
		}
		if !obs.terminalSuccess {
			t.Fatal("terminalSuccess = false; want the session's own terminal")
		}
	})
	t.Run("fatal error", func(t *testing.T) {
		t.Parallel()
		r := minimalRunner(t)
		events := make(chan agent.Event, 2)
		events <- agent.ErrorEvent{Message: "provider crashed", Code: "crashed"}
		close(events)
		obs, _ := r.consumeEvents(context.Background(), &fakeHandle{events: events}, t.TempDir(), QueuedWork{QueuedWork: queuedWorkBase("CONTINUES-2")}, nil, NewBudgetEnforcer(nil, time.Now()), noopSink{}, nil)
		if obs.errorEvent == nil || obs.errorEvent.Message != "provider crashed" {
			t.Fatalf("errorEvent = %+v; want the fatal error recorded", obs.errorEvent)
		}
	})
}
