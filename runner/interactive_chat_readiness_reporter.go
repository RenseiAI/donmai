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
type chatReadinessReporter struct {
	post     func(context.Context, *afclient.SessionChatReadiness) error
	now      func() time.Time
	interval time.Duration
	mu       sync.Mutex
	hasLast  bool
	last     agent.ChatReadiness
	lastPost time.Time
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

func (c *chatReadinessReporter) maybeReport(ctx context.Context, current agent.ChatReadiness) {
	if c == nil || c.post == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	if !c.shouldPost(current, now) {
		c.mu.Unlock()
		return
	}
	wire := chatReadinessWire(current, now)
	post := c.post
	c.mu.Unlock()
	if err := post(ctx, wire); err != nil {
		return
	}
	c.mu.Lock()
	c.last, c.hasLast, c.lastPost = current, true, now
	c.mu.Unlock()
}
