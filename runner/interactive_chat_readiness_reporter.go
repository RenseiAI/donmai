package runner

import (
	"context"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
)

// Refresh bound for unchanged readiness while live. Var for tests.
var chatReadinessRefreshInterval = 60 * time.Second

// Sample cadence for the supervisor's readiness poll. A 60s-only tick misses
// alternate-screen modal windows that open and close between ticks and lets a
// change-driven post desynchronize the refresh grid (the next 60s tick lands
// <60s after the change and is skipped, stalling the receiver for ~2x the
// interval). Sampling every second keeps the worst-case staleness at the
// interval plus one sample. Var for tests.
var chatReadinessSampleInterval = time.Second

func chatReadinessWire(r agent.ChatReadiness, now time.Time) *afclient.SessionChatReadiness {
	if now.IsZero() {
		now = time.Now()
	}
	return &afclient.SessionChatReadiness{
		Version: afclient.SessionChatReadinessVersion, Channel: string(r.Channel),
		State: string(r.State), LiveSupported: r.LiveSupported,
		QueueSupported: r.QueueSupported, Reason: r.Reason,
		ObservedAt: now.UTC().Format(time.RFC3339),
	}
}

func chatReadinessChanged(a, b agent.ChatReadiness) bool {
	return a.Channel != b.Channel || a.State != b.State ||
		a.LiveSupported != b.LiveSupported || a.QueueSupported != b.QueueSupported ||
		a.Reason != b.Reason
}

// Posts running chat readiness on change and at least every interval.
// Ended never posts; failures are swallowed so readiness never fails a session.
//
// Posts run on a background goroutine so a slow status receiver never blocks
// the supervisor: operator stop, PTY teardown and notice retries all stay
// responsive while the POST is in flight. At most one POST runs at a time;
// overlapping change/tick samples are skipped (the watermark is unadvanced,
// so the next sample retries).
type chatReadinessReporter struct {
	post     func(context.Context, *afclient.SessionChatReadiness) error
	now      func() time.Time
	interval time.Duration
	mu       sync.Mutex
	hasLast  bool
	last     agent.ChatReadiness
	lastPost time.Time
	inFlight bool
}

func newChatReadinessReporter(post func(context.Context, *afclient.SessionChatReadiness) error, now func() time.Time, interval time.Duration) *chatReadinessReporter {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = chatReadinessRefreshInterval
	}
	return &chatReadinessReporter{post: post, now: now, interval: interval}
}

func (c *chatReadinessReporter) shouldPost(current agent.ChatReadiness, now time.Time) bool {
	if current.State == agent.ChatStateEnded {
		return false
	}
	if !c.hasLast {
		return true
	}
	return chatReadinessChanged(c.last, current) || now.Sub(c.lastPost) >= c.interval
}

// maybeReport initiates one async POST when the read is new (change) or due
// (interval elapsed). It never blocks on HTTP: the POST runs in the
// background and the watermark advances only on success. It reports whether a
// POST was initiated.
func (c *chatReadinessReporter) maybeReport(ctx context.Context, current agent.ChatReadiness) bool {
	if c == nil || c.post == nil {
		return false
	}
	now := c.now()
	c.mu.Lock()
	if !c.shouldPost(current, now) {
		c.mu.Unlock()
		return false
	}
	if c.inFlight {
		c.mu.Unlock()
		return false
	}
	wire := chatReadinessWire(current, now)
	post := c.post
	c.inFlight = true
	c.mu.Unlock()
	go func() {
		err := post(ctx, wire)
		c.mu.Lock()
		c.inFlight = false
		if err == nil {
			c.last, c.hasLast, c.lastPost = current, true, now
		}
		c.mu.Unlock()
	}()
	return true
}

// waitIdle blocks until no POST is in flight or the timeout elapses.
// Tests use it to join the background POST before asserting the watermark.
func (c *chatReadinessReporter) waitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		idle := !c.inFlight
		c.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
