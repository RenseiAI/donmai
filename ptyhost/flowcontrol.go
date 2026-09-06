package ptyhost

import (
	"log/slog"
	"sync"
	"time"
)

// Output flow-control defaults.
const (
	// DefaultOutputHighWaterBytes is how many undelivered payload bytes may
	// queue for ONE subscriber before the PTY reader stops reading.
	//
	// It is deliberately the ring budget. Both numbers answer the same question
	// — how much host output this session is willing to hold before it admits
	// it cannot keep up — and answering it in two different magnitudes is how a
	// component that is not the ring ends up being the first to give up.
	DefaultOutputHighWaterBytes = DefaultRingBytes

	// defaultOutputLowWaterDivisor derives the resume threshold from the
	// high-water mark. Resuming at the same mark that paused would re-pause on
	// the next frame; a quarter of the budget is one clear drain's worth of
	// hysteresis.
	defaultOutputLowWaterDivisor = 4

	// DefaultOutputPauseBound caps how long the reader may stay paused for a
	// subscriber that never drains.
	//
	// A pause is correct back-pressure while SOMETHING is still consuming.
	// A subscriber whose consumer goroutine has died without closing the
	// subscription is not back-pressure, it is a wedge: the harness would block
	// on its terminal write forever with nothing left that could ever release
	// it. So the pause is bounded, and crossing the bound resumes reading and
	// says so rather than holding a live harness hostage to a dead reader.
	//
	// It is deliberately LONGER than any consumer-side drop bound composed on
	// top of this package, so the ordinary resolution of a genuinely stopped
	// consumer is that consumer dropping its own connection — which closes the
	// subscription, frees the queue, and resumes the reader with no loss — and
	// this bound is only ever reached by a consumer that neither drains nor
	// disconnects.
	DefaultOutputPauseBound = 15 * time.Minute

	// outputFrameOverheadBytes charges a fixed cost per queued frame so a flood
	// of empty frames is bounded by the same mark that bounds a flood of large
	// ones. A byte budget that counted only payload cannot bound memory against
	// a producer emitting nothing.
	outputFrameOverheadBytes = 64
)

// OutputFlowControl enables PTY-read back-pressure on a Session.
//
// Without it a subscriber that stops draining is absorbed by an UNBOUNDED
// per-subscription queue: the reader keeps reading, the queue keeps growing,
// and the only bound left is the ring's eviction — which loses bytes and forces
// a Gap the moment the consumer catches up. With it the reader stops reading,
// the kernel's PTY buffer fills, and the harness blocks on its own terminal
// write, which is exactly what a real terminal does to a program that outruns
// its reader. Nothing is lost and nothing is reordered.
//
// The zero value of each field takes the documented default.
type OutputFlowControl struct {
	// HighWaterBytes is the per-subscriber queue depth that pauses reading.
	HighWaterBytes int
	// LowWaterBytes is the depth every saturated subscriber must fall back to
	// before reading resumes. A value at or above HighWaterBytes is replaced by
	// the default derivation: without hysteresis the reader would re-pause on
	// the frame after every resume.
	LowWaterBytes int
	// PauseBound caps one continuous pause. See DefaultOutputPauseBound.
	PauseBound time.Duration
	// OnChange, when set, observes every pause/resume transition. It is called
	// from a dedicated goroutine — never from the read loop and never under any
	// session lock — so an implementation may log or write to disk.
	OnChange func(OutputFlowState)
}

// OutputFlowState is the observable back-pressure state of one session.
type OutputFlowState struct {
	// Paused reports whether the PTY reader is currently stopped.
	Paused bool
	// Since is when the current pause began, or the zero time when not paused.
	Since time.Time
	// PendingBytes is how much host output is queued for delivery across every
	// live subscriber.
	PendingBytes int
	// SaturatedSubscribers counts subscribers above the high-water mark.
	SaturatedSubscribers int
	// PauseBoundReached reports that a pause hit PauseBound and reading was
	// resumed with a subscriber still saturated. It is the degraded case, not
	// the ordinary one, and it clears when the subscriber drains or goes away.
	PauseBoundReached bool
}

// outputGate is the read-loop side of OutputFlowControl. A nil *outputGate is
// the disabled configuration and every method tolerates it, so the read loop
// carries no branch.
type outputGate struct {
	high, low int
	bound     time.Duration
	onChange  func(OutputFlowState)
	logger    *slog.Logger

	changed chan struct{}
	stopped chan struct{}
	once    sync.Once

	mu        sync.Mutex
	pending   int
	saturated int
	paused    bool
	since     time.Time
	forced    bool
	// resume is a broadcast latch: CLOSED and replaced whenever a pause ends,
	// so every waiter wakes exactly once. A sync.Cond cannot be used here
	// because the pause bound needs a timed wait.
	resume chan struct{}
}

func newOutputGate(cfg *OutputFlowControl, logger *slog.Logger) *outputGate {
	if cfg == nil {
		return nil
	}
	high := cfg.HighWaterBytes
	if high <= 0 {
		high = DefaultOutputHighWaterBytes
	}
	low := cfg.LowWaterBytes
	if low <= 0 || low >= high {
		low = high / defaultOutputLowWaterDivisor
	}
	bound := cfg.PauseBound
	if bound <= 0 {
		bound = DefaultOutputPauseBound
	}
	g := &outputGate{
		high: high, low: low, bound: bound, onChange: cfg.OnChange, logger: logger,
		changed: make(chan struct{}, 1),
		stopped: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	if g.onChange != nil {
		go g.notifyLoop()
	}
	return g
}

func (g *outputGate) highWater() int {
	if g == nil {
		return 0
	}
	return g.high
}

func (g *outputGate) lowWater() int {
	if g == nil {
		return 0
	}
	return g.low
}

// account records a change in the bytes queued across every subscriber. It is
// diagnostic only: the pause decision is per-subscriber, because one saturated
// consumer must not be masked by another that is keeping up.
func (g *outputGate) account(delta int) {
	if g == nil || delta == 0 {
		return
	}
	g.mu.Lock()
	g.pending += delta
	g.mu.Unlock()
}

// saturate reports one subscriber crossing the high-water mark.
func (g *outputGate) saturate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.saturated++
	entered := g.saturated == 1 && !g.paused && !g.forced
	if entered {
		g.paused, g.since = true, time.Now()
	}
	g.mu.Unlock()
	if entered {
		g.signalChange()
	}
}

// relieve reports one subscriber falling back to the low-water mark, or going
// away entirely. Reading resumes when the last saturated subscriber clears.
func (g *outputGate) relieve() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.saturated > 0 {
		g.saturated--
	}
	released := false
	if g.saturated == 0 && (g.paused || g.forced) {
		g.paused, g.forced, g.since = false, false, time.Time{}
		releaseOutputLatch(&g.resume)
		released = true
	}
	g.mu.Unlock()
	if released {
		g.signalChange()
	}
}

// await blocks the PTY read loop while any subscriber is saturated.
//
// This is the whole mechanism: not reading the master is what makes the kernel
// buffer fill, which is what makes the harness block in write(2). Everything
// else here is bookkeeping around that one decision.
func (g *outputGate) await(stop <-chan struct{}) {
	if g == nil {
		return
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		g.mu.Lock()
		if !g.paused {
			g.mu.Unlock()
			return
		}
		wait, remaining := g.resume, g.bound-time.Since(g.since)
		g.mu.Unlock()
		if remaining <= 0 {
			g.forceRelease()
			return
		}
		if timer == nil {
			timer = time.NewTimer(remaining)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(remaining)
		}
		select {
		case <-wait:
		case <-stop:
			return
		case <-timer.C:
		}
	}
}

// forceRelease ends a pause that ran the whole bound out. The subscriber stays
// saturated — nothing about it improved — so the queue grows again from here;
// what changes is that the harness is no longer blocked on a reader that may
// never come back.
func (g *outputGate) forceRelease() {
	g.mu.Lock()
	held := time.Since(g.since)
	pending, saturated := g.pending, g.saturated
	forced := g.paused
	if forced {
		g.paused, g.forced = false, true
		releaseOutputLatch(&g.resume)
	}
	g.mu.Unlock()
	if !forced {
		return
	}
	if g.logger != nil {
		g.logger.Warn("ptyhost: output pause bound reached; resuming PTY reads with a saturated subscriber",
			"held", held.Round(time.Millisecond), "pendingBytes", pending,
			"saturatedSubscribers", saturated, "highWaterBytes", g.high)
	}
	g.signalChange()
}

func (g *outputGate) state() OutputFlowState {
	if g == nil {
		return OutputFlowState{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return OutputFlowState{
		Paused: g.paused, Since: g.since, PendingBytes: g.pending,
		SaturatedSubscribers: g.saturated, PauseBoundReached: g.forced,
	}
}

// signalChange wakes the notifier without ever blocking a caller that may be
// holding the session lock. Transitions coalesce: the notifier always reports
// the CURRENT state, so a collapsed pair is reported as the state that survived.
func (g *outputGate) signalChange() {
	if g.onChange == nil {
		return
	}
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

func (g *outputGate) notifyLoop() {
	for {
		select {
		case <-g.stopped:
			return
		case <-g.changed:
			g.onChange(g.state())
		}
	}
}

func (g *outputGate) close() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.stopped) })
}

// releaseOutputLatch closes the current latch and installs a fresh one, waking
// every waiter exactly once. The caller holds the gate lock.
func releaseOutputLatch(latch *chan struct{}) {
	close(*latch)
	*latch = make(chan struct{})
}
