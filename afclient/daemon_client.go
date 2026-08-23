// Package afclient daemon_client.go — thin HTTP client for the local daemon's
// status/control API. The daemon listens on HTTP at 127.0.0.1:<port> from
// ~/.donmai/daemon.yaml. All paths are relative to that base URL.
package afclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// DaemonConfig holds the minimal daemon connection config read from
// ~/.donmai/daemon.yaml (or overridden by env/flag).
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
	// ProjectAdmissionVersion is 2 when project IDs are explicit and an empty
	// set means serve no projects.
	ProjectAdmissionVersion int `json:"projectAdmissionVersion,omitempty"`
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
	// EnabledProjectIDs is the desired project-admission set from config.
	EnabledProjectIDs []string `json:"enabledProjectIds,omitempty"`
	// AppliedProjectIDs is the project set currently applied by the runtime.
	AppliedProjectIDs []string `json:"appliedProjectIds,omitempty"`
	// ProjectAdmissionMode is the machine owner's standing consent mode:
	// "enumerated" (only EnabledProjectIDs run here) or "all-routed" (any
	// project the orchestrator routes here runs). Empty reads as "enumerated".
	ProjectAdmissionMode string `json:"projectAdmissionMode,omitempty"`

	// Projects carries desired/applied/connection/repository state per project.
	Projects []DaemonProjectStatus `json:"projects,omitempty"`
	// SessionShim is the secret-free local ownership/adoption diagnostic. It is
	// always present, including mode=disabled, so status and doctor never require
	// callers to infer whether omission means unsupported or empty.
	SessionShim DaemonSessionShimStatus `json:"sessionShim"`
	// Timestamp is the RFC3339 time of this snapshot.
	Timestamp string `json:"timestamp"`
}

// DaemonSessionShimOwnershipMode is the configured controller/ownership mode.
type DaemonSessionShimOwnershipMode string

const (
	// DaemonSessionShimDisabled means neither adoption nor new shim ownership is enabled.
	DaemonSessionShimDisabled DaemonSessionShimOwnershipMode = "disabled"
	// DaemonSessionShimAdoptionOnly means startup adoption is enabled but new sessions stay direct-owned.
	DaemonSessionShimAdoptionOnly DaemonSessionShimOwnershipMode = "adoption_only"
	// DaemonSessionShimOwnershipOnly means new shim ownership is enabled without startup adoption.
	DaemonSessionShimOwnershipOnly DaemonSessionShimOwnershipMode = "ownership_only"
	// DaemonSessionShimAdoptionAndOwnership means both migration stages are enabled.
	DaemonSessionShimAdoptionAndOwnership DaemonSessionShimOwnershipMode = "adoption_and_ownership"
)

// DaemonSessionShimAdoptedCorrelation is one live controller correlation. It
// carries only bounded process/fencing diagnostics sourced from authenticated
// shim Hello and daemon durability state; no path, credential, output, host id,
// token, or opaque composing receipt belongs here. The non-secret process-scoped
// controller id is included as immutable correlation evidence.
type DaemonSessionShimAdoptedCorrelation struct {
	OrgID                  string `json:"orgId"`
	SessionID              string `json:"sessionId"`
	ShimID                 string `json:"shimId"`
	ProcessEpoch           uint64 `json:"processEpoch,omitempty"`
	ControllerGeneration   uint64 `json:"controllerGeneration,omitempty"`
	LastForwardedSeq       uint64 `json:"lastForwardedSeq"`
	HarnessPID             int    `json:"harnessPid,omitempty"`
	HarnessStartedAt       int64  `json:"harnessStartedAt,omitempty"`
	ProtocolMin            uint32 `json:"protocolMin,omitempty"`
	ProtocolMax            uint32 `json:"protocolMax,omitempty"`
	Phase                  string `json:"phase,omitempty"`
	Source                 string `json:"source"`
	ConsumesCapacity       bool   `json:"consumesCapacity"`
	ControllerID           string `json:"controllerId,omitempty"`
	ProtocolVersion        uint32 `json:"protocolVersion,omitempty"`
	AuthoritativeSnapshot  bool   `json:"authoritativeSnapshot"`
	CarrierIncompatibility string `json:"carrierIncompatibility,omitempty"`
}

// DaemonSessionShimStatus is shared byte-for-byte by status and doctor.
type DaemonSessionShimStatus struct {
	OwnershipMode       DaemonSessionShimOwnershipMode        `json:"ownershipMode"`
	AdoptionComplete    bool                                  `json:"adoptionComplete"`
	AdoptionCompletedAt string                                `json:"adoptionCompletedAt,omitempty"`
	OccupiedSlots       int                                   `json:"occupiedSlots"`
	Adopted             []DaemonSessionShimAdoptedCorrelation `json:"adopted,omitempty"`
	Quarantined         []sessionshim.QuarantinedSession      `json:"quarantined,omitempty"`
	ControllerID        string                                `json:"controllerId,omitempty"`
}

// DaemonProjectStatus is one truthful host-project status row.
type DaemonProjectStatus struct {
	ProjectID           string   `json:"projectId"`
	Desired             string   `json:"desired"`
	Applied             string   `json:"applied"`
	Connection          string   `json:"connection"`
	RepositoryCount     int      `json:"repositoryCount"`
	PrimaryRepositoryID string   `json:"primaryRepositoryId,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

// DaemonStatsResponse is the response from GET /api/daemon/stats.
type DaemonStatsResponse struct {
	ProjectAdmissionVersion int `json:"projectAdmissionVersion,omitempty"`
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
	// if registration has not yet completed.
	WorkerID string `json:"workerId,omitempty"`
	// Registration carries the human-readable registration status and the
	// timestamp of the most recent successful heartbeat.
	Registration *DaemonRegistrationStats `json:"registration,omitempty"`
	// AllowedProjects is the list of repositories in the daemon's allowlist
	// (from daemon.yaml). May be empty when no projects have been
	// configured.
	AllowedProjects []string `json:"allowedProjects,omitempty"`
	// EnabledProjectIDs is the desired project-admission set from config.
	EnabledProjectIDs []string `json:"enabledProjectIds,omitempty"`
	// AppliedProjectIDs is the project set currently applied by the runtime.
	AppliedProjectIDs []string `json:"appliedProjectIds,omitempty"`
	// ProjectAdmissionMode is the machine owner's standing consent mode:
	// "enumerated" (only EnabledProjectIDs run here) or "all-routed" (any
	// project the orchestrator routes here runs). Empty reads as "enumerated".
	ProjectAdmissionMode string `json:"projectAdmissionMode,omitempty"`
}

// DaemonRegistrationStats summarises the daemon's connection to the platform
// for `daemon stats` consumers.
type DaemonRegistrationStats struct {
	// Status is the registration status reported in the most recent
	// heartbeat: idle / busy / draining / stub / unregistered.
	Status string `json:"status,omitempty"`
	// LastHeartbeatAt is the RFC3339 timestamp of the last heartbeat
	// payload composed by the daemon. Empty when no heartbeat has run.
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
	// HeartbeatRunning reports whether the heartbeat goroutine is active.
	HeartbeatRunning bool `json:"heartbeatRunning,omitempty"`
	// PollRunning reports whether the poll goroutine is active.
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

// DaemonRestartPreflightProtocol is the closed localhost permission wire for a
// planned service-manager restart.
const DaemonRestartPreflightProtocol = "session-shim-restart-preflight-v1"

// DaemonRestartPreflightState is the closed permission result registry.
type DaemonRestartPreflightState string

const (
	// DaemonRestartPrepared means every non-empty authority scope durably
	// acknowledged the daemon's one immutable restart snapshot.
	DaemonRestartPrepared DaemonRestartPreflightState = "prepared"
	// DaemonRestartNotRequired means the atomically drained daemon proved the
	// frozen snapshot has no session-shim correlations to fence.
	DaemonRestartNotRequired DaemonRestartPreflightState = "not_required"
)

// DaemonRestartPreflightResponse is the only 2xx permission shape accepted by
// planned-restart callers. PreparationID is server-owned and opaque; callers
// never provide a fence id or inspect per-scope receipts.
type DaemonRestartPreflightResponse struct {
	Protocol      string                      `json:"protocol"`
	State         DaemonRestartPreflightState `json:"state"`
	PreparationID string                      `json:"preparationId"`
	ScopeCount    int                         `json:"scopeCount"`
}

// DaemonRestartPreflightRefusalCode is the closed typed 409 refusal code.
const DaemonRestartPreflightRefusalCode = "restart_preflight_refused"

// DaemonRestartPreflightRefusal is the daemon's typed non-permission body.
type DaemonRestartPreflightRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// DaemonRestartPreflightRefusalError preserves the typed refusal for callers
// while remaining compatible with errors.Is checks for ErrConflict.
type DaemonRestartPreflightRefusalError struct {
	Code    string
	Message string
}

func (e *DaemonRestartPreflightRefusalError) Error() string {
	if e == nil || e.Message == "" {
		return ErrRestartPreflightRefused.Error()
	}
	return ErrRestartPreflightRefused.Error() + ": " + e.Message
}

func (e *DaemonRestartPreflightRefusalError) Unwrap() []error {
	return []error{ErrRestartPreflightRefused, ErrConflict}
}

// DaemonDrainRequest is the optional body for POST /api/daemon/drain.
type DaemonDrainRequest struct {
	// TimeoutSeconds is the max time to wait for in-flight work to drain.
	// 0 means use the daemon's configured default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// DaemonSessionHandle is the client-side mirror of the daemon's
// SessionHandle wire shape returned by GET /api/daemon/sessions. It is
// duplicated here (rather than importing the daemon package, which would
// create a dependency cycle) so client consumers — e.g. the host-watch
// fleet dashboard — can decode the list endpoint with no platform call.
//
// The WorktreePath / ProjectName / Repository enrichment fields let a
// local reader locate each session's on-disk worktree (and thus its
// `.agent/events.jsonl` + `.agent/state.json`) without a per-session
// GET /api/daemon/sessions/<id> round-trip. They are omitempty and may be
// absent against a pre-enrichment daemon — the reader then falls back to a
// per-session detail call. See
// ADR-2026-06-13-daemon-sessionhandle-enrichment.
type DaemonSessionHandle struct {
	// SessionID is the platform session UUID.
	SessionID string `json:"sessionId"`
	// PID is the spawned worker process id.
	PID int `json:"pid"`
	// AcceptedAt is the RFC3339 time the session was admitted.
	AcceptedAt string `json:"acceptedAt"`
	// State is the session lifecycle state (starting/running/...).
	State string `json:"state"`
	// WorktreePath is the absolute on-disk worktree path for the session
	// (<parent>/<sessionID>). Empty when the daemon cannot resolve it.
	WorktreePath string `json:"worktreePath,omitempty"`
	// ProjectName is the allowlist-resolved project identifier.
	ProjectName string `json:"projectName,omitempty"`
	// Repository is the git URL (or owner/name slug) the session runs on.
	Repository string `json:"repository,omitempty"`
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
	if err := daemonStatusToError(resp, path); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func (c *DaemonClient) post(path string, body any, target any) error {
	return c.postContext(context.Background(), c.httpClient, path, body, target)
}

func (c *DaemonClient) postContext(ctx context.Context, httpClient *http.Client, path string, body any, target any) error {
	var reqBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&reqBody).Encode(body); err != nil {
			return fmt.Errorf("encode body: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := daemonStatusToError(resp, path); err != nil {
		return err
	}
	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}

// daemonStatusToError maps a non-2xx daemon response to the shared
// sentinel errors (statusToError) and, when the daemon supplied a JSON
// `{"error": "..."}` body, appends that detail so callers can show the
// daemon's actual reason (e.g. the kit trust gate's remediation steps)
// instead of a bare "unauthorized". errors.Is against the sentinels
// keeps working — the detail is appended via %w-wrapping the sentinel
// error.
func daemonStatusToError(resp *http.Response, path string) error {
	err := statusToError(resp.StatusCode, path)
	if err == nil {
		return nil
	}
	var body struct {
		Error string `json:"error"`
	}
	if decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); decodeErr == nil && body.Error != "" {
		return fmt.Errorf("%w: %s", err, body.Error)
	}
	return err
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

// GetSessions fetches the daemon's active session handles from
// GET /api/daemon/sessions. The returned handles carry the worktree
// path / project / repository enrichment when the daemon supports it,
// letting a local reader locate each session's `.agent/` log files with
// no further round-trip. A nil slice with a nil error means the daemon
// reported no active sessions.
func (c *DaemonClient) GetSessions() ([]DaemonSessionHandle, error) {
	var resp []DaemonSessionHandle
	if err := c.get("/api/daemon/sessions", &resp); err != nil {
		return nil, fmt.Errorf("daemon sessions: %w", err)
	}
	return resp, nil
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
	return c.DrainContext(context.Background(), timeoutSeconds)
}

// DrainContext initiates a graceful drain using ctx to bound transport waiting.
// timeoutSeconds=0 uses the daemon's configured default.
func (c *DaemonClient) DrainContext(ctx context.Context, timeoutSeconds int) (*DaemonActionResponse, error) {
	httpClient := *c.httpClient
	httpClient.Timeout = 0

	var resp DaemonActionResponse
	req := DaemonDrainRequest{TimeoutSeconds: timeoutSeconds}
	if err := c.postContext(ctx, &httpClient, "/api/daemon/drain", req, &resp); err != nil {
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

// PrepareRestart asks the daemon to enter draining and durably prepare its one
// immutable session-shim restart snapshot. Only a returned closed response is
// permission to invoke the service manager.
func (c *DaemonClient) PrepareRestart() (*DaemonRestartPreflightResponse, error) {
	return c.prepareRestart(context.Background(), c.httpClient)
}

// PrepareRestartContext is PrepareRestart with caller-bounded transport wait.
// The client's ordinary 10-second timeout is disabled in favor of ctx because
// a multi-scope durable acknowledgement may legitimately take longer.
func (c *DaemonClient) PrepareRestartContext(ctx context.Context) (*DaemonRestartPreflightResponse, error) {
	httpClient := *c.httpClient
	httpClient.Timeout = 0
	return c.prepareRestart(ctx, &httpClient)
}

func (c *DaemonClient) prepareRestart(ctx context.Context, httpClient *http.Client) (*DaemonRestartPreflightResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/daemon/restart/prepare", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("daemon restart prepare: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon restart prepare: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		var refusal DaemonRestartPreflightRefusal
		dec := json.NewDecoder(io.LimitReader(resp.Body, 8192))
		dec.DisallowUnknownFields()
		if decodeErr := dec.Decode(&refusal); decodeErr == nil && refusal.Code == DaemonRestartPreflightRefusalCode {
			return nil, &DaemonRestartPreflightRefusalError{Code: refusal.Code, Message: refusal.Error}
		}
	}
	if err := daemonStatusToError(resp, "/api/daemon/restart/prepare"); err != nil {
		return nil, fmt.Errorf("daemon restart prepare: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrInvalidRestartPreflightResponse, err)
	}
	if len(raw) > 8192 {
		return nil, fmt.Errorf("%w: body exceeds 8192 bytes", ErrInvalidRestartPreflightResponse)
	}
	var result DaemonRestartPreflightResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRestartPreflightResponse, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing JSON value", ErrInvalidRestartPreflightResponse)
		}
		return nil, fmt.Errorf("%w: trailing data: %v", ErrInvalidRestartPreflightResponse, err)
	}
	if err := validateDaemonRestartPreflightResponse(result); err != nil {
		return nil, err
	}
	return &result, nil
}

func validateDaemonRestartPreflightResponse(result DaemonRestartPreflightResponse) error {
	if result.Protocol != DaemonRestartPreflightProtocol {
		return fmt.Errorf("%w: protocol %q", ErrInvalidRestartPreflightResponse, result.Protocol)
	}
	if strings.TrimSpace(result.PreparationID) == "" {
		return fmt.Errorf("%w: preparationId is empty", ErrInvalidRestartPreflightResponse)
	}
	switch result.State {
	case DaemonRestartPrepared:
		if result.ScopeCount <= 0 {
			return fmt.Errorf("%w: prepared scopeCount is %d", ErrInvalidRestartPreflightResponse, result.ScopeCount)
		}
	case DaemonRestartNotRequired:
		if result.ScopeCount != 0 {
			return fmt.Errorf("%w: not_required scopeCount is %d", ErrInvalidRestartPreflightResponse, result.ScopeCount)
		}
	default:
		return fmt.Errorf("%w: state %q", ErrInvalidRestartPreflightResponse, result.State)
	}
	return nil
}

// GetPoolStats fetches the full workarea pool state, including per-member
// detail and aggregate counts.  The daemon response includes Layer 6
// correlation IDs so observability subscribers can correlate events.
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
// daemon writes the change to ~/.donmai/daemon.yaml atomically and reloads the
// affected subsystem (e.g. the LRU eviction trigger for poolMaxDiskGb).
func (c *DaemonClient) SetCapacityConfig(key, value string) (*SetCapacityResponse, error) {
	body := map[string]string{"key": key, "value": value}
	var resp SetCapacityResponse
	if err := c.post("/api/daemon/capacity", body, &resp); err != nil {
		return nil, fmt.Errorf("daemon set capacity: %w", err)
	}
	return &resp, nil
}
