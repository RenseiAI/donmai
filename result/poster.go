package result

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultMaxAttempts is the legacy 3-attempt pattern from
// `apiRequestWithError` in worker-runner.ts. Exposed for tests that
// want to assert "did we retry" semantics.
const DefaultMaxAttempts = 3

// DefaultBaseDelay is the base delay for exponential backoff between
// attempts. Backoff is `BaseDelay * 2^(n-1)` — same shape as the legacy
// 1s / 2s / 4s sequence.
const DefaultBaseDelay = time.Second

// Poster posts an [agent.Result] back to the Rensei platform. The zero
// value is not usable — use [NewPoster].
//
// Posters are safe for concurrent use; all fields are read-only after
// construction.
type Poster struct {
	// platformURL is the base URL of the platform (e.g.
	// "https://app.rensei.ai" or "http://127.0.0.1:3010"). Path joining
	// uses the URL package to avoid double-slash bugs.
	platformURL *url.URL

	// authToken is the worker bearer token (runtime_jwt or registration
	// token). Sent in the Authorization header.
	authToken string

	// workerID is the calling worker's id. The platform's status
	// endpoint requires it in the request body for ownership checks.
	workerID string

	// credentialProvider supplies fresh worker credentials before post calls.
	// This lets long-running child processes pick up daemon-side runtime-token
	// refreshes instead of posting with the expired JWT they started with.
	credentialProvider CredentialProvider

	// httpClient is the HTTP client used for both calls. Defaults to
	// &http.Client{Timeout: 30s}.
	httpClient *http.Client

	// maxAttempts caps the retry count for transient failures.
	maxAttempts int

	// baseDelay is the base delay for exponential backoff.
	baseDelay time.Duration

	// now returns "now"; injectable for deterministic tests of the
	// backoff sleep.
	now func() time.Time
}

// Options configure a [Poster].
type Options struct {
	// PlatformURL is the base URL of the platform. Required.
	PlatformURL string

	// AuthToken is the worker bearer token. Required for all
	// non-localhost platform deployments.
	AuthToken string

	// WorkerID identifies the calling worker; required because the
	// platform's status endpoint validates it against the session's
	// owner before accepting transitions.
	WorkerID string

	// CredentialProvider returns the latest worker id + auth token. Empty
	// fields fall back to WorkerID/AuthToken.
	CredentialProvider CredentialProvider

	// HTTPClient overrides the default 30s-timeout client. Optional.
	HTTPClient *http.Client

	// MaxAttempts overrides DefaultMaxAttempts. Values < 1 fall back to
	// DefaultMaxAttempts.
	MaxAttempts int

	// BaseDelay overrides DefaultBaseDelay. Values < 0 fall back to
	// DefaultBaseDelay; a zero value disables sleep between retries
	// (handy in tests).
	BaseDelay time.Duration

	// Now overrides time.Now for deterministic tests. Optional.
	Now func() time.Time
}

// RuntimeCredentials are the bearer-token credentials needed for platform
// result-post requests. Empty fields fall back to the Poster defaults.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials available
// to the caller. Implementations should be cheap and concurrency-safe.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// NewPoster constructs a Poster from opts. Returns an error when the
// required PlatformURL is missing or unparseable. Optional fields fall
// through to their defaults.
func NewPoster(opts Options) (*Poster, error) {
	if strings.TrimSpace(opts.PlatformURL) == "" {
		return nil, errors.New("result: PlatformURL is required")
	}
	u, err := url.Parse(opts.PlatformURL)
	if err != nil {
		return nil, fmt.Errorf("result: parse PlatformURL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("result: PlatformURL %q must include scheme and host", opts.PlatformURL)
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxAttempts
	}
	delay := opts.BaseDelay
	if delay < 0 {
		delay = DefaultBaseDelay
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Poster{
		platformURL:        u,
		authToken:          opts.AuthToken,
		workerID:           opts.WorkerID,
		credentialProvider: opts.CredentialProvider,
		httpClient:         hc,
		maxAttempts:        maxAttempts,
		baseDelay:          delay,
		now:                now,
	}, nil
}

// completionRequest is the wire body for POST /api/sessions/<id>/completion.
// Field set matches the platform handler at
// platform/src/app/api/sessions/[id]/completion/route.ts (Phase 2a port).
type completionRequest struct {
	WorkerID string `json:"workerId,omitempty"`
	Summary  string `json:"summary"`
}

// statusRequest is the wire body for POST /api/sessions/<id>/status.
// Field set matches the platform handler at
// platform/src/app/api/sessions/[id]/status/route.ts (StatusRequestBody).
type statusRequest struct {
	WorkerID          string         `json:"workerId"`
	Status            string         `json:"status"`
	ProviderSessionID string         `json:"providerSessionId,omitempty"`
	WorktreePath      string         `json:"worktreePath,omitempty"`
	Error             *errorEnvelope `json:"error,omitempty"`
	TotalCostUsd      float64        `json:"totalCostUsd,omitempty"`
	InputTokens       int64          `json:"inputTokens,omitempty"`
	OutputTokens      int64          `json:"outputTokens,omitempty"`
}

// errorEnvelope mirrors the shape the platform expects under
// `body.error`: `{ message: string }`. Other fields are ignored by the
// status handler and intentionally omitted.
type errorEnvelope struct {
	Message string `json:"message"`
}

// Post sends the runner's terminal [agent.Result] to the platform.
//
// Order matters: completion first, then status. The completion endpoint
// posts the human-readable Linear comment; the status endpoint
// transitions the FSM and triggers the cleanup chain (release claim,
// archive inbox, release issue lock, promote next pending work). Both
// are wrapped by the retry helper; a permanent (4xx) failure on
// completion does NOT block the status post — the runner still wants to
// release the session lock so the next worker can pick up.
//
// Errors:
//
//   - returns nil when both calls succeed.
//   - returns a wrapped [PermanentError] when a 4xx response is seen on
//     either call.
//   - returns a wrapped [TransientError] when retries are exhausted on
//     a transient failure (5xx, network, timeout). Caller should log
//     and treat as a soft failure.
//   - returns ctx.Err() when the context is cancelled.
//
// When both calls return errors, [errors.Join] combines them so
// downstream logs see the full picture.
func (p *Poster) Post(ctx context.Context, sessionID string, r agent.Result) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("result: sessionID is required")
	}
	if r.Status == "" {
		return errors.New("result: agent.Result.Status is required")
	}

	completionErr := p.postCompletion(ctx, sessionID, r)
	statusErr := p.postStatus(ctx, sessionID, r)

	switch {
	case completionErr == nil && statusErr == nil:
		return nil
	case completionErr != nil && statusErr != nil:
		return errors.Join(
			fmt.Errorf("completion: %w", completionErr),
			fmt.Errorf("status: %w", statusErr),
		)
	case completionErr != nil:
		return fmt.Errorf("completion: %w", completionErr)
	default:
		return fmt.Errorf("status: %w", statusErr)
	}
}

func (p *Poster) credentials(ctx context.Context) RuntimeCredentials {
	creds := RuntimeCredentials{
		WorkerID:  p.workerID,
		AuthToken: p.authToken,
	}
	if p.credentialProvider == nil {
		return creds
	}
	fresh, err := p.credentialProvider(ctx)
	if err != nil {
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

func (p *Poster) postCompletion(ctx context.Context, sessionID string, r agent.Result) error {
	summary := strings.TrimSpace(r.Summary)
	if summary == "" {
		// Synthesise a minimal summary so the platform-side Linear
		// comment is never empty (the handler 400s on missing summary).
		summary = synthSummary(r)
	}
	creds := p.credentials(ctx)
	body := completionRequest{
		WorkerID: creds.WorkerID,
		Summary:  summary,
	}
	path := fmt.Sprintf("/api/sessions/%s/completion", sessionID)
	return p.doRetried(ctx, path, body, creds.AuthToken)
}

func (p *Poster) postStatus(ctx context.Context, sessionID string, r agent.Result) error {
	creds := p.credentials(ctx)
	body := statusRequest{
		WorkerID:          creds.WorkerID,
		Status:            r.Status,
		ProviderSessionID: r.ProviderSessionID,
		WorktreePath:      r.WorktreePath,
	}
	if r.Cost != nil {
		body.TotalCostUsd = r.Cost.TotalCostUsd
		body.InputTokens = r.Cost.InputTokens
		body.OutputTokens = r.Cost.OutputTokens
	}
	if r.Error != "" {
		body.Error = &errorEnvelope{Message: r.Error}
	}
	path := fmt.Sprintf("/api/sessions/%s/status", sessionID)
	return p.doRetried(ctx, path, body, creds.AuthToken)
}

// doRetried executes a POST against path with body, retrying transient
// failures up to p.maxAttempts. Permanent (4xx) responses short-circuit
// the loop so we don't waste time pretending a misconfigured request
// will succeed on the next try.
func (p *Poster) doRetried(ctx context.Context, path string, body any, authToken string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	endpoint := p.urlFor(path)

	var lastErr error
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		err := p.doOnce(ctx, endpoint, payload, authToken)
		if err == nil {
			return nil
		}
		lastErr = err

		var perm *PermanentError
		if errors.As(err, &perm) {
			return err
		}
		// Caller-cancelled context aborts immediately. We deliberately
		// do NOT short-circuit on http.Client.Timeout (which surfaces
		// as context.DeadlineExceeded too) — that's a per-attempt
		// timeout we DO want to retry. Distinguish via ctx.Err().
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if attempt < p.maxAttempts {
			if waitErr := p.sleep(ctx, attempt); waitErr != nil {
				return waitErr
			}
		}
	}
	return &TransientError{
		Attempts: p.maxAttempts,
		Last:     lastErr,
	}
}

// doOnce performs a single POST against endpoint with payload. Returns
// a [PermanentError] for 4xx responses and a transient error for 5xx /
// network failures.
func (p *Poster) doOnce(ctx context.Context, endpoint string, payload []byte, authToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		// Network-level failure (DNS, connection refused, timeout) —
		// treated as transient.
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Read up to 4 KiB of the response body for the error message.
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyText := strings.TrimSpace(string(bodyBytes))

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &PermanentError{
			StatusCode: resp.StatusCode,
			Body:       bodyText,
		}
	}
	return fmt.Errorf("http %d: %s", resp.StatusCode, bodyText)
}

// sleep waits the backoff duration before attempt n+1 (1-indexed). It
// honours context cancellation so a cancelled run aborts immediately.
func (p *Poster) sleep(ctx context.Context, attempt int) error {
	if p.baseDelay == 0 {
		return nil
	}
	d := p.baseDelay << (attempt - 1) // 1s, 2s, 4s, ...
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// urlFor joins p.platformURL with path. Reuses the parsed URL so we
// never hit the http.NewRequest URL-parser path twice.
func (p *Poster) urlFor(path string) string {
	cp := *p.platformURL
	cp.Path = strings.TrimRight(cp.Path, "/") + path
	return cp.String()
}

// synthSummary builds a minimal completion-comment summary from a
// Result when the runner did not provide one. Keeps the platform's
// Linear-comment chain non-empty.
func synthSummary(r agent.Result) string {
	switch r.Status {
	case "completed":
		if r.PullRequestURL != "" {
			return fmt.Sprintf("Session completed. PR: %s", r.PullRequestURL)
		}
		return "Session completed."
	case "failed":
		if r.Error != "" {
			return fmt.Sprintf("Session failed: %s", r.Error)
		}
		return "Session failed."
	case "stopped":
		return "Session stopped."
	default:
		return fmt.Sprintf("Session ended with status %q.", r.Status)
	}
}
