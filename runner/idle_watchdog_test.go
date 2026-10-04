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
type idleManualClock struct {
	timer *idleManualTimer
}

func newIdleManualClock() *idleManualClock {
	return &idleManualClock{timer: &idleManualTimer{ch: make(chan time.Time, 1)}}
}

func (c *idleManualClock) NewTimer(d time.Duration) interviewTimer {
	c.timer.mu.Lock()
	c.timer.armed = d > 0
	c.timer.mu.Unlock()
	return c.timer
}

// fire trips the watchdog once (no-op when disarmed). The tick sits in the
// buffered channel until the consumer's select observes it, so the test
// never races the consumer: fire, then wait for the reset count to grow.
func (c *idleManualClock) fire() {
	c.timer.mu.Lock()
	armed := c.timer.armed
	ch := c.timer.ch
	c.timer.mu.Unlock()
	if !armed {
		return
	}
	select {
	case ch <- time.Now():
	default:
	}
}

// resets reports how many times the consumer re-armed the watchdog via
// Reset. Event observation and in-flight re-arms both count; a fired
// window the consumer re-arms therefore shows up as exactly one more
// reset, which is the handshake the tests wait on.
func (c *idleManualClock) resets() int {
	return c.timer.resetCount()
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
