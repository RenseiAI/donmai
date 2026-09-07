package daemon

// Provenance: durable-ack-is-ambiguity — grep a build for this marker to prove
// a slow durable side can no longer publish `socket_unreachable` for a socket
// that answered the whole time.
//
// # THE STRAND THESE TESTS UNDO
//
// Measured on a consumer daemon: a healthy interactive session, a control plane
// answering every request 200, and one slow DURABLE receipt. The shim
// connection was alive throughout; only the acknowledgement latency was high.
// The controller's own back-pressure gave up after thirty seconds, this
// daemon read that ending as the shim's, quarantined the lineage
// `socket_unreachable` with no re-adoption at all, and the control plane
// terminalized it ninety-five seconds later off that reason.
//
// ADR-2026-08-30 D2 puts "a transport-level acknowledgement without the durable
// post-condition" on the closed list of evidence that is ALWAYS ambiguous, and
// ambiguous evidence "may not unlink, tombstone, release an external claim,
// ... or emit a terminal session outcome". `socket_unreachable` is a claim
// about the socket, and ADR-2026-08-17's 2026-09-02 amendment reaches it only
// "when every [re-adoption] attempt fails" after a loss that was "the daemon
// lost its durable carrier, not the shim's socket". Neither describes a
// reachable socket whose receipts were slow.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestDelayedDurableAckKeepsTheLineageAdopted is the RED test for the field
// incident: the durable side is slow, never refusing, and the shim is alive and
// re-adoptable. The lineage must stay adopted under a strictly newer
// generation, with no quarantine and no terminal evidence anywhere.
func TestDelayedDurableAckKeepsTheLineageAdopted(t *testing.T) {
	t.Parallel()
	// The durable side answers — late. The first attempt is refused the way a
	// still-catching-up carrier refuses; the second is accepted.
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 10 * time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("durable acknowledgement still catching up")
		}
		return nil
	})
	d := f.daemon
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	_, batches := f.snapshot()
	for i, batch := range batches {
		if len(batch.Quarantined) != 0 {
			t.Fatalf("batch %d quarantined a lineage whose socket answered throughout: %+v", i, batch.Quarantined)
		}
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("a delayed durable acknowledgement left %d lineages quarantined: %+v", len(projected), projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set over a slow durable side: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
}

// TestExhaustedDurableAckAmbiguityQuarantinesUnderItsOwnReason pins the
// degrade-visibly half. When even re-adoption cannot recover the lineage the
// daemon does withdraw — but under a reason that describes what happened, and
// the quarantine-only reconciler must not treat that reason as terminal.
func TestExhaustedDurableAckAmbiguityQuarantinesUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("durable acknowledgement never landed")
	})
	d := f.daemon

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	projected := d.QuarantinedSessions()
	if len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want exactly the withdrawn lineage", projected)
	}
	if projected[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine reason = %q, want %q — the socket answered throughout",
			projected[0].Reason, sessionshim.QuarantineDurableAckTimeout)
	}
	if projected[0].Reason == sessionshim.QuarantineSocketUnreachable {
		t.Fatal("a reachable socket was published as unreachable; that reason is what the control plane terminalized")
	}
	if !projected[0].ConsumesCapacity {
		t.Fatal("the withdrawn lineage stopped consuming capacity; its harness is still held")
	}
	if !projected[0].Reason.Known() {
		t.Fatalf("reason %q is outside the closed registry; a consumer branching on it would fall through", projected[0].Reason)
	}

	// NOT TERMINAL. The reconciler discharges a quarantined lineage only on a
	// group-reaped tombstone for that exact incarnation, and this shim is
	// alive, so nothing about this lineage may close.
	if published := d.reconcileQuarantinedTombstones(); len(published) != 0 {
		t.Fatalf("the reconciler terminalized a durable_ack_timeout lineage with no tombstone: %+v", published)
	}
	after := d.QuarantinedSessions()
	if len(after) != 1 || after[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine projection after reconciliation = %+v, want the lineage still held under its own reason", after)
	}
}

// TestSocketFailureAndCarrierRefusalStillQuarantineSocketUnreachable is the
// control. The new reason is for one fact pattern only; every ending that
// really is about the socket keeps the disposition it had.
func TestSocketFailureAndCarrierRefusalStillQuarantineSocketUnreachable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause shimStreamEndCause
	}{
		{
			// The socket went away, or the shim broke the sequence contract:
			// the pre-existing disposition, quarantined at once with no re-dial.
			name:  "a real socket failure",
			cause: shimStreamEnded,
		},
		{
			// An explicit refusal from the durable carrier, re-adopted first
			// and refused every time.
			name:  "an explicit carrier refusal whose re-adoption never lands",
			cause: shimStreamCarrierLost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 2, Backoff: 5 * time.Millisecond}, func(int) error {
				return errors.New("carrier candidate dial refused")
			})
			d := f.daemon

			d.releaseShimIfLive(f.id, f.controller, tc.cause)

			projected := d.QuarantinedSessions()
			if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineSocketUnreachable {
				t.Fatalf("quarantine projection = %+v, want one socket_unreachable lineage", projected)
			}
			if !projected[0].ConsumesCapacity {
				t.Fatal("the quarantined lineage stopped consuming capacity")
			}
		})
	}
}

// TestDurableAckAmbiguityBoundIsTheLineageLiveWindow pins the derivation the
// bound's own documentation claims.
//
// ADR-2026-09-03 makes the bound "equal to the lineage-live readoption window",
// so the package DEFAULT must equal that window's default. A default that
// drifted away from it would leave the derivation true only historically, and
// nothing in either package would notice.
//
// This is the weaker of the two assertions and is kept only as a floor: two
// constants agreeing proves nothing about a CONFIGURED deployment, which is
// what TestDurableAckAmbiguityBoundFollowsTheConfiguredWindow pins.
func TestDurableAckAmbiguityBoundIsTheLineageLiveWindow(t *testing.T) {
	t.Parallel()
	if sessionshim.DurableAckAmbiguityBound != defaultSessionShimReadoptionWindow {
		t.Fatalf("durable-ack ambiguity bound = %s, want the %s lineage-live re-adoption window it is derived from",
			sessionshim.DurableAckAmbiguityBound, defaultSessionShimReadoptionWindow)
	}
}

// TestDurableAckAmbiguityBoundFollowsTheConfiguredWindow is the pin the ADR's
// Risks section asks for by name: "Implementations MUST treat the two as one
// configured value, not two values that happen to default identically today."
//
// Pinning the package default against the package default proves only that two
// constants match. What has to hold is that a deployment which CONFIGURES a
// window gets that window as its ambiguity bound — so a controller this daemon
// dials under a twenty-minute window holds for twenty minutes, not ten.
func TestDurableAckAmbiguityBoundFollowsTheConfiguredWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		want   time.Duration
	}{
		{
			name: "an unconfigured policy takes the default window",
			want: defaultSessionShimReadoptionWindow,
		},
		{
			name: "a configured lineage-live window is the bound",
			policy: SessionShimReadoptionPolicy{
				Mode: ReadoptionLineageLive, Window: 20 * time.Minute, BackoffCap: 30 * time.Second,
			},
			want: 20 * time.Minute,
		},
		{
			// A fixed-attempts policy configures no window at all; its attempt
			// arithmetic answers a different question. It takes the same
			// default window an unset one does.
			name:   "a fixed-attempts policy takes the default window",
			policy: SessionShimReadoptionPolicy{Mode: ReadoptionFixedAttempts, Attempts: 3},
			want:   defaultSessionShimReadoptionWindow,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.policy.Validate(); tc.policy.Mode != 0 && err != nil {
				t.Fatalf("fixture policy is invalid: %v", err)
			}
			cfg := SessionShimConfig{Readoption: tc.policy}
			if got := cfg.durableAckAmbiguityBound(); got != tc.want {
				t.Fatalf("durable-ack ambiguity bound = %s, want the %s window this policy resolves to", got, tc.want)
			}

			// And it must actually REACH every controller this daemon dials.
			// These assert on the structs PRODUCTION builds, not on structs
			// rebuilt here: a test that composes its own ControllerOptions
			// asserts its own arithmetic and stays green with both production
			// assignments deleted, which is precisely the drift the ADR's Risks
			// section is about.
			d := &Daemon{shims: newSessionShimState()}

			adoptOpts, _, err := d.sessionShimAdoptOptions(&sessionshim.Registry{}, cfg)
			if err != nil {
				t.Fatalf("sessionShimAdoptOptions: %v", err)
			}
			if got := adoptOpts.ResolvedDurableAckAmbiguityBound(); got != tc.want {
				t.Fatalf("the adoption pass dials controllers with a %s bound, want %s", got, tc.want)
			}

			launchOpts := d.sessionShimLaunchControllerOptions(cfg, "/work/repo", "/work")
			if got := launchOpts.ResolvedDurableAckAmbiguityBound(); got != tc.want {
				t.Fatalf("the launch path dials controllers with a %s bound, want %s", got, tc.want)
			}
		})
	}
}

// seedAmbiguityStreakToItsBudget puts a lineage one exit short of the cycle
// budget. The streak is seeded rather than slept through: each real cycle costs
// a whole ambiguity bound, and driving it in wall-clock time would also drag in
// the re-adoption window's own re-entry guard — a different mechanism on a
// different clock.
func seedAmbiguityStreakToItsBudget(t *testing.T, d *Daemon, id sessionshim.Identity) {
	t.Helper()
	for cycle := 1; cycle < maxConsecutiveDurableAckAmbiguityCycles; cycle++ {
		if got := d.noteDurableAckAmbiguityCycle(id); got != cycle {
			t.Fatalf("consecutive ambiguity cycles = %d after cycle %d, want %d", got, cycle, cycle)
		}
	}
}

// TestTheBudgetedAmbiguityCycleStillReadoptsALiveShim is the pin the contract
// asks for by name. ADR-2026-09-03: exhausting the bound "does not quarantine
// directly, it re-enters the ordinary re-adoption pipeline first … it must not
// shortcut past the re-adoption check that would otherwise catch a shim that
// is, in fact, still live and reachable." A live, reachable shim on the
// budgeted cycle is re-adopted, not withdrawn.
func TestTheBudgetedAmbiguityCycleStillReadoptsALiveShim(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return nil // the shim is there; the durable side is merely slow
	})
	d := f.daemon
	seedAmbiguityStreakToItsBudget(t, d, f.id)
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("the budgeted cycle withdrew a live, reachable shim: %+v — the ADR rejects exactly this shortcut", projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the budgeted cycle left a re-adoptable lineage un-adopted: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
	// The look is cheaper, not skipped: one attempt, not the policy's three.
	if adoptions, _ := f.snapshot(); adoptions != 1 {
		t.Fatalf("the budgeted cycle ran %d durable adoption(s), want exactly the one reduced attempt", adoptions)
	}
}

// TestTheBudgetedAmbiguityCycleWithdrawsOnlyWhenReadoptionFails pins the other
// outcome of that same pipeline run: the shim really is gone, so the reduced
// attempt fails and the lineage withdraws under its own reason and detail.
func TestTheBudgetedAmbiguityCycleWithdrawsOnlyWhenReadoptionFails(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("durable acknowledgement never landed")
	})
	d := f.daemon
	seedAmbiguityStreakToItsBudget(t, d, f.id)

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	// The pipeline ran — and ran REDUCED. Three attempts here is the livelock
	// the budget exists to shrink; zero is the shortcut the ADR rejects.
	if adoptions, _ := f.snapshot(); adoptions != 1 {
		t.Fatalf("the budgeted cycle ran %d durable adoption(s), want exactly the one reduced attempt", adoptions)
	}
	projected := d.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine after %d consecutive cycles = %+v, want one durable_ack_timeout lineage",
			maxConsecutiveDurableAckAmbiguityCycles, projected)
	}
	if projected[0].Detail != sessionShimDurableAckCyclesSpentDetail {
		t.Fatalf("detail = %q, want the spent-cycles detail — an operator cannot otherwise tell this from one slow bound",
			projected[0].Detail)
	}
	if got := d.durableAckAmbiguityCycles(f.id); got != 0 {
		t.Fatalf("the streak survived the withdrawal (%d); a later lineage on this identity would inherit a spent budget", got)
	}
}

// TestPlatformCarrierLossesAreBoundedBeforeQuarantine proves that a control
// plane persistence outage gets recovery attempts without permanently
// bypassing the re-adoption window. Two recoveries preserve the live harness;
// the bounded third loss withdraws it visibly instead of cycling forever.
func TestPlatformCarrierLossesAreBoundedBeforeQuarantine(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return nil
	})
	d := f.daemon
	d.shims.mu.Lock()
	entry := d.shims.adopted[f.id]
	entry.readoptedAtUnixNano = d.shimNow().UnixNano()
	d.shims.adopted[f.id] = entry
	d.shims.mu.Unlock()

	// The first platform-originated loss retries successfully. The real shim
	// controller owns a goroutine, so seed the preceding completed cycles below
	// rather than racing its asynchronous close path to simulate later losses.
	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLostPlatform)
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("first platform loss quarantined instead of re-adopting: %+v", projected)
	}

	reachable := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error { return nil })
	seedAmbiguityStreakToItsBudget(t, reachable.daemon, reachable.id)
	reachable.daemon.releaseShimIfLive(reachable.id, reachable.controller, shimStreamCarrierLostPlatform)
	if projected := reachable.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("spent platform-loss streak quarantined a reachable shim: %+v", projected)
	}
	if got := reachable.daemon.durableAckAmbiguityCycles(reachable.id); got != 0 {
		t.Fatalf("successful spent-streak re-adoption retained %d ambiguity cycles", got)
	}

	terminal := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("platform persistence remains unavailable")
	})
	seedAmbiguityStreakToItsBudget(t, terminal.daemon, terminal.id)
	terminal.daemon.releaseShimIfLive(terminal.id, terminal.controller, shimStreamCarrierLostPlatform)
	projected := terminal.daemon.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("platform-loss streak quarantine = %+v, want one durable-ack timeout", projected)
	}
	if projected[0].Detail != sessionShimDurableAckCyclesSpentDetail {
		t.Fatalf("platform-loss streak detail = %q, want %q", projected[0].Detail, sessionShimDurableAckCyclesSpentDetail)
	}
}

// TestATerminalExitClearsTheAmbiguityStreak pins the path that returns EARLY.
// finishAdoptedShim removes the adopted entry without going through the
// withdrawal, so a streak left behind there would be inherited by a RE-LAUNCHED
// lineage on the same org+session id — whose very first re-adoption would then
// be reduced to one attempt for a condition it never experienced.
func TestATerminalExitClearsTheAmbiguityStreak(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Disabled: true}, func(int) error { return nil })
	d := f.daemon
	seedAmbiguityStreakToItsBudget(t, d, f.id)
	if d.durableAckAmbiguityCycles(f.id) == 0 {
		t.Fatal("the streak was not seeded")
	}

	d.finishAdoptedShim(f.id, shimwire.ExitMsg{ExitCode: 0})

	if got := d.durableAckAmbiguityCycles(f.id); got != 0 {
		t.Fatalf("consecutive ambiguity cycles = %d after a terminal exit, want 0 — a re-launched lineage would inherit it", got)
	}
}

// TestANonAmbiguousEndingEndsTheAmbiguityStreak pins the word "consecutive".
func TestANonAmbiguousEndingEndsTheAmbiguityStreak(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return nil
	})
	d := f.daemon

	if got := d.noteDurableAckAmbiguityCycle(f.id); got != 1 {
		t.Fatalf("consecutive ambiguity cycles = %d, want 1", got)
	}

	// An ordinary carrier loss is a different fact, and it resets the streak —
	// so an ambiguity exit an hour later starts from one again rather than
	// inheriting a budget spent on an unrelated condition.
	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)
	if got := d.durableAckAmbiguityCycles(f.id); got != 0 {
		t.Fatalf("consecutive ambiguity cycles = %d after an unrelated ending, want 0", got)
	}
}

// TestAmbiguityDoesNotRaiseCarrierBindLost pins that this path asserts only what
// it observed. The carrier never refused anything on the ambiguity path — it was
// slow — so the binding is not gone, and raising bind-lost would hand the
// composing layer a fact that did not happen.
func TestAmbiguityDoesNotRaiseCarrierBindLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		cause     shimStreamEndCause
		wantRaise bool
	}{
		{name: "the ambiguity path never raises it", cause: shimStreamDurableAckAmbiguous},
		// Same reasoning, reached by the other sentinel: a reader that gave up
		// on its own consumer observed nothing about the carrier either.
		{name: "a stalled consumer never raises it", cause: shimStreamConsumerStalled},
		{name: "a real carrier loss still raises it", cause: shimStreamCarrierLost, wantRaise: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var raised atomic.Int64
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy:   SessionShimReadoptionPolicy{Attempts: 2, Backoff: 5 * time.Millisecond},
				adoption: func(context.Context, int) error { return nil },
				onBindLost: func(context.Context, sessionshim.Identity) {
					raised.Add(1)
				},
			})

			f.daemon.releaseShimIfLive(f.id, f.controller, tc.cause)

			if got := raised.Load() > 0; got != tc.wantRaise {
				t.Fatalf("carrier bind-lost raised = %v (%d times), want %v", got, raised.Load(), tc.wantRaise)
			}
		})
	}
}

// TestClassifyShimStreamEndRefinesOnlyItsOwnSentinels pins the seam between the
// two packages. This is the single decision that decides which reason a lineage
// is published under, and it must turn on exact TYPED evidence — errors.Is on a
// sentinel — rather than on any drop the controller happened to make or on the
// text of an error message.
func TestClassifyShimStreamEndRefinesOnlyItsOwnSentinels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cause  shimStreamEndCause
		endErr error
		want   shimStreamEndCause
	}{
		{
			name: "the peer ended the stream",
			want: shimStreamEnded,
		},
		{
			name:   "this daemon gave up with a durable acknowledgement outstanding",
			endErr: sessionshim.ErrDurableAckAmbiguityBound,
			want:   shimStreamDurableAckAmbiguous,
		},
		{
			name:   "the ambiguity sentinel arrives wrapped",
			endErr: errors.Join(errors.New("controller dropped its shim connection"), sessionshim.ErrDurableAckAmbiguityBound),
			want:   shimStreamDurableAckAmbiguous,
		},
		{
			// INVERTED. This used to pin `shimStreamEnded`, on the reading that
			// a consumer with nothing outstanding "has stopped" and keeps the
			// dead-shim disposition. It does not: the sentinel is produced by
			// THIS daemon's reader giving up on THIS daemon's consumer, and it
			// observed nothing whatsoever about the socket. Publishing
			// `socket_unreachable` for it with no re-dial is what the control
			// plane terminalized ninety-five seconds later — the same shape as
			// the ambiguity case above, reached by a different sentinel.
			name:   "this daemon's reader gave up on its own consumer",
			endErr: sessionshim.ErrEventBacklogExceeded,
			want:   shimStreamConsumerStalled,
		},
		{
			name: "the backlog sentinel arrives wrapped",
			endErr: errors.Join(
				errors.New("controller dropped its shim connection"), sessionshim.ErrEventBacklogExceeded,
			),
			want: shimStreamConsumerStalled,
		},
		{
			// The two refinements are disjoint and each is selected by its own
			// typed sentinel, never by the text of an error message.
			name:   "an unrelated ending is not refined by either sentinel",
			endErr: errors.New("use of closed network connection"),
			want:   shimStreamEnded,
		},
		{
			// A carrier loss is already the recovering disposition; the
			// refinement must never demote or re-label it.
			name:   "a carrier loss is left exactly as it was",
			cause:  shimStreamCarrierLost,
			endErr: sessionshim.ErrDurableAckAmbiguityBound,
			want:   shimStreamCarrierLost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyShimStreamEnd(tc.cause, tc.endErr); got != tc.want {
				t.Fatalf("classifyShimStreamEnd(%d, %v) = %d, want %d", tc.cause, tc.endErr, got, tc.want)
			}
		})
	}
}

// TestConsumerStallKeepsTheLineageAdopted is the RED control for the second
// half of the kill path.
//
// Removing the 30-second deadline stopped the carrier being dropped over a slow
// durable consumer. It did not, on its own, stop the DROP that does eventually
// happen from being fatal: `ErrEventBacklogExceeded` still classified as an
// ordinary ending, which quarantines `socket_unreachable` with no re-dial at
// all, and the control plane terminalizes off that reason ninety-five seconds
// later. That moved the kill window from thirty seconds to ten minutes; it did
// not remove it.
//
// The shim is alive at the drop — its harness is retained and its socket
// answers — so the lineage must be re-adopted, and a re-adopted controller
// starts with an empty backlog, which is exactly the recovery the situation
// calls for. Reverting classifyShimStreamEnd's backlog case turns this RED at
// the first assertion: the lineage is in quarantine instead of adopted.
func TestConsumerStallKeepsTheLineageAdopted(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 10 * time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("durable consumer still catching up")
		}
		return nil
	})
	d := f.daemon
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, classifyShimStreamEnd(shimStreamEnded, sessionshim.ErrEventBacklogExceeded))

	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("a stalled consumer left %d lineages quarantined: %+v — the shim was answering throughout",
			len(projected), projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set over this daemon's own stalled consumer: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d); no re-adoption happened",
			entry.controller.Generation())
	}
}

// TestExhaustedConsumerStallQuarantinesUnderItsOwnReason pins the
// degrade-visibly half: when even re-adoption cannot recover the lineage the
// daemon does withdraw, but never under a claim about a socket it never
// observed.
func TestExhaustedConsumerStallQuarantinesUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("durable consumer never came back")
	})
	d := f.daemon

	d.releaseShimIfLive(f.id, f.controller, shimStreamConsumerStalled)

	projected := d.QuarantinedSessions()
	if len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want exactly the withdrawn lineage", projected)
	}
	if projected[0].Reason == sessionshim.QuarantineSocketUnreachable {
		t.Fatal("a reachable socket was published as unreachable over this daemon's own stalled consumer; " +
			"that reason is what the control plane terminalized")
	}
	if projected[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine reason = %q, want %q — the durable side could not keep up, the socket was fine",
			projected[0].Reason, sessionshim.QuarantineDurableAckTimeout)
	}
	if !projected[0].ConsumesCapacity {
		t.Fatal("the withdrawn lineage stopped consuming capacity; its harness is still held")
	}
	if !projected[0].Reason.Known() {
		t.Fatalf("reason %q is outside the closed registry; a consumer branching on it would fall through", projected[0].Reason)
	}
	// NOT TERMINAL: the shim is alive, so nothing about this lineage may close.
	if published := d.reconcileQuarantinedTombstones(); len(published) != 0 {
		t.Fatalf("the reconciler terminalized a stalled-consumer lineage with no tombstone: %+v", published)
	}
}

// TestEveryDaemonSideEndingRedialsBeforeItWithdraws is the guard on the
// predicate two independent changes widened in the same week.
//
// `releaseShimIfLive` branches once on "is this ending about the shim, or about
// me?", and the wrong answer is not a small one: an ending that falls to the
// shim side is quarantined `socket_unreachable` with NO re-dial, which is the
// reason a control plane terminalizes on. Both of the changes that widened this
// condition did so by extending an enumeration, and an enumeration is the shape
// that fails silently — a cause left out of it does not break a build, it kills
// a live seat.
//
// So the predicate is a negative test on the ONE cause that is genuinely about
// the shim, and this asserts that property over the whole value space rather
// than over a list a future change would have to remember to update. A new
// cause defaults to recovering; a cause that really is a statement about the
// shim has to be classified as shimStreamEnded to get there.
//
// Rewriting shimStreamEndRecovers as a positive enumeration turns this RED for
// whichever cause the enumeration forgets.
func TestEveryDaemonSideEndingRedialsBeforeItWithdraws(t *testing.T) {
	t.Parallel()
	for value := range 8 {
		//nolint:gosec // G115: a small literal range over the cause's own type
		cause := shimStreamEndCause(value)
		want := cause != shimStreamEnded
		if got := shimStreamEndRecovers(cause); got != want {
			t.Fatalf("shimStreamEndRecovers(%d) = %v, want %v: an ending this daemon reached about "+
				"itself must re-dial before it withdraws, and only a statement about the shim may skip it",
				cause, got, want)
		}
	}
	// Every named cause, spelled out, so the intent survives a refactor of the
	// loop above — and so the two that were added by separate changes in the
	// same week are each asserted by name rather than by having been swept up.
	if !shimStreamEndRecovers(shimStreamConsumerStalled) || !shimStreamEndRecovers(shimStreamDurableAckAmbiguous) {
		t.Fatal("a reader-side ending was routed to the no-redial branch")
	}
	if !shimStreamEndRecovers(shimStreamCarrierLost) || !shimStreamEndRecovers(shimStreamCarrierLostPlatform) {
		t.Fatal("a carrier loss was routed to the no-redial branch")
	}
	if shimStreamEndRecovers(shimStreamEnded) {
		t.Fatal("an ending the shim or its socket reached was given a re-dial it has no peer for")
	}
}

// TestDurableConsumerSlowEndingsShareOneReasonAndOneStreak names the OTHER
// widened predicate. This one is a positive set by necessity — it selects the
// endings that mean "the durable side could not keep up", which share a
// withdrawal reason and a consecutive-cycle streak because they are one
// condition observed from different sides — so it cannot be made fail-safe by
// construction and needs its members pinned instead. It has three intakes now:
// an outstanding acknowledgement, a reader that gave up on its own consumer,
// and a durable append whose evidence named the persistence path.
//
// A carrier that explicitly REFUSED is not in the set: that one loses its
// binding and gets the socket reason when its re-adoptions fail.
func TestDurableConsumerSlowEndingsShareOneReasonAndOneStreak(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		//nolint:govet // field order here is readability, not layout
		cause shimStreamEndCause
		want  bool
	}{
		{name: "a durable acknowledgement stayed outstanding", cause: shimStreamDurableAckAmbiguous, want: true},
		{name: "this daemon's reader gave up on its own consumer", cause: shimStreamConsumerStalled, want: true},
		{name: "the durable append named the persistence path", cause: shimStreamCarrierLostPlatform, want: true},
		{name: "an explicit carrier refusal is not the same fact", cause: shimStreamCarrierLost},
		{name: "an ending about the shim is not the same fact", cause: shimStreamEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shimStreamEndIsDurableConsumerSlow(tc.cause); got != tc.want {
				t.Fatalf("shimStreamEndIsDurableConsumerSlow(%d) = %v, want %v", tc.cause, got, tc.want)
			}
		})
	}
}

// TestARecoveredConsumerStallClearsTheStreak pins what "consecutive" has to
// mean once this counter has more than one intake.
//
// The streak buys a cheaper LAST LOOK — one attempt instead of the full budget —
// after three endings in a row that each ran their bound out. A recovery in the
// middle of that sequence breaks the run, so leaving the counter standing makes
// it monotone across successes: a long-lived seat on a durable side that is
// occasionally slow arrives at the one-attempt budget and stays there for the
// daemon's lifetime, having recovered every single time.
//
// It matters more with this PR's cause than it did with the first: a shared
// durable consumer stalls every adopted session at once, so every lineage's
// streak advances together and the whole host reaches the reduced budget in
// lock-step.
//
// Deleting the clearDurableAckAmbiguityCycles call from the readoptionSucceeded
// arm turns this RED.
func TestARecoveredConsumerStallClearsTheStreak(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return nil
	})
	d := f.daemon
	seedAmbiguityStreakToItsBudget(t, d, f.id)

	d.releaseShimIfLive(f.id, f.controller, shimStreamConsumerStalled)

	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("a recovered consumer stall quarantined the lineage: %+v", projected)
	}
	if _, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
		t.Fatalf("the lineage left the adopted set after a successful re-adoption: %v", err)
	}
	if got := d.durableAckAmbiguityCycles(f.id); got != 0 {
		t.Fatalf("a successful re-adoption retained %d consecutive cycles; the run was broken by a recovery", got)
	}
}
