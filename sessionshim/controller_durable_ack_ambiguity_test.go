package sessionshim

// Provenance: durable-ack-is-ambiguity — grep a build for this marker to prove
// a slow durable side can no longer make a reachable socket look unreachable.
//
// # THE STRAND THESE TESTS UNDO
//
// Measured on a consumer daemon: a healthy interactive session, a control plane
// answering every request 200, and one slow DURABLE receipt. The socket reader
// parked in eventBacklog.push behind a consumer that was waiting on that
// durable side — and parking the reader is exactly what makes a persistence
// receipt undeliverable, so the cursor acknowledger logged "the durable
// acknowledgement is still pending; keeping the shim connection and retrying"
// every 5 s. For one eventBacklogStallDeadline, and not a second longer: at 30 s
// the reader failed closed, the daemon read its own back-pressure as the shim's
// socket going away, quarantined the lineage `socket_unreachable`, and the
// control plane terminalized it 95 s later. The socket answered throughout.
//
// The stall deadline is asking "has this consumer STOPPED?" — and an
// outstanding durable acknowledgement is positive evidence that it has not.
// ADR-2026-08-30 D2 puts "a transport-level acknowledgement without the durable
// post-condition" on the closed list of always-ambiguous evidence, whose only
// permitted consequences are "preserve; recheck and retry; then degrade
// visibly".

import (
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// newAmbiguityTestBacklog builds a backlog with a sub-floor stall deadline (see
// newTestBacklog) plus the ambiguity predicate and bound this file exercises.
func newAmbiguityTestBacklog(budget int, stall, bound time.Duration, ambiguous func() bool) *eventBacklog {
	b := newEventBacklog(budget, 0, nil, ambiguous, bound)
	b.stall = stall
	return b
}

func ambiguityTestEvent() ControllerEvent {
	return ControllerEvent{Kind: EventOutput, Data: make([]byte, 64)}
}

// fillAmbiguityTestBacklog admits one event and returns the backlog it fills, so
// the NEXT push is guaranteed to stall.
func fillAmbiguityTestBacklog(t *testing.T, budget int, stall, bound time.Duration, ambiguous func() bool) *eventBacklog {
	t.Helper()
	b := newAmbiguityTestBacklog(budget, stall, bound, ambiguous)
	if err := b.push(ambiguityTestEvent()); err != nil {
		t.Fatalf("the first push into an empty backlog failed: %v", err)
	}
	return b
}

// TestOutstandingDurableAckHoldsTheStallOpenInsteadOfFailingClosed is the RED
// test for the field incident. A durable receipt that is merely SLOW must not
// cost the connection, however many stall deadlines it outlives.
func TestOutstandingDurableAckHoldsTheStallOpenInsteadOfFailingClosed(t *testing.T) {
	t.Parallel()
	const stall = 20 * time.Millisecond
	b := fillAmbiguityTestBacklog(t, 100, stall, time.Minute, func() bool { return true })

	pushed := make(chan error, 1)
	go func() { pushed <- b.push(ambiguityTestEvent()) }()

	// Twenty whole stall deadlines. Before the fix the reader failed closed on
	// the first one and the daemon quarantined a live shim over it.
	select {
	case err := <-pushed:
		t.Fatalf("a stalled push with an outstanding durable acknowledgement failed closed: %v", err)
	case <-time.After(20 * stall):
	}

	// The durable side answering ends it: the consumer drains, and the held
	// push completes normally rather than carrying a verdict about the socket.
	if _, ok := b.pop(); !ok {
		t.Fatal("the backlog closed while a push was held for an outstanding durable acknowledgement")
	}
	select {
	case err := <-pushed:
		if err != nil {
			t.Fatalf("the held push failed after the consumer drained: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held push never completed after the consumer drained")
	}
	if _, held := b.ambiguityHeldFor(time.Now()); held {
		t.Fatal("the ambiguity anchor outlived the stall it belonged to; an unrelated later stall would inherit a spent bound")
	}
}

// TestStalledConsumerWithNoOutstandingDurableAckStillFailsClosed is the control.
// The ambiguity hold is evidence-driven, not a blanket reprieve: a consumer that
// has genuinely stopped, with nothing outstanding to prove otherwise, keeps
// exactly the disposition it had.
func TestStalledConsumerWithNoOutstandingDurableAckStillFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		ambiguous func() bool
	}{
		{name: "no predicate at all", ambiguous: nil},
		{name: "nothing outstanding", ambiguous: func() bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := fillAmbiguityTestBacklog(t, 100, 20*time.Millisecond, time.Minute, tc.ambiguous)
			err := b.push(ambiguityTestEvent())
			if !errors.Is(err, ErrEventBacklogExceeded) {
				t.Fatalf("a stopped consumer's push = %v, want ErrEventBacklogExceeded", err)
			}
			if errors.Is(err, ErrDurableAckAmbiguityBound) {
				t.Fatalf("a stopped consumer was excused as ambiguity: %v", err)
			}
		})
	}
}

// TestDurableAckAmbiguityDegradesVisiblyAtItsBound pins the other half of the
// contract's ambiguous row: preserve and retry, THEN degrade visibly. The hold
// is bounded, and crossing the bound reports a sentinel that says what actually
// happened — never one that claims the socket was unreachable.
func TestDurableAckAmbiguityDegradesVisiblyAtItsBound(t *testing.T) {
	t.Parallel()
	const stall = 20 * time.Millisecond
	b := fillAmbiguityTestBacklog(t, 100, stall, 5*stall, func() bool { return true })

	start := time.Now()
	err := b.push(ambiguityTestEvent())
	if !errors.Is(err, ErrDurableAckAmbiguityBound) {
		t.Fatalf("push past the ambiguity bound = %v, want ErrDurableAckAmbiguityBound", err)
	}
	if errors.Is(err, ErrEventBacklogExceeded) {
		t.Fatalf("the ambiguity bound reported itself as a stopped consumer: %v", err)
	}
	// The bound is the bound: it must outlive the stall deadline it replaces,
	// or the hold is decorative.
	if held := time.Since(start); held < 4*stall {
		t.Fatalf("the push gave up after %s, want at least the %s ambiguity bound", held, 5*stall)
	}
}

// TestDurableAckAmbiguityBoundIsAbsoluteNotPerStallDeadline pins the anchor. The
// hold re-anchors the STALL clock every deadline; if it re-anchored the bound
// too, a durable side that answers nothing at all would hold the reader forever
// and the bound would never be reachable.
func TestDurableAckAmbiguityBoundIsAbsoluteNotPerStallDeadline(t *testing.T) {
	t.Parallel()
	const stall = 10 * time.Millisecond
	const bound = 6 * stall
	b := fillAmbiguityTestBacklog(t, 100, stall, bound, func() bool { return true })

	done := make(chan error, 1)
	go func() { done <- b.push(ambiguityTestEvent()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDurableAckAmbiguityBound) {
			t.Fatalf("push = %v, want ErrDurableAckAmbiguityBound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the ambiguity hold never reached its bound; re-anchoring the stall clock also re-anchored the bound")
	}
}

// TestControllerRecordsWhoEndedItsOwnStream pins the seam the daemon reads. A
// consumer that cannot tell "I hung up" from "the peer went away" has to guess,
// and the guess it made published `socket_unreachable` for a reachable socket.
func TestControllerRecordsWhoEndedItsOwnStream(t *testing.T) {
	t.Parallel()
	c := &Controller{done: make(chan struct{}), closing: make(chan struct{})}
	if cause := c.StreamEndCause(); cause != nil {
		t.Fatalf("a controller that has decided nothing reports %v, want nil", cause)
	}
	c.closeStream("durable acknowledgements stayed outstanding", ErrDurableAckAmbiguityBound)
	if cause := c.StreamEndCause(); !errors.Is(cause, ErrDurableAckAmbiguityBound) {
		t.Fatalf("stream end cause = %v, want the ambiguity sentinel", cause)
	}
	// FIRST decision wins: a later unwinding error must not overwrite the one
	// that actually caused the drop.
	c.noteStreamEndCause(errors.New("use of closed network connection"))
	if cause := c.StreamEndCause(); !errors.Is(cause, ErrDurableAckAmbiguityBound) {
		t.Fatalf("stream end cause after unwinding = %v, want the original ambiguity sentinel", cause)
	}
}

// TestDurableAckOutstandingTracksBothWaitingAndRetriedReceipts pins the
// predicate itself: a receipt whose wait bound expired is still outstanding —
// that is precisely the state the acknowledger retries in.
func TestDurableAckOutstandingTracksBothWaitingAndRetriedReceipts(t *testing.T) {
	t.Parallel()
	c := &Controller{done: make(chan struct{}), closing: make(chan struct{})}
	if c.durableAckOutstanding() {
		t.Fatal("a controller that has sent nothing reports a durable acknowledgement outstanding")
	}
	sent := shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 7}
	c.rememberPendingHeartbeatReceipt(sent)
	if !c.durableAckOutstanding() {
		t.Fatal("a heartbeat past its wait bound is not reported outstanding; the retry it is in would fail closed")
	}
	c.heartbeatMu.Lock()
	consumed := c.consumePendingHeartbeatReceiptLocked(sent)
	c.heartbeatMu.Unlock()
	if !consumed {
		t.Fatal("the late receipt was not recognised as the answer to the heartbeat that was sent")
	}
	if c.durableAckOutstanding() {
		t.Fatal("an answered receipt still reports outstanding; the hold would never end")
	}
}
