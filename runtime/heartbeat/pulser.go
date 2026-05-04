package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is the per-session heartbeat cadence. Aligned with
// the worker-level heartbeat in daemon/heartbeat.go for visual
// consistency in operator dashboards.
const DefaultInterval = 30 * time.Second

// DefaultStrikesUntilLost is the consecutive-failure threshold that
// triggers a LostOwnership event. Mirrors MAX_HEARTBEAT_FAILURES from
// the legacy TS worker-runner.ts.
const DefaultStrikesUntilLost = 3

// DefaultMaxAttemptsPerTick is the HTTP retry budget for one tick.
// Mirrors apiRequestWithError in afclient/retry.go (3 attempts with
// 1s/2s/4s exponential backoff).
const DefaultMaxAttemptsPerTick = 3

// Sentinel errors. The runner uses errors.Is to detect a
// LostOwnership transition without scraping log lines.
var (
	// ErrLostOwnership is returned through Pulser.LostOwnership when
	// the platform has stopped accepting heartbeats for this session.
	// The runner halts work + relinquishes the worktree.
	ErrLostOwnership = errors.New("runtime/heartbeat: session ownership lost")
)

// Config carries the inputs Pulser needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/lock-refresh). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in
	// the request body so the platform can detect a hand-off.
	WorkerID string
	// IssueID is the platform-side Linear issue UUID. The platform's
	// /api/sessions/<id>/lock-refresh handler keys the per-issue lock
	// on issue:lock:{IssueID} and rejects the request with 400 when
	// this is empty — so callers must populate it (REN-1465). Sourced
	// from prompt.QueuedWork.IssueID (camelCase "issueId" on the wire).
	IssueID string
	// BaseURL is the platform API base, e.g. "https://app.rensei.ai".
	// Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header.
	// Optional — when empty no auth header is set (test paths use
	// httptest.Server without auth).
	AuthToken string
	// CredentialProvider returns the latest worker id + runtime token.
	// When set, every heartbeat tick calls it before posting so child
	// runners can pick up daemon-side runtime-token refreshes mid-session.
	CredentialProvider CredentialProvider

	// Interval overrides DefaultInterval. Zero falls back to default.
	Interval time.Duration
	// StrikesUntilLost overrides DefaultStrikesUntilLost. Zero falls
	// back to default.
	StrikesUntilLost int
	// MaxAttemptsPerTick overrides DefaultMaxAttemptsPerTick. Zero
	// falls back to default.
	MaxAttemptsPerTick int

	// HTTPClient overrides http.DefaultClient (tests inject
	// httptest.Server.Client()).
	HTTPClient *http.Client
	// Logger overrides slog.Default(). The pulser logs at debug for
	// successful ticks and warn for strikes.
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
	// Sleep overrides time.Sleep for inner-retry backoff in tests.
	Sleep func(time.Duration)
}

// RuntimeCredentials are the bearer-token credentials needed for a heartbeat
// request. Empty fields fall back to Config.WorkerID / Config.AuthToken.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials available
// to the caller. Implementations should be cheap and concurrency-safe.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

func (c Config) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

func (c Config) strikesUntilLost() int {
	if c.StrikesUntilLost > 0 {
		return c.StrikesUntilLost
	}
	return DefaultStrikesUntilLost
}

func (c Config) maxAttempts() int {
	if c.MaxAttemptsPerTick > 0 {
		return c.MaxAttemptsPerTick
	}
	return DefaultMaxAttemptsPerTick
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) credentials(ctx context.Context) RuntimeCredentials {
	creds := RuntimeCredentials{
		WorkerID:  c.WorkerID,
		AuthToken: c.AuthToken,
	}
	if c.CredentialProvider == nil {
		return creds
	}
	fresh, err := c.CredentialProvider(ctx)
	if err != nil {
		c.logger().Warn("heartbeat credential refresh failed; using cached credentials", "err", err)
		return creds
	}
	if fresh.WorkerID != "" {
		creds.WorkerID = fresh.WorkerID
	}
	if fresh.AuthToken != "" {
		creds.AuthToken = fresh.AuthToken
	}
	return creds
}

// Pulser drives the heartbeat loop for one session. Construct via New
// then call Start; Stop releases resources.
type Pulser struct {
	cfg Config

	mu       sync.Mutex
	stopped  bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	lostCh   chan struct{}
	strikes  atomic.Int64
	lastTick atomic.Int64 // unix-millis
}

// New returns a Pulser configured for the given session. Returns an
// error when SessionID or BaseURL is missing.
func New(cfg Config) (*Pulser, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/heartbeat: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/heartbeat: BaseURL required")
	}
	return &Pulser{cfg: cfg}, nil
}

// LostOwnership returns a channel that closes when the platform has
// stopped accepting heartbeats (3 consecutive ticks failed). The
// runner selects on this to abort the session early.
func (p *Pulser) LostOwnership() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh == nil {
		p.lostCh = make(chan struct{})
	}
	return p.lostCh
}

// Strikes returns the current consecutive-failure count. Useful for
// observability; the runner usually only watches LostOwnership.
func (p *Pulser) Strikes() int {
	return int(p.strikes.Load())
}

// LastTick returns the unix-ms timestamp of the most recent successful
// tick. Zero when no tick has succeeded yet.
func (p *Pulser) LastTick() int64 {
	return p.lastTick.Load()
}

// Start begins the heartbeat loop. The first tick fires synchronously
// before Start returns so the platform mirror updates without lag.
//
// The loop runs until ctx is cancelled, Stop is called, or the
// 3-strike threshold trips. Calling Start more than once on the same
// Pulser returns an error; build a new Pulser per session.
func (p *Pulser) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.stopCh != nil {
		p.mu.Unlock()
		return errors.New("runtime/heartbeat: Start called twice")
	}
	if p.lostCh == nil {
		p.lostCh = make(chan struct{})
	}
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	// Fire the first tick synchronously so the platform sees an
	// immediate refresh rather than waiting one Interval.
	p.tick(ctx)
	if p.tripped() {
		// First tick already tripped — surface it but still kick the
		// loop so Stop semantics remain consistent.
		p.cfg.logger().Warn("heartbeat: first tick already failed",
			"sessionId", p.cfg.SessionID)
	}

	go p.run(ctx)
	return nil
}

// Stop signals the loop to exit and blocks until it has. Idempotent
// and safe to call from a deferred cleanup path. Returns nil; the
// signature matches context-aware shutdown helpers elsewhere in the
// codebase for symmetry.
func (p *Pulser) Stop() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	if p.stopCh != nil {
		close(p.stopCh)
	}
	doneCh := p.doneCh
	p.mu.Unlock()

	if doneCh != nil {
		<-doneCh
	}
	return nil
}

// run is the inner loop driving subsequent ticks.
func (p *Pulser) run(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		ch := p.doneCh
		p.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()

	ticker := time.NewTicker(p.cfg.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChannel():
			return
		case <-ticker.C:
			p.tick(ctx)
			if p.tripped() {
				return
			}
		}
	}
}

func (p *Pulser) stopChannel() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCh
}

// tripped reports whether the strike counter has reached the lost
// threshold. The first transition closes the LostOwnership channel.
func (p *Pulser) tripped() bool {
	if int(p.strikes.Load()) < p.cfg.strikesUntilLost() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh != nil {
		// Close idempotently — already-closed channels panic on
		// re-close; guard with stopped flag set on close path.
		select {
		case <-p.lostCh:
			// already closed
		default:
			close(p.lostCh)
		}
	}
	return true
}

// tick performs one heartbeat attempt — including up to maxAttempts
// inner HTTP retries — and updates the strike counter. Returns no
// error: state is observed via Strikes / LostOwnership.
func (p *Pulser) tick(ctx context.Context) {
	attempts := p.cfg.maxAttempts()
	var lastErr error
	for n := 1; n <= attempts; n++ {
		err := p.doRefresh(ctx)
		if err == nil {
			p.strikes.Store(0)
			p.lastTick.Store(p.cfg.now().UnixMilli())
			p.cfg.logger().Debug("heartbeat tick ok",
				"sessionId", p.cfg.SessionID, "attempt", n)
			return
		}
		lastErr = err
		if n < attempts {
			backoff := time.Duration(1<<(n-1)) * time.Second
			if p.cfg.Sleep != nil {
				p.cfg.Sleep(backoff)
			} else {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
		}
	}
	strike := p.strikes.Add(1)
	p.cfg.logger().Warn("heartbeat tick failed",
		"sessionId", p.cfg.SessionID,
		"strike", strike,
		"strikesUntilLost", p.cfg.strikesUntilLost(),
		"err", lastErr)
}

// refreshRequest is the body shape sent to /api/sessions/<id>/lock-refresh.
// Matches the legacy TS body verbatim.
type refreshRequest struct {
	WorkerID string `json:"workerId,omitempty"`
	IssueID  string `json:"issueId,omitempty"`
}

// refreshResponse is the body shape returned by lock-refresh. The
// {"refreshed": false} case is treated as a strike-eligible failure
// because it means the platform did not extend the lock — the session
// has likely already been handed off.
type refreshResponse struct {
	Refreshed bool `json:"refreshed"`
}

// doRefresh issues one POST to /api/sessions/<id>/lock-refresh and
// returns nil only when the platform reports the lock was extended.
func (p *Pulser) doRefresh(ctx context.Context) error {
	creds := p.cfg.credentials(ctx)
	body, err := json.Marshal(refreshRequest{
		WorkerID: creds.WorkerID,
		IssueID:  p.cfg.IssueID,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := p.cfg.BaseURL + "/api/sessions/" + p.cfg.SessionID + "/lock-refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	resp, err := p.cfg.client().Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lock-refresh: status %d", resp.StatusCode)
	}
	var out refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// A parseable refresh response is not strictly required —
		// some operator-mode platform deployments respond 204 with no
		// body. Accept that case.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode: %w", err)
	}
	if !out.Refreshed {
		return errors.New("lock-refresh: platform refused (refreshed=false)")
	}
	return nil
}
