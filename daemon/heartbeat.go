package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	// GetAllowlist returns the daemon's current project allowlist entries
	// (derived from cfg.Projects). Called every beat so a hot yaml reload
	// (when that lands) or in-process mutation reflects in the next
	// heartbeat. Returning nil is the canonical "no projects configured"
	// signal and triggers an empty AllowlistHash. Optional — callers that
	// don't care about allowlist sync can leave it nil.
	//
	// Phase 1d of 2026-05-18-daemon-config-sync-DESIGN.md.
	GetAllowlist func() []ProjectAllowlistEntry

	// OnPendingMutations is invoked when the platform attaches one or more
	// queued daemon-config mutations to a heartbeat response. The callback
	// is expected to apply each mutation against daemon.yaml and return
	// which ones succeeded (appliedIDs) and which failed
	// (failures). The HeartbeatService buffers these and includes them in
	// the NEXT beat's appliedMutations[] / mutationFailures[] fields so
	// the platform can ACK and emit audit events.
	//
	// Optional — leave nil to ignore platform-initiated mutations (the
	// daemon will keep working off its yaml as-edited locally). Phase 2c.
	OnPendingMutations func(ctx context.Context, mutations []PendingMutation) (appliedIDs []string, failures []HeartbeatMutationFailure)

	// OnHostStatus is invoked when the platform's heartbeat response
	// reports a non-ok hostStatus (e.g. pool_deleted). The daemon can
	// use this to surface re-register guidance in `af daemon stats` or
	// to enter a non-claiming state. Called with the latest status on
	// every beat that includes one, so callers can rely on it to clear
	// (status='ok') as well.
	//
	// Optional — leave nil to ignore. Phase 2e.
	OnHostStatus func(detail HostStatusDetail)

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
	// reason is the structured failure reason ("worker-not-found",
	// "runtime-token-expired", "unauthorized", "auth-failure"). Callers
	// should pass it through to RefreshRuntimeToken so the correct
	// recovery path is taken — in particular, "worker-not-found" skips
	// the JWT refresh probe and goes directly to full re-registration
	// (creating a new Redis entry), while "runtime-token-expired" tries
	// the refresh probe first to preserve the workerId.
	//
	// Required when the daemon runs against a real platform; tests that
	// only exercise the local stub path can leave it nil.
	OnReregister func(ctx context.Context, reason string) (workerID, runtimeJWT string, err error)
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

	// lastAllowlistHash tracks the most recently transmitted allowlist
	// hash so we only re-send the full entry list when it changes. Empty
	// string forces the next beat to include the list (covers the boot
	// case and the "previously empty, now populated" transition).
	lastAllowlistHash string

	// pendingApplied / pendingFailures buffer mutation ACKs that arrived
	// in the previous heartbeat response, were applied locally, and are
	// waiting to be reported on the next outbound beat. Cleared only on a
	// successful heartbeat call — a network failure leaves them buffered
	// so they re-ride the next attempt.
	pendingApplied  []string
	pendingFailures []HeartbeatMutationFailure
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

	// Phase 1d: attach allowlist hash every beat, full list only on change.
	if h.opts.GetAllowlist != nil {
		entries := h.opts.GetAllowlist()
		hash := allowlistHash(entries)
		payload.AllowlistHash = hash
		h.mu.Lock()
		if hash != h.lastAllowlistHash {
			payload.Allowlist = entries
			h.lastAllowlistHash = hash
		}
		h.mu.Unlock()
	}

	// Phase 2c: pull any ACKs we owe the platform from the buffer. Cleared
	// only on a SUCCESSFUL POST below; a network failure leaves them
	// queued for the next attempt.
	h.mu.Lock()
	ackApplied := append([]string(nil), h.pendingApplied...)
	ackFailures := append([]HeartbeatMutationFailure(nil), h.pendingFailures...)
	h.mu.Unlock()

	h.mu.Lock()
	h.last = payload
	h.mu.Unlock()

	if h.opts.OnHeartbeat != nil {
		h.opts.OnHeartbeat(payload)
	}

	// Real-endpoint call is gated on (a) operator-requested stub mode and
	// (b) whether the runtime JWT is a stub (the stub path returns a token
	// with the "stub." prefix). REN-1444 inverted the default to real
	// registration; the JWT-prefix check ensures a daemon configured with
	// a non-rs[pk]_live token still does not try to call prod with a
	// stub token.
	h.mu.Lock()
	jwt := h.jwt
	h.mu.Unlock()
	if stubModeRequested() || strings.HasPrefix(jwt, "stub.") {
		return
	}
	resp, err := h.callEndpoint(ctx, payload, ackApplied, ackFailures)
	if err == nil {
		// Successful POST — drop the ACKs we just confirmed, then process
		// the platform's response (hostStatus + pendingMutations).
		h.dropConfirmedAcks(ackApplied, ackFailures)
		h.handleHeartbeatResponse(ctx, resp)
		return
	}
	// On 401 (token expired/invalid) or 404 (worker fell out of Redis),
	// re-register and retry once with fresh credentials. Any other error
	// is logged and left for the platform to detect via missed heartbeats.
	if isAuthFailure(err) && h.opts.OnReregister != nil {
		// Surface the structured [runtime-token] event so REN-1481
		// observers can grep one line per cycle rather than parsing
		// the LogWarn body. The OnReregister implementation is also
		// expected to log the resolution event ("refresh" or
		// "reregister") via RefreshRuntimeToken.
		reason := authFailureReason(err)
		slog.Info("[runtime-token]",
			"event", "auth-failure-detected",
			"path", "heartbeat",
			"reason", reason,
		)
		h.opts.LogWarn("daemon heartbeat rejected (%v) — refreshing runtime token (reason=%s)", err, reason)
		newWorkerID, newJWT, regErr := h.opts.OnReregister(ctx, reason)
		if regErr != nil {
			h.opts.LogWarn("daemon runtime-token refresh failed: %v", regErr)
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
		retryResp, retryErr := h.callEndpoint(ctx, retryPayload, ackApplied, ackFailures)
		if retryErr != nil {
			h.opts.LogWarn("daemon heartbeat post-refresh also failed: %v", retryErr)
		} else {
			h.dropConfirmedAcks(ackApplied, ackFailures)
			h.handleHeartbeatResponse(ctx, retryResp)
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
//	{ activeCount, maxSessions?, load?, allowlistHash?, allowlist?,
//	  appliedMutations?, mutationFailures? }
//
// allowlistHash + allowlist are Phase 1d fields; appliedMutations +
// mutationFailures are Phase 2c ACK fields.
type heartbeatRequestBody struct {
	ActiveCount      int                        `json:"activeCount"`
	MaxSessions      int                        `json:"maxSessions,omitempty"`
	Load             *heartbeatLoadFields       `json:"load,omitempty"`
	AllowlistHash    string                     `json:"allowlistHash,omitempty"`
	Allowlist        []ProjectAllowlistEntry    `json:"allowlist,omitempty"`
	AppliedMutations []string                   `json:"appliedMutations,omitempty"`
	MutationFailures []HeartbeatMutationFailure `json:"mutationFailures,omitempty"`
}

type heartbeatLoadFields struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

// HeartbeatMutationFailure is sent in the request body's mutationFailures[]
// to ACK a queued daemon-config mutation that failed locally.
type HeartbeatMutationFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// PendingMutation mirrors the platform's serializePendingMutation wire shape
// — included in the heartbeat response so the daemon can apply queued
// proposals and ACK on the next beat. Phase 2 of
// 2026-05-18-daemon-config-sync-DESIGN.md.
type PendingMutation struct {
	ID          string          `json:"id"`
	Op          string          `json:"op"` // project.add | project.remove
	Params      json.RawMessage `json:"params"`
	RequestedAt string          `json:"requestedAt"`
	RequestedBy string          `json:"requestedBy"`
}

// HostStatusDetail mirrors the platform's wire shape for hostStatus in the
// heartbeat response. The daemon uses this to decide whether to keep
// claiming work or surface a re-register recommendation.
type HostStatusDetail struct {
	Status            string   `json:"status"` // ok | pool_deleted | pool_draining | pool_disabled | unauthorized
	RecommendedAction string   `json:"recommendedAction,omitempty"`
	CandidatePoolIDs  []string `json:"candidatePoolIds,omitempty"`
}

// heartbeatResponseBody is the JSON the platform sends back from the
// heartbeat endpoint. The pre-Phase-2 platform returned just
// {acknowledged, serverTime, pendingWorkCount}; Phase 2 adds hostStatus
// and pendingMutations. Both are optional — a daemon talking to a
// pre-Phase-2 platform unmarshals the missing fields to zero values.
type heartbeatResponseBody struct {
	Acknowledged     bool              `json:"acknowledged"`
	ServerTime       string            `json:"serverTime"`
	PendingWorkCount int               `json:"pendingWorkCount"`
	HostStatus       *HostStatusDetail `json:"hostStatus,omitempty"`
	PendingMutations []PendingMutation `json:"pendingMutations,omitempty"`
}

func (h *HeartbeatService) callEndpoint(
	ctx context.Context,
	payload HeartbeatPayload,
	ackApplied []string,
	ackFailures []HeartbeatMutationFailure,
) (*heartbeatResponseBody, error) {
	h.mu.Lock()
	workerID := h.workerID
	jwt := h.jwt
	h.mu.Unlock()
	if workerID == "" {
		return nil, fmt.Errorf("no worker id")
	}
	url := strings.TrimRight(h.opts.OrchestratorURL, "/") + "/api/workers/" + workerID + "/heartbeat"

	body := heartbeatRequestBody{
		ActiveCount:      payload.ActiveSessions,
		MaxSessions:      payload.MaxSessions,
		AllowlistHash:    payload.AllowlistHash,
		Allowlist:        payload.Allowlist,
		AppliedMutations: ackApplied,
		MutationFailures: ackFailures,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := h.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		snippet := strings.TrimSpace(string(errBuf))
		return nil, &heartbeatHTTPError{status: res.StatusCode, body: snippet}
	}
	// Parse response body (Phase 2). Pre-Phase-2 platforms send only
	// {acknowledged, serverTime, pendingWorkCount} — the additional
	// fields unmarshal as zero values.
	var resp heartbeatResponseBody
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		// Unparseable response body — log and fall back to "ACK accepted,
		// no mutations to apply". The next beat will re-try the round trip.
		h.opts.LogWarn("daemon heartbeat response unparseable: %v", err)
		return &heartbeatResponseBody{Acknowledged: true}, nil
	}
	return &resp, nil
}

// dropConfirmedAcks removes the ACK entries the platform just accepted
// from the pending buffer. Compares by mutation id so concurrent applies
// landing during the in-flight beat don't get lost.
func (h *HeartbeatService) dropConfirmedAcks(applied []string, failures []HeartbeatMutationFailure) {
	if len(applied) == 0 && len(failures) == 0 {
		return
	}
	confirmedApplied := make(map[string]bool, len(applied))
	for _, id := range applied {
		confirmedApplied[id] = true
	}
	confirmedFailed := make(map[string]bool, len(failures))
	for _, f := range failures {
		confirmedFailed[f.ID] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.pendingApplied[:0]
	for _, id := range h.pendingApplied {
		if !confirmedApplied[id] {
			next = append(next, id)
		}
	}
	h.pendingApplied = append([]string(nil), next...)
	nextF := h.pendingFailures[:0]
	for _, f := range h.pendingFailures {
		if !confirmedFailed[f.ID] {
			nextF = append(nextF, f)
		}
	}
	h.pendingFailures = append([]HeartbeatMutationFailure(nil), nextF...)
}

// handleHeartbeatResponse invokes the response-side callbacks: hostStatus
// surfacing and pending-mutation application. Applied/failed mutation ids
// returned by OnPendingMutations are buffered for ACK on the next beat.
func (h *HeartbeatService) handleHeartbeatResponse(ctx context.Context, resp *heartbeatResponseBody) {
	if resp == nil {
		return
	}
	if resp.HostStatus != nil && h.opts.OnHostStatus != nil {
		h.opts.OnHostStatus(*resp.HostStatus)
	}
	if len(resp.PendingMutations) > 0 && h.opts.OnPendingMutations != nil {
		applied, failures := h.opts.OnPendingMutations(ctx, resp.PendingMutations)
		if len(applied) > 0 || len(failures) > 0 {
			h.mu.Lock()
			h.pendingApplied = append(h.pendingApplied, applied...)
			h.pendingFailures = append(h.pendingFailures, failures...)
			h.mu.Unlock()
		}
	}
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

// authFailureReason classifies the auth-failure error into a stable
// short string for the [runtime-token] log line. Distinguishes
// runtime-token-expired (the canonical REN-1481 trigger) from
// worker-not-found and generic-unauthorized so operators can tell
// which path the daemon entered without scraping the response body.
func authFailureReason(err error) string {
	var hErr *heartbeatHTTPError
	if errors.As(err, &hErr) {
		switch hErr.status {
		case http.StatusUnauthorized:
			if strings.Contains(hErr.body, "Runtime token expired") {
				return "runtime-token-expired"
			}
			return "unauthorized"
		case http.StatusNotFound:
			return "worker-not-found"
		}
	}
	return "auth-failure"
}
