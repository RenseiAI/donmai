package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func claimManagerLease(t *testing.T, manager *worktree.Manager, lease *workarea.TerminalLease, invocationID, claimID string) {
	t.Helper()
	if _, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		InvocationID:     invocationID,
		ClaimID:          claimID,
	}); err != nil {
		t.Fatalf("ClaimTerminalLeaseExecution: %v", err)
	}
}

func managerAcknowledgement(lease *workarea.TerminalLease, invocationID, claimID string) workarea.TerminalResultAcknowledgement {
	return workarea.TerminalResultAcknowledgement{
		SchemaVersion:    workarea.TerminalLeaseAcknowledgementSchemaV1,
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		InvocationID:     invocationID,
		ClaimID:          claimID,
		Acknowledged:     true,
	}
}

func TestAcquireTerminalLeaseSerializesWithTeardown(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		dir := t.TempDir()
		runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
			return nil, os.MkdirAll(args[len(args)-1], 0o750)
		})
		manager, err := worktree.NewManager(worktree.Options{
			ParentDir:     dir,
			CommandRunner: runner.run,
		})
		if err != nil {
			t.Fatal(err)
		}
		path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
			SessionID: "session",
			RepoURL:   "repo",
			Strategy:  worktree.StrategyClone,
		})
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
				SessionID:        "session",
				TerminalResultID: "result",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			teardownErr = manager.Teardown(context.Background(), "session")
		}()
		close(start)
		wg.Wait()

		if teardownErr != nil {
			t.Fatalf("iteration %d Teardown: %v", i, teardownErr)
		}
		if acquireErr == nil {
			if lease == nil {
				t.Fatalf("iteration %d Acquire returned nil lease", i)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("iteration %d active lease points at deleted path: %v", i, statErr)
			}
			continue
		}
		if !errors.Is(acquireErr, worktree.ErrUnknownSession) {
			t.Fatalf("iteration %d Acquire error = %v", i, acquireErr)
		}
	}
}

func TestTwoVerifierClaimsCannotDeleteSharedLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
		return nil, os.MkdirAll(args[len(args)-1], 0o750)
	})
	manager, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "session",
		RepoURL:   "repo",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "session",
		TerminalResultID: "result",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Teardown(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		invocationID string
		claimID      string
		err          error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for _, identity := range [][2]string{{"invocation-a", "claim-a"}, {"invocation-b", "claim-b"}} {
		identity := identity
		go func() {
			<-start
			_, claimErr := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
				LeaseID:          lease.LeaseID,
				SessionID:        lease.SessionID,
				TerminalResultID: lease.TerminalResultID,
				WorkareaID:       lease.WorkareaID,
				InvocationID:     identity[0],
				ClaimID:          identity[1],
			})
			results <- claimResult{invocationID: identity[0], claimID: identity[1], err: claimErr}
		}()
	}
	close(start)
	var winner, loser claimResult
	for i := 0; i < 2; i++ {
		result := <-results
		switch {
		case result.err == nil:
			winner = result
		case errors.Is(result.err, workarea.ErrLeaseExecutionClaimed):
			loser = result
		default:
			t.Fatalf("ClaimTerminalLeaseExecution: %v", result.err)
		}
	}
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, loser.invocationID, loser.claimID)); !errors.Is(err, workarea.ErrLeaseExecutionConflict) {
		t.Fatalf("losing acknowledgement error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("losing verifier deleted shared leaf: %v", err)
	}
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, winner.invocationID, winner.claimID)); err != nil {
		t.Fatalf("winning acknowledgement: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("winning acknowledgement retained leaf: %v", err)
	}
}

func TestExpiredTerminalLeaseReaperRecoversAndRemovesDeferredLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	current := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
		return nil, os.MkdirAll(args[len(args)-1], 0o750)
	})
	first, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		Now:           func() time.Time { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := first.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "session",
		RepoURL:   "repo",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := first.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "session",
		TerminalResultID: "result",
		Policy: workarea.LeasePolicy{
			SettlementBudget: time.Second,
			SafetyMargin:     time.Second,
			LeaseDuration:    3 * time.Second,
			MaxLeaseDuration: 4 * time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Teardown(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("deferred leaf missing before expiry: %v", err)
	}

	current = current.Add(5 * time.Second)
	recovered, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		Now:           func() time.Time { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReapExpiredTerminalLeases(context.Background(), 1, time.Second)
	if err != nil {
		t.Fatalf("ReapExpiredTerminalLeases: %v", err)
	}
	if considered != 1 {
		t.Fatalf("considered = %d, want 1", considered)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired leaf still exists: %v", err)
	}
	persisted, err := recovered.TerminalLease(lease.LeaseID)
	if err != nil || persisted.State != workarea.LeaseReleased {
		t.Fatalf("persisted lease = %+v, err = %v", persisted, err)
	}
}

func TestWorktreeAddReleasePendingRecoveryAcceptsAlreadyRemovedLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	leaseDir := filepath.Join(dir, "leases")
	leaseStore, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	registered := true
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[2] == "worktree" && len(args) >= 4 && args[3] == "add" {
			return nil, os.MkdirAll(args[len(args)-1], 0o750)
		}
		if len(args) >= 4 && args[2] == "worktree" && args[3] == "remove" {
			if !registered {
				return []byte("fatal: path is not a working tree"), errors.New("exit status 128")
			}
			registered = false
			return nil, nil
		}
		if len(args) >= 4 && args[2] == "worktree" && args[3] == "list" {
			return []byte("worktree " + filepath.Join(dir, "parent") + "\n"), nil
		}
		return nil, nil
	})
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		LeaseStore:    leaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID:      "session",
		Strategy:       worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "session",
		TerminalResultID: "result",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Teardown(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	claimManagerLease(t, manager, lease, "invocation-1", "claim-1")

	records := filepath.Join(leaseDir, "records")
	heldRecords := filepath.Join(leaseDir, "records-held")
	_, err = leaseStore.Acknowledge(context.Background(), managerAcknowledgement(lease, "invocation-1", "claim-1"), func(context.Context, workarea.TerminalLease) error {
		registered = false
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return removeErr
		}
		if renameErr := os.Rename(records, heldRecords); renameErr != nil {
			return renameErr
		}
		return os.WriteFile(records, []byte("block final durable save"), 0o600)
	})
	if err == nil {
		t.Fatal("simulated crash window unexpectedly persisted released state")
	}
	if removeErr := os.Remove(records); removeErr != nil {
		t.Fatal(removeErr)
	}
	if renameErr := os.Rename(heldRecords, records); renameErr != nil {
		t.Fatal(renameErr)
	}
	pending, err := leaseStore.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != workarea.LeaseReleasePending {
		t.Fatalf("durable crash-window state = %s", pending.State)
	}

	recovered, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		LeaseStore:    leaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	considered, err := recovered.ReapExpiredTerminalLeases(context.Background(), 1, time.Second)
	if err != nil || considered != 1 {
		t.Fatalf("ReapExpiredTerminalLeases considered=%d err=%v", considered, err)
	}
	released, err := leaseStore.Get(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != workarea.LeaseReleased {
		t.Fatalf("already-removed worktree remained %s: %s", released.State, released.LastReleaseError)
	}
}

func TestLeaseReleaseKeepsPendingStateWhenGitWorktreeRemoveFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	leaseStore, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(dir, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	runner := newStubRunner(
		func(_ string, args ...string) ([]byte, error) {
			return nil, os.MkdirAll(args[len(args)-1], 0o750)
		},
		func(_ string, args ...string) ([]byte, error) {
			for _, arg := range args {
				if arg == "remove" {
					return []byte("worktree is locked"), errors.New("remove failed")
				}
			}
			return nil, nil
		},
	)
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		LeaseStore:    leaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID:      "session",
		Strategy:       worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "session",
		TerminalResultID: "result",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Teardown(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	claimManagerLease(t, manager, lease, "invocation-1", "claim-1")

	_, err = manager.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, "invocation-1", "claim-1"))
	if err == nil {
		t.Fatal("AcknowledgeTerminalResult succeeded after git worktree remove failure")
	}
	persisted, getErr := leaseStore.Get(lease.LeaseID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persisted.State != workarea.LeaseReleasePending || persisted.LastReleaseError == "" {
		t.Fatalf("failed release was not retained: %+v", persisted)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("failed release removed leased path: %v", statErr)
	}
}
