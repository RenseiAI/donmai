package afclient_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func newWorkareaTestServer(t *testing.T, handler http.HandlerFunc) (*afclient.DaemonClient, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return afclient.NewDaemonClientFromURL(s.URL), s
}

// ── ListWorkareas ─────────────────────────────────────────────────────────

func TestDaemonClient_ListWorkareas_HappyPath(t *testing.T) {
	wantBody := afclient.ListWorkareasResponse{
		Active: []afclient.WorkareaSummary{{
			ID: "active-1", Kind: afclient.WorkareaKindActive,
			Status: afclient.WorkareaStatusReady,
		}},
		Archived: []afclient.WorkareaSummary{{
			ID: "arch-1", Kind: afclient.WorkareaKindArchived,
			Status: afclient.WorkareaStatusArchived,
		}},
	}
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workareas" || r.Method != http.MethodGet {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&wantBody)
	})
	got, err := client.ListWorkareas()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got.Active) != 1 || got.Active[0].ID != "active-1" {
		t.Errorf("active mismatch: %+v", got.Active)
	}
	if len(got.Archived) != 1 || got.Archived[0].ID != "arch-1" {
		t.Errorf("archived mismatch: %+v", got.Archived)
	}
}

func TestDaemonClient_ListWorkareas_ServerError(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := client.ListWorkareas(); !errors.Is(err, afclient.ErrServerError) {
		t.Errorf("expected ErrServerError, got %v", err)
	}
}

// ── GetWorkarea ───────────────────────────────────────────────────────────

func TestDaemonClient_GetWorkarea_HappyPath(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := afclient.WorkareaEnvelope{Workarea: afclient.Workarea{
		ID:         "wa-1",
		Kind:       afclient.WorkareaKindArchived,
		Status:     afclient.WorkareaStatusArchived,
		Repository: "github.com/x/y",
		AcquiredAt: &now,
	}}
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/workareas/") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&want)
	})
	got, err := client.GetWorkarea("wa-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Workarea.ID != "wa-1" || got.Workarea.Repository != "github.com/x/y" {
		t.Errorf("workarea mismatch: %+v", got.Workarea)
	}
}

func TestDaemonClient_GetWorkarea_NotFound(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	_, err := client.GetWorkarea("ghost")
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDaemonClient_GetWorkarea_RequiresID(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not have been called: %s", r.URL.Path)
	})
	if _, err := client.GetWorkarea(""); err == nil {
		t.Error("expected error when id is empty")
	}
}

// ── RestoreWorkarea ───────────────────────────────────────────────────────

func TestDaemonClient_RestoreWorkarea_HappyPath(t *testing.T) {
	want := afclient.WorkareaRestoreResult{Workarea: afclient.Workarea{
		ID:     "restored-1",
		Kind:   afclient.WorkareaKindActive,
		Status: afclient.WorkareaStatusReady,
	}}
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/restore") {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		var req afclient.WorkareaRestoreRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if req.Reason != "audit" {
			http.Error(w, fmt.Sprintf("reason: got %q", req.Reason), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&want)
	})
	got, err := client.RestoreWorkarea("wa-1", afclient.WorkareaRestoreRequest{Reason: "audit"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.Workarea.ID != "restored-1" {
		t.Errorf("restored id mismatch: %q", got.Workarea.ID)
	}
}

func TestDaemonClient_RestoreWorkarea_Conflict(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"intoSessionId in use"}`))
	})
	_, err := client.RestoreWorkarea("wa-1", afclient.WorkareaRestoreRequest{IntoSessionID: "x"})
	if !errors.Is(err, afclient.ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}

func TestDaemonClient_RestoreWorkarea_Unavailable(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := client.RestoreWorkarea("wa-1", afclient.WorkareaRestoreRequest{})
	if !errors.Is(err, afclient.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got %v", err)
	}
}

func TestDaemonClient_RestoreWorkarea_BadRequest(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "archive corrupted", http.StatusBadRequest)
	})
	_, err := client.RestoreWorkarea("wa-1", afclient.WorkareaRestoreRequest{})
	if !errors.Is(err, afclient.ErrBadRequest) {
		t.Errorf("expected ErrBadRequest, got %v", err)
	}
}

func TestDaemonClient_RestoreWorkarea_RequiresID(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not have been called")
	})
	if _, err := client.RestoreWorkarea("", afclient.WorkareaRestoreRequest{}); err == nil {
		t.Error("expected error when id is empty")
	}
}

// ── DiffWorkareas ─────────────────────────────────────────────────────────

func TestDaemonClient_DiffWorkareas_JSONPath(t *testing.T) {
	want := afclient.WorkareaDiffEnvelope{Diff: afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{
			WorkareaA: "a", WorkareaB: "b",
			Added: 1, Removed: 1, Modified: 1, Total: 3,
		},
		Entries: []afclient.WorkareaDiffEntry{
			{Path: "added.txt", Status: afclient.WorkareaDiffStatusAdded, SizeB: 5},
			{Path: "modified.txt", Status: afclient.WorkareaDiffStatusModified, HashA: "h1", HashB: "h2"},
			{Path: "removed.txt", Status: afclient.WorkareaDiffStatusRemoved, SizeA: 3},
		},
	}}
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/diff/") {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&want)
	})
	got, err := client.DiffWorkareas("a", "b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if got.Summary.Total != 3 {
		t.Errorf("summary total: %+v", got.Summary)
	}
	if len(got.Entries) != 3 {
		t.Errorf("entries: %d", len(got.Entries))
	}
}

func TestDaemonClient_DiffWorkareas_NDJSONPath(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/diff/") {
			http.Error(w, "wrong route", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		// 4 entries + summary
		entries := []afclient.WorkareaDiffEntry{
			{Path: "a.txt", Status: afclient.WorkareaDiffStatusAdded},
			{Path: "b.txt", Status: afclient.WorkareaDiffStatusModified},
			{Path: "c.txt", Status: afclient.WorkareaDiffStatusModified},
			{Path: "d.txt", Status: afclient.WorkareaDiffStatusRemoved},
		}
		enc := json.NewEncoder(w)
		for _, e := range entries {
			_ = enc.Encode(&e)
		}
		_ = enc.Encode(struct {
			Summary afclient.WorkareaDiffSummary `json:"summary"`
		}{Summary: afclient.WorkareaDiffSummary{
			WorkareaA: "a", WorkareaB: "b",
			Added: 1, Removed: 1, Modified: 2, Total: 4,
		}})
	})
	got, err := client.DiffWorkareas("a", "b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(got.Entries) != 4 {
		t.Errorf("entries: want 4, got %d", len(got.Entries))
	}
	if got.Summary.Total != 4 || got.Summary.Modified != 2 {
		t.Errorf("summary mismatch: %+v", got.Summary)
	}
}

func TestDaemonClient_DiffWorkareas_NDJSON_ServerError(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path":"x.txt","status":"added"}` + "\n"))
		_, _ = w.Write([]byte(`{"error":"walker died midway"}` + "\n"))
	})
	_, err := client.DiffWorkareas("a", "b")
	if err == nil {
		t.Error("expected error from mid-stream server error")
	}
}

func TestDaemonClient_DiffWorkareas_NotFound(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	_, err := client.DiffWorkareas("a", "b")
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDaemonClient_DiffWorkareas_RequiresIDs(t *testing.T) {
	client, _ := newWorkareaTestServer(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called")
	})
	if _, err := client.DiffWorkareas("", "b"); err == nil {
		t.Error("expected error for empty A")
	}
	if _, err := client.DiffWorkareas("a", ""); err == nil {
		t.Error("expected error for empty B")
	}
}

func TestDaemonClient_DiffWorkareas_UnknownContentType(t *testing.T) {
	// Best-effort fall-through: server forgot to set the content-type;
	// the client treats it as JSON. Verify no error when the body is
	// indeed JSON.
	client, _ := newWorkareaTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "")
		_ = json.NewEncoder(w).Encode(&afclient.WorkareaDiffEnvelope{Diff: afclient.WorkareaDiffResult{}})
	})
	if _, err := client.DiffWorkareas("a", "b"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
