package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/interview"
)

func TestSpawner_ForceKillSignalFailureKeepsSession(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"sleep", "30"},
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "signal-fails", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	s.killProcessGroup = func(*exec.Cmd) error { return errors.New("permission denied") }
	if err := s.ForceKillSession("signal-fails"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ForceKillSession error = %v", err)
	}
	if got := s.ActiveCount(); got != 1 {
		t.Fatalf("failed signal removed session; ActiveCount=%d", got)
	}
	if !s.StopSession("signal-fails") {
		t.Fatal("cleanup StopSession failed")
	}
}

func TestSpawner_AcceptWork_ProjectAllowlist(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "agentfactory", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 4,
	})
	ended := sessionEnds(s)
	_, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/foo/bar", Ref: "main"})
	if err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	// Wait for terminal listener delivery and then registry release.
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
}

// TestSpawner_AcceptWork_MatchesByProjectID covers the case where the
// platform passes the Linear project slug as spec.Repository (e.g.
// "smoke-alpha") and the daemon allowlist entry has the GitHub repo URL
// in repository (e.g. ".../rensei-smokes-alpha") with the slug in id.
// The matcher must accept work by p.ID as well as p.Repository. (REN-NEW)
func TestSpawner_AcceptWork_MatchesByProjectID(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{
			ID:         "smoke-alpha",
			Repository: "https://github.com/foo/rensei-smokes-alpha",
		}},
		MaxConcurrentSessions: 1,
	})
	ended := sessionEnds(s)
	_, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "smoke-alpha", Ref: "main"})
	if err != nil {
		t.Fatalf("expected accept by project id (slug), got %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
}

func TestSpawner_RejectsUnknownProject(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/allowed/repo"}},
		MaxConcurrentSessions: 1,
	})
	_, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/disallowed/repo"})
	if err == nil {
		t.Fatal("expected rejection for non-allowlisted repo")
	}
}

func TestSpawner_ProjectAdmissionIndependentOfRepositories(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []string
		projects []ProjectConfig
		spec     SessionSpec
		wantErr  string
	}{
		{
			name:    "admitted project without repository accepts repository-free work",
			allowed: []string{"alpha"},
			spec:    SessionSpec{SessionID: "no-repo", ProjectID: "alpha"},
		},
		{
			name:    "repository must belong to admitted project",
			allowed: []string{"alpha"},
			projects: []ProjectConfig{
				{ID: "alpha", Repository: "example.com/acme/one"},
				{ID: "alpha", Repository: "example.com/acme/two"},
			},
			spec: SessionSpec{SessionID: "second-repo", ProjectID: "alpha", Repository: "example.com/acme/two"},
		},
		{
			name:     "repository cannot override project identity",
			allowed:  []string{"alpha", "beta"},
			projects: []ProjectConfig{{ID: "beta", Repository: "example.com/acme/beta"}},
			spec:     SessionSpec{SessionID: "mismatch", ProjectID: "alpha", Repository: "example.com/acme/beta"},
			wantErr:  "not configured for project",
		},
		{
			name:     "repository resource does not imply admission in v2",
			allowed:  []string{},
			projects: []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/alpha"}},
			spec:     SessionSpec{SessionID: "disabled", ProjectID: "alpha", Repository: "example.com/acme/alpha"},
			wantErr:  "is not allowed",
		},
		{
			name:    "stable repository id selects one repository explicitly",
			allowed: []string{"alpha"},
			projects: []ProjectConfig{
				{ID: "alpha", RepositoryID: "repo-one", Repository: "example.com/acme/one"},
				{ID: "alpha", RepositoryID: "repo-two", Repository: "example.com/acme/two"},
			},
			spec: SessionSpec{SessionID: "repo-id", ProjectID: "alpha", RepositoryID: "repo-two", RequiresRepository: true},
		},
		{
			name:    "repository id cannot override project identity",
			allowed: []string{"alpha", "beta"},
			projects: []ProjectConfig{
				{ID: "beta", RepositoryID: "repo-beta", Repository: "example.com/acme/beta"},
			},
			spec:    SessionSpec{SessionID: "repo-id-mismatch", ProjectID: "alpha", RepositoryID: "repo-beta", RequiresRepository: true},
			wantErr: "not configured for project",
		},
		{
			name:    "repository-required work selects configured primary",
			allowed: []string{"alpha"},
			projects: []ProjectConfig{
				{ID: "alpha", RepositoryID: "repo-primary", Repository: "example.com/acme/primary", Primary: true},
				{ID: "alpha", RepositoryID: "repo-secondary", Repository: "example.com/acme/secondary"},
			},
			spec: SessionSpec{SessionID: "primary", ProjectID: "alpha", RequiresRepository: true},
		},
		{
			name:    "repository-required work rejects ambiguous selection",
			allowed: []string{"alpha"},
			projects: []ProjectConfig{
				{ID: "alpha", RepositoryID: "repo-one", Repository: "example.com/acme/one"},
			},
			spec:    SessionSpec{SessionID: "no-primary", ProjectID: "alpha", RequiresRepository: true},
			wantErr: "explicit repository or configured primary",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewWorkerSpawner(SpawnerOptions{
				EnabledProjectIDs:     test.allowed,
				Projects:              test.projects,
				MaxConcurrentSessions: 1,
			})
			ended := sessionEnds(s)
			_, err := s.AcceptWork(test.spec)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("AcceptWork error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AcceptWork: %v", err)
			}
			waitSessionEnd(t, ended)
		})
	}
}

func TestSpawner_AddEnabledProjectIDs(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{EnabledProjectIDs: []string{}, MaxConcurrentSessions: 1})
	s.AddEnabledProjectIDs([]string{"satellite", "satellite"})
	if got := s.AllEnabledProjectIDs(); len(got) != 1 || got[0] != "satellite" {
		t.Fatalf("AllEnabledProjectIDs() = %v, want [satellite]", got)
	}
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "satellite-work", ProjectID: "satellite"}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)
}

func TestSpawner_CapacityEnforced(t *testing.T) {
	// Use a longer-running stub so we can exceed capacity deterministically.
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 1"},
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "1", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "2", Repository: "github.com/a/b"}); err == nil {
		t.Fatal("expected capacity rejection")
	}
}

func TestSpawner_SetMaxConcurrentSessions(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 1"},
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "1", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if err := s.SetMaxConcurrentSessions(2); err != nil {
		t.Fatalf("SetMaxConcurrentSessions: %v", err)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "2", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("second accept after scale up: %v", err)
	}
	if err := s.SetMaxConcurrentSessions(-1); err == nil {
		t.Fatal("expected negative capacity to fail")
	}
}

func TestSpawner_Drain_RespectsTimeout(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "long", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	start := time.Now()
	err := s.Drain(150 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error from Drain")
	}
	if time.Since(start) > time.Second {
		t.Errorf("Drain blocked too long: %s", time.Since(start))
	}
}

// TestSpawner_StopSession_RemovesSessionAndFreesSlot covers the R5
// per-session kill: StopSession(id) on a live session must return true,
// remove it from the active map (freeing the capacity slot so a new accept
// at the same cap succeeds), and emit SessionEventEnded — all without
// disturbing the spawner's accepting state.
func assertDrainStillBlocked(t *testing.T, done <-chan error, phase string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Drain returned %s: %v", phase, err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSpawner_DrainWaitsForPreSpawnReservation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(entered)
			<-release
			return nil, errors.New("pre-spawn stopped")
		},
	})

	acceptDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	<-entered

	drainDone := make(chan error, 1)
	go func() { drainDone <- s.Drain(time.Second) }()
	deadline := time.After(time.Second)
	for s.IsAccepting() {
		select {
		case <-deadline:
			t.Fatal("Drain did not stop new admissions")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	assertDrainStillBlocked(t, drainDone, "while OnPreSpawn was blocked")
	if _, err := s.AcceptWork(SessionSpec{SessionID: "rejected", Repository: "github.com/a/b"}); err == nil {
		t.Fatal("AcceptWork succeeded after Drain stopped admissions")
	}

	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("blocked AcceptWork unexpectedly succeeded")
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain after pre-spawn abort: %v", err)
	}
	s.mu.Lock()
	reservations := len(s.spawnReservations)
	s.mu.Unlock()
	if reservations != 0 {
		t.Fatalf("spawn reservations = %d, want 0", reservations)
	}
}

func TestSpawner_DrainWaitsForHookReturnToStartFailure(t *testing.T) {
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	aborted := make(chan struct{})
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn:            func(_ SessionSpec, env []string) ([]string, error) { return env, nil },
		OnSpawnAborted:        func(SessionSpec, error) { close(aborted) },
	})
	s.startCommand = func(*exec.Cmd) error {
		close(startEntered)
		<-releaseStart
		return errors.New("controlled start failure")
	}

	acceptDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "start-gap", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	<-startEntered
	drainDone := make(chan error, 1)
	go func() { drainDone <- s.Drain(time.Second) }()
	assertDrainStillBlocked(t, drainDone, "between hook return and start")

	close(releaseStart)
	if err := <-acceptDone; err == nil {
		t.Fatal("AcceptWork unexpectedly succeeded")
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("OnSpawnAborted did not run")
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain after start abort: %v", err)
	}
}

func TestSpawner_DrainWaitsForChildStartToRegistration(t *testing.T) {
	childStarted := make(chan struct{})
	releaseStart := make(chan struct{})
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"sleep", "30"},
	})
	s.startCommand = func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		close(childStarted)
		<-releaseStart
		return nil
	}

	acceptDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "registration-gap", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	<-childStarted
	drainDone := make(chan error, 1)
	go func() { drainDone <- s.Drain(time.Second) }()
	assertDrainStillBlocked(t, drainDone, "before the started child registered")

	close(releaseStart)
	if err := <-acceptDone; err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	if got := s.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount after registration = %d, want 1", got)
	}
	if !s.StopSession("registration-gap") {
		t.Fatal("StopSession did not remove registered child")
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain after session removal: %v", err)
	}
}

func TestSpawner_DrainWaitsForSpawnAbortCleanup(t *testing.T) {
	abortEntered := make(chan struct{})
	releaseAbort := make(chan struct{})
	var events atomic.Int32
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn:            func(_ SessionSpec, env []string) ([]string, error) { return env, nil },
		OnSpawnAborted: func(SessionSpec, error) {
			close(abortEntered)
			<-releaseAbort
		},
	})
	s.startCommand = func(*exec.Cmd) error { return errors.New("controlled start failure") }
	s.On(func(SessionEvent) { events.Add(1) })

	acceptDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "abort-cleanup", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	<-abortEntered
	drainDone := make(chan error, 1)
	go func() { drainDone <- s.Drain(time.Second) }()
	assertDrainStillBlocked(t, drainDone, "before OnSpawnAborted cleanup")
	close(releaseAbort)
	if err := <-acceptDone; err == nil {
		t.Fatal("AcceptWork unexpectedly succeeded")
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("Drain after abort cleanup: %v", err)
	}
	if got := events.Load(); got != 0 {
		t.Fatalf("lifecycle events = %d, want 0", got)
	}
}

func TestSpawner_ReservationsConsumeCapacityAndHookFailureReleases(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			if first {
				first = false
				close(entered)
				<-release
				return nil, errors.New("hook failure")
			}
			return env, nil
		},
	})
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		firstDone <- err
	}()
	<-entered
	if _, err := s.AcceptWork(SessionSpec{SessionID: "over-capacity", Repository: "github.com/a/b"}); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Fatalf("second AcceptWork error = %v, want capacity rejection", err)
	}
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("first AcceptWork unexpectedly succeeded")
	}
	if err := s.Drain(time.Second); err != nil {
		t.Fatalf("Drain after hook failure: %v", err)
	}
}

func TestSpawner_DrainContextReportsPendingReservation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			close(entered)
			<-release
			return nil, errors.New("pre-spawn stopped")
		},
	})
	acceptDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "reserved", Repository: "github.com/a/b"})
		acceptDone <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.DrainContext(ctx)
	var incomplete *DrainIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("DrainContext error = %v, want DrainIncompleteError", err)
	}
	if incomplete.SpawnReservations != 1 || incomplete.ActiveSessions != 0 {
		t.Fatalf("DrainIncompleteError = %+v, want one reservation and no sessions", incomplete)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DrainContext error = %v, want context.Canceled", err)
	}

	close(release)
	if err := <-acceptDone; err == nil {
		t.Fatal("AcceptWork unexpectedly succeeded")
	}
	if err := s.DrainContext(context.Background()); err != nil {
		t.Fatalf("fresh DrainContext after reservation release: %v", err)
	}
}

func TestSpawner_RejectsDuplicateSessionIDsAcrossReservationAndRegistration(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"sleep", "30"},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			close(entered)
			<-release
			return env, nil
		},
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	firstDone := make(chan error, 1)
	go func() {
		_, err := s.AcceptWork(SessionSpec{SessionID: "duplicate", Repository: "github.com/a/b"})
		firstDone <- err
	}()
	<-entered
	if _, err := s.AcceptWork(SessionSpec{SessionID: "duplicate", Repository: "github.com/a/b"}); err == nil || !strings.Contains(err.Error(), "already being started") {
		t.Fatalf("duplicate reservation error = %v, want already being started", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first AcceptWork: %v", err)
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "duplicate", Repository: "github.com/a/b"}); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("duplicate active error = %v, want already active", err)
	}
	if !s.StopSession("duplicate") {
		t.Fatal("StopSession did not remove accepted duplicate-id session")
	}
	if err := s.Drain(time.Second); err != nil {
		t.Fatalf("Drain after duplicate-id session stop: %v", err)
	}
}

func TestSpawner_ReapSessionDoesNotDeleteReplacement(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{})
	stale := &spawnedSession{handle: SessionHandle{SessionID: "same"}, spec: SessionSpec{SessionID: "same"}}
	survivor := &spawnedSession{handle: SessionHandle{SessionID: "same"}, spec: SessionSpec{SessionID: "same"}}
	s.mu.Lock()
	s.sessions["same"] = survivor
	s.mu.Unlock()

	if _, reaped := s.reapSession("same", stale, nil, false); reaped {
		t.Fatal("stale reaper removed a replacement session")
	}
	s.mu.Lock()
	got := s.sessions["same"]
	s.mu.Unlock()
	if got != survivor {
		t.Fatalf("session after stale reaper = %p, want surviving entry %p", got, survivor)
	}
}

func TestSpawner_PumpDrainLifecycle(t *testing.T) {
	const repository = "github.com/a/b"

	t.Run("running worker owns no drain grace", func(t *testing.T) {
		const sessionID = "ordinary-worker"
		clock := newFakePumpDrainClock()
		running := make(chan struct{})
		ended := make(chan SessionEvent, 1)
		var killCalls atomic.Int32
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: repository}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"sleep", "30"},
		})
		s.pumpDrainTimerFactory = clock.newTimer
		s.reaperRunning = func() { close(running) }
		s.killProcessGroup = func(cmd *exec.Cmd) error {
			killCalls.Add(1)
			return killSessionProcessGroup(cmd)
		}
		s.On(func(ev SessionEvent) {
			if ev.Kind == SessionEventEnded {
				ended <- ev
			}
		})
		t.Cleanup(func() {
			s.StopSession(sessionID)
			_ = s.Drain(time.Second)
		})

		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		waitSpawnerSignal(t, running, "running reaper state")
		clock.Advance(2 * pumpDrainGrace)

		if got := clock.TimerCount(); got != 0 {
			t.Fatalf("pump drain timers while worker is running = %d, want 0", got)
		}
		if got := killCalls.Load(); got != 0 {
			t.Fatalf("process-group KILL calls while worker is running = %d, want 0", got)
		}
		s.mu.Lock()
		ss := s.sessions[sessionID]
		var (
			state         SessionState
			stopRequested bool
		)
		if ss != nil {
			state = ss.handle.State
			stopRequested = ss.stopRequested
		}
		s.mu.Unlock()
		if ss == nil {
			t.Fatal("running worker was removed from the session registry")
		}
		if state != SessionRunning {
			t.Fatalf("running worker state = %q, want %q", state, SessionRunning)
		}
		if stopRequested {
			t.Fatal("running worker unexpectedly has stopRequested set")
		}
		select {
		case ev := <-ended:
			t.Fatalf("running worker emitted Ended event: %+v", ev)
		default:
		}

		if !s.StopSession(sessionID) {
			t.Fatal("StopSession cleanup = false")
		}
		cleanupEvent := waitSpawnerEvent(t, ended, "ordinary worker Ended event")
		if cleanupEvent.Handle.State != SessionTerminated {
			t.Fatalf("StopSession Ended state = %q, want %q", cleanupEvent.Handle.State, SessionTerminated)
		}
		waitForActiveCount(t, s, 0)
	})

	t.Run("simultaneous cancellation child exit and pump EOF grants termination grace", func(t *testing.T) {
		const sessionID = "simultaneous-cancellation"
		clock := newFakePumpDrainClock()
		reaperHeld := make(chan struct{})
		allowReaper := make(chan struct{})
		selectArmed := make(chan struct{})
		seamTimedOut := make(chan struct{}, 1)
		ended := make(chan SessionEvent, 1)
		allowEnded := make(chan struct{})
		listenerReturned := make(chan struct{})
		var (
			terminateCalls    atomic.Int32
			killCalls         atomic.Int32
			reaperOnce        sync.Once
			selectCalls       atomic.Int32
			endedOnce         sync.Once
			releaseReaperOnce sync.Once
			releaseEndedOnce  sync.Once
			waitMu            sync.Mutex
			waitDurations     []time.Duration
			graceSeen         bool
		)
		releaseReaper := func() { releaseReaperOnce.Do(func() { close(allowReaper) }) }
		releaseEnded := func() { releaseEndedOnce.Do(func() { close(allowEnded) }) }

		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: repository}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		})
		s.pumpDrainTimerFactory = clock.newTimer
		s.reaperRunning = func() {
			reaperOnce.Do(func() {
				close(reaperHeld)
				<-allowReaper
			})
		}
		// This models the legal select outcome where a ready waitResult or
		// pumpDone arm wins while the real context is already cancelled. The
		// local queues remain buffered while the reaper is held, so their
		// readiness is stable when this seam returns a nil context arm.
		s.runningPhaseContextDone = func(ctx context.Context, waitResult <-chan error, pumpDone <-chan struct{}) <-chan struct{} {
			if selectCalls.Add(1) != 1 {
				return ctx.Done()
			}
			deadline := time.NewTimer(spawnerWaitTimeout)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil && len(waitResult) == 1 && len(pumpDone) == 2 {
					close(selectArmed)
					return nil
				}
				select {
				case <-deadline.C:
					seamTimedOut <- struct{}{}
					return ctx.Done()
				case <-ticker.C:
				}
			}
		}
		s.terminateProcessGroup = func(*exec.Cmd) error {
			terminateCalls.Add(1)
			return nil
		}
		s.killProcessGroup = func(*exec.Cmd) error {
			killCalls.Add(1)
			return nil
		}
		s.waitProcessGroup = func(_ *exec.Cmd, duration time.Duration) processGroupWaitResult {
			waitMu.Lock()
			defer waitMu.Unlock()
			waitDurations = append(waitDurations, duration)
			if duration == sessionTerminationGrace {
				graceSeen = true
				return processGroupGone
			}
			if !graceSeen {
				return processGroupTimedOut
			}
			return processGroupGone
		}
		s.On(func(ev SessionEvent) {
			if ev.Kind != SessionEventEnded {
				return
			}
			endedOnce.Do(func() {
				ended <- ev
				<-allowEnded
				close(listenerReturned)
			})
		})
		t.Cleanup(func() {
			releaseReaper()
			releaseEnded()
			_ = s.StopSession(sessionID)
			if err := s.Drain(time.Second); err != nil {
				t.Errorf("Drain cleanup: %v", err)
			}
		})

		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		waitSpawnerSignal(t, reaperHeld, "reaper before running-phase select")
		if !s.StopSession(sessionID) {
			t.Fatal("StopSession = false")
		}
		if got := terminateCalls.Load(); got != 1 {
			t.Fatalf("SIGTERM requests while reaper is held = %d, want 1", got)
		}
		releaseReaper()
		select {
		case <-selectArmed:
		case <-seamTimedOut:
			t.Fatal("running-phase seam did not observe cancelled context and queued completions")
		case <-time.After(spawnerWaitTimeout):
			t.Fatalf("timed out after %v waiting for running-phase seam", spawnerWaitTimeout)
		}

		ev := waitSpawnerEvent(t, ended, "simultaneous-cancellation Ended event")
		waitMu.Lock()
		gotWaits := append([]time.Duration(nil), waitDurations...)
		waitMu.Unlock()
		if len(gotWaits) == 0 {
			t.Fatal("process-group wait was not called")
		}
		if got := gotWaits[0]; got != sessionTerminationGrace {
			t.Fatalf("first process-group wait = %v, want termination grace %v (never zero-duration probe)", got, sessionTerminationGrace)
		}
		if got := killCalls.Load(); got != 0 {
			t.Fatalf("process-group KILL calls after cooperative TERM flush = %d, want 0", got)
		}
		if got := clock.TimerCount(); got != 0 {
			t.Fatalf("pump drain timers with both EOF notifications queued = %d, want 0", got)
		}
		if ev.Handle.State != SessionTerminated {
			t.Fatalf("Ended state = %q, want %q", ev.Handle.State, SessionTerminated)
		}
		if got := s.ActiveCount(); got != 1 {
			t.Fatalf("active sessions during synchronous Ended listener = %d, want 1", got)
		}
		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("same SessionID during Ended listener error = %v, want already active", err)
		}

		releaseEnded()
		waitSpawnerSignal(t, listenerReturned, "Ended listener return")
		waitForActiveCount(t, s, 0)
		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("same SessionID after Ended listener return: %v", err)
		}
		waitForActiveCount(t, s, 0)
	})

	t.Run("terminal leader with inherited pipe is bounded and joined", func(t *testing.T) {
		const sessionID = "inherited-pipe"
		clock := newFakePumpDrainClock()
		pumpsJoined := make(chan struct{})
		ended := make(chan SessionEvent, 1)
		allowEnded := make(chan struct{})
		listenerReturned := make(chan struct{})
		var (
			killCalls  atomic.Int32
			joinedOnce sync.Once
			endedOnce  sync.Once
			release    sync.Once
		)
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: repository}},
			MaxConcurrentSessions: 2,
			WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30 & exit 0"},
		})
		s.pumpDrainTimerFactory = clock.newTimer
		s.afterPumpsJoined = func() { joinedOnce.Do(func() { close(pumpsJoined) }) }
		s.killProcessGroup = func(cmd *exec.Cmd) error {
			killCalls.Add(1)
			return killSessionProcessGroup(cmd)
		}
		s.On(func(ev SessionEvent) {
			if ev.Kind != SessionEventEnded {
				return
			}
			endedOnce.Do(func() {
				ended <- ev
				<-allowEnded
				close(listenerReturned)
			})
		})
		releaseEnded := func() { release.Do(func() { close(allowEnded) }) }
		t.Cleanup(func() {
			releaseEnded()
			s.StopSession(sessionID)
			_ = s.Drain(time.Second)
		})

		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		timer := waitPumpDrainTimer(t, clock)
		if timer.duration != pumpDrainGrace {
			t.Fatalf("pump drain timer duration = %v, want %v", timer.duration, pumpDrainGrace)
		}
		if got := s.ActiveCount(); got != 1 {
			t.Fatalf("active sessions before pump deadline = %d, want 1", got)
		}
		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("same SessionID before pump deadline error = %v, want already active", err)
		}
		if got := killCalls.Load(); got != 0 {
			t.Fatalf("process-group KILL calls before pump deadline = %d, want 0", got)
		}
		select {
		case ev := <-ended:
			t.Fatalf("terminal leader emitted Ended before pump deadline: %+v", ev)
		default:
		}

		clock.Advance(2 * pumpDrainGrace)
		ev := waitSpawnerEvent(t, ended, "bounded inherited-pipe Ended event")
		select {
		case <-pumpsJoined:
		default:
			t.Fatal("Ended event arrived before both pump completions")
		}
		if got := killCalls.Load(); got != 1 {
			t.Fatalf("remaining-group KILL calls = %d, want 1", got)
		}
		if ev.Handle.State != SessionCompleted {
			t.Fatalf("Ended state = %q, want %q", ev.Handle.State, SessionCompleted)
		}
		if got := s.ActiveCount(); got != 1 {
			t.Fatalf("active sessions during synchronous Ended listener = %d, want 1", got)
		}
		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("same SessionID during Ended listener error = %v, want already active", err)
		}

		releaseEnded()
		waitSpawnerSignal(t, listenerReturned, "Ended listener return")
		waitForActiveCount(t, s, 0)
		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("same SessionID after Ended listener return: %v", err)
		}
		if !s.StopSession(sessionID) {
			t.Fatal("StopSession replacement cleanup = false")
		}
		waitForActiveCount(t, s, 0)
	})

	t.Run("blocked output writer does not retain terminal session", func(t *testing.T) {
		const sessionID = "blocked-writer"
		clock := newFakePumpDrainClock()
		writer := &blockingPrefixWriter{
			entered:  make(chan struct{}),
			release:  make(chan struct{}),
			returned: make(chan struct{}),
		}
		pumpsJoined := make(chan struct{})
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: repository}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "printf blocked\\n"},
			StdoutPrefixWriter:    writer,
		})
		s.pumpDrainTimerFactory = clock.newTimer
		s.afterPumpsJoined = func() { close(pumpsJoined) }
		ended := make(chan SessionEvent, 1)
		s.On(func(ev SessionEvent) {
			if ev.Kind == SessionEventEnded {
				ended <- ev
			}
		})
		writerEntered := false
		t.Cleanup(func() {
			close(writer.release)
			if writerEntered {
				waitSpawnerSignal(t, writer.returned, "blocked output writer return")
			}
			if err := s.Drain(time.Second); err != nil {
				t.Errorf("Drain cleanup: %v", err)
			}
		})

		if _, err := s.AcceptWork(SessionSpec{SessionID: sessionID, Repository: repository}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		waitSpawnerSignal(t, writer.entered, "blocking output writer")
		writerEntered = true
		pumpTimer := waitPumpDrainTimer(t, clock)
		if pumpTimer.duration != pumpDrainGrace {
			t.Fatalf("pump drain timer duration = %v, want %v", pumpTimer.duration, pumpDrainGrace)
		}

		clock.Advance(2 * pumpDrainGrace)
		closeTimer := waitPumpDrainTimer(t, clock)
		if closeTimer.duration != pumpCloseJoinGrace {
			t.Fatalf("post-close pump join timer duration = %v, want %v", closeTimer.duration, pumpCloseJoinGrace)
		}
		clock.Advance(2 * pumpCloseJoinGrace)

		ev := waitSpawnerEvent(t, ended, "blocked-writer Ended event")
		if ev.Handle.State != SessionCompleted {
			t.Fatalf("Ended state = %q, want %q", ev.Handle.State, SessionCompleted)
		}
		waitForActiveCount(t, s, 0)
		select {
		case <-pumpsJoined:
			t.Fatal("blocked writer was treated as a joined output pump")
		default:
		}
		select {
		case <-writer.returned:
			t.Fatal("blocked writer returned before the test released it")
		default:
		}
	})
}

func TestSpawner_DrainContextReturnsNilWhenDeadlineRacesFinalRelease(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{})
	s.mu.Lock()
	s.sessions["last"] = &spawnedSession{handle: SessionHandle{SessionID: "last"}, spec: SessionSpec{SessionID: "last"}}
	s.mu.Unlock()
	s.drainBeforeContextSnapshot = func() {
		s.mu.Lock()
		delete(s.sessions, "last")
		s.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.DrainContext(ctx); err != nil {
		t.Fatalf("DrainContext after final release = %v, want nil", err)
	}
}

func TestSpawner_StopSession_ReleasesSlotOnlyAfterTerminalReap(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"sleep", "30"},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "victim", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if s.ActiveCount() != 1 {
		t.Fatalf("ActiveCount after accept = %d, want 1", s.ActiveCount())
	}
	// At capacity — a second accept must be rejected until the terminal owner
	// has reaped the complete process group and delivered Ended.
	if _, err := s.AcceptWork(SessionSpec{SessionID: "next", Repository: "github.com/a/b"}); err == nil {
		t.Fatal("expected capacity rejection before StopSession")
	}

	if !s.StopSession("victim") {
		t.Fatal("StopSession(victim) = false, want true for a live session")
	}
	if s.ActiveCount() != 1 {
		t.Fatalf("ActiveCount after StopSession = %d, want active terminal owner", s.ActiveCount())
	}
	if _, err := s.AcceptWork(SessionSpec{SessionID: "victim", Repository: "github.com/a/b"}); err == nil {
		t.Fatal("same ID admitted before terminal reap")
	}
	waitSessionEnd(t, ended)

	// Slot is free only after the synchronous terminal listener path returns.
	if _, err := s.AcceptWork(SessionSpec{SessionID: "next", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept after terminal reap: %v", err)
	}
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	// Spawner stays accepting (unlike Drain, StopSession does not pause).
	if !s.IsAccepting() {
		t.Error("StopSession must not flip the spawner out of accepting state")
	}
}

// TestSpawner_StopSession_UnknownSessionReturnsFalse pins the 404-equivalent
// contract: stopping an id that is not in flight is a no-op returning false.
func TestSpawner_StopSession_UnknownSessionReturnsFalse(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
	})
	if s.StopSession("does-not-exist") {
		t.Fatal("StopSession(unknown) = true, want false")
	}
}

// TestSpawner_StopSession_LeavesSiblingsRunning is the head-of-line-blocking
// isolation guarantee: killing one session must not touch the others.
func TestSpawner_StopSession_LeavesSiblingsRunning(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 3,
		WorkerCommand:         []string{"sleep", "30"},
	})
	ended := sessionEnds(s)
	t.Cleanup(func() { _ = s.Drain(time.Second) })
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.AcceptWork(SessionSpec{SessionID: id, Repository: "github.com/a/b"}); err != nil {
			t.Fatalf("accept %q: %v", id, err)
		}
	}
	if s.ActiveCount() != 3 {
		t.Fatalf("ActiveCount = %d, want 3", s.ActiveCount())
	}

	if !s.StopSession("b") {
		t.Fatal("StopSession(b) = false, want true")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 2)
	remaining := map[string]bool{}
	for _, h := range s.ActiveSessions() {
		remaining[h.SessionID] = true
	}
	if remaining["b"] {
		t.Error("stopped session b still active")
	}
	if !remaining["a"] || !remaining["c"] {
		t.Errorf("siblings should still be active, got %+v", remaining)
	}
}

func TestSpawner_PauseResume(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	s.Pause()
	if _, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/a/b"}); err == nil {
		t.Fatal("expected reject while paused")
	}
	s.Resume()
	if _, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/a/b"}); err != nil {
		t.Errorf("expected accept after resume, got %v", err)
	}
}

// TestSpawner_ActiveWorkareas_ProjectsLiveSessions covers the pull-based
// projection added in Wave 11 / S5: a successful AcceptWork must surface
// in ActiveWorkareas() with the spawned spec's repository/ref/sessionID
// and the resolved ProjectID from the allowlist.
func TestSpawner_ActiveWorkareas_ProjectsLiveSessions(t *testing.T) {
	const acceptedAt = "2026-05-07T12:34:56Z"
	parsedAccepted, err := time.Parse(time.RFC3339, acceptedAt)
	if err != nil {
		t.Fatalf("setup: parse fixed timestamp: %v", err)
	}
	now := func() time.Time { return parsedAccepted }
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{
			ID:         "smoke-alpha",
			Repository: "https://github.com/foo/rensei-smokes-alpha",
		}},
		MaxConcurrentSessions: 2,
		// Long-running stub so the session stays in the active map
		// across the assertion window.
		WorkerCommand: []string{"/bin/sh", "-c", "sleep 30"},
		Now:           now,
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-active-1",
		Repository: "smoke-alpha",
		Ref:        "feat/x",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	if got := len(s.ActiveSessions()); got != 1 {
		t.Fatalf("ActiveSessions: want 1, got %d", got)
	}

	got := s.ActiveWorkareas()
	if len(got) != 1 {
		t.Fatalf("ActiveWorkareas: want 1 entry, got %d (%+v)", len(got), got)
	}
	wa := got[0]
	if wa.Kind != afclient.WorkareaKindActive {
		t.Errorf("Kind: want %q, got %q", afclient.WorkareaKindActive, wa.Kind)
	}
	if wa.Status != afclient.WorkareaStatusReady {
		t.Errorf("Status: want %q, got %q", afclient.WorkareaStatusReady, wa.Status)
	}
	if wa.ID != "sess-active-1" {
		t.Errorf("ID: want session id, got %q", wa.ID)
	}
	if wa.SessionID != "sess-active-1" {
		t.Errorf("SessionID: want %q, got %q", "sess-active-1", wa.SessionID)
	}
	if wa.Repository != "smoke-alpha" {
		t.Errorf("Repository: want %q, got %q", "smoke-alpha", wa.Repository)
	}
	if wa.Ref != "feat/x" {
		t.Errorf("Ref: want %q, got %q", "feat/x", wa.Ref)
	}
	if wa.ProjectID != "smoke-alpha" {
		t.Errorf("ProjectID: want %q, got %q", "smoke-alpha", wa.ProjectID)
	}
	if wa.AcquiredAt == nil || !wa.AcquiredAt.Equal(parsedAccepted) {
		t.Errorf("AcquiredAt: want %v, got %v", parsedAccepted, wa.AcquiredAt)
	}
}

// TestSpawner_ActiveWorkareas_EmptyWhenIdle pins the zero-value contract:
// no sessions in flight → empty (non-nil) slice.
func TestSpawner_ActiveWorkareas_EmptyWhenIdle(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	got := s.ActiveWorkareas()
	if got == nil {
		t.Fatal("ActiveWorkareas: want non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("ActiveWorkareas: want 0 entries on idle spawner, got %d (%+v)", len(got), got)
	}
}

// TestSpawner_ActiveWorkareas_DeterministicOrdering exercises the sort
// guarantee — multiple in-flight sessions must come back ordered by
// SessionID so test assertions remain stable across runs.
func TestSpawner_ActiveWorkareas_DeterministicOrdering(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	for _, id := range []string{"sess-zeta", "sess-alpha", "sess-mike"} {
		if _, err := s.AcceptWork(SessionSpec{
			SessionID: id, Repository: "github.com/a/b",
		}); err != nil {
			t.Fatalf("AcceptWork %q: %v", id, err)
		}
	}

	got := s.ActiveWorkareas()
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	want := []string{"sess-alpha", "sess-mike", "sess-zeta"}
	for i, w := range want {
		if got[i].SessionID != w {
			t.Errorf("entry %d: want %q, got %q", i, w, got[i].SessionID)
		}
	}
}

// blockingPrefixWriter models an external output sink that has accepted a line
// but cannot return. Its release channel lets the terminal lifecycle regression
// prove that the session is released before the sink is unblocked.
type blockingPrefixWriter struct {
	entered      chan struct{}
	release      chan struct{}
	returned     chan struct{}
	enteredOnce  sync.Once
	returnedOnce sync.Once
}

func (w *blockingPrefixWriter) WriteWorkerLine(string, string) {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	w.returnedOnce.Do(func() { close(w.returned) })
}

// fakePumpDrainClock drives per-spawner pump timers without wall-clock waits.
// It intentionally implements only the timer behavior the lifecycle owns.
type fakePumpDrainClock struct {
	mu      sync.Mutex
	now     time.Duration
	timers  []*fakePumpDrainTimer
	created chan *fakePumpDrainTimer
}

func newFakePumpDrainClock() *fakePumpDrainClock {
	return &fakePumpDrainClock{created: make(chan *fakePumpDrainTimer, 4)}
}

func (c *fakePumpDrainClock) newTimer(duration time.Duration) pumpDrainTimer {
	c.mu.Lock()
	timer := &fakePumpDrainTimer{
		deadline: c.now + duration,
		duration: duration,
		ch:       make(chan time.Time, 1),
	}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.created <- timer
	return timer
}

func (c *fakePumpDrainClock) Advance(elapsed time.Duration) {
	c.mu.Lock()
	c.now += elapsed
	now := c.now
	timers := append([]*fakePumpDrainTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.fireAt(now)
	}
}

func (c *fakePumpDrainClock) TimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

type fakePumpDrainTimer struct {
	mu       sync.Mutex
	deadline time.Duration
	duration time.Duration
	ch       chan time.Time
	stopped  bool
	fired    bool
}

func (t *fakePumpDrainTimer) C() <-chan time.Time { return t.ch }

func (t *fakePumpDrainTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *fakePumpDrainTimer) fireAt(now time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired || now < t.deadline {
		return
	}
	t.fired = true
	t.ch <- time.Unix(0, int64(now))
}

func waitPumpDrainTimer(t *testing.T, clock *fakePumpDrainClock) *fakePumpDrainTimer {
	t.Helper()
	timer := time.NewTimer(spawnerWaitTimeout)
	defer timer.Stop()
	select {
	case pumpTimer := <-clock.created:
		return pumpTimer
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for pump drain timer", spawnerWaitTimeout)
		return nil
	}
}

func waitSpawnerSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	timer := time.NewTimer(spawnerWaitTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for %s", spawnerWaitTimeout, what)
	}
}

func waitSpawnerEvent(t *testing.T, events <-chan SessionEvent, what string) SessionEvent {
	t.Helper()
	timer := time.NewTimer(spawnerWaitTimeout)
	defer timer.Stop()
	select {
	case event := <-events:
		return event
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for %s", spawnerWaitTimeout, what)
		return SessionEvent{}
	}
}

// spawnerWaitTimeout is the liveness backstop for the event-driven wait
// helpers below. It is NOT a pacing knob: green runs return the moment
// the awaited event fires, so a generous value costs nothing while
// absorbing arbitrary CI -race scheduler load (the old 2s deadlines
// flaked at ~2.02s under load).
const spawnerWaitTimeout = 30 * time.Second

// captureWriter is a PrefixedWriter that accumulates child stdout lines
// for assertion. Tests use it together with a /bin/sh worker that prints
// env entries so we can verify the env actually exec'd by the child.
// Construct via newCaptureWriter so waiters can block on the write
// signal instead of polling.
type captureWriter struct {
	mu     sync.Mutex
	lines  []string
	notify chan struct{}
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{notify: make(chan struct{}, 1)}
}

func (c *captureWriter) WriteWorkerLine(_, line string) {
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
	// Coalescing wake-up: a buffered token is enough — the waiter
	// re-snapshots after every wake, so concurrent writes can't be lost.
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *captureWriter) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// waitForLine blocks until a captured stdout line contains substr, then
// returns the snapshot for further checks. It wakes on the capture
// writer's write signal (no poll interval) and gives up only after the
// spawnerWaitTimeout liveness backstop, returning whatever was captured
// so the caller's assertion produces a useful failure message.
func waitForLine(t *testing.T, capability *captureWriter, substr string) []string {
	t.Helper()
	timer := time.NewTimer(spawnerWaitTimeout)
	defer timer.Stop()
	for {
		snap := capability.snapshot()
		for _, l := range snap {
			if strings.Contains(l, substr) {
				return snap
			}
		}
		select {
		case <-capability.notify:
		case <-timer.C:
			return capability.snapshot()
		}
	}
}

// sessionEnds subscribes to the spawner's lifecycle stream and returns a
// channel that receives once per SessionEventEnded. Subscribe BEFORE the
// AcceptWork that spawns the session — the stub child exits fast, so a
// late subscription would miss the event.
func sessionEnds(s *WorkerSpawner) <-chan struct{} {
	ch := make(chan struct{}, 16)
	s.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	})
	return ch
}

// waitSessionEnd blocks until one SessionEventEnded arrives on ch. Terminal
// listeners run before the reaper releases the session, so callers that need
// the post-release registry state must wait for it explicitly.
func waitSessionEnd(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(spawnerWaitTimeout):
		t.Fatalf("timed out after %v waiting for session end", spawnerWaitTimeout)
	}
}

func waitForActiveCount(t *testing.T, s *WorkerSpawner, want int) {
	t.Helper()
	deadline := time.NewTimer(spawnerWaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := s.ActiveCount(); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("ActiveCount = %d after %v, want %d", s.ActiveCount(), spawnerWaitTimeout, want)
		case <-ticker.C:
		}
	}
}

// TestSpawner_OnPreSpawn_Invoked verifies the hook fires exactly once per
// spawn and receives the SessionSpec the daemon was asked to spawn.
func TestSpawner_OnPreSpawn_Invoked(t *testing.T) {
	var (
		calls    int32
		gotSpec  SessionSpec
		gotEnv   []string
		recordMu sync.Mutex
	)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
			atomic.AddInt32(&calls, 1)
			recordMu.Lock()
			gotSpec = spec
			gotEnv = append([]string(nil), env...)
			recordMu.Unlock()
			return nil, nil
		},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-pre-1",
		Repository: "github.com/a/b",
		Ref:        "main",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("OnPreSpawn call count: want 1, got %d", got)
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	if gotSpec.SessionID != "sess-pre-1" {
		t.Errorf("OnPreSpawn spec.SessionID: want %q, got %q", "sess-pre-1", gotSpec.SessionID)
	}
	// The hook must run AFTER composeEnv so it sees the daemon's own
	// per-session injections (DONMAI_SESSION_ID etc).
	found := false
	for _, kv := range gotEnv {
		if kv == "DONMAI_SESSION_ID=sess-pre-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OnPreSpawn env: missing post-composeEnv DONMAI_SESSION_ID; got %v", gotEnv)
	}
}

// TestSpawner_OnPreSpawn_SeesBaseEnv proves the hook receives the
// post-merge env — i.e., BaseEnv entries are visible before the hook
// runs, so callers can selectively override them.
func TestSpawner_OnPreSpawn_SeesBaseEnv(t *testing.T) {
	var (
		mu     sync.Mutex
		gotEnv []string
	)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"BASE_KEY": "base-value"},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			mu.Lock()
			gotEnv = append([]string(nil), env...)
			mu.Unlock()
			return nil, nil
		},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-base-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, kv := range gotEnv {
		if kv == "BASE_KEY=base-value" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OnPreSpawn did not see BaseEnv entry; got %v", gotEnv)
	}
}

// TestSpawner_OnPreSpawn_ReturnedEnvIsExeced verifies that the slice the
// hook returns is what reaches the child process. A sentinel key inserted
// by the hook is echoed by the worker stub and surfaces via the prefix
// writer.
func TestSpawner_OnPreSpawn_ReturnedEnvIsExeced(t *testing.T) {
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'sentinel=%s\n' "$ONPRESPAWN_SENTINEL"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			return append(env, "ONPRESPAWN_SENTINEL=hello-from-hook"), nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-exec-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "sentinel=")
	want := "sentinel=hello-from-hook"
	found := false
	for _, l := range lines {
		if strings.Contains(l, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("child env did not include hook-supplied sentinel; got lines %v", lines)
	}
}

// TestSpawner_OnPreSpawn_OverridesBaseEnv proves the hook can override an
// existing BaseEnv key. The exec'd env contains the LAST occurrence of a
// given key per Go's exec semantics, so appending an override after the
// input slice is the documented way for callers to win against BaseEnv.
func TestSpawner_OnPreSpawn_OverridesBaseEnv(t *testing.T) {
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"OVERRIDE_ME": "base-value"},
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'override=%s\n' "$OVERRIDE_ME"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			return append(env, "OVERRIDE_ME=hook-wins"), nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-override-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "override=")
	want := "override=hook-wins"
	found := false
	for _, l := range lines {
		if strings.Contains(l, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hook did not override BaseEnv key; got lines %v", lines)
	}
}

// TestSpawner_OnPreSpawn_NilReturnUsesInput verifies that a hook returning
// (nil, nil) is a no-op — the env composeEnv produced is what reaches the child.
func TestSpawner_OnPreSpawn_NilReturnUsesInput(t *testing.T) {
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"BASE_KEY": "base-value"},
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'base=%s\n' "$BASE_KEY"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, _ []string) ([]string, error) {
			return nil, nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-nilret-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "base=")
	want := "base=base-value"
	found := false
	for _, l := range lines {
		if strings.Contains(l, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("nil-return hook should leave env unchanged; got lines %v", lines)
	}
}

// TestSpawner_OnPreSpawn_NilHookNoPanic exercises the zero-value path:
// SpawnerOptions with OnPreSpawn unset must spawn normally without
// panicking and without altering the env contract.
func TestSpawner_OnPreSpawn_NilHookNoPanic(t *testing.T) {
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"BASE_KEY": "base-value"},
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'nohook=%s\n' "$BASE_KEY"; exit 0`},
		StdoutPrefixWriter:    capability,
		// OnPreSpawn intentionally left nil.
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-nohook-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "nohook=")
	want := "nohook=base-value"
	found := false
	for _, l := range lines {
		if strings.Contains(l, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("nil hook should preserve BaseEnv; got lines %v", lines)
	}
}

// TestSpawner_OnPreSpawn_ErrorAbortSpawn verifies the fail-closed path: when
// the hook returns a non-nil error, AcceptWork must fail with that error and
// the child process must never be started (no lifecycle events, active count
// stays at zero). This is the credential-injection gate-failure path for
// byok/metered/shared sessions where the platform snapshot returns 403.
func TestSpawner_OnPreSpawn_ErrorAbortSpawn(t *testing.T) {
	var started int32
	spawnErr := errors.New("credential gate failed: METERED_NOT_ENTITLED")
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 10"},
		OnPreSpawn: func(_ SessionSpec, _ []string) ([]string, error) {
			return nil, spawnErr
		},
	})
	s.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventStarted {
			atomic.AddInt32(&started, 1)
		}
	})

	_, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-gate-fail",
		Repository: "github.com/a/b",
	})
	if err == nil {
		t.Fatal("AcceptWork: expected error from failing OnPreSpawn hook, got nil")
	}
	if !strings.Contains(err.Error(), "METERED_NOT_ENTITLED") {
		t.Errorf("AcceptWork error: want to contain 'METERED_NOT_ENTITLED', got %q", err.Error())
	}
	// Allow a brief window for any async spawn that should NOT have happened.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&started); n != 0 {
		t.Errorf("started count: want 0 (no child process), got %d", n)
	}
	if n := s.ActiveCount(); n != 0 {
		t.Errorf("active count: want 0 after fail-closed spawn, got %d", n)
	}
}

// TestSpawner_OnPreSpawn_ErrorDoesNotConsumeCapacity verifies that a hook
// error releases the session slot: after a fail-closed abort, a subsequent
// AcceptWork on a max-1 spawner must succeed.
func TestSpawner_OnPreSpawn_ErrorDoesNotConsumeCapacity(t *testing.T) {
	firstCall := true
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'ok\n'; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			if firstCall {
				firstCall = false
				return nil, errors.New("transient gate failure")
			}
			return env, nil
		},
	})

	// First spawn must fail-closed.
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-fail",
		Repository: "github.com/a/b",
	}); err == nil {
		t.Fatal("first AcceptWork: expected error, got nil")
	}

	// Second spawn on the same spawner (same max-1 capacity) must succeed
	// because the first never consumed a slot.
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-ok",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("second AcceptWork after fail-closed: %v", err)
	}
	waitForLine(t, capability, "ok")
}

// TestSpawner_OnPreSpawn_EnvMergeAndFailClosed exercises the composite path:
// a hook that merges credential env entries on success and fails-closed on
// a gate error. Asserts (a) merged env reaches the child on success and
// (b) AcceptWork errors without spawning on gate failure.
func TestSpawner_OnPreSpawn_EnvMergeAndFailClosed(t *testing.T) {
	const (
		sessionOK   = "sess-merge-ok"
		sessionFail = "sess-merge-fail"
	)
	type call struct {
		sessionID  string
		shouldFail bool
	}
	calls := []call{
		{sessionID: sessionOK, shouldFail: false},
		{sessionID: sessionFail, shouldFail: true},
	}
	capability := newCaptureWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'apikey=%s\n' "$ANTHROPIC_API_KEY"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(spec SessionSpec, env []string) ([]string, error) {
			// Fail-closed for the designated failure session.
			if spec.SessionID == sessionFail {
				return nil, errors.New("credential gate failed: SHARED_QUOTA_EXCEEDED")
			}
			// Merge the model key for the success session.
			return append(env, "ANTHROPIC_API_KEY=sk-ant-mock"), nil
		},
	})

	for _, c := range calls {
		_, err := s.AcceptWork(SessionSpec{
			SessionID:  c.sessionID,
			Repository: "github.com/a/b",
		})
		if c.shouldFail {
			if err == nil {
				t.Errorf("%s: expected fail-closed error, got nil", c.sessionID)
			} else if !strings.Contains(err.Error(), "SHARED_QUOTA_EXCEEDED") {
				t.Errorf("%s: expected gate error, got %v", c.sessionID, err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: expected success, got %v", c.sessionID, err)
			}
		}
	}
	// Assert the model key reached the child for the success session.
	lines := waitForLine(t, capability, "apikey=")
	found := false
	for _, l := range lines {
		if strings.Contains(l, "apikey=sk-ant-mock") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ANTHROPIC_API_KEY not merged into child env; got lines %v", lines)
	}
}

func TestSpawner_OnSpawnAborted_StartFailureOwnsRollback(t *testing.T) {
	missingWorker := filepath.Join(t.TempDir(), "missing-worker")
	var (
		order       []string
		abortCalls  int
		abortedSpec SessionSpec
		abortedErr  error
		events      int
	)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{missingWorker},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			order = append(order, "pre-spawn")
			return env, nil
		},
		OnSpawnAborted: func(spec SessionSpec, err error) {
			order = append(order, "spawn-aborted")
			abortCalls++
			abortedSpec = spec
			abortedErr = err
		},
	})
	s.On(func(SessionEvent) { events++ })

	_, err := s.AcceptWork(SessionSpec{SessionID: "start-fails", Repository: "github.com/a/b"})
	order = append(order, "accept-returned")
	if err == nil {
		t.Fatal("AcceptWork: expected start failure, got nil")
	}
	if abortCalls != 1 {
		t.Fatalf("OnSpawnAborted calls = %d, want 1", abortCalls)
	}
	if abortedSpec.SessionID != "start-fails" {
		t.Errorf("OnSpawnAborted SessionID = %q, want start-fails", abortedSpec.SessionID)
	}
	if abortedErr != err {
		t.Fatalf("OnSpawnAborted error = %v, want the exact returned error %v", abortedErr, err)
	}
	if !strings.HasPrefix(err.Error(), "start worker: ") {
		t.Errorf("returned error = %q, want wrapped start worker error", err)
	}
	wantOrder := []string{"pre-spawn", "spawn-aborted", "accept-returned"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Errorf("ordering = %v, want %v", order, wantOrder)
	}
	if events != 0 {
		t.Errorf("lifecycle events = %d, want 0 for a process that never started", events)
	}
	if got := s.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount = %d, want 0", got)
	}
}

func TestSpawner_OnSpawnAborted_PreSpawnFailureRetainsOwnership(t *testing.T) {
	preSpawnErr := errors.New("pre-spawn refused")
	var abortCalls int
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{filepath.Join(t.TempDir(), "missing-worker")},
		OnPreSpawn: func(SessionSpec, []string) ([]string, error) {
			return nil, preSpawnErr
		},
		OnSpawnAborted: func(SessionSpec, error) {
			abortCalls++
		},
	})

	_, err := s.AcceptWork(SessionSpec{SessionID: "pre-fails", Repository: "github.com/a/b"})
	if !errors.Is(err, preSpawnErr) {
		t.Fatalf("AcceptWork error = %v, want wrapped pre-spawn error", err)
	}
	if abortCalls != 0 {
		t.Errorf("OnSpawnAborted calls = %d, want 0 when OnPreSpawn retained ownership", abortCalls)
	}
}

func TestSpawner_OnSpawnAborted_SuccessTransfersToSessionEnded(t *testing.T) {
	var abortCalls atomic.Int32
	var endedCalls atomic.Int32
	ended := make(chan struct{}, 1)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			return env, nil
		},
		OnSpawnAborted: func(SessionSpec, error) {
			abortCalls.Add(1)
		},
	})
	s.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			endedCalls.Add(1)
			ended <- struct{}{}
		}
	})

	if _, err := s.AcceptWork(SessionSpec{SessionID: "starts", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)
	if got := abortCalls.Load(); got != 0 {
		t.Errorf("OnSpawnAborted calls = %d, want 0 after successful start", got)
	}
	if got := endedCalls.Load(); got != 1 {
		t.Errorf("SessionEventEnded calls = %d, want 1", got)
	}
}

func TestSpawner_OnSpawnAborted_NilHooks(t *testing.T) {
	t.Run("nil abort hook", func(t *testing.T) {
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{filepath.Join(t.TempDir(), "missing-worker")},
			OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
				return env, nil
			},
		})
		if _, err := s.AcceptWork(SessionSpec{SessionID: "nil-abort", Repository: "github.com/a/b"}); err == nil {
			t.Fatal("AcceptWork: expected start failure with nil abort hook")
		}
	})

	t.Run("nil pre-spawn hook acquires no cleanup ownership", func(t *testing.T) {
		var abortCalls int
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{filepath.Join(t.TempDir(), "missing-worker")},
			OnSpawnAborted: func(SessionSpec, error) {
				abortCalls++
			},
		})
		if _, err := s.AcceptWork(SessionSpec{SessionID: "nil-pre", Repository: "github.com/a/b"}); err == nil {
			t.Fatal("AcceptWork: expected start failure with nil pre-spawn hook")
		}
		if abortCalls != 0 {
			t.Errorf("OnSpawnAborted calls = %d, want 0 without OnPreSpawn ownership", abortCalls)
		}
	})
}

func TestSpawner_OnSpawnAborted_ConcurrentFailuresExactlyOnce(t *testing.T) {
	const attempts = 32
	missingWorker := filepath.Join(t.TempDir(), "missing-worker")
	calls := make(map[string]int, attempts)
	var callsMu sync.Mutex
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: attempts,
		WorkerCommand:         []string{missingWorker},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			return env, nil
		},
		OnSpawnAborted: func(spec SessionSpec, _ error) {
			callsMu.Lock()
			calls[spec.SessionID]++
			callsMu.Unlock()
		},
	})

	errCh := make(chan error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%02d", i)
			_, err := s.AcceptWork(SessionSpec{SessionID: id, Repository: "github.com/a/b"})
			if err == nil {
				errCh <- fmt.Errorf("%s: AcceptWork unexpectedly succeeded", id)
				return
			}
			if !strings.HasPrefix(err.Error(), "start worker: ") {
				errCh <- fmt.Errorf("%s: error = %q", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	callsMu.Lock()
	defer callsMu.Unlock()
	if len(calls) != attempts {
		t.Fatalf("aborted session count = %d, want %d; calls=%v", len(calls), attempts, calls)
	}
	for id, count := range calls {
		if count != 1 {
			t.Errorf("OnSpawnAborted[%s] calls = %d, want exactly 1", id, count)
		}
	}
	if got := s.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount = %d, want 0", got)
	}
}

func TestSpawner_EmitsLifecycleEvents(t *testing.T) {
	var started, ended int32
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	endedCh := make(chan struct{}, 1)
	s.On(func(ev SessionEvent) {
		switch ev.Kind {
		case SessionEventStarted:
			atomic.AddInt32(&started, 1)
		case SessionEventEnded:
			atomic.AddInt32(&ended, 1)
			select {
			case endedCh <- struct{}{}:
			default:
			}
		}
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	waitSessionEnd(t, endedCh)
	if atomic.LoadInt32(&started) == 0 {
		t.Error("expected start event")
	}
	if atomic.LoadInt32(&ended) == 0 {
		t.Error("expected end event")
	}
}

// ── AddProjects tests ──────────────────────────────────────────────────────

// TestSpawner_AddProjects_AcceptsExtraRepo verifies that a repository
// rejected before AddProjects is called becomes accepted after AddProjects
// registers its ProjectConfig.
func TestSpawner_AddProjects_AcceptsExtraRepo(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}},
		MaxConcurrentSessions: 4,
	})

	// Satellite repo must be rejected before AddProjects.
	if _, err := s.AcceptWork(SessionSpec{SessionID: "pre-1", Repository: "github.com/org/satellite"}); err == nil {
		t.Fatal("expected rejection for satellite repo before AddProjects")
	}

	s.AddProjects([]ProjectConfig{{ID: "satellite", Repository: "github.com/org/satellite"}})

	// Satellite repo must be accepted after AddProjects.
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "post-1", Repository: "github.com/org/satellite"}); err != nil {
		t.Fatalf("expected accept after AddProjects, got %v", err)
	}
	waitSessionEnd(t, ended)
}

// TestSpawner_AddProjects_SetProjectsPreservesExtra is the key regression
// guard: a SetProjects call (e.g. yaml-watcher reload of the primary org's
// config) must NOT evict projects previously registered via AddProjects.
func TestSpawner_AddProjects_SetProjectsPreservesExtra(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}},
		MaxConcurrentSessions: 4,
	})

	s.AddProjects([]ProjectConfig{{ID: "satellite", Repository: "github.com/org/satellite"}})

	// Simulate a yaml-watcher reload that replaces the base set. The satellite
	// entry must survive because SetProjects only replaces opts.Projects.
	s.SetProjects([]ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}})

	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "after-reload", Repository: "github.com/org/satellite"}); err != nil {
		t.Fatalf("satellite repo rejected after SetProjects reload: %v", err)
	}
	waitSessionEnd(t, ended)
}

// TestSpawner_AddProjects_Dedup verifies that exact entries are idempotent
// while a second repository for the same project remains independently
// addressable.
func TestSpawner_AddProjects_Dedup(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}},
		MaxConcurrentSessions: 4,
	})

	dup := ProjectConfig{ID: "satellite", Repository: "github.com/org/satellite"}
	s.AddProjects([]ProjectConfig{dup})
	s.AddProjects([]ProjectConfig{dup})
	s.AddProjects([]ProjectConfig{dup})

	// Only one copy should exist in extraProjects; confirm by verifying the
	// spawner still accepts the repo (dedup does not reject valid entries).
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "dup-1", Repository: "github.com/org/satellite"}); err != nil {
		t.Fatalf("expected accept after dedup AddProjects, got %v", err)
	}
	waitSessionEnd(t, ended)

	// A different repository with the same ID is a valid one-to-many binding.
	s.AddProjects([]ProjectConfig{{ID: "satellite", Repository: "github.com/org/satellite-mirror"}})
	ended = sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "dup-2", Repository: "github.com/org/satellite-mirror"}); err != nil {
		t.Fatalf("second repository for project was rejected: %v", err)
	}
	waitSessionEnd(t, ended)
}

// TestSpawner_AddProjects_DedupByRepository verifies that an entry whose
// Repository already exists in the base set is not added to extraProjects.
func TestSpawner_AddProjects_DedupByRepository(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{
			{ID: "primary", Repository: "github.com/org/primary"},
		},
		MaxConcurrentSessions: 2,
	})

	// Attempt to add an entry that shares the same Repository as the base entry
	// but with a different ID.
	s.AddProjects([]ProjectConfig{{ID: "primary-alias", Repository: "github.com/org/primary"}})

	// The dedup-by-Repository path should have rejected the duplicate; there
	// should only be the base entry. The primary repo must still be accepted.
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "dedup-repo-1", Repository: "github.com/org/primary"}); err != nil {
		t.Fatalf("primary repo rejected after duplicate-repo AddProjects: %v", err)
	}
	waitSessionEnd(t, ended)
}

// TestSpawner_AddProjects_Concurrency exercises AddProjects and AcceptWork
// under the race detector by running them concurrently. Any data race on
// extraProjects or opts.Projects will surface here under `go test -race`.
func TestSpawner_AddProjects_Concurrency(_ *testing.T) {
	const numSatellites = 8
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}},
		MaxConcurrentSessions: numSatellites + 2,
	})

	var wg sync.WaitGroup

	// Goroutine 1: concurrently add satellite projects.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range numSatellites {
			s.AddProjects([]ProjectConfig{{
				ID:         fmt.Sprintf("sat-%d", i),
				Repository: fmt.Sprintf("github.com/org/satellite-%d", i),
			}})
		}
	}()

	// Goroutine 2: concurrently call SetProjects (yaml-watcher simulation).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 4 {
			s.SetProjects([]ProjectConfig{{ID: "primary", Repository: "github.com/org/primary"}})
		}
	}()

	// Goroutine 3: concurrently accept work on the primary repo.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 4 {
			_, _ = s.AcceptWork(SessionSpec{
				SessionID:  fmt.Sprintf("concurrent-%d", i),
				Repository: "github.com/org/primary",
			})
		}
	}()

	wg.Wait()
	// Drain so the test does not leave zombie /bin/sh stubs.
	_ = s.Drain(2 * time.Second)
}

// TestSpawner_ActiveInteractiveCount pins the interactive-occupancy split:
// PTY "interactive" and legacy "interview" modes count toward
// activeInteractive; headless and unknown modes remain excluded.
func TestSpawner_ActiveInteractiveCount(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 6,
		WorkerCommand:         []string{"sleep", "30"},
	})
	ended := sessionEnds(s)
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	if active, interactive := s.ActiveSessionCounts(); active != 0 || interactive != 0 {
		t.Fatalf("ActiveSessionCounts on empty spawner = (%d, %d), want (0, 0)", active, interactive)
	}

	cases := []struct {
		spec            SessionSpec
		wantActive      int
		wantInteractive int
	}{
		{spec: SessionSpec{SessionID: "headless", Repository: "github.com/a/b"}, wantActive: 1, wantInteractive: 0},
		{spec: SessionSpec{SessionID: "pty-1", Repository: "github.com/a/b", Mode: interactiveRunMode}, wantActive: 2, wantInteractive: 1},
		{spec: SessionSpec{SessionID: "interview", Repository: "github.com/a/b", Mode: interview.InterviewRunMode}, wantActive: 3, wantInteractive: 2},
		{spec: SessionSpec{SessionID: "unknown", Repository: "github.com/a/b", Mode: "interactive-preview"}, wantActive: 4, wantInteractive: 2},
		{spec: SessionSpec{SessionID: "pty-2", Repository: "github.com/a/b", Mode: interactiveRunMode}, wantActive: 5, wantInteractive: 3},
	}
	for _, tc := range cases {
		if _, err := s.AcceptWork(tc.spec); err != nil {
			t.Fatalf("accept %q: %v", tc.spec.SessionID, err)
		}
		active, interactive := s.ActiveSessionCounts()
		if active != tc.wantActive || interactive != tc.wantInteractive {
			t.Fatalf("ActiveSessionCounts after accepting %q = (%d, %d), want (%d, %d)",
				tc.spec.SessionID, active, interactive, tc.wantActive, tc.wantInteractive)
		}
	}

	active, interactive := s.ActiveSessionCounts()
	if got := s.ActiveCount(); got != active {
		t.Fatalf("ActiveCount = %d, snapshot active = %d", got, active)
	}
	if got := s.ActiveInteractiveCount(); got != interactive {
		t.Fatalf("ActiveInteractiveCount = %d, snapshot interactive = %d", got, interactive)
	}

	// Stopping a headless session changes only the unclassed total.
	if !s.StopSession("headless") {
		t.Fatal("StopSession(headless) = false, want true")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 4)
	active, interactive = s.ActiveSessionCounts()
	if active != 4 || interactive != 3 {
		t.Fatalf("ActiveSessionCounts after stopping headless session = (%d, %d), want (4, 3)", active, interactive)
	}

	// Stopping a legacy interview session changes both the total and interactive subset.
	if !s.StopSession("interview") {
		t.Fatal("StopSession(interview) = false, want true")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 3)
	active, interactive = s.ActiveSessionCounts()
	if active != 3 || interactive != 2 {
		t.Fatalf("ActiveSessionCounts after stopping interview session = (%d, %d), want (3, 2)", active, interactive)
	}

	// Stopping a PTY session also changes both values.
	if !s.StopSession("pty-1") {
		t.Fatal("StopSession(pty-1) = false, want true")
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 2)
	active, interactive = s.ActiveSessionCounts()
	if active != 2 || interactive != 1 {
		t.Fatalf("ActiveSessionCounts after stopping PTY session = (%d, %d), want (2, 1)", active, interactive)
	}
}

func TestSpawner_ActiveSessionCountsConcurrentLifecycle(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{})
	started := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		phase := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			clear(s.sessions)
			switch phase {
			case 1:
				s.sessions["headless"] = &spawnedSession{
					spec: SessionSpec{SessionID: "headless"},
				}
			case 2:
				s.sessions["interview"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interview", Mode: interview.InterviewRunMode},
				}
			case 3:
				s.sessions["interactive"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interactive", Mode: interactiveRunMode},
				}
			case 4:
				s.sessions["interactive"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interactive", Mode: interactiveRunMode},
				}
				s.sessions["interview"] = &spawnedSession{
					spec: SessionSpec{SessionID: "interview", Mode: interview.InterviewRunMode},
				}
				s.sessions["unknown"] = &spawnedSession{
					spec: SessionSpec{SessionID: "unknown", Mode: "interactive-preview"},
				}
			}
			phase = (phase + 1) % 5
			s.mu.Unlock()
		}
	}()
	<-started
	defer func() {
		close(stop)
		wg.Wait()
	}()

	valid := map[[2]int]bool{
		{0, 0}: true,
		{1, 0}: true,
		{1, 1}: true,
		{3, 2}: true,
	}
	for range 10_000 {
		active, interactive := s.ActiveSessionCounts()
		if interactive > active {
			t.Fatalf("interactive occupancy exceeds total: active=%d interactive=%d", active, interactive)
		}
		if !valid[[2]int{active, interactive}] {
			t.Fatalf("torn occupancy snapshot under concurrent lifecycle: active=%d interactive=%d", active, interactive)
		}
	}
}
