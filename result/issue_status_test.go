package result_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/result"
)

// TestUpdateIssueStatus_HappyPath asserts the wire shape: POST to
// /api/issue-tracker-proxy with method=updateIssueStatus and the
// (issueID, statusName) tuple as args.
func TestUpdateIssueStatus_HappyPath(t *testing.T) {
	var captured atomic.Pointer[struct {
		Method string `json:"method"`
		Args   []any  `json:"args"`
	}]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/issue-tracker-proxy") {
			http.Error(w, "wrong path", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Args   []any  `json:"args"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		captured.Store(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"issue-1","identifier":"REN-1"}}`))
	}))
	defer srv.Close()

	p, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "tok",
		WorkerID:    "w1",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.UpdateIssueStatus(ctx, "issue-uuid-1", "Finished"); err != nil {
		t.Fatalf("UpdateIssueStatus: %v", err)
	}

	c := captured.Load()
	if c == nil {
		t.Fatal("no proxy request captured")
	}
	if c.Method != "updateIssueStatus" {
		t.Errorf("method = %q; want updateIssueStatus", c.Method)
	}
	if len(c.Args) != 2 || c.Args[0] != "issue-uuid-1" || c.Args[1] != "Finished" {
		t.Errorf("args = %v; want [issue-uuid-1 Finished]", c.Args)
	}
}

// TestUpdateIssueStatus_RequiresIssueID asserts the input-validation
// path returns a clear error.
func TestUpdateIssueStatus_RequiresIssueID(t *testing.T) {
	p, _ := result.NewPoster(result.Options{
		PlatformURL: "http://localhost:0",
		AuthToken:   "t",
		WorkerID:    "w",
		BaseDelay:   1,
	})
	if err := p.UpdateIssueStatus(context.Background(), "", "Finished"); err == nil {
		t.Errorf("UpdateIssueStatus(\"\", ...) = nil; want error")
	}
	if err := p.UpdateIssueStatus(context.Background(), "id", ""); err == nil {
		t.Errorf("UpdateIssueStatus(id, \"\") = nil; want error")
	}
}

// TestUpdateIssueStatus_PermanentErrorOn4xx asserts a 4xx response
// surfaces as a PermanentError without retrying.
func TestUpdateIssueStatus_PermanentErrorOn4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "bad request", 400)
	}))
	defer srv.Close()

	p, _ := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "t",
		WorkerID:    "w",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	err := p.UpdateIssueStatus(context.Background(), "id", "Finished")
	if err == nil {
		t.Fatal("UpdateIssueStatus = nil; want error")
	}
	var perm *result.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("error type = %T; want *PermanentError (err=%v)", err, err)
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d; want 1 (no retries on 4xx)", attempts.Load())
	}
}

// TestUpdateIssueStatus_TransientErrorRetries asserts a 5xx response
// is retried up to MaxAttempts and surfaces as a TransientError.
func TestUpdateIssueStatus_TransientErrorRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "server error", 500)
	}))
	defer srv.Close()

	p, _ := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "t",
		WorkerID:    "w",
		HTTPClient:  srv.Client(),
		MaxAttempts: 3,
		BaseDelay:   1,
	})
	err := p.UpdateIssueStatus(context.Background(), "id", "Finished")
	if err == nil {
		t.Fatal("UpdateIssueStatus = nil; want error")
	}
	var trans *result.TransientError
	if !errors.As(err, &trans) {
		t.Errorf("error type = %T; want *TransientError", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d; want 3", attempts.Load())
	}
}

// TestUpdateIssueStatus_SuccessFalseFails asserts a 200 response with
// success=false counts as failure (not a silent no-op).
func TestUpdateIssueStatus_SuccessFalseFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":"NO_CLIENT","message":"linear not configured","retryable":false}}`))
	}))
	defer srv.Close()

	p, _ := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "t",
		WorkerID:    "w",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	err := p.UpdateIssueStatus(context.Background(), "id", "Finished")
	if err == nil {
		t.Fatal("UpdateIssueStatus = nil; want error on success=false")
	}
	var perm *result.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("error type = %T; want *PermanentError on retryable=false", err)
	}
}

// TestCreateIssueComment_HappyPath asserts the wire shape and that the
// body string is passed through verbatim as the second arg.
func TestCreateIssueComment_HappyPath(t *testing.T) {
	var captured atomic.Pointer[struct {
		Method string `json:"method"`
		Args   []any  `json:"args"`
	}]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Args   []any  `json:"args"`
		}
		_ = json.Unmarshal(body, &req)
		captured.Store(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"comment-1"}}`))
	}))
	defer srv.Close()

	p, _ := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "t",
		WorkerID:    "w",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err := p.CreateIssueComment(context.Background(), "issue-x", "hello world"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	c := captured.Load()
	if c == nil {
		t.Fatal("no proxy request captured")
	}
	if c.Method != "createComment" {
		t.Errorf("method = %q; want createComment", c.Method)
	}
	if len(c.Args) != 2 || c.Args[0] != "issue-x" || c.Args[1] != "hello world" {
		t.Errorf("args = %v; want [issue-x hello world]", c.Args)
	}
}

// TestUpdateIssueStatus_AuthHeaderSet asserts the bearer token is sent
// in the Authorization header — the worker's session bearer is the
// path the proxy auths against.
func TestUpdateIssueStatus_AuthHeaderSet(t *testing.T) {
	var sawAuth atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		sawAuth.Store(&auth)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer srv.Close()
	p, _ := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		AuthToken:   "the-token",
		WorkerID:    "w",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err := p.UpdateIssueStatus(context.Background(), "id", "Finished"); err != nil {
		t.Fatalf("UpdateIssueStatus: %v", err)
	}
	if h := sawAuth.Load(); h == nil || *h != "Bearer the-token" {
		t.Errorf("Authorization = %v; want 'Bearer the-token'", h)
	}
}
