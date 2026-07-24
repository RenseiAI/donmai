// Package pool selects a credential for an upstream exchange with rotation,
// cooldown, and failover. OSS ships a WORKING single-key source AND the full
// rotation state machine (08 §8): the machine is implemented and tested against
// the CredentialSource interface even though the OSS source yields one
// credential — with a single key it degrades to retry-with-backoff, and the
// platform adds BREADTH (a CredentialSource over an org vault) without changing
// BEHAVIOR.
//
// Structural boundary: a Credential carries only an opaque ID and the secret
// VALUE; there is no config shape that accepts a pooled/remote SUBSCRIPTION
// credential — Class S pooling is unrepresentable here by construction (the
// "never pool consumer subscriptions" guardrail made structural, per
// ADR-2026-07-24 §5).
package pool

import (
	"errors"
	"sync"
	"time"
)

// Credential is one selectable credential. ID is an opaque handle used for
// cooldown bookkeeping and logs; Secret is the key value and is never logged.
type Credential struct {
	ID     string
	Secret string
}

// CredentialSource yields the ordered candidate credentials for an upstream.
// OSS: SingleKey (one). Platform: a vault-backed source (many). The gateway
// never persists Secret to disk — the source hands values in at Acquire time.
type CredentialSource interface {
	Credentials() []Credential
}

// SingleKey is the OSS CredentialSource: exactly one credential from a value
// the caller already resolved (env / local auth state). Empty Secret yields no
// candidates, so Acquire returns ErrNoCredential — the honest "not configured"
// signal rather than a silent unauthenticated dial.
type SingleKey struct {
	Key Credential
}

// Credentials implements CredentialSource.
func (s SingleKey) Credentials() []Credential {
	if s.Key.Secret == "" {
		return nil
	}
	return []Credential{s.Key}
}

// Strategy selects the ordering discipline across available credentials.
type Strategy string

// Strategy constants.
const (
	// FillFirst always prefers the earliest available credential (stable;
	// good for a warmed cache/quota). Default.
	FillFirst Strategy = "fill-first"
	// RoundRobin spreads load across available credentials.
	RoundRobin Strategy = "round-robin"
)

// ErrNoCredential is returned when no credential is available — either none is
// configured or every configured credential is in cooldown.
var ErrNoCredential = errors.New("gateway/pool: no credential available")

// Options configures the state machine. Zero values use production defaults.
type Options struct {
	Strategy Strategy
	// Now is a clock seam for tests. Nil uses time.Now.
	Now func() time.Time
	// Backoff overrides the per-attempt cooldown schedule. Nil uses
	// defaultBackoff. Index i is the cooldown after the (i+1)-th consecutive
	// failure of a credential; the last entry is reused for further failures.
	Backoff []time.Duration
}

var defaultBackoff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// Pool is the credential-selection state machine. Safe for concurrent use.
type Pool struct {
	src      CredentialSource
	strategy Strategy
	now      func() time.Time
	backoff  []time.Duration

	mu     sync.Mutex
	states map[string]*credState
	cursor int // round-robin cursor
}

type credState struct {
	failures      int
	cooldownUntil time.Time
}

// New builds a Pool over src.
func New(src CredentialSource, opts Options) *Pool {
	p := &Pool{
		src:      src,
		strategy: opts.Strategy,
		now:      opts.Now,
		backoff:  opts.Backoff,
		states:   map[string]*credState{},
	}
	if p.strategy == "" {
		p.strategy = FillFirst
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.backoff == nil {
		p.backoff = defaultBackoff
	}
	return p
}

// Acquire returns the next credential not currently in cooldown, honoring the
// strategy. Returns ErrNoCredential when every candidate is cooling down (or
// none is configured) — the caller then surfaces a typed 503-class error
// rather than dialing unauthenticated.
func (p *Pool) Acquire() (Credential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	creds := p.src.Credentials()
	if len(creds) == 0 {
		return Credential{}, ErrNoCredential
	}
	now := p.now()

	// Ordering: round-robin rotates the start index; fill-first starts at 0.
	start := 0
	if p.strategy == RoundRobin {
		start = p.cursor % len(creds)
	}
	for i := 0; i < len(creds); i++ {
		c := creds[(start+i)%len(creds)]
		st := p.states[c.ID]
		if st != nil && now.Before(st.cooldownUntil) {
			continue // still cooling down
		}
		if p.strategy == RoundRobin {
			p.cursor = (start + i + 1) % len(creds)
		}
		return c, nil
	}
	return Credential{}, ErrNoCredential
}

// Report records the outcome of an exchange that used cred. A retryable
// failure status (RetryableStatus) advances the credential's cooldown along
// the backoff schedule (half-open: it becomes available again after the
// cooldown); a success clears its failure state. Non-retryable statuses
// (e.g. 400/404) leave the credential available — the fault is the request,
// not the credential.
func (p *Pool) Report(cred Credential, statusCode int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if statusCode >= 200 && statusCode < 300 {
		delete(p.states, cred.ID)
		return
	}
	if !RetryableStatus(statusCode) {
		return
	}
	st := p.states[cred.ID]
	if st == nil {
		st = &credState{}
		p.states[cred.ID] = st
	}
	st.failures++
	idx := st.failures - 1
	if idx >= len(p.backoff) {
		idx = len(p.backoff) - 1
	}
	st.cooldownUntil = p.now().Add(p.backoff[idx])
}

// RetryableStatus reports whether an HTTP status should cool a credential down
// and trigger failover: 401/403 (auth/quota), 408 (timeout), 429 (rate limit),
// and 5xx (upstream fault). Everything else is treated as a request-level
// error the same credential can serve again.
func RetryableStatus(code int) bool {
	switch code {
	case 401, 403, 408, 429:
		return true
	}
	return code >= 500 && code <= 599
}

// Available reports how many credentials are currently not in cooldown. Used
// by status surfaces and tests; never exposes secrets.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	n := 0
	for _, c := range p.src.Credentials() {
		st := p.states[c.ID]
		if st == nil || !now.Before(st.cooldownUntil) {
			n++
		}
	}
	return n
}
