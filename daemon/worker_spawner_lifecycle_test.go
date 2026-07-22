package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpawner_TerminalListenerOwnsIDUntilSynchronousDeliveryCompletes(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
	})
	listenerEntered := make(chan struct{})
	releaseListener := make(chan struct{})
	listenerReturned := make(chan struct{})
	var ended atomic.Int32
	s.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventEnded {
			return
		}
		if ended.Add(1) != 1 {
			return
		}
		close(listenerEntered)
		<-releaseListener
		close(listenerReturned)
	})

	spec := SessionSpec{SessionID: "same-id", Repository: "github.com/a/b"}
	if _, err := s.AcceptWork(spec); err != nil {
		t.Fatalf("first AcceptWork: %v", err)
	}
	select {
	case <-listenerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal listener did not enter")
	}
	if _, err := s.AcceptWork(spec); err == nil {
		t.Fatal("same ID admitted while terminal listener still owns its generation")
	}
	if got := s.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount during terminal listener = %d, want 1", got)
	}

	close(releaseListener)
	select {
	case <-listenerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal listener did not return")
	}
	if got := ended.Load(); got != 1 {
		t.Fatalf("ended event count for first generation = %d, want 1", got)
	}
	if _, err := s.AcceptWork(spec); err != nil {
		t.Fatalf("same ID after terminal delivery: %v", err)
	}
	if err := s.DrainContext(context.Background()); err != nil {
		t.Fatalf("drain replacement: %v", err)
	}
}

func TestDaemonUpdate_PropagatesIncompleteDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(entered)
			<-release
			return nil, errors.New("pre-spawn interrupted")
		},
	})
	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)
	d.mu.Lock()
	d.config = &Config{AutoUpdate: AutoUpdateConfig{DrainTimeoutSeconds: 0}}
	d.mu.Unlock()

	acceptDone := make(chan error, 1)
	go func() {
		_, err := spawner.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-spawn hook did not enter")
	}
	_, err := d.Update(context.Background())
	var incomplete *DrainIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("Update error = %v, want DrainIncompleteError", err)
	}
	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("reserved AcceptWork unexpectedly succeeded")
	}
}
