// Package afclient daemon_client.go — thin HTTP client for the local daemon's
// status/control API. The daemon listens on HTTP at 127.0.0.1:<port> from
// ~/.rensei/daemon.yaml. All paths are relative to that base URL.
package afclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DaemonConfig holds the minimal daemon connection config read from
// ~/.rensei/daemon.yaml (or overridden by env/flag).
type DaemonConfig struct {
	// Port is the HTTP port the daemon is listening on (default 7734).
	Port int `json:"port" yaml:"port"`
	// Host is the bind address (default "127.0.0.1").
	Host string `json:"host" yaml:"host"`
}

// DefaultDaemonConfig returns a DaemonConfig with sane defaults.
func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{
		Host: "127.0.0.1",
		Port: 7734,
	}
}

// BaseURL returns the base URL for the daemon API derived from cfg.
func (c DaemonConfig) BaseURL() string {
	host := c.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := c.Port
	if port == 0 {
		port = 7734
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// ── Daemon API request/response types ────────────────────────────────────────

// DaemonStatusResponse is the response from GET /api/daemon/status.
type DaemonStatusResponse struct {
	// Status is the lifecycle state of the daemon.
	Status DaemonStatus `json:"status"`
	// Version is the rensei-daemon binary version.
	Version string `json:"version"`
	// MachineID is the configured machine identifier.
	MachineID string `json:"machineId"`
	// PID is the daemon process ID.
	PID int `json:"pid"`
	// UptimeSeconds is how long the daemon has been running.
	UptimeSeconds int64 `json:"uptimeSeconds"`
	// ActiveSessions is the count of sessions currently running.
	ActiveSessions int `json:"activeSessions"`
	// MaxSessions is the declared capacity ceiling.
	MaxSessions int `json:"maxSessions"`
	// ProjectsAllowed is the number of projects in the allowlist.
	ProjectsAllowed int `json:"projectsAllowed"`
	// Timestamp is the RFC3339 time of this snapshot.
	Timestamp string `json:"timestamp"`
}

// DaemonStatsResponse is the response from GET /api/daemon/stats.
type DaemonStatsResponse struct {
	// Capacity is the machine capacity envelope.
	Capacity MachineCapacity `json:"capacity"`
	// ActiveSessions is the count of currently running sessions.
	ActiveSessions int `json:"activeSessions"`
	// QueueDepth is the number of tasks waiting for a session slot.
	QueueDepth int `json:"queueDepth"`
	// Pool is the workarea pool snapshot (populated with --pool).
	Pool *WorkareaPoolStats `json:"pool,omitempty"`
	// ByMachine is the per-machine breakdown (populated with --by-machine).
	ByMachine []MachineStats `json:"byMachine,omitempty"`
	// Timestamp is the RFC3339 time of this snapshot.
	Timestamp string `json:"timestamp"`

	// WorkerID is the platform-assigned worker id (or stub fallback). Empty
	// if registration has not yet completed. (REN-1446.)
	WorkerID string `json:"workerId,omitempty"`
	// Registration carries the human-readable registration status and the
	// timestamp of the most recent successful heartbeat. (REN-1446.)
	Registration *DaemonRegistrationStats `json:"registration,omitempty"`
	// AllowedProjects is the list of repositories in the daemon's allowlist
	// (from daemon.yaml). May be empty when no projects have been
	// configured. (REN-1446.)
	AllowedProjects []string `json:"allowedProjects,omitempty"`
}

// DaemonRegistrationStats summarises the daemon's connection to the platform
// for `daemon stats` consumers. (REN-1446.)
type DaemonRegistrationStats struct {
	// Status is the registration status reported in the most recent
	// heartbeat: idle / busy / draining / stub / unregistered.
	Status string `json:"status,omitempty"`
	// LastHeartbeatAt is the RFC3339 timestamp of the last heartbeat
	// payload composed by the daemon. Empty when no heartbeat has run.
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
	// HeartbeatRunning reports whether the heartbeat goroutine is active.
	HeartbeatRunning bool `json:"heartbeatRunning,omitempty"`
	// PollRunning reports whether the poll goroutine is active. (REN-1441.)
	PollRunning bool `json:"pollRunning,omitempty"`
}

// DaemonActionResponse is the response from action endpoints (pause, resume,
// drain, stop, update).
type DaemonActionResponse struct {
	// OK is true when the action was accepted.
	OK bool `json:"ok"`
	// Message is a human-readable description of the outcome.
	Message string `json:"message"`
}

// DaemonDrainRequest is the optional body for POST /api/daemon/drain.
type DaemonDrainRequest struct {
	// TimeoutSeconds is the max time to wait for in-flight work to drain.
	// 0 means use the daemon's configured default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// ── DaemonClient ─────────────────────────────────────────────────────────────

// DaemonClient is an HTTP client for the local daemon's control API.
// Construct with NewDaemonClient. All methods are safe for concurrent use.
type DaemonClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewDaemonClient constructs a DaemonClient pointing at the daemon derived
// from cfg. The HTTP timeout is set to 10 seconds.
func NewDaemonClient(cfg DaemonConfig) *DaemonClient {
	return &DaemonClient{
		baseURL:    cfg.BaseURL(),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewDaemonClientFromURL constructs a DaemonClient pointing at an arbitrary
// base URL. Primarily used in tests with httptest.Server.
func NewDaemonClientFromURL(baseURL string) *DaemonClient {
	return &DaemonClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *DaemonClient) get(path string, target any) error {
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusToError(resp.StatusCode, path); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func (c *DaemonClient) post(path string, body any, target any) error {
	var reqBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&reqBody).Encode(body); err != nil {
			return fmt.Errorf("encode body: %w", err)
		}
	}
	resp, err := c.httpClient.Post(c.baseURL+path, "application/json", &reqBody)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusToError(resp.StatusCode, path); err != nil {
		return err
	}
	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}

// GetStatus fetches the daemon's current status snapshot.
func (c *DaemonClient) GetStatus() (*DaemonStatusResponse, error) {
	var resp DaemonStatusResponse
	if err := c.get("/api/daemon/status", &resp); err != nil {
		return nil, fmt.Errorf("daemon status: %w", err)
	}
	return &resp, nil
}

// GetStats fetches the daemon's capacity and pool statistics.
func (c *DaemonClient) GetStats(withPool, byMachine bool) (*DaemonStatsResponse, error) {
	path := "/api/daemon/stats"
	params := []string{}
	if withPool {
		params = append(params, "pool=true")
	}
	if byMachine {
		params = append(params, "byMachine=true")
	}
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}
	var resp DaemonStatsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, fmt.Errorf("daemon stats: %w", err)
	}
	return &resp, nil
}

// Pause sends a pause command to the daemon (stops accepting new sessions).
func (c *DaemonClient) Pause() (*DaemonActionResponse, error) {
	var resp DaemonActionResponse
	if err := c.post("/api/daemon/pause", nil, &resp); err != nil {
		return nil, fmt.Errorf("daemon pause: %w", err)
	}
	return &resp, nil
}

// Resume sends a resume command to the daemon (re-enables accepting sessions).
func (c *DaemonClient) Resume() (*DaemonActionResponse, error) {
	var resp DaemonActionResponse
	if err := c.post("/api/daemon/resume", nil, &resp); err != nil {
		return nil, fmt.Errorf("daemon resume: %w", err)
	}
	return &resp, nil
}

// Stop sends a stop command to the daemon. In-flight sessions are interrupted.
func (c *DaemonClient) Stop() (*DaemonActionResponse, error) {
	var resp DaemonActionResponse
	if err := c.post("/api/daemon/stop", nil, &resp); err != nil {
		return nil, fmt.Errorf("daemon stop: %w", err)
	}
	return &resp, nil
}

// Drain initiates a graceful drain. timeoutSeconds=0 uses the daemon default.
func (c *DaemonClient) Drain(timeoutSeconds int) (*DaemonActionResponse, error) {
	var resp DaemonActionResponse
	req := DaemonDrainRequest{TimeoutSeconds: timeoutSeconds}
	if err := c.post("/api/daemon/drain", req, &resp); err != nil {
		return nil, fmt.Errorf("daemon drain: %w", err)
	}
	return &resp, nil
}

// Update triggers a manual daemon update check.
func (c *DaemonClient) Update() (*DaemonActionResponse, error) {
	var resp DaemonActionResponse
	if err := c.post("/api/daemon/update", nil, &resp); err != nil {
		return nil, fmt.Errorf("daemon update: %w", err)
	}
	return &resp, nil
}

// GetPoolStats fetches the full workarea pool state, including per-member
// detail and aggregate counts.  The daemon response includes Layer 6
// correlation IDs so observability subscribers (REN-1313) can correlate events.
func (c *DaemonClient) GetPoolStats() (*WorkareaPoolStats, error) {
	var resp WorkareaPoolStats
	if err := c.get("/api/daemon/pool/stats", &resp); err != nil {
		return nil, fmt.Errorf("daemon pool stats: %w", err)
	}
	return &resp, nil
}

// EvictPool posts an eviction request to the daemon.  Pool members matching
// repoURL and older than the threshold in req are scheduled for destruction.
// The daemon emits a Layer 6 hook event whose correlation ID is echoed back.
func (c *DaemonClient) EvictPool(req EvictPoolRequest) (*EvictPoolResponse, error) {
	var resp EvictPoolResponse
	if err := c.post("/api/daemon/pool/evict", req, &resp); err != nil {
		return nil, fmt.Errorf("daemon pool evict: %w", err)
	}
	return &resp, nil
}

// SetCapacityConfig posts a capacity key-value update to the daemon.  The
// daemon writes the change to ~/.rensei/daemon.yaml atomically and reloads the
// affected subsystem (e.g. the LRU eviction trigger for poolMaxDiskGb).
func (c *DaemonClient) SetCapacityConfig(key, value string) (*SetCapacityResponse, error) {
	body := map[string]string{"key": key, "value": value}
	var resp SetCapacityResponse
	if err := c.post("/api/daemon/capacity", body, &resp); err != nil {
		return nil, fmt.Errorf("daemon set capacity: %w", err)
	}
	return &resp, nil
}
