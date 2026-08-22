package daemon

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSpawner_HandleEnrichment verifies that AcceptWork populates the
// SessionHandle worktree-path / project / repository enrichment fields so
// GET /api/daemon/sessions is self-sufficient for a local reader. Uses a
// long-lived stub worker so the handle is observable via ActiveSessions.
func TestSpawner_HandleEnrichment(t *testing.T) {
	parent := t.TempDir()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "acme", Repository: "github.com/acme/web"}},
		MaxConcurrentSessions: 1,
		WorktreeParentDir:     parent,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 5"},
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "sess-123",
		Repository: "github.com/acme/web",
		Ref:        "main",
	}); err != nil {
		t.Fatalf("accept: %v", err)
	}

	handles := s.ActiveSessions()
	if len(handles) != 1 {
		t.Fatalf("want 1 active session, got %d", len(handles))
	}
	h := handles[0]
	if h.SessionID != "sess-123" {
		t.Errorf("sessionId: got %q", h.SessionID)
	}
	if got := filepath.Join(parent, "sess-123"); h.WorktreePath != got {
		t.Errorf("worktreePath: want %q, got %q", got, h.WorktreePath)
	}
	if h.ProjectName != "acme" {
		t.Errorf("projectName: want acme, got %q", h.ProjectName)
	}
	if h.Repository != "github.com/acme/web" {
		t.Errorf("repository: want github.com/acme/web, got %q", h.Repository)
	}
}

// TestSpawner_HandleEnrichment_NoParentLeavesPathEmpty verifies the
// fail-soft path: when no WorktreeParentDir is configured, WorktreePath is
// left empty (a reader falls back to a per-session detail call) but the
// other enrichment fields are still populated.
func TestSpawner_HandleEnrichment_NoParentLeavesPathEmpty(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "acme", Repository: "github.com/acme/web"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 5"},
		// WorktreeParentDir intentionally unset.
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })

	if _, err := s.AcceptWork(SessionSpec{SessionID: "s", Repository: "github.com/acme/web"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	handles := s.ActiveSessions()
	if len(handles) != 1 {
		t.Fatalf("want 1 active, got %d", len(handles))
	}
	if handles[0].WorktreePath != "" {
		t.Errorf("worktreePath should be empty without a parent dir, got %q", handles[0].WorktreePath)
	}
	if handles[0].ProjectName != "acme" {
		t.Errorf("projectName should still be set, got %q", handles[0].ProjectName)
	}
}
