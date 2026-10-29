package runner

import (
	"context"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// TestRecordWriterQuiescenceReachesDurableLease pins the runner's
// production-call half of the writer-quiescence boundary: after the
// run loop returns, the runner records the ordinary-writer outcome on
// the manager, and the lease acquisition snapshots it into the durable
// lease record — so a daemon reaper or a post-restart manager that
// never supervised the terminal generation still carries the evidence
// the embedder proved before settlement. Emptying the recorder (the
// B2 mutant: no production caller) leaves the lease metadata without
// the snapshot and this test goes RED.
func TestRecordWriterQuiescenceReachesDurableLease(t *testing.T) {
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "55555555-5555-4555-8555-555555555555"
	if _, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, Strategy: worktree.StrategyEmpty,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{wt: manager}
	r.recordWriterQuiescence(sessionID)
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_55555555555555555555555555555555",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ReleaseMetadata["writerQuiesced"] != "true" {
		t.Fatalf("durable lease writerQuiesced = %q, want %q", lease.ReleaseMetadata["writerQuiesced"], "true")
	}
	// The snapshot must survive a process restart: a fresh manager
	// over the same parent directory adopts the durable lease with
	// the evidence intact, so a daemon reaper that never supervised
	// the terminal generation still releases as quiesced.
	restarted, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := restarted.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatalf("restart did not adopt the settled lease: %v", err)
	}
	if adopted.ReleaseMetadata["writerQuiesced"] != "true" {
		t.Fatalf("adopted lease writerQuiesced = %q, want %q", adopted.ReleaseMetadata["writerQuiesced"], "true")
	}
	// Nil-manager and empty-session recorders must be safe no-ops:
	// unit-test runners without a manager stay unquiesced evidence,
	// never a panic or a refusal.
	(&Runner{}).recordWriterQuiescence(sessionID)
	r.recordWriterQuiescence("")
}
