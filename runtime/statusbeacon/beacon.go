// Package statusbeacon posts the runner's current orchestration step to
// the session progress endpoint as a status beacon.
//
// While a session runs, the runner stamps its phase (spawning, streaming,
// steering, backstop, teardown, completed, failed, stopped) into the
// platform session row so operator surfaces can render the agent's own
// words ("step text, recency") instead of a derived clause. The platform
// companion route (see donmai-architecture, session progress endpoint)
// folds the beaconStep / beaconAt body keys into the session metadata row
// and answers beacon-only posts without streaming to the activity buffer
// or forwarding a permanent comment — a runner stamping every step change
// must not produce one comment per step.
//
// Endpoint contract: POST /api/sessions/<id>/progress. Body:
//
//	{
//	  "workerId": "wkr_xxx",
//	  "beaconStep": "streaming",
//	  "beaconAt": "2026-10-05T17:59:00.000Z"
//	}
//
// Auth: Bearer runtime token (refreshed via [Config.CredentialProvider]
// — same model as [github.com/RenseiAI/donmai/runtime/stepheartbeat]).
//
// ALL-OR-NOTHING: both halves (step + stamp) validate or nothing is
// posted. A step without a stamp has no recency, and a stamp without a
// step has no text — the platform reader drops lone halves the same way,
// so Report validates before sending and skips empty steps silently.
//
// Best-effort, never fatal: every POST failure (network error, 404 from a
// build without the companion route, any non-2xx, auth refusal) is logged
// and swallowed — a beacon outage must NEVER fail the run. Mirrors the
// failure posture of runtime/stepheartbeat (liveness) and
// runtime/activity maybePostRunning (status transition).
package statusbeacon

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

// DefaultHTTPTimeout bounds one POST when the caller does not inject an
// [http.Client]. Beacons are tiny; 5s is generous.
const DefaultHTTPTimeout = 5 * time.Second

// MaxStepLength is the longest step text the beacon carries, matching the
// platform reader's gate (single line, at most 140 chars, not UUID-like,
// non-blank). Longer steps are truncated to fit rather than dropped: the
// phase is structural information the descriptor needs even when a harness
// name makes it long.
const MaxStepLength = 140

// RuntimeCredentials are the bearer-token credentials needed for a
// beacon POST. Empty fields fall back to the corresponding [Config]
// fields. Mirrors runtime/stepheartbeat.RuntimeCredentials so the runner
// can share one provider implementation across every seam.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials
// available to the caller. Implementations should be cheap and
// concurrency-safe; Report invokes it before every POST so daemon-side
// runtime-token refreshes propagate without a restart.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// Config carries the inputs a Beacon needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/progress). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in the
	// request body so the platform can verify ownership.
	WorkerID string
	// BaseURL is the platform API base, e.g. "https://platform.example.com".
	// Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header. Empty means
	// no auth header — used by tests against unauthenticated httptest.Server
	// instances.
	AuthToken string
	// CredentialProvider returns the latest worker id + runtime token. When
	// set, every post calls it before posting so child runners pick up
	// daemon-side runtime-token refreshes mid-session.
	CredentialProvider CredentialProvider

	// HTTPClient overrides the default client (tests inject
	// httptest.Server.Client()).
	HTTPClient *http.Client
	// Logger overrides slog.Default().
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
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
// the heartbeat/activity/step-heartbeat packages so callers can share one
// provider.
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
		c.logger().Warn("status-beacon credential refresh failed; using cached credentials", "err", err)
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

// beaconBody is the request body sent to POST /api/sessions/<id>/progress
// for a beacon-only post. WorkerID attributes the beacon; BeaconStep is
// the runner's current orchestration phase; BeaconAt is the stamp the
// platform renders recency from. No display message: a beacon-only post
// never streams to the activity buffer and never forwards a permanent
// comment.
type beaconBody struct {
	WorkerID   string `json:"workerId"`
	BeaconStep string `json:"beaconStep"`
	BeaconAt   string `json:"beaconAt"`
}

// Beacon posts status beacons for one session. Construct via New, then call
// Report as the step changes and once for the terminal step. A Beacon posts
// synchronously on the caller's goroutine — the runner calls Report from
// its own orchestration points, never from the provider event hot path —
// so there is no Start/Stop lifecycle and no background goroutine. The zero
// value is not usable; all methods are safe for concurrent use.
type Beacon struct {
	cfg Config

	mu sync.Mutex
	// lastStep is the most recently posted step, guarded by mu. A repeat
	// of the same step is a no-op (the platform already has it with a
	// stamp at least as fresh as the caller's cadence would give it) —
	// the runner's own rate gate decides cadence, this is the belt.
	lastStep string
}

// New validates cfg and returns a Beacon. Returns an error when SessionID
// or BaseURL is missing.
func New(cfg Config) (*Beacon, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/statusbeacon: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/statusbeacon: BaseURL required")
	}
	return &Beacon{cfg: cfg}, nil
}

// NormalizeStep folds raw step text into the single-line form the platform
// reader renders: whitespace runs collapse to one space, surrounding space
// is trimmed, and text longer than MaxStepLength is truncated. It reports
// false when the step carries nothing renderable (blank) — the caller
// skips the post rather than stamping an empty descriptor.
func NormalizeStep(step string) (string, bool) {
	single := collapseWhitespace(step)
	if len(single) > MaxStepLength {
		single = truncateStep(single)
	}
	if single == "" {
		return "", false
	}
	return single, true
}

// collapseWhitespace folds every whitespace run to a single space and trims
// the ends, so multi-line harness output renders as one descriptor line.
func collapseWhitespace(s string) string {
	out := make([]byte, 0, len(s))
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			if len(out) > 0 {
				space = true
			}
			continue
		}
		if space {
			out = append(out, ' ')
			space = false
		}
		out = append(out, c)
	}
	return string(out)
}

// truncateStep shortens a normalized step to MaxStepLength without splitting
// a UTF-8 rune, reserving room for the ellipsis marker.
func truncateStep(s string) string {
	if len(s) <= MaxStepLength {
		return s
	}
	cut := MaxStepLength - len("…")
	for cut > 0 && !isUTF8Start(s[cut]) {
		cut--
	}
	if cut <= 0 {
		cut = MaxStepLength - len("…")
	}
	return s[:cut] + "…"
}

// isUTF8Start reports whether b begins a UTF-8 sequence (ASCII or a lead
// byte, never a continuation byte).
func isUTF8Start(b byte) bool { return b < 0x80 || b >= 0xC0 }

// ShouldPost reports whether step differs from the last posted step. The
// runner calls it from its step-change points; equal steps skip the POST.
func (b *Beacon) ShouldPost(step string) bool {
	normalized, ok := NormalizeStep(step)
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return normalized != b.lastStep
}

// Report posts step as the session's current-step beacon. A repeat of the
// already-posted step is a no-op returning true; a blank step is skipped
// silently returning false. Every POST failure is logged and swallowed —
// a beacon outage must NEVER fail the run — and a failed post does NOT
// advance lastStep, so the next step change (or the terminal beacon)
// retries with a fresh stamp.
func (b *Beacon) Report(ctx context.Context, step string) bool {
	normalized, ok := NormalizeStep(step)
	if !ok {
		return false
	}
	b.mu.Lock()
	if normalized == b.lastStep {
		b.mu.Unlock()
		return true
	}
	b.mu.Unlock()

	if !b.post(ctx, normalized) {
		return false
	}
	b.mu.Lock()
	b.lastStep = normalized
	b.mu.Unlock()
	return true
}

// LastStep returns the most recently posted step, or "" when none posted.
func (b *Beacon) LastStep() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastStep
}

// post performs one best-effort beacon POST. A failure (network error, 404
// from a build without the companion route, any non-2xx) is logged and
// swallowed — a beacon outage must NEVER fail the session. Mirrors the
// step-heartbeat beat() failure posture.
func (b *Beacon) post(ctx context.Context, step string) bool {
	creds := b.cfg.credentials(ctx)
	body, err := json.Marshal(beaconBody{
		WorkerID:   creds.WorkerID,
		BeaconStep: step,
		BeaconAt:   b.cfg.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		b.cfg.logger().Debug("status-beacon marshal failed",
			"sessionId", b.cfg.SessionID, "err", err)
		return false
	}
	url := b.cfg.BaseURL + "/api/sessions/" + b.cfg.SessionID + "/progress"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		b.cfg.logger().Debug("status-beacon new request failed",
			"sessionId", b.cfg.SessionID, "err", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	resp, err := b.cfg.client().Do(req)
	if err != nil {
		b.cfg.logger().Debug("status-beacon post failed",
			"sessionId", b.cfg.SessionID, "err", err)
		return false
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// Auth-shaped failure: the credentials this post presented
			// were rejected — almost always a worker runtime token that
			// expired or rotated out from under a still-running child.
			// Still swallowed (a beacon outage must never fail the
			// session), but surfaced at WARN with a distinct message so
			// credential expiry is visible in logs rather than vanishing
			// into the routine Debug line below.
			b.cfg.logger().Warn("status-beacon auth rejected — runtime credentials likely expired or rotated out from under this session",
				"sessionId", b.cfg.SessionID,
				"status", resp.StatusCode)
			return false
		}
		// Best-effort: a 404 (build without the companion route yet) or
		// any other non-2xx is logged and swallowed.
		b.cfg.logger().Debug("status-beacon non-2xx",
			"sessionId", b.cfg.SessionID,
			"status", resp.StatusCode)
		return false
	}
	b.cfg.logger().Debug("status-beacon posted",
		"sessionId", b.cfg.SessionID,
		"step", step)
	return true
}
