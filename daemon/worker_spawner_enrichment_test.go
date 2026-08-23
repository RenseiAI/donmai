package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
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
	if h.WorkareaRoot != h.WorktreePath {
		t.Errorf("legacy workareaRoot = %q, want degenerate worktreePath %q", h.WorkareaRoot, h.WorktreePath)
	}
	if h.ProjectName != "acme" {
		t.Errorf("projectName: want acme, got %q", h.ProjectName)
	}
	if h.Repository != "github.com/acme/web" {
		t.Errorf("repository: want github.com/acme/web, got %q", h.Repository)
	}
}

func TestSpawnerHandlePublishesNestedRootAndSelectedRepositoryCWD(t *testing.T) {
	parent := t.TempDir()
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "acme", Repository: "github.com/acme/web"}},
		MaxConcurrentSessions: 1, WorktreeParentDir: parent,
		WorkerCommand: []string{"/bin/sh", "-c", "sleep 5"},
	})
	t.Cleanup(func() { _ = spawner.Drain(time.Second) })
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "github.com/acme/web", Ref: "main"}, Name: "web", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "github.com/acme/docs", Ref: "main"}, Name: "docs", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "docs"},
	}
	if _, err := spawner.AcceptWork(SessionSpec{
		SessionID: "nested-session", Repository: "github.com/acme/web", Ref: "main", RepositoryDeclaration: declaration,
	}); err != nil {
		t.Fatal(err)
	}
	handle := spawner.ActiveSessions()[0]
	if handle.WorkareaRoot == "" || handle.WorkareaRoot == handle.WorktreePath || filepath.Base(handle.WorktreePath) != "docs" {
		t.Fatalf("nested handle = root %q cwd %q", handle.WorkareaRoot, handle.WorktreePath)
	}
	if filepath.Dir(handle.WorktreePath) != handle.WorkareaRoot {
		t.Fatalf("selected CWD %q is not a direct root leaf %q", handle.WorktreePath, handle.WorkareaRoot)
	}
	for name, body := range map[string]string{"web/file": "mutable", "docs/file": "context"} {
		path := filepath.Join(handle.WorkareaRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	normalized, err := declaration.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	record := workarea.NewDeclarationRecord("nested-session", "wa_nested_test", normalized, map[string]string{"web": "a", "docs": "b"})
	if err := workarea.WriteDeclaration(t.Context(), workarea.RootPath(handle.WorkareaRoot), record); err != nil {
		t.Fatal(err)
	}
	workareas := spawner.ActiveWorkareas()
	if len(workareas) != 1 || workareas[0].WorkareaRoot != handle.WorkareaRoot || workareas[0].RepositoryWorktreePath != handle.WorktreePath || workareas[0].SizeBytes < int64(len("mutable")+len("context")) {
		t.Fatalf("active workarea root projection = %#v", workareas)
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
