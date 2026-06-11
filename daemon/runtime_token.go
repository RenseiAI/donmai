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
	"time"
)

// RuntimeTokenRefreshEndpoint is the (probed) platform endpoint the
// daemon hits to refresh an expired runtime JWT WITHOUT re-registering.
// The platform owes a handler at this path that:
//   - accepts the registration token in the Authorization: Bearer header
//   - takes the existing workerId in the URL path
//   - mints a fresh runtime JWT bound to the SAME workerId
//   - returns { runtimeToken, runtimeTokenExpiresAt, heartbeatInterval, pollInterval }
//
// As of 2026-05-03 this endpoint does NOT exist on the platform side —
// a platform-side companion is required. Until it ships the daemon probes
// this URL, observes a 404, and falls back to full re-register (which
// mints a new workerId — the original 5-minute-cycle bug). When
// the platform side ships the endpoint the daemon picks it up
// automatically with no further changes.
// #nosec G101 -- URL endpoint path, not a credential
const RuntimeTokenRefreshEndpoint = "/api/workers/refresh-token"

// RefreshTokenResult is the outcome of an attempted runtime-token
// refresh. The OnReregister callback wired into HeartbeatService and
// PollService synthesises one of these per attempt; logged via the
// `[runtime-token]` structured line.
type RefreshTokenResult struct {
	// Mode is the path the refresh actually took: "refresh" (platform
	// honoured the refresh probe and minted a new JWT bound to the
	// same workerId), "reregister" (probe returned 404 / endpoint
	// missing — the daemon fell back to full POST /api/workers/register
	// and got a NEW workerId), or "error" (both paths failed).
	Mode string

	// WorkerID is the worker id in effect after the refresh attempt.
	// On Mode=refresh this is the SAME workerId; on Mode=reregister
	// it's a fresh one.
	WorkerID string

	// RuntimeToken is the fresh runtime JWT.
	RuntimeToken string

	// RuntimeTokenExpiresAt, HeartbeatInterval and PollInterval mirror the
	// platform's response (refresh endpoint or full re-register). Callers need
	// them to persist a COMPLETE CachedJWT after a refresh — the on-disk cache
	// entry carries the expiry + cadence, not just the token, so a daemon that
	// refreshes its token in-memory but writes back only the token would leave
	// a half-populated cache. Empty/zero when the platform omitted them.
	RuntimeTokenExpiresAt string
	HeartbeatInterval     int
	PollInterval          int

	// RegistrationTokenSwapped is true when Mode=reregister produced a
	// different workerId. Operators care about this signal because the
	// platform forgets the old workerId after a fresh registration —
	// any in-flight heartbeats / polls keyed on it 404 until the daemon
	// swaps credentials.
	RegistrationTokenSwapped bool

	// Reason is the structured reason the refresh path was taken
	// (e.g. "runtime-token-expired", "worker-not-found"). Surfaces in
	// the [runtime-token] log line.
	Reason string
}

// RefreshRuntimeToken attempts to refresh the daemon's runtime JWT
// without re-registering — i.e. preserving the workerId. This is the
// runtime-token refresh fix path. Behaviour:
//
//  1. When reason is "worker-not-found" (HTTP 404 on poll or heartbeat),
//     the worker's Redis registration entry has expired — the runtime
//     token itself is still valid, but the platform has no record of this
//     worker. Probing the refresh endpoint would return a fresh JWT for
//     the SAME workerId, which would loop forever. Skip the probe and go
//     directly to full re-register to create a new Redis entry.
//  2. Otherwise, probe POST /api/workers/<id>/refresh-token with the
//     registration token in the Authorization: Bearer header. On 200, the
//     platform mints a fresh JWT bound to the same workerId — best case.
//  3. On 404 (endpoint missing — current platform-side state) or 405
//     (method not allowed) from the refresh probe, fall through to FULL
//     re-register via Register(ForceReregister=true). The runtime token
//     gets refreshed but at the cost of a new workerId.
//  4. On any other failure (5xx, network, 401-on-registration-token),
//     return an error. Caller logs + retries on next tick.
//
// This is the only path that should call Register() with
// ForceReregister=true outside boot. All in-flight 401/404 detection
// in HeartbeatService / PollService routes through here so the
// `[runtime-token]` log line is the single source of truth for
// operators investigating the 5-minute re-register cycle.
func RefreshRuntimeToken(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) (*RefreshTokenResult, error) {
	logger := slog.Default()

	// One line per refresh attempt. The reason carries the trigger:
	// reactive ("runtime-token-expired", "worker-not-found", ...) or the
	// scheduled "proactive-expiry" path — so the historical "401" event
	// name no longer fits.
	logger.Info("[runtime-token]",
		"event", "refresh-requested",
		"workerId", currentWorkerID,
		"reason", reason,
	)

	// 1. Probe the refresh endpoint — but ONLY when the runtime token
	// itself may still be valid (401 path). A 404 "worker-not-found"
	// means the worker's Redis registration has expired; the refresh
	// endpoint would return a fresh JWT for the SAME workerId, which
	// the platform would then reject again on the next poll/heartbeat
	// (the Redis entry is still gone). Skip straight to full
	// re-registration so the platform creates a new Redis entry.
	workerNotFound := reason == "worker-not-found"
	if !workerNotFound && currentWorkerID != "" && looksLikeRegistrationToken(regOpts.RegistrationToken) {
		fresh, err := callRefreshEndpoint(ctx, regOpts, currentWorkerID)
		if err == nil {
			logger.Info("[runtime-token]",
				"event", "refresh",
				"workerId", currentWorkerID,
				"reason", reason,
			)
			return &RefreshTokenResult{
				Mode:                  "refresh",
				WorkerID:              currentWorkerID,
				RuntimeToken:          fresh.RuntimeToken,
				RuntimeTokenExpiresAt: fresh.RuntimeTokenExpiresAt,
				HeartbeatInterval:     fresh.HeartbeatInterval,
				PollInterval:          fresh.PollInterval,
				Reason:                reason,
			}, nil
		}
		// 404 / 405 → endpoint not deployed yet. Fall through to
		// re-register. Anything else surfaces as an error so the caller
		// logs + retries on next tick.
		var probeErr *refreshHTTPError
		if !errors.As(err, &probeErr) ||
			(probeErr.status != http.StatusNotFound && probeErr.status != http.StatusMethodNotAllowed) {
			logger.Warn("[runtime-token]",
				"event", "refresh.error",
				"workerId", currentWorkerID,
				"reason", reason,
				"err", err.Error(),
			)
			return nil, fmt.Errorf("refresh probe failed: %w", err)
		}
		logger.Info("[runtime-token]",
			"event", "refresh.unavailable",
			"workerId", currentWorkerID,
			"reason", reason,
			"detail", "platform refresh endpoint not deployed; falling back to full re-register (workerId will change — platform-side refresh endpoint pending)",
		)
	} else if workerNotFound {
		logger.Info("[runtime-token]",
			"event", "reregister.worker-not-found",
			"workerId", currentWorkerID,
			"reason", reason,
			"detail", "worker Redis entry expired — skipping refresh probe, going directly to full re-register to create a new registration entry",
		)
	}

	// 2. Fallback — full re-register. Burns the workerId per
	// platform's registerWorker() (always mints a fresh wkr_ uuid).
	regOpts.ForceReregister = true
	rr, rerr := Register(ctx, regOpts)
	if rerr != nil {
		logger.Warn("[runtime-token]",
			"event", "reregister.error",
			"workerId", currentWorkerID,
			"reason", reason,
			"err", rerr.Error(),
		)
		return nil, fmt.Errorf("reregister: %w", rerr)
	}
	swapped := rr.WorkerID != "" && rr.WorkerID != currentWorkerID
	logger.Info("[runtime-token]",
		"event", "reregister",
		"workerId", rr.WorkerID,
		"oldWorkerId", currentWorkerID,
		"reason", reason,
		"workerIdSwapped", swapped,
	)
	return &RefreshTokenResult{
		Mode:                     "reregister",
		WorkerID:                 rr.WorkerID,
		RuntimeToken:             rr.RuntimeToken,
		RuntimeTokenExpiresAt:    rr.RuntimeTokenExpiresAt,
		HeartbeatInterval:        rr.HeartbeatInterval,
		PollInterval:             rr.PollInterval,
		RegistrationTokenSwapped: swapped,
		Reason:                   reason,
	}, nil
}

// persistRefreshedToken writes the refreshed credentials to the on-disk JWT
// cache (daemon.jwt) so processes that read the token from disk — the
// per-session credential resolver and the runner's platform client — pick up
// the fresh token instead of the now-stale cached one. The daemon's heartbeat
// and poll loops swap the token in memory, but anything reading daemon.jwt does
// not see that swap; without this write they keep presenting the expired token
// (the platform then 307-redirects credential snapshots to an HTML login page
// and 401s status updates — the stale-cached-token credential root cause).
//
// Best-effort by contract: callers log and continue on error; a cache-write
// failure must never abort the refresh, since the in-memory swap already keeps
// poll + heartbeat alive. Returns nil for an empty path or nil result.
func persistRefreshedToken(jwtPath string, result *RefreshTokenResult, nowFn func() time.Time) error {
	if jwtPath == "" || result == nil {
		return nil
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return SaveCachedJWT(jwtPath, &RegisterResponse{
		WorkerID:              result.WorkerID,
		RuntimeToken:          result.RuntimeToken,
		HeartbeatInterval:     result.HeartbeatInterval,
		PollInterval:          result.PollInterval,
		RuntimeTokenExpiresAt: result.RuntimeTokenExpiresAt,
	}, nowFn())
}

// refreshHTTPError carries the HTTP status from the refresh probe so
// callers can distinguish "endpoint missing" (404 / 405) from other
// failures.
type refreshHTTPError struct {
	status int
	body   string
}

func (e *refreshHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("refresh: HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("refresh: HTTP %d", e.status)
}

// refreshResponse mirrors the (planned) platform refresh-endpoint
// response body. Only RuntimeToken is load-bearing today; the cadence
// fields are honoured when present and ignored when absent (existing
// services keep their current cadence).
type refreshResponse struct {
	RuntimeToken          string `json:"runtimeToken"`
	RuntimeTokenExpiresAt string `json:"runtimeTokenExpiresAt,omitempty"`
	HeartbeatInterval     int    `json:"heartbeatInterval,omitempty"`
	PollInterval          int    `json:"pollInterval,omitempty"`
}

// callRefreshEndpoint posts to the platform's refresh probe with the
// registration token in Authorization: Bearer + the workerId in the
// URL path. The path the daemon probes is
// `/api/workers/<id>/refresh-token`; until the platform side ships
// its companion handler the platform 404s and we fall through to
// re-register.
func callRefreshEndpoint(
	ctx context.Context,
	opts RegistrationOptions,
	workerID string,
) (*refreshResponse, error) {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(opts.OrchestratorURL, "/") +
		"/api/workers/" + workerID + "/refresh-token"
	body := bytes.NewBufferString("{}")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opts.RegistrationToken)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, &refreshHTTPError{status: res.StatusCode, body: strings.TrimSpace(string(errBuf))}
	}
	var resp refreshResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.RuntimeToken == "" {
		return nil, fmt.Errorf("refresh response missing runtimeToken")
	}
	return &resp, nil
}
