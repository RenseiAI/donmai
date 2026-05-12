// Package afclient provides the AgentFactory coordinator API client and types.
package afclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

	// Architecture-aware fetch methods added in REN-1333.
	// These surface the new Machine/Daemon/Pool/Workarea/SandboxProvider/Kit
	// concepts for TUI Phase 2/3 panels per 009-linear-realignment §issues 56-59.

	// GetStatsV2 fetches fleet statistics extended with per-machine and
	// per-provider breakdowns.
	GetStatsV2() (*StatsResponseV2, error)

	// GetMachineStats fetches the per-machine capacity and status snapshot for
	// the registered daemon fleet (014 FleetGrid + MachinePivot primitives).
	GetMachineStats() ([]MachineStats, error)

	// GetWorkareaPoolStats fetches the local workarea pool snapshot for a given
	// machine. An empty machineID returns the aggregate across all machines.
	// Corresponds to `af daemon stats --pool` (011-local-daemon-fleet §Observability).
	GetWorkareaPoolStats(machineID MachineID) (*WorkareaPoolStats, error)

	// GetSandboxProviderStats fetches the runtime snapshot for all registered
	// SandboxProviders (004-sandbox-capability-matrix §Capacity snapshots).
	GetSandboxProviderStats() ([]SandboxProviderStats, error)

	// GetKitDetections fetches the kit detection results for a session.
	// Corresponds to the KitDetectResult TUI primitive in 014.
	GetKitDetections(sessionID string) ([]KitDetection, error)

	// GetKitContributions fetches the kit contribution summary for a session.
	// Corresponds to the KitContributionDiff TUI primitive in 014.
	GetKitContributions(sessionID string) ([]KitContribution, error)

	// GetAuditChain fetches the Layer 6 audit chain for a session.
	// Corresponds to the AuditChain TUI primitive in 014.
	GetAuditChain(sessionID string) ([]AuditChainEntry, error)
}

// Client is the HTTP implementation of DataSource.
//
// Scope fields (OrgScope, ProjectScope) carry per-invocation routing
// context. When non-empty they are sent on EVERY request as
// `X-Rensei-Org` and `X-Rensei-Project`. The platform's CLI auth path
// treats `X-Rensei-Org` as authoritative (after membership check), so
// these headers eliminate the org-resolution drift that bites when
// multiple humans + agents on a host share a WorkOS access token whose
// `org_id` claim is frozen to whichever org the user happened to be in
// at token-mint time. Empty = don't send the header (server falls back
// to its own resolution).
type Client struct {
	BaseURL    string
	APIToken   string // Bearer token for authenticated requests (rsk_...)
	HTTPClient *http.Client

	// OrgScope, when non-empty, is sent as `X-Rensei-Org` on every
	// request. Accepts the platform org id (`org_…`), org slug, or
	// the WorkOS org id — the server resolves whichever is supplied.
	OrgScope string
	// ProjectScope, when non-empty, is sent as `X-Rensei-Project` on
	// every request. Accepts the project slug or platform project id.
	// Server-side honoring is route-by-route (not all routes are
	// project-scoped).
	ProjectScope string
}

// setRequestHeaders applies the standard auth + scope headers to req.
// Empty values are skipped so callers that haven't configured a token
// or a scope still produce minimal-header requests (matches pre-scope
// behaviour for unauthenticated public endpoints).
func (c *Client) setRequestHeaders(req *http.Request) {
	if c.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIToken)
	}
	if c.OrgScope != "" {
		req.Header.Set("X-Rensei-Org", c.OrgScope)
	}
	if c.ProjectScope != "" {
		req.Header.Set("X-Rensei-Project", c.ProjectScope)
	}
}

// CredentialsFromDataSource attempts to recover the platform base URL and
// rsk_ Bearer token from a DataSource. Returns ok=false when the
// underlying value isn't an *afclient.Client (e.g. MockClient in tests),
// when the token is empty, or when the base URL is empty.
//
// Used by callers that need to dispatch through a sibling platform surface
// — for example the linear subcommand tree at
// `afcli/linear.go` routes GraphQL through `/api/cli/linear/graphql` when
// an authenticated platform client is available (per
// ADR-2026-05-12-cli-linear-proxy). Callers that get ok=false should fall
// back to their direct-API auth path.
//
// Pure read-only access — no copies made; the strings live on the same
// `*Client` the DataSource wraps.
func CredentialsFromDataSource(ds DataSource) (baseURL, token string, ok bool) {
	if ds == nil {
		return "", "", false
	}
	c, isClient := ds.(*Client)
	if !isClient {
		return "", "", false
	}
	if c.BaseURL == "" || c.APIToken == "" {
		return "", "", false
	}
	return c.BaseURL, c.APIToken, true
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
	case status == http.StatusConflict:
		return fmt.Errorf("%s: %w", path, ErrConflict)
	case status == http.StatusServiceUnavailable:
		return fmt.Errorf("%s: %w", path, ErrUnavailable)
	case status == http.StatusBadRequest:
		return fmt.Errorf("%s: %w", path, ErrBadRequest)
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
	c.setRequestHeaders(req)

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
//
// The endpoint changed from a path-segment shape
// (`/api/public/sessions/<id>/activities`) to a query-parameter shape
// (`/api/public/session-activities?sessionId=<id>[&sessionHash=<hash>]`)
// when the activity reader was ported into the platform's app-router
// REST surface. Older servers that still expose the legacy path-
// segment route will respond with a 404 to the query-param URL —
// clients targeting those need to pin a version of this package that
// pre-dates this commit.
//
// Auth model: when the client carries an APIToken (rsk_ key) it sends
// the token in the Authorization header and omits sessionHash, so the
// server takes its rsk_/cookie auth branch. That branch accepts both
// the raw linearSessionId and the hashed id form returned by
// /api/public/sessions list/detail, doing a reverse-lookup if needed.
// When the client is unauthenticated (no APIToken — typically a TUI
// viewer with the raw linearSessionId from a shared link) it sends
// sessionHash so the server's public-hash branch admits it; that
// branch only accepts the raw linearSessionId.
func (c *Client) GetActivities(sessionID string, afterCursor *string) (*ActivityListResponse, error) {
	q := url.Values{}
	q.Set("sessionId", sessionID)
	if c.APIToken == "" {
		q.Set("sessionHash", hashSessionID(sessionID))
	}
	if afterCursor != nil {
		q.Set("after", *afterCursor)
	}
	path := "/api/public/session-activities?" + q.Encode()
	var resp ActivityListResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// hashSessionID computes the public session hash that
// /api/public/session-activities accepts in lieu of worker auth.
// Mirrors the platform's hashSessionId implementation in
// src/lib/worker-protocol/session-hash.ts: first 32 hex chars of
// SHA-256("session:" + sessionID).
func hashSessionID(sessionID string) string {
	sum := sha256.Sum256([]byte("session:" + sessionID))
	return hex.EncodeToString(sum[:])[:32]
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
	c.setRequestHeaders(req)

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

// ── Architecture-aware methods (REN-1333) ─────────────────────────────────────

// GetStatsV2 fetches fleet statistics extended with per-machine and per-provider
// breakdowns from the /api/public/stats/v2 endpoint.
func (c *Client) GetStatsV2() (*StatsResponseV2, error) {
	var resp StatsResponseV2
	if err := c.get("/api/public/stats/v2", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetMachineStats fetches per-machine capacity and status snapshots.
func (c *Client) GetMachineStats() ([]MachineStats, error) {
	var resp []MachineStats
	if err := c.get("/api/public/machines", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetWorkareaPoolStats fetches the local workarea pool snapshot.
// An empty machineID returns the aggregate across all machines.
func (c *Client) GetWorkareaPoolStats(machineID MachineID) (*WorkareaPoolStats, error) {
	path := "/api/public/workarea-pool"
	if machineID != "" {
		path += "?machine=" + machineID
	}
	var resp WorkareaPoolStats
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSandboxProviderStats fetches runtime snapshots for all registered SandboxProviders.
func (c *Client) GetSandboxProviderStats() ([]SandboxProviderStats, error) {
	var resp []SandboxProviderStats
	if err := c.get("/api/public/sandbox-providers", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetKitDetections fetches kit detection results for a session.
func (c *Client) GetKitDetections(sessionID string) ([]KitDetection, error) {
	var resp []KitDetection
	if err := c.get("/api/public/sessions/"+sessionID+"/kit-detections", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetKitContributions fetches kit contribution summaries for a session.
func (c *Client) GetKitContributions(sessionID string) ([]KitContribution, error) {
	var resp []KitContribution
	if err := c.get("/api/public/sessions/"+sessionID+"/kit-contributions", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetAuditChain fetches the Layer 6 audit chain for a session.
func (c *Client) GetAuditChain(sessionID string) ([]AuditChainEntry, error) {
	var resp []AuditChainEntry
	if err := c.get("/api/public/sessions/"+sessionID+"/audit-chain", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}
