package daemon

import (
	"strings"
	"sync"
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

// captureWriter is a PrefixedWriter that accumulates child stdout lines
// for assertion. Tests use it together with a /bin/sh worker that prints
// env entries so we can verify the env actually exec'd by the child.
type captureWriter struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureWriter) WriteWorkerLine(_, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *captureWriter) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// waitForLine polls the captured stdout buffer until `substr` appears or
// the deadline expires. Returns the captured snapshot for further checks.
func waitForLine(t *testing.T, capability *captureWriter, substr string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := capability.snapshot()
		for _, l := range snap {
			if strings.Contains(l, substr) {
				return snap
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return capability.snapshot()
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
		OnPreSpawn: func(spec SessionSpec, env []string) []string {
			atomic.AddInt32(&calls, 1)
			recordMu.Lock()
			gotSpec = spec
			gotEnv = append([]string(nil), env...)
			recordMu.Unlock()
			return nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-pre-1",
		Repository: "github.com/a/b",
		Ref:        "main",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("OnPreSpawn call count: want 1, got %d", got)
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	if gotSpec.SessionID != "sess-pre-1" {
		t.Errorf("OnPreSpawn spec.SessionID: want %q, got %q", "sess-pre-1", gotSpec.SessionID)
	}
	// The hook must run AFTER composeEnv so it sees the daemon's own
	// per-session injections (RENSEI_SESSION_ID etc).
	found := false
	for _, kv := range gotEnv {
		if kv == "RENSEI_SESSION_ID=sess-pre-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OnPreSpawn env: missing post-composeEnv RENSEI_SESSION_ID; got %v", gotEnv)
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
		OnPreSpawn: func(_ SessionSpec, env []string) []string {
			mu.Lock()
			gotEnv = append([]string(nil), env...)
			mu.Unlock()
			return nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-base-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
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
	capability := &captureWriter{}
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'sentinel=%s\n' "$ONPRESPAWN_SENTINEL"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, env []string) []string {
			return append(env, "ONPRESPAWN_SENTINEL=hello-from-hook")
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-exec-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "sentinel=", 2*time.Second)
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
	capability := &captureWriter{}
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"OVERRIDE_ME": "base-value"},
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'override=%s\n' "$OVERRIDE_ME"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, env []string) []string {
			return append(env, "OVERRIDE_ME=hook-wins")
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-override-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "override=", 2*time.Second)
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
// nil is a no-op — the env composeEnv produced is what reaches the child.
func TestSpawner_OnPreSpawn_NilReturnUsesInput(t *testing.T) {
	capability := &captureWriter{}
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"BASE_KEY": "base-value"},
		WorkerCommand:         []string{"/bin/sh", "-c", `printf 'base=%s\n' "$BASE_KEY"; exit 0`},
		StdoutPrefixWriter:    capability,
		OnPreSpawn: func(_ SessionSpec, _ []string) []string {
			return nil
		},
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-nilret-1",
		Repository: "github.com/a/b",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	lines := waitForLine(t, capability, "base=", 2*time.Second)
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
	capability := &captureWriter{}
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
	lines := waitForLine(t, capability, "nohook=", 2*time.Second)
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
