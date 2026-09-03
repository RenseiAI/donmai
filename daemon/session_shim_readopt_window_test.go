package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
)

// readoptFixtureOptions composes one re-adoption fixture. Every field is
// optional; the zero value is the fixture the fixed-attempt tests have always
// used.
type readoptFixtureOptions struct {
	// policy is the re-adoption policy the daemon runs under.
	policy SessionShimReadoptionPolicy
	// adoption answers the n-th durable-adoption call (1-based). Nil accepts
	// every one.
	adoption func(ctx context.Context, attempt int) error
	// orphan is the REAL shim's orphan policy. Zero uses a deadline long enough
	// that a test never races the shim's own reaper.
	orphan sessionshim.OrphanPolicy
	// lineageLive, onWindowExhausted, onBindLost and onRebind are the composing
	// seams under test. Nil leaves each unconfigured.
	lineageLive       func(context.Context, sessionshim.Identity) bool
	onWindowExhausted func(context.Context, sessionshim.Identity)
	onBindLost        func(context.Context, sessionshim.Identity)
	onRebind          func(context.Context, sessionshim.Identity) error
	// clock, when set, replaces the daemon's session-shim clock. The window's
	// instants and its waits then both come from it.
	clock *virtualShimClock
}

// newReadoptFixtureWithOptions starts a real shim, adopts it once through the
// ordinary sessionshim.Adopt path, and seeds the daemon's adopted entry with
// the exact evidence a startup adoption would have recorded.
func newReadoptFixtureWithOptions(t *testing.T, opts readoptFixtureOptions) *readoptFixture {
	t.Helper()
	// A Unix socket path has a short platform limit, and t.TempDir() bakes
	// the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "drd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	f := &readoptFixture{registry: registry, dir: dir}
	f.id = sessionshim.Identity{OrgID: "org-readopt", SessionID: "session-readopt"}
	orphan := opts.orphan
	if orphan.Deadline == 0 {
		// Long enough that a test never races the shim's own reaper; the
		// point of re-adoption is that the deadline is never reached.
		orphan = sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second, PropagationMargin: 0}
	}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: f.id, Registry: registry, ProcessEpoch: 5,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
		Orphan:       orphan,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.shim = shim
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	adoption, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "controller-readopt-first",
	})
	if err != nil || len(adoption.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", adoption, err)
	}
	f.controller = adoption.Adopted[0]

	adoptionOutcome := opts.adoption
	if adoptionOutcome == nil {
		adoptionOutcome = func(context.Context, int) error { return nil }
	}
	f.daemon = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     filepath.Join(dir, "registry"),
		CallbackTimeout: 5 * time.Second,
		Orphan:          orphan,
		HostIDForOrg:    func(context.Context, string) (string, error) { return "wh_readopt_host", nil },
		OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			f.mu.Lock()
			f.adoptions++
			attempt := f.adoptions
			f.mu.Unlock()
			if err := adoptionOutcome(ctx, attempt); err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("readopt-" + evidence.Identity.Key())}, nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.batchOutcome != nil {
				if err := f.batchOutcome(batch); err != nil {
					f.refused = append(f.refused, cloneSessionShimAdoptionBatch(batch))
					return SessionShimAdoptionBatchReceipt{}, err
				}
			}
			f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-readopt"), AdoptionRevision: "readopt-revision"}, nil
		},
		Readoption:                   opts.policy,
		LineageLive:                  opts.lineageLive,
		OnReadoptionWindowExhausted:  opts.onWindowExhausted,
		OnSessionShimCarrierBindLost: opts.onBindLost,
		OnSessionShimRebind:          opts.onRebind,
	}})
	t.Cleanup(f.daemon.ReleaseAdoptedSessionShims)
	if opts.clock != nil {
		f.daemon.shims.setSessionShimClock(opts.clock.Now, opts.clock.After)
	}
	evidence, err := f.daemon.sessionShimAdoptionEvidence(context.Background(), f.controller, SessionShimAdoptionPreparationResult{}, "wh_readopt_host")
	if err != nil {
		t.Fatalf("adoption evidence: %v", err)
	}
	evidence.SnapshotProxy = nil
	f.daemon.shims.mu.Lock()
	f.daemon.shims.registry = registry
	f.daemon.shims.adopted[f.id] = adoptedShim{
		controller: f.controller, shimID: f.controller.Hello().ShimID,
		adoption: evidence, adoptionReceipt: SessionShimAdoptionReceipt{DurableCorrelation: []byte("first")},
		// Mirrors what every real adoption path seeds; the production seeding
		// itself is pinned by TestStartupAdoptionSeedsTheBindObservable, which
		// drives adoptSessionShims over a live shim rather than reading this
		// literal back.
		carrierBound:           true,
		carrierBoundAtUnixNano: f.daemon.shimNow().UnixNano(),
	}
	f.daemon.shims.mu.Unlock()
	return f
}

// virtualShimClock is a deterministic clock for the re-adoption window.
//
// A wait does not block: it advances the clock by exactly its own duration and
// fires. The window therefore spends simulated time only on the waits it
// really asks for, which is what makes "a ten-minute window under a ninety
// second orphan deadline" a test that finishes in milliseconds instead of an
// arithmetic assertion pretending to be one.
type virtualShimClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newVirtualShimClock() *virtualShimClock {
	return &virtualShimClock{now: time.Unix(1_800_000_000, 0)}
}

// Now reports the simulated instant.
func (c *virtualShimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After records the wait, advances the clock by it, and fires immediately.
func (c *virtualShimClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	fired := make(chan time.Time, 1)
	fired <- now
	return fired
}

// requestedWaits is every wait the clock was asked for, in order.
func (c *virtualShimClock) requestedWaits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// elapsed is how much simulated time the waits have consumed.
func (c *virtualShimClock) elapsed(from time.Time) time.Duration {
	return c.Now().Sub(from)
}

// waitForOrphanArmed blocks until the shim has published the armed orphan
// deadline that losing its controller starts.
//
// The shim arms that clock from its own serve-loop teardown, so a daemon that
// dials back fast enough can present a keepalive before there is a deadline to
// extend — the shim answers phase_unknown and the exchange extends nothing.
// That is harmless in production (the next tick lands well inside a deadline
// measured in tens of seconds) but it is a coin flip in a test whose whole
// window finishes in milliseconds of real time, so the tests that assert ON the
// extension wait for the state they are asserting about.
func (f *readoptFixture) waitForOrphanArmed(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if record, err := f.registry.Get(f.id); err == nil && record.OrphanDeadlineUnixNano != 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the shim did not publish an armed orphan deadline within %s", within)
}

// refusingCarrier is a durable-adoption outcome that refuses every attempt.
func refusingCarrier() func(context.Context, int) error {
	return func(context.Context, int) error { return errors.New("injected carrier refusal") }
}

// TestLineageLiveWindowReadoptsACarrierThatReturnsAfterTheFixedCutoff is the
// headline pin: a carrier that refuses well past the three attempts
// fixed-attempt mode would have spent, and then returns, gets its lineage back
// with no terminal evidence anywhere.
//
// The RED this replaces is the whole reason the mode exists — under the fixed
// bound the fourth refusal ended the pass, the lineage was quarantined, and
// the shim reaped a healthy harness for a fault nobody at either end had.
func TestLineageLiveWindowReadoptsACarrierThatReturnsAfterTheFixedCutoff(t *testing.T) {
	t.Parallel()
	const returnsOnAttempt = 6
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: 2 * time.Millisecond, Window: 30 * time.Second,
		},
		adoption: func(_ context.Context, attempt int) error {
			if attempt < returnsOnAttempt {
				return errors.New("injected carrier refusal")
			}
			return nil
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) after the carrier returned on attempt %d",
			got, readoptionSucceeded, returnsOnAttempt)
	}
	adoptions, batches := f.snapshot()
	if adoptions != returnsOnAttempt {
		t.Fatalf("durable adoption attempted %d times, want %d — the window stopped at the fixed-attempt cutoff",
			adoptions, returnsOnAttempt)
	}
	if len(batches) != 1 || len(batches[0].Adopted) != 1 || len(batches[0].Quarantined) != 0 {
		t.Fatalf("published batches = %+v, want exactly one carrying the lineage adopted", batches)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v, want none", projected)
	}
	// Zero terminal evidence: the shim never reached its own deadline and left
	// nothing behind that a fence could read as proof of death.
	if _, err := f.registry.GetTombstoneIncarnation(f.id, lost.shimID, lost.controller.Hello().ProcessEpoch); err == nil {
		t.Fatal("a terminal tombstone exists for a lineage that was re-adopted alive")
	}
	if phase := f.recordPhase(t); phase == "exited" {
		t.Fatalf("discovery record phase = %q after a successful re-adoption", phase)
	}
}

// TestLineageLiveBackoffNeverExceedsTheCap pins the ceiling. Doubling without
// one turns a ten-minute window into three attempts and a nine-minute sleep.
func TestLineageLiveBackoffNeverExceedsTheCap(t *testing.T) {
	t.Parallel()
	const (
		cap0              = 4 * time.Second
		keepaliveInterval = 100 * time.Second
	)
	clock := newVirtualShimClock()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Second, BackoffCap: cap0,
			Window: time.Minute, KeepaliveInterval: keepaliveInterval,
		},
		adoption: refusingCarrier(),
		clock:    clock,
	})
	lost := f.lostEntry(t)

	f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost)

	var backoffs []time.Duration
	for _, wait := range clock.requestedWaits() {
		// The keepalive paces on the real clock, so nothing it does reaches
		// here; the interval is set well above the window purely so a future
		// change that routed it through this seam would be visible rather than
		// silently inflating the schedule.
		if wait == keepaliveInterval {
			continue
		}
		backoffs = append(backoffs, wait)
	}
	if len(backoffs) < 4 {
		t.Fatalf("backoff schedule = %v, want enough attempts to reach the cap", backoffs)
	}
	sawCap := false
	for i, backoff := range backoffs {
		if backoff > cap0 {
			t.Fatalf("backoff %d of the schedule %v is %s, above the %s cap", i, backoffs, backoff, cap0)
		}
		if backoff == cap0 {
			sawCap = true
		}
	}
	if !sawCap {
		t.Fatalf("backoff schedule %v never reached the %s cap; the doubling stopped short of it", backoffs, cap0)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, cap0, cap0}; !sameDurations(backoffs[:4], want) {
		t.Fatalf("first four backoffs = %v, want %v", backoffs[:4], want)
	}
}

func sameDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLineageLiveStopsWhenTheLineageIsNoLongerHeld pins the second half of the
// observed-liveness bound. The shim is alive and the record is live; what
// changed is that the composing layer no longer holds the lineage. Retrying
// past that point would re-adopt something nobody upstream wants and would keep
// extending the orphan clock of a harness that should be reaped.
func TestLineageLiveStopsWhenTheLineageIsNoLongerHeld(t *testing.T) {
	t.Parallel()
	// The release is scripted on the ATTEMPT, not on a LineageLive call count:
	// the keepalive consults the same predicate on its own tick, so a
	// call-count script would be measuring the two schedules against each other
	// rather than the loop's reaction.
	const releaseAfterAttempt = 2
	var held struct {
		sync.Mutex
		released bool
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 30 * time.Second,
		},
		adoption: func(_ context.Context, attempt int) error {
			if attempt >= releaseAfterAttempt {
				held.Lock()
				held.released = true
				held.Unlock()
			}
			return errors.New("injected carrier refusal")
		},
		lineageLive: func(context.Context, sessionshim.Identity) bool {
			held.Lock()
			defer held.Unlock()
			return !held.released
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionLineageGone {
		t.Fatalf("disposition = %d, want readoptionLineageGone (%d) once the composing layer let the lineage go",
			got, readoptionLineageGone)
	}
	adoptions, _ := f.snapshot()
	if adoptions != releaseAfterAttempt {
		t.Fatalf("durable adoption attempted %d times, want the %d attempts before the lineage was released",
			adoptions, releaseAfterAttempt)
	}
}

// TestLineageLiveKeepaliveOutlivesAnOrphanDeadlineShorterThanTheBackoff is the
// keepalive's reason for existing, in the one shape that cannot pass without
// it: an orphan deadline of two seconds and a backoff of three. Between two
// attempts the shim's own clock fires, it reaps its harness, and the window
// ends on a lineage that is gone — the exhaustion outcome unreachable because
// the incarnation already left.
//
// With the keepalive running, the lineage survives the gap and the window ends
// the way the amendment says it must: exhausted, with the shim still
// observable.
//
// It does not run in parallel. Every other assertion here is about a value;
// this one is about a real shim's real timer, and sharing a loaded machine with
// a dozen other shims is how a timing pin becomes a coin flip.
func TestLineageLiveKeepaliveOutlivesAnOrphanDeadlineShorterThanTheBackoff(t *testing.T) {
	const orphanDeadline = 2 * time.Second
	var exhausted struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: 3 * time.Second,
			BackoffCap: 3 * time.Second, Window: 3500 * time.Millisecond,
			KeepaliveInterval: 250 * time.Millisecond,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		adoption: refusingCarrier(),
		onWindowExhausted: func(context.Context, sessionshim.Identity) {
			exhausted.Lock()
			exhausted.calls++
			exhausted.Unlock()
		},
	})
	lost := f.lostEntry(t)
	f.waitForOrphanArmed(t, 5*time.Second)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionWindowExhausted {
		t.Fatalf("disposition = %d, want readoptionWindowExhausted (%d) — the shim did not survive the window",
			got, readoptionWindowExhausted)
	}
	exhausted.Lock()
	calls := exhausted.calls
	exhausted.Unlock()
	if calls != 1 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", calls)
	}
	keepalive := f.daemon.sessionShimKeepaliveObservations(f.id)
	if keepalive.extensions == 0 {
		t.Fatal("the daemon extended the shim's orphan clock zero times across the whole window")
	}
	if keepalive.lastDeadlineUnixNano == 0 {
		t.Fatal("no re-armed deadline was ever observed from the shim")
	}
	// And the same fact is visible from outside the process, which is the only
	// way an operator learns that a mixed-version host is degrading.
	if binding := bindingFor(t, f.daemon, f.id); binding.KeepaliveExtensions != keepalive.extensions ||
		binding.LastOrphanDeadline.IsZero() {
		t.Fatalf("bind projection = %+v, want it to carry the window's %d keepalive extensions",
			binding, keepalive.extensions)
	}
	if _, err := f.registry.GetTombstoneIncarnation(f.id, lost.shimID, lost.controller.Hello().ProcessEpoch); err == nil {
		t.Fatal("the shim left a terminal tombstone inside a window the daemon was holding open")
	}
}

// TestLineageLiveWindowReachesTenMinutesUnderANinetySecondOrphanDeadline pins
// the amendment's headline number against the deadline it is allowed to exceed.
//
// The window is ten minutes, the resolved orphan deadline ninety seconds, and
// the keepalive interval is derived from the deadline rather than chosen — so
// a deployment that tightens its deadline and forgets this policy still gets a
// keepalive below it.
func TestLineageLiveWindowReachesTenMinutesUnderANinetySecondOrphanDeadline(t *testing.T) {
	t.Parallel()
	const orphanDeadline = 90 * time.Second
	clock := newVirtualShimClock()
	var exhausted struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: DefaultLineageLiveSessionShimReadoptionPolicy(),
		// The daemon's own resolved deadline; the real shim keeps a long one so
		// this test measures the window, not the reaper.
		orphan:   sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second},
		adoption: refusingCarrier(),
		clock:    clock,
		onWindowExhausted: func(context.Context, sessionshim.Identity) {
			exhausted.Lock()
			exhausted.calls++
			exhausted.Unlock()
		},
	})
	// The keepalive interval is a pure function of the resolved deadline.
	derived := SessionShimConfig{Orphan: sessionshim.OrphanPolicy{Deadline: orphanDeadline}}.readoptionKeepaliveInterval()
	if derived >= orphanDeadline {
		t.Fatalf("derived keepalive interval %s is not below the %s orphan deadline it must keep re-arming",
			derived, orphanDeadline)
	}
	started := clock.Now()
	lost := f.lostEntry(t)
	f.waitForOrphanArmed(t, 5*time.Second)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionWindowExhausted {
		t.Fatalf("disposition = %d, want readoptionWindowExhausted (%d)", got, readoptionWindowExhausted)
	}
	if elapsed := clock.elapsed(started); elapsed < defaultSessionShimReadoptionWindow-defaultSessionShimReadoptionBackoffCap {
		t.Fatalf("the window spent %s of simulated time, want it to reach the %s it is configured for",
			elapsed, defaultSessionShimReadoptionWindow)
	}
	adoptions, _ := f.snapshot()
	if adoptions <= defaultSessionShimReadoptionAttempts {
		t.Fatalf("durable adoption attempted %d times over a ten-minute window, want more than the %d a fixed-attempt policy would spend",
			adoptions, defaultSessionShimReadoptionAttempts)
	}
	exhausted.Lock()
	calls := exhausted.calls
	exhausted.Unlock()
	if calls != 1 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", calls)
	}
	if keepalive := f.daemon.sessionShimKeepaliveObservations(f.id); keepalive.extensions == 0 {
		t.Fatal("the window ran without ever extending the shim's orphan clock")
	}
}

// TestExhaustedWindowQuarantinesUnderItsOwnReason pins the post-window outcome
// end to end, through the disconnect path an operator actually sees. Both
// dispositions reach socket_unreachable — the closed reason registry is not
// this change's to extend — and the reason carried alongside it is what makes
// "the carrier never came back" a different fact from "the shim died".
func TestExhaustedWindowQuarantinesUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		policy     SessionShimReadoptionPolicy
		wantDetail string
	}{
		{
			name: "an exhausted lineage-live window carries its own reason",
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
				BackoffCap: time.Millisecond, Window: 50 * time.Millisecond,
			},
			wantDetail: sessionShimReadoptionWindowExhaustedDetail,
		},
		{
			name:       "a spent fixed-attempt policy keeps the dead-shim reason",
			policy:     SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
			wantDetail: sessionShimControllerLostDetail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: tc.policy, adoption: refusingCarrier(),
			})

			f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

			projected := f.daemon.QuarantinedSessions()
			if len(projected) != 1 {
				t.Fatalf("quarantine projection = %+v, want exactly the lineage", projected)
			}
			if projected[0].Reason != sessionshim.QuarantineSocketUnreachable {
				t.Fatalf("quarantine reason = %q, want socket_unreachable", projected[0].Reason)
			}
			if projected[0].Detail != tc.wantDetail {
				t.Fatalf("quarantine detail = %q, want %q", projected[0].Detail, tc.wantDetail)
			}
		})
	}
	if sessionShimReadoptionWindowExhaustedDetail == sessionShimControllerLostDetail {
		t.Fatal("the two post-window details are identical; the outcomes are indistinguishable again")
	}
	if !strings.Contains(sessionShimReadoptionWindowExhaustedDetail, "readoption_window_exhausted") {
		t.Fatalf("exhausted detail %q does not name the outcome", sessionShimReadoptionWindowExhaustedDetail)
	}
}

// TestExhaustedWindowNeverStrandsTheLineage pins the one thing an exhausted
// window must never do: leave the lineage adopted.
//
// The keepalive stops when the window does, so a retained entry's shim reaps on
// its own deadline within minutes and writes a tombstone that nothing consumes
// — reconcileQuarantinedTombstones iterates the QUARANTINED set, and the
// consumer goroutine for the controller this path already closed has long since
// returned. The daemon would go on reporting an adopted lineage whose shim is
// gone, holding its capacity forever: a false complete-snapshot and a permanent
// leak. So the withdrawal is unconditional, the notification runs first while
// the lineage is still adopted, and occupancy comes back to zero on its own
// once the shim's terminal proof lands.
func TestExhaustedWindowNeverStrandsTheLineage(t *testing.T) {
	const orphanDeadline = 500 * time.Millisecond
	var notified struct {
		sync.Mutex
		adoptedWhenNotified []sessionshim.Identity
		calls               int
	}
	// The hook must see the lineage still adopted: a composition that wants its
	// own disposition gets it BEFORE the withdrawal, not instead of it.
	var f *readoptFixture
	f = newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 50 * time.Millisecond,
			KeepaliveInterval: orphanDeadline / 4,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		adoption: refusingCarrier(),
		onWindowExhausted: func(context.Context, sessionshim.Identity) {
			notified.Lock()
			notified.calls++
			notified.adoptedWhenNotified = f.daemon.AdoptedSessionShims()
			notified.Unlock()
		},
	})

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	notified.Lock()
	calls, adoptedThen := notified.calls, notified.adoptedWhenNotified
	notified.Unlock()
	if calls != 1 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", calls)
	}
	if len(adoptedThen) != 1 || adoptedThen[0] != f.id {
		t.Fatalf("adopted set when the hook ran = %+v, want the lineage still adopted", adoptedThen)
	}
	if adopted := f.daemon.AdoptedSessionShims(); len(adopted) != 0 {
		t.Fatalf("adopted set after an exhausted window = %+v, want the lineage withdrawn", adopted)
	}
	projected := f.daemon.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Detail != sessionShimReadoptionWindowExhaustedDetail {
		t.Fatalf("quarantine projection = %+v, want the lineage quarantined under the exhausted-window reason", projected)
	}

	// The shim is no longer being extended, so it reaps on its own deadline and
	// the tombstone reconciler — which only ever looks at the quarantined set —
	// gives the capacity back. A retained lineage could never reach this.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f.daemon.SessionShimOccupancy() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("occupancy is still %d after the shim reaped; the lineage's terminal proof was never consumed",
		f.daemon.SessionShimOccupancy())
}

// TestLineageMayNotReenterAWindowBeforeThePreviousDeadline pins the re-entry
// bound against the window that GOVERNED the previous re-adoption. A lineage
// that flaps just outside the fixed-attempt arithmetic would otherwise re-enter
// a fresh ten-minute window indefinitely, each cycle costing an adoption
// revision the receiver has to re-attest.
func TestLineageMayNotReenterAWindowBeforeThePreviousDeadline(t *testing.T) {
	t.Parallel()
	// The carrier accepts, so the second half returns on its first attempt: the
	// pin is which window the re-entry guard measures against, not how long a
	// window runs.
	policy := SessionShimReadoptionPolicy{
		Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
		BackoffCap: time.Millisecond, Window: 10 * time.Minute,
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: policy})
	lost := f.lostEntry(t)
	// Re-adopted 90 seconds ago: outside the 60 s a fixed-attempt policy would
	// measure against, well inside the ten minutes this one does.
	lost.readoptedAtUnixNano = f.daemon.shimNow().Add(-90 * time.Second).UnixNano()

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionRefused {
		t.Fatalf("disposition = %d, want readoptionRefused (%d) inside the governing window", got, readoptionRefused)
	}
	if adoptions, _ := f.snapshot(); adoptions != 0 {
		t.Fatalf("durable adoption attempted %d times inside the previous window, want none", adoptions)
	}

	// Past the governing window, the lineage may enter a new one.
	lost.readoptedAtUnixNano = f.daemon.shimNow().Add(-11 * time.Minute).UnixNano()
	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) after the previous window's deadline had passed",
			got, readoptionSucceeded)
	}
	if adoptions, _ := f.snapshot(); adoptions != 1 {
		t.Fatalf("durable adoption attempted %d times, want the one attempt the returning carrier accepted", adoptions)
	}
}

// TestReadoptionPolicyValidateRejectsContradictoryFields pins the refusal at
// CONFIG LOAD. {Fixed + Window} used to run a windowed loop, report a
// fixed-attempt WorstCaseWindow, and admit a fresh window every sixty seconds —
// one policy, three answers, discovered during the incident it was configured
// to survive.
func TestReadoptionPolicyValidateRejectsContradictoryFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		valid  bool
	}{
		{name: "zero value is fixed-attempt mode", policy: SessionShimReadoptionPolicy{}, valid: true},
		{name: "the exported fixed default", policy: DefaultSessionShimReadoptionPolicy(), valid: true},
		{name: "the exported lineage-live default", policy: DefaultLineageLiveSessionShimReadoptionPolicy(), valid: true},
		{name: "fixed mode with a window", policy: SessionShimReadoptionPolicy{Window: 5 * time.Minute}},
		{name: "fixed mode with a backoff cap", policy: SessionShimReadoptionPolicy{BackoffCap: time.Second}},
		{name: "fixed mode with a keepalive interval", policy: SessionShimReadoptionPolicy{KeepaliveInterval: time.Second}},
		{name: "lineage-live mode with attempts", policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, Attempts: 3}},
		{name: "negative window", policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, Window: -time.Second}},
		{name: "unknown mode", policy: SessionShimReadoptionPolicy{Mode: SessionShimReadoptionMode(9)}},
	} {
		err := tc.policy.Validate()
		if tc.valid {
			if err != nil {
				t.Errorf("%s: Validate = %v, want nil", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrSessionShimReadoptionPolicy) {
			t.Errorf("%s: Validate = %v, want ErrSessionShimReadoptionPolicy", tc.name, err)
		}
	}
}

// TestStartupRefusesAContradictoryReadoptionPolicy is the other half of the
// same pin: the refusal has to happen where a misconfiguration is cheap.
func TestStartupRefusesAContradictoryReadoptionPolicy(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "drv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir: filepath.Join(dir, "registry"),
		Readoption:  SessionShimReadoptionPolicy{Window: 5 * time.Minute},
	}})
	t.Cleanup(d.ReleaseAdoptedSessionShims)

	err = d.adoptSessionShims(context.Background())
	if !errors.Is(err, ErrSessionShimReadoptionPolicy) {
		t.Fatalf("startup adoption = %v, want it refused with ErrSessionShimReadoptionPolicy", err)
	}
}

// TestWorstCaseWindowReportsTheGoverningWindowInBothModes pins the number an
// embedder sizes its orphan deadline against. Answering the fixed-attempt
// arithmetic for a lineage-live policy is how one sized a ninety second
// deadline against sixty seconds and terminalized a live lineage anyway.
func TestWorstCaseWindowReportsTheGoverningWindowInBothModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		want   time.Duration
	}{
		{name: "zero resolves to the fixed default", policy: SessionShimReadoptionPolicy{}, want: 60 * time.Second},
		{name: "the exported fixed default", policy: DefaultSessionShimReadoptionPolicy(), want: 60 * time.Second},
		{
			name:   "one attempt sleeps nothing",
			policy: SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Hour, AttemptTimeout: 7 * time.Second},
			want:   7 * time.Second,
		},
		{
			name:   "the exported lineage-live default answers its window",
			policy: DefaultLineageLiveSessionShimReadoptionPolicy(),
			want:   defaultSessionShimReadoptionWindow,
		},
		{
			name:   "a lineage-live policy with no window answers the default window",
			policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive},
			want:   defaultSessionShimReadoptionWindow,
		},
		{
			name:   "a lineage-live policy answers its own window, not the arithmetic",
			policy: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, Window: 4 * time.Minute, Backoff: time.Second},
			want:   4 * time.Minute,
		},
	} {
		if got := tc.policy.WorstCaseWindow(); got != tc.want {
			t.Errorf("%s: WorstCaseWindow() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestCarrierLossClosesTheLostControllerSoTheShimReapsOnItsDeadline is the
// regression pin for the failure this change had to avoid inventing.
//
// Carrier loss closes the lost controller before anything else happens. That
// close is what arms the shim's orphan clock, and that clock is the ONLY
// producer of the terminal proof §D10's fence requires. Leaving the socket open
// so a re-adoption could reuse it would mean a lineage the daemon has already
// quarantined and withdrawn authority from runs its harness process group
// forever, writes no tombstone, and holds host capacity indefinitely — strictly
// worse than the incident the window exists to fix.
func TestCarrierLossClosesTheLostControllerSoTheShimReapsOnItsDeadline(t *testing.T) {
	t.Parallel()
	// Long enough that the quarantine is still projected when it is read: the
	// shim's reap withdraws the row again through the tombstone reconciler, and
	// a deadline shorter than a loaded quarantine publication would have this
	// test racing its own success.
	const orphanDeadline = 3 * time.Second
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
	})
	hello := f.controller.Hello()

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	if projected := f.daemon.QuarantinedSessions(); len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want the lineage quarantined", projected)
	}
	// The lost controller's socket is gone: nothing can still be asked of the
	// shim through the connection the daemon gave up on.
	if err := f.controller.Heartbeat(0); err == nil {
		t.Fatal("the lost controller's connection is still usable after the quarantine")
	}
	// And because it is gone, the shim armed its own clock and reaped.
	const reapWait = 20 * time.Second
	deadline := time.Now().Add(reapWait)
	for time.Now().Before(deadline) {
		if _, err := f.registry.GetTombstoneIncarnation(f.id, hello.ShimID, hello.ProcessEpoch); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal tombstone %s after a quarantine whose orphan deadline was %s — the harness runs forever",
		reapWait, orphanDeadline)
}

// TestStartupRefusesLineageLiveWithoutALinealPredicateUnderAnExternalThreshold
// is the §D8 inequality pin, and it is a pin about a COMBINATION: neither the
// orphan policy nor the re-adoption policy is wrong on its own.
//
// A lineage-live window's keepalive re-arms the orphan deadline for as long as
// the window runs, so the real time from controller loss to a reaped harness
// becomes `Window + Deadline + grace + margin` — ten minutes past a
// three-minute threshold for the shipped default. The amendment permits that
// only while the daemon "still holds the lineage", which is exactly the claim
// the external releaser would take away, and only LineageLive can answer it. A
// nil predicate means "yes, always": right for a standalone daemon that has
// nothing above it, unsound the moment a threshold is declared.
func TestStartupRefusesLineageLiveWithoutALineagePredicateUnderAnExternalThreshold(t *testing.T) {
	t.Parallel()
	safeOrphan := sessionshim.OrphanPolicy{
		Deadline: 90 * time.Second, TerminationGrace: 5 * time.Second,
		PropagationMargin: 30 * time.Second, ExternalReleaseThreshold: 3 * time.Minute,
	}
	if err := safeOrphan.Validate(); err != nil {
		t.Fatalf("precondition: the orphan policy alone is safe, got %v", err)
	}
	live := func(context.Context, sessionshim.Identity) bool { return true }
	for _, tc := range []struct {
		name       string
		cfg        SessionShimConfig
		wantRefusd bool
	}{
		{
			name: "lineage-live under a declared threshold with no predicate",
			cfg: SessionShimConfig{
				Orphan:     safeOrphan,
				Readoption: DefaultLineageLiveSessionShimReadoptionPolicy(),
			},
			wantRefusd: true,
		},
		{
			name: "the same configuration with the predicate wired",
			cfg: SessionShimConfig{
				Orphan:      safeOrphan,
				Readoption:  DefaultLineageLiveSessionShimReadoptionPolicy(),
				LineageLive: live,
			},
		},
		{
			name: "a standalone daemon declares no threshold, so the nil predicate is sound",
			cfg: SessionShimConfig{
				Orphan:     sessionshim.OrphanPolicy{Deadline: 90 * time.Second, TerminationGrace: 5 * time.Second},
				Readoption: DefaultLineageLiveSessionShimReadoptionPolicy(),
			},
		},
		{
			name: "fixed-attempt mode stays inside the deadline, so it needs no predicate",
			cfg:  SessionShimConfig{Orphan: safeOrphan, Readoption: DefaultSessionShimReadoptionPolicy()},
		},
		{
			name: "re-adoption disabled altogether extends nothing",
			cfg: SessionShimConfig{
				Orphan:     safeOrphan,
				Readoption: SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, Disabled: true},
			},
		},
		{
			name: "a configured keepalive interval that leaves no slack against the deadline",
			cfg: SessionShimConfig{
				Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second},
				Readoption: SessionShimReadoptionPolicy{
					Mode: ReadoptionLineageLive, KeepaliveInterval: 100 * time.Second,
				},
			},
			wantRefusd: true,
		},
		{
			name: "half the deadline is the loosest interval that still leaves a missed exchange of slack",
			cfg: SessionShimConfig{
				Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second},
				Readoption: SessionShimReadoptionPolicy{
					Mode: ReadoptionLineageLive, KeepaliveInterval: 30 * time.Second,
				},
			},
		},
	} {
		err := tc.cfg.validateReadoptionAgainstOrphanPolicy()
		if tc.wantRefusd {
			if !errors.Is(err, ErrSessionShimReadoptionPolicy) {
				t.Errorf("%s: validate = %v, want ErrSessionShimReadoptionPolicy", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: validate = %v, want nil", tc.name, err)
		}
	}

	// And it is refused where a misconfiguration is cheap: at config load.
	dir, err := os.MkdirTemp("/tmp", "drx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir: filepath.Join(dir, "registry"),
		Orphan:      safeOrphan,
		Readoption:  DefaultLineageLiveSessionShimReadoptionPolicy(),
	}})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	if err := d.adoptSessionShims(context.Background()); !errors.Is(err, ErrSessionShimReadoptionPolicy) {
		t.Fatalf("startup adoption = %v, want it refused with ErrSessionShimReadoptionPolicy", err)
	}
}

// TestKeepaliveStopsAsSoonAsTheLineageIsReleased pins the other half of finding
// 1. The attempt loop consults LineageLive once per backoff — up to a whole
// BackoffCap apart — so a keepalive that did not consult it itself would go on
// extending the orphan clock of a lineage the composing layer has already let
// go, for up to thirty seconds. That extension is exactly the one §D8's
// inequality forbids.
func TestKeepaliveStopsAsSoonAsTheLineageIsReleased(t *testing.T) {
	const orphanDeadline = 2 * time.Second
	var held struct {
		sync.Mutex
		released bool
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: 100 * time.Millisecond,
			BackoffCap: 100 * time.Millisecond, Window: 30 * time.Second,
			KeepaliveInterval: 50 * time.Millisecond,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		adoption: refusingCarrier(),
		lineageLive: func(context.Context, sessionshim.Identity) bool {
			held.Lock()
			defer held.Unlock()
			return !held.released
		},
	})
	lost := f.lostEntry(t)
	f.waitForOrphanArmed(t, 5*time.Second)
	registry, err := f.daemon.sessionShimRegistry()
	if err != nil {
		t.Fatal(err)
	}

	stop := f.daemon.startSessionShimOrphanKeepalive(registry, f.daemon.sessionShimConfig(), f.id, lost.controller.Hello())
	defer stop()

	extended := func() int { return f.daemon.sessionShimKeepaliveObservations(f.id).extensions }
	deadline := time.Now().Add(5 * time.Second)
	for extended() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if extended() < 2 {
		t.Fatalf("the keepalive honoured %d extensions while the lineage was held, want it running", extended())
	}

	held.Lock()
	held.released = true
	held.Unlock()
	// Within a few intervals the extension must have stopped for good. One
	// exchange may already be in flight; nothing may follow it.
	time.Sleep(300 * time.Millisecond)
	settled := extended()
	time.Sleep(500 * time.Millisecond)
	if got := extended(); got != settled {
		t.Fatalf("the keepalive honoured %d extensions after the lineage was released (was %d); it kept the shim alive past the release",
			got, settled)
	}
}

// TestStartupAdoptionSeedsTheBindObservable drives REAL startup adoption over a
// real shim, because the window fixture writes its own adopted entry and a test
// that reads back the fixture's literal pins nothing about production.
//
// The seeding is what makes SessionShimAlreadyBound reachable for a healthy
// session: without it the first rebind on a session whose carrier is perfectly
// fine drives a full re-adoption nobody asked for.
func TestStartupAdoptionSeedsTheBindObservable(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "drs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registryDir := filepath.Join(dir, "registry")
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-seed", SessionID: "session-seed"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 9,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
		Orphan:       sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: registryDir, ControllerID: "controller-seed",
		HostID: "wh_seed_host",
	}})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	before := time.Now()
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}

	bindings := d.AdoptedSessionShimBindings()
	if len(bindings) != 1 || bindings[0].Identity != id {
		t.Fatalf("bind projection after startup adoption = %+v, want exactly %s", bindings, id)
	}
	if !bindings[0].CarrierBound {
		t.Fatal("a lineage this daemon just adopted reports CarrierBound=false; AlreadyBound is unreachable for a healthy session")
	}
	if bindings[0].BoundAt.Before(before) {
		t.Fatalf("BoundAt = %s, want an instant from this adoption (after %s)", bindings[0].BoundAt, before)
	}
	if !bindings[0].LastCarrierLossAt.IsZero() {
		t.Fatalf("LastCarrierLossAt = %s on a lineage that never lost its binding", bindings[0].LastCarrierLossAt)
	}
	// The first rebind on it therefore costs nothing.
	if result, err := d.RebindAdoptedSessionShim(context.Background(), id.OrgID, id.SessionID); err != nil ||
		result != SessionShimAlreadyBound {
		t.Fatalf("RebindAdoptedSessionShim on a freshly adopted lineage = %s, %v, want SessionShimAlreadyBound", result, err)
	}
}
