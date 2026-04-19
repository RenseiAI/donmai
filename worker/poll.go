package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Poll fetches the next batch of work items assigned to this worker.
//
// It issues GET /api/workers/{WorkerID}/poll with the runtime JWT in the
// Authorization header. An empty WorkItems slice is NOT an error; it simply
// means the coordinator has no pending work.
//
// A 401 response maps to ErrRuntimeJWTExpired — callers should re-register
// to obtain a fresh token. 404 maps to ErrNotFound (unknown worker), 429 to
// ErrRateLimited, and any 5xx to ErrPollFailed.
func (c *Client) Poll(ctx context.Context) (*PollResponse, error) {
	if c.WorkerID == "" {
		return nil, fmt.Errorf("poll: worker not registered")
	}

	path := "/api/workers/" + c.WorkerID + "/poll"

	var resp PollResponse
	if err := c.getWithRuntime(ctx, path, &resp); err != nil {
		// 5xx responses surface as ErrServerError from the shared status
		// mapping; remap to the package-level ErrPollFailed so callers
		// have a single sentinel for "poll did not succeed for a
		// server-side reason".
		if errors.Is(err, ErrServerError) {
			return nil, fmt.Errorf("poll: %w", ErrPollFailed)
		}
		return nil, fmt.Errorf("poll: %w", err)
	}

	slog.Default().Debug("poll", "worker_id", c.WorkerID, "items", len(resp.WorkItems))

	return &resp, nil
}

// PollLoop drives Poll on the given interval, invoking handler for each
// WorkItem returned. It blocks until ctx is cancelled or an unrecoverable
// error occurs.
//
// A handler error is logged at warn and does not stop the loop. An
// ErrRuntimeJWTExpired from Poll is returned to the caller (so a fleet
// manager can re-register). Other Poll errors are logged at warn and the
// loop continues.
func (c *Client) PollLoop(ctx context.Context, interval time.Duration, handler func(WorkItem) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resp, err := c.Poll(ctx)
			if err != nil {
				// Surface an expired runtime JWT so a fleet manager
				// can re-register; other errors are transient and
				// should not stop the loop.
				if errors.Is(err, ErrRuntimeJWTExpired) {
					return err
				}
				slog.Default().Warn("poll error", "worker_id", c.WorkerID, "err", err)
				continue
			}
			for _, item := range resp.WorkItems {
				if herr := handler(item); herr != nil {
					slog.Default().Warn("poll handler error", "worker_id", c.WorkerID, "item_id", item.ID, "err", herr)
				}
			}
		}
	}
}
