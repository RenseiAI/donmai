package activity

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

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Default values for [Config]. Exposed so callers can tune individual
// knobs without re-deriving the rest.
const (
	// DefaultQueueSize is the bounded send-queue capacity. Sized so the
	// runner can buffer one tool-heavy minute (~256 events at typical
	// AssistantText + ToolUse cadence) before the queue starts dropping
	// on backpressure.
	DefaultQueueSize = 256

	// DefaultMaxRetries is the per-event HTTP retry budget. After the
	// limit the event is dropped with a warn log; we never re-queue —
	// the platform's activity table is best-effort observability.
	DefaultMaxRetries = 5

	// DefaultInitialBackoff is the first-retry sleep; subsequent
	// retries double up to MaxBackoff. Matches result.Poster's pattern
	// (1s base, exponential), but tighter on the floor because activity
	// events are higher-volume.
	DefaultInitialBackoff = 250 * time.Millisecond

	// DefaultMaxBackoff caps the exponential backoff for a single
	// event's retry loop.
	DefaultMaxBackoff = 5 * time.Second

	// DefaultHTTPTimeout is the per-request timeout used when the
	// caller does not inject an [http.Client]. Activity posts are tiny;
	// 5s is generous.
	DefaultHTTPTimeout = 5 * time.Second

	// DefaultStopDrainTimeout caps how long Stop waits for in-flight
	// jobs to drain before the worker goroutine returns.
	DefaultStopDrainTimeout = 2 * time.Second

	// MaxToolSummaryChars caps the activity content for ToolUseEvent /
	// ToolResultEvent so the platform's activity buffer doesn't grow
	// unbounded on noisy tool outputs.
	MaxToolSummaryChars = 200

	// MaxToolOutputChars caps ToolResultEvent.Content forwarded as
	// toolOutput. Generous enough to preserve the gh pr create URL the
	// platform-side parser scans for.
	MaxToolOutputChars = 500
)

// RuntimeCredentials are the bearer-token credentials needed for an
// activity post. Empty fields fall back to the corresponding [Config]
// fields. Mirrored on
// [github.com/RenseiAI/agentfactory-tui/runtime/heartbeat.RuntimeCredentials]
// for symmetry — the runner builds one provider for both seams.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials
// available to the caller. Implementations should be cheap and
// concurrency-safe; the poster invokes it before every HTTP retry so
// daemon-side runtime-token refreshes propagate without restart.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// Config carries the inputs Poster needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/activity). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in the
	// request body so the platform can verify ownership.
	WorkerID string
	// BaseURL is the platform API base, e.g. "https://app.rensei.ai".
	// Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header. Empty
	// means no auth header — used by tests against unauthenticated
	// httptest.Server instances.
	AuthToken string
	// CredentialProvider returns the latest worker id + runtime token.
	// When set, every HTTP attempt calls it before posting so child
	// runners pick up daemon-side runtime-token refreshes mid-session.
	CredentialProvider CredentialProvider

	// HTTPClient overrides http.DefaultClient.
	HTTPClient *http.Client
	// Logger overrides slog.Default(). The poster logs at debug for
	// successful posts and warn for drops / unrecoverable failures.
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
	// Sleep overrides time.Sleep for retry backoff in tests.
	Sleep func(time.Duration)

	// ProviderName identifies the AgentRuntime provider that emitted the
	// events ("claude", "codex", "stub", …). The platform-side hook bridge
	// uses it to build a ProviderRef for Layer 6 hook events when
	// translating activities into pre-tool-use / post-tool-use payloads.
	// Empty is permitted; the bridge falls back to "unknown".
	ProviderName string

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

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// credentials resolves the freshest runtime credentials, falling back
// to the static Config values when CredentialProvider is unset or
// errors. Mirrors the heartbeat package's contract so callers can
// share one provider implementation.
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
		c.logger().Warn("activity credential refresh failed; using cached credentials", "err", err)
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

// job is one queued activity emission.
type job struct {
	event      agent.Event
	enqueuedAt time.Time
	// durationMs is the elapsed wall-clock time between a ToolUseEvent and
	// the matching ToolResultEvent (paired by ToolUseID). Zero for events
	// that aren't ToolResultEvent, or for ToolResultEvent without a paired
	// start time on the in-process timing map (e.g. when the result arrives
	// after a poster restart). Stamped in Send so deliver doesn't have to
	// re-look up the start time in the (possibly evicted) timing map.
	durationMs int64
}

// Poster pushes [agent.Event] values to the platform's
// /api/sessions/<id>/activity endpoint asynchronously, with a bounded
// retry queue. Construct via [New], call [Poster.Start] to launch the
// worker, [Poster.Send] to enqueue events, and [Poster.Stop] to drain
// + shut down. All methods are safe for concurrent use.
type Poster struct {
	cfg Config

	queue chan job

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	stopped   atomic.Bool
	done      chan struct{}

	// runningPosted gates the one-shot status=running nudge fired after
	// the first successful activity POST. atomic.Bool keeps Send paths
	// lock-free.
	runningPosted atomic.Bool

	// toolUseStartTimes tracks the wall-clock at which each ToolUseEvent
	// was enqueued, keyed by ToolUseID. The matching ToolResultEvent
	// computes durationMs by subtracting the recorded start (after which
	// the entry is deleted). sync.Map fits the read-mostly-then-delete
	// access pattern and keeps Send lock-free. Memory is bounded by the
	// in-flight tool-call count; orphan entries (no matching result event)
	// are tolerated — they leak only for the lifetime of the Poster.
	toolUseStartTimes sync.Map
}

// New validates cfg and returns a non-started Poster. Returns an error
// when SessionID or BaseURL is missing.
func New(cfg Config) (*Poster, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/activity: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/activity: BaseURL required")
	}
	p := &Poster{
		cfg:   cfg,
		queue: make(chan job, cfg.queueSize()),
		done:  make(chan struct{}),
	}
	return p, nil
}

// Start launches the background worker goroutine. Idempotent: subsequent
// calls are no-ops. The supplied ctx scopes the worker's lifetime; the
// worker also exits when [Poster.Stop] closes the queue.
func (p *Poster) Start(ctx context.Context) error {
	p.startOnce.Do(func() {
		p.started.Store(true)
		go p.run(ctx)
	})
	return nil
}

// Stop closes the queue, waits up to [Config.StopDrainTimeout] for
// in-flight jobs to drain, and then returns. Idempotent and safe to
// call from a deferred runner cleanup path.
func (p *Poster) Stop() error {
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		close(p.queue)
	})
	if !p.started.Load() {
		return nil
	}
	select {
	case <-p.done:
	case <-time.After(p.cfg.stopDrainTimeout()):
		p.cfg.logger().Warn("activity poster stop drain timeout",
			"sessionId", p.cfg.SessionID,
			"timeout", p.cfg.stopDrainTimeout())
	}
	return nil
}

// Send enqueues ev for async delivery. Non-blocking — if the queue is
// full or the poster is stopped/never-started, the event is dropped
// with a warn log. Events that map to nothing (Init / System /
// ToolProgress) are filtered up-front so they never consume queue
// capacity.
func (p *Poster) Send(_ context.Context, ev agent.Event) {
	if ev == nil {
		return
	}
	if !p.started.Load() {
		return
	}
	if p.stopped.Load() {
		return
	}
	now := p.cfg.now()
	if _, ok := mapEvent(ev, now, p.cfg.ProviderName, 0); !ok {
		// Skip events that don't map to a platform activity shape —
		// no point enqueuing only to drop them in the worker.
		return
	}

	// Track tool-use timings for downstream durationMs calculation.
	// On a tool-use: record start time.
	// On a tool-result: look up start, compute delta, delete the entry.
	// Both branches are no-ops when ToolUseID is empty (some providers
	// don't expose it; downstream consumers tolerate zero durationMs).
	var durationMs int64
	switch e := ev.(type) {
	case agent.ToolUseEvent:
		if e.ToolUseID != "" {
			p.toolUseStartTimes.Store(e.ToolUseID, now)
		}
	case agent.ToolResultEvent:
		if e.ToolUseID != "" {
			if startAny, ok := p.toolUseStartTimes.LoadAndDelete(e.ToolUseID); ok {
				if start, ok := startAny.(time.Time); ok {
					durationMs = now.Sub(start).Milliseconds()
				}
			}
		}
	}

	j := job{event: ev, enqueuedAt: now, durationMs: durationMs}
	select {
	case p.queue <- j:
	default:
		p.cfg.logger().Warn("activity queue full; dropping event",
			"sessionId", p.cfg.SessionID,
			"kind", ev.Kind())
	}
}

// run is the worker goroutine. Reads jobs until the queue closes,
// calling deliver for each. Closes Poster.done on exit so Stop can
// observe completion.
func (p *Poster) run(ctx context.Context) {
	defer close(p.done)
	for j := range p.queue {
		// ctx-cancel doesn't stop the loop — Stop closes the queue,
		// which is the canonical shutdown signal. We pass ctx into
		// deliver so per-attempt request contexts respect it; once
		// it's done, the HTTP layer surfaces ctx.Err() and we move on.
		p.deliver(ctx, j)
	}
}

// deliver runs the retry loop for one event. After the first
// successful POST it best-effort fires the running-status nudge.
func (p *Poster) deliver(ctx context.Context, j job) {
	body, ok := mapEvent(j.event, j.enqueuedAt, p.cfg.ProviderName, j.durationMs)
	if !ok {
		// Defense in depth — Send already filtered these out. Reachable
		// only if the mapping table changes mid-run.
		return
	}

	maxAttempts := p.cfg.maxRetries() + 1 // n retries → n+1 attempts
	backoff := p.cfg.initialBackoff()
	maxBackoff := p.cfg.maxBackoff()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := p.postActivity(ctx, body)
		if err == nil {
			p.cfg.logger().Debug("activity posted",
				"sessionId", p.cfg.SessionID,
				"type", body.Type,
				"attempt", attempt)
			p.maybePostRunning(ctx)
			return
		}
		lastErr = err

		var sErr stopErr
		if errors.As(err, &sErr) {
			p.cfg.logger().Warn("activity post non-retryable; dropping",
				"sessionId", p.cfg.SessionID,
				"type", body.Type,
				"status", sErr.status)
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
	p.cfg.logger().Warn("activity post failed after retries; dropping",
		"sessionId", p.cfg.SessionID,
		"type", body.Type,
		"attempts", maxAttempts,
		"err", lastErr)
}

// stopErr signals a non-retryable HTTP status (4xx other than 401).
// The retry loop unwraps it to short-circuit further attempts.
type stopErr struct {
	status int
	err    error
}

func (e stopErr) Error() string { return e.err.Error() }
func (e stopErr) Unwrap() error { return e.err }

// activityPayload is the full request body shape for
// POST /api/sessions/<id>/activity.
type activityPayload struct {
	WorkerID string  `json:"workerId"`
	Activity payload `json:"activity"`
}

// payload is the inner activity object — see platform's
// AgentActivity interface in src/app/api/sessions/[id]/activity/route.ts.
//
// The platform reconstructs Layer 6 hook events (pre-tool-use,
// post-tool-use, tool-use-error) from this payload per
// ADR-2026-05-12-cross-process-hook-bus-bridge; the additional fields
// ToolUseID / IsError / DurationMs / ProviderName below are what the
// bridge needs to do that translation faithfully.
type payload struct {
	Type         string         `json:"type"`
	Content      string         `json:"content"`
	ToolName     string         `json:"toolName,omitempty"`
	ToolInput    map[string]any `json:"toolInput,omitempty"`
	ToolCategory string         `json:"toolCategory,omitempty"`
	ToolOutput   string         `json:"toolOutput,omitempty"`
	// ToolUseID pairs ToolUseEvent and ToolResultEvent on the platform
	// bridge side; omitted when the source agent.Event didn't carry one.
	ToolUseID string `json:"toolUseId,omitempty"`
	// IsError is true for tool results that failed. Drives the platform's
	// translation rule between post-tool-use and tool-use-error.
	IsError bool `json:"isError,omitempty"`
	// DurationMs is the elapsed wall-clock time between the matching
	// ToolUseEvent and ToolResultEvent (paired by ToolUseID). Populated
	// only on ToolResultEvent payloads. Zero when no start timestamp
	// was found (orphan result, poster-restart edge case).
	DurationMs int64 `json:"durationMs,omitempty"`
	// ProviderName identifies the AgentRuntime provider that produced
	// the event. The platform-side hook bridge maps this onto the
	// ProviderRef.id for the Layer 6 event.
	ProviderName string `json:"providerName,omitempty"`
	Timestamp    string `json:"timestamp,omitempty"`
}

// postActivity issues one POST to /api/sessions/<id>/activity. Returns:
//   - nil on 2xx
//   - a stopErr on non-retryable 4xx (400, 403, 404, 422 …)
//   - a plain error on retryable conditions (network error, 401 with no
//     fresh creds, 5xx)
//
// 401 is treated as retryable specifically because the credential
// provider may have a fresher token than our cached one — the next
// attempt re-resolves credentials before sending.
func (p *Poster) postActivity(ctx context.Context, body payload) error {
	creds := p.cfg.credentials(ctx)
	wireBody := activityPayload{
		WorkerID: creds.WorkerID,
		Activity: body,
	}
	data, err := json.Marshal(wireBody)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := p.cfg.BaseURL + "/api/sessions/" + p.cfg.SessionID + "/activity"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
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
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Retryable — the credential provider may have a fresher token.
		return fmt.Errorf("activity post: 401 unauthorized")
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return stopErr{
			status: resp.StatusCode,
			err:    fmt.Errorf("activity post: status %d", resp.StatusCode),
		}
	}
	return fmt.Errorf("activity post: status %d", resp.StatusCode)
}

// maybePostRunning fires a one-shot POST /api/sessions/<id>/status with
// {"status":"running","workerId":"..."} after the first successful
// activity. Best-effort: any error is logged at debug and discarded.
func (p *Poster) maybePostRunning(ctx context.Context) {
	if !p.runningPosted.CompareAndSwap(false, true) {
		return
	}
	creds := p.cfg.credentials(ctx)
	body, err := json.Marshal(map[string]string{
		"status":   "running",
		"workerId": creds.WorkerID,
	})
	if err != nil {
		p.cfg.logger().Debug("status=running marshal failed",
			"sessionId", p.cfg.SessionID, "err", err)
		return
	}
	url := p.cfg.BaseURL + "/api/sessions/" + p.cfg.SessionID + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		p.cfg.logger().Debug("status=running new request failed",
			"sessionId", p.cfg.SessionID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	resp, err := p.cfg.client().Do(req)
	if err != nil {
		p.cfg.logger().Debug("status=running post failed",
			"sessionId", p.cfg.SessionID, "err", err)
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.cfg.logger().Debug("status=running non-2xx",
			"sessionId", p.cfg.SessionID, "status", resp.StatusCode)
		return
	}
	p.cfg.logger().Debug("status=running posted",
		"sessionId", p.cfg.SessionID)
}

// mapEvent translates an agent.Event into the platform activity shape.
// Returns ok=false for events that should not be forwarded
// (Init / System / ToolProgress) — those are runner-internal lifecycle
// signals the platform doesn't render.
//
// timestamp is the wall-clock time at which the event was observed; the
// platform server defaults to "now" when omitted, but emitting it here
// preserves causal ordering when the runner buffers under backpressure.
//
// providerName is stamped on every payload that doesn't already carry one
// so the platform-side hook-bus bridge can build a ProviderRef for the
// reconstructed pre/post-tool-use events. Empty is permitted.
//
// durationMs is meaningful only for ToolResultEvent payloads (the elapsed
// wall-clock time between the paired ToolUseEvent and this result, computed
// by the caller). Ignored for all other event kinds.
func mapEvent(ev agent.Event, ts time.Time, providerName string, durationMs int64) (payload, bool) {
	out := payload{
		Timestamp:    ts.UTC().Format(time.RFC3339Nano),
		ProviderName: providerName,
	}
	switch e := ev.(type) {
	case agent.AssistantTextEvent:
		text := strings.TrimSpace(e.Text)
		if text == "" {
			return payload{}, false
		}
		out.Type = "thought"
		out.Content = text
		return out, true

	case agent.ToolUseEvent:
		out.Type = "action"
		out.Content = summarizeToolUse(e)
		out.ToolName = e.ToolName
		out.ToolUseID = e.ToolUseID
		if e.ToolCategory != "" {
			out.ToolCategory = e.ToolCategory
		}
		if len(e.Input) > 0 {
			// Copy to a fresh map so downstream JSON marshal is
			// race-safe — the source map is owned by the provider
			// goroutine.
			input := make(map[string]any, len(e.Input))
			for k, v := range e.Input {
				input[k] = v
			}
			out.ToolInput = input
		}
		return out, true

	case agent.ToolResultEvent:
		out.Type = "action"
		name := e.ToolName
		if name == "" {
			name = "tool"
		}
		out.Content = name + " result"
		if e.IsError {
			out.Content = name + " result (error)"
		}
		out.ToolName = e.ToolName
		out.ToolUseID = e.ToolUseID
		out.IsError = e.IsError
		out.DurationMs = durationMs
		out.ToolOutput = truncate(e.Content, MaxToolOutputChars)
		return out, true

	case agent.ResultEvent:
		out.Type = "response"
		switch {
		case e.Message != "":
			out.Content = e.Message
		case e.Success:
			out.Content = "Session completed"
		case len(e.Errors) > 0:
			out.Content = strings.Join(e.Errors, "; ")
		default:
			out.Content = "Session ended"
		}
		return out, true

	case agent.ErrorEvent:
		out.Type = "error"
		switch {
		case e.Message != "":
			out.Content = e.Message
		case e.Code != "":
			out.Content = e.Code
		default:
			out.Content = "agent error"
		}
		return out, true

	case agent.InitEvent, agent.SystemEvent, agent.ToolProgressEvent:
		return payload{}, false
	}
	return payload{}, false
}

// summarizeToolUse produces a short one-line summary of a tool call,
// capped at MaxToolSummaryChars. Heuristics for the common Claude Code
// tools (Bash, Read, Edit, Write, Grep) preserve the most salient
// argument; unknown tools fall back to the bare tool name.
func summarizeToolUse(e agent.ToolUseEvent) string {
	name := e.ToolName
	if name == "" {
		name = "tool"
	}
	var arg string
	switch {
	case strings.EqualFold(name, "Bash"):
		arg = stringArg(e.Input, "command")
	case strings.EqualFold(name, "Read"), strings.EqualFold(name, "Edit"),
		strings.EqualFold(name, "Write"), strings.EqualFold(name, "NotebookEdit"):
		arg = stringArg(e.Input, "file_path")
	case strings.EqualFold(name, "Grep"), strings.EqualFold(name, "Glob"):
		arg = stringArg(e.Input, "pattern")
	case strings.EqualFold(name, "Agent"), strings.EqualFold(name, "Task"):
		// Sub-agent dispatch — surface the description so /topology can
		// render a human-readable sub-agent label.
		arg = stringArg(e.Input, "description")
		if arg == "" {
			arg = stringArg(e.Input, "prompt")
		}
	}
	if arg == "" {
		return truncate(name, MaxToolSummaryChars)
	}
	return truncate(name+": "+collapseWhitespace(arg), MaxToolSummaryChars)
}

// stringArg returns the trimmed string value of input[key], or "" when
// missing or non-string.
func stringArg(input map[string]any, key string) string {
	v, ok := input[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// truncate clips s to at most n runes, appending an ellipsis when
// the input was shortened. n <= 0 returns s untouched.
func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	if len(s) <= n {
		return s
	}
	const ellipsis = "..."
	if n <= len(ellipsis) {
		return s[:n]
	}
	return s[:n-len(ellipsis)] + ellipsis
}

// collapseWhitespace replaces runs of whitespace with a single space.
// Used by summarizeToolUse to keep the one-line summary readable when
// the tool input contains newlines (e.g. multi-line bash commands).
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	out := b.String()
	return strings.TrimRight(out, " ")
}
