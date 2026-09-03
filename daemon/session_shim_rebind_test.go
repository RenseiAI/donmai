package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// loseTheCarrierBinding drives the daemon through the exact transition a
// carrier fault drives it through: the binding this daemon believed held is
// gone, the lineage is still adopted, and its controller is still live. That is
// the state RebindAdoptedSessionShim exists to repair, and it is reached in
// production from the disconnect path and from a re-adoption whose carrier
// activation failed.
func loseTheCarrierBinding(t *testing.T, f *readoptFixture) {
	t.Helper()
	if !f.daemon.noteSessionShimCarrierBindLost(f.id, f.controller) {
		t.Fatal("the fixture's lineage was not carrier-bound to begin with")
	}
}

func bindingFor(t *testing.T, d *Daemon, id sessionshim.Identity) SessionShimBinding {
	t.Helper()
	for _, binding := range d.AdoptedSessionShimBindings() {
		if binding.Identity == id {
			return binding
		}
	}
	t.Fatalf("no bind row for %s in %+v", id, d.AdoptedSessionShimBindings())
	return SessionShimBinding{}
}

// TestSessionShimBindStateIsSeededAtAdoptionAndClearedOnCarrierLoss pins the
// observable an embedder drives the rebind seam from.
//
// Both halves matter. A lineage that has never been rebound must report BOUND,
// or "already bound" names a state no healthy session can ever be in and the
// first rebind on a perfectly healthy session drives a repair nobody asked for.
// And the loss instant must be stamped on the LOSS, not on the bind: one field
// written by both transitions answers "how long has this lineage been unbound"
// with the moment it was last bound, which is wrong in sign.
func TestSessionShimBindStateIsSeededAtAdoptionAndClearedOnCarrierLoss(t *testing.T) {
	t.Parallel()
	var lost struct {
		sync.Mutex
		ids []sessionshim.Identity
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		onBindLost: func(_ context.Context, id sessionshim.Identity) {
			lost.Lock()
			lost.ids = append(lost.ids, id)
			lost.Unlock()
		},
	})

	seeded := bindingFor(t, f.daemon, f.id)
	if !seeded.CarrierBound {
		t.Fatal("a freshly adopted lineage reports CarrierBound=false; the state is unreachable for a healthy session")
	}
	if !seeded.LastCarrierLossAt.IsZero() {
		t.Fatalf("LastCarrierLossAt = %s on a lineage that never lost its binding, want the zero time", seeded.LastCarrierLossAt)
	}
	if diagnostics := f.daemon.SessionShimDiagnostics(); len(diagnostics.Adopted) != 1 ||
		!diagnostics.Adopted[0].CarrierBound || diagnostics.Adopted[0].LastCarrierLossAt != 0 {
		t.Fatalf("diagnostics = %+v, want the same bind state the adopted projection reports", diagnostics.Adopted)
	}

	// A carrier fault, through the disconnect path an embedder never sees.
	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	lost.Lock()
	notified := append([]sessionshim.Identity(nil), lost.ids...)
	lost.Unlock()
	if len(notified) != 1 || notified[0] != f.id {
		t.Fatalf("OnSessionShimCarrierBindLost notifications = %+v, want exactly one for %s", notified, f.id)
	}
}

// TestSessionShimBindLossStampsTheLossInstantOnly is the sign check on its own,
// because it is the half a passing "the field is populated" assertion hides.
func TestSessionShimBindLossStampsTheLossInstantOnly(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})
	before := f.daemon.shimNow()
	time.Sleep(2 * time.Millisecond)
	loseTheCarrierBinding(t, f)

	unbound := bindingFor(t, f.daemon, f.id)
	if unbound.CarrierBound {
		t.Fatal("CarrierBound is still true after the binding was lost")
	}
	if !unbound.LastCarrierLossAt.After(before) {
		t.Fatalf("LastCarrierLossAt = %s, want an instant after the bind at %s", unbound.LastCarrierLossAt, before)
	}
	if !unbound.BoundAt.Before(unbound.LastCarrierLossAt) {
		t.Fatalf("BoundAt %s is not before LastCarrierLossAt %s; one field is being written by both transitions",
			unbound.BoundAt, unbound.LastCarrierLossAt)
	}
}

// TestRebindDrivesARealDaemonSideReadoption pins the seam's whole point. A
// rebind is not a callback trampoline: it dials the shim afresh, proposes a
// strictly newer generation, adopts durably, and publishes a complete batch, so
// the receiver learns the lineage is bound again. The embedder's own hook runs
// afterwards, and it runs exactly once.
func TestRebindDrivesARealDaemonSideReadoption(t *testing.T) {
	t.Parallel()
	var hook struct {
		sync.Mutex
		calls int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		onRebind: func(context.Context, sessionshim.Identity) error {
			hook.Lock()
			hook.calls++
			hook.Unlock()
			return nil
		},
	})
	beforeGeneration := f.controller.Generation()
	loseTheCarrierBinding(t, f)

	result, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
	if err != nil || result != SessionShimRebound {
		t.Fatalf("RebindAdoptedSessionShim = %s, %v, want SessionShimRebound", result, err)
	}
	adoptions, batches := f.snapshot()
	if adoptions != 1 {
		t.Fatalf("durable adoption ran %d times, want exactly the one the rebind drove", adoptions)
	}
	if len(batches) != 1 || len(batches[0].Adopted) != 1 {
		t.Fatalf("published batches = %+v, want exactly one carrying the lineage; the receiver was never told", batches)
	}
	if got := batches[0].Adopted[0].Evidence.ControllerGeneration; got <= uint64(beforeGeneration) {
		t.Fatalf("re-adopted generation %d does not advance the previous %d", got, beforeGeneration)
	}
	if bound := bindingFor(t, f.daemon, f.id); !bound.CarrierBound {
		t.Fatal("the lineage still reports CarrierBound=false after a successful rebind")
	}

	// Idempotent: a second call on a bound lineage does nothing at all.
	result, err = f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
	if err != nil || result != SessionShimAlreadyBound {
		t.Fatalf("second RebindAdoptedSessionShim = %s, %v, want SessionShimAlreadyBound", result, err)
	}
	if again, _ := f.snapshot(); again != adoptions {
		t.Fatalf("durable adoption ran %d times after the second call, want it unchanged at %d", again, adoptions)
	}
	hook.Lock()
	calls := hook.calls
	hook.Unlock()
	if calls != 1 {
		t.Fatalf("OnSessionShimRebind ran %d times across two calls, want exactly once", calls)
	}
}

// TestRebindOfAHealthyLineageIsAlreadyBound pins that the FIRST call on a
// session whose binding is fine costs nothing. Before the bind state was seeded
// at adoption, this call drove a full re-adoption of a lineage that needed none.
func TestRebindOfAHealthyLineageIsAlreadyBound(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})

	result, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
	if err != nil || result != SessionShimAlreadyBound {
		t.Fatalf("RebindAdoptedSessionShim = %s, %v, want SessionShimAlreadyBound", result, err)
	}
	if adoptions, _ := f.snapshot(); adoptions != 0 {
		t.Fatalf("durable adoption ran %d times for a lineage that was already bound, want none", adoptions)
	}
}

// TestConcurrentRebindsDriveExactlyOneReadoption pins the concurrency contract
// under -race. Two callers arriving together must produce ONE daemon-side
// operation; the loser is told a rebind is in flight rather than being told the
// binding is current, which it may not be.
func TestConcurrentRebindsDriveExactlyOneReadoption(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		adoption: func(ctx context.Context, _ int) error {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	})
	loseTheCarrierBinding(t, f)

	results := make([]SessionShimRebindResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
	}()
	<-entered
	results[1], errs[1] = f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
	close(release)
	wg.Wait()

	if results[0] != SessionShimRebound || errs[0] != nil {
		t.Fatalf("the winning caller got %s, %v, want SessionShimRebound", results[0], errs[0])
	}
	if results[1] != SessionShimRebindInProgress {
		t.Fatalf("the second caller got %s, want SessionShimRebindInProgress", results[1])
	}
	if !errors.Is(errs[1], ErrSessionShimRebindInProgress) {
		t.Fatalf("the second caller's error = %v, want it to wrap ErrSessionShimRebindInProgress", errs[1])
	}
	if adoptions, _ := f.snapshot(); adoptions != 1 {
		t.Fatalf("durable adoption ran %d times for two concurrent rebinds, want exactly one operation", adoptions)
	}
}

// TestRebindErrorsAreDiscriminableThroughThePublicAPI pins that the refusals
// come back as sentinels an embedder can switch on, on the error the exported
// function really returned — not on one a test wrapped itself.
func TestRebindErrorsAreDiscriminableThroughThePublicAPI(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})

	result, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, "no-such-session")
	if result != SessionShimNotAdopted {
		t.Fatalf("rebind of an unknown session = %s, want SessionShimNotAdopted", result)
	}
	if !errors.Is(err, ErrSessionShimNotAdopted) {
		t.Fatalf("rebind of an unknown session error = %v, want it to wrap ErrSessionShimNotAdopted", err)
	}

	// The exact sentence existing callers read is preserved, byte for byte —
	// adding %w to adoptedShimEntry must not have reworded the refusal that
	// session_shim_burst_test.go's own pins match on.
	if want := "session shim: " + f.id.OrgID + "/no-such-session is not adopted by this daemon"; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("refusal message = %q, want it to end in %q", err.Error(), want)
	}

	// An adopted lineage whose controller is gone is a different refusal.
	f.daemon.shims.mu.Lock()
	entry := f.daemon.shims.adopted[f.id]
	entry.controller = nil
	f.daemon.shims.adopted[f.id] = entry
	f.daemon.shims.mu.Unlock()
	if _, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID); !errors.Is(err, ErrSessionShimNoController) {
		t.Fatalf("rebind of a lineage with no controller = %v, want it to wrap ErrSessionShimNoController", err)
	}

	// And a daemon with no session-shim state at all refuses distinctly.
	bare := New(Options{SkipRegistration: true})
	bare.shims = nil
	if _, err := bare.RebindAdoptedSessionShim(context.Background(), "org", "session"); !errors.Is(err, ErrSessionShimAdoptionNotConfigured) {
		t.Fatalf("rebind on an unconfigured daemon = %v, want it to wrap ErrSessionShimAdoptionNotConfigured", err)
	}
}
