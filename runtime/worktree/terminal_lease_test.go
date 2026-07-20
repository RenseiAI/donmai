package worktree_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const (
	leaseTestSessionID    = "11111111-1111-4111-8111-111111111111"
	leaseTestSessionIDB   = "22222222-2222-4222-8222-222222222222"
	leaseTestResultID     = "tr_11111111111111111111111111111111"
	leaseTestResultIDB    = "tr_22222222222222222222222222222222"
	leaseTestInvocationID = "33333333-3333-4333-8333-333333333333"
	leaseTestInvocationB  = "44444444-4444-4444-8444-444444444444"
	leaseTestClaimID      = "55555555-5555-4555-8555-555555555555"
	leaseTestClaimIDB     = "66666666-6666-4666-8666-666666666666"
)

func claimManagerLease(t *testing.T, manager *worktree.Manager, lease *workarea.TerminalLease, invocationID, claimID string) *workarea.ExecutionClaimResult {
	t.Helper()
	claim, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: invocationID, ClaimID: claimID,
	})
	if err != nil {
		t.Fatalf("ClaimTerminalLeaseExecution: %v", err)
	}
	return claim
}

func managerAcknowledgement(lease *workarea.TerminalLease, invocationID, claimID string) workarea.TerminalResultAcknowledgement {
	return workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: invocationID, ClaimID: claimID,
	}
}

func acquireManagerLease(t *testing.T, manager *worktree.Manager, sessionID, resultID string) (*workarea.TerminalLease, string) {
	t.Helper()
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: "repo", Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: resultID, Policy: workarea.DefaultLeasePolicy(),
		ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease, path
}

func TestAcquireTerminalLeaseSerializesWithTeardown(t *testing.T) {
	t.Parallel()
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
			return nil, os.MkdirAll(args[len(args)-1], 0o750)
		})
		manager, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
		if err != nil {
			t.Fatal(err)
		}
		path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{SessionID: leaseTestSessionID, RepoURL: "repo", Strategy: worktree.StrategyClone})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var lease *workarea.TerminalLease
		var acquireErr, teardownErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			lease, acquireErr = manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
				SessionID: leaseTestSessionID, TerminalResultID: leaseTestResultID,
				Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "destroy",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			teardownErr = manager.Teardown(context.Background(), leaseTestSessionID)
		}()
		close(start)
		wg.Wait()
		if teardownErr != nil {
			t.Fatal(teardownErr)
		}
		if acquireErr == nil {
			if lease == nil {
				t.Fatal("nil lease")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("leased path was removed: %v", err)
			}
		} else if !errors.Is(acquireErr, worktree.ErrUnknownSession) {
			t.Fatalf("acquire error=%v", acquireErr)
		}
	}
}

func TestTwoVerifierClaimsCannotDeleteSameLeaf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) { return nil, os.MkdirAll(args[len(args)-1], 0o750) })
	manager, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	lease, path := acquireManagerLease(t, manager, leaseTestSessionID, leaseTestResultID)
	if err := manager.Teardown(context.Background(), leaseTestSessionID); err != nil {
		t.Fatal(err)
	}
	winner := claimManagerLease(t, manager, lease, leaseTestInvocationID, leaseTestClaimID)
	if _, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: leaseTestInvocationB, ClaimID: leaseTestClaimIDB,
	}); !errors.Is(err, workarea.ErrLeaseExecutionClaimed) {
		t.Fatalf("competing claim error=%v", err)
	}
	mismatch, err := manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, leaseTestInvocationB, leaseTestClaimIDB))
	if err != nil || mismatch.Outcome != workarea.AcknowledgementRejected {
		t.Fatalf("mismatch outcome=%+v err=%v", mismatch, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mismatch removed path: %v", err)
	}
	if winner.ClaimNowMS != winner.Claim.ClaimedAt.UnixMilli() {
		t.Fatalf("claimNowMs mismatch: %+v", winner)
	}
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, leaseTestInvocationID, leaseTestClaimID)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("released path still exists: %v", err)
	}
}

func TestExpiredTerminalLeaseRecoversAfterManagerRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	current := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) { return nil, os.MkdirAll(args[len(args)-1], 0o750) })
	first, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	lease, path := acquireManagerLease(t, first, leaseTestSessionID, leaseTestResultID)
	if err := first.Teardown(context.Background(), leaseTestSessionID); err != nil {
		t.Fatal(err)
	}
	current = current.Add(workarea.DefaultLeaseDuration + time.Millisecond)
	recovered, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReapExpiredTerminalLeases(context.Background(), 1, time.Second)
	if err != nil || considered != 1 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired path still exists: %v", err)
	}
	persisted, err := recovered.TerminalLease(lease.LeaseID)
	if err != nil || persisted.State != workarea.LeaseReleased {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestReacquiringDestroyedPathGetsNewWorkareaIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) { return nil, os.MkdirAll(args[len(args)-1], 0o750) })
	manager, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := acquireManagerLease(t, manager, leaseTestSessionID, leaseTestResultID)
	claimManagerLease(t, manager, first, leaseTestInvocationID, leaseTestClaimID)
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(first, leaseTestInvocationID, leaseTestClaimID)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	second, _ := acquireManagerLease(t, manager, leaseTestSessionIDB, leaseTestResultIDB)
	if first.WorkareaID == second.WorkareaID {
		t.Fatalf("workarea identity reused: %s", first.WorkareaID)
	}
}
