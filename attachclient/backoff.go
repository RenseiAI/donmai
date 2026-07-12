package attachclient

import (
	"context"
	"math/rand/v2"
	"time"
)

// backoff is a cancel-aware exponential backoff with equal jitter. It is reset
// on every success (§ reconnect discipline: reset-on-success). Not safe for
// concurrent use — RunHost drives it from a single goroutine.
type backoff struct {
	lo, hi time.Duration
	cur    time.Duration
}

func newBackoff(lo, hi time.Duration) *backoff {
	return &backoff{lo: lo, hi: hi}
}

// reset returns the backoff to its pre-first-failure state.
func (b *backoff) reset() { b.cur = 0 }

// next advances the schedule and returns the next delay with equal jitter in
// [cur/2, cur], so the delay is never zero (no tight loop) and never fixed.
func (b *backoff) next() time.Duration {
	switch {
	case b.cur == 0:
		b.cur = b.lo
	default:
		b.cur *= 2
		if b.cur > b.hi {
			b.cur = b.hi
		}
	}
	half := b.cur / 2
	if half <= 0 {
		return b.cur
	}
	// Jitter only — not security-sensitive.
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // G404: jitter, not crypto
}

// sleepCtx sleeps for d, or returns early with the context error if ctx is
// cancelled first. A non-positive d still honors cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
