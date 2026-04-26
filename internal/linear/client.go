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

const (
	defaultBaseURL = "https://api.linear.app/graphql"

	queryListIssuesByProject = `query ListIssuesByProject($name: String!, $states: [String!]) { issues(filter: { project: { name: { eq: $name } }, state: { name: { in: $states } } }) { nodes { id identifier title state { name } project { name } parent { id } } } }`
	queryGetIssue            = `query GetIssue($id: String!) { issue(id: $id) { id identifier title state { name } project { name } parent { id } } }`
	queryListSubIssues       = `query ListSubIssues($parentId: ID!) { issues(filter: { parent: { id: { eq: $parentId } } }) { nodes { id identifier title state { name } project { name } parent { id } } } }`
)

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
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
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

// nodesToIssues converts internal issueNode slice to the public Issue type.
func nodesToIssues(nodes []issueNode) []Issue {
	out := make([]Issue, len(nodes))
	for i, n := range nodes {
		out[i] = nodeToIssue(n)
	}
	return out
}

func nodeToIssue(n issueNode) Issue {
	iss := Issue{
		ID:         n.ID,
		Identifier: n.Identifier,
		Title:      n.Title,
	}
	iss.State.Name = n.State.Name
	iss.Project.Name = n.Project.Name
	if n.Parent != nil {
		iss.ParentID = n.Parent.ID
	}
	return iss
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

// ListSubIssues returns the direct children of the given parent issue ID.
func (c *Client) ListSubIssues(ctx context.Context, parentID string) ([]Issue, error) {
	vars := map[string]any{"parentId": parentID}
	var data listIssuesData
	if err := c.do(ctx, queryListSubIssues, vars, &data); err != nil {
		return nil, err
	}
	return nodesToIssues(data.Issues.Nodes), nil
}
