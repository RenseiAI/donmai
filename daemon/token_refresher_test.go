package daemon

import (
	"context"
	"errors"
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
