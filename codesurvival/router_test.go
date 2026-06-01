package codesurvival

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/worker"
)

func TestBatchHandler_UnknownWorkType_Skipped(t *testing.T) {
	exec := NewExecutor(Options{GitRunner: newStubGit(), Logger: discardLogger()})
	h := BatchHandler(exec)

	item := worker.BatchWorkItem{
		BatchJobID: "batch:other:1",
		WorkType:   "some-future-work-type",
		Raw:        json.RawMessage(`{"batchJobId":"batch:other:1","workType":"some-future-work-type"}`),
	}
	// Unknown work-type must be skipped (nil error), not crash.
	if err := h(context.Background(), item); err != nil {
		t.Errorf("unknown work-type should skip with nil error, got %v", err)
	}
}

func TestBatchHandler_RoutesCodeSurvival(t *testing.T) {
	// A code-survival item with a malformed raw payload exercises the decode
	// branch — it should attempt decode and surface the error, proving the
	// router dispatched to the survival executor (not silently skipped).
	exec := NewExecutor(Options{GitRunner: newStubGit(), Logger: discardLogger()})
	h := BatchHandler(exec)

	item := worker.BatchWorkItem{
		BatchJobID: "batch:due_checkpoint:1",
		WorkType:   WorkTypeCodeSurvivalScan,
		Raw:        json.RawMessage(`{not valid json`),
	}
	if err := h(context.Background(), item); err == nil {
		t.Error("expected decode error for malformed code-survival payload")
	}
}
