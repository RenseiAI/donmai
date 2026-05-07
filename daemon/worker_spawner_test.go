package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func TestSpawner_AcceptWork_ProjectAllowlist(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "agentfactory", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 4,
	})
	_, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/foo/bar", Ref: "main"})
	if err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	// Wait for the session to exit (stub exits quickly).
	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.ActiveCount() != 0 {
		t.Fatalf("expected sessions to drain, still %d active", s.ActiveCount())
	}
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
	_, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "smoke-alpha", Ref: "main"})
	if err != nil {
		t.Fatalf("expected accept by project id (slug), got %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.ActiveCount() != 0 {
		t.Fatalf("expected sessions to drain, still %d active", s.ActiveCount())
	}
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

func TestSpawner_EmitsLifecycleEvents(t *testing.T) {
	var started, ended int32
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	s.On(func(ev SessionEvent) {
		switch ev.Kind {
		case SessionEventStarted:
			atomic.AddInt32(&started, 1)
		case SessionEventEnded:
			atomic.AddInt32(&ended, 1)
		}
	})
	if _, err := s.AcceptWork(SessionSpec{SessionID: "s1", Repository: "github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&ended) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&started) == 0 {
		t.Error("expected start event")
	}
	if atomic.LoadInt32(&ended) == 0 {
		t.Error("expected end event")
	}
}
