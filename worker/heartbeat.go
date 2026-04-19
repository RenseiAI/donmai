package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Heartbeat reports the worker's liveness + active agent count to the
// coordinator. It issues POST /api/workers/{WorkerID}/heartbeat with the
// runtime JWT in the Authorization header.
//
// A 401 response maps to ErrRuntimeJWTExpired — callers should re-register
// to obtain a fresh token. 429 maps to ErrRateLimited, and any 5xx to
// ErrHeartbeatFailed.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	if c.WorkerID == "" {
		return nil, fmt.Errorf("heartbeat: worker not registered")
	}

	path := "/api/workers/" + c.WorkerID + "/heartbeat"

	var resp HeartbeatResponse
	if err := c.postWithRuntime(ctx, path, req, &resp); err != nil {
		// 5xx responses surface as ErrServerError from the shared status
		// mapping; remap to the package-level ErrHeartbeatFailed so
		// callers have a single sentinel for "heartbeat did not succeed
		// for a server-side reason".
		if errors.Is(err, ErrServerError) {
			return nil, fmt.Errorf("heartbeat: %w", ErrHeartbeatFailed)
		}
		return nil, fmt.Errorf("heartbeat: %w", err)
	}

	slog.Default().Debug("heartbeat", "worker_id", c.WorkerID, "active", req.ActiveAgentCount)

	return &resp, nil
}

// HeartbeatLoop sends a heartbeat on the given interval using counter() to
// determine ActiveAgentCount for each tick. It blocks until ctx is cancelled
// or the runtime JWT expires.
//
// An ErrRuntimeJWTExpired response is returned to the caller so a fleet
// manager can re-register. Other heartbeat errors are logged at warn and
// the loop continues.
func (c *Client) HeartbeatLoop(ctx context.Context, interval time.Duration, counter func() int) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			active := 0
			if counter != nil {
				active = counter()
			}
			req := HeartbeatRequest{ActiveAgentCount: active}
			if _, err := c.Heartbeat(ctx, req); err != nil {
				if errors.Is(err, ErrRuntimeJWTExpired) {
					return err
				}
				slog.Default().Warn("heartbeat failed", "worker_id", c.WorkerID, "err", err)
			}
		}
	}
}
