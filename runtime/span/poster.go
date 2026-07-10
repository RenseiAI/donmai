package span

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
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	// DefaultEndpointPath is the daemon-authenticated additive span ingest
	// route. BaseURL remains caller-supplied; the OSS package embeds no hosted
	// service address.
	DefaultEndpointPath = "/api/daemon/traces"
	// DefaultFlushInterval bounds how long a partial batch waits.
	DefaultFlushInterval = 100 * time.Millisecond
	// DefaultFlushSpans forces a flush at this many completed spans.
	DefaultFlushSpans = 20
	// DefaultQueueSize bounds memory and protects the agent hot path.
	DefaultQueueSize = 512
	// DefaultMaxRetries is the per-batch retry budget after the first attempt.
	DefaultMaxRetries = 2
	// DefaultInitialBackoff is the delay before the first retry.
	DefaultInitialBackoff = 100 * time.Millisecond
	// DefaultMaxBackoff caps exponential retry delay for one batch.
	DefaultMaxBackoff = time.Second
	// DefaultHTTPTimeout bounds a request when no client is injected.
	DefaultHTTPTimeout = 5 * time.Second
	// DefaultStopDrainTimeout bounds the final Stop flush.
	DefaultStopDrainTimeout = 2 * time.Second
)

// RuntimeCredentials are the bearer credentials for one span batch.
type RuntimeCredentials struct {
	AuthToken string
}

// CredentialProvider returns the freshest runtime bearer token. It is invoked
// before every HTTP attempt so token rotation does not require a restart.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// PosterConfig configures the bounded, batched span forwarder.
type PosterConfig struct {
	BaseURL      string
	EndpointPath string
	AuthToken    string

	CredentialProvider CredentialProvider
	HTTPClient         *http.Client
	Logger             *slog.Logger
	Sleep              func(time.Duration)

	FlushInterval    time.Duration
	FlushSpans       int
	QueueSize        int
	MaxRetries       int
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
	StopDrainTimeout time.Duration
}

func (c PosterConfig) endpointPath() string {
	path := c.EndpointPath
	if path == "" {
		path = DefaultEndpointPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func (c PosterConfig) flushInterval() time.Duration {
	if c.FlushInterval > 0 {
		return c.FlushInterval
	}
	return DefaultFlushInterval
}

func (c PosterConfig) flushSpans() int {
	if c.FlushSpans > 0 {
		return c.FlushSpans
	}
	return DefaultFlushSpans
}

func (c PosterConfig) queueSize() int {
	if c.QueueSize > 0 {
		return c.QueueSize
	}
	return DefaultQueueSize
}

func (c PosterConfig) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return DefaultMaxRetries
}

func (c PosterConfig) initialBackoff() time.Duration {
	if c.InitialBackoff > 0 {
		return c.InitialBackoff
	}
	return DefaultInitialBackoff
}

func (c PosterConfig) maxBackoff() time.Duration {
	if c.MaxBackoff > 0 {
		return c.MaxBackoff
	}
	return DefaultMaxBackoff
}

func (c PosterConfig) stopDrainTimeout() time.Duration {
	if c.StopDrainTimeout > 0 {
		return c.StopDrainTimeout
	}
	return DefaultStopDrainTimeout
}

func (c PosterConfig) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

func (c PosterConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c PosterConfig) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (c PosterConfig) credentials(ctx context.Context) RuntimeCredentials {
	creds := RuntimeCredentials{AuthToken: c.AuthToken}
	if c.CredentialProvider == nil {
		return creds
	}
	fresh, err := c.CredentialProvider(ctx)
	if err != nil {
		c.logger().Warn("span credential refresh failed; using cached credentials", "err", err)
		return creds
	}
	if fresh.AuthToken != "" {
		creds.AuthToken = fresh.AuthToken
	}
	return creds
}

// Poster batches completed spans on a 100ms-or-N schedule and POSTs each batch
// as a JSON array. Send is non-blocking and race-safe with Stop; queue overflow
// drops telemetry rather than delaying the model/tool hot path.
type Poster struct {
	cfg PosterConfig

	mu      sync.Mutex
	queue   chan agent.Span
	started bool
	stopped bool
	done    chan struct{}

	dropped atomic.Uint64
	posted  atomic.Uint64
	batches atomic.Uint64
}

// NewPoster validates cfg and returns a stopped-safe, not-yet-started poster.
func NewPoster(cfg PosterConfig) (*Poster, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("runtime/span: BaseURL required")
	}
	return &Poster{
		cfg:   cfg,
		queue: make(chan agent.Span, cfg.queueSize()),
		done:  make(chan struct{}),
	}, nil
}

// Start launches the batching worker. It is idempotent until Stop; starting a
// stopped poster returns an error instead of launching on a closed queue.
func (p *Poster) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return errors.New("runtime/span: cannot start stopped Poster")
	}
	if p.started {
		return nil
	}
	p.started = true
	// The caller-supplied context scopes the worker and every HTTP request;
	// Stop provides the independent queue-close/drain signal.
	go p.run(ctx) //nolint:gosec // G118: run receives ctx; only Stop's bounded tail flush uses Background.
	return nil
}

// Send enqueues s without blocking. The bool reports acceptance; false means
// the poster was not running or the bounded queue was full.
func (p *Poster) Send(s agent.Span) bool {
	if s == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.stopped {
		return false
	}
	select {
	case p.queue <- s:
		return true
	default:
		p.dropped.Add(1)
		p.cfg.logger().Warn("span queue full; dropping span")
		return false
	}
}

// Stop closes the queue and waits for a final best-effort flush. It is
// idempotent and safe to race with Send.
func (p *Poster) Stop() error {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.queue)
		if !p.started {
			close(p.done)
		}
	}
	p.mu.Unlock()

	select {
	case <-p.done:
	case <-time.After(p.cfg.stopDrainTimeout()):
		p.cfg.logger().Warn("span poster stop drain timeout", "timeout", p.cfg.stopDrainTimeout())
	}
	return nil
}

// Dropped returns the number of spans rejected by queue backpressure,
// serialization, or exhausted/non-retryable delivery.
func (p *Poster) Dropped() uint64 { return p.dropped.Load() }

// Posted returns the number of spans acknowledged by a 2xx response.
func (p *Poster) Posted() uint64 { return p.posted.Load() }

// Batches returns the number of batches acknowledged by a 2xx response.
func (p *Poster) Batches() uint64 { return p.batches.Load() }

func (p *Poster) run(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(p.cfg.flushInterval())
	defer ticker.Stop()
	buf := make([]agent.Span, 0, p.cfg.flushSpans())
	deliveryCtx := ctx
	ctxDone := ctx.Done()

	flush := func(flushCtx context.Context) {
		if len(buf) == 0 {
			return
		}
		batch := append([]agent.Span(nil), buf...)
		buf = buf[:0]
		p.deliver(flushCtx, batch)
	}

	for {
		select {
		case s, ok := <-p.queue:
			if !ok {
				drainCtx, cancel := context.WithTimeout(context.Background(), p.cfg.stopDrainTimeout())
				flush(drainCtx)
				cancel()
				return
			}
			buf = append(buf, s)
			if len(buf) >= p.cfg.flushSpans() {
				flush(deliveryCtx)
			}
		case <-ticker.C:
			flush(deliveryCtx)
		case <-ctxDone:
			// Runner cancellation must not strand queued telemetry before the
			// deferred Stop. Stop remains the lifecycle signal; after caller
			// cancellation, bounded HTTP-client timeouts govern best-effort
			// delivery until Stop closes the queue and supplies its own drain ctx.
			deliveryCtx = context.Background()
			ctxDone = nil
		}
	}
}

func (p *Poster) deliver(ctx context.Context, spans []agent.Span) {
	raw := make([]json.RawMessage, 0, len(spans))
	for _, s := range spans {
		encoded, err := agent.MarshalSpan(s)
		if err != nil {
			p.dropped.Add(1)
			p.cfg.logger().Warn("span serialization failed; dropping span", "err", err)
			continue
		}
		raw = append(raw, encoded)
	}
	if len(raw) == 0 {
		return
	}
	body, err := json.Marshal(raw)
	if err != nil {
		p.dropped.Add(uint64(len(raw)))
		p.cfg.logger().Warn("span batch serialization failed; dropping batch", "err", err)
		return
	}

	maxAttempts := p.cfg.maxRetries() + 1
	backoff := p.cfg.initialBackoff()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		err := p.post(ctx, body)
		if err == nil {
			p.posted.Add(uint64(len(raw)))
			p.batches.Add(1)
			return
		}
		lastErr = err
		var permanent permanentHTTPError
		if errors.As(err, &permanent) {
			break
		}
		if attempt < maxAttempts {
			if backoff > p.cfg.maxBackoff() {
				backoff = p.cfg.maxBackoff()
			}
			p.cfg.sleep(backoff)
			backoff *= 2
		}
	}
	p.dropped.Add(uint64(len(raw)))
	p.cfg.logger().Warn("span batch delivery failed; dropping batch", "spans", len(raw), "err", lastErr)
}

func (p *Poster) post(ctx context.Context, body []byte) error {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + p.cfg.endpointPath()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return permanentHTTPError{status: 0, err: fmt.Errorf("build span request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	creds := p.cfg.credentials(ctx)
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	res, err := p.cfg.client().Do(req)
	if err != nil {
		return fmt.Errorf("span post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil
	}
	errBody, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	msg := strings.TrimSpace(string(errBody))
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode >= 400 && res.StatusCode < 500 {
		return permanentHTTPError{
			status: res.StatusCode,
			err:    fmt.Errorf("span HTTP %d: %s", res.StatusCode, msg),
		}
	}
	return fmt.Errorf("span HTTP %d: %s", res.StatusCode, msg)
}

type permanentHTTPError struct {
	status int
	err    error
}

func (e permanentHTTPError) Error() string { return e.err.Error() }
func (e permanentHTTPError) Unwrap() error { return e.err }
