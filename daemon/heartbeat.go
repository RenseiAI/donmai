package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// HeartbeatOptions configure a HeartbeatService.
type HeartbeatOptions struct {
	WorkerID        string
	Hostname        string
	OrchestratorURL string
	// RuntimeJWT is the runtime token (a JWT) returned by /api/workers/register
	// and sent in Authorization: Bearer on every heartbeat.
	RuntimeJWT      string
	IntervalSeconds int
	GetActiveCount  func() int
	GetMaxCount     func() int
	GetStatus       func() RegistrationStatus
	Region          string

	// HTTPClient is the client used for the real-endpoint call.
	HTTPClient *http.Client
	// LogWarn is called when the real-endpoint call fails (transient
	// failures are non-fatal — the platform will detect via missed
	// heartbeats and Redis TTL expiry).
	LogWarn func(format string, args ...any)
	// Now provides the heartbeat sentAt timestamp.
	Now func() time.Time
	// OnHeartbeat is invoked after each heartbeat payload is composed
	// (whether or not the network call succeeded). Used by tests and
	// observability.
	OnHeartbeat func(payload HeartbeatPayload)
	// OnReregister is called when the runtime token is rejected (HTTP 401)
	// or the worker is reported missing (HTTP 404 — likely Redis TTL
	// expired). Implementations re-issue Register() against the platform
	// and return the fresh worker id + runtime token. Returning a non-nil
	// error leaves the heartbeat in its prior state and logs via LogWarn;
	// the next tick retries the heartbeat with the stale token (which will
	// fail again and re-trigger this path).
	//
	// Required when the daemon runs against a real platform; tests that
	// only exercise the local stub path can leave it nil.
	OnReregister func(ctx context.Context) (workerID, runtimeJWT string, err error)
}

// HeartbeatService manages the periodic heartbeat goroutine. It is safe to
// Start / Stop multiple times; consecutive Starts are idempotent.
type HeartbeatService struct {
	opts HeartbeatOptions

	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	last     HeartbeatPayload
	workerID string // mutable: refreshed by OnReregister
	jwt      string // mutable: refreshed by OnReregister
}

// NewHeartbeatService constructs a HeartbeatService from opts. Required
// callbacks are GetActiveCount, GetMaxCount, and GetStatus.
func NewHeartbeatService(opts HeartbeatOptions) *HeartbeatService {
	if opts.IntervalSeconds <= 0 {
		opts.IntervalSeconds = int(HeartbeatDefaultInterval / time.Second)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LogWarn == nil {
		opts.LogWarn = func(string, ...any) {}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &HeartbeatService{
		opts:     opts,
		workerID: opts.WorkerID,
		jwt:      opts.RuntimeJWT,
	}
}

// Start launches the heartbeat goroutine. It sends an immediate heartbeat,
// then continues at IntervalSeconds. Subsequent calls are no-ops.
func (h *HeartbeatService) Start() {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.running = true
	h.mu.Unlock()

	go h.loop(ctx)
}

// Stop terminates the heartbeat goroutine. Safe to call multiple times.
func (h *HeartbeatService) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.running = false
}

// IsRunning reports whether the heartbeat goroutine is active.
func (h *HeartbeatService) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// LastPayload returns the most recently composed heartbeat payload (for
// debugging / status surfaces).
func (h *HeartbeatService) LastPayload() HeartbeatPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// CurrentCredentials returns the worker id and runtime JWT currently in
// use. They may differ from the values passed at construction time after a
// re-register on 401.
func (h *HeartbeatService) CurrentCredentials() (workerID, runtimeJWT string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workerID, h.jwt
}

func (h *HeartbeatService) loop(ctx context.Context) {
	// Immediate first heartbeat.
	h.sendOne(ctx)

	tick := time.NewTicker(time.Duration(h.opts.IntervalSeconds) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.sendOne(ctx)
		}
	}
}

func (h *HeartbeatService) sendOne(ctx context.Context) {
	payload := HeartbeatPayload{
		WorkerID:       h.workerIDLocked(),
		Hostname:       h.opts.Hostname,
		Status:         h.opts.GetStatus(),
		ActiveSessions: h.opts.GetActiveCount(),
		MaxSessions:    h.opts.GetMaxCount(),
		Region:         h.opts.Region,
		SentAt:         h.opts.Now().UTC().Format(time.RFC3339),
	}

	h.mu.Lock()
	h.last = payload
	h.mu.Unlock()

	if h.opts.OnHeartbeat != nil {
		h.opts.OnHeartbeat(payload)
	}

	// Real-endpoint call is gated on the same env switch as registration so
	// tests / dev runs against file:// or stub paths don't reach prod.
	if os.Getenv("RENSEI_DAEMON_REAL_REGISTRATION") == "" {
		return
	}
	err := h.callEndpoint(ctx, payload)
	if err == nil {
		return
	}
	// On 401 (token expired/invalid) or 404 (worker fell out of Redis),
	// re-register and retry once with fresh credentials. Any other error
	// is logged and left for the platform to detect via missed heartbeats.
	if isAuthFailure(err) && h.opts.OnReregister != nil {
		h.opts.LogWarn("daemon heartbeat rejected (%v) — re-registering", err)
		newWorkerID, newJWT, regErr := h.opts.OnReregister(ctx)
		if regErr != nil {
			h.opts.LogWarn("daemon re-register failed: %v", regErr)
			return
		}
		h.mu.Lock()
		h.workerID = newWorkerID
		h.jwt = newJWT
		h.mu.Unlock()
		// Re-send with fresh credentials. If this also fails we log and
		// move on — the next tick will try again.
		retryPayload := payload
		retryPayload.WorkerID = newWorkerID
		if retryErr := h.callEndpoint(ctx, retryPayload); retryErr != nil {
			h.opts.LogWarn("daemon heartbeat post-reregister also failed: %v", retryErr)
		}
		return
	}
	h.opts.LogWarn("daemon heartbeat HTTP call failed: %v — orchestrator will detect via missed heartbeats", err)
}

// workerIDLocked returns the current worker id under the lock.
func (h *HeartbeatService) workerIDLocked() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workerID
}

// heartbeatRequestBody is the JSON body sent on POST
// /api/workers/<id>/heartbeat. Matches the platform contract:
//
//	{ activeCount: number, load?: { cpu, memory } }
type heartbeatRequestBody struct {
	ActiveCount int                  `json:"activeCount"`
	Load        *heartbeatLoadFields `json:"load,omitempty"`
}

type heartbeatLoadFields struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

func (h *HeartbeatService) callEndpoint(ctx context.Context, payload HeartbeatPayload) error {
	h.mu.Lock()
	workerID := h.workerID
	jwt := h.jwt
	h.mu.Unlock()
	if workerID == "" {
		return fmt.Errorf("no worker id")
	}
	url := strings.TrimRight(h.opts.OrchestratorURL, "/") + "/api/workers/" + workerID + "/heartbeat"

	body := heartbeatRequestBody{
		ActiveCount: payload.ActiveSessions,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := h.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		snippet := strings.TrimSpace(string(errBuf))
		return &heartbeatHTTPError{status: res.StatusCode, body: snippet}
	}
	return nil
}

// heartbeatHTTPError carries the HTTP status so callers can branch on 401
// without parsing strings.
type heartbeatHTTPError struct {
	status int
	body   string
}

func (e *heartbeatHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// isAuthFailure returns true for the HTTP statuses that indicate the runtime
// token must be refreshed via re-register: 401 (Unauthorized — JWT expired
// or invalid) and 404 (Worker not found — fell out of Redis after TTL).
func isAuthFailure(err error) bool {
	var hErr *heartbeatHTTPError
	if errors.As(err, &hErr) {
		return hErr.status == http.StatusUnauthorized || hErr.status == http.StatusNotFound
	}
	return false
}
