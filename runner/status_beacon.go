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

// beaconPostTimeout bounds one beacon POST independent of the injected
// HTTP client: the runner always injects its own client (30 s default),
// so without this bound a blackholed progress endpoint would hold the
// beacon worker — and, before the async rework, the run loop itself —
// for the full client timeout per post. Five seconds is generous for a
// tiny beacon body and keeps a hung endpoint from stalling teardown.
//
// beaconCloseTimeout bounds Close's wait for the worker to drain the
// terminal post. Best-effort: on expiry Close returns and the run
// proceeds — a beacon outage must never fail the session.
//
// Both are package vars (not consts) so tests can shrink them to pin
// the bounds without waiting out the production 5s/10s values.
var (
	beaconPostTimeout  = 5 * time.Second
	beaconCloseTimeout = 10 * time.Second
)

// beaconQueueSize bounds the worker mailbox. A run posts at most a
// handful of beacons (spawning, streaming, steering, backstop, terminal),
// so 16 never fills in practice; a full queue drops the step rather than
// blocking the caller.
const beaconQueueSize = 16

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
// backstop, plus the terminal completed/failed/stopped) to the session progress
// endpoint so operator surfaces render the agent's own words instead of a
// derived clause. "teardown" is the terminal fallback when the result
// carries no terminal status — it is never reported as a mid-run step.
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
// Async: Report never performs network I/O on the caller's goroutine.
// The caller (run loop, provider event consumer) only decides the cadence
// under a mutex and hands the step to a single worker goroutine through a
// latest-wins mailbox. The worker serializes posts — so a mid-run step can
// never land after the terminal step — and bounds every post with
// beaconPostTimeout, independent of the injected client's Timeout (which
// may be 30 s or unset entirely). Close enqueues the terminal step,
// shuts the worker down, and waits up to beaconCloseTimeout.
//
// Terminal beacon: the runLoop defers Close, which posts the final step
// (completed, failed, or stopped) even when the interval gate would
// otherwise swallow it — the descriptor must not keep showing a mid-run
// phase after the session ends.
type statusBeaconReporter struct {
	beacon   *statusbeacon.Beacon
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	pending  string // latest reported step not yet enqueued
	lastPost time.Time
	timer    *time.Timer
	closed   bool

	queue      chan string
	workerDone chan struct{}
}

// newStatusBeaconReporter builds the per-session reporter. interval == 0
// falls back to DefaultStatusBeaconInterval; a NEGATIVE interval disables
// beacon posts entirely (returns nil — Report/Close are no-ops on nil).
// A nil now falls back to time.Now. Returns nil when beacons stay quiet
// for this run — the local-transport mode whose stub platform URL serves
// no progress route, or a beacon that cannot be constructed — so the
// caller skips beacon traffic without touching the run.
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
	if isLocalRuntimeTransport(qw.runtimeTransport) {
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
	s := &statusBeaconReporter{
		beacon:     beacon,
		interval:   interval,
		now:        now,
		queue:      make(chan string, beaconQueueSize),
		workerDone: make(chan struct{}),
	}
	go s.run()
	return s
}

// run is the single beacon worker: it posts enqueued steps serially,
// each bounded by beaconPostTimeout, until the queue closes. Serial
// execution is the ordering guarantee — the terminal step Close enqueues
// last is posted last, and no timer callback can slip a stale step past
// it once Close has shut the mailbox.
func (s *statusBeaconReporter) run() {
	defer close(s.workerDone)
	for step := range s.queue {
		ctx, cancel := context.WithTimeout(context.Background(), beaconPostTimeout)
		s.beacon.Report(ctx, step)
		cancel()
	}
}

// enqueueLocked hands step to the worker without blocking the caller.
// Caller holds mu; the send never blocks because a full mailbox drops the
// step instead — beacons are best-effort observability, never worth
// stalling the run loop for.
func (s *statusBeaconReporter) enqueueLocked(step string) {
	select {
	case s.queue <- step:
	default:
	}
}

// Report records a step change without blocking on the network. When the
// interval has elapsed since the last enqueue it hands the step to the
// worker immediately (stamping the slot first, so concurrent callers cannot
// both see the gate open); otherwise it parks the step as pending and arms
// a timer for the remainder, coalescing further changes into the latest
// step. A repeat of the already-posted step is a no-op. Nil-safe: a nil
// reporter drops the step. A Report after Close is dropped.
func (s *statusBeaconReporter) Report(_ context.Context, step string) {
	if s == nil {
		return
	}
	normalized, ok := statusbeacon.NormalizeStep(step)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if normalized == s.beacon.LastStep() || normalized == s.pending {
		// Already posted, or already the parked latest step: nothing new.
		// The pending check keeps a repeat of the parked step from
		// re-arming work; the timer already owns it.
		if normalized == s.beacon.LastStep() {
			return
		}
		s.pending = normalized
		return
	}
	if s.now().Sub(s.lastPost) >= s.interval {
		s.lastPost = s.now()
		s.enqueueLocked(normalized)
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
			defer s.mu.Unlock()
			if s.closed {
				s.timer = nil
				s.pending = ""
				return
			}
			step := s.pending
			s.pending = ""
			s.timer = nil
			if step == "" {
				return
			}
			if step == s.beacon.LastStep() {
				return
			}
			s.lastPost = s.now()
			s.enqueueLocked(step)
		})
	}
}

// Close posts the terminal step unconditionally (bypassing the interval
// gate so the descriptor never keeps a mid-run phase after the session
// ends), stops the coalescing timer, drops any pending step it
// supersedes, shuts the worker down, and waits up to beaconCloseTimeout
// for the terminal post to drain. Best-effort: on timeout Close returns
// and the run proceeds. Nil-safe. Idempotent: the second call returns
// without posting.
func (s *statusBeaconReporter) Close(_ context.Context, step string) {
	if s == nil {
		return
	}
	normalized, ok := statusbeacon.NormalizeStep(step)
	if !ok {
		s.shutdown()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.pending = ""
	s.closed = true
	if !s.beacon.ShouldPost(normalized) {
		close(s.queue)
		s.mu.Unlock()
		s.wait()
		return
	}
	s.lastPost = s.now()
	s.enqueueLocked(normalized)
	close(s.queue)
	s.mu.Unlock()
	s.wait()
}

// shutdown closes a reporter that has nothing terminal to post (blank
// step): it stops the timer, marks closed, drains the worker, and
// returns without posting.
func (s *statusBeaconReporter) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.pending = ""
	s.closed = true
	close(s.queue)
	s.mu.Unlock()
	s.wait()
}

// wait blocks up to beaconCloseTimeout for the worker to drain.
func (s *statusBeaconReporter) wait() {
	select {
	case <-s.workerDone:
	case <-time.After(beaconCloseTimeout):
	}
}
