// Package stepheartbeat emits a decoupled per-session step-liveness signal
// to the platform. It exists to close governor Class-1 stale detection: a
// runner process that is alive and holding its ownership lock (the
// runtime/heartbeat pulser keeps ticking) but is producing no genuine
// tool/token events for minutes (a slow build, a hung subprocess, a network
// stall) is otherwise invisible to the fast (60s) stale-detection class,
// because nothing writes agent_sessions.last_step_heartbeat or the Redis
// session:heartbeat:<id> pointer the governor reads.
//
// The Emitter POSTs every Interval (default 15s) to
//
//	POST {BaseURL}/api/sessions/{SessionID}/step-heartbeat
//	{"workerId": "<worker>", "emittedAt": "<RFC3339>"}
//
// so the platform route can stamp agent_sessions.last_step_heartbeat +
// last_progress_at and refresh the Redis session:heartbeat pointer.
//
// This is deliberately a sibling of runtime/heartbeat (ownership
// lock-refresh) and runtime/activity (genuine-event ingest): it shares their
// Config / New / Start(ctx) / Stop() lifecycle so it drops into the runner
// identically. Unlike the ownership pulser, a step-heartbeat outage MUST NOT
// affect the session — every POST is best-effort (a 404 from a platform
// build without the companion route, a network error, any non-2xx) is logged
// and swallowed, mirroring maybePostRunning in runtime/activity/poster.go.
//
// The 15s cadence is calibrated against the platform's
// SESSION_STALE_THRESHOLD_MS (60s) — see
// donmai-libraries/packages/server/src/session-heartbeat.ts
// (DEFAULT_SESSION_HEARTBEAT_INTERVAL_MS = 15_000) and
// ADR-2026-04-29-long-running-runtime-substrate.md Decision 5. Do NOT
// parametrize the default away from 15s: a faster/slower daemon cadence
// silently desyncs the 60s governor threshold.
package stepheartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// DefaultInterval is the step-heartbeat cadence. Calibrated against the
// platform's SESSION_STALE_THRESHOLD_MS (60s): four beats fit inside one
// stale window so a single dropped POST never trips the governor.
const DefaultInterval = 15 * time.Second

// DefaultHTTPTimeout bounds one POST when the caller does not inject an
// [http.Client]. Step heartbeats are tiny; 5s is generous.
const DefaultHTTPTimeout = 5 * time.Second

// RuntimeCredentials are the bearer-token credentials needed for a
// step-heartbeat POST. Empty fields fall back to the corresponding [Config]
// fields. Mirrors runtime/heartbeat.RuntimeCredentials and
// runtime/activity.RuntimeCredentials so the runner can share one provider
// implementation across all three seams.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials
// available to the caller. Implementations should be cheap and
// concurrency-safe; the emitter invokes it before every POST so daemon-side
// runtime-token refreshes propagate without a restart.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// Config carries the inputs Emitter needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/step-heartbeat). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in the
	// request body so the platform can attribute the beat.
	WorkerID string
	// BaseURL is the platform API base, e.g. "https://platform.example.com".
	// Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header. Empty means
	// no auth header — used by tests against unauthenticated httptest.Server
	// instances.
	AuthToken string
	// CredentialProvider returns the latest worker id + runtime token. When
	// set, every tick calls it before posting so child runners pick up
	// daemon-side runtime-token refreshes mid-session.
	CredentialProvider CredentialProvider

	// Interval overrides DefaultInterval. Zero falls back to the 15s
	// default; do NOT override it in production (see package doc).
	Interval time.Duration

	// HTTPClient overrides the default client (tests inject
	// httptest.Server.Client()).
	HTTPClient *http.Client
	// Logger overrides slog.Default(). The emitter logs at debug for
	// successful beats and warn for swallowed failures.
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

func (c Config) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
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

// credentials resolves the freshest runtime credentials, falling back to the
// static Config values when CredentialProvider is unset or errors. Mirrors
// the heartbeat/activity packages so callers can share one provider.
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
		c.logger().Warn("step-heartbeat credential refresh failed; using cached credentials", "err", err)
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

// stepHeartbeatBody is the request body sent to
// POST /api/sessions/<id>/step-heartbeat. WorkerID attributes the beat;
// EmittedAt is a monotonic RFC3339 timestamp the platform stamps onto
// agent_sessions.last_step_heartbeat + last_progress_at.
type stepHeartbeatBody struct {
	WorkerID  string `json:"workerId,omitempty"`
	EmittedAt string `json:"emittedAt"`
}

// Emitter drives the step-heartbeat loop for one session. Construct via New,
// call Start to launch the loop, and Stop to shut it down. All methods are
// safe for concurrent use.
type Emitter struct {
	cfg Config

	mu      sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// New validates cfg and returns a non-started Emitter. Returns an error when
// SessionID or BaseURL is missing.
func New(cfg Config) (*Emitter, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/stepheartbeat: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/stepheartbeat: BaseURL required")
	}
	return &Emitter{cfg: cfg}, nil
}

// Start begins the step-heartbeat loop. The first beat fires synchronously
// before Start returns so the platform sees an immediate signal rather than
// waiting one Interval (mirrors the ownership pulser's synchronous first
// tick). The loop then runs until ctx is cancelled or Stop is called.
// Calling Start more than once returns an error; build a new Emitter per
// session.
func (e *Emitter) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("runtime/stepheartbeat: Start called twice")
	}
	e.started = true
	e.stopCh = make(chan struct{})
	e.doneCh = make(chan struct{})
	e.mu.Unlock()

	// Fire the first beat synchronously so the platform stamps
	// last_step_heartbeat immediately on session start.
	e.beat(ctx)

	go e.run(ctx)
	return nil
}

// Stop signals the loop to exit and blocks until it has. Idempotent and safe
// to call from a deferred cleanup path. Returns nil; the signature matches
// the sibling Stop helpers for symmetry.
func (e *Emitter) Stop() error {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	if e.stopCh != nil {
		close(e.stopCh)
	}
	doneCh := e.doneCh
	e.mu.Unlock()

	if doneCh != nil {
		<-doneCh
	}
	return nil
}

// run is the inner loop driving subsequent beats every Interval.
func (e *Emitter) run(ctx context.Context) {
	defer func() {
		e.mu.Lock()
		ch := e.doneCh
		e.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()

	ticker := time.NewTicker(e.cfg.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopChannel():
			return
		case <-ticker.C:
			e.beat(ctx)
		}
	}
}

func (e *Emitter) stopChannel() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopCh
}

// beat performs one best-effort step-heartbeat POST. A failure (network
// error, 404 from a platform build without the companion route, any non-2xx)
// is logged and swallowed — a step-heartbeat outage must NEVER fail the
// session. Mirrors maybePostRunning's failure posture in
// runtime/activity/poster.go.
func (e *Emitter) beat(ctx context.Context) {
	creds := e.cfg.credentials(ctx)
	body, err := json.Marshal(stepHeartbeatBody{
		WorkerID:  creds.WorkerID,
		EmittedAt: e.cfg.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		e.cfg.logger().Debug("step-heartbeat marshal failed",
			"sessionId", e.cfg.SessionID, "err", err)
		return
	}
	url := e.cfg.BaseURL + "/api/sessions/" + e.cfg.SessionID + "/step-heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		e.cfg.logger().Debug("step-heartbeat new request failed",
			"sessionId", e.cfg.SessionID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	resp, err := e.cfg.client().Do(req)
	if err != nil {
		e.cfg.logger().Debug("step-heartbeat post failed",
			"sessionId", e.cfg.SessionID, "err", err)
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Best-effort: a 404 (platform build without the companion route
		// yet) or any other non-2xx is logged and swallowed. The daemon
		// emitter sits inert against an unmodified platform without ever
		// disturbing the run.
		e.cfg.logger().Debug("step-heartbeat non-2xx",
			"sessionId", e.cfg.SessionID,
			"status", resp.StatusCode)
		return
	}
	e.cfg.logger().Debug("step-heartbeat posted",
		"sessionId", e.cfg.SessionID)
}
