package attachwire

import (
	"testing"
	"time"
)

func TestTokenBucketFakeClock(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }

	// 10 tokens/sec, burst 5, starts full.
	b := NewTokenBucket(10, 5, clock)
	if b.Rate() != 10 || b.Burst() != 5 {
		t.Fatalf("rate/burst = %v/%v", b.Rate(), b.Burst())
	}
	if got := b.Tokens(); got != 5 {
		t.Fatalf("starts full: tokens = %v, want 5", got)
	}

	// Drain the burst.
	for i := 0; i < 5; i++ {
		if !b.Allow(1) {
			t.Fatalf("Allow(1) #%d should succeed with a full bucket", i)
		}
	}
	if b.Allow(1) {
		t.Fatalf("Allow(1) should fail once the bucket is empty")
	}

	// Advance 0.25s -> +2.5 tokens.
	now = now.Add(250 * time.Millisecond)
	if got := b.Tokens(); got < 2.49 || got > 2.51 {
		t.Fatalf("after 0.25s tokens = %v, want ~2.5", got)
	}
	if !b.Allow(2) {
		t.Fatalf("Allow(2) should succeed with ~2.5 tokens")
	}

	// Advance far -> refill caps at burst.
	now = now.Add(10 * time.Second)
	if got := b.Tokens(); got != 5 {
		t.Fatalf("refill must cap at burst: tokens = %v, want 5", got)
	}

	// Non-positive request always succeeds without consuming.
	before := b.Tokens()
	if !b.Allow(0) || !b.Allow(-3) {
		t.Fatalf("Allow(<=0) should always succeed")
	}
	if b.Tokens() != before {
		t.Fatalf("Allow(<=0) must not consume")
	}
}

func TestTokenBucketNilClockDefault(t *testing.T) {
	b := NewTokenBucket(1, 1, nil)
	if !b.Allow(1) {
		t.Fatalf("full bucket should allow 1")
	}
}

func TestTokenBucketMonotonicClockGuard(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := NewTokenBucket(100, 10, clock)
	b.Allow(10) // drain
	// Clock goes backwards (should not add tokens).
	now = now.Add(-time.Second)
	if got := b.Tokens(); got != 0 {
		t.Fatalf("backwards clock must not refill: tokens = %v", got)
	}
}

func TestViewerSendQueueMaxBytesType(t *testing.T) {
	// The named §11.2 parameter concept exists as a type; value is policy (draft).
	var bound ViewerSendQueueMaxBytes = 1 << 20
	if bound != 1048576 {
		t.Fatalf("unexpected bound value")
	}
}
