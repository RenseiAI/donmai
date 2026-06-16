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

// BatchHandler processes a single non-agent batchWork[] item. It is invoked on
// a SEPARATE code path from the agent WorkItem handler — a batch item NEVER
// reaches the agent handler and NEVER enters runner.Run / AgentRuntimeProvider.
// Implementations are best-effort: a returned error is logged and the loop
// continues (a batch failure must never stop polling or perturb agent dispatch).
type BatchHandler func(ctx context.Context, item BatchWorkItem) error

// PollLoop drives Poll on the given interval, invoking handler for each agent
// WorkItem returned. It blocks until ctx is cancelled or an unrecoverable error
// occurs.
//
// Batch work (resp.BatchWork) is NOT dispatched here — callers that handle the
// batch lane must use PollLoopWithBatch. PollLoop preserves the legacy
// agent-only behaviour byte-for-byte for callers that pass no batch handler.
//
// A handler error is logged at warn and does not stop the loop. An
// ErrRuntimeJWTExpired from Poll is returned to the caller (so a fleet
// manager can re-register). Other Poll errors are logged at warn and the
// loop continues.
func (c *Client) PollLoop(ctx context.Context, interval time.Duration, handler func(WorkItem) error) error {
	return c.PollLoopWithBatch(ctx, interval, handler, nil)
}

// PollLoopWithBatch is PollLoop plus a SEPARATE batch lane. Each poll, agent
// WorkItems flow through agentHandler exactly as before; batchWork items flow
// through batchHandler on a distinct code path. The two lanes are isolated: an
// item present in BatchWork is never handed to agentHandler, and vice versa.
//
// When batchHandler is nil, batch items are logged + skipped (graceful
// degradation for a worker not built to run batch work). When agentHandler is
// nil, agent items are skipped (a batch-only worker); at least one should be set.
func (c *Client) PollLoopWithBatch(
	ctx context.Context,
	interval time.Duration,
	agentHandler func(WorkItem) error,
	batchHandler BatchHandler,
) error {
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
			// AGENT LANE — unchanged. Batch items are NOT in this slice.
			for _, item := range resp.WorkItems {
				if agentHandler == nil {
					continue
				}
				if herr := agentHandler(item); herr != nil {
					slog.Default().Warn("poll handler error", "worker_id", c.WorkerID, "item_id", item.ID, "err", herr)
				}
			}
			// BATCH LANE — separate from the agent path. Routed here BEFORE the
			// agent handler ever sees it (the lanes are distinct slices). A nil
			// batchHandler or unknown work-type degrades gracefully. The
			// code-survival batchWork[], kg-extraction kgExtractWork[], and FD-4
			// landing landingWork[] lanes all share the BatchWorkItem envelope and
			// flow through the SAME batchHandler — the handler is a workType mux
			// that fans each item out to its executor (see afcli/worker_start.go).
			// No lane ever touches resp.WorkItems.
			for _, item := range resp.BatchWork {
				c.dispatchBatchItem(ctx, item, batchHandler)
			}
			for _, item := range resp.KgExtractWork {
				c.dispatchBatchItem(ctx, item, batchHandler)
			}
			for _, item := range resp.LandingWork {
				c.dispatchBatchItem(ctx, item, batchHandler)
			}
		}
	}
}

// dispatchBatchItem routes a single batch item to batchHandler. A nil handler
// (worker not built for batch work) logs + skips; a handler error logs + skips.
// It NEVER returns an error and NEVER touches the agent path — the poll loop
// keeps running regardless.
func (c *Client) dispatchBatchItem(ctx context.Context, item BatchWorkItem, batchHandler BatchHandler) {
	if batchHandler == nil {
		slog.Default().Info("poll: batch work received but no batch handler configured; skipping",
			"worker_id", c.WorkerID, "batchJobId", item.BatchJobID, "workType", item.WorkType)
		return
	}
	if herr := batchHandler(ctx, item); herr != nil {
		slog.Default().Warn("poll batch handler error", "worker_id", c.WorkerID,
			"batchJobId", item.BatchJobID, "workType", item.WorkType, "err", herr)
	}
}
