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
// see REN-1481 platform-companion. Until it ships the daemon probes
// this URL, observes a 404, and falls back to full re-register (which
// mints a new workerId, the bug REN-1481 originally documented). When
// the platform side ships the endpoint the daemon picks it up
// automatically with no further changes.
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

	// RegistrationTokenSwapped is true when Mode=reregister produced a
	// different workerId. Operators care about this signal because the
	// platform forgets the old workerId after a fresh registration —
	// any in-flight heartbeats / polls keyed on it 404 until the daemon
	// swaps credentials. (REN-1481 root cause.)
	RegistrationTokenSwapped bool

	// Reason is the structured reason the refresh path was taken
	// (e.g. "runtime-token-expired", "worker-not-found"). Surfaces in
	// the [runtime-token] log line.
	Reason string
}

// RefreshRuntimeToken attempts to refresh the daemon's runtime JWT
// without re-registering — i.e. preserving the workerId. This is the
// REN-1481 fix path. Behaviour:
//
//  1. Probe POST /api/workers/<id>/refresh-token with the registration
//     token in the Authorization: Bearer header. On 200, the platform
//     has minted a fresh JWT bound to the same workerId — best case.
//  2. On 404 (endpoint missing — current platform-side state) or 405
//     (method not allowed), fall through to FULL re-register via
//     Register(ForceReregister=true). The runtime token gets refreshed
//     but at the cost of a new workerId.
//  3. On any other failure (5xx, network, 401-on-registration-token),
//     return an error. Caller logs + retries on next tick.
//
// This is the only path that should call Register() with
// ForceReregister=true outside boot. All in-flight 401/404 detection
// in HeartbeatService / PollService routes through here so the
// `[runtime-token]` log line is the single source of truth for
// operators investigating the 5-minute cycle in REN-1481.
func RefreshRuntimeToken(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) (*RefreshTokenResult, error) {
	logger := slog.Default()

	logger.Info("[runtime-token]",
		"event", "401",
		"workerId", currentWorkerID,
		"reason", reason,
	)

	// 1. Probe the refresh endpoint.
	if currentWorkerID != "" && looksLikeRegistrationToken(regOpts.RegistrationToken) {
		fresh, err := callRefreshEndpoint(ctx, regOpts, currentWorkerID)
		if err == nil {
			logger.Info("[runtime-token]",
				"event", "refresh",
				"workerId", currentWorkerID,
				"reason", reason,
			)
			return &RefreshTokenResult{
				Mode:         "refresh",
				WorkerID:     currentWorkerID,
				RuntimeToken: fresh.RuntimeToken,
				Reason:       reason,
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
			"detail", "platform refresh endpoint not deployed; falling back to full re-register (workerId will change — REN-1481 platform-side companion fix)",
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
		RegistrationTokenSwapped: swapped,
		Reason:                   reason,
	}, nil
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
// REN-1481-companion the platform 404s and we fall through to
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
