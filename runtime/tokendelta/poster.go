// Package tokendelta implements the batched token-delta forwarder used by
// the interactive interview run-mode. During an
// interview the runner streams the agent's assistant-text output as
// token-delta frames so the browser can render the question incrementally.
//
// Transport (CONTRACT-FREEZE §4 + the SHARED CROSS-LANE token-delta INGEST
// contract): the runner POSTs batched frames to
//
//	POST {BaseURL}/api/sessions/{sessionId}/token-delta
//
// with worker runtime-JWT auth (exactly like /api/sessions/{id}/activity).
// Body:
//
//	{ "turnId": "...", "frames": [ { "index": N, "text": "...", "done": bool } ] }
//
// The platform resolves the interviewId from interviews.session_id and
// re-publishes each frame to the Redis channel interview:{interviewId}:token-deltas.
// The runner NEVER touches Redis directly.
//
// BATCHING IS A CONTRACT: the poster flushes at most every 100ms OR every
// 20 tokens (frames), whichever comes first. Raw per-token forwarding is
// prohibited — it would flood the activity path and evict the platform's
// 200-cap replica-local ring buffer.
package tokendelta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Batching contract constants (CONTRACT-FREEZE §4). The poster flushes
// when EITHER threshold is reached, whichever is first.
const (
	// DefaultFlushInterval is the maximum wall-clock between flushes.
	DefaultFlushInterval = 100 * time.Millisecond

	// DefaultFlushFrames is the maximum number of buffered frames before
	// a flush is forced regardless of the interval timer.
	DefaultFlushFrames = 20

	// DefaultQueueSize is the bounded ingest-channel capacity. Tokens
	// arrive faster than HTTP can drain on a slow link; a full queue
	// drops the frame with a warn rather than blocking the runner.
	DefaultQueueSize = 512

	// DefaultMaxRetries is the per-flush HTTP retry budget. Token deltas
	// are best-effort UI sugar — the authoritative transcript is the
	// interview_messages row written from the user-turn / completion path.
	DefaultMaxRetries = 2

	// DefaultInitialBackoff is the first-retry sleep; doubles up to
	// DefaultMaxBackoff.
	DefaultInitialBackoff = 100 * time.Millisecond

	// DefaultMaxBackoff caps the exponential backoff for one flush's
	// retry loop.
	DefaultMaxBackoff = 1 * time.Second

	// DefaultHTTPTimeout is the per-request timeout when the caller does
	// not inject an http.Client.
	DefaultHTTPTimeout = 5 * time.Second

	// DefaultStopDrainTimeout caps how long Stop waits for the worker to
	// flush the final buffer before returning.
	DefaultStopDrainTimeout = 2 * time.Second
)

// Frame is one token-delta frame. Maps to the platform wire-types
// TokenDeltaFrame (which the platform re-publishes onto the Redis channel
// as an AI SDK v6 UIMessageChunk text-delta).
//
// Index is monotonic per-turn (0-based); the browser orders frames by it.
// Done marks the final frame of a turn (the question is complete).
type Frame struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	Done  bool   `json:"done"`
}

// RuntimeCredentials are the bearer-token credentials needed for a
// token-delta post. Mirrors runtime/activity.RuntimeCredentials so the
// runner can share one CredentialProvider implementation.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials.
// Implementations should be cheap and concurrency-safe; the poster calls
// it before every HTTP attempt so daemon-side runtime-token refreshes
// propagate mid-interview.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// Config carries the inputs Poster needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/token-delta). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in the
	// body so the platform can verify ownership.
	WorkerID string
	// BaseURL is the platform API base (qw.PlatformURL). Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header. Empty is
	// permitted for tests against unauthenticated httptest servers.
	AuthToken string
	// CredentialProvider, when set, supplies the freshest worker id +
	// runtime token before every HTTP attempt.
	CredentialProvider CredentialProvider

	// HTTPClient overrides the default client.
	HTTPClient *http.Client
	// Logger overrides slog.Default().
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
	// Sleep overrides time.Sleep for retry backoff in tests.
	Sleep func(time.Duration)

	// FlushInterval overrides DefaultFlushInterval.
	FlushInterval time.Duration
	// FlushFrames overrides DefaultFlushFrames.
	FlushFrames int
	// QueueSize overrides DefaultQueueSize.
	QueueSize int
	// MaxRetries overrides DefaultMaxRetries.
	MaxRetries int
	// InitialBackoff overrides DefaultInitialBackoff.
	InitialBackoff time.Duration
	// MaxBackoff overrides DefaultMaxBackoff.
	MaxBackoff time.Duration
	// StopDrainTimeout overrides DefaultStopDrainTimeout.
	StopDrainTimeout time.Duration
}

func (c Config) flushInterval() time.Duration {
	if c.FlushInterval > 0 {
		return c.FlushInterval
	}
	return DefaultFlushInterval
}

func (c Config) flushFrames() int {
	if c.FlushFrames > 0 {
		return c.FlushFrames
	}
	return DefaultFlushFrames
}

func (c Config) queueSize() int {
	if c.QueueSize > 0 {
		return c.QueueSize
	}
	return DefaultQueueSize
}

func (c Config) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return DefaultMaxRetries
}

func (c Config) initialBackoff() time.Duration {
	if c.InitialBackoff > 0 {
		return c.InitialBackoff
	}
	return DefaultInitialBackoff
}

func (c Config) maxBackoff() time.Duration {
	if c.MaxBackoff > 0 {
		return c.MaxBackoff
	}
	return DefaultMaxBackoff
}

func (c Config) stopDrainTimeout() time.Duration {
	if c.StopDrainTimeout > 0 {
		return c.StopDrainTimeout
	}
	return DefaultStopDrainTimeout
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

func (c Config) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// credentials resolves the freshest runtime credentials, falling back to
// the static Config values when CredentialProvider is unset or errors.
func (c Config) credentials(ctx context.Context) RuntimeCredentials {
	creds := RuntimeCredentials{WorkerID: c.WorkerID, AuthToken: c.AuthToken}
	if c.CredentialProvider == nil {
		return creds
	}
	fresh, err := c.CredentialProvider(ctx)
	if err != nil {
		c.logger().Warn("token-delta credential refresh failed; using cached", "err", err)
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

// Poster batches token-delta frames and POSTs them to the platform on the
// 100ms-or-20-frames flush schedule. Construct via New, call Start to
// launch the worker, Send to enqueue a frame, and Stop to flush+shutdown.
// All methods are safe for concurrent use; in interview mode only the
// single runner goroutine calls Send.
type Poster struct {
	cfg Config

	queue chan Frame

	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	started   bool
	stopped   bool
	turnID    string // current turn correlation id; guarded by mu
	done      chan struct{}
}

// New validates cfg and returns a non-started Poster. Returns an error
// when SessionID or BaseURL is missing.
func New(cfg Config) (*Poster, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/tokendelta: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/tokendelta: BaseURL required")
	}
	return &Poster{
		cfg:   cfg,
		queue: make(chan Frame, cfg.queueSize()),
		done:  make(chan struct{}),
	}, nil
}

// Start launches the background flush worker. Idempotent. The supplied ctx
// scopes the worker's lifetime; the worker also exits when Stop closes the
// queue.
func (p *Poster) Start(ctx context.Context) error {
	p.startOnce.Do(func() {
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()
		go p.run(ctx)
	})
	return nil
}

// Send enqueues a frame for batched delivery. Non-blocking — a full queue
// or a stopped/never-started poster drops the frame with a debug log. The
// authoritative transcript lives in interview_messages, so a dropped delta
// only degrades the live token-by-token render, never correctness.
func (p *Poster) Send(f Frame) {
	p.mu.Lock()
	started := p.started
	stopped := p.stopped
	p.mu.Unlock()
	if !started || stopped {
		return
	}
	select {
	case p.queue <- f:
	default:
		p.cfg.logger().Debug("token-delta queue full; dropping frame",
			"sessionId", p.cfg.SessionID, "index", f.Index)
	}
}

// Stop closes the queue, waits up to StopDrainTimeout for the worker to
// flush the final buffer, then returns. Idempotent.
func (p *Poster) Stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		started := p.started
		p.mu.Unlock()
		close(p.queue)
		if !started {
			// Worker never launched; nothing to drain.
			close(p.done)
		}
	})
	select {
	case <-p.done:
	case <-time.After(p.cfg.stopDrainTimeout()):
		p.cfg.logger().Warn("token-delta poster stop drain timeout",
			"sessionId", p.cfg.SessionID, "timeout", p.cfg.stopDrainTimeout())
	}
	return nil
}

// run is the flush worker. It accumulates frames and flushes when EITHER
// the buffer reaches flushFrames OR the flush-interval timer fires
// (whichever first), exactly per the batching contract. On queue close it
// flushes whatever remains and exits.
func (p *Poster) run(ctx context.Context) {
	defer close(p.done)

	buf := make([]Frame, 0, p.cfg.flushFrames())
	ticker := time.NewTicker(p.cfg.flushInterval())
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		batch := make([]Frame, len(buf))
		copy(batch, buf)
		buf = buf[:0]
		p.deliver(ctx, batch)
	}

	for {
		select {
		case f, ok := <-p.queue:
			if !ok {
				// Queue closed by Stop — flush the tail and exit.
				flush()
				return
			}
			buf = append(buf, f)
			// 20-frame threshold: force a flush without waiting for the
			// interval timer. A `done` frame also forces an immediate
			// flush so the turn's terminal token reaches the browser
			// promptly (≤100ms otherwise).
			if len(buf) >= p.cfg.flushFrames() || f.Done {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// Best-effort final flush on cancel.
			flush()
			return
		}
	}
}

// requestBody is the POST body shape — matches the SHARED CROSS-LANE
// token-delta INGEST contract: { turnId, frames: [{index, text, done}] }.
type requestBody struct {
	TurnID string  `json:"turnId"`
	Frames []Frame `json:"frames"`
}

// deliver runs the retry loop for one batch of frames. The whole batch
// shares the same turnId (frames are produced within a single turn).
func (p *Poster) deliver(ctx context.Context, frames []Frame) {
	if len(frames) == 0 {
		return
	}
	// All frames in one flush belong to the same turn; the runner only
	// streams one turn at a time (claude single-in-flight). Carry the
	// turnId from the queued frames via the Poster's per-turn turnId — but
	// to keep frames self-contained the runner stamps turnId on the
	// Poster, not the Frame. We read it under lock.
	p.mu.Lock()
	turnID := p.turnID
	p.mu.Unlock()

	body := requestBody{TurnID: turnID, Frames: frames}

	maxAttempts := p.cfg.maxRetries() + 1
	backoff := p.cfg.initialBackoff()
	maxBackoff := p.cfg.maxBackoff()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := p.post(ctx, body)
		if err == nil {
			p.cfg.logger().Debug("token-delta batch posted",
				"sessionId", p.cfg.SessionID,
				"turnId", turnID,
				"frames", len(frames),
				"attempt", attempt)
			return
		}
		lastErr = err
		var sErr stopErr
		if errors.As(err, &sErr) {
			p.cfg.logger().Warn("token-delta post non-retryable; dropping batch",
				"sessionId", p.cfg.SessionID, "status", sErr.status)
			return
		}
		if attempt < maxAttempts {
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			p.cfg.sleep(backoff)
			backoff *= 2
		}
	}
	p.cfg.logger().Warn("token-delta post failed after retries; dropping batch",
		"sessionId", p.cfg.SessionID, "attempts", maxAttempts, "err", lastErr)
}

// SetTurn stamps the turn correlation id used for subsequent flushes. The
// runner calls it at the start of each question turn (before Send). Frames
// already buffered for a previous turn are flushed by the worker with the
// previous turnId only if SetTurn is called after a flush boundary; in
// practice the runner streams one turn to completion (terminating with a
// Done frame that forces a flush) before starting the next, so a turn's
// frames never straddle a SetTurn.
func (p *Poster) SetTurn(turnID string) {
	p.mu.Lock()
	p.turnID = turnID
	p.mu.Unlock()
}

// post issues one POST to /api/sessions/<id>/token-delta. Returns:
//   - nil on 2xx
//   - a stopErr on non-retryable 4xx (400/403/404/422)
//   - a plain error on retryable conditions (network, 401-no-fresh-creds, 5xx)
func (p *Poster) post(ctx context.Context, body requestBody) error {
	raw, err := json.Marshal(body)
	if err != nil {
		// Marshal failure is non-retryable.
		return stopErr{status: 0, err: fmt.Errorf("marshal token-delta body: %w", err)}
	}
	creds := p.cfg.credentials(ctx)
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/api/sessions/" + p.cfg.SessionID + "/token-delta"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build token-delta request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	res, err := p.cfg.client().Do(req)
	if err != nil {
		return fmt.Errorf("token-delta post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil
	}
	errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	msg := strings.TrimSpace(string(errBuf))
	// 401 is retryable (a fresh credential may land on the next attempt);
	// other 4xx are non-retryable.
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode >= 400 && res.StatusCode < 500 {
		return stopErr{status: res.StatusCode, err: fmt.Errorf("token-delta HTTP %d: %s", res.StatusCode, msg)}
	}
	return fmt.Errorf("token-delta HTTP %d: %s", res.StatusCode, msg)
}

// stopErr signals a non-retryable HTTP status. The retry loop unwraps it
// to short-circuit further attempts.
type stopErr struct {
	status int
	err    error
}

func (e stopErr) Error() string { return e.err.Error() }
func (e stopErr) Unwrap() error { return e.err }
