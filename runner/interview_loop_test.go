package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// scriptedHandle is a controllable agent.Handle for interview-loop tests.
// Each "turn" is a slice of events the test feeds via PushTurn; the handle
// emits them on its events channel and records every Inject call. The
// terminal ResultEvent of a turn signals the loop to stop consuming and
// park; the next PushTurn supplies the resume turn's events.
type scriptedHandle struct {
	events chan agent.Event

	mu      sync.Mutex
	injects []string
}

func newScriptedHandle() *scriptedHandle {
	return &scriptedHandle{events: make(chan agent.Event, 16)}
}

func (h *scriptedHandle) SessionID() string          { return "scripted" }
func (h *scriptedHandle) Events() <-chan agent.Event { return h.events }

func (h *scriptedHandle) Inject(_ context.Context, text string) error {
	h.mu.Lock()
	h.injects = append(h.injects, text)
	h.mu.Unlock()
	return nil
}

func (h *scriptedHandle) Stop(context.Context) error { return nil }

// pushTurn emits a sequence of events ending in a terminal ResultEvent so
// consumeInterviewTurn returns. Non-blocking up to the channel buffer.
func (h *scriptedHandle) pushTurn(evs ...agent.Event) {
	for _, e := range evs {
		h.events <- e
	}
}

func (h *scriptedHandle) injectCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.injects))
	copy(out, h.injects)
	return out
}

// manualClock is a fake interviewClock returning a manualTimer whose fire
// channel the test trips explicitly via fire(). The loop arms ONE timer and
// resets it per park, so a single shared channel models the real behaviour.
type manualClock struct {
	timer *manualTimer
}

func newManualClock() *manualClock {
	return &manualClock{timer: &manualTimer{ch: make(chan time.Time, 1)}}
}

func (c *manualClock) NewTimer(d time.Duration) interviewTimer {
	c.timer.mu.Lock()
	c.timer.armed = d > 0
	c.timer.mu.Unlock()
	return c.timer
}

// fire trips the idle-grace timer (no-op when disarmed).
func (c *manualClock) fire() {
	c.timer.mu.Lock()
	armed := c.timer.armed
	ch := c.timer.ch
	c.timer.mu.Unlock()
	if armed && ch != nil {
		select {
		case ch <- time.Now():
		default:
		}
	}
}

// armed reports whether the loop has reset the timer to a positive duration
// (i.e. it is parked and waiting on the idle-grace channel).
func (c *manualClock) isArmed() bool {
	c.timer.mu.Lock()
	defer c.timer.mu.Unlock()
	return c.timer.armed
}

type manualTimer struct {
	mu    sync.Mutex
	ch    chan time.Time
	armed bool
}

func (t *manualTimer) Chan() <-chan time.Time { return t.ch }

func (t *manualTimer) Reset(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = d > 0
	// Drain any stale tick so a fire() from a prior park can't trip the
	// next select.
	select {
	case <-t.ch:
	default:
	}
}

func (t *manualTimer) Stop() {
	t.mu.Lock()
	t.armed = false
	t.mu.Unlock()
}

// recordingTokenSink captures every frame + SetTurn call so tests can assert
// token-delta production and turn correlation.
type recordingTokenSink struct {
	mu     sync.Mutex
	turns  []string
	frames []tokenFrame
}

func (s *recordingTokenSink) SetTurn(turnID string) {
	s.mu.Lock()
	s.turns = append(s.turns, turnID)
	s.mu.Unlock()
}

func (s *recordingTokenSink) Send(f tokenFrame) {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	s.mu.Unlock()
}

func (s *recordingTokenSink) Stop() error { return nil }

func (s *recordingTokenSink) snapshotFrames() []tokenFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tokenFrame, len(s.frames))
	copy(out, s.frames)
	return out
}

func (s *recordingTokenSink) snapshotTurns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.turns))
	copy(out, s.turns)
	return out
}

// interviewCfg assembles an interviewLoopConfig wired to the supplied fakes.
func interviewCfg(
	h *scriptedHandle,
	injectCh <-chan heartbeat.InjectPayload,
	clk interviewClock,
	tokens tokenSink,
	idleGrace time.Duration,
) interviewLoopConfig {
	return interviewLoopConfig{
		handle:    h,
		worktree:  "", // appendInterviewJSONL tolerates a missing dir best-effort
		qw:        QueuedWork{QueuedWork: queuedWorkBase("REN-ITVW")},
		res:       &Result{SessionID: "test-session-REN-ITVW"},
		sink:      &recordingSink{},
		tokens:    tokens,
		injectCh:  injectCh,
		clock:     clk,
		idleGrace: idleGrace,
	}
}

// TestInterviewLoop_QuestionParkUserInjectResumeRepeat is the core happy
// path: the agent asks a question (turn-0), the loop parks; a user inject
// arrives, the loop resumes (Inject called), the agent asks a second
// question, the loop parks again; a second user inject arrives, the agent
// resumes and emits the completion sentinel, exiting cleanly.
func TestInterviewLoop_QuestionParkUserInjectResumeRepeat(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload, 4)
	tokens := &recordingTokenSink{}

	// turn-0: opening question.
	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "What problem are you solving?"},
		agent.ResultEvent{Success: true},
	)

	// Drive the loop in a goroutine; feed injects + subsequent turns.
	type result struct {
		res *Result
		err error
	}
	resCh := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, newManualClock(), tokens, 0))
		resCh <- result{res, err}
	}()

	// Loop streamed turn-0 and is now parked. Send user turn 1.
	injectCh <- heartbeat.InjectPayload{
		DeliveryID: "dlv-1", Text: "Scheduling conflicts", Kind: interview.InjectKindUser, TurnID: "turn-1",
	}
	// Resume turn for user turn 1: second question.
	h.pushTurn(
		agent.AssistantTextEvent{Text: "Who are the users?"},
		agent.ResultEvent{Success: true},
	)

	// Send user turn 2, whose resume turn emits the completion sentinel.
	injectCh <- heartbeat.InjectPayload{
		DeliveryID: "dlv-2", Text: "Busy professionals", Kind: interview.InjectKindUser, TurnID: "turn-2",
	}
	h.pushTurn(
		agent.AssistantTextEvent{Text: "Great, scoping complete.\n" + interview.InterviewCompleteSentinel},
		agent.ResultEvent{Success: true},
	)

	select {
	case got := <-resCh:
		if got.err != nil {
			t.Fatalf("loop returned err: %v", got.err)
		}
		if got.res.Status != "completed" {
			t.Fatalf("status = %q; want completed (sentinel exit)", got.res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interview loop did not exit within 5s")
	}

	// Both user replies were injected (claude --resume) in order.
	injects := h.injectCalls()
	if len(injects) != 2 {
		t.Fatalf("expected 2 Inject calls, got %d: %v", len(injects), injects)
	}
	if injects[0] != "Scheduling conflicts" || injects[1] != "Busy professionals" {
		t.Fatalf("unexpected inject order/content: %v", injects)
	}

	// Token deltas were produced and turn-correlated: SetTurn fired for the
	// opening turn-0 plus the two user turns.
	turns := tokens.snapshotTurns()
	wantTurns := []string{"turn-0", "turn-1", "turn-2"}
	if strings.Join(turns, ",") != strings.Join(wantTurns, ",") {
		t.Fatalf("SetTurn sequence = %v; want %v", turns, wantTurns)
	}
	// Each turn produced at least one text frame + a terminal done frame.
	frames := tokens.snapshotFrames()
	var sawText, sawDone bool
	for _, f := range frames {
		if f.Text != "" && !f.Done {
			sawText = true
		}
		if f.Done {
			sawDone = true
		}
	}
	if !sawText || !sawDone {
		t.Fatalf("expected both text and done frames; sawText=%v sawDone=%v (frames=%d)", sawText, sawDone, len(frames))
	}
}

// TestInterviewLoop_SentinelExitOnFirstTurn verifies an immediate clean exit
// when the opening turn itself emits the completion sentinel (a zero-question
// interview, e.g. a fully pre-filled spec).
func TestInterviewLoop_SentinelExitOnFirstTurn(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload, 1)
	tokens := &recordingTokenSink{}

	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "All fields known.\n" + interview.InterviewCompleteSentinel},
		agent.ResultEvent{Success: true},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, newManualClock(), tokens, 0))
	if err != nil {
		t.Fatalf("loop returned err: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q; want completed", res.Status)
	}
	if got := h.injectCalls(); len(got) != 0 {
		t.Fatalf("expected no Inject calls on immediate sentinel exit, got %v", got)
	}
}

// TestInterviewLoop_SentinelStraddlesEvents verifies the sliding-tail
// detection: the sentinel split across two AssistantTextEvents within one
// turn is still detected.
func TestInterviewLoop_SentinelStraddlesEvents(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload, 1)

	half := len(interview.InterviewCompleteSentinel) / 2
	first := interview.InterviewCompleteSentinel[:half]
	second := interview.InterviewCompleteSentinel[half:]

	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "done " + first},
		agent.AssistantTextEvent{Text: second},
		agent.ResultEvent{Success: true},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, newManualClock(), &recordingTokenSink{}, 0))
	if err != nil {
		t.Fatalf("loop returned err: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q; want completed (sentinel straddling two events)", res.Status)
	}
}

// TestInterviewLoop_IdleGraceExit verifies the idle-grace exit: after the
// opening question the loop parks; no user inject arrives and the idle-grace
// timer fires, so the loop exits cleanly as completed.
func TestInterviewLoop_IdleGraceExit(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload) // never fed
	clk := newManualClock()
	tokens := &recordingTokenSink{}

	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "What's the goal?"},
		agent.ResultEvent{Success: true},
	)

	type result struct {
		res *Result
		err error
	}
	resCh := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// idleGrace > 0 so the loop requests a timer each park iteration.
		res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, clk, tokens, 30*time.Second))
		resCh <- result{res, err}
	}()

	// Give the loop time to stream turn-0 and arm its idle timer, then fire
	// it. Poll for the armed timer rather than sleeping a fixed time.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if clk.isArmed() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loop never armed an idle-grace timer")
		}
		time.Sleep(2 * time.Millisecond)
	}
	clk.fire()

	select {
	case got := <-resCh:
		if got.err != nil {
			t.Fatalf("loop returned err: %v", got.err)
		}
		if got.res.Status != "completed" {
			t.Fatalf("status = %q; want completed (idle-grace exit)", got.res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interview loop did not exit on idle-grace within 5s")
	}

	if got := h.injectCalls(); len(got) != 0 {
		t.Fatalf("expected no Inject calls on idle-grace exit, got %v", got)
	}
}

// TestInterviewLoop_CtxCancelExit verifies a clean stopped-status exit when
// ctx is cancelled while the loop is parked.
func TestInterviewLoop_CtxCancelExit(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload) // never fed

	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "Question?"},
		agent.ResultEvent{Success: true},
	)

	type result struct {
		res *Result
		err error
	}
	resCh := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, newManualClock(), &recordingTokenSink{}, 0))
		resCh <- result{res, err}
	}()

	// Let the loop stream turn-0 and park, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-resCh:
		if got.res.Status != "stopped" {
			t.Fatalf("status = %q; want stopped (ctx-cancel exit)", got.res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interview loop did not exit on ctx cancel within 5s")
	}
}

// TestInterviewLoop_MemoryInjectBetweenTurns verifies that a memory inject
// (Kind=="" / memory) arriving while parked is still delivered between turns
// (memory works in interview mode) and the loop keeps parking afterward — a
// subsequent user inject + sentinel then exits cleanly.
func TestInterviewLoop_MemoryInjectBetweenTurns(t *testing.T) {
	r := minimalRunner(t)
	h := newScriptedHandle()
	injectCh := make(chan heartbeat.InjectPayload, 4)
	tokens := &recordingTokenSink{}

	h.pushTurn(
		agent.InitEvent{SessionID: "prov-1"},
		agent.AssistantTextEvent{Text: "Opening question?"},
		agent.ResultEvent{Success: true},
	)

	type result struct {
		res *Result
		err error
	}
	resCh := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		res, err := r.runInterviewLoop(ctx, interviewCfg(h, injectCh, newManualClock(), tokens, 0))
		resCh <- result{res, err}
	}()

	// A memory inject (no Kind) arrives while parked; it is delivered and its
	// resume turn streamed, then the loop parks again.
	injectCh <- heartbeat.InjectPayload{DeliveryID: "mem-1", Text: "recall: prefer existing scheduler lib"}
	h.pushTurn(
		agent.AssistantTextEvent{Text: "(noted) Continuing question?"},
		agent.ResultEvent{Success: true},
	)

	// Then a real user turn whose resume emits the sentinel.
	injectCh <- heartbeat.InjectPayload{
		DeliveryID: "dlv-9", Text: "yes", Kind: interview.InjectKindUser, TurnID: "turn-9",
	}
	h.pushTurn(
		agent.AssistantTextEvent{Text: "done\n" + interview.InterviewCompleteSentinel},
		agent.ResultEvent{Success: true},
	)

	select {
	case got := <-resCh:
		if got.err != nil {
			t.Fatalf("loop returned err: %v", got.err)
		}
		if got.res.Status != "completed" {
			t.Fatalf("status = %q; want completed", got.res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interview loop did not exit within 5s")
	}

	// Both the memory block and the user reply were injected, in order.
	injects := h.injectCalls()
	if len(injects) != 2 {
		t.Fatalf("expected 2 Inject calls (memory + user), got %d: %v", len(injects), injects)
	}
	if !strings.Contains(injects[0], "prefer existing scheduler lib") {
		t.Fatalf("first inject should be the memory block, got %q", injects[0])
	}
	if injects[1] != "yes" {
		t.Fatalf("second inject should be the user reply, got %q", injects[1])
	}
	// The memory turn's token deltas were stamped with the synthetic
	// turn-mem-<deliveryId> correlation id.
	turns := tokens.snapshotTurns()
	var sawMemTurn bool
	for _, tn := range turns {
		if tn == "turn-mem-mem-1" {
			sawMemTurn = true
		}
	}
	if !sawMemTurn {
		t.Fatalf("expected a turn-mem-mem-1 SetTurn for the memory resume turn, got %v", turns)
	}
}
