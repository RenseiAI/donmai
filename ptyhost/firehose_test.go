package ptyhost

import (
	"context"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestFirehoseBackpressure is the T10 plan's 10 MB/s firehose backpressure
// test. It spawns a real dd producer under a PTY session that sustains well
// over 10 MB/s for well over 5s (measured, not assumed — see the throughput
// asserts below) and exercises two subscriber shapes against it.
//
// Designed semantics (read from ring.go / session.go before writing these
// assertions):
//
//   - The RING (ring.go: `type ring struct{ maxBytes int; curBytes int;
//     frames []attachwire.Frame }`) is a single, byte-bounded buffer shared by
//     the Session for §13 resume: ring.append evicts from the front whenever
//     curBytes exceeds maxBytes (Spec.RingBytes, default 8 MiB). This bound is
//     enforced unconditionally on every publish, independent of how many
//     subscribers exist or how fast they drain — SlowSubscriberVsFastPeer below
//     asserts it holds even with a stalled subscriber attached.
//   - Fan-out (session.go: Session.publishLocked) appends the frame to the
//     ring AND, for every live subscription, calls subscription.enqueue,
//     which only takes the SUBSCRIPTION's own private mutex to append to its
//     own queue slice (ring.go: `subscription.queue`) — it never blocks on a
//     channel send. The blocking channel send (`s.frames <- f`) happens only
//     in each subscription's own `pump()` goroutine. Consequence: the emit
//     path (and therefore every OTHER subscriber, and the host's own read
//     loop) can never be stalled by one slow consumer — "head-of-line
//     isolation per viewer" (spec §11.2) holds by construction.
//     SlowSubscriberVsFastPeer below asserts a fast subscriber's throughput
//     is unaffected by a concurrent non-draining one.
//   - HOWEVER: unlike the ring, `subscription.queue` (ring.go) carries NO byte
//     or length bound, no drop-oldest, no coalesce-to-snapshot, and no
//     disconnect-on-overflow. The wire spec's §11.2 frozen invariant
//     ("buffering toward any single viewer is never unbounded" /
//     `viewerSendQueueMaxBytes`, implemented as a mechanism-only primitive in
//     attachwire.TokenBucket / ViewerSendQueueMaxBytes) is explicitly relay
//     policy in the spec, but nothing in THIS package wires that mechanism to
//     a Session subscription — a stalled local consumer (a slow AttachLocal
//     reader, or a relay-forwarder goroutine backed up on the network) grows
//     its subscription queue by the full volume produced while stalled, with
//     no cap. SlowSubscriberVsFastPeer documents and quantifies this — it is
//     a real gap flagged in the T10 report, not fixed here per the lane's
//     "report, don't redesign" boundary.
func TestFirehoseBackpressure(t *testing.T) {
	if testing.Short() {
		t.Skip("firehose backpressure test skipped in -short mode (multi-second real PTY throughput run)")
	}

	t.Run("FastSubscriberKeepsUpNoGaps", testFirehoseFastSubscriber)
	t.Run("SlowSubscriberVsFastPeer", testFirehoseSlowVsFastSubscriber)
}

// firehoseCommand produces n bytes of raw zero bytes as fast as the PTY will
// carry them, using 1 MiB writes so the producer itself is not the
// bottleneck. Verified empirically (T10 report) to sustain comfortably over
// 10 MB/s through the FULL ptyhost pipeline (PTY read -> VT feed -> framing ->
// ring/fan-out -> subscription channel), not just the raw kernel PTY.
func firehoseCommand(mib int) []string {
	return []string{"sh", "-c", "dd if=/dev/zero bs=1048576 count=" + strconv.Itoa(mib) + " 2>/dev/null"}
}

// testFirehoseFastSubscriber: a single fast-draining subscriber sees strictly
// contiguous host seqs, measures a real sustained rate >=10 MB/s over >=5s,
// and process heap growth stays bounded (<64 MiB) once the firehose drains and
// a GC runs — the ring (8 MiB default) plus one subscriber's transient queue
// should never balloon when the consumer keeps up.
func testFirehoseFastSubscriber(t *testing.T) {
	// Volume and throughput floor are race-mode-aware: this lane's gate runs
	// `go test -race`, and the race detector's per-op bookkeeping measurably
	// slows the pipeline (empirically ~15 MiB/s plain -> ~3 MB/s under -race
	// on the T10 dev machine), which throttles the producer via ordinary PTY
	// buffer backpressure (dd blocks on write once the kernel PTY buffer is
	// full and our reader isn't draining fast enough to keep it empty). A
	// smaller volume keeps runtime -race-reasonable while still safely
	// clearing >=5s; the >=10 MB/s bar from the plan is enforced verbatim in
	// the plain (non -race) build, where it was verified to hold with margin.
	mib, floorMBs := 224, 10.0
	if raceEnabled {
		mib, floorMBs = 18, 2.0
	}
	s, err := Spawn(Spec{Command: firehoseCommand(mib)})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	var total int64
	var lastSeq uint64
	deadline := time.After(60 * time.Second)
loop:
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				break loop
			}
			if f.Type == attachwire.TypeControl {
				continue // out-of-namespace (§2): seq/rel_time ignored by design
			}
			if lastSeq != 0 && f.Seq != lastSeq+1 {
				t.Fatalf("seq gap: got %d after %d", f.Seq, lastSeq)
			}
			lastSeq = f.Seq
			if f.Type == attachwire.TypeOutput {
				total += int64(len(f.Payload))
			}
			if f.Type == attachwire.TypeExit {
				break loop
			}
		case <-deadline:
			t.Fatalf("timed out waiting for firehose to finish; received %d bytes so far", total)
		}
	}
	elapsed := time.Since(start)

	wantBytes := int64(mib) << 20
	if total != wantBytes {
		t.Errorf("received %d bytes, want exactly %d (dd count)", total, wantBytes)
	}
	rateMBs := float64(total) / elapsed.Seconds() / 1e6
	t.Logf("firehose: %d bytes in %v = %.2f MB/s (decimal, race=%v)", total, elapsed, rateMBs, raceEnabled)
	if elapsed < 5*time.Second {
		t.Errorf("firehose ran for only %v, want >=5s of sustained production (increase volume)", elapsed)
	}
	if rateMBs < floorMBs {
		t.Errorf("measured throughput %.2f MB/s, want >=%.1f MB/s (race=%v)", rateMBs, floorMBs, raceEnabled)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	var growth int64
	if after.HeapAlloc > before.HeapAlloc {
		growth = int64(after.HeapAlloc - before.HeapAlloc)
	}
	t.Logf("heap growth after GC: %d bytes (%.2f MiB)", growth, float64(growth)/(1<<20))
	if growth > 64<<20 {
		t.Errorf("heap grew %d bytes (%.2f MiB) with a fast-draining subscriber, want <64 MiB — a kept-up consumer should never retain more than ~ring-bound memory", growth, float64(growth)/(1<<20))
	}
}

// testFirehoseSlowVsFastSubscriber attaches a FAST subscriber and a SLOW
// (non-draining) subscriber to the SAME firehose session and asserts the
// designed semantics documented on TestFirehoseBackpressure: the ring stays
// byte-bounded regardless, the fast peer is unaffected by the slow one, and
// the slow subscriber's own queue is NOT bounded (documented gap).
func testFirehoseSlowVsFastSubscriber(t *testing.T) {
	// See testFirehoseFastSubscriber for why volume/floor are race-mode-aware.
	// Both values comfortably clear the 8 MiB default ring bound so the
	// ring-stays-bounded and slow-queue-exceeds-ring-bound assertions below
	// are meaningful regardless of timing variance.
	mib, floorMBs := 224, 10.0
	if raceEnabled {
		mib, floorMBs = 18, 2.0
	}
	s, err := Spawn(Spec{Command: firehoseCommand(mib)})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	fast, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe (fast): %v", err)
	}
	defer func() { _ = fast.Close() }()

	slowIface, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe (slow): %v", err)
	}
	slow, ok := slowIface.(*subscription)
	if !ok {
		t.Fatalf("subscription concrete type = %T, want *subscription", slowIface)
	}
	defer func() { _ = slow.Close() }()
	// The slow subscriber deliberately never reads slow.Frames() at all for
	// the duration of the firehose — the worst case for its queue, and the
	// clearest demonstration of the unbounded-growth gap.

	start := time.Now()
	var total int64
	var lastSeq uint64
	deadline := time.After(60 * time.Second)
loop:
	for {
		select {
		case f, ok := <-fast.Frames():
			if !ok {
				break loop
			}
			if f.Type == attachwire.TypeControl {
				continue
			}
			if lastSeq != 0 && f.Seq != lastSeq+1 {
				t.Fatalf("fast subscriber saw a seq gap: got %d after %d (slow peer must not affect it, session.go publishLocked / ring.go enqueue)", f.Seq, lastSeq)
			}
			lastSeq = f.Seq
			if f.Type == attachwire.TypeOutput {
				total += int64(len(f.Payload))
			}
			if f.Type == attachwire.TypeExit {
				break loop
			}
		case <-deadline:
			t.Fatalf("fast subscriber timed out (slow peer must not stall it); received %d bytes so far", total)
		}
	}
	elapsed := time.Since(start)
	rateMBs := float64(total) / elapsed.Seconds() / 1e6
	t.Logf("fast peer: %d bytes in %v = %.2f MB/s despite a concurrent non-draining subscriber (race=%v)", total, elapsed, rateMBs, raceEnabled)
	if elapsed < 5*time.Second {
		t.Errorf("fast peer ran for only %v, want >=5s of sustained production (increase volume)", elapsed)
	}
	if rateMBs < floorMBs {
		t.Errorf("fast peer measured %.2f MB/s despite a stalled peer, want >=%.1f MB/s (a slow subscriber must not throttle a fast one; race=%v)", rateMBs, floorMBs, raceEnabled)
	}

	// ---- ring.go: the shared ring stays byte-bounded regardless (hard
	// invariant, asserted directly against the unexported ring fields). ----
	s.mu.Lock()
	ringBytes := s.ring.curBytes
	ringMax := s.ring.maxBytes
	s.mu.Unlock()
	t.Logf("ring: curBytes=%d maxBytes=%d (§13 resume ring, ring.go)", ringBytes, ringMax)
	if ringBytes > ringMax {
		t.Errorf("ring.curBytes = %d exceeds ring.maxBytes = %d — the ring's byte bound (ring.go append) must never be exceeded", ringBytes, ringMax)
	}

	// ---- ring.go: the SLOW subscriber's private queue, by contrast, is NOT
	// bounded. Quantify it: it must have accumulated close to the full
	// firehose volume (the ring bound above was never applied to it), proving
	// this queue is a materially different, uncapped data structure from the
	// ring. This is the documented gap from the TestFirehoseBackpressure
	// doc comment — asserted here as the OBSERVED behavior, not redesigned. ----
	slow.mu.Lock()
	slowQueueBytes := 0
	slowQueueFrames := len(slow.queue)
	for _, f := range slow.queue {
		slowQueueBytes += len(f.Payload)
	}
	slow.mu.Unlock()
	t.Logf("slow subscriber (never drained): queued %d frames / %d bytes (ring bound was %d) — ring.go subscription.queue has no cap", slowQueueFrames, slowQueueBytes, ringMax)
	if slowQueueBytes <= ringMax {
		t.Errorf("slow subscriber only queued %d bytes (<= the ring's %d-byte bound) — expected it to exceed the ring bound, proving subscription.queue is uncapped; if this now holds, ptyhost gained a per-subscriber bound and this test (and its doc comment) need updating", slowQueueBytes, ringMax)
	}
}
