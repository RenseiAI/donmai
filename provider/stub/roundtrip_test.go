package stub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Test_Roundtrip_SucceedWithPR drives a full Spawn → drain → assert
// cycle for the canonical successful sequence. It checks the full
// shape (including structural fields, not just kinds) so a future
// drift in the F.4 smoke contract surfaces here too.
func Test_Roundtrip_SucceedWithPR(t *testing.T) {
	t.Parallel()
	p, err := New(WithSessionIDFunc(func() string { return "stub-session-test" }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorSucceedWithPR)},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if got := h.SessionID(); got != "stub-session-test" {
		t.Fatalf("SessionID: got %q want stub-session-test", got)
	}

	var events []agent.Event
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
loop:
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				break loop
			}
			events = append(events, ev)
		case <-deadline.C:
			t.Fatalf("timed out waiting for events; collected %d so far", len(events))
		}
	}

	if len(events) != 7 {
		t.Fatalf("expected 7 events, got %d: %v", len(events), eventKinds(events))
	}

	init, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("event[0] not InitEvent: %T", events[0])
	}
	if init.SessionID != "stub-session-test" {
		t.Fatalf("InitEvent.SessionID = %q want stub-session-test", init.SessionID)
	}

	sys, ok := events[1].(agent.SystemEvent)
	if !ok {
		t.Fatalf("event[1] not SystemEvent: %T", events[1])
	}
	if sys.Subtype != "starting" {
		t.Fatalf("SystemEvent.Subtype = %q want starting", sys.Subtype)
	}

	// Tool-use → tool-result pairing.
	tu, ok := events[3].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("event[3] not ToolUseEvent: %T", events[3])
	}
	tr, ok := events[4].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event[4] not ToolResultEvent: %T", events[4])
	}
	if tu.ToolUseID != tr.ToolUseID || tu.ToolUseID == "" {
		t.Fatalf("tool-use/result id pairing broken: tu=%q tr=%q", tu.ToolUseID, tr.ToolUseID)
	}
	if tu.ToolName != "Bash" {
		t.Fatalf("ToolUseEvent.ToolName = %q want Bash", tu.ToolName)
	}
	if tr.IsError {
		t.Fatalf("ToolResultEvent.IsError should be false for succeed-with-pr")
	}

	// WORK_RESULT marker appears in second AssistantTextEvent.
	at, ok := events[5].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event[5] not AssistantTextEvent: %T", events[5])
	}
	if !strings.Contains(at.Text, "WORK_RESULT:passed") {
		t.Fatalf("AssistantTextEvent should carry WORK_RESULT marker, got %q", at.Text)
	}

	// Terminal Result.
	result, ok := events[6].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event[6] not ResultEvent: %T", events[6])
	}
	if !result.Success {
		t.Fatalf("ResultEvent.Success = false want true")
	}
	if !strings.Contains(result.Message, stubPullRequestURL) {
		t.Fatalf("ResultEvent.Message should reference stub PR URL, got %q", result.Message)
	}
	if result.Cost == nil || result.Cost.TotalCostUsd != 0.001 {
		t.Fatalf("ResultEvent.Cost mismatch: %+v", result.Cost)
	}
}

// eventKinds extracts the kind sequence from a slice of events; used
// for diagnostic messages on failure.
func eventKinds(evs []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind()
	}
	return out
}
