package runner

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
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
	// uncertain marks the receiver as possibly ahead of the watermark: the
	// last POST reached the receiver but its response was lost (or the POST
	// failed after the receiver applied it). The next non-ended sample must
	// re-post even when it equals the last acknowledged read, otherwise a
	// busy update the receiver applied stays stuck until the 60s refresh.
	// It is set only for transient failures: a permanent (4xx) rejection
	// proves the receiver answered and did not apply the update, so
	// retrying the same read every sample would storm the status endpoint
	// at the 1s sample rate against the change-or-60s contract.
	uncertain bool
	inFlight  bool
	// failures counts consecutive transient POST failures. The corrective
	// re-post backs off (1s doubling up to the refresh interval) instead
	// of retrying on every 1s sample, so a receiver outage does not become
	// a retry storm. The first failure retries immediately so a lost
	// response still corrects on the next sample.
	failures int
	// nextRetry gates an uncertain re-post: samples before it are skipped.
	// Zero means an uncertain re-post may go immediately.
	nextRetry time.Time
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
	if c.uncertain {
		// A transient failure backs off: the corrective re-post waits for
		// nextRetry instead of firing on every 1s sample.
		if !c.nextRetry.IsZero() && now.Before(c.nextRetry) {
			return false
		}
		return true
	}
	if !c.hasLast {
		return true
	}
	return chatReadinessChanged(c.last, current) || now.Sub(c.lastPost) >= c.interval
}

// isChatReadinessPermanent reports whether err proves the receiver answered
// without applying the update (a typed 4xx rejection). Such an error must
// not mark the cache uncertain: the response was not lost.
func isChatReadinessPermanent(err error) bool {
	var perm *result.PermanentError
	return errors.As(err, &perm)
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
		switch {
		case err == nil:
			c.last, c.hasLast, c.lastPost = current, true, now
			c.uncertain = false
			c.failures = 0
			c.nextRetry = time.Time{}
		case isChatReadinessPermanent(err):
			// The receiver answered with a definite rejection (409 on a
			// repeated running transition, or any other 4xx): the
			// response was not lost, so the receiver holds nothing new
			// to correct. Advance the watermark instead of marking the
			// cache uncertain, pacing retries to change-or-interval
			// rather than storming the status endpoint every sample.
			c.last, c.hasLast, c.lastPost = current, true, now
			c.uncertain = false
			c.failures = 0
			c.nextRetry = time.Time{}
		default:
			// The POST may have reached the receiver before the response
			// was lost: the receiver can hold the attempted read while
			// the watermark still carries the previous acknowledged
			// value. Marking the cache uncertain forces the next sample
			// to re-post, so a reverted read corrects the receiver
			// instead of reading as unchanged until the next refresh.
			// Back off the corrective re-post (1s doubling up to the
			// refresh interval) so a persistent outage does not retry
			// on every 1s sample; the first failure still corrects
			// immediately on the next sample.
			c.uncertain = true
			c.failures++
			if c.failures <= 1 {
				c.nextRetry = now
			} else {
				delay := time.Second << (c.failures - 2)
				if delay <= 0 || delay > c.interval {
					delay = c.interval
				}
				c.nextRetry = now.Add(delay)
			}
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
