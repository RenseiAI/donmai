package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// capturedRequest records the last GraphQL request a captureClient
// server received so a test can assert on the query string and the
// variables map.
type capturedRequest struct {
	query     string
	variables map[string]any
}

// captureClient returns a Client whose server records the last GraphQL
// request body and responds with the supplied data JSON. The captured
// request is exposed via the returned *capturedRequest pointer.
func captureClient(t *testing.T, dataJSON string) (*Client, *capturedRequest) {
	t.Helper()
	rec := &capturedRequest{}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		rec.query = req.Query
		rec.variables = req.Variables
		writeGQLData(w, dataJSON)
	})
	return c, rec
}

// --- buildListBacklogQuery ---

func TestBuildListBacklogQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		parentsOnly     bool
		wantParentNull  bool
		wantStatesParam bool
	}{
		{"parents-only on splices parent-null", true, true, true},
		{"parents-only off omits parent-null", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := buildListBacklogQuery(tc.parentsOnly)
			if got := strings.Contains(q, "parent: { null: true }"); got != tc.wantParentNull {
				t.Errorf("parent-null clause present = %v; want %v\nquery:\n%s", got, tc.wantParentNull, q)
			}
			if got := strings.Contains(q, "$states: [String!]"); got != tc.wantStatesParam {
				t.Errorf("$states param present = %v; want %v", got, tc.wantStatesParam)
			}
			if !strings.Contains(q, "state: { name: { in: $states } }") {
				t.Errorf("query missing states-in filter:\n%s", q)
			}
		})
	}
}

// --- ListBacklogIssues ---

func TestListBacklogIssues_DefaultStatusIsBacklog(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, `{"issues":{"nodes":[]}}`)

	if _, err := c.ListBacklogIssues(context.Background(), "proj-1", nil, false); err != nil {
		t.Fatalf("ListBacklogIssues: %v", err)
	}
	states, _ := rec.variables["states"].([]any)
	if len(states) != 1 || states[0] != "Backlog" {
		t.Errorf("default states = %v; want [Backlog]", rec.variables["states"])
	}
	if rec.variables["projectId"] != "proj-1" {
		t.Errorf("projectId = %v; want proj-1", rec.variables["projectId"])
	}
	if strings.Contains(rec.query, "parent: { null: true }") {
		t.Errorf("parent-null clause present without parents-only:\n%s", rec.query)
	}
}

func TestListBacklogIssues_StatusesAndParentsOnly(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, `{"issues":{"nodes":[]}}`)

	if _, err := c.ListBacklogIssues(context.Background(), "proj-1", []string{"Icebox", "Backlog"}, true); err != nil {
		t.Fatalf("ListBacklogIssues: %v", err)
	}
	states, _ := rec.variables["states"].([]any)
	if len(states) != 2 || states[0] != "Icebox" || states[1] != "Backlog" {
		t.Errorf("states = %v; want [Icebox Backlog]", rec.variables["states"])
	}
	if !strings.Contains(rec.query, "parent: { null: true }") {
		t.Errorf("parents-only query missing parent-null clause:\n%s", rec.query)
	}
}

func TestListBacklogIssues_ProjectsParentIDIntoIssues(t *testing.T) {
	t.Parallel()
	// A parent issue (no parent) and a sub-issue (parent set) — confirm the
	// ParentID round-trips so callers can assert top-level vs sub.
	nodes := []map[string]any{
		{"id": "p-1", "identifier": "ENG-1", "title": "Parent", "state": map[string]any{"name": "Icebox"}},
		{"id": "c-1", "identifier": "ENG-2", "title": "Child", "state": map[string]any{"name": "Icebox"}, "parent": map[string]any{"id": "p-1"}},
	}
	c, _ := captureClient(t, `{"issues":{"nodes":`+issueNodesJSON(nodes)+`}}`)

	issues, err := c.ListBacklogIssues(context.Background(), "proj-1", []string{"Icebox"}, false)
	if err != nil {
		t.Fatalf("ListBacklogIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues; want 2", len(issues))
	}
	if issues[0].ParentID != "" {
		t.Errorf("parent issue ParentID = %q; want empty", issues[0].ParentID)
	}
	if issues[1].ParentID != "p-1" {
		t.Errorf("sub issue ParentID = %q; want p-1", issues[1].ParentID)
	}
}

// --- GetTeamByName / by UUID ---

func TestGetTeamByName_ResolvesByKeyOrName(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, `{"teams":{"nodes":[{"id":"team-uuid","key":"ENG","name":"Engineering"}]}}`)

	team, err := c.GetTeamByName(context.Background(), "ENG")
	if err != nil {
		t.Fatalf("GetTeamByName: %v", err)
	}
	if team.ID != "team-uuid" || team.Key != "ENG" {
		t.Errorf("team = %+v; want id=team-uuid key=ENG", team)
	}
	// The filter must NOT include an id predicate for a non-UUID input.
	assertNoIDPredicate(t, rec)
}

func TestGetTeamByName_ResolvesByUUID(t *testing.T) {
	t.Parallel()
	const teamUUID = "11111111-2222-3333-4444-555555555555"
	c, rec := captureClient(t, `{"teams":{"nodes":[{"id":"`+teamUUID+`","key":"ENG","name":"Engineering"}]}}`)

	team, err := c.GetTeamByName(context.Background(), teamUUID)
	if err != nil {
		t.Fatalf("GetTeamByName(uuid): %v", err)
	}
	if team.ID != teamUUID {
		t.Errorf("team.ID = %q; want %q", team.ID, teamUUID)
	}
	// The filter MUST include an id predicate when the input is a UUID.
	or, ok := nestedOr(rec)
	if !ok {
		t.Fatalf("filter.or missing in variables: %#v", rec.variables)
	}
	if !orHasIDPredicate(or, teamUUID) {
		t.Errorf("filter.or missing id predicate for UUID input: %#v", or)
	}
}

// --- GetProjectByNameInTeam ---

func TestGetProjectByName_NoTeamScope(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, `{"projects":{"nodes":[{"id":"proj-1","name":"Platform","slugId":"platform-slug"}]}}`)

	proj, err := c.GetProjectByName(context.Background(), "Platform")
	if err != nil {
		t.Fatalf("GetProjectByName: %v", err)
	}
	if proj.ID != "proj-1" {
		t.Errorf("proj.ID = %q; want proj-1", proj.ID)
	}
	if proj.SlugID != "platform-slug" {
		t.Errorf("proj.SlugID = %q; want platform-slug", proj.SlugID)
	}
	filter, _ := rec.variables["filter"].(map[string]any)
	if _, has := filter["accessibleTeams"]; has {
		t.Errorf("no-team-scope query must not include accessibleTeams: %#v", filter)
	}
	or, ok := nestedOr(rec)
	if !ok || !orHasStringPredicate(or, "name", "Platform") || !orHasStringPredicate(or, "slugId", "Platform") {
		t.Errorf("project resolver filter must match name and slug: %#v", filter)
	}
}

func TestGetProjectByNameInTeam_ScopesByTeamUUID(t *testing.T) {
	t.Parallel()
	const teamUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	c, rec := captureClient(t, `{"projects":{"nodes":[{"id":"proj-1","name":"Platform"}]}}`)

	proj, err := c.GetProjectByNameInTeam(context.Background(), "Platform", teamUUID)
	if err != nil {
		t.Fatalf("GetProjectByNameInTeam: %v", err)
	}
	if proj.ID != "proj-1" {
		t.Errorf("proj.ID = %q; want proj-1", proj.ID)
	}
	filter, _ := rec.variables["filter"].(map[string]any)
	at, ok := filter["accessibleTeams"].(map[string]any)
	if !ok {
		t.Fatalf("team-scoped query missing accessibleTeams: %#v", filter)
	}
	some, _ := at["some"].(map[string]any)
	id, _ := some["id"].(map[string]any)
	if id["eq"] != teamUUID {
		t.Errorf("accessibleTeams.some.id.eq = %v; want %q", id["eq"], teamUUID)
	}
}

func TestGetProjectByNameInTeam_RejectsAmbiguousMatch(t *testing.T) {
	t.Parallel()
	const teamUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	c, _ := captureClient(t, `{"projects":{"nodes":[{"id":"proj-1","name":"Same","slugId":"one"},{"id":"proj-2","name":"Same","slugId":"two"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)

	_, err := c.GetProjectByNameInTeam(context.Background(), "Same", teamUUID)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous-project rejection", err)
	}
}

func TestGetProjectByNameInTeam_UUIDAddsIDPredicate(t *testing.T) {
	t.Parallel()
	const projectUUID = "11111111-2222-3333-4444-555555555555"
	c, rec := captureClient(t, `{"projects":{"nodes":[{"id":"`+projectUUID+`","name":"Platform","slugId":"platform"}]}}`)

	project, err := c.GetProjectByNameInTeam(context.Background(), projectUUID, "")
	if err != nil {
		t.Fatalf("GetProjectByNameInTeam(uuid): %v", err)
	}
	if project.ID != projectUUID {
		t.Fatalf("project.ID = %q, want %q", project.ID, projectUUID)
	}
	or, ok := nestedOr(rec)
	if !ok || !orHasIDPredicate(or, projectUUID) {
		t.Fatalf("filter.or missing UUID predicate: %#v", rec.variables)
	}
}

// --- helpers ---

func assertNoIDPredicate(t *testing.T, rec *capturedRequest) {
	t.Helper()
	or, ok := nestedOr(rec)
	if !ok {
		return
	}
	for _, clause := range or {
		m, _ := clause.(map[string]any)
		if _, has := m["id"]; has {
			t.Errorf("filter.or unexpectedly includes id predicate for non-UUID input: %#v", or)
		}
	}
}

func nestedOr(rec *capturedRequest) ([]any, bool) {
	filter, ok := rec.variables["filter"].(map[string]any)
	if !ok {
		return nil, false
	}
	or, ok := filter["or"].([]any)
	return or, ok
}

func orHasIDPredicate(or []any, wantEq string) bool {
	for _, clause := range or {
		m, _ := clause.(map[string]any)
		id, ok := m["id"].(map[string]any)
		if !ok {
			continue
		}
		if id["eq"] == wantEq {
			return true
		}
	}
	return false
}

func orHasStringPredicate(or []any, field, wantEq string) bool {
	for _, clause := range or {
		m, _ := clause.(map[string]any)
		comparison, ok := m[field].(map[string]any)
		if ok && comparison["eqIgnoreCase"] == wantEq {
			return true
		}
	}
	return false
}

// --- UpdateIssue priority / estimate (C3) ---

// issueUpdateResponseJSON returns a minimal issueUpdate GraphQL response.
func issueUpdateResponseJSON() string {
	return `{"issueUpdate":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1","title":"T","state":{"name":"Backlog"},"team":{"id":"t1","key":"ENG","name":"Engineering"},"labels":{"nodes":[]}}}}`
}

func TestUpdateIssue_ProjectIDSentToLinear(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, issueUpdateResponseJSON())

	if _, err := c.UpdateIssue(context.Background(), "issue-1", UpdateIssueInput{ProjectID: "proj-dest"}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	input, _ := rec.variables["input"].(map[string]any)
	if len(input) != 1 || input["projectId"] != "proj-dest" {
		t.Fatalf("input = %#v, want only projectId=proj-dest", input)
	}
}

func TestUpdateIssue_PrioritySentToLinear(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, issueUpdateResponseJSON())

	p := 1
	_, err := c.UpdateIssue(context.Background(), "issue-1", UpdateIssueInput{Priority: &p})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	inp, _ := rec.variables["input"].(map[string]any)
	if got, ok := inp["priority"].(float64); !ok || int(got) != 1 {
		t.Errorf("input.priority = %v; want 1", inp["priority"])
	}
	if _, has := inp["estimate"]; has {
		t.Errorf("estimate must not appear in input when not set: %#v", inp)
	}
}

func TestUpdateIssue_EstimateSentToLinear(t *testing.T) {
	t.Parallel()
	c, rec := captureClient(t, issueUpdateResponseJSON())

	e := 5
	_, err := c.UpdateIssue(context.Background(), "issue-1", UpdateIssueInput{Estimate: &e})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	inp, _ := rec.variables["input"].(map[string]any)
	if got, ok := inp["estimate"].(float64); !ok || int(got) != 5 {
		t.Errorf("input.estimate = %v; want 5", inp["estimate"])
	}
	if _, has := inp["priority"]; has {
		t.Errorf("priority must not appear in input when not set: %#v", inp)
	}
}

func TestUpdateIssue_PriorityZeroSentToLinear(t *testing.T) {
	t.Parallel()
	// priority=0 (no priority) is a valid Linear value; pointer is non-nil so
	// the zero value MUST be forwarded, not suppressed.
	c, rec := captureClient(t, issueUpdateResponseJSON())

	p := 0
	_, err := c.UpdateIssue(context.Background(), "issue-1", UpdateIssueInput{Priority: &p})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	inp, _ := rec.variables["input"].(map[string]any)
	if _, has := inp["priority"]; !has {
		t.Fatalf("input must contain priority=0 when pointer is non-nil: %#v", inp)
	}
	if got, ok := inp["priority"].(float64); !ok || int(got) != 0 {
		t.Errorf("input.priority = %v; want 0", inp["priority"])
	}
}

func TestUpdateIssue_NilPriorityNotSentToLinear(t *testing.T) {
	t.Parallel()
	// When Priority is nil (flag not passed), the field must not appear in the input.
	c, rec := captureClient(t, issueUpdateResponseJSON())

	_, err := c.UpdateIssue(context.Background(), "issue-1", UpdateIssueInput{})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	inp, _ := rec.variables["input"].(map[string]any)
	if _, has := inp["priority"]; has {
		t.Errorf("priority must not appear in input when nil: %#v", inp)
	}
	if _, has := inp["estimate"]; has {
		t.Errorf("estimate must not appear in input when nil: %#v", inp)
	}
}
