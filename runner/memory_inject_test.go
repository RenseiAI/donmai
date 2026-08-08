package runner

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// TestDrainMemoryInjects_DeliversBufferedBlock verifies the Wave 3 runtime
// memory drain: a buffered InjectPayload at the post-terminal seam is
// delivered into the live session and the resume turn's events (including
// the assistant event carrying the block text) are re-consumed + mirrored.
func TestDrainMemoryInjects_DeliversBufferedBlock(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := r.registry.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := withCtx(t)
	defer cancel()
	handle, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{
			"stub.behavior": string(stub.BehaviorInjectTest),
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	// Pre-buffer one inject as the heartbeat transport would have.
	injectCh := make(chan heartbeat.InjectPayload, 8)
	const memText = "recall: reuse afclient/retry.go, do not write a new retry loop"
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: memText}

	rec := &recordingSink{}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-1")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	r.drainMemoryInjects(ctx, handle, wpath, qw, res, enforcer, rec, nil, injectCh)

	// The resume turn must have produced an AssistantTextEvent echoing the
	// injected block (the stub prefixes it with "injected: ").
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var sawBlock bool
	for _, ev := range rec.events {
		if at, ok := ev.(agent.AssistantTextEvent); ok && strings.Contains(at.Text, memText) {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatalf("expected an assistant event carrying the memory block text; got %v", kindsOf(rec.events))
	}
}

// TestDrainMemoryInjects_EmptyChannelNoop verifies a no-op drain when no
// inject was buffered — the helper returns a zero observation without
// touching the handle.
func TestDrainMemoryInjects_EmptyChannelNoop(t *testing.T) {
	r := minimalRunner(t)

	injectCh := make(chan heartbeat.InjectPayload, 8)
	rec := &recordingSink{}
	handle := &fakeHandle{events: make(chan agent.Event)} // never read
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-2")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := r.drainMemoryInjects(ctx, handle, t.TempDir(), qw, res, enforcer, rec, nil, injectCh)

	if got.terminalEvent != nil || got.pullRequestURL != "" {
		t.Fatalf("expected zero observation on empty drain, got %+v", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 0 {
		t.Fatalf("expected no events mirrored on empty drain, got %v", kindsOf(rec.events))
	}
}

// TestDrainMemoryInjects_SkipsEmptyText verifies that an inject with
// whitespace-only text is dropped (never delivered to the handle) — the
// empty-text gate.
func TestDrainMemoryInjects_SkipsEmptyText(t *testing.T) {
	r := minimalRunner(t)

	// recordInjectHandle records every Inject call so we can assert the
	// empty-text block was never delivered.
	handle := &recordInjectHandle{events: make(chan agent.Event)}

	injectCh := make(chan heartbeat.InjectPayload, 8)
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-empty", Text: "   \n\t "}

	rec := &recordingSink{}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-MEM-3")}
	res := &Result{SessionID: qw.SessionID}
	enforcer := NewBudgetEnforcer(nil, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.drainMemoryInjects(ctx, handle, t.TempDir(), qw, res, enforcer, rec, nil, injectCh)

	if got := handle.injectCount(); got != 0 {
		t.Fatalf("empty-text block must not be delivered; Inject called %d times", got)
	}
}

// newTestInjectAcceptor builds the PRODUCTION accept callback (loop.go's
// newInjectAcceptor — the exact function runLoop wires into the heartbeat).
// These tests deliberately call the real thing: the previous local mirror of
// this closure meant deleting the production code left them green.
// ackOnBuffer=true is the headless/interview wiring these tests cover;
// TestInjectAcceptor_AckMode covers both modes side by side.
func newTestInjectAcceptor(seenInject map[string]struct{}, injectCh chan heartbeat.InjectPayload) func(heartbeat.InjectPayload) bool {
	return newInjectAcceptor(injectCh, seenInject, slog.New(slog.DiscardHandler), "test-session", true)
}

// TestInjectAcceptor_AckMode pins WHERE the ack belongs, per run mode.
//
// The regression: the acceptor acked the instant a payload landed on the
// 8-slot channel. For an interactive session — whose write waits on a human
// and whose session can end at any moment — that stamped up to nine payloads
// delivered while nothing had been written; the platform then never re-offered
// them. ackOnBuffer=false is the fix: take custody, do NOT claim delivery, and
// let the consumer confirm with AckInject once the bytes land.
func TestInjectAcceptor_AckMode(t *testing.T) {
	tests := []struct {
		name        string
		ackOnBuffer bool
		wantFresh   bool // ack for a first-time delivery that buffers fine
		wantSeen    bool // ack for a re-offer of something already buffered
	}{
		{name: "headless and interview ack on buffer", ackOnBuffer: true, wantFresh: true, wantSeen: true},
		{name: "interactive acks on delivery, not on buffer", ackOnBuffer: false, wantFresh: false, wantSeen: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			injectCh := make(chan heartbeat.InjectPayload, 2)
			seen := map[string]struct{}{}
			onInject := newInjectAcceptor(
				injectCh, seen, slog.New(slog.DiscardHandler), "test-session", tc.ackOnBuffer,
			)

			p := heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "hello"}
			if got := onInject(p); got != tc.wantFresh {
				t.Fatalf("first offer acked=%v; want %v", got, tc.wantFresh)
			}
			// Custody is unconditional: the payload must be readable by the
			// consumer whichever ack mode is in force.
			if got := len(injectCh); got != 1 {
				t.Fatalf("payload was not buffered (len=%d); custody must not depend on the ack mode", got)
			}
			// A re-offer of the same delivery must not double-buffer, and
			// must not be acked in ack-on-delivery mode either — it has still
			// not been delivered.
			if got := onInject(p); got != tc.wantSeen {
				t.Fatalf("re-offer acked=%v; want %v", got, tc.wantSeen)
			}
			if got := len(injectCh); got != 1 {
				t.Fatalf("re-offer was double-buffered (len=%d)", got)
			}

			// A full buffer is never acked, in either mode.
			<-injectCh
			for i := 0; i < 2; i++ {
				onInject(heartbeat.InjectPayload{DeliveryID: "fill-" + string(rune('a'+i))})
			}
			if onInject(heartbeat.InjectPayload{DeliveryID: "dlv-overflow"}) {
				t.Fatal("a payload rejected for lack of buffer space must never be acked")
			}
		})
	}
}

// TestMemoryInjectOnInject_DedupesByDeliveryID exercises the dedup-by-
// DeliveryID gate that lives in the OnInject closure (the heartbeat-side
// guard against a re-delivery in the window before the ack lands). A second
// delivery with the same DeliveryID must be dropped (but still acked); a
// different one passes.
func TestMemoryInjectOnInject_DedupesByDeliveryID(t *testing.T) {
	seenInject := map[string]struct{}{}
	injectCh := make(chan heartbeat.InjectPayload, 8)
	onInject := newTestInjectAcceptor(seenInject, injectCh)

	if !onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "first"}) {
		t.Fatal("first delivery must be accepted")
	}
	// Duplicate: dropped from the buffer but ACKED (already buffered once;
	// re-acking stops the platform from re-sending forever).
	if !onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "duplicate"}) {
		t.Fatal("duplicate delivery must still report accepted (ack stops re-sends)")
	}
	if !onInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "second"}) {
		t.Fatal("second distinct delivery must be accepted")
	}

	if got := len(injectCh); got != 2 {
		t.Fatalf("expected 2 distinct injects buffered (dup dropped), got %d", got)
	}
	first := <-injectCh
	second := <-injectCh
	if first.DeliveryID != "dlv-1" || second.DeliveryID != "dlv-2" {
		t.Fatalf("unexpected dedup result: %q then %q", first.DeliveryID, second.DeliveryID)
	}
}

// TestMemoryInjectOnInject_RejectsWhenChannelFull verifies the non-blocking
// send: when injectCh is full the OnInject closure REJECTS the inject
// (returns false, so the pulser leaves it unacked and the platform
// re-delivers) rather than stalling the heartbeat goroutine — and the
// rejected DeliveryID is NOT marked seen, so the re-delivery is buffered
// once capacity frees up (ack-or-requeue, KG-04).
func TestMemoryInjectOnInject_RejectsWhenChannelFull(t *testing.T) {
	seenInject := map[string]struct{}{}
	injectCh := make(chan heartbeat.InjectPayload, 1) // tiny buffer
	onInject := newTestInjectAcceptor(seenInject, injectCh)

	if !onInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "fits"}) {
		t.Fatal("first inject must be accepted")
	}
	if onInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "overflow"}) {
		t.Fatal("expected the second inject to be rejected on a full channel")
	}
	if got := len(injectCh); got != 1 {
		t.Fatalf("channel should hold exactly 1 inject, got %d", got)
	}

	// Drain the buffered inject — the platform's re-delivery of the
	// rejected dlv-2 must now be accepted (it was never marked seen).
	<-injectCh
	if !onInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "overflow retry"}) {
		t.Fatal("re-delivery of a rejected inject must be accepted once capacity frees up")
	}
	if got := <-injectCh; got.DeliveryID != "dlv-2" {
		t.Fatalf("re-delivered inject = %q; want dlv-2", got.DeliveryID)
	}
}

// The runtime-inject gate is the provider capability ALONE
// (runtimeInjectEnabled := caps.SupportsMessageInjection in runLoop): there is
// no worker-side enable flag, because the PLATFORM decides whether a block is
// delivered at all. That gate used to be "covered" by a test that constructed
// an agent.Capabilities value and asserted the field equalled itself — it
// touched no runner code and could not fail.
//
// What actually needed covering is the seam BELOW the gate: that runLoop hands
// the channel to the dispatcher for the run mode in play. Both live consumers
// are exercised end to end instead:
//
//   - interactive → TestInteractive_RunLoopHandsInjectChToDispatch
//     (interactive_inject_test.go) drives the full runner and asserts the
//     inject reaches the live PTY as a submitted line.
//   - interview   → TestInterviewLoop_* (interview_loop_test.go) park on the
//     same channel per turn.

// recordInjectHandle is an agent.Handle that records Inject calls and never
// emits events. Used to assert which blocks are delivered.
type recordInjectHandle struct {
	events  chan agent.Event
	injects []string
}

func (h *recordInjectHandle) SessionID() string          { return "" }
func (h *recordInjectHandle) Events() <-chan agent.Event { return h.events }
func (h *recordInjectHandle) Inject(_ context.Context, text string) error {
	h.injects = append(h.injects, text)
	return nil
}
func (h *recordInjectHandle) Stop(context.Context) error { return nil }
func (h *recordInjectHandle) injectCount() int           { return len(h.injects) }
