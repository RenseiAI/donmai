package daemon

import (
	"context"
	"errors"
	"os/exec"
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
	released, ok := s.sessionRelease(spec.SessionID)
	if !ok {
		t.Fatal("first generation release signal missing after admission")
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
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("first generation was not released after terminal listener returned")
	}
	if _, err := s.AcceptWork(spec); err != nil {
		t.Fatalf("same ID after terminal delivery: %v", err)
	}
	if err := s.DrainContext(context.Background()); err != nil {
		t.Fatalf("drain replacement: %v", err)
	}
}

func TestDaemonUpdateRefusesUnsettledDirectReservation(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Update(ctx)
	if !errors.Is(err, ErrRestartPreflightRefused) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Update error = %v, want restart-preflight deadline refusal", err)
	}
	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("reserved AcceptWork unexpectedly succeeded")
	}
}

func TestSpawner_TerminalListenerMakesGenerationUnsignalable(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
	})
	var termSignals atomic.Int32
	var killSignals atomic.Int32
	s.terminateProcessGroup = func(*exec.Cmd) error { termSignals.Add(1); return nil }
	s.killProcessGroup = func(*exec.Cmd) error { killSignals.Add(1); return nil }
	entered := make(chan struct{})
	release := make(chan struct{})
	s.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			close(entered)
			<-release
		}
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "terminal", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal listener did not enter")
	}
	if !s.StopSession("terminal") {
		t.Fatal("StopSession did not acknowledge a terminal generation")
	}
	if err := s.ForceKillSession("terminal"); err != nil {
		t.Fatalf("ForceKillSession terminal generation: %v", err)
	}
	if got := termSignals.Load(); got != 0 {
		t.Fatalf("TERM signals after terminal classification = %d, want 0", got)
	}
	if got := killSignals.Load(); got != 0 {
		t.Fatalf("SIGKILL signals after terminal classification = %d, want 0", got)
	}
	close(release)
	waitForActiveCount(t, s, 0)
	if s.StopSession("terminal") {
		t.Fatal("StopSession acknowledged a released terminal generation")
	}
}

func TestSpawner_PostWaitPermissionClassificationReleasesTerminalGeneration(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
	})
	var waits atomic.Int32
	var kills atomic.Int32
	s.waitProcessGroup = func(*exec.Cmd, time.Duration) processGroupWaitResult {
		waits.Add(1)
		return processGroupPermission
	}
	s.killProcessGroup = func(*exec.Cmd) error {
		kills.Add(1)
		return errSessionProcessExited
	}
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "permission-only", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	if got := waits.Load(); got < 2 {
		t.Fatalf("process-group classifications = %d, want initial and bounded post-Wait observations", got)
	}
	if got := kills.Load(); got != 1 {
		t.Fatalf("descendant cleanup signals = %d, want one", got)
	}
}

func TestDaemonDonePublishesStoppedState(t *testing.T) {
	d := New(Options{})
	d.spawner = NewWorkerSpawner(SpawnerOptions{})
	d.setState(StateRunning)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-d.Done():
		if got := d.State(); got != StateStopped {
			t.Fatalf("state observed after Done = %q, want stopped", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Done did not close")
	}
}
