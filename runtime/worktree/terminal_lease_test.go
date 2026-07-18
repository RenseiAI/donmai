package worktree_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

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

	_, err = manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		LeaseID:          lease.LeaseID,
		SessionID:        lease.SessionID,
		TerminalResultID: lease.TerminalResultID,
		WorkareaID:       lease.WorkareaID,
		Acknowledged:     true,
	})
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
