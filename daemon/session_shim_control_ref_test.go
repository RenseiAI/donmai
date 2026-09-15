package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
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
	consumed := make(chan struct{}, 1)
	f.daemon.shims.afterReleaseShimIfLive = func() { consumed <- struct{}{} }
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
	awaitRecoverySignal(t, consumed, "old consumer EOF disposition")
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
			return errors.New("transient adoption callback refusal")
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

func TestTransportCarrierLossRefusesShutdownAndTerminalPrecedence(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Disabled: true}})
	ref := controlRefFor(t, f.daemon, f.id)
	f.daemon.lifecycleMu.Lock()
	f.daemon.stopGen = &stopGeneration{id: 1}
	f.daemon.lifecycleMu.Unlock()
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("transport during shutdown")); err == nil || changed {
		t.Fatalf("shutdown report = %v, %v, want refusal without mutation", changed, err)
	}
	f.daemon.lifecycleMu.Lock()
	f.daemon.stopGen = nil
	f.daemon.lifecycleMu.Unlock()

	// finishAdoptedShim is the same production terminal owner the real Exit
	// event reaches. Once it has consumed terminality, a later transport report
	// cannot re-open recovery for that lineage.
	f.daemon.finishAdoptedShim(f.id, shimwire.ExitMsg{})
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("transport after exit")); err == nil || changed {
		t.Fatalf("post-Exit report = %v, %v, want terminal precedence refusal", changed, err)
	}
}

// awaitRecoverySignal observes completion, never elapsed time as evidence.
func awaitRecoverySignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// holdRecoveryDrain holds a real Stop before final controller release. The
// landing cancellation proves Stop crossed its synchronized drain boundary.
func holdRecoveryDrain(t *testing.T, d *Daemon) (<-chan error, func()) {
	t.Helper()
	release := make(chan struct{})
	d.landingMu.Lock()
	d.landingDone = release
	d.landingMu.Unlock()
	done := make(chan error, 1)
	go func() { done <- d.Stop(context.Background()) }()
	awaitRecoverySignal(t, d.landingCtx.Done(), "Stop drain admission")
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return done, unblock
}

func TestTransportRecoveryStopBeforeReportLeavesControllerOpen(t *testing.T) {
	t.Parallel()
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond}})
	ref := controlRefFor(t, f.daemon, f.id)
	done, release := holdRecoveryDrain(t, f.daemon)
	if changed, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("late transport close")); changed || err == nil {
		t.Fatalf("late report = %v, %v", changed, err)
	}
	select {
	case <-f.controller.Done():
		t.Fatal("Stop-before-report closed controller during drain")
	default:
	}
	if n, _ := f.snapshot(); n != 0 {
		t.Fatalf("late report prepared %d adoptions", n)
	}
	if f.daemon.sessionShimReconcileStopped() {
		t.Fatal("Stop drain suppressed terminal reconciliation before release")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTransportRecoveryStopBeforeAttemptAdmission(t *testing.T) {
	t.Parallel()
	entered, resume, consumed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{policy: SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond}})
	f.daemon.shims.transportRecoveryAttemptHook = func() { close(entered); <-resume }
	f.daemon.shims.afterReleaseShimIfLive = func() { consumed <- struct{}{} }
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.daemon.consumeShimEvents(f.controller)
	if _, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(controlRefFor(t, f.daemon, f.id), errors.New("transport close")); err != nil {
		t.Fatal(err)
	}
	awaitRecoverySignal(t, entered, "attempt admission barrier")
	done, release := holdRecoveryDrain(t, f.daemon)
	unblock()
	awaitRecoverySignal(t, consumed, "cancelled consumer disposition")
	if n, _ := f.snapshot(); n != 0 {
		t.Fatalf("Stop-before-attempt ran %d adoptions", n)
	}
	if q := f.daemon.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("shutdown quarantined live lineage: %+v", q)
	}
	if _, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
		t.Fatalf("shutdown removed lineage before final release: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTransportRecoveryStopCancelsAdmittedPrepareAndJoins(t *testing.T) {
	t.Parallel()
	entered, cancelled, resume, consumed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var calls struct {
		sync.Mutex
		n int
	}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond},
		prepare: func(ctx context.Context, _ SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			calls.Lock()
			calls.n++
			n := calls.n
			calls.Unlock()
			if n == 1 {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-resume
			}
			return sessionshim.PreparedAdoption{}, ctx.Err()
		},
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	joined := make(chan struct{})
	var joinOnce sync.Once
	finishConsumer := func() { joinOnce.Do(func() { close(joined) }) }
	t.Cleanup(finishConsumer)
	f.daemon.shims.afterReleaseShimIfLive = func() { consumed <- struct{}{}; <-joined }
	f.daemon.consumeShimEvents(f.controller)
	if _, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(controlRefFor(t, f.daemon, f.id), errors.New("transport close")); err != nil {
		t.Fatal(err)
	}
	awaitRecoverySignal(t, entered, "actual PrepareAdoption callback")
	done, release := holdRecoveryDrain(t, f.daemon)
	awaitRecoverySignal(t, cancelled, "prepare context cancellation")
	select {
	case err := <-done:
		t.Fatalf("Stop returned before recovery joined: %v", err)
	default:
	}
	unblock()
	awaitRecoverySignal(t, consumed, "cancelled prepare consumer completion")
	calls.Lock()
	n := calls.n
	calls.Unlock()
	if n != 1 {
		t.Fatalf("shutdown started another prepare: calls=%d", n)
	}
	if q := f.daemon.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("shutdown cancellation quarantined lineage: %+v", q)
	}
	if _, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err != nil {
		t.Fatalf("shutdown removed lineage before final release: %v", err)
	}
	release()
	awaitRecoverySignal(t, f.daemon.shims.reconcileStop, "final release before consumer join")
	select {
	case err := <-done:
		t.Fatalf("Stop returned while consumer still owned bookkeeping: %v", err)
	default:
	}
	finishConsumer()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTransportReportActualExitWinsInConsumer(t *testing.T) {
	t.Parallel()
	entered, resume, terminal, consumed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)
	var terminalOnce, exitOnce sync.Once
	proof := make(chan SessionShimTerminalEvidence, 1)
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond},
		onTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			terminalOnce.Do(func() { proof <- evidence; terminal <- struct{}{} })
			return nil
		},
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.daemon.opts.SessionShim.OnSessionEvent = func(_ sessionshim.Identity, ev sessionshim.ControllerEvent) {
		if ev.Kind == sessionshim.EventExit || (ev.Kind == sessionshim.EventHostFrame && ev.FrameType == attachwire.TypeExit) {
			exitOnce.Do(func() { close(entered); <-resume })
		}
	}
	f.daemon.shims.afterReleaseShimIfLive = func() { consumed <- struct{}{} }
	ref := controlRefFor(t, f.daemon, f.id)
	f.daemon.consumeShimEvents(f.controller)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.shim.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	awaitRecoverySignal(t, entered, "actual Exit delivered to consumer")
	// The consumer has received a genuine Exit but has not yet committed its
	// terminal bookkeeping. A transport report must not make EOF own recovery.
	if _, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("Done raced Exit")); err != nil {
		t.Fatal(err)
	}
	unblock()
	awaitRecoverySignal(t, terminal, "terminal evidence publication")
	if evidence := <-proof; evidence.Adoption == nil || len(evidence.DurableAdoptionCorrelation) == 0 {
		t.Fatal("Exit was published by quarantine reconciliation instead of the adopted terminal consumer")
	}
	awaitRecoverySignal(t, consumed, "terminal consumer completion")
	if n, _ := f.snapshot(); n != 0 {
		t.Fatalf("actual Exit started %d recoveries", n)
	}
	if q := f.daemon.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("actual Exit quarantined: %+v", q)
	}
}

func TestTransportReportAndFailedFrameShareConsumerRecovery(t *testing.T) {
	t.Parallel()
	for _, watcherFirst := range []bool{true, false} {
		name := "frame_first"
		if watcherFirst {
			name = "watcher_first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, resume, consumed, prepareEntered, prepareResume := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
			var outputOnce, adoptionOnce, resumeOnce, prepareOnce sync.Once
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond},
				adoption: func(ctx context.Context, _ int) error {
					adoptionOnce.Do(func() { close(prepareEntered) })
					select {
					case <-prepareResume:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			unblock := func() { resumeOnce.Do(func() { close(resume) }); prepareOnce.Do(func() { close(prepareResume) }) }
			t.Cleanup(unblock)
			f.daemon.opts.SessionShim.OnSessionEventDurable = func(_ sessionshim.Identity, ev sessionshim.ControllerEvent) error {
				isOutput := ev.Kind == sessionshim.EventOutput || (ev.Kind == sessionshim.EventHostFrame && ev.FrameType == attachwire.TypeOutput)
				first := false
				if isOutput {
					outputOnce.Do(func() { first = true; close(entered) })
				}
				if first {
					<-resume
					return errors.New("carrier transport closed")
				}
				return nil
			}
			f.daemon.shims.afterReleaseShimIfLive = func() { consumed <- struct{}{} }
			ref := controlRefFor(t, f.daemon, f.id)
			oldHarness := f.controller.HarnessIdentity()
			f.daemon.consumeShimEvents(f.controller)
			if err := f.controller.WriteInput([]byte("competition\n")); err != nil {
				t.Fatal(err)
			}
			awaitRecoverySignal(t, entered, "real output durable callback")
			if !watcherFirst {
				resumeOnce.Do(func() { close(resume) })
				awaitRecoverySignal(t, prepareEntered, "consumer recovery adoption")
			}
			if _, err := f.daemon.ReportAdoptedSessionShimCarrierTransportLostFor(ref, errors.New("watcher observed Done")); err != nil {
				t.Fatal(err)
			}
			resumeOnce.Do(func() { close(resume) })
			awaitRecoverySignal(t, prepareEntered, "single recovery adoption")
			prepareOnce.Do(func() { close(prepareResume) })
			awaitRecoverySignal(t, consumed, "recovery consumer completion")
			if n, _ := f.snapshot(); n != 1 {
				t.Fatalf("frame/Done competition ran %d adoption owners", n)
			}
			entry, err := f.daemon.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if entry.controller == f.controller || entry.controller.HarnessIdentity() != oldHarness {
				t.Fatal("recovery did not preserve exact harness")
			}
			if q := f.daemon.QuarantinedSessions(); len(q) != 0 {
				t.Fatalf("competing notifications quarantined lineage: %+v", q)
			}
		})
	}
}
