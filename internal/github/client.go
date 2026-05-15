package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Client is a minimal GitHub REST API client. It uses a personal access token
// or GitHub App installation token for authentication.
//
// Authentication:
//   - Personal access token (classic or fine-grained): set GITHUB_TOKEN env var.
//     All requests are sent with "Authorization: Bearer <token>".
//   - GitHub App installation token: obtained via the app's private-key JWT flow;
//     same Bearer header format. Use NewClientWithToken to supply it directly.
//
// The client targets GitHub's REST API v3 (api.github.com). No external
// dependencies beyond the Go standard library are needed.
type Client struct {
	BaseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a Client authenticated with the given personal access token
// or GitHub App installation token.
func NewClient(token string) *Client {
	return &Client{
		BaseURL:    defaultBaseURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	rawURL := strings.TrimRight(c.BaseURL, "/") + path

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		// success
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	default:
		var apiErr APIError
		apiErr.StatusCode = resp.StatusCode
		if jsonErr := json.Unmarshal(respBody, &apiErr); jsonErr != nil {
			apiErr.Message = string(respBody)
		}
		return &apiErr
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ─── Issues ───────────────────────────────────────────────────────────────────

// GetIssue fetches a single issue by number.
func (c *Client) GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	req, err := c.newRequest(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number), nil)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := c.do(req, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// ListIssues lists issues in a repository with optional filters.
func (c *Client) ListIssues(ctx context.Context, owner, repo string, opts ListIssuesOptions) ([]Issue, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	} else {
		q.Set("state", "open")
	}
	if opts.Labels != "" {
		q.Set("labels", opts.Labels)
	}
	if opts.Assignee != "" {
		q.Set("assignee", opts.Assignee)
	}
	if opts.Creator != "" {
		q.Set("creator", opts.Creator)
	}
	if opts.Milestone != "" {
		q.Set("milestone", opts.Milestone)
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Direction != "" {
		q.Set("direction", opts.Direction)
	}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	q.Set("per_page", strconv.Itoa(perPage))
	if opts.Page > 0 {
		q.Set("page", strconv.Itoa(opts.Page))
	}

	path := fmt.Sprintf("/repos/%s/%s/issues?%s", owner, repo, q.Encode())
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := c.do(req, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// ListIssueComments returns comments for an issue.
func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var comments []Comment
	if err := c.do(req, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// CreateIssue creates a new issue.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, input CreateIssueInput) (*Issue, error) {
	req, err := c.newRequest(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues", owner, repo), input)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := c.do(req, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// UpdateIssue updates an existing issue. Zero-value fields are omitted.
func (c *Client) UpdateIssue(ctx context.Context, owner, repo string, number int, input UpdateIssueInput) (*Issue, error) {
	req, err := c.newRequest(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number), input)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := c.do(req, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// CreateIssueComment posts a comment on an issue.
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error) {
	payload := map[string]string{"body": body}
	req, err := c.newRequest(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number), payload)
	if err != nil {
		return nil, err
	}
	var comment Comment
	if err := c.do(req, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

// AddLabels adds labels to an issue (non-destructive; existing labels are kept).
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) ([]Label, error) {
	payload := map[string][]string{"labels": labels}
	req, err := c.newRequest(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, number), payload)
	if err != nil {
		return nil, err
	}
	var out []Label
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetAssignees replaces the assignees on an issue.
func (c *Client) SetAssignees(ctx context.Context, owner, repo string, number int, assignees []string) (*Issue, error) {
	return c.UpdateIssue(ctx, owner, repo, number, UpdateIssueInput{Assignees: assignees})
}

// ─── Repos ────────────────────────────────────────────────────────────────────

// GetRepo fetches repository metadata.
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	req, err := c.newRequest(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return nil, err
	}
	var r Repo
	if err := c.do(req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListLabels lists all labels in a repository.
func (c *Client) ListLabels(ctx context.Context, owner, repo string) ([]Label, error) {
	path := fmt.Sprintf("/repos/%s/%s/labels?per_page=100", owner, repo)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var labels []Label
	if err := c.do(req, &labels); err != nil {
		return nil, err
	}
	return labels, nil
}

// ─── Users ────────────────────────────────────────────────────────────────────

// GetAuthenticatedUser returns the currently authenticated user.
func (c *Client) GetAuthenticatedUser(ctx context.Context) (*User, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return nil, err
	}
	var user User
	if err := c.do(req, &user); err != nil {
		return nil, err
	}
	return &user, nil
}
