package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient creates an httptest.Server and a Client pointed at it.
// The server is closed automatically when the test finishes.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	const dummyAPIKey = "test-fixture-key-not-a-secret" //nolint:gosec // dummy fixture value used only by httptest server
	c := &Client{
		BaseURL:    srv.URL,
		APIKey:     dummyAPIKey,
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

func TestListTeamsPaginates(t *testing.T) {
	var cursors []any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "ListTeams") {
			t.Fatalf("query = %q, want ListTeams", req.Query)
		}
		cursors = append(cursors, req.Variables["after"])
		if len(cursors) == 1 {
			writeGQLData(w, `{"teams":{"nodes":[{"id":"team-b","key":"B","name":"Beta"}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}`)
			return
		}
		writeGQLData(w, `{"teams":{"nodes":[{"id":"team-a","key":"A","name":"Alpha"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})

	teams, err := c.ListTeams(context.Background())
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 2 || teams[0].Key != "B" || teams[1].Key != "A" {
		t.Fatalf("teams = %#v", teams)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "cursor-1" {
		t.Fatalf("after cursors = %#v, want [nil cursor-1]", cursors)
	}
}

func TestListTeamsFailsClosedOnMissingCursor(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"teams":{"nodes":[{"id":"team-a","key":"A","name":"Alpha"}],"pageInfo":{"hasNextPage":true,"endCursor":null}}}`)
	})

	teams, err := c.ListTeams(context.Background())
	if err == nil || !strings.Contains(err.Error(), "teams hasNextPage without endCursor") {
		t.Fatalf("ListTeams error = %v, want missing cursor", err)
	}
	if teams != nil {
		t.Fatalf("ListTeams returned partial teams: %#v", teams)
	}
}

func TestListTeamsDistinguishesEmptyFromMalformedCatalog(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name: "honest empty connection",
			data: `{"teams":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
		},
		{
			name:    "missing connection",
			data:    `{}`,
			wantErr: "teams connection is missing",
		},
		{
			name:    "missing nodes",
			data:    `{"teams":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "teams nodes are missing",
		},
		{
			name:    "null node",
			data:    `{"teams":{"nodes":[null],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "teams node 0 is null",
		},
		{
			name:    "missing team key",
			data:    `{"teams":{"nodes":[{"id":"team-1","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "teams node 0 key is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, tc.data)
			})

			teams, err := c.ListTeams(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ListTeams: %v", err)
				}
				if teams == nil || len(teams) != 0 {
					t.Fatalf("ListTeams = %#v, want non-nil empty slice", teams)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListTeams error = %v, want %q", err, tc.wantErr)
			}
			if teams != nil {
				t.Fatalf("ListTeams returned partial teams: %#v", teams)
			}
		})
	}
}

func TestListProjectsFiltersByResolvedTeamAndPreservesState(t *testing.T) {
	var sawProjectFilter map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch {
		case strings.Contains(req.Query, "ListTeams"):
			writeGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListProjects"):
			sawProjectFilter, _ = req.Variables["filter"].(map[string]any)
			writeGQLData(w, `{"projects":{"nodes":[{"id":"project-1","name":"Launch","state":"started","teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"},{"id":"team-2","key":"OPS","name":"Operations"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	projects, err := c.ListProjects(context.Background(), "ENG")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %#v", projects)
	}
	if got := projects[0]; got.ID != "project-1" || got.State != "started" || len(got.TeamKeys) != 1 || got.TeamKeys[0] != "ENG" {
		t.Fatalf("project = %#v", got)
	}
	accessible, ok := sawProjectFilter["accessibleTeams"].(map[string]any)
	if !ok {
		t.Fatalf("project filter = %#v, missing accessibleTeams", sawProjectFilter)
	}
	some, _ := accessible["some"].(map[string]any)
	id, _ := some["id"].(map[string]any)
	if id["eq"] != "team-1" {
		t.Fatalf("project filter id = %#v, want team-1", id)
	}
}

func TestListProjectsRejectsTruncatedTeamMembership(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"projects":{"nodes":[{"id":"project-1","name":"Launch","state":"started","teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":true,"endCursor":"more"}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})

	projects, err := c.ListProjects(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "project \"project-1\" teams: pagination is not supported") {
		t.Fatalf("ListProjects error = %v, want nested pagination failure", err)
	}
	if projects != nil {
		t.Fatalf("ListProjects returned partial projects: %#v", projects)
	}
}

func TestListProjectsFailsClosedOnMalformedCatalog(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name: "honest empty connection",
			data: `{"projects":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
		},
		{
			name:    "missing connection",
			data:    `{}`,
			wantErr: "projects connection is missing",
		},
		{
			name:    "missing nodes",
			data:    `{"projects":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "projects nodes are missing",
		},
		{
			name:    "null project node",
			data:    `{"projects":{"nodes":[null],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "projects node 0 is null",
		},
		{
			name:    "missing project state",
			data:    `{"projects":{"nodes":[{"id":"project-1","name":"Launch","teams":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "projects node 0 state is missing",
		},
		{
			name:    "missing nested team key",
			data:    `{"projects":{"nodes":[{"id":"project-1","name":"Launch","state":"started","teams":{"nodes":[{"id":"team-1","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "teams node 0 key is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, tc.data)
			})

			projects, err := c.ListProjects(context.Background(), "")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ListProjects: %v", err)
				}
				if projects == nil || len(projects) != 0 {
					t.Fatalf("ListProjects = %#v, want non-nil empty slice", projects)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListProjects error = %v, want %q", err, tc.wantErr)
			}
			if projects != nil {
				t.Fatalf("ListProjects returned partial projects: %#v", projects)
			}
		})
	}
}

func TestListProjectsTeamSelectorUsesKeyNotDisplayName(t *testing.T) {
	var projectFilter map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch {
		case strings.Contains(req.Query, "ListTeams"):
			// One team's display name collides with the other team's key, and
			// display names are duplicated. The selector must still choose key OPS.
			writeGQLData(w, `{"teams":{"nodes":[{"id":"team-ops","key":"OPS","name":"Shared"},{"id":"team-eng","key":"ENG","name":"OPS"},{"id":"team-alt","key":"ALT","name":"Shared"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListProjects"):
			projectFilter, _ = req.Variables["filter"].(map[string]any)
			writeGQLData(w, `{"projects":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	projects, err := c.ListProjects(context.Background(), "OPS")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if projects == nil || len(projects) != 0 {
		t.Fatalf("projects = %#v, want non-nil empty slice", projects)
	}
	accessible, _ := projectFilter["accessibleTeams"].(map[string]any)
	some, _ := accessible["some"].(map[string]any)
	id, _ := some["id"].(map[string]any)
	if id["eq"] != "team-ops" {
		t.Fatalf("project filter = %#v, want team-ops", projectFilter)
	}
}

func TestListProjectsTeamSelectorRejectsDisplayNameAndDuplicateKey(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		teams   string
		wantErr string
	}{
		{
			name:    "display name is not a selector",
			ref:     "Engineering",
			teams:   `[{"id":"team-1","key":"ENG","name":"Engineering"}]`,
			wantErr: `team key or id "Engineering" not found`,
		},
		{
			name:    "duplicate key is ambiguous",
			ref:     "ENG",
			teams:   `[{"id":"team-1","key":"ENG","name":"One"},{"id":"team-2","key":"ENG","name":"Two"}]`,
			wantErr: `team key or id "ENG" is ambiguous`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req graphqlRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if !strings.Contains(req.Query, "ListTeams") {
					t.Fatalf("unexpected query: %s", req.Query)
				}
				writeGQLData(w, `{"teams":{"nodes":`+tc.teams+`,"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
			})

			projects, err := c.ListProjects(context.Background(), tc.ref)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListProjects error = %v, want %q", err, tc.wantErr)
			}
			if projects != nil {
				t.Fatalf("ListProjects returned projects: %#v", projects)
			}
		})
	}
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
	if gotAuth != "test-fixture-key-not-a-secret" {
		t.Errorf("Authorization = %q, want raw API key without Bearer prefix", gotAuth)
	}
}

// TestProxyModeSendsBearerAuthHeader pins the auth-header branch added by
// ADR-2026-05-12-cli-linear-proxy. ProxyMode=true must produce
// `Authorization: Bearer <token>` (platform proxy semantics), whereas
// direct mode keeps the raw header value (Linear API semantics).
func TestProxyModeSendsBearerAuthHeader(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeGQLData(w, `{"issues":{"nodes":[]}}`)
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		BaseURL: srv.URL,
		// #nosec G101 -- test fixture; not a real credential
		APIKey:     "rsk_test_token",
		HTTPClient: srv.Client(),
		ProxyMode:  true,
	}
	_, _ = c.ListIssuesByProject(context.Background(), "proj", nil)
	const want = "Bearer rsk_test_token"
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestNewProxiedClient pins the constructor — base URL composition, the
// proxy mode flag, the input validation, and that the resulting client
// produces a Bearer-style auth header when used.
func TestNewProxiedClient(t *testing.T) {
	t.Parallel()

	if _, err := NewProxiedClient("", "rsk_x"); err == nil {
		t.Fatal("empty platform base URL: want error, got nil")
	}
	if _, err := NewProxiedClient("https://platform.example.com", ""); err == nil {
		t.Fatal("empty rsk token: want error, got nil")
	}
	if _, err := NewProxiedClient("   ", "rsk_x"); err == nil {
		t.Fatal("whitespace platform base URL: want error, got nil")
	}

	c, err := NewProxiedClient("https://platform.example.com/", "rsk_abc")
	if err != nil {
		t.Fatalf("constructor: unexpected error: %v", err)
	}
	if c.BaseURL != "https://platform.example.com/api/cli/linear/graphql" {
		t.Errorf("BaseURL = %q, want trailing slash stripped + path appended", c.BaseURL)
	}
	if !c.ProxyMode {
		t.Error("ProxyMode = false, want true")
	}
	if c.APIKey != "rsk_abc" {
		t.Errorf("APIKey = %q, want %q", c.APIKey, "rsk_abc")
	}
}

// --- ListIssuesByProject ---

func TestListIssuesByProjectSuccess(t *testing.T) {
	t.Parallel()
	nodes := []map[string]any{
		{
			"id":         "issue-1",
			"identifier": "ENG-1",
			"title":      "First issue",
			"state":      map[string]any{"name": "In Progress"},
			"project":    map[string]any{"name": "MyProject"},
			"parent":     nil,
		},
		{
			"id":         "issue-2",
			"identifier": "ENG-2",
			"title":      "Second issue",
			"state":      map[string]any{"name": "Todo"},
			"project":    map[string]any{"name": "MyProject"},
			"parent":     map[string]any{"id": "issue-0"},
		},
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"issues":{"nodes":`+issueNodesJSON(nodes)+`}}`)
	})

	issues, err := c.ListIssuesByProject(context.Background(), "MyProject", []string{"In Progress", "Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByProject: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].ID != "issue-1" || issues[0].Identifier != "ENG-1" {
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
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGQLData(w, `{"issue":{"id":"issue-1","identifier":"ENG-1","title":"Some issue","state":{"name":"Done"},"project":{"name":"MyProj"},"parent":null}}`)
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
			"identifier": "ENG-10",
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
