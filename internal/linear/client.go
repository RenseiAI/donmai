package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.linear.app/graphql"

// ─── GraphQL queries and mutations ───────────────────────────────────────────

const (
	// Fragments
	issueFragment = `fragment IssueFields on Issue {
  id identifier title description url priority createdAt updatedAt
  state { id name }
  team { id key name }
  project { id name }
  labels { nodes { id name } }
  parent { id }
  assignee { id name email }
}`

	queryGetIssue = `query GetIssue($id: String!) {
  issue(id: $id) { ...IssueFields }
}
` + issueFragment

	queryListIssuesByProject = `query ListIssuesByProject($name: String!, $states: [String!]) {
  issues(filter: { project: { name: { eq: $name } }, state: { name: { in: $states } } }) {
    nodes { ...IssueFields }
  }
}
` + issueFragment

	queryListIssues = `query ListIssues($filter: IssueFilter, $first: Int, $orderBy: PaginationOrderBy) {
  issues(filter: $filter, first: $first, orderBy: $orderBy) {
    nodes { ...IssueFields }
  }
}
` + issueFragment

	queryListSubIssues = `query ListSubIssues($parentId: ID!) {
  issues(filter: { parent: { id: { eq: $parentId } } }) {
    nodes { ...IssueFields }
  }
}
` + issueFragment

	queryListBacklogIssues = `query ListBacklogIssues($projectId: ID!) {
  issues(filter: { project: { id: { eq: $projectId } }, state: { name: { eqIgnoreCase: "Backlog" } } }) {
    nodes { ...IssueFields }
  }
}
` + issueFragment

	queryListComments = `query ListComments($issueId: String!) {
  issue(id: $issueId) {
    comments { nodes { id body createdAt user { id name } } }
  }
}`

	queryListRelations = `query ListRelations($issueId: String!) {
  issue(id: $issueId) {
    relations {
      nodes { id type relatedIssue { id identifier } createdAt }
    }
    inverseRelations {
      nodes { id type issue { id identifier } createdAt }
    }
  }
}`

	queryListWorkflowStates = `query ListWorkflowStates($teamId: String!) {
  workflowStates(filter: { team: { id: { eq: $teamId } } }) {
    nodes { id name type }
  }
}`

	queryListLabels = `query ListLabels {
  issueLabels { nodes { id name } }
}`

	queryListUsers = `query ListUsers($filter: UserFilter) {
  users(filter: $filter) { nodes { id name email } }
}`

	queryListTeams = `query ListTeams($filter: TeamFilter) {
  teams(filter: $filter) { nodes { id key name } }
}`

	queryListProjects = `query ListProjects($filter: ProjectFilter) {
  projects(filter: $filter) { nodes { id name } }
}`

	queryViewer = `query Viewer { viewer { id name email } }`

	mutationCreateIssue = `mutation CreateIssue($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { ...IssueFields }
  }
}
` + issueFragment

	mutationUpdateIssue = `mutation UpdateIssue($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
    issue { ...IssueFields }
  }
}
` + issueFragment

	mutationCreateComment = `mutation CreateComment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id body createdAt user { id name } }
  }
}`

	mutationCreateRelation = `mutation CreateRelation($issueId: String!, $relatedIssueId: String!, $type: IssueRelationType!) {
  issueRelationCreate(input: { issueId: $issueId, relatedIssueId: $relatedIssueId, type: $type }) {
    success
    issueRelation { id type relatedIssue { id identifier } createdAt }
  }
}`

	mutationDeleteRelation = `mutation DeleteRelation($id: String!) {
  issueRelationDelete(id: $id) { success }
}`
)

// ─── RelationResult is the structured output of list-relations ───────────────

// RelationsResult is returned by GetIssueRelations.
type RelationsResult struct {
	IssueID          string
	Relations        []RelationEntry
	InverseRelations []InverseRelationEntry
}

// RelationEntry is one outgoing relation (issue → related).
type RelationEntry struct {
	ID                     string
	Type                   string
	RelatedIssueID         string
	RelatedIssueIdentifier string
	CreatedAt              *time.Time
}

// InverseRelationEntry is one incoming relation (other → this issue).
type InverseRelationEntry struct {
	ID              string
	Type            string
	IssueID         string
	IssueIdentifier string
	CreatedAt       *time.Time
}

// Client is a lightweight Linear GraphQL client backed by stdlib net/http.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewClient constructs a Client for the given apiKey.
// Returns ErrInvalidAPIKey if apiKey is empty.
func NewClient(apiKey string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrInvalidAPIKey
	}
	return &Client{
		BaseURL:    defaultBaseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// do executes a GraphQL request and decodes the response into out.
func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	payload := graphqlRequest{Query: query, Variables: vars}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("linear: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("linear: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("linear: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := statusToError(resp.StatusCode); err != nil {
		return err
	}

	// Decode the outer envelope; the generic "out" value carries the typed data.
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphqlError  `json:"errors,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("linear: decode response: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("%w: %s", ErrGraphQLError, env.Errors[0].Message)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("linear: decode data: %w", err)
	}
	return nil
}

// statusToError maps HTTP status codes to sentinel errors.
func statusToError(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return ErrForbidden
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrServerError
	default:
		return fmt.Errorf("linear: unexpected status %d", status)
	}
}

// ─── node → public type converters ───────────────────────────────────────────

func nodeToIssue(n issueNode) Issue {
	iss := Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		URL:         n.URL,
		Priority:    n.Priority,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
	}
	iss.State.ID = n.State.ID
	iss.State.Name = n.State.Name
	iss.Team.ID = n.Team.ID
	iss.Team.Key = n.Team.Key
	iss.Team.Name = n.Team.Name
	if n.Project != nil {
		iss.Project.ID = n.Project.ID
		iss.Project.Name = n.Project.Name
	}
	if n.Parent != nil {
		iss.ParentID = n.Parent.ID
	}
	for _, l := range n.Labels.Nodes {
		iss.Labels = append(iss.Labels, Label{ID: l.ID, Name: l.Name})
	}
	if n.Assignee != nil {
		iss.Assignee = &User{ID: n.Assignee.ID, Name: n.Assignee.Name, Email: n.Assignee.Email}
	}
	return iss
}

func nodesToIssues(nodes []issueNode) []Issue {
	out := make([]Issue, len(nodes))
	for i, n := range nodes {
		out[i] = nodeToIssue(n)
	}
	return out
}

func nodeToComment(n commentNode) Comment {
	c := Comment{
		ID:        n.ID,
		Body:      n.Body,
		CreatedAt: n.CreatedAt,
	}
	if n.User != nil {
		c.User = &User{ID: n.User.ID, Name: n.User.Name}
	}
	return c
}

// ─── Read operations ──────────────────────────────────────────────────────────

// GetIssue fetches a single issue by its Linear ID or identifier (e.g. "REN-42").
func (c *Client) GetIssue(ctx context.Context, id string) (*Issue, error) {
	vars := map[string]any{"id": id}
	var data getIssueData
	if err := c.do(ctx, queryGetIssue, vars, &data); err != nil {
		return nil, err
	}
	if data.Issue == nil {
		return nil, ErrNotFound
	}
	iss := nodeToIssue(*data.Issue)
	return &iss, nil
}

// ListIssuesByProject returns issues belonging to the named project, optionally
// filtered to the given state names. Pass nil or empty states to skip the filter.
func (c *Client) ListIssuesByProject(ctx context.Context, projectName string, states []string) ([]Issue, error) {
	vars := map[string]any{"name": projectName}
	if len(states) > 0 {
		vars["states"] = states
	}
	var data listIssuesData
	if err := c.do(ctx, queryListIssuesByProject, vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}

// ListIssues returns issues matching the given filter map.
// filter is sent verbatim as the GraphQL IssueFilter variable.
func (c *Client) ListIssues(ctx context.Context, filter map[string]any, limit int, orderBy string) ([]Issue, error) {
	vars := map[string]any{
		"filter":  filter,
		"first":   limit,
		"orderBy": orderBy,
	}
	var data listIssuesData
	if err := c.do(ctx, queryListIssues, vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}

// ListSubIssues returns the direct children of the given parent issue ID.
func (c *Client) ListSubIssues(ctx context.Context, parentID string) ([]Issue, error) {
	vars := map[string]any{"parentId": parentID}
	var data listIssuesData
	if err := c.do(ctx, queryListSubIssues, vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}

// ListBacklogIssues returns all issues in the named project with state "Backlog".
func (c *Client) ListBacklogIssues(ctx context.Context, projectID string) ([]Issue, error) {
	vars := map[string]any{"projectId": projectID}
	var data listIssuesData
	if err := c.do(ctx, queryListBacklogIssues, vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}

// GetIssueComments returns comments for the given issue ID.
func (c *Client) GetIssueComments(ctx context.Context, issueID string) ([]Comment, error) {
	vars := map[string]any{"issueId": issueID}
	var data listCommentsData
	if err := c.do(ctx, queryListComments, vars, &data); err != nil {
		return nil, err
	}
	out := make([]Comment, len(data.Issue.Comments.Nodes))
	for i, n := range data.Issue.Comments.Nodes {
		out[i] = nodeToComment(n)
	}
	return out, nil
}

// GetIssueRelations returns forward and inverse relations for the given issue ID.
func (c *Client) GetIssueRelations(ctx context.Context, issueID string) (*RelationsResult, error) {
	vars := map[string]any{"issueId": issueID}
	var data listRelationsData
	if err := c.do(ctx, queryListRelations, vars, &data); err != nil {
		return nil, err
	}

	res := &RelationsResult{IssueID: issueID}
	for _, n := range data.Issue.Relations.Nodes {
		e := RelationEntry{
			ID:        n.ID,
			Type:      n.Type,
			CreatedAt: n.CreatedAt,
		}
		if n.RelatedIssue != nil {
			e.RelatedIssueID = n.RelatedIssue.ID
			e.RelatedIssueIdentifier = n.RelatedIssue.Identifier
		}
		res.Relations = append(res.Relations, e)
	}
	for _, n := range data.Issue.InverseRelations.Nodes {
		e := InverseRelationEntry{
			ID:        n.ID,
			Type:      n.Type,
			CreatedAt: n.CreatedAt,
		}
		if n.Issue != nil {
			e.IssueID = n.Issue.ID
			e.IssueIdentifier = n.Issue.Identifier
		}
		res.InverseRelations = append(res.InverseRelations, e)
	}
	return res, nil
}

// ListWorkflowStates returns all workflow states for the given team ID.
// Returns a map of state name → state ID for easy lookup.
func (c *Client) ListWorkflowStates(ctx context.Context, teamID string) (map[string]string, error) {
	vars := map[string]any{"teamId": teamID}
	var data listWorkflowStatesData
	if err := c.do(ctx, queryListWorkflowStates, vars, &data); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(data.WorkflowStates.Nodes))
	for _, n := range data.WorkflowStates.Nodes {
		out[n.Name] = n.ID
	}
	return out, nil
}

// ListLabels returns all issue labels, as a map of label name → label ID.
func (c *Client) ListLabels(ctx context.Context) (map[string]string, error) {
	var data listLabelsData
	if err := c.do(ctx, queryListLabels, nil, &data); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(data.IssueLabels.Nodes))
	for _, n := range data.IssueLabels.Nodes {
		out[n.Name] = n.ID
	}
	return out, nil
}

// GetTeamByName returns the team with the given name or key (case-insensitive).
func (c *Client) GetTeamByName(ctx context.Context, nameOrKey string) (*Team, error) {
	// Try by key first, then by name
	vars := map[string]any{
		"filter": map[string]any{
			"or": []map[string]any{
				{"key": map[string]any{"eqIgnoreCase": nameOrKey}},
				{"name": map[string]any{"eqIgnoreCase": nameOrKey}},
			},
		},
	}
	var data listTeamsData
	if err := c.do(ctx, queryListTeams, vars, &data); err != nil {
		return nil, err
	}
	if len(data.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("%w: team %q", ErrNotFound, nameOrKey)
	}
	n := data.Teams.Nodes[0]
	return &Team{ID: n.ID, Key: n.Key, Name: n.Name}, nil
}

// GetProjectByName returns the project with the given name (case-insensitive).
func (c *Client) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	vars := map[string]any{
		"filter": map[string]any{
			"name": map[string]any{"eqIgnoreCase": name},
		},
	}
	var data listProjectsData
	if err := c.do(ctx, queryListProjects, vars, &data); err != nil {
		return nil, err
	}
	if len(data.Projects.Nodes) == 0 {
		return nil, fmt.Errorf("%w: project %q", ErrNotFound, name)
	}
	n := data.Projects.Nodes[0]
	return &Project{ID: n.ID, Name: n.Name}, nil
}

// GetUserByNameOrEmail returns the user matching the given name or email.
func (c *Client) GetUserByNameOrEmail(ctx context.Context, nameOrEmail string) (*User, error) {
	vars := map[string]any{
		"filter": map[string]any{
			"or": []map[string]any{
				{"name": map[string]any{"eqIgnoreCase": nameOrEmail}},
				{"email": map[string]any{"eq": nameOrEmail}},
			},
		},
	}
	var data listUsersData
	if err := c.do(ctx, queryListUsers, vars, &data); err != nil {
		return nil, err
	}
	if len(data.Users.Nodes) == 0 {
		return nil, fmt.Errorf("%w: user %q", ErrNotFound, nameOrEmail)
	}
	n := data.Users.Nodes[0]
	return &User{ID: n.ID, Name: n.Name, Email: n.Email}, nil
}

// GetViewer returns the currently authenticated user.
func (c *Client) GetViewer(ctx context.Context) (*User, error) {
	var data viewerData
	if err := c.do(ctx, queryViewer, nil, &data); err != nil {
		return nil, err
	}
	v := data.Viewer
	return &User{ID: v.ID, Name: v.Name, Email: v.Email}, nil
}

// ─── Write operations ─────────────────────────────────────────────────────────

// CreateIssue creates a new Linear issue.
func (c *Client) CreateIssue(ctx context.Context, input CreateIssueInput) (*Issue, error) {
	inp := map[string]any{
		"teamId": input.TeamID,
		"title":  input.Title,
	}
	if input.Description != "" {
		inp["description"] = input.Description
	}
	if input.StateID != "" {
		inp["stateId"] = input.StateID
	}
	if input.ProjectID != "" {
		inp["projectId"] = input.ProjectID
	}
	if input.ParentID != "" {
		inp["parentId"] = input.ParentID
	}
	if len(input.LabelIDs) > 0 {
		inp["labelIds"] = input.LabelIDs
	}
	if input.AssigneeID != "" {
		inp["assigneeId"] = input.AssigneeID
	}

	vars := map[string]any{"input": inp}
	var data createIssueData
	if err := c.do(ctx, mutationCreateIssue, vars, &data); err != nil {
		return nil, err
	}
	if !data.IssueCreate.Success {
		return nil, ErrMutationFailed
	}
	iss := nodeToIssue(data.IssueCreate.Issue)
	return &iss, nil
}

// UpdateIssue updates an existing issue. The id can be a UUID or identifier.
func (c *Client) UpdateIssue(ctx context.Context, id string, input UpdateIssueInput) (*Issue, error) {
	inp := map[string]any{}
	if input.Title != "" {
		inp["title"] = input.Title
	}
	if input.Description != "" {
		inp["description"] = input.Description
	}
	if input.StateID != "" {
		inp["stateId"] = input.StateID
	}
	if input.LabelIDs != nil {
		inp["labelIds"] = input.LabelIDs
	}
	if input.ParentID != nil {
		if *input.ParentID == "" {
			inp["parentId"] = nil
		} else {
			inp["parentId"] = *input.ParentID
		}
	}
	if input.AssigneeID != "" {
		inp["assigneeId"] = input.AssigneeID
	}

	vars := map[string]any{"id": id, "input": inp}
	var data updateIssueData
	if err := c.do(ctx, mutationUpdateIssue, vars, &data); err != nil {
		return nil, err
	}
	if !data.IssueUpdate.Success {
		return nil, ErrMutationFailed
	}
	iss := nodeToIssue(data.IssueUpdate.Issue)
	return &iss, nil
}

// CreateComment creates a comment on the given issue.
func (c *Client) CreateComment(ctx context.Context, issueID, body string) (*Comment, error) {
	vars := map[string]any{"issueId": issueID, "body": body}
	var data createCommentData
	if err := c.do(ctx, mutationCreateComment, vars, &data); err != nil {
		return nil, err
	}
	if !data.CommentCreate.Success {
		return nil, ErrMutationFailed
	}
	comment := nodeToComment(data.CommentCreate.Comment)
	return &comment, nil
}

// CreateRelation creates a relation between two issues.
// relationType must be one of: "related", "blocks", "duplicate".
func (c *Client) CreateRelation(ctx context.Context, issueID, relatedIssueID, relationType string) (string, bool, error) {
	vars := map[string]any{
		"issueId":        issueID,
		"relatedIssueId": relatedIssueID,
		"type":           relationType,
	}
	var data createRelationData
	if err := c.do(ctx, mutationCreateRelation, vars, &data); err != nil {
		return "", false, err
	}
	if !data.IssueRelationCreate.Success {
		return "", false, ErrMutationFailed
	}
	return data.IssueRelationCreate.IssueRelation.ID, true, nil
}

// DeleteRelation deletes the relation with the given ID.
func (c *Client) DeleteRelation(ctx context.Context, relationID string) error {
	vars := map[string]any{"id": relationID}
	var data deleteRelationData
	if err := c.do(ctx, mutationDeleteRelation, vars, &data); err != nil {
		return err
	}
	if !data.IssueRelationDelete.Success {
		return ErrMutationFailed
	}
	return nil
}
