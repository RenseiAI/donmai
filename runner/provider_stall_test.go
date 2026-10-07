package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// stallScriptProvider plays one scripted session where the FIRST turn's
// provider never responds: after the tool result its events channel stays
// open and silent, as a hung model request does. A Resume starts the next
// scripted turn on a fresh handle. It drives the PRODUCTION entry point
// (Runner.Run), not a helper: the test asserts the seat completes through
// the stall detector and the stop-and-resume rail.
type stallScriptProvider struct {
	agent.HarnessProvider
	resumeTurns [][]agent.Event
	stops       atomic.Int32
	resumes     atomic.Int32
}

func (p *stallScriptProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	h := &stallScriptHandle{
		provider: p,
		events:   make(chan agent.Event, 64),
	}
	h.events <- agent.InitEvent{SessionID: "stall-session"}
	h.events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1", Input: map[string]any{"command": "go test ./..."}}
	h.events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: "ok"}
	// Then silence: the provider accepted the follow-up model request and
	// never responds. The channel stays open so the idle watchdog — not a
	// channel close — owns the silence.
	return h, nil
}

func (p *stallScriptProvider) Resume(_ context.Context, sessionID string, _ agent.Spec) (agent.Handle, error) {
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	p.resumes.Add(1)
	n := int(p.resumes.Load()) - 1
	h := &stallScriptHandle{
		provider: p,
		events:   make(chan agent.Event, 64),
	}
	h.events <- agent.InitEvent{SessionID: "stall-session"}
	var turn []agent.Event
	if n < len(p.resumeTurns) {
		turn = p.resumeTurns[n]
	} else {
		turn = []agent.Event{agent.ResultEvent{Success: true, Message: "done"}}
	}
	for _, ev := range turn {
		h.events <- ev
	}
	return h, nil
}

type stallScriptHandle struct {
	provider *stallScriptProvider
	events   chan agent.Event
}

func (h *stallScriptHandle) SessionID() string          { return "stall-session" }
func (h *stallScriptHandle) Events() <-chan agent.Event { return h.events }
func (h *stallScriptHandle) Inject(_ context.Context, _ string) error {
	return agent.ErrUnsupported
}

func (h *stallScriptHandle) Stop(_ context.Context) error {
	h.provider.stops.Add(1)
	return nil
}

// TestRun_StalledProviderRequestIsRetriedAndCompletes is the acceptance
// test: a fake provider that accepts a request and never responds has that
// request aborted and retried within the stall bound, and the session
// completes. The first turn ends in silence after a tool result — the hung
// provider request — so the ONLY signal that moves the seat is the stall
// detector firing, the hung request being stopped, and the turn resuming.
func TestRun_StalledProviderRequestIsRetriedAndCompletes(t *testing.T) {
	provider := &stallScriptProvider{
		resumeTurns: [][]agent.Event{
			{
				agent.AssistantTextEvent{Text: "Done.\nWORK_RESULT: passed"},
				agent.ResultEvent{Success: true, Message: "Done.\nWORK_RESULT: passed"},
			},
		},
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType:             WorkTypeQAStr,
		repository:           followUpRepository,
		pulls:                map[int]string{7: pullAtSessionCommit},
		providerStallTimeout: 50 * time.Millisecond,
		providerStallRetries: 2,
		provider: func(base agent.HarnessProvider) agent.Provider {
			provider.HarnessProvider = base
			return provider
		},
		turns: []verdictScriptTurn{},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed (the stalled request is retried, not fatal)", res.Status, res.FailureMode, res.Error)
	}
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed from the resumed turn", res.WorkResult)
	}
	if provider.resumes.Load() == 0 {
		t.Fatalf("resumes = 0; want the stalled request stopped and the turn resumed")
	}
	if provider.stops.Load() == 0 {
		t.Fatalf("stops = 0; want the hung provider request aborted before the retry")
	}
	if res.TurnContinuations == nil || res.TurnContinuations.Retried != 1 {
		t.Fatalf("TurnContinuations = %+v; want one recorded stall retry", res.TurnContinuations)
	}
}

// TestRun_StalledProviderRequestExhaustedFailsAsProviderError pins the
// classification: a seat whose stalled requests keep stalling fails as
// provider-error — never no-progress — so the no-charge rule for
// provider-side failures applies. It drives the PRODUCTION entry point
// (Runner.Run) with a provider that stalls every turn.
func TestRun_StalledProviderRequestExhaustedFailsAsProviderError(t *testing.T) {
	provider := &stallScriptProvider{
		resumeTurns: [][]agent.Event{
			// The resumed turn stalls too: silence, no terminal.
			nil,
			nil,
			nil,
		},
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType:             WorkTypeQAStr,
		repository:           followUpRepository,
		pulls:                map[int]string{7: pullAtSessionCommit},
		providerStallTimeout: 50 * time.Millisecond,
		providerStallRetries: 2,
		provider: func(base agent.HarnessProvider) agent.Provider {
			provider.HarnessProvider = base
			return provider
		},
		turns: []verdictScriptTurn{},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status = %q, FailureMode = %q; want failed/%s (a stall that exhausts its retries is a provider error, not no-progress)", res.Status, res.FailureMode, FailureProviderError)
	}
	if res.TurnContinuations == nil || res.TurnContinuations.Retried != 2 || !res.TurnContinuations.Exhausted {
		t.Fatalf("TurnContinuations = %+v; want 2 retries and exhausted", res.TurnContinuations)
	}
	if provider.resumes.Load() != 2 {
		t.Fatalf("resumes = %d; want 2 (one per stall retry)", provider.resumes.Load())
	}
}

// TestRun_StalledProviderRequestDisabledSkipsRetries pins the opt-out: a
// negative retry count disables the stall retries, so a stall ends the
// seat without resuming. It drives the PRODUCTION entry point
// (Runner.Run).
func TestRun_StalledProviderRequestDisabledSkipsRetries(t *testing.T) {
	provider := &stallScriptProvider{
		resumeTurns: [][]agent.Event{
			{
				agent.AssistantTextEvent{Text: "Done.\nWORK_RESULT: passed"},
				agent.ResultEvent{Success: true, Message: "Done.\nWORK_RESULT: passed"},
			},
		},
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType:             WorkTypeQAStr,
		repository:           followUpRepository,
		pulls:                map[int]string{7: pullAtSessionCommit},
		providerStallTimeout: 50 * time.Millisecond,
		providerStallRetries: -1,
		idleTimeout:          time.Hour,
		provider: func(base agent.HarnessProvider) agent.Provider {
			provider.HarnessProvider = base
			return provider
		},
		turns: []verdictScriptTurn{},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status = %q, FailureMode = %q; want failed/%s", res.Status, res.FailureMode, FailureProviderError)
	}
	if provider.resumes.Load() != 0 {
		t.Fatalf("resumes = %d; want 0 (retries disabled)", provider.resumes.Load())
	}
}

// detector at the consumeEvents level: silence with no tool call in flight
// flags providerStall (not just noProgress), while the idle watchdog still
// owns the outer window. Driven by wall-clock timers at millisecond scale
// so no manual-clock handshake is involved.
func TestConsumeEvents_StallDetectorFiresWithoutToolCallInFlight(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = 10 * time.Second
	r.providerStallTimeout = 30 * time.Millisecond

	events := make(chan agent.Event)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("STALL-1")}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil)
	if !obs.providerStall {
		t.Fatalf("obs.providerStall = false; want true (silence with no tool in flight is a stall)")
	}
	if obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (the stall is its own signal, not the idle cut-off)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled (detector cancels the stream ctx)", err)
	}
}

// TestConsumeEvents_SlowToolDoesNotTripStallDetector is the invariant: a
// slow tool with no output keeps the detector re-armed while the call runs,
// so the stream stays alive past the stall window. The tool's own bounded
// timeout still ends the CALL with an error the agent sees.
func TestConsumeEvents_SlowToolDoesNotTripStallDetector(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = 10 * time.Second
	r.providerStallTimeout = 30 * time.Millisecond
	clk := newIdleManualClock()
	r.idleClock = clk

	events := make(chan agent.Event, 4)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("STALL-2")}

	type consumeResult struct {
		obs streamObservation
		err error
	}
	done := make(chan consumeResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		enforcer := NewBudgetEnforcer(nil, time.Now())
		obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil)
		done <- consumeResult{obs: obs, err: err}
	}()

	// The call starts; the consumer observes it synchronously before the
	// first idle re-arm, so waiting for reset 1 proves the call is tracked
	// as in flight.
	events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "slow-1"}
	clk.waitForIdleResets(t, 1)

	// Two silent stall windows elapse while the call runs; the detector
	// must re-arm instead of flagging, so the stream stays alive.
	clk.fire()
	clk.waitForIdleResets(t, 2)
	clk.fire()
	clk.waitForIdleResets(t, 3)
	select {
	case res := <-done:
		t.Fatalf("consumeEvents returned early: err = %v, providerStall = %v; want the slow tool to keep the stream alive", res.err, res.obs.providerStall)
	default:
	}

	events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "slow-1", Content: "done after a long run"}
	events <- agent.ResultEvent{Success: true}

	var res consumeResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumeEvents did not return after the tool result and terminal event")
	}
	if res.err != nil {
		t.Fatalf("consumeEvents: %v; want nil (slow tool must suppress the stall detector)", res.err)
	}
	if res.obs.providerStall {
		t.Fatalf("obs.providerStall = true; want false (a tool in flight is not a stalled request)")
	}
	if !res.obs.terminalSuccess {
		t.Fatalf("obs.terminalSuccess = false; want true (turn completed after the tool result)")
	}
}
