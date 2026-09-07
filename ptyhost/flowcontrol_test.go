package ptyhost

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// TestOutputGateResolvesItsMarks pins the derived configuration, including the
// one derivation that is not arithmetic: a low-water mark at or above the
// high-water mark is REPLACED rather than honoured, because without hysteresis
// the reader re-pauses on the frame after every resume and the harness runs at
// the speed of the gate's own bookkeeping.
func TestOutputGateResolvesItsMarks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		cfg               OutputFlowControl
		wantHigh, wantLow int
		wantBound         time.Duration
	}{
		{
			name:      "zero takes every default",
			wantHigh:  DefaultOutputHighWaterBytes,
			wantLow:   DefaultOutputHighWaterBytes / defaultOutputLowWaterDivisor,
			wantBound: DefaultOutputPauseBound,
		},
		{
			name:      "explicit marks are honoured",
			cfg:       OutputFlowControl{HighWaterBytes: 4096, LowWaterBytes: 1024, PauseBound: time.Minute},
			wantHigh:  4096,
			wantLow:   1024,
			wantBound: time.Minute,
		},
		{
			name:      "low water at the high water is derived instead",
			cfg:       OutputFlowControl{HighWaterBytes: 4096, LowWaterBytes: 4096},
			wantHigh:  4096,
			wantLow:   1024,
			wantBound: DefaultOutputPauseBound,
		},
		{
			name:      "low water above the high water is derived instead",
			cfg:       OutputFlowControl{HighWaterBytes: 4096, LowWaterBytes: 9000},
			wantHigh:  4096,
			wantLow:   1024,
			wantBound: DefaultOutputPauseBound,
		},
		{
			name:      "negative values take the defaults",
			cfg:       OutputFlowControl{HighWaterBytes: -1, LowWaterBytes: -1, PauseBound: -time.Second},
			wantHigh:  DefaultOutputHighWaterBytes,
			wantLow:   DefaultOutputHighWaterBytes / defaultOutputLowWaterDivisor,
			wantBound: DefaultOutputPauseBound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			gate := newOutputGate(&cfg, nil)
			t.Cleanup(gate.close)
			if gate.high != tc.wantHigh || gate.low != tc.wantLow || gate.bound != tc.wantBound {
				t.Fatalf("gate = {high:%d low:%d bound:%s}, want {high:%d low:%d bound:%s}",
					gate.high, gate.low, gate.bound, tc.wantHigh, tc.wantLow, tc.wantBound)
			}
			if gate.low >= gate.high {
				t.Fatalf("resolved marks have no hysteresis: low %d >= high %d", gate.low, gate.high)
			}
		})
	}
	if gate := newOutputGate(nil, nil); gate != nil {
		t.Fatal("a nil configuration built a gate; flow control must stay opt-in")
	}
	// The disabled gate is a nil pointer the read loop calls straight through,
	// so every method has to tolerate it rather than the loop carrying a branch.
	var disabled *outputGate
	disabled.await(nil)
	disabled.account(1)
	disabled.saturate()
	disabled.relieve()
	disabled.close()
	if got := disabled.state(); got.Paused || got.PendingBytes != 0 {
		t.Fatalf("disabled gate state = %+v, want the zero value", got)
	}
	if disabled.highWater() != 0 || disabled.lowWater() != 0 {
		t.Fatal("a disabled gate reported marks; a subscription would pause against them")
	}
}

// TestOutputGateAwaitReturnsAtThePauseBound pins the wedge guard.
//
// Back-pressure is correct while SOMETHING is still consuming. A subscriber
// whose consumer goroutine died without closing the subscription is not
// back-pressure — nothing will ever drain it — and holding the reader for it
// blocks the harness in write(2) forever. So the pause is bounded, and crossing
// the bound resumes reading and says so. Deleting forceRelease turns this RED
// by hanging.
func TestOutputGateAwaitReturnsAtThePauseBound(t *testing.T) {
	t.Parallel()
	cfg := OutputFlowControl{HighWaterBytes: 64, PauseBound: 40 * time.Millisecond}
	gate := newOutputGate(&cfg, nil)
	t.Cleanup(gate.close)
	gate.saturate()
	if !gate.state().Paused {
		t.Fatal("a saturated gate did not pause the reader")
	}

	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); gate.await(nil) }()
	select {
	case <-done:
		if held := time.Since(start); held < cfg.PauseBound {
			t.Fatalf("await returned after %s, want it to hold the whole %s bound", held, cfg.PauseBound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await never returned: a consumer that stopped without closing wedges the harness")
	}
	state := gate.state()
	if state.Paused || !state.PauseBoundReached {
		t.Fatalf("state after the bound = %+v, want resumed and marked as having reached the bound", state)
	}
	// A gate that has force-released must not silently re-pause on the same
	// unrelieved subscriber: that would be the same wedge on a loop.
	gate.saturate()
	if gate.state().Paused {
		t.Fatal("the gate re-paused for a subscriber that never drained after the bound was reached")
	}
	// Relief clears it, and the next saturation pauses normally again.
	gate.relieve()
	gate.relieve()
	if state := gate.state(); state.PauseBoundReached || state.Paused {
		t.Fatalf("state after relief = %+v, want clean", state)
	}
	gate.saturate()
	if !gate.state().Paused {
		t.Fatal("the gate never paused again after a relieved bound")
	}
}

// flowTestSpec builds a session whose child blocks on stdin until released,
// then emits lines of a known shape and exits. Blocking first is what makes the
// test deterministic: the subscription exists before a single byte is produced,
// so the pause is reached by the mechanism rather than by scheduling luck.
func flowTestSpec(t *testing.T, lines int, flow *OutputFlowControl) Spec {
	t.Helper()
	script := fmt.Sprintf(
		"stty -echo; read -r _; i=0; while [ $i -lt %d ]; do "+
			"printf 'ln%%04d-0123456789012345678901234567890123456789\\n' \"$i\"; i=$((i+1)); done",
		lines,
	)
	return Spec{
		Command:           []string{"/bin/sh", "-c", script},
		OutputFlowControl: flow,
	}
}

func waitForFlow(t *testing.T, sess *Session, want string, pred func(OutputFlowState) bool) OutputFlowState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if state := sess.OutputFlowState(); pred(state) {
			return state
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last state %+v", want, sess.OutputFlowState())
	return OutputFlowState{}
}

// drainSubscription reads a subscription to completion and returns the Output
// payloads concatenated plus every host sequence seen, in arrival order.
func drainSubscription(sub agent.InteractiveSubscription) (string, []uint64) {
	var (
		out  strings.Builder
		seqs []uint64
	)
	for frame := range sub.Frames() {
		seqs = append(seqs, frame.Seq)
		if frame.Type == attachwire.TypeOutput {
			out.Write(attachwire.DecodeOutput(frame.Payload).Data)
		}
	}
	return out.String(), seqs
}

// TestSlowConsumerPausesThePTYReaderAndLosesNothing is the pin for the whole
// mechanism.
//
// Without it a subscriber that stops draining is absorbed by an UNBOUNDED
// per-subscription queue while the reader keeps reading — so memory grows, the
// ring evicts underneath it, and the consumer that eventually catches up is
// served a Gap and a replay of everything it missed. That replay is what a
// viewer experiences as a minute of frozen terminal.
//
// With it the reader stops reading, the kernel PTY buffer fills, and the child
// blocks in write(2), which is what a terminal has always done to a program
// that outruns its reader. Nothing is dropped and nothing is reordered.
//
// Setting Spec.OutputFlowControl to nil turns this RED at the pause assertion.
func TestSlowConsumerPausesThePTYReaderAndLosesNothing(t *testing.T) {
	// The line count is load-bearing. The discriminating measurement below is
	// "how much of this output reached memory while nobody was draining it", so
	// the total has to be far larger than the high-water mark: at 4000 lines the
	// unthrottled reader buffers ~200 KB and the throttled one settles an order
	// of magnitude below that.
	const lines = 4000
	const lineBytes = 51 // "ln%04d-" + 40 digits + CRLF
	flow := &OutputFlowControl{HighWaterBytes: 2048, PauseBound: time.Minute}
	sess, err := Spawn(flowTestSpec(t, lines, flow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(t.Context()) })

	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close() //nolint:errcheck

	if _, err := sess.WriteInput([]byte("go\n")); err != nil {
		t.Fatal(err)
	}

	// Nothing is draining the subscription, so the queue crosses the high-water
	// mark and the reader stops. This is the assertion the whole change exists
	// for: the producer is throttled, rather than the buffer growing.
	paused := waitForFlow(t, sess, "the PTY reader to pause", func(s OutputFlowState) bool { return s.Paused })
	if paused.PendingBytes <= flow.HighWaterBytes {
		t.Fatalf("paused with %d pending bytes, want more than the %d high-water mark",
			paused.PendingBytes, flow.HighWaterBytes)
	}
	if paused.SaturatedSubscribers != 1 {
		t.Fatalf("paused with %d saturated subscribers, want exactly 1", paused.SaturatedSubscribers)
	}
	if paused.PauseBoundReached {
		t.Fatal("the pause bound was reached immediately; the bound is not being measured")
	}

	// The state alone proves only that the ACCOUNTING noticed. What has to be
	// true is that the READER stopped: with nothing draining, a reader that
	// keeps reading buffers the child's whole output, and a reader that stops
	// settles just above the mark and stays there. Two samples a quarter-second
	// apart, plus a ceiling far under the total, separate the two.
	settled := sess.OutputFlowState().PendingBytes
	time.Sleep(250 * time.Millisecond)
	still := sess.OutputFlowState().PendingBytes
	if still != settled {
		t.Fatalf("pending bytes moved from %d to %d while the reader was paused: it is still reading",
			settled, still)
	}
	if ceiling := lines * lineBytes / 4; still >= ceiling {
		t.Fatalf("pending bytes settled at %d against a %d high-water mark: the reader buffered "+
			"the child's output instead of declining to take it (ceiling %d, total ~%d)",
			still, flow.HighWaterBytes, ceiling, lines*lineBytes)
	}

	// The consumer comes back. The reader resumes, the child finishes, and the
	// stream the consumer sees is the whole stream.
	output, seqs := drainSubscription(sub)

	if len(seqs) == 0 {
		t.Fatal("the subscription delivered nothing")
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("host sequence %d of %d = %d, want %d: the stream was reordered or lost frames",
				i, len(seqs), seq, i+1)
		}
	}
	for i := range lines {
		want := fmt.Sprintf("ln%04d-0123456789012345678901234567890123456789", i)
		if !strings.Contains(output, want) {
			t.Fatalf("line %d (%q) is missing from a stream that was only ever back-pressured, never dropped", i, want)
		}
	}
	if idx, prev := 0, -1; true {
		for i := range lines {
			idx = strings.Index(output, fmt.Sprintf("ln%04d-", i))
			if idx <= prev {
				t.Fatalf("line %d appears at %d, not after the previous line at %d: the stream was reordered", i, idx, prev)
			}
			prev = idx
		}
	}
	if state := sess.OutputFlowState(); state.Paused {
		t.Fatalf("the reader is still paused after the consumer drained: %+v", state)
	}
}

// TestAbandonedSubscriptionReleasesThePTYReader pins the other liveness edge: a
// consumer that goes away entirely must release the harness immediately, not at
// the pause bound. Closing a subscription is not back-pressure — nothing is
// waiting for those bytes any more.
//
// Deleting abandonQueueLocked's release turns this RED: the reader stays parked
// until the pause bound, and the harness with it.
func TestAbandonedSubscriptionReleasesThePTYReader(t *testing.T) {
	flow := &OutputFlowControl{HighWaterBytes: 2048, PauseBound: time.Hour}
	sess, err := Spawn(flowTestSpec(t, 300, flow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(t.Context()) })

	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.WriteInput([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	waitForFlow(t, sess, "the PTY reader to pause", func(s OutputFlowState) bool { return s.Paused })

	if err := sub.Close(); err != nil {
		t.Fatalf("close subscription: %v", err)
	}
	state := waitForFlow(t, sess, "the reader to resume once nothing is subscribed",
		func(s OutputFlowState) bool { return !s.Paused })
	if state.SaturatedSubscribers != 0 || state.PendingBytes != 0 {
		t.Fatalf("state after the only subscriber went away = %+v, want no saturation and nothing pending", state)
	}
	if state.PauseBoundReached {
		t.Fatal("the reader resumed by exhausting the pause bound rather than by the subscriber going away")
	}
	// And the session still finishes on its own, which is what "released" has
	// to mean in the end.
	select {
	case <-sess.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the session never completed after its only subscriber went away")
	}
}
