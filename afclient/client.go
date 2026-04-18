// Package afclient provides the AgentFactory coordinator API client and types.
package afclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DataSource is the interface both Client and MockClient implement.
type DataSource interface {
	GetStats() (*StatsResponse, error)
	GetSessions() (*SessionsListResponse, error)
	GetSessionsFiltered(project string) (*SessionsListResponse, error)
	GetSessionDetail(id string) (*SessionDetailResponse, error)
	GetActivities(sessionID string, afterCursor *string) (*ActivityListResponse, error)
	StopSession(id string) (*StopSessionResponse, error)
	ChatSession(id string, req ChatSessionRequest) (*ChatSessionResponse, error)
	ReconnectSession(id string, req ReconnectSessionRequest) (*ReconnectSessionResponse, error)
	SubmitTask(req SubmitTaskRequest) (*SubmitTaskResponse, error)
	StopAgent(req StopAgentRequest) (*StopAgentResponse, error)
	ForwardPrompt(req ForwardPromptRequest) (*ForwardPromptResponse, error)
	GetCostReport() (*CostReportResponse, error)
	ListFleet() (*ListFleetResponse, error)
}

// Client is the HTTP implementation of DataSource.
type Client struct {
	BaseURL    string
	APIToken   string // Bearer token for authenticated requests (rsk_...)
	HTTPClient *http.Client
}

// NewClient creates a new API client pointing at the given server URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewAuthenticatedClient creates an API client with Bearer token auth.
func NewAuthenticatedClient(baseURL, apiToken string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIToken:   apiToken,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// statusToError maps an HTTP status code to a sentinel error for expected
// failure modes, or a generic error for unexpected codes. Returns nil for 2xx.
func statusToError(status int, path string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s: %w", path, ErrNotAuthenticated)
	case status == http.StatusForbidden:
		return fmt.Errorf("%s: %w", path, ErrUnauthorized)
	case status == http.StatusNotFound:
		return fmt.Errorf("%s: %w", path, ErrNotFound)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", path, ErrRateLimited)
	case status >= 500:
		return fmt.Errorf("%s: %w", path, ErrServerError)
	default:
		return fmt.Errorf("unexpected status %d for %s", status, path)
	}
}

func (c *Client) get(path string, target any) error {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request failed: %w", err)
	}
	if c.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := statusToError(resp.StatusCode, path); err != nil {
		return err
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode failed: %w", err)
	}
	return nil
}

// GetStats fetches fleet-wide statistics.
func (c *Client) GetStats() (*StatsResponse, error) {
	var resp StatsResponse
	if err := c.get("/api/public/stats", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSessions fetches the list of all sessions (fleet-wide).
func (c *Client) GetSessions() (*SessionsListResponse, error) {
	return c.GetSessionsFiltered("")
}

// GetSessionsFiltered fetches sessions optionally scoped to a project.
// An empty project returns fleet-wide sessions (same as GetSessions).
// Non-empty project values are passed as the `project` query parameter;
// the platform accepts either a project slug or ID.
func (c *Client) GetSessionsFiltered(project string) (*SessionsListResponse, error) {
	path := "/api/public/sessions"
	if project != "" {
		path += "?project=" + url.QueryEscape(project)
	}
	var resp SessionsListResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSessionDetail fetches detailed info for a single session.
func (c *Client) GetSessionDetail(id string) (*SessionDetailResponse, error) {
	var resp SessionDetailResponse
	if err := c.get("/api/public/sessions/"+id, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetActivities fetches activity events for a session, optionally after a cursor.
func (c *Client) GetActivities(sessionID string, afterCursor *string) (*ActivityListResponse, error) {
	path := "/api/public/sessions/" + sessionID + "/activities"
	if afterCursor != nil {
		path += "?after=" + *afterCursor
	}
	var resp ActivityListResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) post(path string, body any, target any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal failed: %w", err)
	}
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusToError(resp.StatusCode, path); err != nil {
		return err
	}
	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return fmt.Errorf("decode failed: %w", err)
		}
	}
	return nil
}

// StopSession sends a stop request for the given session and returns the
// coordinator's response describing the status transition.
func (c *Client) StopSession(id string) (*StopSessionResponse, error) {
	var resp StopSessionResponse
	if err := c.post("/api/public/sessions/"+id+"/stop", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ChatSession forwards a prompt to the given session's agent and returns the
// delivery confirmation.
func (c *Client) ChatSession(id string, req ChatSessionRequest) (*ChatSessionResponse, error) {
	var resp ChatSessionResponse
	if err := c.post("/api/public/sessions/"+id+"/prompt", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReconnectSession resumes an activity stream for the given session.
func (c *Client) ReconnectSession(id string, req ReconnectSessionRequest) (*ReconnectSessionResponse, error) {
	var resp ReconnectSessionResponse
	if err := c.post("/api/public/sessions/"+id+"/reconnect", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SubmitTask submits a new task to the fleet work queue.
func (c *Client) SubmitTask(req SubmitTaskRequest) (*SubmitTaskResponse, error) {
	var resp SubmitTaskResponse
	if err := c.post("/api/mcp/submit-task", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopAgent requests to stop a running agent.
func (c *Client) StopAgent(req StopAgentRequest) (*StopAgentResponse, error) {
	var resp StopAgentResponse
	if err := c.post("/api/mcp/stop-agent", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ForwardPrompt forwards a message to a running agent session.
func (c *Client) ForwardPrompt(req ForwardPromptRequest) (*ForwardPromptResponse, error) {
	var resp ForwardPromptResponse
	if err := c.post("/api/mcp/forward-prompt", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetCostReport fetches the fleet-wide cost report.
func (c *Client) GetCostReport() (*CostReportResponse, error) {
	var resp CostReportResponse
	if err := c.get("/api/mcp/cost-report", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListFleet fetches a list of agents with optional filtering.
func (c *Client) ListFleet() (*ListFleetResponse, error) {
	var resp ListFleetResponse
	if err := c.get("/api/mcp/list-fleet", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// WhoAmI verifies the API key and returns org context.
func (c *Client) WhoAmI() (*WhoAmIResponse, error) {
	var resp WhoAmIResponse
	if err := c.get("/api/cli/whoami", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
