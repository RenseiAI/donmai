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
// The corpus names no duration for a durable acknowledgement outstanding on a
// LIVE stream, so the bound takes the longest one it does name for a daemon
// that is visibly still here: the lineage-live re-adoption window. A default
// that drifted away from that window would leave the derivation true only
// historically, and nothing in either package would notice.
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
			// And it must actually REACH the controller, not merely be
			// computable: a derivation nothing passes on is the same drift.
			if got := (sessionshim.ControllerOptions{
				DurableAckAmbiguityBound: cfg.durableAckAmbiguityBound(),
			}).ResolvedDurableAckAmbiguityBound(); got != tc.want {
				t.Fatalf("the bound a dialled controller resolves = %s, want %s", got, tc.want)
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

// TestClassifyShimStreamEndOnlyRefinesTheAmbiguitySentinel pins the seam
// between the two packages. This is the single decision that decides which
// reason a lineage is published under, and it must turn on exact evidence
// rather than on any drop the controller happened to make.
func TestClassifyShimStreamEndOnlyRefinesTheAmbiguitySentinel(t *testing.T) {
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
			// A consumer that genuinely stopped, with nothing outstanding to
			// prove otherwise, is not ambiguity and keeps its disposition.
			name:   "a stopped consumer exceeded the backlog budget",
			endErr: sessionshim.ErrEventBacklogExceeded,
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
