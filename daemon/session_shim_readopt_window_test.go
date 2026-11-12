package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// readoptFixtureOptions composes one re-adoption fixture. Every field is
// optional; the zero value is the fixture the fixed-attempt tests have always
// used.
type readoptFixtureOptions struct {
	// policy is the re-adoption policy the daemon runs under.
	policy  SessionShimReadoptionPolicy
	prepare func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error)
	// prepareV2 is the typed proof-v2 prepare seam. Nil leaves it unconfigured.
	// It is mutually exclusive with prepare: setting both is refused at
	// preparation time, so a test needing the recovery path leaves prepare nil.
	prepareV2 func(context.Context, SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error)
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
	// heldPastWindow is SessionShimConfig.ReadoptionHeldPastWindow. Nil leaves
	// the bounded window, which is what every test that predates it wants.
	heldPastWindow func(context.Context, sessionshim.Identity) bool
	// onTerminalEvidence receives every terminal fact the daemon hands its
	// composer. Nil leaves the seam unconfigured, which is what every test that
	// is not about terminal evidence wants.
	onTerminalEvidence func(context.Context, SessionShimTerminalEvidence) error
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
	// The shell-free echo child: it only has to stay alive behind the
	// adopted connection, answering every input line with an `ack:` echo.
	echoSpec, err := daemonShimEchoSpec()
	if err != nil {
		t.Fatal(err)
	}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: f.id, Registry: registry, ProcessEpoch: 5,
		Spec:         echoSpec,
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
		PrepareAdoption:              opts.prepare,
		PrepareAdoptionV2:            opts.prepareV2,
		OnTerminalEvidence:           opts.onTerminalEvidence,
		Readoption:                   opts.policy,
		LineageLive:                  opts.lineageLive,
		OnReadoptionWindowExhausted:  opts.onWindowExhausted,
		OnSessionShimCarrierBindLost: opts.onBindLost,
		OnSessionShimRebind:          opts.onRebind,
		ReadoptionHeldPastWindow:     opts.heldPastWindow,
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

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
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

// TestLineageLiveBackoffNeverExceedsTheCap pins the ceiling (see
// DefaultSessionShimReadoptionBackoffCap). Doubling without one turns a hold
// past the old window into three attempts and a nine-minute sleep; with the
// cap the hold retries with capped backoff for as long as the shim is
// observable.
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

	// The hold never ends while the shim is observable, so run it in the
	// background and stop it once the schedule is pinned.
	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var backoffs []time.Duration
		for _, wait := range clock.requestedWaits() {
			if wait == keepaliveInterval {
				continue
			}
			backoffs = append(backoffs, wait)
		}
		if len(backoffs) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold did not schedule four backoffs within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.daemon.shims.stopRecovery()
	select {
	case got := <-done:
		if got != readoptionShutdown {
			t.Fatalf("disposition = %d, want readoptionShutdown (%d)", got, readoptionShutdown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not stop within 10s of recovery being cancelled")
	}

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

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionLineageGone {
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
// it: an orphan deadline of five seconds and a backoff of seven. Between two
// attempts the shim's own clock would fire and reap its harness without the
// keepalive. With it running, the lineage survives past the old window while
// staying adopted with no quarantine and no tombstone.
//
// It does not run in parallel. Every other assertion here is about a value;
// this one is about a real shim's real timer, and sharing a loaded machine with
// a dozen other shims is how a timing pin becomes a coin flip.
func TestLineageLiveKeepaliveOutlivesAnOrphanDeadlineShorterThanTheBackoff(t *testing.T) {
	const orphanDeadline = 5 * time.Second
	var exhausted struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		// The backoff exceeds the orphan deadline on purpose: between two
		// attempts the shim's own clock fires unless the keepalive holds it.
		// The margin between the interval and the deadline is generous because
		// CI runs this beside a dozen other real shims under -race, and a
		// timing pin with one keepalive of slack is a coin flip there.
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: 7 * time.Second,
			BackoffCap: 7 * time.Second, Window: 8 * time.Second,
			KeepaliveInterval: 500 * time.Millisecond,
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

	// The hold never ends while the shim is observable, so run it in the
	// background and stop it once it has survived past the old window.
	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	// Past the old 8s window the lineage must stay adopted with no quarantine.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if n, _ := f.snapshot(); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold did not attempt past the old window within 20s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
		t.Fatalf("the lineage left the adopted set while the shim was still observable: %v", err)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v while the shim was still observable, want none", projected)
	}
	exhausted.Lock()
	calls := exhausted.calls
	exhausted.Unlock()
	if calls != 0 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times while the shim was still observable, want none", calls)
	}
	f.daemon.shims.stopRecovery()
	select {
	case got := <-done:
		if got != readoptionShutdown {
			t.Fatalf("disposition = %d, want readoptionShutdown (%d)", got, readoptionShutdown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not stop within 10s of recovery being cancelled")
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
// the hold past the old ten-minute bound: the daemon keeps retrying with the
// keepalive running while the shim stays observable, instead of exhausting at
// the window. The window sizes the initial budget; past it the hold continues.
// The keepalive interval is derived from the deadline rather than chosen.
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

	// The hold never ends while the shim is observable, so run it in the
	// background and stop it once it has survived past the old window.
	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if elapsed := clock.elapsed(started); elapsed >= defaultSessionShimReadoptionWindow-defaultSessionShimReadoptionBackoffCap {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold did not survive past the old ten-minute window within 15s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	adoptions, _ := f.snapshot()
	if adoptions <= defaultSessionShimReadoptionAttempts {
		t.Fatalf("durable adoption attempted %d times past the old ten-minute window, want more than the %d a fixed-attempt policy would spend",
			adoptions, defaultSessionShimReadoptionAttempts)
	}
	// While observable the lineage stays adopted with no quarantine and no
	// exhaustion hook: the window no longer ends it.
	if _, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
		t.Fatalf("the lineage left the adopted set while the shim was still observable: %v", err)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v while the shim was still observable, want none", projected)
	}
	exhausted.Lock()
	calls := exhausted.calls
	exhausted.Unlock()
	if calls != 0 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times while the shim was still observable, want none", calls)
	}
	if keepalive := f.daemon.sessionShimKeepaliveObservations(f.id); keepalive.extensions == 0 {
		t.Fatal("the hold ran without ever extending the shim's orphan clock")
	}
	f.daemon.shims.stopRecovery()
	select {
	case got := <-done:
		if got != readoptionShutdown {
			t.Fatalf("disposition = %d, want readoptionShutdown (%d)", got, readoptionShutdown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not stop within 10s of recovery being cancelled")
	}
}

// TestExhaustedWindowQuarantinesUnderItsOwnReason pins that silence alone
// never quarantines an observable shim: past the old window/budget the lineage
// stays adopted with no quarantine, and the terminal detail (when a withdrawal
// finally happens on positive evidence) names the harness liveness at release.
// Both legacy dispositions still reach socket_unreachable — the closed reason
// registry is not this change's to extend — and the detail carried alongside
// it is what makes "the carrier never came back" a different fact from "the
// shim died", now with an explicit harness-alive suffix.
func TestExhaustedWindowQuarantinesUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
	}{
		{
			name: "a lineage-live window past its old bound holds while observable",
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
				BackoffCap: time.Millisecond, Window: 50 * time.Millisecond,
			},
		},
		{
			name:   "a fixed-attempt budget past its old bound holds while observable",
			policy: SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: tc.policy, adoption: refusingCarrier(),
			})
			lost := f.lostEntry(t)
			done := make(chan sessionShimReadoptionDisposition, 1)
			go func() {
				done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
			}()
			// Past the old 1-attempt/50ms bound the lineage must stay adopted
			// with no quarantine while the shim is observable.
			deadline := time.Now().Add(10 * time.Second)
			for {
				if n, _ := f.snapshot(); n >= 3 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the hold did not retry past the old bound within 10s")
				}
				time.Sleep(2 * time.Millisecond)
			}
			if _, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
				t.Fatalf("the lineage left the adopted set while the shim was still observable: %v", err)
			}
			if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
				t.Fatalf("quarantine projection = %+v while the shim was still observable, want none", projected)
			}
			f.daemon.shims.stopRecovery()
			select {
			case got := <-done:
				if got != readoptionShutdown {
					t.Fatalf("disposition = %d, want readoptionShutdown (%d)", got, readoptionShutdown)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hold did not stop within 10s of recovery being cancelled")
			}
		})
	}
	details := map[string]bool{
		sessionShimControllerLostDetail:            true,
		sessionShimReadoptionAttemptsSpentDetail:   true,
		sessionShimReadoptionWindowExhaustedDetail: true,
	}
	if len(details) != 3 {
		t.Fatal("two of the three quarantine details are identical; the outcomes are indistinguishable again")
	}
	if !strings.Contains(sessionShimReadoptionWindowExhaustedDetail, "readoption_window_exhausted") {
		t.Fatalf("exhausted detail %q does not name the outcome", sessionShimReadoptionWindowExhaustedDetail)
	}
}

// TestExhaustedWindowNeverStrandsTheLineage pins that a hold while observable
// never strands the lineage: past the old window the lineage stays adopted
// with the keepalive running, not withdrawn. When the shim is gone (positive
// evidence), the lineage withdraws and the tombstone reconciler gives the
// capacity back — no adopted entry with a dead shim, no capacity leak.
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

	// Past the old window the hold keeps the lineage adopted with no
	// quarantine and no exhaustion hook while the shim is observable.
	lost := f.lostEntry(t)
	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := f.snapshot(); n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold did not retry past the old window within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if adopted := f.daemon.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != f.id {
		t.Fatalf("adopted set while holding = %+v, want the lineage still adopted", adopted)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v while holding, want none", projected)
	}
	notified.Lock()
	calls := notified.calls
	notified.Unlock()
	if calls != 0 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times while holding, want none", calls)
	}
	f.daemon.shims.stopRecovery()
	select {
	case got := <-done:
		if got != readoptionShutdown {
			t.Fatalf("disposition = %d, want readoptionShutdown (%d)", got, readoptionShutdown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not stop within 10s of recovery being cancelled")
	}
}

// TestLineageMayNotReenterAWindowBeforeThePreviousDeadline pins the re-entry
// bound against the window that GOVERNED the previous re-adoption. A lineage
// that flaps while still held re-enters paced by one backoff (so a flapping
// carrier costs at most one adoption revision per backoff); a lineage the
// composing layer no longer holds is refused instead of re-adopted.
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
	// measure against, well inside the ten minutes this one does. While the
	// lineage is held (nil LineageLive means held for a standalone daemon) it
	// re-enters paced by one backoff instead of being refused.
	lost.readoptedAtUnixNano = f.daemon.shimNow().Add(-90 * time.Second).UnixNano()

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) for a held lineage re-entering inside the window", got, readoptionSucceeded)
	}
	if adoptions, _ := f.snapshot(); adoptions != 1 {
		t.Fatalf("durable adoption attempted %d times for a held re-entry, want 1", adoptions)
	}

	// Past the governing window, the lineage may enter a new one (fresh fixture:
	// the first re-entry already installed a new controller, so the old lost
	// entry is stale and would never install again).
	f2 := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: policy})
	lost2 := f2.lostEntry(t)
	lost2.readoptedAtUnixNano = f2.daemon.shimNow().Add(-11 * time.Minute).UnixNano()
	if got := f2.daemon.readoptSessionShimAfterControllerLoss(f2.id, lost2, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) after the previous window's deadline had passed",
			got, readoptionSucceeded)
	}
	if adoptions, _ := f2.snapshot(); adoptions != 1 {
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
		// The backoff ceiling is configurable in both modes (see
		// DefaultSessionShimReadoptionBackoffCap): a fixed hold past its budget
		// retries with capped backoff.
		{name: "fixed mode with a backoff cap", policy: SessionShimReadoptionPolicy{BackoffCap: time.Second}, valid: true},
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
	// shim's reap withdraws the row again through the tombstone reconciler, so
	// a deadline the loaded publication path can outrun has this test racing
	// its own success. The reap it waits for afterwards is bounded by the
	// deadline, not by this number, so buying protection here costs only the
	// wait below.
	const orphanDeadline = 10 * time.Second
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
	const reapWait = 90 * time.Second
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
			// Both modes now hold past their initial budget while the shim
			// stays observable, so fixed also extends indefinitely and needs
			// the predicate under a declared threshold.
			name:       "fixed-attempt mode holds past its budget, so it needs the predicate too",
			cfg:        SessionShimConfig{Orphan: safeOrphan, Readoption: DefaultSessionShimReadoptionPolicy()},
			wantRefusd: true,
		},
		{
			name: "fixed-attempt mode with the predicate wired holds safely",
			cfg:  SessionShimConfig{Orphan: safeOrphan, Readoption: DefaultSessionShimReadoptionPolicy(), LineageLive: live},
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
			// Only the EFFECTIVE interval catches this: the raw field is far
			// under the deadline, and the floor is what the loop will really
			// pace at.
			name: "a sub-floor interval whose floored value leaves no slack",
			cfg: SessionShimConfig{
				Orphan: sessionshim.OrphanPolicy{Deadline: 400 * time.Millisecond, TerminationGrace: time.Second},
				Readoption: SessionShimReadoptionPolicy{
					Mode: ReadoptionLineageLive, KeepaliveInterval: 10 * time.Millisecond,
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
	// The orphan deadline is long enough that the shim cannot reap during the
	// test whatever the machine is doing: this pin is about the predicate
	// stopping the loop, and a reap would stop it for the wrong reason.
	const orphanDeadline = 20 * time.Second
	var held struct {
		sync.Mutex
		released bool
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: 100 * time.Millisecond,
			BackoffCap: 100 * time.Millisecond, Window: 30 * time.Second,
			KeepaliveInterval: 100 * time.Millisecond,
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
	deadline := time.Now().Add(20 * time.Second)
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
	time.Sleep(time.Second)
	settled := extended()
	time.Sleep(2 * time.Second)
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
	// The shell-free echo child: the seed only has to be alive behind the
	// adopted connection for the bind projection to be observable.
	echoSpec, err := daemonShimEchoSpec()
	if err != nil {
		t.Fatal(err)
	}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 9,
		Spec:         echoSpec,
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

// socketDeviceInode is the (device, inode) pair a discovery record binds to. A
// record whose pair does not match the live socket is refused before any frame
// is exchanged, so a fake shim has to carry the real one.
func socketDeviceInode(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform does not report socket device/inode")
	}
	return uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // platform-dependent widths, both non-negative identifiers
}

// preContractShim is a fake that answers every keepalive the way a shim binary
// built before this contract does: a valid Hello, then the handshake's
// `expected Welcome` refusal. It never arms an orphan clock, so it can never
// honour a keepalive — which is what every shim already running on a host makes
// this daemon the first time the change is rolled out, since surviving a daemon
// replacement is the whole reason those shims are being re-adopted.
type preContractShim struct {
	record   sessionshim.Record
	listener net.Listener

	mu        sync.Mutex
	exchanges int
}

func (p *preContractShim) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exchanges
}

// startPreContractShim publishes a discovery record for the fake and serves it
// until the test ends.
func startPreContractShim(t *testing.T, registry *sessionshim.Registry, dir string, id sessionshim.Identity) *preContractShim {
	t.Helper()
	socket := filepath.Join(dir, "pre.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	device, inode := socketDeviceInode(t, socket)
	self, err := sessionshim.Self()
	if err != nil {
		t.Fatal(err)
	}
	fake := &preContractShim{
		listener: listener,
		record: sessionshim.Record{
			SchemaVersion: sessionshim.RecordSchemaVersion,
			OrgID:         id.OrgID, SessionID: id.SessionID,
			ShimID: "00112233445566778899aabbccddeeff", ProcessEpoch: 4,
			PID: self.PID, ProcessStartedAt: self.StartedAt,
			SocketPath: socket, SocketDevice: device, SocketInode: inode,
			ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V1,
			Phase:             shimwire.PhaseRunning,
			WorkareaPath:      filepath.Join(dir, "workarea"),
			CreatedAtUnixNano: time.Now().UnixNano(),
		},
	}
	if err := registry.Put(fake.record); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go fake.serve(conn)
		}
	}()
	return fake
}

func (p *preContractShim) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	w := shimwire.NewWriter(conn)
	r := shimwire.NewReader(conn)
	hello, err := shimwire.EncodeHello(shimwire.Hello{
		Protocol: shimwire.ProtocolName, Min: shimwire.V1, Max: shimwire.V1,
		OrgID: p.record.OrgID, SessionID: p.record.SessionID,
		ShimID: p.record.ShimID, ProcessEpoch: p.record.ProcessEpoch,
		PID: p.record.PID, ProcessStartedAt: p.record.ProcessStartedAt,
		WorkareaPath: p.record.WorkareaPath, Phase: shimwire.PhaseRunning,
	})
	if err != nil || w.Write(shimwire.TypeHello, hello) != nil {
		return
	}
	if _, err := r.Read(); err != nil {
		return
	}
	p.mu.Lock()
	p.exchanges++
	p.mu.Unlock()
	body, err := shimwire.EncodeError(shimwire.ErrorMsg{Code: shimwire.CodeMalformed, Detail: "expected Welcome"})
	if err == nil {
		_ = w.Write(shimwire.TypeError, body)
	}
}

// TestKeepaliveAgainstAShimThatCanNeverHonourItStaysBounded is the pin for the
// cost of the unhonoured path.
//
// The fast probe exists for a race that resolves in milliseconds. Repeating it
// for as long as nothing has been honoured turned it into a spin against the
// one shim that can never honour anything — fifty exchanges a second for a
// whole window, each taking the shim's handshake lock and freezing its
// harness's output sequence through a round trip, and each asking the embedder
// its control-plane question. Escalating instead costs a handful of probes and
// then settles onto the configured pacing.
func TestKeepaliveAgainstAShimThatCanNeverHonourItStaysBounded(t *testing.T) {
	t.Parallel()
	const (
		orphanDeadline = 15 * time.Second // derives a 5 s keepalive interval
		observeFor     = time.Second
	)
	dir, err := os.MkdirTemp("/tmp", "drp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-pre", SessionID: "session-pre"}
	fake := startPreContractShim(t, registry, dir, id)

	var live struct {
		sync.Mutex
		calls int
	}
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     filepath.Join(dir, "registry"),
		CallbackTimeout: time.Second,
		Orphan:          sessionshim.OrphanPolicy{Deadline: orphanDeadline, TerminationGrace: time.Second},
		Readoption:      DefaultLineageLiveSessionShimReadoptionPolicy(),
		LineageLive: func(context.Context, sessionshim.Identity) bool {
			live.Lock()
			live.calls++
			live.Unlock()
			return true
		},
	}})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	cfg := d.sessionShimConfig()
	interval := cfg.readoptionKeepaliveInterval()
	if interval != orphanDeadline/sessionShimKeepaliveDeadlineDivisor {
		t.Fatalf("derived keepalive interval = %s, want %s", interval, orphanDeadline/sessionShimKeepaliveDeadlineDivisor)
	}

	stop := d.startSessionShimOrphanKeepalive(registry, cfg, id, shimwire.Hello{
		ShimID: fake.record.ShimID, ProcessEpoch: fake.record.ProcessEpoch,
	})
	time.Sleep(observeFor)
	stop()

	// Escalating from 20 ms doubles: 20, 40, 80, … so a second of probing is a
	// handful of exchanges, not a flood. The bound is deliberately generous —
	// the point is the order of magnitude, and the repeating version measured
	// forty-four.
	const maxExchanges = 12
	if got := fake.count(); got > maxExchanges {
		t.Fatalf("%d keepalive exchanges against a shim that can never honour one in %s, want at most %d — the fast probe is repeating rather than escalating",
			got, observeFor, maxExchanges)
	}
	if got := fake.count(); got == 0 {
		t.Fatal("no keepalive exchange happened at all; the fake was never reached")
	}
	live.Lock()
	calls := live.calls
	live.Unlock()
	// Only PACED ticks consult the predicate, and no paced tick is due inside
	// one second at a five-second interval — so the opening one is all there is.
	if calls > 2 {
		t.Fatalf("the embedder's LineageLive predicate was consulted %d times in %s; the fast probe is asking it too",
			calls, observeFor)
	}
	if keepalive := d.sessionShimKeepaliveObservations(id); keepalive.extensions != 0 || keepalive.refusals == 0 {
		t.Fatalf("observations = %+v, want no honoured extensions and a recorded refusal", keepalive)
	}
}

// TestTheFirstKeepaliveOfAWindowDoesNotWaitAWholeInterval pins the fast probe
// itself, without a fixture that pre-arms the deadline the probe exists to wait
// for.
//
// The window opens while the shim still has a controller attached — the state a
// daemon that re-dials promptly really arrives in, since the shim arms its
// orphan clock from its own serve-loop teardown. The first exchange is refused;
// the extension has to land shortly after the controller goes, not a whole
// keepalive interval later.
func TestTheFirstKeepaliveOfAWindowDoesNotWaitAWholeInterval(t *testing.T) {
	const (
		orphanDeadline = 6 * time.Second
		interval       = 3 * time.Second
	)
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Second,
			BackoffCap: time.Second, Window: 30 * time.Second,
			KeepaliveInterval: interval,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
	})
	registry, err := f.daemon.sessionShimRegistry()
	if err != nil {
		t.Fatal(err)
	}
	hello := f.controller.Hello()

	// Opens against an ATTACHED controller: nothing to extend, so the first
	// exchange is refused exactly as it is in the race this fallback covers.
	stop := f.daemon.startSessionShimOrphanKeepalive(registry, f.daemon.sessionShimConfig(), f.id, hello)
	defer stop()
	time.Sleep(200 * time.Millisecond)
	if keepalive := f.daemon.sessionShimKeepaliveObservations(f.id); keepalive.extensions != 0 {
		t.Fatalf("observations = %+v, want nothing honoured while a controller is attached", keepalive)
	}

	closed := time.Now()
	_ = f.controller.Close()

	deadline := time.Now().Add(interval - 500*time.Millisecond)
	for time.Now().Before(deadline) {
		if f.daemon.sessionShimKeepaliveObservations(f.id).extensions > 0 {
			if got := time.Since(closed); got >= interval {
				t.Fatalf("the first extension landed %s after the controller went, at or past the %s interval", got, interval)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no keepalive was honoured within %s of the controller going; the window waited a whole %s interval for its first probe",
		interval-500*time.Millisecond, interval)
}

// TestTheKeepaliveAsksWhetherTheLineageIsHeldBeforeItExtends pins the ORDER of
// the per-tick gate against the extension it guards. An extension sent after
// the composing layer released the lineage is the one §D8's inequality forbids,
// so the check has to come first — with the order reversed the loop lands one
// more extension on a lineage nobody holds any more.
func TestTheKeepaliveAsksWhetherTheLineageIsHeldBeforeItExtends(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 30 * time.Second,
			KeepaliveInterval: 50 * time.Millisecond,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: 5 * time.Second, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		// Released before the window ever opens.
		lineageLive: func(context.Context, sessionshim.Identity) bool { return false },
	})
	lost := f.lostEntry(t)
	// Armed and extendable: the ONLY thing that can stop an extension here is
	// the gate, which is what makes the count an assertion about ordering.
	f.waitForOrphanArmed(t, 5*time.Second)
	registry, err := f.daemon.sessionShimRegistry()
	if err != nil {
		t.Fatal(err)
	}

	stop := f.daemon.startSessionShimOrphanKeepalive(registry, f.daemon.sessionShimConfig(), f.id, lost.controller.Hello())
	time.Sleep(300 * time.Millisecond)
	stop()

	if keepalive := f.daemon.sessionShimKeepaliveObservations(f.id); keepalive.extensions != 0 {
		t.Fatalf("observations = %+v, want no extension at all: the lineage was already released when the window opened",
			keepalive)
	}
}

// TestKeepaliveObservationsAreResetAtEachWindow pins the projection's meaning.
// The count answers "did THIS window extend the shim's clock", which is the
// question an operator watching a mixed-version host is asking; a stale count
// carried over from a previous window answers a different one and reads as
// healthy.
func TestKeepaliveObservationsAreResetAtEachWindow(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 30 * time.Second,
			KeepaliveInterval: time.Second,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: 5 * time.Second, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		// Released, so the goroutine returns before extending anything and the
		// only thing that can zero the count is the reset itself.
		lineageLive: func(context.Context, sessionshim.Identity) bool { return false },
	})
	lost := f.lostEntry(t)
	registry, err := f.daemon.sessionShimRegistry()
	if err != nil {
		t.Fatal(err)
	}
	f.daemon.shims.mu.Lock()
	f.daemon.shims.keepalives[f.id] = sessionShimKeepaliveState{
		extensions: 99, refusals: 7, lastDeadlineUnixNano: 1234,
	}
	f.daemon.shims.mu.Unlock()

	stop := f.daemon.startSessionShimOrphanKeepalive(registry, f.daemon.sessionShimConfig(), f.id, lost.controller.Hello())
	stop()

	if got := f.daemon.sessionShimKeepaliveObservations(f.id); got.extensions != 0 || got.refusals != 0 || got.lastDeadlineUnixNano != 0 {
		t.Fatalf("observations at the start of a new window = %+v, want zero; a previous window's count leaked into the projection", got)
	}
}

// TestAConfiguredKeepaliveIntervalIsFlooredLikeADerivedOne pins the floor
// against the field that used to bypass it.
//
// The floor is a property of what a keepalive costs — a dial, the shim's
// handshake lock, and a question put to the embedder's control plane — not of
// who chose the number. Flooring only the derived interval left the configured
// field as a way straight past it: ten milliseconds paced the loop, and the
// LineageLive predicate with it, at nearly ninety calls a second against a shim
// that gains nothing from being asked more often than its own deadline needs.
func TestAConfiguredKeepaliveIntervalIsFlooredLikeADerivedOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		configured time.Duration
		deadline   time.Duration
		want       time.Duration
	}{
		{
			name:       "a configured interval far below the floor is clamped to it",
			configured: 10 * time.Millisecond, deadline: time.Minute,
			want: minSessionShimKeepaliveInterval,
		},
		{
			name:       "the floor itself is honoured as written",
			configured: minSessionShimKeepaliveInterval, deadline: time.Minute,
			want: minSessionShimKeepaliveInterval,
		},
		{
			name:       "a configured interval above the floor is left alone",
			configured: 4 * time.Second, deadline: time.Minute,
			want: 4 * time.Second,
		},
		{
			name:     "a derived interval is floored the same way",
			deadline: 300 * time.Millisecond,
			want:     minSessionShimKeepaliveInterval,
		},
		{
			name:     "a derived interval above the floor is a third of the deadline",
			deadline: 90 * time.Second,
			want:     30 * time.Second,
		},
	} {
		cfg := SessionShimConfig{
			Orphan: sessionshim.OrphanPolicy{Deadline: tc.deadline, TerminationGrace: time.Second},
			Readoption: SessionShimReadoptionPolicy{
				Mode: ReadoptionLineageLive, KeepaliveInterval: tc.configured,
			},
		}
		if got := cfg.readoptionKeepaliveInterval(); got != tc.want {
			t.Errorf("%s: effective keepalive interval = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestATinyConfiguredKeepaliveIntervalDoesNotPaceTheLoopOrThePredicate is the
// behavioural half: the floor has to reach the loop, not merely the accessor.
func TestATinyConfiguredKeepaliveIntervalDoesNotPaceTheLoopOrThePredicate(t *testing.T) {
	const (
		orphanDeadline = 15 * time.Second
		observeFor     = 2 * time.Second
	)
	dir, err := os.MkdirTemp("/tmp", "drf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-floor", SessionID: "session-floor"}
	fake := startPreContractShim(t, registry, dir, id)

	var live struct {
		sync.Mutex
		calls int
	}
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     filepath.Join(dir, "registry"),
		CallbackTimeout: time.Second,
		Orphan:          sessionshim.OrphanPolicy{Deadline: orphanDeadline, TerminationGrace: time.Second},
		Readoption: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Second, BackoffCap: time.Second,
			Window: time.Minute, KeepaliveInterval: 10 * time.Millisecond,
		},
		LineageLive: func(context.Context, sessionshim.Identity) bool {
			live.Lock()
			live.calls++
			live.Unlock()
			return true
		},
	}})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	cfg := d.sessionShimConfig()
	if got := cfg.readoptionKeepaliveInterval(); got < minSessionShimKeepaliveInterval {
		t.Fatalf("effective keepalive interval = %s, want it floored at %s", got, minSessionShimKeepaliveInterval)
	}

	stop := d.startSessionShimOrphanKeepalive(registry, cfg, id, shimwire.Hello{
		ShimID: fake.record.ShimID, ProcessEpoch: fake.record.ProcessEpoch,
	})
	time.Sleep(observeFor)
	stop()

	// At 10 ms the loop managed ~90 exchanges a second. Floored, the escalation
	// from 20 ms tops out at the 250 ms floor, so two seconds is a handful.
	const maxExchanges = 20
	if got := fake.count(); got > maxExchanges {
		t.Fatalf("%d keepalive exchanges in %s under a 10ms configured interval, want at most %d — the floor did not reach the loop",
			got, observeFor, maxExchanges)
	}
	live.Lock()
	calls := live.calls
	live.Unlock()
	// Only paced ticks ask, and a paced tick is the floor apart at the fastest.
	const maxPredicateCalls = 12
	if calls > maxPredicateCalls {
		t.Fatalf("the embedder's LineageLive predicate was consulted %d times in %s, want at most %d",
			calls, observeFor, maxPredicateCalls)
	}
}
