package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func writeIssuePage(w http.ResponseWriter, nodes []issueNode, hasNext bool, endCursor *string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			"issues": map[string]any{
				"nodes": nodes,
				"pageInfo": map[string]any{
					"hasNextPage": hasNext,
					"endCursor":   endCursor,
				},
			},
		},
	})
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

func TestListLabelsPaginates(t *testing.T) {
	var cursors []any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "ListLabels") {
			t.Fatalf("query = %q, want ListLabels", req.Query)
		}
		if req.Variables["filter"] != nil {
			t.Fatalf("unscoped label filter = %#v, want nil", req.Variables["filter"])
		}
		cursors = append(cursors, req.Variables["after"])
		if len(cursors) == 1 {
			writeGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-b","name":"Bug"}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}`)
			return
		}
		writeGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-f","name":"Feature"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})

	labels, err := c.ListLabels(context.Background())
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != 2 || labels["Bug"] != "label-b" || labels["Feature"] != "label-f" {
		t.Fatalf("labels = %#v", labels)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "cursor-1" {
		t.Fatalf("after cursors = %#v, want [nil cursor-1]", cursors)
	}
}

func TestListLabelsForTeamIncludesTeamAndWorkspaceLabels(t *testing.T) {
	var labelFilter map[string]any
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
		case strings.Contains(req.Query, "ListLabels"):
			labelFilter, _ = req.Variables["filter"].(map[string]any)
			writeGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-team","name":"Team label"},{"id":"label-workspace","name":"Workspace label"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	labels, err := c.ListLabelsForTeam(context.Background(), "ENG")
	if err != nil {
		t.Fatalf("ListLabelsForTeam: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("labels = %#v", labels)
	}
	or, ok := labelFilter["or"].([]any)
	if !ok || len(or) != 2 {
		t.Fatalf("label filter = %#v, want two OR clauses", labelFilter)
	}
	var sawTeam, sawWorkspace bool
	for _, rawClause := range or {
		clause, _ := rawClause.(map[string]any)
		team, _ := clause["team"].(map[string]any)
		if team["null"] == true {
			sawWorkspace = true
		}
		id, _ := team["id"].(map[string]any)
		if id["eq"] == "team-1" {
			sawTeam = true
		}
	}
	if !sawTeam || !sawWorkspace {
		t.Fatalf("label filter = %#v, want selected-team and workspace clauses", labelFilter)
	}
}

func TestListLabelsFailsClosedOnMalformedCatalog(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "missing connection",
			data:    `{}`,
			wantErr: "issue labels connection is missing",
		},
		{
			name:    "missing nodes",
			data:    `{"issueLabels":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "issue labels nodes are missing",
		},
		{
			name:    "missing cursor",
			data:    `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}`,
			wantErr: "issue labels hasNextPage without endCursor",
		},
		{
			name:    "missing label name",
			data:    `{"issueLabels":{"nodes":[{"id":"label-1"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			wantErr: "issue labels node 0 name is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, tc.data)
			})
			labels, err := c.ListLabels(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListLabels error = %v, want %q", err, tc.wantErr)
			}
			if labels != nil {
				t.Fatalf("ListLabels returned partial labels: %#v", labels)
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

func TestListIssuesPaginatesAboveLinearPageLimit(t *testing.T) {
	t.Parallel()

	var requests []map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, req.Variables)
		if !strings.Contains(req.Query, "$after: String") || !strings.Contains(req.Query, "pageInfo { hasNextPage endCursor }") {
			t.Fatalf("query does not request cursor pagination: %s", req.Query)
		}

		start := 0
		count := 250
		hasNext := true
		var cursor *string
		if req.Variables["after"] != nil {
			start = 250
			count = 50
			hasNext = false
		} else {
			value := "page-1"
			cursor = &value
		}
		nodes := make([]issueNode, count)
		for i := range nodes {
			nodes[i].ID = fmt.Sprintf("issue-%03d", start+i)
			nodes[i].Identifier = fmt.Sprintf("ENG-%d", start+i)
			nodes[i].Title = fmt.Sprintf("Issue %d", start+i)
		}
		writeIssuePage(w, nodes, hasNext, cursor)
	})

	filter := map[string]any{"team": map[string]any{"id": map[string]any{"eq": "team-1"}}}
	issues, err := c.ListIssues(context.Background(), filter, 300, "updatedAt")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 300 || issues[0].ID != "issue-000" || issues[299].ID != "issue-299" {
		t.Fatalf("issues length/order = %d, first=%q last=%q", len(issues), issues[0].ID, issues[len(issues)-1].ID)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[0]["first"] != float64(250) || requests[0]["after"] != nil {
		t.Fatalf("first request variables = %#v", requests[0])
	}
	if requests[1]["first"] != float64(50) || requests[1]["after"] != "page-1" {
		t.Fatalf("second request variables = %#v", requests[1])
	}
	if requests[0]["orderBy"] != "updatedAt" {
		t.Fatalf("orderBy = %#v, want updatedAt", requests[0]["orderBy"])
	}
}

func TestListIssuesDeduplicatesAndPreservesFirstSeenOrder(t *testing.T) {
	t.Parallel()

	var afters []any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		afters = append(afters, req.Variables["after"])
		issue := func(id string) issueNode {
			return issueNode{ID: id, Identifier: strings.ToUpper(id), Title: id}
		}
		switch len(afters) {
		case 1:
			cursor := "page-1"
			writeIssuePage(w, []issueNode{issue("a"), issue("b"), issue("c")}, true, &cursor)
		case 2:
			if req.Variables["first"] != float64(1) {
				t.Fatalf("second page first = %#v, want 1", req.Variables["first"])
			}
			cursor := "page-2"
			writeIssuePage(w, []issueNode{issue("c")}, true, &cursor)
		default:
			writeIssuePage(w, []issueNode{issue("d")}, false, nil)
		}
	})

	issues, err := c.ListIssues(context.Background(), nil, 4, "createdAt")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	got := make([]string, len(issues))
	for i := range issues {
		got[i] = issues[i].ID
	}
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("issue order = %v, want [a b c d]", got)
	}
	if fmt.Sprint(afters) != "[<nil> page-1 page-2]" {
		t.Fatalf("after cursors = %v", afters)
	}
}

func TestListIssuesRejectsInvalidLimitsWithoutRequest(t *testing.T) {
	t.Parallel()

	requests := 0
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) { requests++ })
	for _, limit := range []int{0, -1, MaxIssueListLimit + 1} {
		issues, err := c.ListIssues(context.Background(), nil, limit, "createdAt")
		if err == nil {
			t.Fatalf("limit %d: want error", limit)
		}
		if issues != nil {
			t.Fatalf("limit %d: returned partial issues %#v", limit, issues)
		}
	}
	if requests != 0 {
		t.Fatalf("network requests = %d, want 0", requests)
	}
}

func TestListIssuesFailsClosedOnMalformedConnection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{name: "missing connection", data: `{}`, wantErr: "issues connection is missing"},
		{name: "missing nodes", data: `{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, wantErr: "issues nodes are missing"},
		{name: "missing page info", data: `{"issues":{"nodes":[]}}`, wantErr: "issues pageInfo is missing"},
		{name: "missing has next page", data: `{"issues":{"nodes":[],"pageInfo":{"endCursor":null}}}`, wantErr: "issues pageInfo is missing"},
		{name: "missing cursor", data: `{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}`, wantErr: "issues hasNextPage without endCursor"},
		{name: "missing issue id", data: `{"issues":{"nodes":[{"identifier":"ENG-1"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, wantErr: "issues node 0 id is missing"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, tc.data)
			})
			issues, err := c.ListIssues(context.Background(), nil, 10, "createdAt")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ListIssues error = %v, want %q", err, tc.wantErr)
			}
			if issues != nil {
				t.Fatalf("returned partial issues: %#v", issues)
			}
		})
	}
}

func TestListIssuesRejectsPageLargerThanRequested(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeIssuePage(w, []issueNode{{ID: "issue-1"}, {ID: "issue-2"}}, false, nil)
	})
	issues, err := c.ListIssues(context.Background(), nil, 1, "createdAt")
	if err == nil || !strings.Contains(err.Error(), "issues returned 2 nodes for page size 1") {
		t.Fatalf("ListIssues error = %v, want oversized-page error", err)
	}
	if issues != nil {
		t.Fatalf("returned partial issues: %#v", issues)
	}
}

func TestListIssuesFailsClosedOnCursorCycle(t *testing.T) {
	t.Parallel()

	request := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		request++
		cursor := "page-1"
		if request == 2 {
			cursor = "page-2"
		}
		writeIssuePage(w, []issueNode{}, true, &cursor)
	})

	issues, err := c.ListIssues(context.Background(), nil, 1, "createdAt")
	if err == nil || !strings.Contains(err.Error(), `issues cursor cycle detected at "page-1"`) {
		t.Fatalf("ListIssues error = %v, want cursor-cycle error", err)
	}
	if issues != nil {
		t.Fatalf("returned partial issues: %#v", issues)
	}
}

func TestListIssuesDiscardsPartialResultsWhenLaterPageFails(t *testing.T) {
	t.Parallel()

	request := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		request++
		if request == 1 {
			cursor := "page-1"
			writeIssuePage(w, []issueNode{{ID: "issue-1", Identifier: "ENG-1"}}, true, &cursor)
			return
		}
		writeGQLError(w, "later page denied")
	})

	issues, err := c.ListIssues(context.Background(), nil, 2, "createdAt")
	if !errors.Is(err, ErrGraphQLError) {
		t.Fatalf("ListIssues error = %v, want ErrGraphQLError", err)
	}
	if issues != nil {
		t.Fatalf("returned partial issues: %#v", issues)
	}
}

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

// --- Issue label mutations ---

func TestCreateIssueLabelUsesExplicitTeamScope(t *testing.T) {
	t.Parallel()
	var gotInput map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "issueLabelCreate") {
			t.Fatalf("query = %q, want issueLabelCreate", req.Query)
		}
		gotInput, _ = req.Variables["input"].(map[string]any)
		writeGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"label-1","name":"Security"}}}`)
	})

	label, err := c.CreateIssueLabel(context.Background(), "Security", "team-1")
	if err != nil {
		t.Fatalf("CreateIssueLabel: %v", err)
	}
	if label.ID != "label-1" || label.Name != "Security" {
		t.Fatalf("label = %#v", label)
	}
	if gotInput["name"] != "Security" || gotInput["teamId"] != "team-1" {
		t.Fatalf("input = %#v, want name and teamId", gotInput)
	}
}

func TestCreateIssueLabelRejectsMissingTeamWithoutRequest(t *testing.T) {
	t.Parallel()
	requests := 0
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		requests++
	})

	label, err := c.CreateIssueLabel(context.Background(), "Security", "")
	if err == nil || !strings.Contains(err.Error(), "team id is required") {
		t.Fatalf("CreateIssueLabel error = %v", err)
	}
	if label != nil || requests != 0 {
		t.Fatalf("label = %#v, requests = %d; want no request", label, requests)
	}
}

func TestAddIssueLabelUsesAdditiveMutation(t *testing.T) {
	t.Parallel()
	var gotVars map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "issueAddLabel") || strings.Contains(req.Query, "issueUpdate") {
			t.Fatalf("query = %q, want only issueAddLabel", req.Query)
		}
		gotVars = req.Variables
		writeGQLData(w, `{"issueAddLabel":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1","title":"Issue","state":{"name":"Backlog"},"team":{"id":"team-1","key":"ENG","name":"Engineering"},"labels":{"nodes":[{"id":"label-1","name":"Security"}]}}}}`)
	})

	issue, err := c.AddIssueLabel(context.Background(), "issue-1", "label-1")
	if err != nil {
		t.Fatalf("AddIssueLabel: %v", err)
	}
	if gotVars["id"] != "issue-1" || gotVars["labelId"] != "label-1" {
		t.Fatalf("variables = %#v", gotVars)
	}
	if issue.ID != "issue-1" || len(issue.Labels) != 1 || issue.Labels[0].ID != "label-1" {
		t.Fatalf("issue = %#v", issue)
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
