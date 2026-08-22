package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRunLegacyWorkItemRemainsFlatWithoutSessionRootNegotiation is the V16
// control for the protocol activation boundary. The current work-item wire has
// no versioned repository declaration and no exact-executor session-root-v1
// attestation, so the runner must retain the representable legacy layout rather
// than activating a protocol the producer never negotiated.
func TestRunLegacyWorkItemRemainsFlatWithoutSessionRootNegotiation(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("LEGACY-WORKAREA")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if got := filepath.Base(res.WorktreePath); got != qw.SessionID {
		t.Errorf("legacy worktree leaf = %q; want session id %q until session-root-v1 is negotiated", got, qw.SessionID)
	}
}
