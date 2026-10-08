package runner

import (
	"sync"
	"testing"
	"time"
)

// idleManualClock is a fake interviewClock driving the consumeEvents
// idle/no-progress watchdog deterministically: the test trips expiry
// explicitly via fire() instead of sleeping past a wall-clock window, so
// loaded-machine scheduling can never flip the outcome. Each fire models
// one silent idle window elapsing while the events channel stays open.
//
// The clock hands out one independent timer per NewTimer call: consumeEvents
// arms the idle watchdog and the stalled-model-request detector as two
// separate timers, and the shared reset-count handshake the tests wait on
// counts re-arms of the IDLE timer only. The stall timer re-arms alongside
// it on every observed event but never contributes to the count, so the
// existing handshake stays exact while both windows stay armed.
type idleManualClock struct {
	mu sync.Mutex
	// idleTimer is the handle on the FIRST timer handed out — the idle
	// watchdog's. It stays reachable after NewTimer gives it away so
	// resets keeps counting its re-arms.
	idleTimer *idleManualTimer
	handedOut bool
	stalled   []*idleManualTimer
}

func newIdleManualClock() *idleManualClock {
	return &idleManualClock{idleTimer: &idleManualTimer{ch: make(chan time.Time, 1)}}
}

func (c *idleManualClock) NewTimer(d time.Duration) interviewTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.handedOut {
		c.handedOut = true
		t := c.idleTimer
		t.mu.Lock()
		t.armed = d > 0
		t.mu.Unlock()
		return t
	}
	t := &idleManualTimer{ch: make(chan time.Time, 1)}
	t.mu.Lock()
	t.armed = d > 0
	t.mu.Unlock()
	c.stalled = append(c.stalled, t)
	return t
}

// fire trips the watchdog once (no-op when disarmed). The tick sits in the
// buffered channel until the consumer's select observes it, so the test
// never races the consumer: fire, then wait for the reset count to grow.
func (c *idleManualClock) fire() {
	c.mu.Lock()
	timers := append([]*idleManualTimer{c.idleTimer}, c.stalled...)
	c.mu.Unlock()
	for _, t := range timers {
		if t == nil {
			continue
		}
		t.mu.Lock()
		armed := t.armed
		ch := t.ch
		t.mu.Unlock()
		if !armed {
			continue
		}
		select {
		case ch <- time.Now():
		default:
		}
	}
}

// resets reports how many times the consumer re-armed the IDLE watchdog
// via Reset. Event observation and in-flight re-arms both count; a fired
// window the consumer re-arms therefore shows up as exactly one more
// reset, which is the handshake the tests wait on. The stall detector's
// own re-arms never count here.
func (c *idleManualClock) resets() int {
	c.mu.Lock()
	t := c.idleTimer
	c.mu.Unlock()
	if t == nil {
		return 0
	}
	return t.resetCount()
}

// waitForIdleResets polls until the consumer has re-armed the watchdog
// want times, or fails. It is a liveness poll, not a timing assertion:
// there is no sleep-versus-timeout ratio left to lose.
func (c *idleManualClock) waitForIdleResets(t *testing.T, want int) {
	t.Helper()
	waitFor(t, 10*time.Second, "idle watchdog re-arm was never observed", func() bool {
		return c.resets() == want
	})
}

type idleManualTimer struct {
	mu     sync.Mutex
	ch     chan time.Time
	armed  bool
	resets int
}

func (t *idleManualTimer) Chan() <-chan time.Time { return t.ch }

func (t *idleManualTimer) Reset(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = d > 0
	if d > 0 {
		t.resets++
	}
	// Drain a stale tick so a fire() from a prior window cannot trip the
	// next select — the same contract realInterviewTimer keeps.
	select {
	case <-t.ch:
	default:
	}
}

func (t *idleManualTimer) Stop() {
	t.mu.Lock()
	t.armed = false
	t.mu.Unlock()
}

func (t *idleManualTimer) resetCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resets
}
