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
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// newAmbiguityTestBacklog builds a backlog with a sub-floor stall deadline AND
// a sub-floor ambiguity bound.
//
// Both are clamped unbypassably in newEventBacklog — TestDurableAckAmbiguity
// BoundIsClampedToItsFloor pins that — so, exactly as newTestBacklog does for
// the stall deadline, the fields are written directly, in-package, where a test
// reaching below a production floor is visible rather than smuggled through the
// public seam.
func newAmbiguityTestBacklog(budget int, stall, bound time.Duration, ambiguous func() bool) *eventBacklog {
	b := newEventBacklog(budget, 0, nil, ambiguous, 0, 0)
	b.stall = stall
	b.ambiguityBound = bound
	// The flow-control hold sits between the stall deadline and the drop, so a
	// bound the ambiguity tests can actually reach has to be set too; twice the
	// deadline keeps these tests about ambiguity rather than about waiting.
	b.dropBound = 2 * stall
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
	if b.ambiguityAnchored() {
		t.Fatal("the ambiguity anchor outlived the stall it belonged to; an unrelated later stall would inherit a spent bound")
	}
}

// TestDurableAckAmbiguityBoundIsClampedToItsFloor pins the seam B2 asked for.
// The bound exists to OUTLIVE the stall deadline it holds open, so a bound
// below that deadline does not tune the hold, it deletes it — the reader fails
// closed at whichever comes first, and the measured incident returns through
// the very knob added to prevent it.
func TestDurableAckAmbiguityBoundIsClampedToItsFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		configured time.Duration
		stall      time.Duration
		want       time.Duration
	}{
		{
			name: "zero takes the package default",
			want: DurableAckAmbiguityBound,
		},
		{
			name:       "a bound under the floor is raised to it",
			configured: time.Second,
			want:       durableAckAmbiguityFloor,
		},
		{
			name:       "a bound under the resolved stall deadline is raised to that",
			configured: time.Second,
			stall:      durableAckAmbiguityFloor + time.Minute,
			want:       durableAckAmbiguityFloor + time.Minute,
		},
		{
			name:       "a bound above both is honoured",
			configured: time.Hour,
			want:       time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clampDurableAckAmbiguityBound(tc.configured, tc.stall); got != tc.want {
				t.Fatalf("clampDurableAckAmbiguityBound(%s, %s) = %s, want %s",
					tc.configured, tc.stall, got, tc.want)
			}
			// The option seam must reach the same answer: an embedder cannot
			// configure a shorter bound by any route.
			opts := ControllerOptions{DurableAckAmbiguityBound: tc.configured, EventBacklogStallDeadline: tc.stall}
			if got := newEventBacklog(1024, opts.eventBacklogStallDeadline(), nil,
				func() bool { return true }, opts.durableAckAmbiguityBound(), 0).ambiguityBound; got != tc.want {
				t.Fatalf("backlog built through the option seam has ambiguityBound = %s, want %s", got, tc.want)
			}
		})
	}
	// The floor is the stall deadline itself, not some smaller number that
	// merely looks like a floor.
	if durableAckAmbiguityFloor < eventBacklogStallDeadline {
		t.Fatalf("ambiguity floor %s is shorter than the %s stall deadline it must outlive",
			durableAckAmbiguityFloor, eventBacklogStallDeadline)
	}
}

// outstandingCorrelations is the ledger durableAckOutstanding reads.
func outstandingCorrelations(c *Controller) []heartbeatCorrelation {
	c.heartbeatMu.Lock()
	defer c.heartbeatMu.Unlock()
	return append([]heartbeatCorrelation(nil), c.pendingReceipts...)
}

// TestAnsweredReceiptExpiresOlderOutstandingCorrelations pins the sticky-latch
// fix END TO END, through acceptHeartbeatReceipt — the only way the ledger is
// ever pruned in production.
//
// Pinning the pruning helper directly proves the helper works; it stays green
// with both production call sites deleted, which is the state that reintroduces
// the bug. So both of acceptHeartbeatReceipt's resolution paths are driven
// here: the one that resolves a LIVE call, and the one that consumes a LATE
// receipt.
//
// The bug: a retry never re-sends the correlation that stalled — the cursor has
// advanced — so without expiry one slow receipt leaves the ledger non-empty for
// the life of the controller, and every LATER stall on it holds the full
// ambiguity bound instead of failing closed at the stall deadline.
func TestAnsweredReceiptExpiresOlderOutstandingCorrelations(t *testing.T) {
	t.Parallel()
	t.Run("resolving a live call clears what it overtook", func(t *testing.T) {
		t.Parallel()
		c := &Controller{done: make(chan struct{}), closing: make(chan struct{})}
		// A stall at seq 7 left a correlation nothing will ever answer.
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 7})
		// A correlation from a LATER generation is not overtaken by this beat.
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 4, AckedSeq: 2})
		// The retry is in flight at the advanced cursor.
		live := shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 9}
		call := &heartbeatCall{expected: live, done: make(chan heartbeatResult, 1)}
		c.heartbeatCall = call

		if err := c.acceptHeartbeatReceipt(shimwire.HeartbeatMsg{
			Generation: 3, AckedSeq: 9, Phase: shimwire.PhaseRunning,
		}); err != nil {
			t.Fatalf("acceptHeartbeatReceipt for the live call: %v", err)
		}
		if result := <-call.done; result.err != nil {
			t.Fatalf("the live call resolved with %v, want success", result.err)
		}

		remaining := outstandingCorrelations(c)
		want := []heartbeatCorrelation{{generation: 4, ackedSeq: 2}}
		if len(remaining) != len(want) || remaining[0] != want[0] {
			t.Fatalf("outstanding correlations after an answer at (3,9) = %+v, want %+v — a stale entry latches the hold on forever",
				remaining, want)
		}
	})

	t.Run("consuming a late receipt clears what it overtook", func(t *testing.T) {
		t.Parallel()
		c := &Controller{done: make(chan struct{}), closing: make(chan struct{})}
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 2, AckedSeq: 4})
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 7})
		if !c.durableAckOutstanding() {
			t.Fatal("two outstanding correlations are not reported outstanding")
		}

		if err := c.acceptHeartbeatReceipt(shimwire.HeartbeatMsg{
			Generation: 3, AckedSeq: 7, Phase: shimwire.PhaseRunning,
		}); err != nil {
			t.Fatalf("acceptHeartbeatReceipt for a late receipt: %v", err)
		}
		if remaining := outstandingCorrelations(c); len(remaining) != 0 {
			t.Fatalf("outstanding correlations after a late answer at (3,7) = %+v, want none — an older generation cannot outlive it",
				remaining)
		}
		if c.durableAckOutstanding() {
			t.Fatal("the ledger never empties; every later stall on this controller would hold the full ambiguity bound")
		}
	})

	t.Run("an older answer does not clear a newer outstanding entry", func(t *testing.T) {
		t.Parallel()
		c := &Controller{done: make(chan struct{}), closing: make(chan struct{})}
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 7})
		c.rememberPendingHeartbeatReceipt(shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 9})

		// The 7 lands late. The 9 is still genuinely outstanding: the durable
		// side has not said anything about it, and forgetting it would fail
		// closed on a consumer that really is still waiting.
		if err := c.acceptHeartbeatReceipt(shimwire.HeartbeatMsg{
			Generation: 3, AckedSeq: 7, Phase: shimwire.PhaseRunning,
		}); err != nil {
			t.Fatalf("acceptHeartbeatReceipt for the older receipt: %v", err)
		}
		remaining := outstandingCorrelations(c)
		want := []heartbeatCorrelation{{generation: 3, ackedSeq: 9}}
		if len(remaining) != len(want) || remaining[0] != want[0] {
			t.Fatalf("outstanding correlations after an answer at (3,7) = %+v, want %+v — pruning ran ahead of the evidence",
				remaining, want)
		}
	})
}

// TestAnchoredButNoLongerAmbiguousFallsThroughToTheOrdinaryVerdict pins the
// reporting bug. An anchor set by an earlier hold, once the durable side has
// answered, must not let a stall that continued for its OWN reasons report a
// ten-minute bound it reached in a second and a bit.
func TestAnchoredButNoLongerAmbiguousFallsThroughToTheOrdinaryVerdict(t *testing.T) {
	t.Parallel()
	var outstanding atomic.Bool
	outstanding.Store(true)
	b := fillAmbiguityTestBacklog(t, 100, 20*time.Millisecond, time.Hour, outstanding.Load)

	// One hold anchors it.
	if outcome, _ := b.holdForDurableAckAmbiguity(time.Now()); outcome != ambiguityHoldGranted {
		t.Fatalf("first hold = %d, want granted", outcome)
	}
	if !b.ambiguityAnchored() {
		t.Fatal("a granted hold left no anchor")
	}

	// The durable side answers. The consumer is still behind, but nothing is
	// outstanding any more, so this is an ordinary stopped consumer again.
	outstanding.Store(false)
	err := b.push(ambiguityTestEvent())
	if errors.Is(err, ErrDurableAckAmbiguityBound) {
		t.Fatalf("a stall that outlived its ambiguity reported the bound it never reached: %v", err)
	}
	if !errors.Is(err, ErrEventBacklogExceeded) {
		t.Fatalf("push = %v, want ErrEventBacklogExceeded", err)
	}
	if b.ambiguityAnchored() {
		t.Fatal("the anchor survived the end of ambiguity")
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
