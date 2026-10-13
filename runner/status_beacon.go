package runner

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/runtime/statusbeacon"
)

// Status beacon steps are the runner's current-step vocabulary posted to
// the session progress endpoint. They mirror the state.json CurrentStep
// phases so the platform descriptor and the local record agree; terminal
// steps reuse the agent.Result Status words (completed/failed/stopped).
const (
	StatusBeaconSpawning  = "spawning"
	StatusBeaconStreaming = "streaming"
	StatusBeaconSteering  = "steering"
	StatusBeaconBackstop  = "backstop"
	StatusBeaconTeardown  = "teardown"
)

// terminalBeaconStep maps the run's terminal Result onto its beacon step:
// the Result Status word when set (completed/failed/stopped), "failed"
// when the result carries a failure the status has not caught up with yet.
func terminalBeaconStep(res *Result) string {
	if res == nil {
		return StatusBeaconTeardown
	}
	switch res.Status {
	case "completed", "failed", "stopped":
		return res.Status
	}
	if res.FailureMode != "" || res.Error != "" {
		return "failed"
	}
	return StatusBeaconTeardown
}

// statusBeaconReporter is the runner's per-session current-step beacon:
// it posts the orchestration phase (spawning, streaming, steering,
// backstop, teardown, completed, failed, stopped) to the session progress
// endpoint so operator surfaces render the agent's own words instead of a
// derived clause.
//
// Cadence: the beacon fires on step CHANGE, bounded to at most one POST
// per interval (the latest step wins the next slot). The bound keeps a
// step-change burst — spawn, streaming, steering, backstop inside one fast
// session — from becoming one POST per transition. A Beacon that fails to
// construct (missing session id or platform URL) is nil-safe: Report is a
// no-op and the run proceeds exactly as before.
//
// Failure posture: every POST failure is logged and swallowed inside
// runtime/statusbeacon — a beacon outage must NEVER fail the run, block a
// step transition, or kill the session. Construction failure is likewise
// a warn-and-continue: the run is unaffected.
//
// Terminal beacon: the runLoop defers Close, which posts the final step
// (completed, failed, or stopped) even when the interval gate would
// otherwise swallow it — the descriptor must not keep showing a mid-run
// phase after the session ended.
type statusBeaconReporter struct {
	beacon   *statusbeacon.Beacon
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	pending  string // latest reported step not yet posted
	lastPost time.Time
	timer    *time.Timer
	closed   bool
}

// newStatusBeaconReporter builds the per-session reporter. interval <= 0
// falls back to DefaultStatusBeaconInterval; a NEGATIVE interval disables
// beacon posts entirely (returns nil — Report/Close are no-ops on nil).
// A nil now falls back to time.Now. Returns nil when the beacon cannot be
// constructed — the caller logs and continues without beacons.
//
// httpClient and logger ride alongside the Runner so tests can point the
// beacon at an httptest double; production passes the runner's own.
func newStatusBeaconReporter(qw QueuedWork, interval time.Duration, now func() time.Time, r *Runner) *statusBeaconReporter {
	var client *http.Client
	var logger *slog.Logger
	if r != nil {
		client = r.httpClient
		logger = r.logger
	}
	return newStatusBeaconReporterWith(qw, interval, now, r, client, logger)
}

// newStatusBeaconReporterWith is newStatusBeaconReporter with explicit
// transport seams, so tests can point the beacon at an httptest double
// without a Runner.
func newStatusBeaconReporterWith(qw QueuedWork, interval time.Duration, now func() time.Time, r *Runner, client *http.Client, logger *slog.Logger) *statusBeaconReporter {
	if interval < 0 {
		return nil
	}
	if interval == 0 {
		interval = DefaultStatusBeaconInterval
	}
	if now == nil {
		now = time.Now
	}
	var credProvider statusbeacon.CredentialProvider
	if r != nil && r.credentialProvider != nil {
		credProvider = func(ctx context.Context) (statusbeacon.RuntimeCredentials, error) {
			creds, err := r.credentialProvider(ctx)
			return statusbeacon.RuntimeCredentials{
				WorkerID:  creds.WorkerID,
				AuthToken: creds.AuthToken,
			}, err
		}
	}
	beacon, err := statusbeacon.New(statusbeacon.Config{
		SessionID:          qw.SessionID,
		WorkerID:           qw.WorkerID,
		BaseURL:            qw.PlatformURL,
		AuthToken:          qw.AuthToken,
		CredentialProvider: credProvider,
		HTTPClient:         client,
		Logger:             logger,
		Now:                now,
	})
	if err != nil {
		return nil
	}
	return &statusBeaconReporter{
		beacon:   beacon,
		interval: interval,
		now:      now,
	}
}

// Report records a step change. When the interval has elapsed since the
// last POST it posts synchronously on the caller's goroutine; otherwise it
// parks the step as pending and arms a timer for the remainder, coalescing
// further changes into the latest step. A repeat of the already-posted
// step is a no-op. Nil-safe: a nil reporter drops the step.
func (s *statusBeaconReporter) Report(ctx context.Context, step string) {
	if s == nil {
		return
	}
	normalized, ok := statusbeacon.NormalizeStep(step)
	if !ok {
		return
	}
	s.mu.Lock()
	if normalized == s.lastPostedLocked() {
		s.mu.Unlock()
		return
	}
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.now().Sub(s.lastPost) >= s.interval {
		s.mu.Unlock()
		s.post(ctx, normalized)
		return
	}
	s.pending = normalized
	if s.timer == nil {
		wait := s.interval - s.now().Sub(s.lastPost)
		if wait < 0 {
			wait = 0
		}
		s.timer = time.AfterFunc(wait, func() {
			s.mu.Lock()
			step := s.pending
			s.pending = ""
			s.timer = nil
			s.mu.Unlock()
			if step != "" {
				s.post(context.Background(), step)
			}
		})
	}
	s.mu.Unlock()
}

// Close posts the terminal step unconditionally (bypassing the interval
// gate so the descriptor never keeps a mid-run phase after the session
// ends), stops the coalescing timer, and drops any pending step it
// supersedes. Nil-safe.
func (s *statusBeaconReporter) Close(ctx context.Context, step string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.pending = ""
	s.closed = true
	s.mu.Unlock()
	normalized, ok := statusbeacon.NormalizeStep(step)
	if !ok {
		return
	}
	if !s.beacon.ShouldPost(normalized) {
		return
	}
	s.post(ctx, normalized)
}

// lastPostedLocked returns the beacon's last posted step. Caller holds mu.
func (s *statusBeaconReporter) lastPostedLocked() string {
	return s.beacon.LastStep()
}

// post performs one beacon POST and stamps lastPost on success. A failure
// leaves lastPost untouched so the next step change retries promptly
// rather than waiting out a full interval after a swallowed error.
func (s *statusBeaconReporter) post(ctx context.Context, step string) {
	if !s.beacon.Report(ctx, step) {
		return
	}
	s.mu.Lock()
	s.lastPost = s.now()
	s.mu.Unlock()
}
