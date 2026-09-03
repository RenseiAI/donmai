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
		// Seeded exactly as every real adoption path seeds it.
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
	var held struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 30 * time.Second,
		},
		adoption: refusingCarrier(),
		lineageLive: func(context.Context, sessionshim.Identity) bool {
			held.Lock()
			defer held.Unlock()
			held.calls++
			return held.calls < 3
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost); got != readoptionLineageGone {
		t.Fatalf("disposition = %d, want readoptionLineageGone (%d) once the composing layer let the lineage go",
			got, readoptionLineageGone)
	}
	held.Lock()
	calls := held.calls
	held.Unlock()
	if calls != 3 {
		t.Fatalf("LineageLive consulted %d times, want exactly 3 — the loop kept going after it said no", calls)
	}
	adoptions, _ := f.snapshot()
	if adoptions != 2 {
		t.Fatalf("durable adoption attempted %d times, want the 2 attempts before the lineage was released", adoptions)
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
	extensions, lastDeadline := f.daemon.sessionShimKeepaliveObservations(f.id)
	if extensions == 0 {
		t.Fatal("the daemon extended the shim's orphan clock zero times across the whole window")
	}
	if lastDeadline.IsZero() {
		t.Fatal("no re-armed deadline was ever observed from the shim")
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
	if extensions, _ := f.daemon.sessionShimKeepaliveObservations(f.id); extensions == 0 {
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

// TestExhaustedWindowNotifyOnlyKeepsTheLineageAdopted pins the other configured
// outcome. ReadoptionNotifyOnly exists so a composition that can repair the
// carrier out of band is never told the lineage was withdrawn — an enum member
// that suppressed nothing would be worse than not having one.
func TestExhaustedWindowNotifyOnlyKeepsTheLineageAdopted(t *testing.T) {
	t.Parallel()
	var notified struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Millisecond,
			BackoffCap: time.Millisecond, Window: 50 * time.Millisecond,
			PostWindowOutcome: ReadoptionNotifyOnly,
		},
		adoption: refusingCarrier(),
		onWindowExhausted: func(context.Context, sessionshim.Identity) {
			notified.Lock()
			notified.calls++
			notified.Unlock()
		},
	})

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v, want none under ReadoptionNotifyOnly", projected)
	}
	adopted := f.daemon.AdoptedSessionShims()
	if len(adopted) != 1 || adopted[0] != f.id {
		t.Fatalf("adopted set = %+v, want the lineage retained under ReadoptionNotifyOnly", adopted)
	}
	notified.Lock()
	calls := notified.calls
	notified.Unlock()
	if calls != 1 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", calls)
	}
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
		{name: "fixed mode with a post-window outcome", policy: SessionShimReadoptionPolicy{PostWindowOutcome: ReadoptionNotifyOnly}},
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
	const orphanDeadline = 500 * time.Millisecond
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.registry.GetTombstoneIncarnation(f.id, hello.ShimID, hello.ProcessEpoch); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal tombstone %s after a quarantine whose orphan deadline was %s — the harness runs forever",
		10*time.Second, orphanDeadline)
}
