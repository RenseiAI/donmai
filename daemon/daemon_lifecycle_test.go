package daemon

import (
	"context"
	"testing"
	"time"
)

func TestDaemonStop_CancelsAndJoinsEnteredLandingWork(t *testing.T) {
	entered := make(chan struct{})
	returned := make(chan struct{})
	d := New(Options{
		OnLandingWork: func(ctx context.Context, _ PollWorkItem) error {
			close(entered)
			<-ctx.Done()
			close(returned)
			return ctx.Err()
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)

	workDone := make(chan error, 1)
	go func() {
		workDone <- d.handlePollWorkItem(PollWorkItem{WorkType: LandingWorkType}, "")
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("landing handler did not enter")
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Stop returned before entered landing work completed")
	}
	select {
	case err := <-workDone:
		if err != nil {
			t.Fatalf("handlePollWorkItem: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("landing poll callback did not return")
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Done did not close after joined landing work")
	}
}

func TestDaemonStop_RejectsPostStopLandingWork(t *testing.T) {
	called := make(chan struct{})
	d := New(Options{
		OnLandingWork: func(context.Context, PollWorkItem) error {
			close(called)
			return nil
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.handlePollWorkItem(PollWorkItem{WorkType: LandingWorkType}, ""); err != nil {
		t.Fatalf("post-stop landing dispatch: %v", err)
	}
	select {
	case <-called:
		t.Fatal("landing handler entered after stop")
	default:
	}
}

func TestDaemonStopDeadlineBoundsLandingAndLaterStopFinishesGeneration(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	d := New(Options{
		OnLandingWork: func(context.Context, PollWorkItem) error {
			close(entered)
			<-release // deliberately ignores cancellation
			return nil
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	landingDone := make(chan error, 1)
	go func() { landingDone <- d.handlePollWorkItem(PollWorkItem{WorkType: LandingWorkType}, "") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("landing handler did not enter")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := d.Stop(ctx); err != context.DeadlineExceeded {
		t.Fatalf("first Stop = %v, want deadline exceeded", err)
	}
	if d.State() != StateDraining {
		t.Fatalf("state after deadline = %q, want draining", d.State())
	}
	select {
	case <-d.Done():
		t.Fatal("Done closed while landing callback remained live")
	default:
	}

	close(release)
	if err := <-landingDone; err != nil {
		t.Fatalf("landing handler: %v", err)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if d.State() != StateStopped {
		t.Fatalf("state = %q, want stopped", d.State())
	}
	select {
	case <-d.Done():
	default:
		t.Fatal("Done did not close after second Stop")
	}
}

func TestDaemonDrainResumeReopensAdmissionUnlessTerminalStopBegan(t *testing.T) {
	d := New(Options{})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if d.State() != StateDraining || d.spawner.IsAccepting() {
		t.Fatalf("manual drain state/admission = (%q, %v), want draining/false", d.State(), d.spawner.IsAccepting())
	}
	if !d.Resume() || d.State() != StateRunning || !d.spawner.IsAccepting() {
		t.Fatalf("Resume did not reopen manual drain: state=%q accepting=%v", d.State(), d.spawner.IsAccepting())
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if d.Resume() {
		t.Fatal("Resume succeeded after terminal stop began")
	}
}
