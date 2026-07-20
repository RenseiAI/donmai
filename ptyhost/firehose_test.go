package ptyhost

import (
	"context"
	"math"
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

const firehoseMinimumDuration = 5 * time.Second

type firehoseCalibration struct {
	warmupMiB      int
	targetDuration time.Duration
	minMiB         int
	maxMiB         int
	fallbackMiB    int
	floorMBs       float64
}

func firehoseCalibrationForMode(race bool) firehoseCalibration {
	if race {
		return firehoseCalibration{
			warmupMiB:      8,
			targetDuration: 8 * time.Second,
			minMiB:         24,
			maxMiB:         256,
			fallbackMiB:    64,
			floorMBs:       2.0,
		}
	}
	return firehoseCalibration{
		warmupMiB:      32,
		targetDuration: 8 * time.Second,
		minMiB:         256,
		maxMiB:         384,
		fallbackMiB:    384,
		floorMBs:       10.0,
	}
}

// calibratedFirehoseMiB projects a measured end-to-end sample over the target
// duration, rounds up to a whole MiB for dd, and clamps the result. The lower
// bound keeps the main run materially larger than the resume ring; the upper
// bound prevents a fast host from turning the deliberately stalled subscriber
// into runaway CI memory and I/O volume.
func calibratedFirehoseMiB(bytes int64, elapsed time.Duration, calibration firehoseCalibration) int {
	if bytes <= 0 || elapsed <= 0 {
		return calibration.fallbackMiB
	}

	targetBytes := float64(bytes) * float64(calibration.targetDuration) / float64(elapsed)
	mib := int(math.Ceil(targetBytes / (1 << 20)))
	if mib < calibration.minMiB {
		return calibration.minMiB
	}
	if mib > calibration.maxMiB {
		return calibration.maxMiB
	}
	return mib
}

// firehoseWorkload sizes both normal and -race producers from the REAL
// end-to-end pipeline throughput measured on this host. A fixed byte count is
// host-speed-sensitive: 224 MiB now drains in roughly 4.0-4.9s on faster normal
// hosts, while the test intentionally requires at least 5s of sustained PTY
// production. Under -race, instrumentation produces the same problem at a much
// lower and hardware-dependent rate.
//
// The warmup drains through the same Spawn -> Subscribe -> Frames path as the
// main test. The projected eight-second workload leaves headroom above the
// five-second floor without sleeping or pausing I/O, and mode-specific caps
// bound worst-case CI output and slow-subscriber queue growth.
func firehoseWorkload(t *testing.T) (int, float64) {
	t.Helper()

	calibration := firehoseCalibrationForMode(raceEnabled)
	s, err := Spawn(Spec{Command: firehoseCommand(calibration.warmupMiB)})
	if err != nil {
		t.Fatalf("warmup Spawn: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("warmup Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	start := time.Now()
	var bytes int64
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()

drain:
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				break drain
			}
			switch f.Type {
			case attachwire.TypeControl:
				continue // out-of-namespace (§2), same as the real subtests
			case attachwire.TypeOutput:
				bytes += int64(len(f.Payload))
			case attachwire.TypeExit:
				break drain
			}
		case <-deadline.C:
			t.Fatalf("warmup firehose timed out; received %d bytes so far", bytes)
		}
	}
	elapsed := time.Since(start)

	wantWarmupBytes := int64(calibration.warmupMiB) << 20
	if bytes != wantWarmupBytes {
		t.Fatalf("warmup received %d bytes, want exactly %d (dd count)", bytes, wantWarmupBytes)
	}

	mib := calibratedFirehoseMiB(bytes, elapsed, calibration)
	rateMBs := float64(bytes) / elapsed.Seconds() / 1e6
	t.Logf("firehose warmup: %d bytes in %v = %.2f MB/s (race=%v); sizing main volume to %d MiB for %v target (clamped to [%d,%d])",
		bytes, elapsed, rateMBs, raceEnabled, mib, calibration.targetDuration, calibration.minMiB, calibration.maxMiB)
	return mib, calibration.floorMBs
}

func TestCalibratedFirehoseMiB(t *testing.T) {
	calibration := firehoseCalibration{
		targetDuration: 8 * time.Second,
		minMiB:         24,
		maxMiB:         384,
		fallbackMiB:    64,
	}

	tests := []struct {
		name    string
		bytes   int64
		elapsed time.Duration
		wantMiB int
	}{
		{name: "projects and rounds up", bytes: 10 << 20, elapsed: 3 * time.Second, wantMiB: 27},
		{name: "minimum clamp", bytes: 8 << 20, elapsed: 8 * time.Second, wantMiB: 24},
		{name: "maximum clamp", bytes: 64 << 20, elapsed: time.Second, wantMiB: 384},
		{name: "zero bytes fallback", bytes: 0, elapsed: time.Second, wantMiB: 64},
		{name: "non-positive elapsed fallback", bytes: 8 << 20, elapsed: 0, wantMiB: 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calibratedFirehoseMiB(tt.bytes, tt.elapsed, calibration); got != tt.wantMiB {
				t.Fatalf("calibratedFirehoseMiB(%d, %v) = %d MiB, want %d MiB", tt.bytes, tt.elapsed, got, tt.wantMiB)
			}
		})
	}
}

func TestFirehoseCalibrationBounds(t *testing.T) {
	for _, race := range []bool{false, true} {
		calibration := firehoseCalibrationForMode(race)
		t.Run("race="+strconv.FormatBool(race), func(t *testing.T) {
			if calibration.warmupMiB <= 0 {
				t.Errorf("warmupMiB = %d, want positive", calibration.warmupMiB)
			}
			if calibration.targetDuration <= firehoseMinimumDuration {
				t.Errorf("targetDuration = %v, want > sustained-production floor %v", calibration.targetDuration, firehoseMinimumDuration)
			}
			if calibration.minMiB<<20 <= DefaultRingBytes {
				t.Errorf("min volume = %d MiB, want greater than %d-byte ring", calibration.minMiB, DefaultRingBytes)
			}
			if calibration.fallbackMiB < calibration.minMiB || calibration.fallbackMiB > calibration.maxMiB {
				t.Errorf("fallbackMiB = %d, want within [%d,%d]", calibration.fallbackMiB, calibration.minMiB, calibration.maxMiB)
			}
			if calibration.maxMiB > 384 {
				t.Errorf("maxMiB = %d, want <=384 to bound CI volume", calibration.maxMiB)
			}
		})
	}
}

// testFirehoseFastSubscriber: a single fast-draining subscriber sees strictly
// contiguous host seqs, measures a real sustained rate >=10 MB/s over >=5s,
// and process heap growth stays bounded (<64 MiB) once the firehose drains and
// a GC runs — the ring (8 MiB default) plus one subscriber's transient queue
// should never balloon when the consumer keeps up.
func testFirehoseFastSubscriber(t *testing.T) {
	// Both modes calibrate volume from this host's observed PTY pipeline rate.
	// The throughput floor remains mode-specific and unchanged: >=10 MB/s in a
	// plain build and >=2 MB/s under race-detector instrumentation.
	mib, floorMBs := firehoseWorkload(t)
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
	if elapsed < firehoseMinimumDuration {
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
		growth = int64(after.HeapAlloc - before.HeapAlloc) //nolint:gosec // G115: the guard above proves this is a positive delta well under MaxInt64 (no process holds exabytes of heap)
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
	// Calibrate immediately before this run so host-load changes between
	// subtests cannot inherit a stale rate. The configured minimum remains above
	// the default ring so both bounded-ring and uncapped-queue assertions stay
	// meaningful.
	mib, floorMBs := firehoseWorkload(t)
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
	wantBytes := int64(mib) << 20
	if total != wantBytes {
		t.Errorf("fast peer received %d bytes, want exactly %d (dd count)", total, wantBytes)
	}
	rateMBs := float64(total) / elapsed.Seconds() / 1e6
	t.Logf("fast peer: %d bytes in %v = %.2f MB/s despite a concurrent non-draining subscriber (race=%v)", total, elapsed, rateMBs, raceEnabled)
	if elapsed < firehoseMinimumDuration {
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
