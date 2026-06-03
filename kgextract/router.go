package kgextract

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/RenseiAI/donmai/worker"
)

// BatchHandler adapts an Executor to worker.BatchHandler. It is the router that
// the worker poll loop hands every kgExtractWork[] item to. It routes by
// WorkType to the executor, decoding the item-specific payload.
//
// CRITICAL ISOLATION: this router lives entirely outside the runner/agent path.
// It NEVER imports or calls runner.Run / AgentRuntimeProvider. An unknown
// work-type is logged + skipped (graceful degradation per the isolation
// contract), returning nil so the poll loop continues.
//
// Wire as (alongside codesurvival via a workType mux — see
// afcli/worker_start.go):
//
//	exec := kgextract.NewExecutor(kgextract.Options{...})
//	handler := kgextract.BatchHandler(exec)
func BatchHandler(exec *Executor) worker.BatchHandler {
	return func(ctx context.Context, item worker.BatchWorkItem) error {
		switch item.WorkType {
		case WorkTypeKGExtraction:
			var kgItem KgExtractWorkItem
			if err := json.Unmarshal(item.Raw, &kgItem); err != nil {
				return fmt.Errorf("kgextract: decode batch item %q: %w", item.BatchJobID, err)
			}
			return exec.Handle(ctx, kgItem)
		default:
			// Graceful degradation: a stale worker may receive a work-type a
			// newer platform dispatched. Log + skip; never crash the loop.
			slog.Default().Info("kgextract: unknown batch work-type; skipping",
				"batchJobId", item.BatchJobID, "workType", item.WorkType)
			return nil
		}
	}
}
