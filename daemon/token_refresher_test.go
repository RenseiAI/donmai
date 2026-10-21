package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTokenRefresher_ProactiveRefreshBeforeExpiry covers the headline
// behaviour: the refresher fires BEFORE the known expiry (expiry − lead) and
// reschedules off the new expiry the refresh returns, refreshing again in the
// next window — the steady state that keeps the reactive 401 paths quiet.
func TestTokenRefresher_ProactiveRefreshBeforeExpiry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	refreshed := make(chan struct{}, 8)

	const ttl = 80 * time.Millisecond
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: time.Now().Add(ttl),
		Lead:      40 * time.Millisecond,
		MinWait:   time.Millisecond,
		Refresh: func(context.Context) (time.Time, error) {
			calls.Add(1)
			refreshed <- struct{}{}
			return time.Now().Add(ttl), nil
		},
	})
	r.Start()
	t.Cleanup(r.Stop)

	// Two refreshes prove both the initial schedule and the reschedule off
	// the returned expiry.
	for i := 0; i < 2; i++ {
		select {
		case <-refreshed:
		case <-time.After(2 * time.Second):
			t.Fatalf("refresh %d never fired", i+1)
		}
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("refresh calls = %d, want >= 2", got)
	}
}

// TestTokenRefresher_RetryAfterFailure pins the failure posture: a failed
// proactive attempt retries on the (short) retry interval instead of going
// dormant until the stale expiry — the reactive path must never become the
// steady state just because one scheduled attempt hit a transient error.
func TestTokenRefresher_RetryAfterFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	refreshed := make(chan struct{}, 8)

	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: time.Now().Add(30 * time.Millisecond),
		Lead:      20 * time.Millisecond,
		Retry:     10 * time.Millisecond,
		MinWait:   time.Millisecond,
		Refresh: func(context.Context) (time.Time, error) {
			n := calls.Add(1)
			refreshed <- struct{}{}
			if n == 1 {
				return time.Time{}, errors.New("transient platform error")
			}
			return time.Now().Add(time.Hour), nil
		},
	})
	r.Start()
	t.Cleanup(r.Stop)

	for i := 0; i < 2; i++ {
		select {
		case <-refreshed:
		case <-time.After(2 * time.Second):
			t.Fatalf("attempt %d never fired (failure must reschedule a retry)", i+1)
		}
	}
}

// TestTokenRefresher_UnknownExpiryAssumesTTL ensures a zero expiry (platform
// omitted runtimeTokenExpiresAt) still schedules — assuming the documented
// TTL — instead of disabling proactive refresh.
func TestTokenRefresher_UnknownExpiryAssumesTTL(t *testing.T) {
	t.Parallel()

	refreshed := make(chan struct{}, 4)
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt:  time.Time{}, // unknown
		AssumedTTL: 30 * time.Millisecond,
		Lead:       20 * time.Millisecond,
		MinWait:    time.Millisecond,
		Refresh: func(context.Context) (time.Time, error) {
			refreshed <- struct{}{}
			return time.Time{}, nil // platform keeps omitting expiry
		},
	})
	r.Start()
	t.Cleanup(r.Stop)

	for i := 0; i < 2; i++ {
		select {
		case <-refreshed:
		case <-time.After(2 * time.Second):
			t.Fatalf("refresh %d never fired under assumed TTL", i+1)
		}
	}
}

// TestTokenRefresher_StopHaltsLoop verifies Stop cancels the goroutine: no
// refresh fires after Stop returns.
func TestTokenRefresher_StopHaltsLoop(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: time.Now().Add(20 * time.Millisecond),
		Lead:      10 * time.Millisecond,
		MinWait:   5 * time.Millisecond,
		Refresh: func(context.Context) (time.Time, error) {
			calls.Add(1)
			return time.Now().Add(20 * time.Millisecond), nil
		},
	})
	r.Start()
	r.Stop()

	settled := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != settled {
		t.Fatalf("refresh fired after Stop: %d -> %d", settled, got)
	}
	// Idempotent lifecycle: double Stop and re-Stop after Start must not panic.
	r.Stop()
}

// TestTokenRefresher_NilRefreshNeverStarts pins the guard: without a Refresh
// callback Start is a no-op (stub registrations wire no refresher at all,
// but defence in depth keeps a nil callback from panicking the loop).
func TestTokenRefresher_NilRefreshNeverStarts(t *testing.T) {
	t.Parallel()
	r := newTokenRefresher(tokenRefresherOptions{ExpiresAt: time.Now()})
	r.Start() // must not panic / spin
	r.Stop()
}

// TestTokenRefresher_NextWaitCappedAtMaxWait pins the sleep ceiling: a
// token far from expiry arms a short re-check wait — never one long timer
// across the whole window — so host sleep (which freezes the monotonic
// clock timers run on) cannot push the single wake past expiry.
func TestTokenRefresher_NextWaitCappedAtMaxWait(t *testing.T) {
	t.Parallel()

	now := time.Now()
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: now.Add(60 * time.Minute), // 55 min to the lead boundary
		Lead:      5 * time.Minute,
		MinWait:   time.Millisecond,
		MaxWait:   time.Minute,
		Now:       func() time.Time { return now },
		Refresh: func(context.Context) (time.Time, error) {
			t.Error("Refresh must not fire while building the wait")
			return time.Time{}, nil
		},
	})

	if got := r.nextWait(); got > time.Minute {
		t.Fatalf("nextWait() = %v, want <= maxWait (%v)", got, time.Minute)
	}

	// Boundary pin: a wait of 1.5*MaxWait clamps exactly to MaxWait.
	clamped := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: now.Add(30*time.Millisecond + 5*time.Millisecond),
		Lead:      5 * time.Millisecond,
		MinWait:   time.Millisecond,
		MaxWait:   20 * time.Millisecond,
		Now:       func() time.Time { return now },
		Refresh: func(context.Context) (time.Time, error) {
			return time.Time{}, nil
		},
	})
	if got := clamped.nextWait(); got != 20*time.Millisecond {
		t.Fatalf("nextWait() = %v for a 1.5*MaxWait wait, want exactly MaxWait (%v)", got, 20*time.Millisecond)
	}
}

// TestTokenRefresher_SimulatedSleepRefreshesOnNextWake drives the loop
// through a wall-clock jump between wakes: Now leaps past expiry-lead
// (as if the host slept) and the very next wake must call Refresh exactly
// once — the proactive refresh fires at the wall-clock boundary, not when
// one long monotonic timer would have elapsed.
func TestTokenRefresher_SimulatedSleepRefreshesOnNextWake(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	now := time.Now()
	current := now
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(d)
	}

	var calls atomic.Int32
	refreshed := make(chan struct{}, 8)
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: now.Add(60 * time.Minute),
		Lead:      5 * time.Minute,
		MinWait:   time.Millisecond,
		MaxWait:   20 * time.Millisecond,
		Now:       nowFn,
		Refresh: func(context.Context) (time.Time, error) {
			calls.Add(1)
			refreshed <- struct{}{}
			return current.Add(time.Hour), nil
		},
	})
	r.Start()
	t.Cleanup(r.Stop)

	// Let the loop arm on capped re-check waits, then jump the wall clock
	// past expiry-lead in one step: the next wake must refresh.
	time.Sleep(50 * time.Millisecond)
	advance(56 * time.Minute)

	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh never fired after the wall clock jumped past expiry-lead")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Refresh calls = %d, want exactly 1 after the jump", got)
	}
}

// TestTokenRefresher_WakeBeforeLeadDoesNotRefresh pins the wall-clock
// gate: a wake while now is still before expiry-lead re-arms without
// calling Refresh — capped waits are re-checks, not refresh triggers.
// It also pins the exact boundary: now == expiry-lead is already due.
func TestTokenRefresher_WakeBeforeLeadDoesNotRefresh(t *testing.T) {
	t.Parallel()

	now := time.Now()
	expiry := now.Add(time.Hour)
	lead := 5 * time.Minute

	var calls atomic.Int32
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: expiry,
		Lead:      lead,
		MinWait:   time.Millisecond,
		MaxWait:   10 * time.Millisecond,
		Now:       func() time.Time { return now },
		Refresh: func(context.Context) (time.Time, error) {
			calls.Add(1)
			return time.Now().Add(time.Hour), nil
		},
	})

	if r.due() {
		t.Fatal("due() = true 55 min before expiry-lead, want false")
	}

	// Exact boundary: now == expiry-lead is due.
	atBoundary := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt: expiry,
		Lead:      lead,
		MinWait:   time.Millisecond,
		MaxWait:   10 * time.Millisecond,
		Now:       func() time.Time { return expiry.Add(-lead) },
		Refresh: func(context.Context) (time.Time, error) {
			return time.Time{}, nil
		},
	})
	if !atBoundary.due() {
		t.Fatal("due() = false exactly at expiry-lead, want true")
	}

	r.Start()
	t.Cleanup(r.Stop)
	time.Sleep(60 * time.Millisecond) // several capped wakes, all before the lead
	if got := calls.Load(); got != 0 {
		t.Fatalf("Refresh calls = %d while still before expiry-lead, want 0", got)
	}
}

// TestTokenRefresher_UnknownExpiryGatedUntilAssumedBoundary covers the
// seeded unknown-expiry window with a production-sized TTL: with no expiry
// from the platform the refresher assumes now+AssumedTTL, re-checks on the
// MaxWait ceiling without refreshing, and refreshes exactly once after a
// wall-clock jump past the assumed expiry-lead — the same host-sleep shape
// as a known expiry, not a refresh on the first capped wake.
func TestTokenRefresher_UnknownExpiryGatedUntilAssumedBoundary(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	now := time.Now()
	current := now
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(d)
	}

	var calls atomic.Int32
	refreshed := make(chan struct{}, 8)
	r := newTokenRefresher(tokenRefresherOptions{
		ExpiresAt:  time.Time{}, // unknown: seeded to now+AssumedTTL
		AssumedTTL: time.Hour,
		Lead:       5 * time.Minute,
		MinWait:    time.Millisecond,
		MaxWait:    20 * time.Millisecond,
		Now:        nowFn,
		Refresh: func(context.Context) (time.Time, error) {
			calls.Add(1)
			refreshed <- struct{}{}
			return current.Add(time.Hour), nil
		},
	})

	// Before the assumed boundary the seeded window is not due, and the
	// capped wait only re-arms.
	if r.due() {
		t.Fatal("due() = true at the start of a seeded unknown-expiry window, want false")
	}
	if got := r.nextWait(); got != 20*time.Millisecond {
		t.Fatalf("nextWait() = %v for a seeded 55m window, want capped MaxWait (%v)", got, 20*time.Millisecond)
	}

	r.Start()
	t.Cleanup(r.Stop)
	time.Sleep(60 * time.Millisecond) // several capped wakes, all before the assumed lead
	if got := calls.Load(); got != 0 {
		t.Fatalf("Refresh calls = %d before the assumed expiry-lead, want 0", got)
	}

	// Jump the wall clock past the assumed expiry-lead: the next wake
	// must refresh exactly once.
	advance(56 * time.Minute)

	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh never fired after the wall clock jumped past the assumed expiry-lead")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Refresh calls = %d after the jump, want exactly 1", got)
	}
}

// TestSetCredentials_FansOutToServices pins the cross-service credential
// push: when one path re-mints the token (proactive refresher or either
// reactive loop), SetCredentials updates what the OTHER loops present —
// without it each loop burns its own 401 round-trip + log cycle per expiry.
func TestSetCredentials_FansOutToServices(t *testing.T) {
	t.Parallel()

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_old",
		OrchestratorURL: "http://127.0.0.1:0",
		RuntimeJWT:      "old.jwt",
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     func() int { return 1 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
	})
	ps := NewPollService(PollOptions{
		WorkerID:        "wkr_old",
		OrchestratorURL: "http://127.0.0.1:0",
		RuntimeJWT:      "old.jwt",
		OnWork:          func(PollWorkItem) error { return nil },
	})

	hs.SetCredentials("wkr_new", "fresh.jwt")
	ps.SetCredentials("wkr_new", "fresh.jwt")

	if id, jwt := hs.CurrentCredentials(); id != "wkr_new" || jwt != "fresh.jwt" {
		t.Errorf("heartbeat credentials = (%q, %q), want (wkr_new, fresh.jwt)", id, jwt)
	}
	if id, jwt := ps.CurrentCredentials(); id != "wkr_new" || jwt != "fresh.jwt" {
		t.Errorf("poll credentials = (%q, %q), want (wkr_new, fresh.jwt)", id, jwt)
	}

	// Empty values must be ignored, not clobber live credentials.
	hs.SetCredentials("", "")
	ps.SetCredentials("", "")
	if id, jwt := hs.CurrentCredentials(); id != "wkr_new" || jwt != "fresh.jwt" {
		t.Errorf("heartbeat credentials clobbered by empties: (%q, %q)", id, jwt)
	}
	if id, jwt := ps.CurrentCredentials(); id != "wkr_new" || jwt != "fresh.jwt" {
		t.Errorf("poll credentials clobbered by empties: (%q, %q)", id, jwt)
	}
}

func TestParseTokenExpiry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		in       string
		wantZero bool
	}{
		{name: "valid RFC3339", in: "2026-06-10T12:00:00Z", wantZero: false},
		{name: "valid with offset", in: "2026-06-10T12:00:00+02:00", wantZero: false},
		{name: "empty", in: "", wantZero: true},
		{name: "malformed", in: "not-a-time", wantZero: true},
		{name: "unix seconds", in: "1765368000", wantZero: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseTokenExpiry(tc.in)
			if got.IsZero() != tc.wantZero {
				t.Errorf("parseTokenExpiry(%q) = %v, wantZero=%v", tc.in, got, tc.wantZero)
			}
		})
	}
}
