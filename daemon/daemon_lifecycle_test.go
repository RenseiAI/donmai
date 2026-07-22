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
