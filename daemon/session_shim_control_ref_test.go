package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

func controlRefFor(t *testing.T, d *Daemon, id sessionshim.Identity) SessionShimControlRef {
	t.Helper()
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	return SessionShimControlRef{
		Identity:             id,
		ShimID:               entry.shimID,
		ProcessEpoch:         entry.adoption.ProcessEpoch,
		ControllerGeneration: entry.adoption.ControllerGeneration,
	}
}

func TestReportAdoptedSessionShimCarrierTransportLostForExactRefIsOnceOnly(t *testing.T) {
	t.Parallel()
	var callbacks struct {
		sync.Mutex
		n int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Disabled: true},
		onBindLost: func(context.Context, sessionshim.Identity) {
			callbacks.Lock()
			callbacks.n++
			callbacks.Unlock()
		},
	})
	ref := controlRefFor(t, f.daemon, f.id)
	changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("carrier transport closed"))
	if err != nil || !changed {
		t.Fatalf("ReportAdoptedSessionShimCarrierTransportLostFor = %v, %v, want true, nil", changed, err)
	}
	first := bindingFor(t, f.daemon, f.id)
	if first.CarrierBound || first.LastCarrierLossAt.IsZero() {
		t.Fatalf("binding after loss = %+v, want unbound with one loss instant", first)
	}
	changed, err = f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("duplicate carrier transport close"))
	if err != nil || changed {
		t.Fatalf("duplicate report = %v, %v, want false, nil", changed, err)
	}
	if second := bindingFor(t, f.daemon, f.id); second.LastCarrierLossAt != first.LastCarrierLossAt {
		t.Fatalf("duplicate report changed loss instant: first=%s second=%s", first.LastCarrierLossAt, second.LastCarrierLossAt)
	}
	callbacks.Lock()
	defer callbacks.Unlock()
	if callbacks.n != 1 {
		t.Fatalf("carrier-loss callbacks = %d, want 1", callbacks.n)
	}
}

func TestCarrierLossAndRebindForRefuseStaleOrReplacementAuthority(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})
	ref := controlRefFor(t, f.daemon, f.id)
	for name, mutate := range map[string]func(SessionShimControlRef) SessionShimControlRef{
		"foreign org":      func(in SessionShimControlRef) SessionShimControlRef { in.Identity.OrgID = "other-org"; return in },
		"foreign shim":     func(in SessionShimControlRef) SessionShimControlRef { in.ShimID = "other-shim"; return in },
		"stale epoch":      func(in SessionShimControlRef) SessionShimControlRef { in.ProcessEpoch++; return in },
		"stale generation": func(in SessionShimControlRef) SessionShimControlRef { in.ControllerGeneration++; return in },
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(mutate(ref), errors.New("stale carrier transport close"))
			if err == nil || changed {
				t.Fatalf("stale report = %v, %v, want false and refusal", changed, err)
			}
			if binding := bindingFor(t, f.daemon, f.id); !binding.CarrierBound {
				t.Fatalf("stale report mutated current binding: %+v", binding)
			}
		})
	}

	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("carrier transport closed")); err != nil || !changed {
		t.Fatalf("exact loss report = %v, %v", changed, err)
	}
	if result, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID); err != nil || result != SessionShimRebound {
		t.Fatalf("RebindAdoptedSessionShim = %s, %v, want rebound", result, err)
	}
	adoptions, _ := f.snapshot()
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("old carrier transport close")); err == nil || changed {
		t.Fatalf("old ref after replacement = %v, %v, want stale refusal", changed, err)
	}
	if got, _ := f.snapshot(); got != adoptions {
		t.Fatalf("stale report drove %d adoptions, want unchanged at %d", got, adoptions)
	}
	if binding := bindingFor(t, f.daemon, f.id); !binding.CarrierBound {
		t.Fatalf("stale callback unbound replacement: %+v", binding)
	}
}

func TestReleaseSessionShimRebindClaimIsFencedToItsClaimedController(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})
	old, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	loseTheCarrierBinding(t, f)
	if result, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID); err != nil || result != SessionShimRebound {
		t.Fatalf("RebindAdoptedSessionShim = %s, %v, want rebound", result, err)
	}

	f.daemon.shims.mu.Lock()
	replacement := f.daemon.shims.adopted[f.id]
	if replacement.controller == old.controller {
		f.daemon.shims.mu.Unlock()
		t.Fatal("rebind did not install a replacement controller")
	}
	replacement.rebinding = true
	f.daemon.shims.adopted[f.id] = replacement
	f.daemon.shims.mu.Unlock()

	f.daemon.releaseSessionShimRebindClaim(f.id, old.controller)
	f.daemon.shims.mu.RLock()
	stillClaimed := f.daemon.shims.adopted[f.id].rebinding
	f.daemon.shims.mu.RUnlock()
	if !stillClaimed {
		t.Fatal("old completion cleared the replacement controller's active rebind claim")
	}
}

func TestFailedRebindReleasesItsOwnControllerClaim(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithAdoption(t, SessionShimReadoptionPolicy{Disabled: true}, func(context.Context, int) error {
		return context.DeadlineExceeded
	})
	loseTheCarrierBinding(t, f)
	if _, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID); err == nil {
		t.Fatal("rebind with refused adoption unexpectedly succeeded")
	}
	f.daemon.shims.mu.RLock()
	stillClaimed := f.daemon.shims.adopted[f.id].rebinding
	f.daemon.shims.mu.RUnlock()
	if stillClaimed {
		t.Fatal("failed rebind left its own controller claim set")
	}
}

func TestRebindKeepsLiveShimWhileReplacementGenerationIsStaged(t *testing.T) {
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
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	old := f.controller
	f.daemon.consumeShimEvents(old)
	oldHarness := old.HarnessIdentity()
	loseTheCarrierBinding(t, f)
	result := make(chan error, 1)
	go func() {
		_, err := f.daemon.RebindAdoptedSessionShim(context.Background(), f.id.OrgID, f.id.SessionID)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rebind never reached paused adoption after the shim accepted its new generation")
	}
	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old controller did not receive the shim's generation-replacement EOF")
	}
	// The old consumer has now observed EOF while the old entry still owns the
	// active rebind claim. It must not quarantine the live harness.
	time.Sleep(25 * time.Millisecond)
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("old EOF quarantined a live shim during staged replacement: %+v", projected)
	}
	f.daemon.shims.mu.RLock()
	staged := f.daemon.shims.adopted[f.id]
	f.daemon.shims.mu.RUnlock()
	if staged.controller != old || !staged.rebinding {
		t.Fatalf("staged entry = %+v, want old controller with active rebind claim", staged)
	}

	close(release)
	if err := <-result; err != nil {
		t.Fatalf("paused rebind: %v", err)
	}
	entry, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("replacement entry missing: %v", err)
	}
	if entry.controller == old || entry.controller.HarnessIdentity() != oldHarness {
		t.Fatalf("replacement did not retain the same live harness: old=%+v new=%+v", oldHarness, entry.controller.HarnessIdentity())
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("successful staged rebind left quarantine: %+v", projected)
	}
}

func TestTransportCarrierLossUsesExistingBoundedRecovery(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("transient prepare refusal")
		}
		return nil
	})
	ref := controlRefFor(t, f.daemon, f.id)
	old := f.controller
	f.daemon.consumeShimEvents(old)
	harness := old.HarnessIdentity()
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("typed transport loss")); err != nil || !changed {
		t.Fatalf("ReportAdoptedSessionShimCarrierTransportLostFor = %v, %v", changed, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entry, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
		if err == nil && entry.controller != old {
			if entry.controller.HarnessIdentity() != harness {
				t.Fatalf("bounded recovery changed harness identity: old=%+v new=%+v", harness, entry.controller.HarnessIdentity())
			}
			if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
				t.Fatalf("bounded recovery quarantined live shim: %+v", projected)
			}
			adoptions, _ := f.snapshot()
			if adoptions != 2 {
				t.Fatalf("bounded recovery attempts = %d, want transient refusal then success", adoptions)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entry, entryErr := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	adoptions, _ := f.snapshot()
	t.Fatalf("transport loss did not converge through the existing bounded recovery owner: entry=%+v err=%v adoptions=%d quarantined=%+v", entry, entryErr, adoptions, f.daemon.QuarantinedSessions())
}
