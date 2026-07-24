package pool

import (
	"testing"
	"time"
)

func TestSingleKey_EmptyYieldsNoCandidate(t *testing.T) {
	p := New(SingleKey{}, Options{})
	if _, err := p.Acquire(); err != ErrNoCredential {
		t.Fatalf("empty single key: err = %v, want ErrNoCredential", err)
	}
}

func TestSingleKey_DegradesToRetryWithBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	p := New(SingleKey{Key: Credential{ID: "k", Secret: "s"}},
		Options{Now: clock, Backoff: []time.Duration{10 * time.Second}})

	c, err := p.Acquire()
	if err != nil || c.ID != "k" {
		t.Fatalf("first acquire: %v %+v", err, c)
	}
	// A 429 cools the one key down; it is unavailable until the cooldown lapses.
	p.Report(c, 429)
	if _, err := p.Acquire(); err != ErrNoCredential {
		t.Fatalf("during cooldown: err = %v, want ErrNoCredential", err)
	}
	if p.Available() != 0 {
		t.Fatalf("available during cooldown = %d, want 0", p.Available())
	}
	// Half-open: after the cooldown lapses the key is retryable again.
	now = now.Add(11 * time.Second)
	if _, err := p.Acquire(); err != nil {
		t.Fatalf("after cooldown: err = %v, want available", err)
	}
}

func TestReport_SuccessClearsCooldown(t *testing.T) {
	now := time.Unix(0, 0)
	p := New(SingleKey{Key: Credential{ID: "k", Secret: "s"}}, Options{Now: func() time.Time { return now }})
	c, _ := p.Acquire()
	p.Report(c, 500)
	if p.Available() != 0 {
		t.Fatal("expected key cooling down after 500")
	}
	p.Report(c, 200)
	if p.Available() != 1 {
		t.Fatal("success should clear cooldown immediately")
	}
}

func TestReport_NonRetryableLeavesAvailable(t *testing.T) {
	p := New(SingleKey{Key: Credential{ID: "k", Secret: "s"}}, Options{})
	c, _ := p.Acquire()
	p.Report(c, 400) // request-level error, not a credential fault
	if p.Available() != 1 {
		t.Fatal("400 should not cool the credential down")
	}
}

// multiSource is a test CredentialSource yielding several keys (the platform
// shape the OSS state machine already supports).
type multiSource struct{ creds []Credential }

func (m multiSource) Credentials() []Credential { return m.creds }

func TestFailover_AcrossMultipleCredentials(t *testing.T) {
	now := time.Unix(0, 0)
	src := multiSource{creds: []Credential{{ID: "a", Secret: "1"}, {ID: "b", Secret: "2"}}}
	p := New(src, Options{Now: func() time.Time { return now }, Backoff: []time.Duration{time.Minute}})

	c, _ := p.Acquire()
	if c.ID != "a" {
		t.Fatalf("fill-first first pick = %q, want a", c.ID)
	}
	p.Report(c, 429) // a cools down → failover to b
	c2, err := p.Acquire()
	if err != nil || c2.ID != "b" {
		t.Fatalf("failover pick = %+v %v, want b", c2, err)
	}
	p.Report(c2, 401) // both cooling down now
	if _, err := p.Acquire(); err != ErrNoCredential {
		t.Fatalf("both cooling: err = %v, want ErrNoCredential", err)
	}
}

func TestRoundRobin_SpreadsAcrossCredentials(t *testing.T) {
	src := multiSource{creds: []Credential{{ID: "a", Secret: "1"}, {ID: "b", Secret: "2"}}}
	p := New(src, Options{Strategy: RoundRobin})
	first, _ := p.Acquire()
	second, _ := p.Acquire()
	if first.ID == second.ID {
		t.Fatalf("round robin returned same id twice: %q", first.ID)
	}
}

func TestRetryableStatus(t *testing.T) {
	retry := []int{401, 403, 408, 429, 500, 502, 503, 504}
	for _, c := range retry {
		if !RetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range []int{200, 400, 404, 422} {
		if RetryableStatus(c) {
			t.Errorf("status %d should not be retryable", c)
		}
	}
}

func TestBackoff_EscalatesThenClamps(t *testing.T) {
	now := time.Unix(0, 0)
	p := New(SingleKey{Key: Credential{ID: "k", Secret: "s"}},
		Options{Now: func() time.Time { return now }, Backoff: []time.Duration{time.Second, 10 * time.Second}})
	c, _ := p.Acquire()
	p.Report(c, 500) // failure 1 → 1s
	if p.states["k"].cooldownUntil != now.Add(time.Second) {
		t.Fatalf("first backoff = %v, want 1s", p.states["k"].cooldownUntil.Sub(now))
	}
	p.Report(c, 500) // failure 2 → 10s
	if p.states["k"].cooldownUntil != now.Add(10*time.Second) {
		t.Fatalf("second backoff = %v, want 10s", p.states["k"].cooldownUntil.Sub(now))
	}
	p.Report(c, 500) // failure 3 → clamps at last entry (10s)
	if p.states["k"].cooldownUntil != now.Add(10*time.Second) {
		t.Fatalf("clamped backoff = %v, want 10s", p.states["k"].cooldownUntil.Sub(now))
	}
}
