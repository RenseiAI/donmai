package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient creates an httptest.Server and a Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("ghp_test_fixture_not_a_secret") //nolint:gosec // dummy fixture
	c.BaseURL = srv.URL
	return c, srv
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func issueFixture(number int, title, state string) map[string]any {
	return map[string]any{
		"number":    number,
		"title":     title,
		"body":      "body text",
		"state":     state,
		"html_url":  fmt.Sprintf("https://github.com/owner/repo/issues/%d", number),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"labels":    []map[string]any{{"id": 1, "name": "bug", "color": "d73a4a", "description": ""}},
		"assignees": []map[string]any{},
		"user":      map[string]any{"login": "alice", "name": "Alice", "email": "", "html_url": "", "avatar_url": ""},
		"milestone": nil,
		"comments":  0,
	}
}

// ─── NewClient ────────────────────────────────────────────────────────────────

func TestNewClient(t *testing.T) {
	t.Parallel()
	c := NewClient("ghp_test")
	if c.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, defaultBaseURL)
	}
	if c.token != "ghp_test" {
		t.Errorf("token not stored correctly")
	}
}

// ─── GetIssue ─────────────────────────────────────────────────────────────────

func TestGetIssue(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues/42" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ghp_test_fixture_not_a_secret" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, issueFixture(42, "Test Issue", "open"))
	})

	issue, err := c.GetIssue(context.Background(), "owner", "repo", 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Number != 42 {
		t.Errorf("Number = %d, want 42", issue.Number)
	}
	if issue.Title != "Test Issue" {
		t.Errorf("Title = %q, want Test Issue", issue.Title)
	}
	if issue.State != "open" {
		t.Errorf("State = %q, want open", issue.State)
	}
	if len(issue.Labels) != 1 || issue.Labels[0].Name != "bug" {
		t.Errorf("Labels = %v, want [{bug}]", issue.Labels)
	}
}

func TestGetIssueNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})

	_, err := c.GetIssue(context.Background(), "owner", "repo", 9999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestGetIssueUnauthorized(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
	})

	_, err := c.GetIssue(context.Background(), "owner", "repo", 1)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

// ─── ListIssues ───────────────────────────────────────────────────────────────

func TestListIssues(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("state") != "open" {
			http.Error(w, "unexpected state filter", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, []any{
			issueFixture(1, "First", "open"),
			issueFixture(2, "Second", "open"),
		})
	})

	issues, err := c.ListIssues(context.Background(), "owner", "repo", ListIssuesOptions{State: "open"})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].Title != "First" {
		t.Errorf("issues[0].Title = %q, want First", issues[0].Title)
	}
}

func TestListIssuesDefaultsToOpen(t *testing.T) {
	t.Parallel()
	var gotState string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		writeJSON(w, http.StatusOK, []any{})
	})

	_, err := c.ListIssues(context.Background(), "owner", "repo", ListIssuesOptions{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if gotState != "open" {
		t.Errorf("default state = %q, want open", gotState)
	}
}

// ─── ListIssueComments ────────────────────────────────────────────────────────

func TestListIssueComments(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Format(time.RFC3339)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues/5/comments" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 1001, "body": "hello", "html_url": "https://x", "created_at": now, "updated_at": now,
				"user": map[string]any{"login": "bob", "name": "Bob", "email": "", "html_url": "", "avatar_url": ""}},
		})
	})

	comments, err := c.ListIssueComments(context.Background(), "owner", "repo", 5)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	if comments[0].Body != "hello" {
		t.Errorf("Body = %q, want hello", comments[0].Body)
	}
}

// ─── CreateIssue ──────────────────────────────────────────────────────────────

func TestCreateIssue(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body CreateIssueInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.Title == "" {
			http.Error(w, "missing title", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusCreated, issueFixture(99, body.Title, "open"))
	})

	issue, err := c.CreateIssue(context.Background(), "owner", "repo", CreateIssueInput{
		Title:  "New Bug",
		Body:   "Something is wrong",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.Title != "New Bug" {
		t.Errorf("Title = %q, want New Bug", issue.Title)
	}
}

// ─── UpdateIssue ──────────────────────────────────────────────────────────────

func TestUpdateIssue(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body UpdateIssueInput
		_ = json.NewDecoder(r.Body).Decode(&body)
		fixture := issueFixture(42, "Updated Title", body.State)
		if body.Title != "" {
			fixture["title"] = body.Title
		}
		writeJSON(w, http.StatusOK, fixture)
	})

	issue, err := c.UpdateIssue(context.Background(), "owner", "repo", 42, UpdateIssueInput{
		Title: "Updated Title",
		State: "closed",
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if issue.Title != "Updated Title" {
		t.Errorf("Title = %q, want Updated Title", issue.Title)
	}
}

// ─── CreateIssueComment ───────────────────────────────────────────────────────

func TestCreateIssueComment(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Format(time.RFC3339)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": 5001, "body": body["body"],
			"html_url": "https://x", "created_at": now, "updated_at": now,
			"user": map[string]any{"login": "carol", "name": "Carol", "email": "", "html_url": "", "avatar_url": ""},
		})
	})

	comment, err := c.CreateIssueComment(context.Background(), "owner", "repo", 42, "Great fix!")
	if err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if comment.Body != "Great fix!" {
		t.Errorf("Body = %q, want Great fix!", comment.Body)
	}
}

// ─── AddLabels ────────────────────────────────────────────────────────────────

func TestAddLabels(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 1, "name": "bug", "color": "d73a4a", "description": ""},
			{"id": 2, "name": "enhancement", "color": "a2eeef", "description": ""},
		})
	})

	labels, err := c.AddLabels(context.Background(), "owner", "repo", 42, []string{"bug", "enhancement"})
	if err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("got %d labels, want 2", len(labels))
	}
	if labels[0].Name != "bug" {
		t.Errorf("labels[0].Name = %q, want bug", labels[0].Name)
	}
}

// ─── GetRepo ──────────────────────────────────────────────────────────────────

func TestGetRepo(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"full_name": "owner/repo", "name": "repo",
			"description": "A test repo", "html_url": "https://github.com/owner/repo",
			"private": false, "open_issues_count": 5,
		})
	})

	repo, err := c.GetRepo(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.FullName != "owner/repo" {
		t.Errorf("FullName = %q, want owner/repo", repo.FullName)
	}
	if repo.OpenIssues != 5 {
		t.Errorf("OpenIssues = %d, want 5", repo.OpenIssues)
	}
}

// ─── ListLabels ───────────────────────────────────────────────────────────────

func TestListLabels(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 1, "name": "bug", "color": "d73a4a", "description": "Something is broken"},
			{"id": 2, "name": "docs", "color": "0075ca", "description": "Documentation"},
		})
	})

	labels, err := c.ListLabels(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("got %d labels, want 2", len(labels))
	}
}

// ─── GetAuthenticatedUser ─────────────────────────────────────────────────────

func TestGetAuthenticatedUser(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"login": "dave", "name": "Dave",
			"email": "dave@example.com", "html_url": "https://github.com/dave", "avatar_url": "",
		})
	})

	user, err := c.GetAuthenticatedUser(context.Background())
	if err != nil {
		t.Fatalf("GetAuthenticatedUser: %v", err)
	}
	if user.Login != "dave" {
		t.Errorf("Login = %q, want dave", user.Login)
	}
}

// ─── APIError ─────────────────────────────────────────────────────────────────

func TestAPIError(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"message": "Validation Failed",
			"documentation_url": "https://docs.github.com/rest",
		})
	})

	_, err := c.GetIssue(context.Background(), "owner", "repo", 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusUnprocessableEntity)
	}
}

// ─── GitHub interface compliance ─────────────────────────────────────────────

func TestClientImplementsInterface(_ *testing.T) {
	var _ GitHub = (*Client)(nil)
}

// ─── NewProxiedClient ─────────────────────────────────────────────────────────

func TestNewProxiedClient(t *testing.T) {
	t.Parallel()
	c := NewProxiedClient("https://app.example.com", "rsk_test")
	want := "https://app.example.com/api/cli/github/rest"
	if c.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, want)
	}
}
