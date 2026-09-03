package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// newTestBacklog builds a backlog whose stall deadline is set BELOW the
// production floor.
//
// newEventBacklog clamps every configured deadline to eventBacklogStallFloor,
// deliberately and unbypassably, so an embedder cannot tune the back-pressure
// into the drop it replaced (TestEventBacklogStallDeadlineIsClampedToTheFloor
// pins that). Tests still need sub-second deadlines to exercise the stall
// semantics in bounded time, so they write the field directly, in-package,
// where the intent is visible rather than smuggled through the public seam.
func newTestBacklog(budget int, stall time.Duration, abort <-chan struct{}) *eventBacklog {
	b := newEventBacklog(budget, 0, abort)
	b.stall = stall
	return b
}

func TestControllerProtocolRangeRequiresExplicitFullFrameConsumption(t *testing.T) {
	t.Parallel()
	protocolMin, protocolMax, err := (ControllerOptions{}).protocolRange()
	if err != nil || protocolMin != shimwire.V1 || protocolMax != shimwire.V2 {
		t.Fatalf("zero-value controller range = [%d,%d], %v; want released [1,2]", protocolMin, protocolMax, err)
	}
	protocolMin, protocolMax, err = (ControllerOptions{RequireFullHostFrames: true}).protocolRange()
	if err != nil || protocolMin != shimwire.V1 || protocolMax != shimwire.ProtocolMax {
		t.Fatalf("full-frame controller range = [%d,%d], %v; want [1,%d]", protocolMin, protocolMax, err, shimwire.ProtocolMax)
	}
	if _, _, err := (ControllerOptions{ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3}).protocolRange(); err == nil {
		t.Fatal("controller advertised max 3 without declaring full HostFrame consumption")
	}
	if _, _, err := (ControllerOptions{
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2, RequireFullHostFrames: true,
	}).protocolRange(); err == nil {
		t.Fatal("controller declared full HostFrame consumption while capped below v3")
	}
}

func TestVerifyHelloRequiresExactNestedRootButAllowsDegenerateLegacyOmission(t *testing.T) {
	hello := shimwire.Hello{
		Protocol: shimwire.ProtocolName, OrgID: "org", SessionID: "session", ShimID: "shim",
		PID: 42, ProcessStartedAt: 99, Phase: shimwire.PhaseRunning, WorkareaPath: "/work/root/repo",
	}
	record := Record{
		OrgID: "org", SessionID: "session", ShimID: "shim", PID: 42, ProcessStartedAt: 99,
		WorkareaPath: "/work/root/repo",
	}
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("nested missing-root verification = %v", err)
	}
	record.WorkareaRoot = "/work/root"
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); err != nil {
		t.Fatalf("exact nested root refused: %v", err)
	}
	record.WorkareaRoot = "/work/other"
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("wrong nested root verification = %v", err)
	}
	record.WorkareaRoot = ""
	hello.WorkareaPath = "/work/legacy"
	record.WorkareaPath = "/work/legacy"
	if err := verifyHello(hello, record, "/work/legacy", "/work/legacy"); err != nil {
		t.Fatalf("degenerate legacy omission refused: %v", err)
	}
}

func TestSelectedV3HeartbeatReceiptBypassesFullPublicEventBuffer(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 7, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 64), backlog: newEventBacklog(0, 0, nil),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.dispatchEvents()
	go controller.readLoop()
	peerDone := make(chan error, 1)
	burstWritten := make(chan struct{})
	allowReceipt := make(chan struct{})
	go func() {
		defer shimConn.Close() //nolint:errcheck
		reader, writer := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
		message, err := reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("heartbeat request = %s, %v", message.Type, err)
			return
		}
		heartbeat, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		const burst = 80 // deliberately greater than the public 64-event buffer
		for sequence := uint64(1); sequence <= burst; sequence++ {
			frame := attachwire.Frame{Type: attachwire.TypeOutput, Seq: sequence, Payload: []byte{byte(sequence)}}
			body, encodeErr := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: frame.Encode()})
			if encodeErr != nil {
				peerDone <- encodeErr
				return
			}
			if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
				peerDone <- err
				return
			}
		}
		close(burstWritten)
		<-allowReceipt
		receipt, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: heartbeat.Generation, AckedSeq: heartbeat.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		peerDone <- writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, receipt)
	}()

	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- controller.Heartbeat(0) }()
	<-burstWritten
	select {
	case err := <-heartbeatDone:
		t.Fatalf("Heartbeat returned before its persistence receipt: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowReceipt)
	select {
	case err := <-heartbeatDone:
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat deadlocked behind the full public event buffer")
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	for event := range controller.Events() {
		sequences = append(sequences, event.Seq)
	}
	if len(sequences) != 80 {
		t.Fatalf("priority queue delivered %d events, want 80", len(sequences))
	}
	for index, sequence := range sequences {
		if sequence != uint64(index+1) {
			t.Fatalf("priority queue sequence[%d] = %d, want %d", index, sequence, index+1)
		}
	}
}

// TestEventBacklogBudgetMatchesTheShimRing pins the equality that keeps the
// daemon from being the first component to give up on a burst.
//
// Both numbers answer the same question - how much host output may be in flight
// before this system admits it has lost some. When they disagreed (a 192-frame
// controller bound against an 8 MiB ring) the controller collapsed on volume
// the shim absorbs by design, and the Gap the ring exists to declare became
// unreachable. Sourcing one from the other is what makes that impossible.
func TestEventBacklogBudgetMatchesTheShimRing(t *testing.T) {
	t.Parallel()
	if EventBacklogBudget != ptyhost.DefaultRingBytes {
		t.Fatalf("event backlog budget = %d, want the shim ring budget %d",
			EventBacklogBudget, ptyhost.DefaultRingBytes)
	}
	if publicEventBufferLimit != 64 {
		t.Fatalf("public event buffer = %d, want 64", publicEventBufferLimit)
	}
}

// TestEventBacklogAtBudgetStallsInsteadOfDropping is the pin for the failure
// this file's budget was supposed to prevent and did not.
//
// Reaching the budget used to be a verdict on the CONNECTION: push refused, the
// read loop dropped the shim connection, the durable carrier was lost, and a
// healthy seat was quarantined and later reaped. It happened on production hosts
// twice in one day, on the resume Snapshot of a lineage with a long screen
// history — one frame, arriving at the one moment a carrier is most fragile.
//
// A consumer that is BEHIND is not a consumer that is broken. So the budget is
// now a back-pressure high-water mark: the reader stalls, the shim's pump stalls
// behind a socket nobody is draining, and the moment the consumer drains the
// stream continues on the same carrier. Restoring the immediate refusal turns
// this RED at the first assertion.
func TestEventBacklogAtBudgetStallsInsteadOfDropping(t *testing.T) {
	t.Parallel()
	const payload = 100
	budget := 4 * (eventBacklogOverheadBytes + payload)
	controller := &Controller{
		selected: shimwire.V3,
		backlog:  newTestBacklog(budget, 30*time.Second, nil),
		closing:  make(chan struct{}),
	}
	for i := range 4 {
		event := ControllerEvent{Kind: EventHostFrame, Seq: uint64(i + 1), FrameBytes: make([]byte, payload)}
		if err := controller.publishEvent(event); err != nil {
			t.Fatalf("fill backlog at %d: %v", i, err)
		}
	}

	stalled := make(chan error, 1)
	go func() {
		stalled <- controller.publishEvent(
			ControllerEvent{Kind: EventHostFrame, Seq: 5, FrameBytes: make([]byte, payload)})
	}()
	select {
	case err := <-stalled:
		t.Fatalf("publish at the budget returned %v, want a stall until the consumer drains", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Bytes, not frames: draining one event makes room, and the stalled publish
	// completes on the SAME connection.
	if _, ok := controller.backlog.pop(); !ok {
		t.Fatal("backlog drained empty")
	}
	select {
	case err := <-stalled:
		if err != nil {
			t.Fatalf("stalled publish after draining = %v, want it to land", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draining an event did not release the stalled publish")
	}
	if got := controller.backlog.queuedBytes(); got != budget {
		t.Fatalf("queued bytes = %d, want %d", got, budget)
	}
	select {
	case <-controller.closing:
		t.Fatal("the controller dropped its connection over a consumer that was merely behind")
	default:
	}
}

// TestEventBacklogFailsClosedOnlyOnAStuckConsumer keeps the other half of the
// guarantee: a consumer that produces NO progress for the whole stall deadline
// is not behind, it has stopped, and the reader must not be parked behind it
// forever — it is the only goroutine that can deliver a durable heartbeat
// receipt. Removing the deadline turns this RED by hanging.
func TestEventBacklogFailsClosedOnlyOnAStuckConsumer(t *testing.T) {
	t.Parallel()
	backlog := newTestBacklog(64, 50*time.Millisecond, nil)
	if err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 1, FrameBytes: make([]byte, 4096)}); err != nil {
		t.Fatalf("oversized first event refused: %v", err)
	}
	start := time.Now()
	err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 2})
	if !errors.Is(err, ErrEventBacklogExceeded) {
		t.Fatalf("push against a stuck consumer = %v, want ErrEventBacklogExceeded", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("push failed closed after %s, want it to stall the whole deadline first", waited)
	}
}

// TestEventBacklogDeadlineIsCumulativeNotPerPush is the pin for the hole a
// per-call timer leaves open.
//
// A consumer that hands back one small event every few seconds is not keeping
// up, but it does release a waiter — so with a timer declared per push, EVERY
// push returns before its own clock expires and the fail-closed verdict is never
// reached. The reader is then parked in push essentially forever: heartbeat
// receipts only trickle through, and the daemon's cursor acknowledger loops on
// ErrHeartbeatReceiptPending with nothing ever resolving it.
//
// The deadline therefore lives on the backlog. Moving it back onto the call (a
// `timer` declared in push and armed on first stall) turns this RED: the
// dribbling consumer is never refused.
//
// The margins are deliberately wide, and the hand-off count is asserted. An
// earlier version of this test used a tight 400ms deadline and could pass for
// the WRONG reason under load — the ticker consumer starved past the deadline,
// making it a STUCK consumer, which any implementation refuses. Requiring that
// the consumer really did dribble is what makes the pin discriminate.
func TestEventBacklogDeadlineIsCumulativeNotPerPush(t *testing.T) {
	t.Parallel()
	const payload = 100
	const stall = time.Second
	const handOff = stall / 10
	// Twenty events of headroom against ten hand-offs per deadline: the consumer
	// takes about half a budget in a full deadline, so it never earns the reset
	// however many individual pushes it releases.
	budget := 20 * (eventBacklogOverheadBytes + payload)
	backlog := newTestBacklog(budget, stall, nil)

	var delivered atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(handOff)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, ok := backlog.pop(); ok {
					delivered.Add(1)
				}
			}
		}
	}()

	deadline := time.After(20 * stall)
	for seq := uint64(1); ; seq++ {
		err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: seq, FrameBytes: make([]byte, payload)})
		if errors.Is(err, ErrEventBacklogExceeded) {
			// It must have been refused for DRIBBLING, not for having starved
			// into a stuck consumer that every implementation refuses.
			if got := delivered.Load(); got < 3 {
				t.Fatalf("refused after only %d hand-offs: the consumer starved rather than dribbled, "+
					"so this run did not exercise the cumulative bound", got)
			}
			return
		}
		if err != nil {
			t.Fatalf("push %d = %v, want a stall or the cumulative refusal", seq, err)
		}
		select {
		case <-deadline:
			t.Fatalf("a consumer that never caught up was never refused after %d hand-offs: "+
				"the stall deadline is a per-push idle timer, not a cumulative no-progress bound",
				delivered.Load())
		default:
		}
	}
}

// TestEventBacklogSaturatedConsumerKeepsItsCarrier is the pin for the other way
// a cumulative bound can be wrong: by measuring the wrong thing.
//
// Anchoring the reset on queue EMPTINESS refuses a consumer that is keeping up
// perfectly well. A saturating producer means the queue never reaches zero no
// matter how fast the consumer runs, so a carrier draining megabytes a second
// under a heavy build log accumulates against the deadline exactly like a
// consumer that has stopped, and loses its carrier — reintroducing, through the
// definition of progress, the failure class this whole mechanism exists to
// remove.
//
// Progress is therefore counted in BYTES taken. This drives the case in
// lock-step so there is no race to lose: the queue is held at the budget and
// every iteration hands over exactly one event, so it PROVABLY never reaches
// empty, while the consumer turns over many budgets' worth over many deadlines.
//
// Restoring the emptiness test (`if len(b.queue) == 0 { b.stalledSince = ... }`
// as the only reset) turns this RED at the first deadline.
// It does not run in parallel, and its stall deadline is wider than the
// mechanism needs. Both are about the CLOCK, not the property: the run hands
// events over on a wall-clock cadence and asserts none was refused, so a test
// goroutine descheduled past the deadline fails it for a reason that has
// nothing to do with how progress is measured. The margin is what a loaded
// -race run costs; the assertions are unchanged.
func TestEventBacklogSaturatedConsumerKeepsItsCarrier(t *testing.T) {
	const payload = 100
	const stall = 500 * time.Millisecond
	const depth = 4
	budget := depth * (eventBacklogOverheadBytes + payload)
	backlog := newTestBacklog(budget, stall, nil)
	event := func(seq uint64) ControllerEvent {
		return ControllerEvent{Kind: EventHostFrame, Seq: seq, FrameBytes: make([]byte, payload)}
	}

	// Fill to the budget. Every push from here has to wait for a hand-off.
	for seq := range uint64(depth) {
		if err := backlog.push(event(seq + 1)); err != nil {
			t.Fatalf("fill at %d: %v", seq, err)
		}
	}

	// Run well past several deadlines, never letting the queue reach empty.
	until := time.Now().Add(8 * stall)
	seq := uint64(depth)
	for time.Now().Before(until) {
		seq++
		landed := make(chan error, 1)
		go func() { landed <- backlog.push(event(seq)) }()
		time.Sleep(stall / 20)
		if _, ok := backlog.pop(); !ok {
			t.Fatal("backlog closed mid-run")
		}
		select {
		case err := <-landed:
			if err != nil {
				t.Fatalf("a saturated consumer that drained %d events was refused: %v — "+
					"progress is being measured as queue emptiness, which a saturating producer "+
					"never allows however fast the consumer runs", seq-uint64(depth), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hand-off did not release the stalled push")
		}
		if got := backlog.queuedBytes(); got == 0 {
			t.Fatal("the queue reached empty: this run did not exercise a saturated backlog")
		}
	}
	if drained := seq - uint64(depth); drained < uint64(4*depth) {
		t.Fatalf("only %d events were handed over; the run was too short to cross a deadline", drained)
	}
}

// TestEventBacklogDeadlineResetsWhenTheConsumerCatchesUp is the third case: the
// cumulative bound must not accumulate against a consumer that DOES catch up, or
// a long-lived busy session would eventually be refused for having been briefly
// behind minutes earlier.
//
// Deleting the reset in pop turns this RED at the second burst.
// Not parallel, and widened, for the same reason as the saturated case above:
// the bursts are paced against the deadline on the wall clock.
func TestEventBacklogDeadlineResetsWhenTheConsumerCatchesUp(t *testing.T) {
	const payload = 100
	const stall = 700 * time.Millisecond
	budget := 2 * (eventBacklogOverheadBytes + payload)
	backlog := newTestBacklog(budget, stall, nil)
	event := func(seq uint64) ControllerEvent {
		return ControllerEvent{Kind: EventHostFrame, Seq: seq, FrameBytes: make([]byte, payload)}
	}

	fillToBudgetAndStall := func(t *testing.T, first uint64) {
		t.Helper()
		for i := range uint64(2) {
			if err := backlog.push(event(first + i)); err != nil {
				t.Fatalf("fill at %d: %v", first+i, err)
			}
		}
		landed := make(chan error, 1)
		go func() { landed <- backlog.push(event(first + 2)) }()
		// Let the stall establish, then catch the consumer fully up.
		time.Sleep(stall / 2)
		for range 2 {
			if _, ok := backlog.pop(); !ok {
				t.Error("backlog closed mid-drain")
				return
			}
		}
		select {
		case err := <-landed:
			if err != nil {
				t.Fatalf("push after the consumer caught up = %v, want it to land", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("catching up did not release the stalled push")
		}
		// The pops above cleared the anchor; the released push then re-queued
		// exactly one event, so drain it too.
		if _, ok := backlog.pop(); !ok {
			t.Fatal("backlog closed before the queue emptied")
		}
		if got := backlog.queuedBytes(); got != 0 {
			t.Fatalf("queued bytes after catching up = %d, want 0", got)
		}
	}

	fillToBudgetAndStall(t, 1)
	// Well past the original deadline. A cumulative bound that never reset would
	// refuse the very first stall of the second burst.
	time.Sleep(stall)
	fillToBudgetAndStall(t, 10)
}

// TestEventBacklogStallDeadlineIsClampedToTheFloor pins the guard on the public
// knob.
//
// EventBacklogStallDeadline is exported on ControllerOptions, AdoptOptions and
// the daemon's session-shim config, and an embedder reaching for a "tighter"
// value can set it BELOW heartbeatReceiptWaitBound — at which point the reader
// fails closed while the consumer is still waiting on a receipt only the reader
// can deliver, and the knob reintroduces exactly the drop it was meant to tune
// away. The clamp lives in newEventBacklog, the one place every controller
// passes through, so no configuration route can get under it.
func TestEventBacklogStallDeadlineIsClampedToTheFloor(t *testing.T) {
	t.Parallel()
	if eventBacklogStallFloor <= heartbeatReceiptWaitBound {
		t.Fatalf("stall floor %s does not exceed the heartbeat receipt wait bound %s",
			eventBacklogStallFloor, heartbeatReceiptWaitBound)
	}
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "zero takes the default", want: eventBacklogStallDeadline},
		{name: "negative takes the default", configured: -time.Second, want: eventBacklogStallDeadline},
		{name: "under the heartbeat bound is raised", configured: 2 * time.Second, want: eventBacklogStallFloor},
		{name: "exactly the bound is still raised", configured: heartbeatReceiptWaitBound, want: eventBacklogStallFloor},
		{name: "above the floor is honoured", configured: time.Minute, want: time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := newEventBacklog(1024, tc.configured, nil).stall; got != tc.want {
				t.Fatalf("newEventBacklog(stall=%s).stall = %s, want %s", tc.configured, got, tc.want)
			}
			// And through the public option seam an embedder actually uses, which
			// is the route the clamp has to close.
			opts := ControllerOptions{EventBacklogStallDeadline: tc.configured}
			if got := newEventBacklog(1024, opts.eventBacklogStallDeadline(), nil).stall; got != tc.want {
				t.Fatalf("through ControllerOptions(stall=%s) = %s, want %s", tc.configured, got, tc.want)
			}
		})
	}
}

// TestEventBacklogAdmitsAnOversizedFrameAfterDraining pins the rule that the
// production drop actually broke: a single frame larger than the whole budget —
// a resume Snapshot of a long-lived screen is exactly that — is admitted, it
// just has to wait for the queue ahead of it. Before the fix the SAME frame was
// refused outright whenever anything at all was queued in front of it, and the
// refusal severed the carrier.
func TestEventBacklogAdmitsAnOversizedFrameAfterDraining(t *testing.T) {
	t.Parallel()
	backlog := newTestBacklog(1024, 30*time.Second, nil)
	if err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 1, FrameBytes: make([]byte, 16)}); err != nil {
		t.Fatalf("first event refused: %v", err)
	}
	oversized := ControllerEvent{Kind: EventHostFrame, Seq: 2, FrameBytes: make([]byte, 16<<10)}
	admitted := make(chan error, 1)
	go func() { admitted <- backlog.push(oversized) }()
	select {
	case err := <-admitted:
		t.Fatalf("oversized push behind a queued event returned %v, want a stall", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := backlog.pop(); !ok {
		t.Fatal("backlog drained empty")
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("oversized push after draining = %v, want it admitted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("oversized push was never admitted after the backlog drained")
	}
	event, ok := backlog.pop()
	if !ok || event.Seq != 2 || len(event.FrameBytes) != 16<<10 {
		t.Fatalf("popped %+v ok=%v, want the intact oversized frame", event.Seq, ok)
	}
}

// TestEventBacklogStallAbortsWhenTheControllerCloses pins the liveness the abort
// channel exists for: Close must not have to wait out the stall deadline on a
// read loop parked behind a queue only that read loop's unwinding would drain.
func TestEventBacklogStallAbortsWhenTheControllerCloses(t *testing.T) {
	t.Parallel()
	closing := make(chan struct{})
	backlog := newTestBacklog(64, time.Hour, closing)
	if err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 1, FrameBytes: make([]byte, 4096)}); err != nil {
		t.Fatalf("first event refused: %v", err)
	}
	aborted := make(chan error, 1)
	go func() { aborted <- backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 2}) }()
	close(closing)
	select {
	case err := <-aborted:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stalled push on close = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the controller did not release the stalled push")
	}
}

// TestSelectedV3HeartbeatReceiptTimeoutKeepsTheController replaces the pin that
// used to require the OPPOSITE — that a receipt timeout drop the connection.
//
// Measured on an installed host: a durable write that was merely slow took the
// persistence receipt past the wait bound, this controller dropped the shim
// connection over it, nothing re-adopted the shim, and it reaped its own live
// harness when its orphan deadline expired. Twice, in the same minute, on two
// healthy sessions. "The receipt has not arrived yet" is a statement about how
// fast the durable side is answering; it is not evidence that this socket is
// broken, and it is never a reason to unsupervise a running harness.
//
// So the bound now bounds ONE CALLER'S WAIT: it reports the receipt pending,
// keeps the stream, and a receipt that lands late is consumed as the answer it
// is rather than as the unsolicited frame that WOULD be a reason to drop. The
// cursor still does not advance — the shim has not said it stored that
// sequence — which is what makes retrying safe rather than optimistic.
func TestSelectedV3HeartbeatReceiptTimeoutKeepsTheController(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 11, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 1), backlog: newEventBacklog(0, 0, nil),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.readLoop()

	peerDone := make(chan error, 1)
	releaseLateReceipt := make(chan struct{})
	go func() {
		defer shimConn.Close() //nolint:errcheck
		reader, writer := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
		// The slow one: read the request and answer nothing until released.
		message, err := reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("first heartbeat request = %s, %v", message.Type, err)
			return
		}
		first, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		<-releaseLateReceipt
		late, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: first.Generation, AckedSeq: first.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, late); err != nil {
			peerDone <- err
			return
		}
		// The retry: answered promptly, proving the stream survived the stall.
		message, err = reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("retried heartbeat request = %s, %v", message.Type, err)
			return
		}
		retried, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		receipt, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: retried.Generation, AckedSeq: retried.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		peerDone <- writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, receipt)
	}()

	started := time.Now()
	err := controller.Heartbeat(1)
	if !errors.Is(err, ErrHeartbeatReceiptPending) {
		t.Fatalf("heartbeat with an unanswered receipt = %v, want ErrHeartbeatReceiptPending", err)
	}
	if waited := time.Since(started); waited < heartbeatReceiptWaitBound {
		t.Fatalf("heartbeat reported the receipt pending after %s, want at least the %s wait bound",
			waited, heartbeatReceiptWaitBound)
	}
	// THE POINT: the shim connection is still up. A closed one here is the
	// measured regression, and every later assertion would be unreachable.
	select {
	case <-controller.closing:
		t.Fatal("a pending persistence receipt dropped the shim connection")
	case <-controller.Done():
		t.Fatal("a pending persistence receipt ended the controller read loop")
	default:
	}

	// The late receipt is the answer to a heartbeat this controller really
	// sent; consuming it must not be read as an unsolicited frame.
	close(releaseLateReceipt)

	if err := controller.Heartbeat(2); err != nil {
		t.Fatalf("heartbeat retried after a pending receipt: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("peer transport: %v", err)
	}
}

func TestSelectedV3RejectsHeartbeatInterposedInsideLiveSnapshotPair(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	call := &snapshotCall{
		request: shimwire.SnapshotRequest{RequestID: 77, Generation: 7, Mode: shimwire.SnapshotEmit},
		done:    make(chan struct{}),
	}
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 7, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 64), backlog: newEventBacklog(0, 0, nil),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: map[uint64]*snapshotCall{77: call},
	}
	go controller.dispatchEvents()
	go controller.readLoop()
	frame := attachwire.Frame{
		Type: attachwire.TypeSnapshot, Seq: 1,
		Payload: (attachwire.SnapshotEnvelope{
			AtSeq: 0, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte{1},
		}).Encode(),
	}
	body, err := shimwire.EncodeHostFrame(shimwire.HostFrame{RequestID: 77, FrameBytes: frame.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	writer := shimwire.NewWriter(shimConn)
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
		t.Fatal(err)
	}
	heartbeat, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: 7, AckedSeq: 0, Phase: shimwire.PhaseRunning})
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, heartbeat); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("interposed Heartbeat did not terminate the malformed pair")
	}
	if !errors.Is(call.err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("interposed Heartbeat snapshot error = %v", call.err)
	}
	if event, ok := <-controller.Events(); ok {
		t.Fatalf("partial requested HostFrame escaped before its result: %+v", event)
	}
	_ = shimConn.Close()
}

func TestValidateAdoptionCommitRequiresExactGenerationAndExtensions(t *testing.T) {
	t.Parallel()

	wantExtensions := shimwire.Extensions{
		Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
		Required: []string{shimwire.ExtCarrierEpoch},
	}
	tests := []struct {
		name    string
		adopted shimwire.Adopted
		wantErr bool
	}{
		{name: "exact", adopted: shimwire.Adopted{Generation: 7, Extensions: wantExtensions}},
		{name: "higher generation", adopted: shimwire.Adopted{Generation: 8, Extensions: wantExtensions}, wantErr: true},
		{name: "omitted extension echo", adopted: shimwire.Adopted{Generation: 7}, wantErr: true},
		{name: "changed carrier epoch", adopted: shimwire.Adopted{
			Generation: 7,
			Extensions: shimwire.Extensions{
				Values:   map[string]string{shimwire.ExtCarrierEpoch: "20"},
				Required: []string{shimwire.ExtCarrierEpoch},
			},
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateAdoptionCommit(tc.adopted, 7, wantExtensions)
			if tc.wantErr && !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("validateAdoptionCommit = %v, want ErrAdoptionRefused", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateAdoptionCommit exact echo: %v", err)
			}
		})
	}
}

func TestDialRefusesInexactAdoptionCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*shimwire.Adopted)
		wantErr bool
	}{
		{name: "exact"},
		{name: "higher generation", mutate: func(adopted *shimwire.Adopted) { adopted.Generation++ }, wantErr: true},
		{name: "omitted extension echo", mutate: func(adopted *shimwire.Adopted) { adopted.Extensions = shimwire.Extensions{} }, wantErr: true},
		{name: "changed extension echo", mutate: func(adopted *shimwire.Adopted) {
			adopted.Extensions.Values[shimwire.ExtCarrierEpoch] = "20"
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := dialFakeAdoptionCommit(t, tc.mutate)
			if tc.wantErr && !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("Dial = %v, want ErrAdoptionRefused", err)
			}
			if tc.wantErr {
				generation, ok := authenticatedHelloGeneration(err)
				if !ok || generation != 6 {
					t.Fatalf("authenticated Hello generation = %d/%v, want 6/true", generation, ok)
				}
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Dial exact commit: %v", err)
			}
		})
	}
}

func dialFakeAdoptionCommit(t *testing.T, mutate func(*shimwire.Adopted)) error {
	t.Helper()
	registry, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-fake", SessionID: "session-fake"}
	socketPath := registry.SocketPath(id)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socketPath, RecordFileMode); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}
	device, inode, err := statSocket(socketPath)
	if err != nil {
		t.Fatalf("statSocket: %v", err)
	}
	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-fake", ProcessEpoch: 4,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socketPath, SocketDevice: device, SocketInode: inode,
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: time.Now().UnixNano(),
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		writer, reader := shimwire.NewWriter(conn), shimwire.NewReader(conn)
		hello := shimwire.Hello{
			Protocol: shimwire.ProtocolName, Min: shimwire.ProtocolMin, Max: shimwire.ProtocolMax,
			OrgID: id.OrgID, SessionID: id.SessionID,
			ShimID: record.ShimID, ProcessEpoch: record.ProcessEpoch,
			PID: self.PID, ProcessStartedAt: self.StartedAt,
			Phase: shimwire.PhaseRunning, Generation: 6,
		}
		if writeErr := writeTyped(writer, shimwire.TypeHello, func() ([]byte, error) { return shimwire.EncodeHello(hello) }); writeErr != nil {
			serverErr <- writeErr
			return
		}
		message, readErr := reader.Read()
		if readErr != nil {
			serverErr <- readErr
			return
		}
		welcome, decodeErr := shimwire.DecodeWelcome(message.Body)
		if decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		adopted := shimwire.Adopted{
			Generation: welcome.ProposedGeneration,
			Extensions: welcome.Extensions,
			Contiguous: true,
			Phase:      shimwire.PhaseRunning,
		}
		if mutate != nil {
			adopted.Extensions.Values = cloneStringMap(adopted.Extensions.Values)
			mutate(&adopted)
		}
		serverErr <- writeTyped(writer, shimwire.TypeAdopted, func() ([]byte, error) { return shimwire.EncodeAdopted(adopted) })
	}()
	extensions := shimwire.Extensions{
		Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
		Required: []string{shimwire.ExtCarrierEpoch},
	}
	controller, dialErr := Dial(context.Background(), record, ControllerOptions{
		ControllerID:       "controller-fake",
		ProposedGeneration: 7,
		Extensions:         extensions,
	})
	if controller != nil {
		_ = controller.Close()
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake shim server: %v", err)
	}
	return dialErr
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// TestFailClosedStreamDropNamesItsReason pins the diagnosis half of a real
// field failure: a controller that drops its own connection must say why.
//
// Every fail-closed decision in the read loop used to close the socket
// silently. From every later caller's side that is indistinguishable from a
// peer that went away — input, resize, and the durable heartbeat all come back
// with "use of closed network connection", minutes later, naming nothing. The
// one operator-visible line was an acknowledgement failure for a frame the
// daemon had already accepted, which points at the wrong layer entirely.
//
// Restoring the bare `_ = c.Close()` in readLoop turns this RED.
func TestFailClosedStreamDropNamesItsReason(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	defer shimConn.Close()   //nolint:errcheck

	var log strings.Builder
	controller := &Controller{
		id: Identity{OrgID: "org-drop", SessionID: "session-drop"},
		w:  shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 3, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		// A one-byte budget plus a short stall deadline makes the bound reachable
		// without writing its full production depth or waiting out the real
		// deadline; the decision under test is the same one. Nothing drains it:
		// dispatchEvents is deliberately not started, which is what makes this a
		// STUCK consumer rather than a slow one.
		events: make(chan ControllerEvent), backlog: newTestBacklog(1, 100*time.Millisecond, nil),
		logger:        slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})),
		done:          make(chan struct{}),
		closing:       make(chan struct{}),
		snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.readLoop()

	frame := attachwire.Frame{
		Type: attachwire.TypeOutput, Seq: 1,
		Payload: attachwire.EncodeOutput([]byte("x")),
	}
	body, err := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: frame.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	writer := shimwire.NewWriter(shimConn)
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
		t.Fatal(err)
	}
	// The first frame is retained even oversized (the ring's own rule); the
	// second is what exceeds the budget.
	second := attachwire.Frame{Type: attachwire.TypeOutput, Seq: 2, Payload: attachwire.EncodeOutput([]byte("y"))}
	secondBody, err := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: second.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, secondBody) }()
	select {
	case <-controller.closing:
	case <-time.After(5 * time.Second):
		t.Fatal("fail-closed queue bound did not drop the connection")
	}
	<-controller.Done()
	line := log.String()
	if !strings.Contains(line, "controller dropped its shim connection") ||
		!strings.Contains(line, "org-drop/session-drop") ||
		!strings.Contains(line, "exceeded the in-flight budget") {
		t.Fatalf("fail-closed drop log = %q, want the session and the exact reason", line)
	}
}
