package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalHeldOccupancyDeduplicatesOwnedAndReservedIdentities(t *testing.T) {
	t.Parallel()
	s := NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 3, ExternalSessionIDs: func() []string { return []string{"", "held", "held", "owned", "reserved"} }})
	s.mu.Lock()
	s.sessions["owned"] = &spawnedSession{}
	s.spawnReservations["reserved"] = struct{}{}
	count := s.externalSessionOccupancyLocked()
	s.mu.Unlock()
	if count != 1 {
		t.Fatalf("unresolved distinct occupancy=%d", count)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "new"}); err == nil || !strings.Contains(err.Error(), "at capacity (3/3") {
		t.Fatalf("held capacity not charged before admission: %v", err)
	}
}

func TestLocalHeldOccupancyDuringEndedListenerRetainsOneSlot(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var held []string
	sentinel := errors.New("owned probe stops before process creation")
	s := NewWorkerSpawner(SpawnerOptions{Projects: []ProjectConfig{{ID: "project", Repository: "https://github.com/example/project.git"}}, MaxConcurrentSessions: 2, WorkerCommand: []string{"/bin/sh", "-c", "exit 0"}, ExternalSessionIDs: func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), held...) }, OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
		if spec.SessionID == "probe" {
			return nil, sentinel
		}
		return env, nil
	}})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.On(func(event SessionEvent) {
		if event.Kind == SessionEventEnded && event.Spec.SessionID == "owned" {
			mu.Lock()
			held = []string{"owned", "owned"}
			mu.Unlock()
			close(entered)
			<-release
		}
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "owned", Repository: "https://github.com/example/project.git"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owned process did not reach ended listener")
	}
	// The callback released its own lock before reentering the spawner. The
	// original process remains owned until the synchronous listener returns.
	if _, err := s.AcceptWork(SessionSpec{SessionID: "probe", Repository: "https://github.com/example/project.git"}); !errors.Is(err, sentinel) {
		t.Fatalf("ended/held identity consumed two slots: %v", err)
	}
	released, owned := s.sessionRelease("owned")
	if !owned {
		t.Fatal("ended listener lost ownership early")
	}
	unblock()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("owned process not released")
	}
	if err := s.SetMaxConcurrentSessions(1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "probe", Repository: "https://github.com/example/project.git"}); err == nil || !strings.Contains(err.Error(), "at capacity (1/1") {
		t.Fatalf("unresolved identity lost occupancy after owner exit: %v", err)
	}
}
