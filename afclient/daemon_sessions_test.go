package afclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDaemonClient_GetSessions exercises GET /api/daemon/sessions through the
// real HTTP client, asserting the enrichment fields (worktreePath /
// projectName / repository) decode onto DaemonSessionHandle.
func TestDaemonClient_GetSessions(t *testing.T) {
	t.Parallel()
	fixture := []DaemonSessionHandle{
		{
			SessionID:    "sess-1",
			PID:          4242,
			AcceptedAt:   "2026-06-13T14:00:00Z",
			State:        "running",
			WorktreePath: "/home/u/.donmai/worktrees/sess-1",
			ProjectName:  "acme",
			Repository:   "github.com/acme/web",
		},
		{SessionID: "sess-2", State: "starting"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/sessions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	c := NewDaemonClientFromURL(srv.URL)
	got, err := c.GetSessions()
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}
	h := got[0]
	if h.WorktreePath != fixture[0].WorktreePath {
		t.Errorf("worktreePath = %q, want %q", h.WorktreePath, fixture[0].WorktreePath)
	}
	if h.ProjectName != "acme" {
		t.Errorf("projectName = %q, want acme", h.ProjectName)
	}
	if h.Repository != "github.com/acme/web" {
		t.Errorf("repository = %q", h.Repository)
	}
	// The unenriched second handle decodes with empty enrichment fields.
	if got[1].WorktreePath != "" || got[1].ProjectName != "" {
		t.Errorf("second handle should have empty enrichment, got %#v", got[1])
	}
}

// TestDaemonClient_GetSessions_Error maps a non-2xx to an error.
func TestDaemonClient_GetSessions_Error(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := NewDaemonClientFromURL(srv.URL)
	if _, err := c.GetSessions(); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}
