package daemon

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// auto_update_scheduler.go — the opt-in scheduled-update loop.
//
// An unconfigured daemon is inert: the scheduler only exists when the
// operator explicitly opted in (see autoUpdateSchedulerWanted), and every
// tick re-checks that opt-in against the live config before touching the
// network. The yaml watcher adopts AutoUpdate edits live, so a revoked
// opt-in (cleared source, manual schedule, removed signers) stops the
// loop without a restart and the next tick is inert. Each tick performs
// a lightweight manifest pre-check FIRST; only a strictly newer release
// enters Update, so an unchanged release never drains the machine. The
// actual work reuses Daemon.Update — restart preflight, the fail-closed
// signer verification and the swap — with no new lifecycle state: the
// outcomes below reuse the existing
// running/draining/updating states.
//
// State outcomes (existing states only):
//
//	trigger                 result              end state
//	up-to-date manifest     no lifecycle step   running (never drained)
//	pre-check failure       check failed        running (never drained,
//	                                            no Update entered)
//	preflight refused       Update refused      draining (explicit resume required)
//	Update-phase timeout/   Update error        draining (explicit resume required)
//	failure                   after entering Update
//	verification failed     swap refused        draining (explicit resume required)
//	swap applied            restart requested   draining until the supervisor
//	(new signed release)  via exit code 3;      restarts into the new binary;
//	                        scheduler takes     scheduler takes no further
//	                        no further ticks    ticks (applied flag)
//
// A failed or refused attempt never resumes: finish leaves the daemon
// draining, so a failure cannot silently undo a deliberate drain.
//
// The loop is owned by Start/Stop: Start constructs it (opted-in configs
// only) and Stop cancels its context and joins the goroutine, so every
// exit releases scheduler ownership. Overlapping ticks collapse into one
// attempt via the inFlight guard. The per-request HTTP client stays at its
// 60-second bound (see NewUpdater); the attempt context adds one total
// bound covering drain plus downloads.

// autoUpdateNightlyHour is the local hour of the nightly check (per the
// fleet contract: nightly checks at 03:00 local time).
const autoUpdateNightlyHour = 3

// autoUpdateOnReleaseInterval is the manifest poll interval for the
// on-release schedule. The fleet contract triggers on-release off a
// release notification (SaaS/webhook), which the OSS daemon has no
// channel for; an hourly manifest poll is the local approximation with
// the same observable contract: a newer release is picked up without
// operator action, and an unchanged release costs one manifest fetch.
const autoUpdateOnReleaseInterval = time.Hour

// autoUpdateRunBound caps the post-drain half of one scheduled attempt
// (manifest re-check, downloads, verification, swap), mirroring the
// 5-minute budget the manual POST /api/daemon/update handler grants its
// asynchronous run. The total attempt bound is drainTimeout + this.
const autoUpdateRunBound = 5 * time.Minute

// updateClock is the scheduler's time source. Production uses the system
// clock; tests inject a manual clock to fire ticks deterministically
// through production Start/Stop.
type updateClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// systemUpdateClock is the production updateClock.
type systemUpdateClock struct{}

func (systemUpdateClock) Now() time.Time { return time.Now() }

func (systemUpdateClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// autoUpdateSchedulerWanted reports whether the loaded config opts in to
// scheduled checks. All three guards must hold:
//
//   - schedule is nightly or on-release (manual, empty and unknown mean
//     "never schedule" — a defaulted nightly alone is not opt-in);
//   - source names an explicit update origin (empty disables everything,
//     so no default infrastructure is ever dialed);
//   - signers pins at least one trusted release identity (the operator's
//     local consent to swap, retained independently of any platform
//     request; fail-closed verification would refuse every swap without
//     it, so consentless hosts make no calls at all).
func autoUpdateSchedulerWanted(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	switch cfg.AutoUpdate.Schedule {
	case ScheduleNightly, ScheduleOnRelease:
	default:
		return false
	}
	if strings.TrimSpace(cfg.AutoUpdate.Source) == "" {
		return false
	}
	return len(cfg.AutoUpdate.Signers) != 0
}

// nextAutoUpdateDelay returns how long the scheduler waits before its next
// tick: until 03:00 local time for nightly, one poll interval for
// on-release. Pure so the cadence is pinnable without a clock.
func nextAutoUpdateDelay(now time.Time, schedule UpdateSchedule) time.Duration {
	if schedule == ScheduleOnRelease {
		return autoUpdateOnReleaseInterval
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), autoUpdateNightlyHour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

// autoUpdateScheduler owns one cancellable, non-overlapping scheduled
// update loop. Zero value is inert; Start builds it via
// startAutoUpdateScheduler and Stop tears it down via
// stopAutoUpdateScheduler.
type autoUpdateScheduler struct {
	daemon *Daemon
	clock  updateClock
	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}
	done   chan struct{}
	// inFlight collapses overlapping ticks into one attempt: a tick that
	// arrives while an attempt runs logs and returns without work.
	inFlight atomic.Bool
	// applied latches after a successful swap: the restart handoff below
	// owns completion, so further ticks stop instead of re-applying the
	// same release against the old running version.
	applied atomic.Bool
}

// startAutoUpdateScheduler constructs and starts the scheduled-update loop
// when the loaded config opts in; otherwise it leaves the daemon without
// one (nil field = inert, no goroutine, no calls). Idempotent: a second
// call while a loop runs keeps the existing loop (and cancels the spare
// context) instead of leaking the old goroutine.
func (d *Daemon) startAutoUpdateScheduler() {
	if !autoUpdateSchedulerWanted(d.Config()) {
		return
	}
	clock := d.autoUpdateClockOrDefault()
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancel stored on the scheduler and called in stopAutoUpdateScheduler
	s := &autoUpdateScheduler{
		daemon: d,
		clock:  clock,
		ctx:    ctx,
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	s.cancel = cancel
	d.lifecycleMu.Lock()
	// A queued watcher callback that waited behind yamlReloadMu while
	// Stop completed must not install a loop after terminal publication.
	// The check and install share this critical section so a concurrent
	// Stop cannot interleave between them. The context is discarded via
	// cancel when stopping began.
	if d.stopGen != nil || d.autoUpdateScheduler != nil {
		d.lifecycleMu.Unlock()
		cancel()
		return
	}
	d.autoUpdateScheduler = s
	d.lifecycleMu.Unlock()
	go s.run()
}

// stopAutoUpdateScheduler cancels the scheduled-update loop and joins it.
// A never-settling fetch ends with the attempt context (bounded by
// drainTimeout + the run bound), so the join terminates within the attempt
// bound. Nil-safe: a daemon that never started the loop stops nothing.
func (d *Daemon) stopAutoUpdateScheduler() {
	d.stopAutoUpdateSchedulerContext(context.Background())
}

// stopAutoUpdateSchedulerContext cancels the loop and joins it, but the
// join itself is bounded by ctx: shutdown never waits past the caller's
// deadline even when the in-flight pre-check ignores cancellation (for
// example a trust-root read blocked on a FIFO before any HTTP request
// starts). Cancel still bounds the attempt: the check below observes the
// scheduler context and returns, letting run exit and close done.
func (d *Daemon) stopAutoUpdateSchedulerContext(ctx context.Context) {
	d.lifecycleMu.Lock()
	s := d.autoUpdateScheduler
	d.autoUpdateScheduler = nil
	d.lifecycleMu.Unlock()
	if s == nil {
		return
	}
	close(s.quit)
	s.cancel()
	if ctx == nil {
		<-s.done
		return
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

// autoUpdateClockOrDefault returns the injected test clock, or the system
// clock when none was injected.
func (d *Daemon) autoUpdateClockOrDefault() updateClock {
	if d.autoUpdateClock != nil {
		return d.autoUpdateClock
	}
	return systemUpdateClock{}
}

// autoUpdateRunBoundOrDefault returns the injected test run bound, or the
// production 5-minute post-drain budget when none was injected.
func (d *Daemon) autoUpdateRunBoundOrDefault() time.Duration {
	if d.autoUpdateRunBound > 0 {
		return d.autoUpdateRunBound
	}
	return autoUpdateRunBound
}

// run is the blocking loop body: one wait per tick, recomputed from the
// live config each round so a revoked opt-in goes inert without a
// restart. Every exit closes done, releasing scheduler ownership.
func (s *autoUpdateScheduler) run() {
	defer close(s.done)
	for {
		schedule := UpdateSchedule("")
		if cfg := s.daemon.Config(); cfg != nil {
			schedule = cfg.AutoUpdate.Schedule
		}
		timer := s.clock.After(nextAutoUpdateDelay(s.clock.Now(), schedule))
		select {
		case <-s.ctx.Done():
			return
		case <-s.quit:
			return
		case <-timer:
		}
		s.attemptOnce()
	}
}

// attemptOnce performs one scheduled check-and-update round: a lightweight
// manifest pre-check first, then the full Update (preflight + verify +
// swap) only when a strictly newer release exists. Overlapping ticks
// return after the log line below; a revoked opt-in or an already-applied
// swap returns silently.
func (s *autoUpdateScheduler) attemptOnce() {
	if s == nil || s.daemon == nil {
		return
	}
	// A scheduler that survived Stop (a watcher AfterFunc that entered
	// before the watcher closed and waited behind yamlReloadMu) stays
	// inert: no network after stopping began, even if installation raced
	// terminal publication.
	s.daemon.lifecycleMu.Lock()
	stopping := s.daemon.stopGen != nil
	s.daemon.lifecycleMu.Unlock()
	if stopping {
		return
	}
	if s.applied.Load() {
		// Restart already requested for this release; never re-apply the
		// same swap against the old running version.
		return
	}
	if !s.inFlight.CompareAndSwap(false, true) {
		slog.Info("daemon: auto-update tick skipped: previous attempt still running")
		return
	}
	defer s.inFlight.Store(false)
	d := s.daemon
	cfg := d.Config()
	if cfg == nil || !autoUpdateSchedulerWanted(cfg) {
		return
	}
	bound := d.drainTimeout() + d.autoUpdateRunBoundOrDefault()
	ctx, cancel := context.WithTimeout(s.ctx, bound)
	defer cancel()
	manifest, err := d.checkAutoUpdateRelease(ctx, cfg)
	if err != nil {
		slog.Warn("daemon: scheduled auto-update check failed", "error", err)
		return
	}
	if manifest == nil {
		// Up-to-date: no lifecycle transition, the machine never drains.
		return
	}
	// Reuse the pre-checked snapshot so a live reload cannot change the
	// trust root between the pre-check construction and the Update
	// construction. Construction is non-blocking (see
	// checkAutoUpdateRelease), so the attempt deadline only bounds the
	// network half.
	result, err := d.updateWithConfig(ctx, cfg, true)
	if err != nil {
		// Update leaves the daemon draining on every exit path; never
		// resume here — a failed attempt must not silently abandon a
		// deliberately drained host.
		slog.Warn("daemon: scheduled auto-update failed; daemon remains draining", "error", err)
		return
	}
	if result != nil && result.Updated {
		// The swap is on disk; the restart handoff below owns completion.
		// Latch first so a second tick can never re-apply the same
		// release, then ask the supervisor to exec the new binary. The
		// daemon stays draining until that restart lands.
		s.applied.Store(true)
		slog.Info("daemon: scheduled auto-update applied; requesting restart", "version", result.Version)
		d.requestAutoUpdateRestart()
	}
}

// checkAutoUpdateRelease fetches the version manifest through the
// production updater construction (60-second per-request client) and
// reports whether a strictly newer release exists. A nil manifest with a
// nil error means up-to-date. Construction itself (trust-root file read
// plus executable resolution) is non-blocking: loadUpdateTrustRoot
// refuses non-regular files without opening them, so no FIFO open can
// hold the attempt past its bound and no construction goroutine can leak.
// The manifest fetch below already observes ctx (per-request client
// bound plus the attempt deadline), so no waiter goroutine is needed.
func (d *Daemon) checkAutoUpdateRelease(ctx context.Context, cfg *Config) (*VersionManifest, error) {
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: d.EffectiveVersion(),
		Config:         cfg.AutoUpdate,
		CDNBase:        strings.TrimSpace(cfg.AutoUpdate.Source),
		SkipExit:       true,
	})
	return u.CheckForUpdate(ctx)
}
