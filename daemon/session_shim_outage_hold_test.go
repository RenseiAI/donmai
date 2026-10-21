package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestFixedHoldRetriesPastBudgetUntilCarrierReturns pins the fixed-mode half
// of the outage hold: spending the attempt budget with the shim still
// observable and the composing layer holding the lineage past the bound does
// not quarantine. The hold retries with capped backoff and the keepalive
// running until the carrier returns, so a healthy harness survives silence
// past the budget.
func TestFixedHoldRetriesPastBudgetUntilCarrierReturns(t *testing.T) {
	t.Parallel()
	const returnsOnAttempt = 3
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(_ context.Context, attempt int) error {
			if attempt < returnsOnAttempt {
				return errors.New("injected carrier outage: connection refused")
			}
			return nil
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d) — the hold did not retry past the spent budget", got, readoptionSucceeded)
	}
	if held.calls.Load() == 0 {
		t.Fatal("ReadoptionHeldPastWindow was never consulted, yet the budget was exceeded")
	}
	adoptions, batches := f.snapshot()
	if adoptions != returnsOnAttempt {
		t.Fatalf("durable adoption attempted %d times, want %d — the hold stopped at the fixed-attempt cutoff", adoptions, returnsOnAttempt)
	}
	if len(batches) != 1 || len(batches[0].Adopted) != 1 {
		t.Fatalf("published batches = %+v, want exactly one carrying the lineage adopted", batches)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v, want none — a healthy harness on silence must stay adopted", projected)
	}
	found := false
	for _, id := range f.daemon.AdoptedSessionShims() {
		if id == f.id {
			found = true
		}
	}
	if !found {
		t.Fatalf("adopted projection = %+v, want it to still list the held lineage", f.daemon.AdoptedSessionShims())
	}
}

// TestFixedHoldKeepsLineageLocallyVisibleDuringOutage pins the local-interaction
// half: while the hold retries past the budget, the lineage stays in the
// adopted projection (host watch keeps working, local attach stays possible)
// and out of quarantine. Only a release ends it.
func TestFixedHoldKeepsLineageLocallyVisibleDuringOutage(t *testing.T) {
	t.Parallel()
	var hold atomic.Bool
	hold.Store(true)
	held := &heldPastWindowCounter{answer: hold.Load}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(context.Context, int) error {
			return errors.New("injected carrier outage: relay draining")
		},
	})
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
			t.Fatal("the hold did not retry past the spent budget within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	found := false
	for _, id := range f.daemon.AdoptedSessionShims() {
		if id == f.id {
			found = true
		}
	}
	if !found {
		t.Fatalf("adopted projection = %+v during the hold, want it to still list the lineage", f.daemon.AdoptedSessionShims())
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v during the hold, want none", projected)
	}
	hold.Store(false)
	select {
	case got := <-done:
		if got != readoptionLineageGone {
			t.Fatalf("disposition = %d, want readoptionLineageGone (%d) once the composing layer let the lineage go", got, readoptionLineageGone)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not end within 10s of the composing layer letting go")
	}
}

// TestFixedHoldDecisiveReleaseEndsHoldAtOnce pins the positive-evidence half:
// a decisive, authenticated released answer ends an armed hold at once even
// when the shim is still observable. Silence retries; a release withdraws.
func TestFixedHoldDecisiveReleaseEndsHoldAtOnce(t *testing.T) {
	t.Parallel()
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(_ context.Context, attempt int) error {
			if attempt < 2 {
				return errors.New("injected carrier outage: timeout")
			}
			return errors.Join(errors.New("carrier refused"), ErrSessionShimLineageReleased)
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionLineageGone {
		t.Fatalf("disposition = %d, want readoptionLineageGone (%d) — a decisive release must end the hold", got, readoptionLineageGone)
	}
	if adoptions, _ := f.snapshot(); adoptions != 2 {
		t.Fatalf("durable adoption attempted %d times, want 2 — the hold must stop at the release, not retry through it", adoptions)
	}
}

// TestFixedHoldCredentialExpiryEndsHold pins the security half: an expired
// credential ends an armed hold even when the shim is still observable and
// held. An unreachable control plane never grants anything new and never
// extends credential lifetime.
func TestFixedHoldCredentialExpiryEndsHold(t *testing.T) {
	t.Parallel()
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(_ context.Context, attempt int) error {
			if attempt < 2 {
				return errors.New("injected carrier outage: 503")
			}
			return errors.Join(errors.New("proof invalid"), ErrSessionShimCredentialExpired)
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionLineageGone {
		t.Fatalf("disposition = %d, want readoptionLineageGone (%d) — expired credentials must end the hold", got, readoptionLineageGone)
	}
}

// TestFixedHoldEndsWhenComposingLayerLetsGo pins that the hold checks the
// composing layer's answer on every iteration: once the layer stops holding
// the lineage past the bound, the hold ends and the lineage is withdrawn so
// its harness can reap on the ordinary deadline.
func TestFixedHoldEndsWhenComposingLayerLetsGo(t *testing.T) {
	t.Parallel()
	const releaseAfterAttempt = 2
	var released atomic.Bool
	held := &heldPastWindowCounter{answer: func() bool { return !released.Load() }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(_ context.Context, attempt int) error {
			if attempt >= releaseAfterAttempt {
				released.Store(true)
			}
			return errors.New("injected carrier outage: timeout")
		},
	})
	lost := f.lostEntry(t)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionLineageGone {
		t.Fatalf("disposition = %d, want readoptionLineageGone (%d) once the composing layer let the lineage go", got, readoptionLineageGone)
	}
}

// TestFixedHoldKeepsOrphanKeepaliveRunning pins the keepalive half: while the
// hold retries past the spent budget, the daemon keeps extending the shim's
// orphan clock, so the shim does not reap a harness the daemon can still see.
func TestFixedHoldKeepsOrphanKeepaliveRunning(t *testing.T) {
	const orphanDeadline = time.Second
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 2, Backoff: 50 * time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		adoption: func(_ context.Context, attempt int) error {
			if attempt < 7 {
				return errors.New("injected carrier outage: 503")
			}
			return nil
		},
	})
	lost := f.lostEntry(t)
	f.waitForOrphanArmed(t, 5*time.Second)

	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d)", got, readoptionSucceeded)
	}
	if extensions := f.daemon.sessionShimKeepaliveObservations(f.id).extensions; extensions == 0 {
		t.Fatal("the hold ran past the budget without ever extending the shim's orphan clock")
	}
	if _, err := f.registry.GetTombstoneIncarnation(f.id, lost.shimID, lost.controller.Hello().ProcessEpoch); err == nil {
		t.Fatal("a terminal tombstone exists for a lineage the hold kept alive")
	}
}

// TestFixedHoldCancelMidHoldReturnsShutdown pins that the hold is bounded by
// the daemon's lifetime: cancelling recovery mid-hold stops retrying and
// reports shutdown, never a quarantine and never another attempt.
func TestFixedHoldCancelMidHoldReturnsShutdown(t *testing.T) {
	t.Parallel()
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:         SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		lineageLive:    alwaysLive,
		heldPastWindow: held.predicate,
		adoption: func(context.Context, int) error {
			return errors.New("injected carrier outage: connection refused")
		},
	})
	lost := f.lostEntry(t)

	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := f.snapshot(); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hold did not start retrying within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
	before, _ := f.snapshot()
	f.daemon.shims.stopRecovery()
	select {
	case got := <-done:
		if got != readoptionShutdown {
			t.Fatalf("disposition = %d, want readoptionShutdown (%d) after recovery was cancelled mid-hold", got, readoptionShutdown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not stop within 10s of recovery being cancelled")
	}
	if after, _ := f.snapshot(); after > before+1 {
		t.Fatalf("durable adoption ran %d times after cancellation (was %d) — a cancelled hold must not keep dialling", after, before)
	}
}

// TestFixedHoldConfigValidation pins the config gate: a fixed-attempt policy
// may hold past its budget only when the composing layer holds the lineage on
// unbounded terms, and the keepalive and cap fields stay refused in fixed
// mode — the hold runs on the default cap and the derived interval.
func TestFixedHoldConfigValidation(t *testing.T) {
	t.Parallel()
	orphan := sessionshim.OrphanPolicy{Deadline: 90 * time.Second, TerminationGrace: 5 * time.Second}
	held := func(context.Context, sessionshim.Identity) bool { return true }
	fixed := DefaultSessionShimReadoptionPolicy()
	heldFixed := SessionShimConfig{
		Orphan: orphan, Readoption: fixed,
		LineageLive: alwaysLive, ReadoptionHeldPastWindow: held,
	}
	if err := heldFixed.validateReadoptionAgainstOrphanPolicy(); err != nil {
		t.Fatalf("fixed hold with both predicates validate = %v, want nil", err)
	}
	unheld := SessionShimConfig{
		Orphan: orphan, Readoption: fixed,
		ReadoptionHeldPastWindow: held,
	}
	if err := unheld.validateReadoptionAgainstOrphanPolicy(); !errors.Is(err, ErrSessionShimReadoptionPolicy) {
		t.Fatalf("fixed hold without LineageLive validate = %v, want ErrSessionShimReadoptionPolicy", err)
	}
	capped := SessionShimReadoptionPolicy{BackoffCap: time.Second}
	if err := capped.Validate(); !errors.Is(err, ErrSessionShimReadoptionPolicy) {
		t.Fatalf("fixed BackoffCap Validate = %v, want ErrSessionShimReadoptionPolicy", err)
	}
	interval := SessionShimReadoptionPolicy{KeepaliveInterval: time.Second}
	if err := interval.Validate(); !errors.Is(err, ErrSessionShimReadoptionPolicy) {
		t.Fatalf("fixed KeepaliveInterval Validate = %v, want ErrSessionShimReadoptionPolicy", err)
	}
}
