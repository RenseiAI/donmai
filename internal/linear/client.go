package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	queryListIssues = `query ListIssues($filter: IssueFilter, $first: Int, $after: String, $orderBy: PaginationOrderBy) {
  issues(filter: $filter, first: $first, after: $after, orderBy: $orderBy) {
    nodes { ...IssueFields }
    pageInfo { hasNextPage endCursor }
  }
}
` + issueFragment

	queryListSubIssues = `query ListSubIssues($parentId: ID!, $first: Int, $after: String) {
  issues(filter: { parent: { id: { eq: $parentId } } }, first: $first, after: $after) {
    nodes { ...IssueFields }
    pageInfo { hasNextPage endCursor }
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

	queryListLabels = `query ListLabels($filter: IssueLabelFilter, $after: String) {
  issueLabels(filter: $filter, first: 100, after: $after) {
    nodes { id name }
    pageInfo { hasNextPage endCursor }
  }
}`

	queryListUsers = `query ListUsers($filter: UserFilter) {
  users(filter: $filter) { nodes { id name email } }
}`

	queryListTeams = `query ListTeams($filter: TeamFilter, $after: String) {
	  teams(filter: $filter, first: 100, after: $after) {
	    nodes { id key name }
	    pageInfo { hasNextPage endCursor }
	  }
}`

	// Keep the outer page at 25 because expanding 100 projects by 100 teams
	// exceeds Linear's 10,000 point query complexity limit.
	queryListProjects = `query ListProjects($filter: ProjectFilter, $after: String) {
	  projects(filter: $filter, first: 25, after: $after) {
	    nodes {
	      id name slugId state
	      teams(first: 100) {
	        nodes { id key name }
	        pageInfo { hasNextPage endCursor }
	      }
	    }
	    pageInfo { hasNextPage endCursor }
	  }
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

	mutationCreateIssueLabel = `mutation CreateIssueLabel($input: IssueLabelCreateInput!) {
  issueLabelCreate(input: $input) {
    success
    issueLabel { id name }
  }
}`

	mutationAddIssueLabel = `mutation AddIssueLabel($id: String!, $labelId: String!) {
  issueAddLabel(id: $id, labelId: $labelId) {
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

var knownIssueRelationTypes = []string{"related", "blocks", "duplicate", "similar"}

// KnownIssueRelationTypes returns Linear's supported IssueRelationType values.
func KnownIssueRelationTypes() []string {
	return append([]string(nil), knownIssueRelationTypes...)
}

// IsKnownIssueRelationType reports whether relationType is a supported Linear
// IssueRelationType. Unknown values must fail closed when reading relations.
func IsKnownIssueRelationType(relationType string) bool {
	for _, known := range knownIssueRelationTypes {
		if relationType == known {
			return true
		}
	}
	return false
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
// platformBaseURL is the platform ORIGIN (e.g. "https://platform.example.com");
// rskToken is the user's platform API key (Bearer-style). Returns
// ErrInvalidAPIKey when the token is empty.
//
// platformBaseURL is validated as a bare, credential-free HTTP(S) origin and
// canonicalized (see canonicalProxyOrigin); anything else returns an error
// wrapping ErrInvalidPlatformURL. That check is the constructor's job on
// purpose: the origin arrives from operator/credential configuration, so it
// must fail closed before any request is built and before the bearer token is
// attached to one. The error text never echoes the rejected value.
//
// The returned client speaks the exact same GraphQL queries as a direct
// Linear client — the proxy mirrors api.linear.app/graphql's envelope.
func NewProxiedClient(platformBaseURL, rskToken string) (*Client, error) {
	if strings.TrimSpace(rskToken) == "" {
		return nil, ErrInvalidAPIKey
	}
	origin, err := canonicalProxyOrigin(platformBaseURL)
	if err != nil {
		return nil, fmt.Errorf("linear: %w", err)
	}
	return &Client{
		BaseURL:    origin + "/api/cli/linear/graphql",
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

	// Decode the outer envelope; the generic "out" value carries the typed data.
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphqlError  `json:"errors,omitempty"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("linear: read response: %w", err)
	}
	decodeErr := json.Unmarshal(body, &env)
	if statusErr := statusToError(resp.StatusCode); statusErr != nil {
		if decodeErr == nil && len(env.Errors) > 0 {
			return fmt.Errorf("%w: %w: %s", statusErr, ErrGraphQLError, env.Errors[0].Message)
		}
		return statusErr
	}
	if decodeErr != nil {
		return fmt.Errorf("linear: decode response: %w", decodeErr)
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

const (
	maxIssueListPageSize = 250
	maxIssueListPages    = 100
	maxSubIssueListPages = maxIssueListPages
	// MaxIssueListLimit bounds automatic pagination to 100 Linear pages.
	MaxIssueListLimit = maxIssueListPageSize * maxIssueListPages
)

// ValidateIssueListLimit reports whether limit is safe for bounded automatic
// pagination. Commands should call it before resolving any network-backed
// filters; ListIssues also calls it so non-CLI consumers receive the same
// contract.
func ValidateIssueListLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("issue list limit must be positive")
	}
	if limit > MaxIssueListLimit {
		return fmt.Errorf("issue list limit %d exceeds maximum %d", limit, MaxIssueListLimit)
	}
	return nil
}

// ListIssues returns up to limit unique issues matching the given filter map.
// filter is sent verbatim as the GraphQL IssueFilter variable. Linear caps a
// connection page at 250 nodes, so larger limits are fetched using cursors.
// The page bound prevents a malformed or unexpectedly large connection from
// causing unbounded network work.
func (c *Client) ListIssues(ctx context.Context, filter map[string]any, limit int, orderBy string) ([]Issue, error) {
	if err := ValidateIssueListLimit(limit); err != nil {
		return nil, err
	}

	var after *string
	issues := make([]Issue, 0, limit)
	seenIssueIDs := make(map[string]struct{}, limit)
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxIssueListPages; page++ {
		pageSize := min(limit-len(issues), maxIssueListPageSize)
		vars := map[string]any{
			"filter":  filter,
			"first":   pageSize,
			"after":   after,
			"orderBy": orderBy,
		}
		var data paginatedListIssuesData
		if err := c.do(ctx, queryListIssues, vars, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, fmt.Errorf("issues connection is missing")
		}
		if data.Issues.Nodes == nil {
			return nil, fmt.Errorf("issues nodes are missing")
		}
		if len(*data.Issues.Nodes) > pageSize {
			return nil, fmt.Errorf("issues returned %d nodes for page size %d", len(*data.Issues.Nodes), pageSize)
		}

		for i, node := range *data.Issues.Nodes {
			if node.ID == "" {
				return nil, fmt.Errorf("issues node %d id is missing", i)
			}
			if _, duplicate := seenIssueIDs[node.ID]; duplicate {
				continue
			}
			seenIssueIDs[node.ID] = struct{}{}
			issues = append(issues, nodeToIssue(node))
		}

		next, complete, err := nextConnectionPage("issues", data.Issues.PageInfo, after)
		if err != nil {
			return nil, err
		}
		if complete || len(issues) == limit {
			return issues, nil
		}
		if _, repeated := seenCursors[*next]; repeated {
			return nil, fmt.Errorf("issues cursor cycle detected at %q", *next)
		}
		seenCursors[*next] = struct{}{}
		after = next
	}
	return nil, fmt.Errorf("issues: exceeded %d pages", maxIssueListPages)
}

// ListSubIssues returns the direct children of the given parent issue ID.
func (c *Client) ListSubIssues(ctx context.Context, parentID string) ([]Issue, error) {
	var after *string
	issues := make([]Issue, 0)
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxSubIssueListPages; page++ {
		vars := map[string]any{
			"parentId": parentID,
			"first":    maxIssueListPageSize,
			"after":    after,
		}
		var data paginatedListIssuesData
		if err := c.do(ctx, queryListSubIssues, vars, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, fmt.Errorf("sub-issues connection is missing")
		}
		if data.Issues.Nodes == nil {
			return nil, fmt.Errorf("sub-issues nodes are missing")
		}
		if len(*data.Issues.Nodes) > maxIssueListPageSize {
			return nil, fmt.Errorf("sub-issues returned %d nodes for page size %d", len(*data.Issues.Nodes), maxIssueListPageSize)
		}

		for i, node := range *data.Issues.Nodes {
			if node.ID == "" {
				return nil, fmt.Errorf("sub-issues node %d id is missing", i)
			}
			issues = append(issues, nodeToIssue(node))
		}

		next, complete, err := nextConnectionPage("sub-issues", data.Issues.PageInfo, after)
		if err != nil {
			return nil, err
		}
		if complete {
			return issues, nil
		}
		if _, repeated := seenCursors[*next]; repeated {
			return nil, fmt.Errorf("sub-issues cursor cycle detected at %q", *next)
		}
		seenCursors[*next] = struct{}{}
		after = next
	}
	return nil, fmt.Errorf("sub-issues: exceeded %d pages", maxSubIssueListPages)
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
	if !IsKnownIssueRelationType(*node.Type) {
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

func nextConnectionPage(name string, pageInfo *connectionPageInfo, current *string) (*string, bool, error) {
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

func nextRelationPage(name string, pageInfo *connectionPageInfo, current *string) (*string, bool, error) {
	return nextConnectionPage(name, pageInfo, current)
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

// ListLabels returns every accessible issue label, as a map of label name →
// label ID. The catalog is paginated so callers never receive a silently
// truncated result.
func (c *Client) ListLabels(ctx context.Context) (map[string]string, error) {
	return c.listLabels(ctx, nil)
}

// ListLabelsForTeam returns labels that can be applied to issues in the team
// identified by a canonical key or UUID. Linear workspace-level labels have a
// null team and are valid for every team, so they are included alongside labels
// owned by the selected team.
func (c *Client) ListLabelsForTeam(ctx context.Context, teamRef string) (map[string]string, error) {
	team, err := c.resolveCatalogTeamKeyOrID(ctx, teamRef)
	if err != nil {
		return nil, fmt.Errorf("resolve team for label scope: %w", err)
	}
	filter := map[string]any{
		"or": []map[string]any{
			{"team": map[string]any{"id": map[string]any{"eq": team.ID}}},
			{"team": map[string]any{"null": true}},
		},
	}
	return c.listLabels(ctx, filter)
}

func (c *Client) listLabels(ctx context.Context, filter map[string]any) (map[string]string, error) {
	var after *string
	labels := make(map[string]string)
	for page := 0; page < maxCatalogPages; page++ {
		var data listLabelsData
		if err := c.do(ctx, queryListLabels, map[string]any{"filter": filter, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.IssueLabels == nil {
			return nil, fmt.Errorf("issue labels connection is missing")
		}
		if data.IssueLabels.Nodes == nil {
			return nil, fmt.Errorf("issue labels nodes are missing")
		}
		for i, node := range *data.IssueLabels.Nodes {
			if node == nil {
				return nil, fmt.Errorf("issue labels node %d is null", i)
			}
			id, err := requiredCatalogString(node.ID, fmt.Sprintf("issue labels node %d id", i))
			if err != nil {
				return nil, err
			}
			name, err := requiredCatalogString(node.Name, fmt.Sprintf("issue labels node %d name", i))
			if err != nil {
				return nil, err
			}
			labels[name] = id
		}
		next, complete, err := nextConnectionPage("issue labels", data.IssueLabels.PageInfo, after)
		if err != nil {
			return nil, err
		}
		if complete {
			return labels, nil
		}
		after = next
	}
	return nil, fmt.Errorf("issue labels: exceeded %d pages", maxCatalogPages)
}

const maxCatalogPages = 100

func requiredCatalogString(value *string, field string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("%s is missing", field)
	}
	if *value == "" {
		return "", fmt.Errorf("%s is empty", field)
	}
	return *value, nil
}

func catalogTeam(node *teamNode, index int) (Team, error) {
	if node == nil {
		return Team{}, fmt.Errorf("teams node %d is null", index)
	}
	id, err := requiredCatalogString(node.ID, fmt.Sprintf("teams node %d id", index))
	if err != nil {
		return Team{}, err
	}
	key, err := requiredCatalogString(node.Key, fmt.Sprintf("teams node %d key", index))
	if err != nil {
		return Team{}, err
	}
	name, err := requiredCatalogString(node.Name, fmt.Sprintf("teams node %d name", index))
	if err != nil {
		return Team{}, err
	}
	return Team{ID: id, Key: key, Name: name}, nil
}

func catalogTeams(connection *teamConnection) ([]Team, error) {
	if connection == nil {
		return nil, fmt.Errorf("teams connection is missing")
	}
	if connection.Nodes == nil {
		return nil, fmt.Errorf("teams nodes are missing")
	}
	teams := make([]Team, 0, len(*connection.Nodes))
	for i, node := range *connection.Nodes {
		team, err := catalogTeam(node, i)
		if err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	return teams, nil
}

func catalogProject(node *projectNode, index int, selectedTeamKey string) (Project, error) {
	if node == nil {
		return Project{}, fmt.Errorf("projects node %d is null", index)
	}
	id, err := requiredCatalogString(node.ID, fmt.Sprintf("projects node %d id", index))
	if err != nil {
		return Project{}, err
	}
	name, err := requiredCatalogString(node.Name, fmt.Sprintf("projects node %d name", index))
	if err != nil {
		return Project{}, err
	}
	state, err := requiredCatalogString(node.State, fmt.Sprintf("projects node %d state", index))
	if err != nil {
		return Project{}, err
	}
	if node.Teams == nil {
		return Project{}, fmt.Errorf("project %q teams connection is missing", id)
	}
	if node.Teams.Nodes == nil {
		return Project{}, fmt.Errorf("project %q teams nodes are missing", id)
	}
	if _, complete, err := nextConnectionPage("project "+id+" teams", node.Teams.PageInfo, nil); err != nil {
		return Project{}, err
	} else if !complete {
		return Project{}, fmt.Errorf("project %q teams: pagination is not supported", id)
	}

	memberTeams, err := catalogTeams(node.Teams)
	if err != nil {
		return Project{}, fmt.Errorf("project %q: %w", id, err)
	}
	teamKeys := make([]string, 0, len(memberTeams))
	for _, team := range memberTeams {
		if selectedTeamKey == "" || team.Key == selectedTeamKey {
			teamKeys = append(teamKeys, team.Key)
		}
	}
	if selectedTeamKey != "" && len(teamKeys) == 0 {
		return Project{}, fmt.Errorf("project %q is missing selected team %q", id, selectedTeamKey)
	}
	project := Project{ID: id, Name: name, State: state, TeamKeys: teamKeys}
	if node.SlugID != nil {
		project.SlugID = strings.TrimSpace(*node.SlugID)
	}
	return project, nil
}

// ListTeams returns every accessible team. The result is paginated so callers
// never receive a silently truncated catalog.
func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	var after *string
	teams := make([]Team, 0)
	for page := 0; page < maxCatalogPages; page++ {
		var data listTeamsData
		if err := c.do(ctx, queryListTeams, map[string]any{"after": after}, &data); err != nil {
			return nil, err
		}
		pageTeams, err := catalogTeams(data.Teams)
		if err != nil {
			return nil, err
		}
		next, complete, err := nextConnectionPage("teams", data.Teams.PageInfo, after)
		if err != nil {
			return nil, err
		}
		teams = append(teams, pageTeams...)
		if complete {
			return teams, nil
		}
		after = next
	}
	return nil, fmt.Errorf("teams: exceeded %d pages", maxCatalogPages)
}

// ListProjects returns every accessible project. When teamRef is set, it must
// be a canonical team key or UUID and the result is restricted to that team.
func (c *Client) ListProjects(ctx context.Context, teamRef string) ([]Project, error) {
	var (
		after           *string
		filter          map[string]any
		selectedTeamKey string
	)
	if teamRef != "" {
		team, err := c.resolveCatalogTeamKeyOrID(ctx, teamRef)
		if err != nil {
			return nil, fmt.Errorf("resolve team for project scope: %w", err)
		}
		selectedTeamKey = team.Key
		filter = map[string]any{
			"accessibleTeams": map[string]any{
				"some": map[string]any{
					"id": map[string]any{"eq": team.ID},
				},
			},
		}
	}

	projects := make([]Project, 0)
	for page := 0; page < maxCatalogPages; page++ {
		var data listProjectsData
		if err := c.do(ctx, queryListProjects, map[string]any{"filter": filter, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.Projects == nil {
			return nil, fmt.Errorf("projects connection is missing")
		}
		if data.Projects.Nodes == nil {
			return nil, fmt.Errorf("projects nodes are missing")
		}
		pageProjects := make([]Project, 0, len(*data.Projects.Nodes))
		for i, node := range *data.Projects.Nodes {
			project, err := catalogProject(node, i, selectedTeamKey)
			if err != nil {
				return nil, err
			}
			pageProjects = append(pageProjects, project)
		}
		next, complete, err := nextConnectionPage("projects", data.Projects.PageInfo, after)
		if err != nil {
			return nil, err
		}
		projects = append(projects, pageProjects...)
		if complete {
			return projects, nil
		}
		after = next
	}
	return nil, fmt.Errorf("projects: exceeded %d pages", maxCatalogPages)
}

// resolveCatalogTeamKeyOrID resolves only a canonical team key or UUID. It
// intentionally excludes display names because names are not a stable routing
// identifier and can be duplicated.
func (c *Client) resolveCatalogTeamKeyOrID(ctx context.Context, ref string) (*Team, error) {
	teams, err := c.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]Team, 0, 1)
	for _, team := range teams {
		if (looksLikeID(ref) && strings.EqualFold(team.ID, ref)) || (!looksLikeID(ref) && team.Key == ref) {
			matches = append(matches, team)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("team key or id %q not found", ref)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("team key or id %q is ambiguous", ref)
	}
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
		"after":  nil,
	}
	var data listTeamsData
	if err := c.do(ctx, queryListTeams, vars, &data); err != nil {
		return nil, err
	}
	if data.Teams == nil {
		return nil, fmt.Errorf("incomplete teams response: teams connection is missing")
	}
	if data.Teams.Nodes == nil {
		return nil, fmt.Errorf("incomplete teams response: teams nodes are missing")
	}
	if len(*data.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("%w: team %q", ErrNotFound, nameOrKeyOrID)
	}
	team, err := catalogTeam((*data.Teams.Nodes)[0], 0)
	if err != nil {
		return nil, fmt.Errorf("incomplete teams response: %w", err)
	}
	return &team, nil
}

// GetProjectByName resolves a project by display name or slug
// (case-insensitive), or by canonical UUID. It is a thin wrapper over
// GetProjectByNameInTeam with no team disambiguation.
func (c *Client) GetProjectByName(ctx context.Context, ref string) (*Project, error) {
	return c.GetProjectByNameInTeam(ctx, ref, "")
}

// GetProjectByNameInTeam resolves a project reference, optionally scoped to a
// team to disambiguate workspace-wide duplicate names. teamID may be a team
// UUID or a team key/name (resolved to an id first); empty disables the team
// filter. More than one exact name/slug match fails loud rather than selecting
// an arbitrary project.
func (c *Client) GetProjectByNameInTeam(ctx context.Context, ref, teamID string) (*Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: project reference is empty", ErrNotFound)
	}
	or := []map[string]any{
		{"name": map[string]any{"eqIgnoreCase": ref}},
		{"slugId": map[string]any{"eqIgnoreCase": ref}},
	}
	if looksLikeID(ref) {
		or = append(or, map[string]any{"id": map[string]any{"eq": ref}})
	}
	filter := map[string]any{"or": or}
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
	vars := map[string]any{"filter": filter, "after": nil}
	var data listProjectsData
	if err := c.do(ctx, queryListProjects, vars, &data); err != nil {
		return nil, err
	}
	if data.Projects == nil {
		return nil, fmt.Errorf("incomplete projects response: projects connection is missing")
	}
	if data.Projects.Nodes == nil {
		return nil, fmt.Errorf("incomplete projects response: projects nodes are missing")
	}
	if len(*data.Projects.Nodes) == 0 {
		return nil, fmt.Errorf("%w: project %q", ErrNotFound, ref)
	}
	if len(*data.Projects.Nodes) > 1 || (data.Projects.PageInfo != nil && data.Projects.PageInfo.HasNextPage != nil && *data.Projects.PageInfo.HasNextPage) {
		return nil, fmt.Errorf("project %q is ambiguous within the selected team; use its unique slug or UUID", ref)
	}
	n := (*data.Projects.Nodes)[0]
	if n == nil {
		return nil, fmt.Errorf("incomplete projects response: projects node 0 is null")
	}
	id, err := requiredCatalogString(n.ID, "projects node 0 id")
	if err != nil {
		return nil, fmt.Errorf("incomplete projects response: %w", err)
	}
	projectName, err := requiredCatalogString(n.Name, "projects node 0 name")
	if err != nil {
		return nil, fmt.Errorf("incomplete projects response: %w", err)
	}
	project := &Project{ID: id, Name: projectName}
	if n.SlugID != nil {
		project.SlugID = strings.TrimSpace(*n.SlugID)
	}
	return project, nil
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
	if input.ProjectID != "" {
		inp["projectId"] = input.ProjectID
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

// CreateIssueLabel creates a label owned by the given team. A team ID is
// required here because omitting it in Linear creates a workspace-wide label.
func (c *Client) CreateIssueLabel(ctx context.Context, name, teamID string) (*Label, error) {
	name = strings.TrimSpace(name)
	teamID = strings.TrimSpace(teamID)
	if name == "" {
		return nil, fmt.Errorf("label name is required")
	}
	if teamID == "" {
		return nil, fmt.Errorf("team id is required for label creation")
	}

	vars := map[string]any{"input": map[string]any{"name": name, "teamId": teamID}}
	var data createIssueLabelData
	if err := c.do(ctx, mutationCreateIssueLabel, vars, &data); err != nil {
		return nil, err
	}
	if !data.IssueLabelCreate.Success {
		return nil, ErrMutationFailed
	}
	if data.IssueLabelCreate.IssueLabel == nil {
		return nil, fmt.Errorf("created issue label is missing")
	}
	id, err := requiredCatalogString(data.IssueLabelCreate.IssueLabel.ID, "created issue label id")
	if err != nil {
		return nil, err
	}
	createdName, err := requiredCatalogString(data.IssueLabelCreate.IssueLabel.Name, "created issue label name")
	if err != nil {
		return nil, err
	}
	return &Label{ID: id, Name: createdName}, nil
}

// AddIssueLabel atomically adds one label without replacing labels that may
// have been attached concurrently by another actor.
func (c *Client) AddIssueLabel(ctx context.Context, issueID, labelID string) (*Issue, error) {
	vars := map[string]any{"id": issueID, "labelId": labelID}
	var data addIssueLabelData
	if err := c.do(ctx, mutationAddIssueLabel, vars, &data); err != nil {
		return nil, err
	}
	if !data.IssueAddLabel.Success {
		return nil, ErrMutationFailed
	}
	if data.IssueAddLabel.Issue == nil {
		return nil, fmt.Errorf("updated issue is missing after adding label")
	}
	iss := nodeToIssue(*data.IssueAddLabel.Issue)
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
// relationType must be one of the values returned by KnownIssueRelationTypes.
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
