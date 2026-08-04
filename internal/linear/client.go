package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// uuidRE matches a canonical UUID (the shape Linear uses for entity
// ids: team/project/issue ids). Used by the team/project resolvers to
// recognise when the caller passed an id rather than a key/name so the
// query can match on the `id` field directly.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// looksLikeID reports whether s has the canonical UUID shape Linear
// uses for entity identifiers.
func looksLikeID(s string) bool {
	return uuidRE.MatchString(s)
}

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

	// queryListBacklogIssues filters a project's issues by a caller-supplied
	// set of workflow-state names ($states: [String!]). The parent-null
	// clause that restricts the result to top-level (parent) issues is
	// appended dynamically by buildListBacklogQuery — Linear's IssueFilter
	// is structurally typed, so a `parent: { null: true }` predicate cannot
	// be made conditional from inside a single static query string. The two
	// %s placeholders carry the parent clause (empty when parents-only is
	// off) inside the filter and an empty trailer respectively.
	queryListBacklogIssuesFmt = `query ListBacklogIssues($projectId: ID!, $states: [String!]) {
  issues(filter: { project: { id: { eq: $projectId } }, state: { name: { in: $states } }%s }) {
    nodes { ...IssueFields }
  }
}
` + issueFragment

	// parentNullFilterClause is the IssueFilter predicate that restricts a
	// query to top-level (parent) issues — issues whose `parent` is null.
	// Verified against Linear's IssueFilter schema: the nullable-relation
	// predicate is `parent: { null: true }` (a NullableParentFilter), NOT
	// `parent: { id: { null: true } }`. Spliced into queryListBacklogIssuesFmt
	// when parents-only listing is requested.
	parentNullFilterClause = `, parent: { null: true }`

	queryListComments = `query ListComments($issueId: String!) {
  issue(id: $issueId) {
    comments { nodes { id body createdAt user { id name } } }
  }
}`

	queryListRelations = `query ListRelations($issueId: String!, $relationsAfter: String, $inverseRelationsAfter: String) {
  issue(id: $issueId) {
    relations(first: 100, after: $relationsAfter) {
      nodes { id type relatedIssue { id identifier } createdAt }
      pageInfo { hasNextPage endCursor }
    }
    inverseRelations(first: 100, after: $inverseRelationsAfter) {
      nodes { id type issue { id identifier } createdAt }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

	// $teamId must be ID! — Linear's schema rejects String! at this filter
	// position with "Variable $teamId of type String! used in position
	// expecting type ID." Sibling queries (queryListSubIssues,
	// queryListBacklogIssues) already type their filter-id variables as
	// ID!; this one was the outlier (REN — caught by the CLI Linear proxy
	// 2026-05-12 once normalized errors surfaced).
	queryListWorkflowStates = `query ListWorkflowStates($teamId: ID!) {
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

	// ProxyMode toggles the Authorization header shape (per
	// ADR-2026-05-12-cli-linear-proxy):
	//   - false (default): `Authorization: <APIKey>` — direct Linear API
	//     calls where APIKey holds a Linear-issued lin_api_* / OAuth token.
	//   - true: `Authorization: Bearer <APIKey>` — calls go through the
	//     platform's /api/cli/linear/graphql proxy where APIKey holds the
	//     user's platform rsk_ token. The platform unwraps the rsk_, looks
	//     up the org's stored Linear OAuth credential, and forwards the
	//     GraphQL under that credential.
	// BaseURL is the only other thing that changes between the two modes;
	// every query/mutation string and response decoder is identical.
	ProxyMode bool
}

// NewClient constructs a Client for direct Linear API calls.
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

// NewProxiedClient constructs a Client that routes GraphQL through the
// platform's /api/cli/linear/graphql proxy under the caller's rsk_ token.
// platformBaseURL is the platform root (e.g. "https://platform.example.com");
// rskToken is the user's platform API key (Bearer-style). Returns
// ErrInvalidAPIKey when either is empty.
//
// The returned client speaks the exact same GraphQL queries as a direct
// Linear client — the proxy mirrors api.linear.app/graphql's envelope.
func NewProxiedClient(platformBaseURL, rskToken string) (*Client, error) {
	if strings.TrimSpace(rskToken) == "" {
		return nil, ErrInvalidAPIKey
	}
	if strings.TrimSpace(platformBaseURL) == "" {
		return nil, fmt.Errorf("linear: platform base URL is required")
	}
	// Strip trailing slash for clean URL composition.
	base := strings.TrimRight(platformBaseURL, "/")
	return &Client{
		BaseURL:    base + "/api/cli/linear/graphql",
		APIKey:     rskToken,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		ProxyMode:  true,
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
	if c.ProxyMode {
		// Platform proxy expects an rsk_ token in Bearer-style.
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	} else {
		// Linear's own GraphQL API expects the token as the raw header
		// value (no Bearer prefix). This is per Linear's docs and has
		// been stable since their public API launched.
		req.Header.Set("Authorization", c.APIKey)
	}

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

// GetIssue fetches a single issue by its Linear ID or identifier (e.g. "ENG-42").
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

// ListBacklogIssues returns the issues in the project whose workflow
// state matches one of the supplied state names. When states is empty
// it defaults to {"Backlog"} (the historical behaviour). When
// parentsOnly is true the result is restricted to top-level issues
// (parent == null) so a grooming pass enumerates parents and cascades
// into sub-issues itself rather than treating a sub-issue as a
// standalone target.
func (c *Client) ListBacklogIssues(ctx context.Context, projectID string, states []string, parentsOnly bool) ([]Issue, error) {
	if len(states) == 0 {
		states = []string{"Backlog"}
	}
	vars := map[string]any{
		"projectId": projectID,
		"states":    states,
	}
	var data listIssuesData
	if err := c.do(ctx, buildListBacklogQuery(parentsOnly), vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}

// buildListBacklogQuery renders the backlog-issues query, splicing the
// parent-null filter clause into the IssueFilter when parentsOnly is
// requested. Kept separate so the (small) string assembly is unit-
// testable and the format placeholder is never exposed to callers.
func buildListBacklogQuery(parentsOnly bool) string {
	clause := ""
	if parentsOnly {
		clause = parentNullFilterClause
	}
	return fmt.Sprintf(queryListBacklogIssuesFmt, clause)
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
	res := &RelationsResult{IssueID: issueID}
	var relationsAfter, inverseRelationsAfter *string
	relationsComplete, inverseRelationsComplete := false, false

	const maxRelationPages = 100
	for page := 0; page < maxRelationPages; page++ {
		vars := map[string]any{
			"issueId":               issueID,
			"relationsAfter":        relationsAfter,
			"inverseRelationsAfter": inverseRelationsAfter,
		}
		var data listRelationsData
		if err := c.do(ctx, queryListRelations, vars, &data); err != nil {
			return nil, err
		}
		if data.Issue == nil {
			return nil, fmt.Errorf("incomplete relations response for issue %q: issue is null", issueID)
		}
		if data.Issue.Relations == nil {
			return nil, fmt.Errorf("incomplete relations response for issue %q: relations connection is missing", issueID)
		}
		if data.Issue.InverseRelations == nil {
			return nil, fmt.Errorf("incomplete relations response for issue %q: inverseRelations connection is missing", issueID)
		}
		if data.Issue.Relations.Nodes == nil {
			return nil, fmt.Errorf("incomplete relations response for issue %q: relations nodes are missing", issueID)
		}
		if data.Issue.InverseRelations.Nodes == nil {
			return nil, fmt.Errorf("incomplete relations response for issue %q: inverseRelations nodes are missing", issueID)
		}

		if !relationsComplete {
			for i, n := range *data.Issue.Relations.Nodes {
				if err := validateRelationNode("relations", i, n, false); err != nil {
					return nil, fmt.Errorf("incomplete relations response for issue %q: %w", issueID, err)
				}
				e := RelationEntry{
					ID:        n.ID,
					Type:      *n.Type,
					CreatedAt: n.CreatedAt,
				}
				e.RelatedIssueID = n.RelatedIssue.ID
				e.RelatedIssueIdentifier = n.RelatedIssue.Identifier
				res.Relations = append(res.Relations, e)
			}
			next, complete, err := nextRelationPage("relations", data.Issue.Relations.PageInfo, relationsAfter)
			if err != nil {
				return nil, fmt.Errorf("incomplete relations response for issue %q: %w", issueID, err)
			}
			relationsAfter, relationsComplete = next, complete
		}

		if !inverseRelationsComplete {
			for i, n := range *data.Issue.InverseRelations.Nodes {
				if err := validateRelationNode("inverseRelations", i, n, true); err != nil {
					return nil, fmt.Errorf("incomplete relations response for issue %q: %w", issueID, err)
				}
				e := InverseRelationEntry{
					ID:        n.ID,
					Type:      *n.Type,
					CreatedAt: n.CreatedAt,
				}
				e.IssueID = n.Issue.ID
				e.IssueIdentifier = n.Issue.Identifier
				res.InverseRelations = append(res.InverseRelations, e)
			}
			next, complete, err := nextRelationPage("inverseRelations", data.Issue.InverseRelations.PageInfo, inverseRelationsAfter)
			if err != nil {
				return nil, fmt.Errorf("incomplete relations response for issue %q: %w", issueID, err)
			}
			inverseRelationsAfter, inverseRelationsComplete = next, complete
		}

		if relationsComplete && inverseRelationsComplete {
			return res, nil
		}
	}

	return nil, fmt.Errorf("incomplete relations response for issue %q: exceeded %d pages", issueID, maxRelationPages)
}

func validateRelationNode(connection string, index int, node *relationNode, inverse bool) error {
	if node == nil {
		return fmt.Errorf("%s node %d is null", connection, index)
	}
	if node.Type == nil {
		return fmt.Errorf("%s node %d type is missing", connection, index)
	}
	if *node.Type == "" {
		return fmt.Errorf("%s node %d type is empty", connection, index)
	}
	if !isKnownRelationType(*node.Type) {
		return fmt.Errorf("%s node %d has unknown type %q", connection, index, *node.Type)
	}
	if node.ID == "" {
		return fmt.Errorf("%s node %d id is missing", connection, index)
	}
	if node.CreatedAt == nil {
		return fmt.Errorf("%s node %d createdAt is missing", connection, index)
	}
	if inverse {
		if node.Issue == nil {
			return fmt.Errorf("%s node %d issue is missing", connection, index)
		}
		if node.Issue.ID == "" {
			return fmt.Errorf("%s node %d issue id is missing", connection, index)
		}
		return nil
	}
	if node.RelatedIssue == nil {
		return fmt.Errorf("%s node %d relatedIssue is missing", connection, index)
	}
	if node.RelatedIssue.ID == "" {
		return fmt.Errorf("%s node %d relatedIssue id is missing", connection, index)
	}
	return nil
}

func isKnownRelationType(relationType string) bool {
	switch relationType {
	case "related", "blocks", "duplicate":
		return true
	default:
		return false
	}
}

func nextRelationPage(name string, pageInfo *connectionPageInfo, current *string) (*string, bool, error) {
	if pageInfo == nil || pageInfo.HasNextPage == nil {
		return nil, false, fmt.Errorf("%s pageInfo is missing", name)
	}
	if !*pageInfo.HasNextPage {
		return nil, true, nil
	}
	if pageInfo.EndCursor == nil || *pageInfo.EndCursor == "" {
		return nil, false, fmt.Errorf("%s hasNextPage without endCursor", name)
	}
	if current != nil && *current == *pageInfo.EndCursor {
		return nil, false, fmt.Errorf("%s cursor did not advance", name)
	}
	return pageInfo.EndCursor, false, nil
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

// GetTeamByName returns the team identified by the given key, name
// (case-insensitive), or id/UUID. The platform feeds a team UUID in
// some dispatch paths, so the resolver matches an `id` predicate too
// (added only when the input has the canonical UUID shape, so a
// non-UUID key/name is never sent to the ID comparator).
func (c *Client) GetTeamByName(ctx context.Context, nameOrKeyOrID string) (*Team, error) {
	or := []map[string]any{
		{"key": map[string]any{"eqIgnoreCase": nameOrKeyOrID}},
		{"name": map[string]any{"eqIgnoreCase": nameOrKeyOrID}},
	}
	if looksLikeID(nameOrKeyOrID) {
		or = append(or, map[string]any{"id": map[string]any{"eq": nameOrKeyOrID}})
	}
	vars := map[string]any{
		"filter": map[string]any{"or": or},
	}
	var data listTeamsData
	if err := c.do(ctx, queryListTeams, vars, &data); err != nil {
		return nil, err
	}
	if len(data.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("%w: team %q", ErrNotFound, nameOrKeyOrID)
	}
	n := data.Teams.Nodes[0]
	return &Team{ID: n.ID, Key: n.Key, Name: n.Name}, nil
}

// GetProjectByName returns the project with the given name
// (case-insensitive). It is a thin wrapper over GetProjectByNameInTeam
// with no team disambiguation.
func (c *Client) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	return c.GetProjectByNameInTeam(ctx, name, "")
}

// GetProjectByNameInTeam returns the project with the given name
// (case-insensitive), optionally scoped to a team to disambiguate
// same-named projects across teams. teamID may be a team UUID or a
// team key/name (resolved to an id first); empty disables the team
// filter and the resolver behaves like GetProjectByName.
func (c *Client) GetProjectByNameInTeam(ctx context.Context, name, teamID string) (*Project, error) {
	filter := map[string]any{
		"name": map[string]any{"eqIgnoreCase": name},
	}
	if teamID != "" {
		resolvedTeamID := teamID
		// A key/name (non-UUID) must be resolved to an id before it can
		// scope the project filter; a UUID is used as-is.
		if !looksLikeID(teamID) {
			t, err := c.GetTeamByName(ctx, teamID)
			if err != nil {
				return nil, fmt.Errorf("resolve team for project scope: %w", err)
			}
			resolvedTeamID = t.ID
		}
		// ProjectFilter scopes by team membership via accessibleTeams.
		filter["accessibleTeams"] = map[string]any{
			"some": map[string]any{
				"id": map[string]any{"eq": resolvedTeamID},
			},
		}
	}
	vars := map[string]any{"filter": filter}
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
	if input.Priority != nil {
		inp["priority"] = *input.Priority
	}
	if input.Estimate != nil {
		inp["estimate"] = *input.Estimate
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
