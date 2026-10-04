package runner

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// TestRun_ToolCallsSumAcrossContinuation pins that the terminal envelope
// sums tool calls across the initial turn and a continuation turn.
func TestRun_ToolCallsSumAcrossContinuation(t *testing.T) {
	// The first turn stops early (no verdict, no pull request), so the
	// runner continues it; the continuation delivers the verdict. A qa
	// turn needs no pull request, so the run completes after one
	// continuation turn.
	res := runScripted(t, WorkTypeQAStr, false, "",
		verdictScriptTurn{toolCalls: 2, text: "Working."},
		verdictScriptTurn{toolCalls: 3, text: "Done.\nWORK_RESULT: passed"},
	)
	if res.ToolCalls != 5 {
		t.Fatalf("ToolCalls = %d; want 5 (2 initial + 3 continuation)", res.ToolCalls)
	}
}

// TestRun_ToolCallsZeroWhenNoToolCall pins that a run with no tool calls
// reports 0 on the envelope (not absent): the zero value serializes on
// the wire because agent.Result.ToolCalls has no omitempty.
func TestRun_ToolCallsZeroWhenNoToolCall(t *testing.T) {
	res := runScripted(t, WorkTypeQAStr, true, "",
		verdictScriptTurn{text: "Done.\nWORK_RESULT: passed"},
	)
	if res.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d; want 0", res.ToolCalls)
	}
	var wire map[string]any
	raw, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("marshal Result: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	v, present := wire["toolCalls"]
	if !present {
		t.Fatalf("toolCalls absent from wire JSON; want explicit 0")
	}
	if v != float64(0) {
		t.Fatalf("toolCalls = %v; want 0", v)
	}
}

// TestRun_ToolCallsCountedOnStoppedRun pins that a run the runner stops
// mid-turn still reports the tool calls that turn made: the turn makes four
// tool calls and then wedges, the no-progress watchdog ends the session, and
// both the envelope and the terminal status the platform receives carry 4.
func TestRun_ToolCallsCountedOnStoppedRun(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:    WorkTypeQAStr,
		repository:  followUpRepository,
		pulls:       map[int]string{7: pullAtSessionCommit},
		idleTimeout: 300 * time.Millisecond,
		platform:    platform,
		turns: []verdictScriptTurn{
			{toolCalls: 4, text: "Working.", hang: true},
		},
	})
	if res.Status != "failed" || res.FailureMode != FailureNoProgress {
		t.Fatalf("Status = %q, FailureMode = %q; want failed, %q", res.Status, res.FailureMode, FailureNoProgress)
	}
	if res.ToolCalls != 4 {
		t.Fatalf("ToolCalls = %d; want 4 (the stopped turn's tool calls)", res.ToolCalls)
	}
	if got := string(platform.terminalStatus(t)["toolCalls"]); got != "4" {
		t.Fatalf("terminal status toolCalls = %s; want 4", got)
	}
}

// TestRun_ToolCallsAndScalarsAcrossBufferedInjects drains two buffered
// memory injects after the initial turn. The first inject turn reports the
// pull request and the review verdict; the second reports neither. The
// envelope must keep the first inject turn's pull request and verdicts, and
// count every turn's tool calls exactly once (1 + 2 + 2).
func TestRun_ToolCallsAndScalarsAcrossBufferedInjects(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "recall: first memory block"})
	platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "recall: second memory block"})
	// Hold the first inject turn until the second inject is buffered (its
	// ack has reached the platform), so the drain delivers both.
	secondBuffered := func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if acks, _ := platform.injectReports(); slices.Contains(acks, "dlv-2") {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("second inject was never buffered")
	}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:          WorkTypeQAStr,
		repository:        followUpRepository,
		pulls:             map[int]string{7: pullAtSessionCommit},
		heartbeatInterval: 10 * time.Millisecond,
		platform:          platform,
		turns: []verdictScriptTurn{
			{toolCalls: 1, text: "Reviewing."},
			{
				toolCalls: 2, before: secondBuffered,
				text: "Reviewed " + followUpPR + "\nREVIEW_VERDICT: APPROVE\nWORK_RESULT: passed",
			},
			{toolCalls: 2, text: "Noted."},
		},
	})
	if len(provider.prompts) != 2 {
		t.Fatalf("injected prompts = %d; want 2 (both memory injects): %q", len(provider.prompts), provider.prompts)
	}
	if res.ToolCalls != 5 {
		t.Fatalf("ToolCalls = %d; want 5 (1 initial + 2 + 2 inject turns, each once)", res.ToolCalls)
	}
	if res.PullRequestURL != followUpPR {
		t.Errorf("PullRequestURL = %q; want the first inject turn's %q", res.PullRequestURL, followUpPR)
	}
	if res.ReviewVerdict != "APPROVE" {
		t.Errorf("ReviewVerdict = %q; want APPROVE from the first inject turn", res.ReviewVerdict)
	}
	if res.WorkResult != "passed" {
		t.Errorf("WorkResult = %q; want passed from the first inject turn", res.WorkResult)
	}
}

// TestApplyTo_DoesNotCountToolCalls pins that applying an observation never
// touches the tool-call count: consumeEvents meters it, so an observation
// applied more than once (per inject turn, then by the caller) cannot
// double count.
func TestApplyTo_DoesNotCountToolCalls(t *testing.T) {
	t.Parallel()
	res := &Result{}
	obs := streamObservation{toolCalls: 3}
	obs.applyTo(res, agent.ProviderStub)
	obs.applyTo(res, agent.ProviderStub)
	if res.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d; want 0 (applyTo must not count)", res.ToolCalls)
	}
}
