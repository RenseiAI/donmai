package daemon

// token_refresher.go — proactive runtime-token refresh.
//
// The platform runtime JWT carries a short TTL (~1h). Before this refresher
// existed the daemon only ever refreshed REACTIVELY: the token expired, the
// next heartbeat 401'd (one log cycle + refresh), then the poll loop 401'd
// seconds later (a second log cycle + a second refresh) — every hour, around
// the clock, growing daemon-error.log without bound.
//
// The refresher re-mints the token shortly BEFORE expiry, so the steady state
// is one quiet scheduled refresh per TTL window and zero auth failures. The
// reactive paths in HeartbeatService / PollService remain as the backstop for
// what a schedule cannot predict — clock skew, token revocation, and the
// worker's Redis registration entry expiring ("worker-not-found").

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultRefreshLead is how long before token expiry the proactive
	// refresh fires. Generous enough to absorb a couple of failed attempts
	// (retried at DefaultRefreshRetry) before the token actually lapses.
	DefaultRefreshLead = 5 * time.Minute

	// DefaultRefreshRetry is the delay before retrying a failed proactive
	// refresh attempt.
	DefaultRefreshRetry = time.Minute

	// DefaultAssumedTokenTTL is the expiry horizon assumed when the
	// platform response omits runtimeTokenExpiresAt. Matches the
	// platform's documented runtime-JWT TTL; refreshing early is harmless
	// (one extra HTTP call), refreshing late just falls back to the
	// reactive path.
	DefaultAssumedTokenTTL = time.Hour

	// minRefreshWait floors the scheduled wait so an already-expired (or
	// skewed) expiry cannot hot-loop the refresher.
	minRefreshWait = 10 * time.Second
)

// tokenRefresherOptions configures a tokenRefresher. Refresh is required;
// everything else has production defaults.
type tokenRefresherOptions struct {
	// ExpiresAt is the current token's expiry. Zero means unknown — the
	// refresher assumes Now()+AssumedTTL rather than going dormant, since
	// a scheduled no-op refresh is cheaper than an hourly reactive 401
	// cycle.
	ExpiresAt time.Time

	// Refresh performs the actual credential refresh (the daemon's shared
	// refresh closure) and returns the NEW token expiry (zero when the
	// platform omitted it).
	Refresh func(ctx context.Context) (newExpiresAt time.Time, err error)

	// Lead overrides DefaultRefreshLead.
	Lead time.Duration
	// Retry overrides DefaultRefreshRetry.
	Retry time.Duration
	// AssumedTTL overrides DefaultAssumedTokenTTL.
	AssumedTTL time.Duration
	// MinWait overrides minRefreshWait (tests only).
	MinWait time.Duration
	// Now overrides time.Now (tests only).
	Now func() time.Time
}

func (o tokenRefresherOptions) lead() time.Duration {
	if o.Lead > 0 {
		return o.Lead
	}
	return DefaultRefreshLead
}

func (o tokenRefresherOptions) retry() time.Duration {
	if o.Retry > 0 {
		return o.Retry
	}
	return DefaultRefreshRetry
}

func (o tokenRefresherOptions) assumedTTL() time.Duration {
	if o.AssumedTTL > 0 {
		return o.AssumedTTL
	}
	return DefaultAssumedTokenTTL
}

func (o tokenRefresherOptions) minWait() time.Duration {
	if o.MinWait > 0 {
		return o.MinWait
	}
	return minRefreshWait
}

func (o tokenRefresherOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// tokenRefresher runs the proactive refresh loop. Lifecycle mirrors the
// other daemon services: Start is idempotent, Stop cancels the goroutine.
type tokenRefresher struct {
	opts tokenRefresherOptions

	mu        sync.Mutex
	cancel    context.CancelFunc
	running   bool
	expiresAt time.Time
}

// newTokenRefresher constructs a tokenRefresher. Refresh must be non-nil.
func newTokenRefresher(opts tokenRefresherOptions) *tokenRefresher {
	return &tokenRefresher{opts: opts, expiresAt: opts.ExpiresAt}
}

// Start launches the refresh goroutine. Subsequent calls are no-ops.
func (r *tokenRefresher) Start() {
	r.mu.Lock()
	if r.running || r.opts.Refresh == nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.running = true
	r.mu.Unlock()

	go r.loop(ctx)
}

// Stop terminates the refresh goroutine. Safe to call multiple times.
func (r *tokenRefresher) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	r.cancel()
	r.running = false
}

// loop schedules one refresh per expiry window: sleep until expiry−lead,
// refresh, reschedule off the new expiry. Failures retry on the (much
// shorter) retry interval; the reactive 401 path remains the final backstop.
func (r *tokenRefresher) loop(ctx context.Context) {
	for {
		timer := time.NewTimer(r.nextWait())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		newExp, err := r.opts.Refresh(ctx)
		if ctx.Err() != nil {
			return
		}
		now := r.opts.now()
		if err != nil {
			// Routine retry — Info, not Warn/Error. The reactive path
			// escalates if the token actually lapses.
			slog.Info("[runtime-token]",
				"event", "proactive-refresh.retry",
				"retryIn", r.opts.retry().String(),
				"err", err.Error(),
			)
			r.setExpiresAt(now.Add(r.opts.retry() + r.opts.lead()))
			continue
		}
		if newExp.IsZero() {
			newExp = now.Add(r.opts.assumedTTL())
		}
		slog.Info("[runtime-token]",
			"event", "proactive-refresh",
			"nextExpiry", newExp.UTC().Format(time.RFC3339),
		)
		r.setExpiresAt(newExp)
	}
}

// nextWait computes the sleep until the next proactive refresh:
// expiry − lead, floored at minWait. An unknown expiry assumes
// now + AssumedTTL.
func (r *tokenRefresher) nextWait() time.Duration {
	r.mu.Lock()
	exp := r.expiresAt
	r.mu.Unlock()

	now := r.opts.now()
	if exp.IsZero() {
		exp = now.Add(r.opts.assumedTTL())
	}
	wait := exp.Sub(now) - r.opts.lead()
	if wait < r.opts.minWait() {
		wait = r.opts.minWait()
	}
	return wait
}

func (r *tokenRefresher) setExpiresAt(t time.Time) {
	r.mu.Lock()
	r.expiresAt = t
	r.mu.Unlock()
}

// parseTokenExpiry parses the platform's runtimeTokenExpiresAt (RFC3339).
// Returns zero on empty/malformed input — callers treat zero as "unknown".
func parseTokenExpiry(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
