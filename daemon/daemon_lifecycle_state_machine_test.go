package daemon

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func awaitLifecycleSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestLifecycleLeaseWaitHonorsContextForStartAndUpdateOwners(t *testing.T) {
	for _, kind := range []struct {
		name string
		kind lifecycleKind
	}{
		{name: "start", kind: lifecycleStart},
		{name: "update", kind: lifecycleUpdate},
	} {
		t.Run(kind.name, func(t *testing.T) {
			d := New(Options{})
			d.setState(StateRunning)
			owner, err := d.claimLifecycle(context.Background(), kind.kind)
			if err != nil {
				t.Fatalf("claim owner: %v", err)
			}
			defer d.releaseLifecycle(owner)

			ctx, cancel := context.WithCancel(context.Background())
			stopDone := make(chan error, 1)
			go func() { stopDone <- d.Stop(ctx) }()
			cancel()

			select {
			case err := <-stopDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Stop while %s owns lifecycle = %v, want context canceled", kind.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Stop stayed blocked behind %s owner after context cancellation", kind.name)
			}

			d.lifecycleMu.Lock()
			stopGen := d.stopGen
			state := d.State()
			d.lifecycleMu.Unlock()
			if stopGen != nil {
				t.Fatalf("Stop created generation while %s owner remained active", kind.name)
			}
			if state != StateRunning {
				t.Fatalf("State = %q while %s owner remained active, want running", state, kind.name)
			}
		})
	}
}

func TestDaemonResumeRejectsActiveManualDrainGeneration(t *testing.T) {
	preSpawnEntered := make(chan struct{})
	releasePreSpawn := make(chan struct{})
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "project", Repository: "github.com/acme/project"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(preSpawnEntered)
			<-releasePreSpawn
			return nil, errors.New("release pending reservation")
		},
	})
	pendingAccept := make(chan error, 1)
	go func() {
		_, err := spawner.AcceptWork(SessionSpec{SessionID: "pending", Repository: "github.com/acme/project"})
		pendingAccept <- err
	}()
	awaitLifecycleSignal(t, preSpawnEntered, "pending spawn reservation")

	drainSnapshotEntered := make(chan struct{})
	releaseDrainSnapshot := make(chan struct{})
	spawner.drainBeforeContextSnapshot = func() {
		close(drainSnapshotEntered)
		<-releaseDrainSnapshot
	}

	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	drainDone := make(chan error, 1)
	go func() { drainDone <- d.Drain(drainCtx) }()
	cancelDrain()
	awaitLifecycleSignal(t, drainSnapshotEntered, "manual drain final snapshot")

	if d.Resume() {
		t.Fatal("Resume reopened admission while manual Drain still owned lifecycle")
	}
	if d.State() != StateDraining {
		t.Fatalf("State during active Drain = %q, want draining", d.State())
	}
	if spawner.IsAccepting() {
		t.Fatal("spawner accepted work while manual Drain was active")
	}

	close(releasePreSpawn)
	select {
	case err := <-pendingAccept:
		if err == nil {
			t.Fatal("pending AcceptWork unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending reservation did not release")
	}
	close(releaseDrainSnapshot)
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("Drain after reservation release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manual Drain did not return after reservation release")
	}

	if !d.Resume() {
		t.Fatal("Resume did not reopen admission after Drain returned")
	}
	if d.State() != StateRunning || !spawner.IsAccepting() {
		t.Fatalf("manual drain resume state/admission = (%q, %v), want (running, true)", d.State(), spawner.IsAccepting())
	}
}

func TestDaemonStopStartsWorkerDrainWhenPollAndLandingCannotJoin(t *testing.T) {
	landingEntered := make(chan struct{})
	releaseLanding := make(chan struct{})
	d := New(Options{
		OnLandingWork: func(context.Context, PollWorkItem) error {
			close(landingEntered)
			<-releaseLanding // deliberately ignores cancellation
			return nil
		},
	})
	spawner := NewWorkerSpawner(SpawnerOptions{})
	termRequested := make(chan struct{})
	spawner.terminateProcessGroup = func(_ *exec.Cmd) error {
		close(termRequested)
		return nil
	}
	spawner.mu.Lock()
	spawner.sessions["worker"] = &spawnedSession{
		handle: SessionHandle{SessionID: "worker"},
		spec:   SessionSpec{SessionID: "worker"},
		cancel: func() {},
	}
	spawner.mu.Unlock()
	d.spawner = spawner
	d.setState(StateRunning)

	landingDone := make(chan error, 1)
	go func() {
		landingDone <- d.handlePollWorkItem(PollWorkItem{WorkType: LandingWorkType}, "")
	}()
	awaitLifecycleSignal(t, landingEntered, "landing callback")

	stopCtx, cancelStop := context.WithCancel(context.Background())
	pollDone := make(chan struct{})
	d.poller = &PollService{cancel: cancelStop, done: pollDone, running: true}
	attemptReady := make(chan struct{})
	releaseAttempt := make(chan struct{})
	d.stopAttemptBeforeRelease = func(error) {
		close(attemptReady)
		<-releaseAttempt
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- d.Stop(stopCtx) }()

	awaitLifecycleSignal(t, termRequested, "worker SIGTERM request")
	awaitLifecycleSignal(t, attemptReady, "incomplete Stop result")
	if spawner.IsAccepting() {
		t.Fatal("spawner admission remained open after Stop started")
	}
	if d.State() != StateDraining {
		t.Fatalf("State after incomplete Stop = %q, want draining", d.State())
	}
	select {
	case <-d.Done():
		t.Fatal("Done closed before poll, landing, and worker barriers completed")
	default:
	}

	close(releaseAttempt)
	select {
	case err := <-stopDone:
		var incomplete *DrainIncompleteError
		if !errors.As(err, &incomplete) {
			t.Fatalf("Stop error = %v, want DrainIncompleteError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("incomplete Stop did not return")
	}

	close(pollDone)
	close(releaseLanding)
	select {
	case err := <-landingDone:
		if err != nil {
			t.Fatalf("landing callback: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("landing callback did not release")
	}
	spawner.mu.Lock()
	delete(spawner.sessions, "worker")
	spawner.mu.Unlock()
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop retry after all barriers released: %v", err)
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Done remained open after successful Stop retry")
	}
}

func TestDaemonStopAttemptOwnershipMakesTerminalResultImmutable(t *testing.T) {
	spawner := NewWorkerSpawner(SpawnerOptions{})
	spawner.terminateProcessGroup = func(_ *exec.Cmd) error { return nil }
	spawner.mu.Lock()
	spawner.sessions["straggler"] = &spawnedSession{
		handle: SessionHandle{SessionID: "straggler"},
		spec:   SessionSpec{SessionID: "straggler"},
		cancel: func() {},
	}
	spawner.mu.Unlock()

	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)
	attemptResultReady := make(chan struct{})
	releaseAttemptA := make(chan struct{})
	var hookOnce sync.Once
	d.stopAttemptBeforeRelease = func(error) {
		hookOnce.Do(func() {
			close(attemptResultReady)
			<-releaseAttemptA
		})
	}

	attemptACtx, cancelAttemptA := context.WithCancel(context.Background())
	cancelAttemptA()
	attemptADone := make(chan error, 1)
	go func() { attemptADone <- d.Stop(attemptACtx) }()
	awaitLifecycleSignal(t, attemptResultReady, "attempt A incomplete result")

	d.lifecycleMu.Lock()
	ownerA := d.lifecycleOwner
	gen := d.stopGen
	d.lifecycleMu.Unlock()
	if ownerA == nil || ownerA.kind != lifecycleStop || gen == nil || gen.terminal {
		t.Fatal("attempt A did not retain the expected non-terminal Stop ownership")
	}

	attemptBStarted := make(chan struct{})
	attemptBDone := make(chan error, 1)
	go func() {
		close(attemptBStarted)
		attemptBDone <- d.Stop(context.Background())
	}()
	awaitLifecycleSignal(t, attemptBStarted, "attempt B invocation")
	select {
	case err := <-attemptBDone:
		t.Fatalf("attempt B advanced while attempt A retained its lease: %v", err)
	default:
	}
	d.lifecycleMu.Lock()
	ownerDuringA := d.lifecycleOwner
	terminalDuringA := d.stopGen.terminal
	d.lifecycleMu.Unlock()
	if ownerDuringA != ownerA || terminalDuringA {
		t.Fatal("attempt B entered drain or terminal publication before attempt A released ownership")
	}

	spawner.mu.Lock()
	delete(spawner.sessions, "straggler")
	spawner.mu.Unlock()
	close(releaseAttemptA)
	select {
	case err := <-attemptADone:
		var incomplete *DrainIncompleteError
		if !errors.As(err, &incomplete) {
			t.Fatalf("attempt A result = %v, want DrainIncompleteError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attempt A did not release")
	}
	select {
	case err := <-attemptBDone:
		if err != nil {
			t.Fatalf("attempt B after straggler release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attempt B did not complete terminal Stop")
	}

	thirdCtx, cancelThird := context.WithCancel(context.Background())
	cancelThird()
	if err := d.Stop(thirdCtx); err != nil {
		t.Fatalf("third Stop did not return immutable terminal success: %v", err)
	}
	if d.State() != StateStopped {
		t.Fatalf("State = %q, want stopped", d.State())
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Done remained open after terminal Stop")
	}
	d.lifecycleMu.Lock()
	terminalErr := d.stopGen.terminalErr
	d.lifecycleMu.Unlock()
	if terminalErr != nil {
		t.Fatalf("terminal Stop result mutated to %v, want nil", terminalErr)
	}
}

func TestLandingStopRetriesShareCompletionGeneration(t *testing.T) {
	landingEntered := make(chan struct{})
	releaseLanding := make(chan struct{})
	d := New(Options{
		OnLandingWork: func(context.Context, PollWorkItem) error {
			close(landingEntered)
			<-releaseLanding // deliberately ignores cancellation
			return nil
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	landingWorkDone := make(chan error, 1)
	go func() {
		landingWorkDone <- d.handlePollWorkItem(PollWorkItem{WorkType: LandingWorkType}, "")
	}()
	awaitLifecycleSignal(t, landingEntered, "landing callback")

	d.landingMu.Lock()
	generation := d.landingDone
	d.landingMu.Unlock()
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := d.Stop(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop retry %d = %v, want context canceled", i, err)
		}
		d.landingMu.Lock()
		sameGeneration := d.landingDone == generation
		d.landingMu.Unlock()
		if !sameGeneration {
			t.Fatalf("Stop retry %d replaced the active landing completion generation", i)
		}
	}

	close(releaseLanding)
	select {
	case err := <-landingWorkDone:
		if err != nil {
			t.Fatalf("landing callback: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("landing callback did not return")
	}
	select {
	case <-generation:
	default:
		t.Fatal("captured landing completion generation did not close")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after landing completion: %v", err)
	}
}
