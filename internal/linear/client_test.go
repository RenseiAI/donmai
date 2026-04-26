package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient creates an httptest.Server and a Client pointed at it.
// The server is closed automatically when the test finishes.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL:    srv.URL,
		APIKey:     "lin_api_testkey",
		HTTPClient: srv.Client(),
	}
	return c, srv
}

// issueJSON returns a JSON object fragment for a single issue node.
func issueNodesJSON(issues []map[string]any) string {
	data, _ := json.Marshal(issues)
	return string(data)
}

// writeGQLResponse writes a GraphQL response envelope with the given data JSON.
func writeGQLData(w http.ResponseWriter, dataJSON string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":` + dataJSON + `}`))
}

// writeGQLError writes a GraphQL error response.
func writeGQLError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"data":   nil,
		"errors": []map[string]any{{"message": msg}},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// --- NewClient ---

func TestNewClientEmptyAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewClient("")
	if !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("got %v, want ErrInvalidAPIKey", err)
	}
}

func TestNewClientWhitespaceAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewClient("   ")
	if !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("got %v, want ErrInvalidAPIKey", err)
	}
}

func TestNewClientValidKey(t *testing.T) {
	t.Parallel()
	c, err := NewClient("lin_api_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, defaultBaseURL)
	}
	if c.APIKey != "lin_api_abc" {
		t.Errorf("APIKey not set correctly")
	}
	if c.HTTPClient == nil {
		t.Error("HTTPClient is nil")
	}
}

// --- Authorization header ---

func TestClientSendsRawAPIKeyHeader(t *testing.T) {
	t.Parallel()
	var gotAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeGQLData(w, `{"issues":{"nodes":[]}}`)
	})
	_, _ = c.ListIssuesByProject(context.Background(), "proj", nil)
	if gotAuth != "lin_api_testkey" {
		t.Errorf("Authorization = %q, want raw API key without Bearer prefix", gotAuth)
	}
}

// --- ListIssuesByProject ---

func TestListIssuesByProjectSuccess(t *testing.T) {
	t.Parallel()
	nodes := []map[string]any{
		{
			"id":         "issue-1",
			"identifier": "REN-1",
			"title":      "First issue",
			"state":      map[string]any{"name": "In Progress"},
			"project":    map[string]any{"name": "MyProject"},
			"parent":     nil,
		},
		{
			"id":         "issue-2",
			"identifier": "REN-2",
			"title":      "Second issue",
			"state":      map[string]any{"name": "Todo"},
			"project":    map[string]any{"name": "MyProject"},
			"parent":     map[string]any{"id": "issue-0"},
		},
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeGQLData(w, `{"issues":{"nodes":`+issueNodesJSON(nodes)+`}}`)
	})

	issues, err := c.ListIssuesByProject(context.Background(), "MyProject", []string{"In Progress", "Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByProject: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].ID != "issue-1" || issues[0].Identifier != "REN-1" {
		t.Errorf("issues[0] = %+v", issues[0])
	}
	if issues[1].ParentID != "issue-0" {
		t.Errorf("issues[1].ParentID = %q, want issue-0", issues[1].ParentID)
	}
}

func TestListIssuesByProjectNoStates(t *testing.T) {
	t.Parallel()
	var gotVars map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVars = req.Variables
		writeGQLData(w, `{"issues":{"nodes":[]}}`)
	})

	_, err := c.ListIssuesByProject(context.Background(), "proj", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := gotVars["states"]; ok {
		t.Error("states variable should not be set when nil states passed")
	}
}

// --- GetIssue ---

func TestGetIssueSuccess(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeGQLData(w, `{"issue":{"id":"issue-1","identifier":"REN-1","title":"Some issue","state":{"name":"Done"},"project":{"name":"MyProj"},"parent":null}}`)
	})

	iss, err := c.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if iss.ID != "issue-1" || iss.Title != "Some issue" {
		t.Errorf("unexpected issue: %+v", iss)
	}
	if iss.State.Name != "Done" {
		t.Errorf("state = %q, want Done", iss.State.Name)
	}
}

func TestGetIssueNullReturnsNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"issue":null}`)
	})
	_, err := c.GetIssue(context.Background(), "missing-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// --- ListSubIssues ---

func TestListSubIssuesSuccess(t *testing.T) {
	t.Parallel()
	nodes := []map[string]any{
		{
			"id":         "child-1",
			"identifier": "REN-10",
			"title":      "Child issue",
			"state":      map[string]any{"name": "In Progress"},
			"project":    map[string]any{"name": "Proj"},
			"parent":     map[string]any{"id": "parent-1"},
		},
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"issues":{"nodes":`+issueNodesJSON(nodes)+`}}`)
	})

	issues, err := c.ListSubIssues(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if issues[0].ID != "child-1" || issues[0].ParentID != "parent-1" {
		t.Errorf("unexpected child: %+v", issues[0])
	}
}

// --- HTTP error → sentinel error mapping ---

func TestHTTPStatusErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401 → ErrUnauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"403 → ErrForbidden", http.StatusForbidden, ErrForbidden},
		{"404 → ErrNotFound", http.StatusNotFound, ErrNotFound},
		{"429 → ErrRateLimited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 → ErrServerError", http.StatusInternalServerError, ErrServerError},
		{"502 → ErrServerError", http.StatusBadGateway, ErrServerError},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := c.GetIssue(context.Background(), "any-id")
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// --- GraphQL error in response body ---

func TestGraphQLErrorInBody(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLError(w, "Entity not found")
	})
	_, err := c.GetIssue(context.Background(), "bad-id")
	if !errors.Is(err, ErrGraphQLError) {
		t.Fatalf("got %v, want ErrGraphQLError", err)
	}
}

func TestGraphQLErrorInBodyListIssues(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLError(w, "Access denied")
	})
	_, err := c.ListIssuesByProject(context.Background(), "proj", nil)
	if !errors.Is(err, ErrGraphQLError) {
		t.Fatalf("got %v, want ErrGraphQLError", err)
	}
}

func TestGraphQLErrorInBodyListSubIssues(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLError(w, "Parent not found")
	})
	_, err := c.ListSubIssues(context.Background(), "parent-id")
	if !errors.Is(err, ErrGraphQLError) {
		t.Fatalf("got %v, want ErrGraphQLError", err)
	}
}

// --- Interface compliance ---

func TestClientImplementsLinearInterface(t *testing.T) {
	t.Parallel()
	var _ Linear = (*Client)(nil)
}
