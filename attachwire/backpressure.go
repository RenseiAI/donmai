package attachwire

import "time"

// §11.2 backpressure. This is the MECHANISM only — a small, allocation-light
// token bucket with an injectable clock. Relay POLICY (the bucket rate/burst
// numbers, the drop-vs-snapshot threshold, when to disconnect) lives elsewhere;
// this library ships no policy. The frozen invariant is that buffering toward a
// single viewer is never unbounded; the token bucket is the draft mechanism a
// relay uses to decide when a slow viewer must be coalesced/dropped and brought
// current with a fresh Snapshot rather than queued.

// ViewerSendQueueMaxBytes is the §11.2 per-viewer send-queue bound
// (spec parameter name viewerSendQueueMaxBytes). Its EXISTENCE and enforcement
// are v1-frozen; the NUMBER is v1-draft and set by relay policy. This library
// only names the concept — a viewer whose queue overflows past this bound and
// cannot be flushed even by a catch-up Snapshot is disconnected with
// error.code = backpressure (CodeBackpressure).
type ViewerSendQueueMaxBytes int64

// Clock returns the current time. Inject a fake clock in tests; nil defaults to
// time.Now.
type Clock func() time.Time

// TokenBucket is a classic rate/burst token bucket. It is not safe for
// concurrent use; a relay wraps one per viewer under that viewer's lock. All
// operations are allocation-free after construction.
type TokenBucket struct {
	ratePerSec float64
	burst      float64
	tokens     float64
	last       time.Time
	now        Clock
}

// NewTokenBucket creates a bucket that refills at ratePerSec tokens per second
// up to a maximum of burst tokens, starting full. now is the clock (nil →
// time.Now). rate/burst are configurable per §11.2 (draft parameters).
func NewTokenBucket(ratePerSec, burst float64, now Clock) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	return &TokenBucket{
		ratePerSec: ratePerSec,
		burst:      burst,
		tokens:     burst,
		last:       now(),
		now:        now,
	}
}

// refill adds tokens accrued since the last observation, capped at burst.
func (b *TokenBucket) refill() {
	t := b.now()
	if !t.After(b.last) {
		return
	}
	elapsed := t.Sub(b.last).Seconds()
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = t
}

// Allow refills, then consumes n tokens if at least n are available, reporting
// whether it did. n <= 0 always succeeds without consuming. A false result is
// the relay's signal to coalesce/drop and re-Snapshot rather than queue (§11.2).
func (b *TokenBucket) Allow(n float64) bool {
	if n <= 0 {
		return true
	}
	b.refill()
	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// Tokens refills and returns the current token count (for observability/tests).
func (b *TokenBucket) Tokens() float64 {
	b.refill()
	return b.tokens
}

// Burst returns the configured maximum token capacity.
func (b *TokenBucket) Burst() float64 { return b.burst }

// Rate returns the configured refill rate in tokens per second.
func (b *TokenBucket) Rate() float64 { return b.ratePerSec }
