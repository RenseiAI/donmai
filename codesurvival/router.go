package codesurvival

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/RenseiAI/donmai/worker"
)

// BatchHandler adapts an Executor to worker.BatchHandler. It is the router that
// the worker poll loop hands every batchWork[] item to. It routes by WorkType
// to the matching executor, decoding the item-specific payload.
//
// CRITICAL ISOLATION: this router lives entirely outside the runner/agent path.
// It NEVER imports or calls runner.Run / AgentRuntimeProvider. An unknown
// work-type is logged + skipped (graceful degradation per the isolation
// contract), returning nil so the poll loop continues.
//
// Wire as:
//
//	exec := codesurvival.NewExecutor(codesurvival.Options{...})
//	c.PollLoopWithBatch(ctx, interval, agentHandler, codesurvival.BatchHandler(exec))
func BatchHandler(exec *Executor) worker.BatchHandler {
	return func(ctx context.Context, item worker.BatchWorkItem) error {
		switch item.WorkType {
		case WorkTypeCodeSurvivalScan:
			var csItem BatchWorkItem
			if err := json.Unmarshal(item.Raw, &csItem); err != nil {
				return fmt.Errorf("codesurvival: decode batch item %q: %w", item.BatchJobID, err)
			}
			return exec.Handle(ctx, csItem)
		default:
			// Graceful degradation: a stale worker may receive a work-type a
			// newer platform dispatched. Log + skip; never crash the loop.
			slog.Default().Info("codesurvival: unknown batch work-type; skipping",
				"batchJobId", item.BatchJobID, "workType", item.WorkType)
			return nil
		}
	}
}
