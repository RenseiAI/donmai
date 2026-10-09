package runner

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
)

// TestInteractive_TerminalReportCarriesTranscriptUsage drives the real
// dispatchInteractive supervisor with a fixture transcript — two per-turn
// usage events with cache buckets and observed prices — to the end of the
// session and asserts the terminal report's usage matches the transcript.
// Before the supervisor metered transcript usage, an interactive session
// that ran turns ended with a nil cost, so the status the existing
// cost-event writer records carried zero tokens.
func TestInteractive_TerminalReportCarriesTranscriptUsage(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	worktreePath := t.TempDir()
	session := completedRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	turnOneCost := 0.01
	turnTwoCost := 0.02
	base.events <- agent.LlmCallEvent{
		InputTokens: 1200, OutputTokens: 80,
		CachedInputTokens: 48000, CacheWriteTokens: 512,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &turnOneCost, TurnCompleted: true,
	}
	base.events <- agent.LlmCallEvent{
		InputTokens: 300, OutputTokens: 40,
		CachedInputTokens: 1000, CacheWriteTokens: 64,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &turnTwoCost, TurnCompleted: true,
	}
	handle := &testInteractiveHandle{Handle: base, session: session}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-usage", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	result, err := r.dispatchInteractive(
		context.Background(), handle, worktreePath, qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}

	want := agent.CostData{
		InputTokens:       1500,
		OutputTokens:      120,
		CachedInputTokens: 49000,
		CacheWriteTokens:  576,
		TotalCostUsd:      0.03,
		NumTurns:          2,
	}
	if result.Cost == nil {
		t.Fatalf("terminal Cost = nil; want %+v", want)
	}
	got := *result.Cost
	if diff := got.TotalCostUsd - want.TotalCostUsd; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("terminal TotalCostUsd = %v; want %v", got.TotalCostUsd, want.TotalCostUsd)
	}
	got.TotalCostUsd = want.TotalCostUsd
	if got != want {
		t.Errorf("terminal Cost = %+v; want %+v", got, want)
	}
}

// TestInteractive_TerminalReportWithoutUsageKeepsNilCost pins the negative
// space: a session whose transcript carried no per-turn usage still ends
// with a nil cost, so the report shape is unchanged when there is nothing
// to record.
func TestInteractive_TerminalReportWithoutUsageKeepsNilCost(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	session := completedRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	base.events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1", Input: map[string]any{"command": "echo hi"}}
	handle := &testInteractiveHandle{Handle: base, session: session}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-no-usage", Mode: interactiveRunMode}}

	result, err := minimalRunner(t).dispatchInteractive(
		context.Background(), handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, &recordingSink{}, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}
	if result.Cost != nil {
		t.Errorf("terminal Cost = %+v; want nil (no usage was metered)", *result.Cost)
	}
}

// TestInteractiveUsageTotals_AddIgnoresAggregate pins the meter's evidence
// rule: aggregate/synthetic usage is derived from a rolled-up total, so it
// is neither new usage nor evidence and must not move the total.
func TestInteractiveUsageTotals_AddIgnoresAggregate(t *testing.T) {
	u := &interactiveUsageTotals{}
	turnCost := 0.01
	u.add(agent.LlmCallEvent{InputTokens: 100, OutputTokens: 10, UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &turnCost, TurnCompleted: true})
	u.add(agent.LlmCallEvent{InputTokens: 100, OutputTokens: 10, UsageSource: agent.LlmUsageAggregate, Synthetic: true, TurnCompleted: true})
	got, ok := u.snapshot()
	if !ok {
		t.Fatal("snapshot = nothing metered; want the one provider turn")
	}
	want := agent.CostData{InputTokens: 100, OutputTokens: 10, TotalCostUsd: 0.01, NumTurns: 1}
	if diff := got.TotalCostUsd - want.TotalCostUsd; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("TotalCostUsd = %v; want %v", got.TotalCostUsd, want.TotalCostUsd)
	}
	got.TotalCostUsd = want.TotalCostUsd
	if got != want {
		t.Errorf("snapshot = %+v; want %+v (aggregate turn ignored)", got, want)
	}
}

// TestInteractiveUsageTotals_UnpricedUsageKeepsTokensWithoutDollars pins
// that a turn with usage but no observed price still moves the token
// counts while leaving the dollar total at zero — never recorded as a
// priced zero.
func TestInteractiveUsageTotals_UnpricedUsageKeepsTokensWithoutDollars(t *testing.T) {
	u := &interactiveUsageTotals{}
	u.add(agent.LlmCallEvent{InputTokens: 100, OutputTokens: 10, UsageSource: agent.LlmUsageProvider, TurnCompleted: true})
	got, ok := u.snapshot()
	if !ok {
		t.Fatal("snapshot = nothing metered; want the unpriced turn's tokens")
	}
	if got.InputTokens != 100 || got.OutputTokens != 10 || got.NumTurns != 1 {
		t.Errorf("snapshot = %+v; want the turn's tokens with one turn", got)
	}
	if got.TotalCostUsd != 0 {
		t.Errorf("TotalCostUsd = %v; want 0 (no price was observed)", got.TotalCostUsd)
	}
	if snap := u.heartbeatSnapshot(); snap.TotalCostUsd != 0 || snap.InputTokens != 100 {
		t.Errorf("heartbeat snapshot = %+v; want input 100 with no dollars", snap)
	}
}

// TestInteractiveUsageTotals_HeartbeatSnapshotEmptyWhenNothingMetered pins
// that the step-heartbeat beat keeps today's body shape (no usage object)
// until the first turn's usage lands.
func TestInteractiveUsageTotals_HeartbeatSnapshotEmptyWhenNothingMetered(t *testing.T) {
	u := &interactiveUsageTotals{}
	if got := u.heartbeatSnapshot(); got.InputTokens != 0 || got.OutputTokens != 0 ||
		got.CachedInputTokens != 0 || got.TotalCostUsd != 0 {
		t.Errorf("heartbeat snapshot = %+v; want zero (emitter leaves it off the wire)", got)
	}
	if _, ok := u.snapshot(); ok {
		t.Error("snapshot reports metered usage; want nothing metered")
	}
}

// TestInteractive_StepHeartbeatReadsLiveTranscriptTotals pins the heartbeat
// half of the wiring: while the supervisor runs, the session's registered
// totals — the same value the step-heartbeat emitter reads through
// interactiveUsageForSession — already carry a turn's usage, before the
// session ends. The supervisor runs in the background behind a live PTY
// session; the test meters one usage event through the handle channel,
// observes the running totals move, then lets the session exit.
func TestInteractive_StepHeartbeatReadsLiveTranscriptTotals(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	session := liveRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	handle := &testInteractiveHandle{Handle: base, session: session}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-live-usage", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	dispatchDone := make(chan struct{})
	var result *Result
	var dispatchErr error
	go func() {
		defer close(dispatchDone)
		result, dispatchErr = r.dispatchInteractive(
			context.Background(), handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, &recordingSink{}, nil, nil, agent.NoticeDeliveryPTYNotice,
		)
	}()

	// Wait until the dispatch has registered its totals — the point from
	// which the emitter's beats carry usage.
	deadline := time.Now().Add(10 * time.Second)
	var live *interactiveUsageTotals
	for live == nil {
		r.interactiveUsageMu.Lock()
		live = r.interactiveUsage[qw.SessionID]
		r.interactiveUsageMu.Unlock()
		if live != nil {
			break
		}
		if time.Now().After(deadline) {
			close(session.done)
			<-dispatchDone
			t.Fatal("dispatch never registered its usage totals")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := live.heartbeatSnapshot(); got != (stepheartbeat.UsageSnapshot{}) {
		t.Errorf("heartbeat snapshot before any turn = %+v; want zero", got)
	}

	turnCost := 0.015
	base.events <- agent.LlmCallEvent{
		InputTokens: 800, OutputTokens: 60, CachedInputTokens: 9000,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &turnCost, TurnCompleted: true,
	}
	// The supervisor meters the event off the handle channel; the
	// registered totals must move while the session is still running.
	deadline = time.Now().Add(10 * time.Second)
	for {
		if got := r.interactiveUsageForSession(qw.SessionID).heartbeatSnapshot(); got.InputTokens == 800 {
			if got.OutputTokens != 60 || got.CachedInputTokens != 9000 || got.TotalCostUsd != 0.015 {
				t.Errorf("live heartbeat snapshot = %+v; want the turn's usage", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registered totals never metered the turn; snapshot = %+v",
				r.interactiveUsageForSession(qw.SessionID).heartbeatSnapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(session.done)
	<-dispatchDone
	if dispatchErr != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, dispatchErr)
	}
	if result.Cost == nil || result.Cost.InputTokens != 800 || result.Cost.NumTurns != 1 {
		t.Errorf("terminal Cost = %+v; want the live-metered turn", result.Cost)
	}
	// The dispatch releases its totals on return — no session entry leaks.
	r.interactiveUsageMu.Lock()
	_, leaked := r.interactiveUsage[qw.SessionID]
	r.interactiveUsageMu.Unlock()
	if leaked {
		t.Error("usage totals still registered after dispatch returned")
	}
}

// TestInteractive_StoppedReportCarriesTranscriptUsage pins the stop exit
// (runner cancel or the wall-clock cap, both of which end interactiveCtx):
// the turns metered before the stop still spent tokens, so the stopped
// report carries them — the same rule the headless lane's stop paths
// follow with the budget meter. Before, this exit returned with a nil cost
// and the session recorded nothing.
func TestInteractive_StoppedReportCarriesTranscriptUsage(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	session := liveRecordingInteractiveSession()
	t.Cleanup(func() { close(session.done) })
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	handle := &testInteractiveHandle{Handle: base, session: session}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-stopped-usage", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatchDone := make(chan struct{})
	var result *Result
	var dispatchErr error
	go func() {
		defer close(dispatchDone)
		result, dispatchErr = r.dispatchInteractive(
			ctx, handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, &recordingSink{}, nil, nil, agent.NoticeDeliveryPTYNotice,
		)
	}()

	turnCost := 0.02
	base.events <- agent.LlmCallEvent{
		InputTokens: 500, OutputTokens: 50, CachedInputTokens: 4000,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &turnCost, TurnCompleted: true,
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.interactiveUsageForSession(qw.SessionID).heartbeatSnapshot().InputTokens != 500 {
		if time.Now().After(deadline) {
			cancel()
			<-dispatchDone
			t.Fatal("supervisor never metered the turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-dispatchDone

	if dispatchErr == nil || result.Status != "stopped" {
		t.Fatalf("dispatchInteractive = status %q err %v, want stopped with the ctx error", result.Status, dispatchErr)
	}
	want := agent.CostData{InputTokens: 500, OutputTokens: 50, CachedInputTokens: 4000, TotalCostUsd: 0.02, NumTurns: 1}
	if result.Cost == nil || *result.Cost != want {
		t.Fatalf("stopped Cost = %+v; want %+v", result.Cost, want)
	}
}

// TestInteractiveUsageTotals_ImpossibleValuesReadAsUnreported pins that a
// transcript turn no real model call produces never becomes a wrong
// number: a negative token count or a running total that would overflow
// turns the whole total into "nothing metered" (nil cost, no heartbeat
// usage), and a dollar sum that leaves the finite range drops the dollars
// — keeping the tokens — instead of carrying +Inf, which would fail to
// encode and lose the terminal report.
func TestInteractiveUsageTotals_ImpossibleValuesReadAsUnreported(t *testing.T) {
	t.Parallel()
	price := 0.01
	huge := math.MaxFloat64 / 1.5
	negative := -0.5
	good := agent.LlmCallEvent{InputTokens: 100, OutputTokens: 10, UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &price, TurnCompleted: true}
	tests := []struct {
		name       string
		events     []agent.LlmCallEvent
		wantOK     bool
		wantTokens int64 // InputTokens, when wantOK
		wantUsd    float64
	}{
		{
			name: "negative token count",
			events: []agent.LlmCallEvent{
				good,
				{InputTokens: -90, OutputTokens: 10, UsageSource: agent.LlmUsageProvider, TurnCompleted: true},
			},
		},
		{
			name: "negative count first",
			events: []agent.LlmCallEvent{
				{CachedInputTokens: -1, UsageSource: agent.LlmUsageProvider, TurnCompleted: true}, good,
			},
		},
		{
			name: "token sum overflows",
			events: []agent.LlmCallEvent{
				good,
				{InputTokens: math.MaxInt64, UsageSource: agent.LlmUsageProvider, TurnCompleted: true},
			},
		},
		{
			name: "dollar sum leaves the finite range",
			events: []agent.LlmCallEvent{
				{InputTokens: 1, UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &huge, TurnCompleted: true},
				{InputTokens: 1, UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &huge, TurnCompleted: true},
				good,
			},
			wantOK: true, wantTokens: 102, wantUsd: 0,
		},
		{
			name: "negative price",
			events: []agent.LlmCallEvent{
				good,
				{InputTokens: 1, UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &negative, TurnCompleted: true},
			},
			wantOK: true, wantTokens: 101, wantUsd: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &interactiveUsageTotals{}
			for _, ev := range tc.events {
				u.add(ev)
			}
			got, ok := u.snapshot()
			if ok != tc.wantOK {
				t.Fatalf("snapshot ok = %v (%+v); want %v", ok, got, tc.wantOK)
			}
			res := &Result{}
			u.applyTo(res)
			if !tc.wantOK {
				if res.Cost != nil {
					t.Errorf("terminal Cost = %+v; want nil (unreported)", *res.Cost)
				}
				if hb := u.heartbeatSnapshot(); hb != (stepheartbeat.UsageSnapshot{}) {
					t.Errorf("heartbeat snapshot = %+v; want zero (unreported)", hb)
				}
				return
			}
			if got.InputTokens != tc.wantTokens || got.TotalCostUsd != tc.wantUsd {
				t.Errorf("snapshot = %+v; want input %d with $%v", got, tc.wantTokens, tc.wantUsd)
			}
			if _, err := json.Marshal(res.Cost); err != nil {
				t.Errorf("terminal Cost does not encode: %v", err)
			}
		})
	}
}

// TestStepHeartbeatUsage_ReadsTheSessionLanesMeter pins the emitter wiring
// runLoop hands the step heartbeat: an interactive session's beats carry the
// transcript-tail totals its supervisor registered, while a headless session
// keeps reading the budget enforcer's meter. The interactive lane never
// meters through the enforcer, so reading it there would leave every
// interactive beat usage-less.
func TestStepHeartbeatUsage_ReadsTheSessionLanesMeter(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	const sessionID = "step-usage-lane"
	transcriptCost := 0.02
	totals := &interactiveUsageTotals{}
	totals.add(agent.LlmCallEvent{
		InputTokens: 700, OutputTokens: 70, CachedInputTokens: 7000,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: &transcriptCost, TurnCompleted: true,
	})
	r.registerInteractiveUsage(sessionID, totals)
	t.Cleanup(func() { r.releaseInteractiveUsage(sessionID) })

	enforcer := NewBudgetEnforcer(nil, time.Now())
	if breach := enforcer.ObserveEvent(agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: 300, OutputTokens: 30, TotalCostUsd: 0.01, NumTurns: 1}}); breach != nil {
		t.Fatalf("ObserveEvent with no budget = %v; want no breach", breach)
	}

	interactive := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: sessionID, Mode: interactiveRunMode}}
	if got, want := r.stepHeartbeatUsage(interactive, enforcer)(context.Background()),
		(stepheartbeat.UsageSnapshot{InputTokens: 700, OutputTokens: 70, CachedInputTokens: 7000, TotalCostUsd: 0.02}); got != want {
		t.Errorf("interactive beat usage = %+v; want the transcript totals %+v", got, want)
	}
	headless := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: sessionID}}
	if got, want := r.stepHeartbeatUsage(headless, enforcer)(context.Background()),
		(stepheartbeat.UsageSnapshot{InputTokens: 300, OutputTokens: 30, TotalCostUsd: 0.01}); got != want {
		t.Errorf("headless beat usage = %+v; want the enforcer's meter %+v", got, want)
	}
}

// TestInteractive_HarnessAccountedCostWinsOverLiveTranscriptTotals pins how
// the two interactive usage sources compose: a harness that reads its own
// transcript at exit puts the session totals on its terminal ResultEvent,
// while its live per-turn events also feed the transcript-tail meter. The
// terminal report carries the harness-accounted totals — never the live
// sum, and never the two added together.
func TestInteractive_HarnessAccountedCostWinsOverLiveTranscriptTotals(t *testing.T) {
	requireSh(t)
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	r := minimalRunner(t)
	sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	accounted := &agent.CostData{InputTokens: 900, OutputTokens: 90, CachedInputTokens: 9000, NumTurns: 3}
	events := make(chan agent.Event, 4)
	events <- agent.InitEvent{}
	events <- agent.LlmCallEvent{InputTokens: 100, OutputTokens: 10, UsageSource: agent.LlmUsageProvider, TurnCompleted: true}
	events <- agent.ResultEvent{Success: true, Cost: accounted}
	close(events)
	h := &costTerminalHandle{interactivePTYHandle: newInteractivePTYHandle(sess), events: events}

	qw := QueuedWork{}
	qw.SessionID = "harness-accounted"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := r.dispatchInteractive(ctx, h, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if err != nil || out.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v; want completed", out.Status, err)
	}
	if out.Cost == nil || *out.Cost != *accounted {
		t.Fatalf("Cost = %+v; want the harness-accounted totals %+v", out.Cost, *accounted)
	}
}
