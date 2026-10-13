package worktree_test

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestArchiveReleaseRecordsReferenceOnLease(t *testing.T) {
	const sessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	parent := t.TempDir()
	committedAt := time.Date(2026, 7, 18, 12, 30, 0, 123_000_000, time.UTC)
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: sessionID,
		ArchiveRoot: func(_ context.Context, spec worktree.ArchiveRootSpec) (*workarea.TerminalArchiveReference, error) {
			if spec.TerminalResultID != "tr_cccccccccccccccccccccccccccccccc" {
				t.Errorf("archive did not receive the terminal result binding: %+v", spec)
			}
			return &workarea.TerminalArchiveReference{
				SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: spec.WorkareaID,
				ArchiveDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SessionID:       spec.SessionID, WorkareaID: spec.WorkareaID,
				TerminalResultID: spec.TerminalResultID, CommittedAt: committedAt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), nestedSpec(sessionID, nil)); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_cccccccccccccccccccccccccccccccc",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ClaimID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: claim.Claim.InvocationID, ClaimID: claim.Claim.ClaimID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	released, err := manager.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	ref := released.ArchiveReference
	if ref == nil {
		t.Fatal("released lease carries no committed archive reference")
	}
	if ref.ArchiveID != lease.WorkareaID || ref.SessionID != sessionID || ref.WorkareaID != lease.WorkareaID ||
		ref.TerminalResultID != "tr_cccccccccccccccccccccccccccccccc" ||
		ref.ArchiveDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("released reference = %+v", ref)
	}
	// The reference survives a manager restart: the lease record is the
	// durable lifecycle-state binding.
	recovered, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := recovered.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ArchiveReference == nil || !replayed.ArchiveReference.IdentityEqual(*ref) {
		t.Fatalf("replayed reference = %+v, want %+v", replayed.ArchiveReference, ref)
	}
}

func TestArchiveReleaseWithoutReferenceStillReleases(t *testing.T) {
	const sessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: sessionID,
		ArchiveRoot: func(_ context.Context, spec worktree.ArchiveRootSpec) (*workarea.TerminalArchiveReference, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), nestedSpec(sessionID, nil)); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_cccccccccccccccccccccccccccccccc",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	released, err := manager.TerminalLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if released.ArchiveReference != nil {
		t.Fatalf("legacy provider must not invent a reference: %+v", released.ArchiveReference)
	}
}
