package worktree_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// stubRunner returns a CommandRunner that records invocations and
// returns canned outputs in order. After the canned list is exhausted
// the runner returns success with empty output.
type stubRunner struct {
	calls atomic.Int64
	mu    chan struct{}
	plan  []func(name string, args ...string) ([]byte, error)
}

func newStubRunner(plan ...func(name string, args ...string) ([]byte, error)) *stubRunner {
	return &stubRunner{plan: plan, mu: make(chan struct{}, 1)}
}

func (s *stubRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	idx := s.calls.Add(1) - 1
	if int(idx) < len(s.plan) {
		return s.plan[idx](name, args...)
	}
	return nil, nil
}

func TestNewManagerRejectsEmptyParent(t *testing.T) {
	t.Parallel()
	if _, err := worktree.NewManager(worktree.Options{}); !errors.Is(err, worktree.ErrNoParentDir) {
		t.Fatalf("expected ErrNoParentDir, got %v", err)
	}
}

func TestProvisionCloneSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(
		func(name string, args ...string) ([]byte, error) {
			if name != "git" || args[0] != "clone" {
				t.Errorf("unexpected call: %s %v", name, args)
			}
			// Materialize the destination dir so isRetriable does not
			// see "already exists" on subsequent calls.
			dst := args[len(args)-1]
			_ = os.MkdirAll(dst, 0o750)
			return []byte(""), nil
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "git@example.com:org/repo.git",
		Branch:    "main",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.HasSuffix(path, "/s1") {
		t.Fatalf("expected path to end in /s1, got %q", path)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("expected 1 git call, got %d", got)
	}
	if p, err := m.Path("s1"); err != nil || p != path {
		t.Fatalf("Path mismatch: %q %v", p, err)
	}
}

func TestProvisionEmptySuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner()
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "repository-free",
		Strategy:  worktree.StrategyEmpty,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if path != filepath.Join(dir, "repository-free") {
		t.Fatalf("path = %q, want %q", path, filepath.Join(dir, "repository-free"))
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read empty workarea: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty workarea contains %d entries", len(entries))
	}
	if got := runner.calls.Load(); got != 0 {
		t.Fatalf("empty workarea invoked git %d time(s)", got)
	}
	if got, err := m.Path("repository-free"); err != nil || got != path {
		t.Fatalf("Path = %q, %v; want %q", got, err, path)
	}
	if err := m.Teardown(context.Background(), "repository-free"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty workarea remains after teardown: %v", err)
	}
}

func TestProvisionEmptyRejectsRepositoryProvenance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec worktree.ProvisionSpec
		want string
	}{
		{name: "repository", spec: worktree.ProvisionSpec{RepoURL: "https://example.test/repository.git"}, want: "RepoURL must be empty"},
		{name: "parent repository", spec: worktree.ProvisionSpec{ParentRepoPath: "/tmp/parent"}, want: "git parent, reference, and sparse paths"},
		{name: "branch", spec: worktree.ProvisionSpec{Branch: "main"}, want: "repository provenance"},
		{name: "source ref", spec: worktree.ProvisionSpec{SourceRef: "main"}, want: "repository provenance"},
		{name: "cache seed", spec: worktree.ProvisionSpec{CacheSeedID: "seed-one"}, want: "repository provenance"},
		{name: "sparse paths", spec: worktree.ProvisionSpec{SparsePaths: []string{"src"}}, want: "git parent, reference, and sparse paths"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			test.spec.SessionID = "not-empty"
			test.spec.Strategy = worktree.StrategyEmpty
			_, err = m.Provision(context.Background(), test.spec)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Provision error = %v, want %q refusal", err, test.want)
			}
		})
	}
}

func TestProvisionRetryThenSucceed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var attempts atomic.Int64
	runner := newStubRunner(
		func(_ string, _ ...string) ([]byte, error) {
			attempts.Add(1)
			return []byte("fatal: 'main' is already checked out at /tmp/other"), exec.Command("false").Run()
		},
		// StrategyClone has no parent repo → cleanupConflict makes
		// no runner call; the next entry is the retry attempt.
		func(_ string, args ...string) ([]byte, error) {
			attempts.Add(1)
			dst := args[len(args)-1]
			_ = os.MkdirAll(dst, 0o750)
			return nil, nil
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		RetryDelay:    1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "git@example.com:org/repo.git",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.HasSuffix(path, "/s1") {
		t.Fatalf("expected path to end in /s1, got %q", path)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 git clone attempts, got %d", got)
	}
}

func TestProvisionLostOwnership(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(
		func(_ string, _ ...string) ([]byte, error) {
			return []byte("fatal: branch already checked out"), exec.Command("false").Run()
		},
	)
	var probeCalls atomic.Int64
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		RetryDelay:    1 * time.Millisecond,
		OwnershipProber: func(_ context.Context, _ string) (bool, error) {
			probeCalls.Add(1)
			return false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "git@example.com:org/repo.git",
		Strategy:  worktree.StrategyClone,
	})
	if !errors.Is(err, worktree.ErrLostOwnership) {
		t.Fatalf("expected ErrLostOwnership, got %v", err)
	}
	if probeCalls.Load() == 0 {
		t.Fatal("ownership prober was not called between retries")
	}
}

func TestProvisionNonRetriableFailsFast(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(
		func(_ string, _ ...string) ([]byte, error) {
			return []byte("fatal: repository not found"), exec.Command("false").Run()
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		RetryDelay:    1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "git@example.com:org/repo.git",
		Strategy:  worktree.StrategyClone,
	})
	if err == nil || errors.Is(err, worktree.ErrLostOwnership) {
		t.Fatalf("expected non-retriable failure, got %v", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", got)
	}
}

func TestProvisionExhaustsRetries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner()
	for i := 0; i < 6; i++ {
		runner.plan = append(
			runner.plan,
			func(_ string, _ ...string) ([]byte, error) {
				return []byte("already checked out"), exec.Command("false").Run()
			},
		)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		RetryDelay:    1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "git@example.com:org/repo.git",
		Strategy:  worktree.StrategyClone,
	})
	if err == nil || errors.Is(err, worktree.ErrLostOwnership) {
		t.Fatalf("expected exhaustion error, got %v", err)
	}
	if !strings.Contains(err.Error(), "after") || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("expected attempt-count framing, got %v", err)
	}
}

func TestProvisionContextCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(
		func(_ string, _ ...string) ([]byte, error) {
			return []byte("already checked out"), exec.Command("false").Run()
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		RetryDelay:    100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestTeardownRemovesPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(
		func(_ string, args ...string) ([]byte, error) {
			dst := args[len(args)-1]
			return nil, os.MkdirAll(dst, 0o750)
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected path to exist: %v", err)
	}
	if err := m.Teardown(context.Background(), "s1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected path removed, stat err=%v", err)
	}
	// idempotent
	if err := m.Teardown(context.Background(), "s1"); err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
}

func TestTerminalLeaseRetainsLeafUntilAcknowledgement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	leaseStore, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: filepath.Join(dir, "leases")})
	if err != nil {
		t.Fatal(err)
	}
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
		dst := args[len(args)-1]
		return nil, os.MkdirAll(dst, 0o750)
	})
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		LeaseStore:    leaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: leaseTestSessionID,
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: leaseTestSessionID, TerminalResultID: leaseTestResultID,
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatalf("AcquireTerminalLease: %v", err)
	}
	persisted, err := leaseStore.Get(lease.LeaseID)
	if err != nil || persisted.State != workarea.LeaseActive {
		t.Fatalf("lease not durable before teardown: lease=%+v err=%v", persisted, err)
	}

	if err := m.Teardown(context.Background(), leaseTestSessionID); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("leased leaf was removed before acknowledgement: %v", err)
	}
	retained, err := leaseStore.Get(lease.LeaseID)
	if err != nil || !retained.ReleaseRequested || retained.State != workarea.LeaseActive {
		t.Fatalf("teardown did not retain active lease: lease=%+v err=%v", retained, err)
	}
	recovered, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
		LeaseStore:    leaseStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: leaseTestSessionID,
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	}); !errors.Is(err, workarea.ErrWorkareaLeased) {
		t.Fatalf("retained workarea was reusable after restart: %v", err)
	}

	claimManagerLease(t, m, lease, leaseTestInvocationID, leaseTestClaimID)
	ack := managerAcknowledgement(lease, leaseTestInvocationID, leaseTestClaimID)
	applied, ackErr := m.AcknowledgeTerminalResult(context.Background(), ack)
	if ackErr != nil || applied.Outcome != workarea.AcknowledgementApplied {
		t.Fatalf("AcknowledgeTerminalResult: outcome=%+v err=%v", applied, ackErr)
	}
	if _, err := m.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("release pending lease: %v", err)
	}
	duplicate, ackErr := m.AcknowledgeTerminalResult(context.Background(), ack)
	if ackErr != nil || duplicate.Outcome != workarea.AcknowledgementAlreadyApplied {
		t.Fatalf("duplicate acknowledgement: outcome=%+v err=%v", duplicate, ackErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged leaf still exists: %v", err)
	}
	if _, err := m.Path(leaseTestSessionID); !errors.Is(err, worktree.ErrUnknownSession) {
		t.Fatalf("released session remained owned: %v", err)
	}
}

func TestTerminalLeaseAcknowledgementRecoversAfterManagerRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newStubRunner(func(_ string, args ...string) ([]byte, error) {
		dst := args[len(args)-1]
		return nil, os.MkdirAll(dst, 0o750)
	})
	first, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	path, err := first.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: leaseTestSessionID,
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := first.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: leaseTestSessionID, TerminalResultID: leaseTestResultID,
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Teardown(context.Background(), leaseTestSessionID); err != nil {
		t.Fatal(err)
	}

	recovered, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	claimManagerLease(t, recovered, lease, leaseTestInvocationID, leaseTestClaimID)
	_, err = recovered.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, leaseTestInvocationID, leaseTestClaimID))
	if err != nil {
		t.Fatalf("recovered acknowledgement: %v", err)
	}
	if _, err := recovered.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("recovered release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("recovered release retained leaf: %v", err)
	}
}

func TestPathUnknownSession(t *testing.T) {
	t.Parallel()
	m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Path("nope"); !errors.Is(err, worktree.ErrUnknownSession) {
		t.Fatalf("expected ErrUnknownSession, got %v", err)
	}
}

func TestProvisionStrategyWorktreeAdd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var captured []string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "worktree" {
			captured = append([]string(nil), args...)
			dst := args[len(args)-2] // dst, then origin/branch tail
			_ = os.MkdirAll(dst, 0o750)
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID:      "s1",
		Branch:         "main",
		Strategy:       worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := []string{"-C", parent, "worktree", "add", "-B", "main"}
	for i := range want {
		if captured[i] != want[i] {
			t.Fatalf("git args mismatch:\n got: %v\nwant prefix: %v", captured, want)
		}
	}
}

func TestProvisionWorktreeRefreshesBaseAndRecordsReceipt(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[2] {
		case "fetch":
			return nil, nil
		case "rev-parse":
			return []byte("abc123\n"), nil
		case "worktree":
			_ = os.MkdirAll(args[len(args)-2], 0o750)
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "receipt", Branch: "main", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[0][2] != "fetch" || calls[1][2] != "worktree" || calls[2][2] != "rev-parse" {
		t.Fatalf("git calls = %#v, want fetch, worktree, rev-parse", calls)
	}
	result, err := m.Result("receipt")
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseRef != "main" || result.BaseSHA != "abc123" || !result.BaseFetched {
		t.Fatalf("base receipt = %#v", result)
	}
	if result.BaseFetchDuration > time.Second {
		t.Fatalf("base fetch duration = %s, want a bounded test duration", result.BaseFetchDuration)
	}
}

func TestProvisionFetchFailureDoesNotCreateBranch(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var calls int
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if args[2] == "fetch" {
			return []byte("network unavailable"), errors.New("fetch failed")
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "failed-fetch", Branch: "main", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
	})
	if !errors.Is(err, worktree.ErrBaseFetch) || calls != worktree.MaxSpawnRetries {
		t.Fatalf("error = %v, calls = %d; want typed fetch failure and no branch", err, calls)
	}
}

func TestProvisionAbsentBaseRefFailsWithoutRetry(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetchCalls atomic.Int64
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] == "fetch" {
			fetchCalls.Add(1)
			return []byte("fatal: couldn't find remote ref missing"), errors.New("fetch failed")
		}
		t.Fatal("worktree command ran after absent ref")
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "absent-ref", Branch: "missing", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
	})
	if !errors.Is(err, worktree.ErrInvalidBaseRef) {
		t.Fatalf("error = %v, want ErrInvalidBaseRef", err)
	}
	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}

func TestProvisionFetchRetryProbesOwnership(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetchCalls, probeCalls atomic.Int64
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "fetch":
			if fetchCalls.Add(1) == 1 {
				return []byte("network unavailable"), errors.New("fetch failed")
			}
		case "worktree":
			_ = os.MkdirAll(args[len(args)-2], 0o750)
		case "rev-parse":
			return []byte("abc123\n"), nil
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond,
		OwnershipProber: func(context.Context, string) (bool, error) {
			probeCalls.Add(1)
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "fetch-retry", Branch: "main", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := fetchCalls.Load(); got != 2 {
		t.Fatalf("fetch calls = %d, want 2", got)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("ownership probes = %d, want 1", got)
	}
}

func TestProvisionSkipBaseFetchPreservesOfflineBehaviour(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var calls int
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if args[2] == "worktree" {
			_ = os.MkdirAll(args[len(args)-2], 0o750)
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "offline", Branch: "main", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent, SkipBaseFetch: true,
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("git calls = %d, want only worktree add with fetch skipped", calls)
	}
	result, err := m.Result("offline")
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseFetched || result.BaseFetchDuration != 0 {
		t.Fatalf("skipped base receipt = %#v, want not fetched with zero duration", result)
	}
}

func TestProvisionBaseFetchTimeoutLeavesNoWorkarea(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	runner := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] == "fetch" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		t.Fatal("branch command ran after fetch timeout")
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, BaseFetchTimeout: time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "timeout", Branch: "main", Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
	})
	if !errors.Is(err, worktree.ErrBaseFetch) {
		t.Fatalf("error = %v, want typed base-fetch timeout", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "timeout")); !os.IsNotExist(statErr) {
		t.Fatalf("workarea exists after timeout: %v", statErr)
	}
}

func TestProvisionRejectsLeaseStateDirectoryAsLeaf(t *testing.T) {
	t.Parallel()
	m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
		LeafName:  ".terminal-leases",
	})
	if err == nil || !strings.Contains(err.Error(), "lease state directory") {
		t.Fatalf("Provision error = %v, want lease state directory conflict", err)
	}
}

func TestProvisionRequiresSessionID(t *testing.T) {
	t.Parallel()
	m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{})
	if err == nil {
		t.Fatal("expected error for missing SessionID")
	}
}

// envHas reports whether env contains an exact "KEY=VALUE" entry.
func envHas(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// configValueForKey resolves the GIT_CONFIG_VALUE_n paired with the
// GIT_CONFIG_KEY_n holding key. Returns ("", false) when key is absent.
func configValueForKey(env []string, key string) (string, bool) {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	// Highest-numbered occurrence wins, matching git: a key may repeat (an
	// inherited http.extraHeader plus our reset). Ranging over the map would
	// pick an arbitrary one and flake.
	var val string
	var found bool
	for i := 0; ; i++ {
		k, ok := m[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		if !ok {
			return val, found
		}
		if k != key {
			continue
		}
		if v, ok := m[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]; ok {
			val, found = v, true
		}
	}
}

// TestProvisionGitAuthEngaged asserts that when a GitAuth callback is set the
// clone runs through the env-aware runner with the hardened env (suppressed
// credential helper + injected http.extraHeader + GIT_TERMINAL_PROMPT=0) and
// that the cloned URL is the userinfo-stripped (clean) form.
func TestProvisionGitAuthEngaged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const dirtyURL = "https://x-access-token:ghp_secret@github.com/org/repo.git"
	const cleanURL = "https://github.com/org/repo.git"
	const header = "Authorization: Bearer ghp_secret"

	var (
		gotEnv     []string
		gotArgs    []string
		envRunHits int
		authURL    string
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: dir,
		// The plain CommandRunner must NOT be used on the engaged path.
		CommandRunner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			t.Error("plain CommandRunner called on GitAuth-engaged path")
			return nil, nil
		},
		EnvCommandRunner: func(_ context.Context, extraEnv []string, name string, args ...string) ([]byte, error) {
			envRunHits++
			gotEnv = append([]string(nil), extraEnv...)
			gotArgs = append([]string(nil), args...)
			if name != "git" {
				t.Errorf("unexpected binary %q", name)
			}
			dst := args[len(args)-1]
			_ = os.MkdirAll(dst, 0o750)
			return nil, nil
		},
		GitAuth: func(_ context.Context, repoURL string) (string, bool, error) {
			authURL = repoURL
			return header, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   dirtyURL,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if envRunHits != 1 {
		t.Fatalf("env runner hits = %d, want 1", envRunHits)
	}
	// GitAuth must be resolved against the ORIGINAL (dirty) URL.
	if authURL != dirtyURL {
		t.Errorf("GitAuth repoURL = %q, want original dirty URL %q", authURL, dirtyURL)
	}
	// Hardened env assertions.
	if !envHas(gotEnv, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("hardened env missing GIT_TERMINAL_PROMPT=0: %v", gotEnv)
	}
	if !envHas(gotEnv, "GCM_INTERACTIVE=never") {
		t.Errorf("hardened env missing GCM_INTERACTIVE=never: %v", gotEnv)
	}
	if !envHas(gotEnv, "GIT_ASKPASS=") {
		t.Errorf("hardened env missing GIT_ASKPASS=: %v", gotEnv)
	}
	if v, ok := configValueForKey(gotEnv, "credential.helper"); !ok || v != "" {
		t.Errorf("credential.helper = %q present=%v, want empty+present", v, ok)
	}
	// The header must be scoped to the remote it authenticates — derived from
	// the userinfo-STRIPPED form, so one key covers both spellings of the URL —
	// and the bare `http.extraHeader` (which would attach this credential to
	// every remote the git process and its descendants touch) must never appear.
	if v, ok := configValueForKey(gotEnv, "http."+cleanURL+".extraHeader"); !ok || v != header {
		t.Errorf("scoped extraHeader for %s = %q present=%v, want %q", cleanURL, v, ok, header)
	}
	if v, ok := configValueForKey(gotEnv, "http.extraHeader"); !ok || v != "" {
		t.Errorf("unscoped http.extraHeader = %q present=%v, want the empty-valued reset", v, ok)
	}
	// The cloned URL must be the CLEAN (userinfo-stripped) form, never the
	// token-bearing one.
	clonedURL := gotArgs[len(gotArgs)-2] // [clone, <url>, <dst>]
	if clonedURL != cleanURL {
		t.Errorf("cloned URL = %q, want clean %q", clonedURL, cleanURL)
	}
	for _, a := range gotArgs {
		if strings.Contains(a, "ghp_secret") {
			t.Fatalf("token leaked into clone argv: %v", gotArgs)
		}
	}
}

// TestProvisionGitAuthInertRegression is the regression guard: with GitAuth nil
// the manager must run git through the plain CommandRunner, never touch the env
// runner, and clone the URL UNCHANGED (no userinfo stripping).
func TestProvisionGitAuthInertRegression(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const dirtyURL = "https://x-access-token:ghp_secret@github.com/org/repo.git"

	var (
		gotArgs       []string
		plainRunHits  int
		envRunnerUsed bool
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: dir,
		CommandRunner: func(_ context.Context, name string, args ...string) ([]byte, error) {
			plainRunHits++
			if name != "git" {
				t.Errorf("unexpected binary %q", name)
			}
			gotArgs = append([]string(nil), args...)
			dst := args[len(args)-1]
			_ = os.MkdirAll(dst, 0o750)
			return nil, nil
		},
		EnvCommandRunner: func(_ context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
			envRunnerUsed = true
			return nil, nil
		},
		// GitAuth deliberately left nil → seam inert.
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   dirtyURL,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if plainRunHits != 1 {
		t.Fatalf("plain runner hits = %d, want 1", plainRunHits)
	}
	if envRunnerUsed {
		t.Fatal("env runner used on inert path — seam not inert")
	}
	// URL must be unchanged (still carries userinfo) — current behaviour.
	clonedURL := gotArgs[len(gotArgs)-2]
	if clonedURL != dirtyURL {
		t.Errorf("inert clone URL = %q, want unchanged %q", clonedURL, dirtyURL)
	}
}

// TestProvisionGitAuthHeaderNotEmittedForUnscopableRemote covers a resolver
// that returns a blanket credential regardless of the remote it was asked
// about. For an SSH remote there is no HTTP request to authenticate, so the
// header must be dropped entirely rather than injected under the bare
// `http.extraHeader` key — which would attach that credential to every https
// remote this git process, and everything it spawns, subsequently touches.
func TestProvisionGitAuthHeaderNotEmittedForUnscopableRemote(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const sshURL = "git@github.com:org/repo.git"
	const header = "Authorization: Bearer ghp_blanket_secret"

	var gotEnv []string
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: dir,
		CommandRunner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			t.Error("plain CommandRunner called on GitAuth-engaged path")
			return nil, nil
		},
		EnvCommandRunner: func(_ context.Context, extraEnv []string, _ string, args ...string) ([]byte, error) {
			gotEnv = append([]string(nil), extraEnv...)
			_ = os.MkdirAll(args[len(args)-1], 0o750)
			return nil, nil
		},
		GitAuth: func(_ context.Context, _ string) (string, bool, error) {
			return header, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s1",
		RepoURL:   sshURL,
		Strategy:  worktree.StrategyClone,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if v, ok := configValueForKey(gotEnv, "http.extraHeader"); !ok || v != "" {
		t.Errorf("unscoped http.extraHeader = %q present=%v, want the empty-valued reset for an SSH remote", v, ok)
	}
	for _, e := range gotEnv {
		if strings.Contains(e, "ghp_blanket_secret") {
			t.Fatalf("credential reached the env for an SSH remote via %q", strings.SplitN(e, "=", 2)[0])
		}
	}
	// The rest of the hardening is unaffected.
	if !envHas(gotEnv, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("hardened env missing GIT_TERMINAL_PROMPT=0: %v", gotEnv)
	}
	if v, ok := configValueForKey(gotEnv, "credential.helper"); !ok || v != "" {
		t.Errorf("credential.helper = %q present=%v, want empty+present", v, ok)
	}
}

// TestProvisionExcludesHarnessStateFromGitStatus is the provision-side control
// for the harness-state hygiene rule: a workarea this manager hands to a
// session must not report the session's own live state as untracked junk. It
// is the general form of the 2026-08-29 loss, in which a seat read `?? .pi/`
// in `git status` and deleted a running session's storage.
//
// The stub clone materializes a REAL git checkout so the exclusion is measured
// by git itself rather than asserted against our own writer.
//
// RED (with the excludeHarnessState call removed from Provision):
//
//	git status --porcelain = "?? .agent/\n?? .claude/\n?? .codex/\n?? .pi/"
//
// GREEN: empty.
func TestProvisionExcludesHarnessStateFromGitStatus(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	dir := t.TempDir()
	runner := newStubRunner(
		func(name string, args ...string) ([]byte, error) {
			if name != "git" || args[0] != "clone" {
				t.Errorf("unexpected call: %s %v", name, args)
			}
			dst := args[len(args)-1]
			if err := os.MkdirAll(dst, 0o750); err != nil {
				return nil, err
			}
			// A real checkout, so `git status` is the judge.
			for _, initArgs := range [][]string{
				{"init", "--quiet", "-b", "main"},
				{"config", "user.email", "test@example.invalid"},
				{"config", "user.name", "test"},
			} {
				cmd := exec.Command("git", append([]string{"-C", dst}, initArgs...)...) //nolint:gosec // G204: test fixture; dst is under t.TempDir().
				if out, err := cmd.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("git %v: %w (%s)", initArgs, err, out)
				}
			}
			return []byte(""), nil
		},
	)
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "s-exclude",
		RepoURL:   "git@example.com:org/repo.git",
		Branch:    "main",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Every harness state dir a session could create, created.
	for _, name := range harnessstate.Dirs() {
		sub := filepath.Join(path, name)
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(sub, "state"), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write into %s: %v", name, err)
		}
	}

	status := exec.Command("git", "-C", path, "status", "--porcelain") //nolint:gosec // G204: test fixture; path is under t.TempDir().
	out, statusErr := status.CombinedOutput()
	if statusErr != nil {
		t.Fatalf("git status: %v\n%s", statusErr, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("provisioned workarea reports harness state as untracked:\n%s", out)
	}

	// Ordinary session output is still visible — the exclusion must not have
	// silenced the whole checkout.
	if err := os.WriteFile(filepath.Join(path, "new.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write new.go: %v", err)
	}
	out, statusErr = exec.Command("git", "-C", path, "status", "--porcelain").CombinedOutput() //nolint:gosec // G204: test fixture; path is under t.TempDir().
	if statusErr != nil {
		t.Fatalf("git status: %v\n%s", statusErr, out)
	}
	if !strings.Contains(string(out), "new.go") {
		t.Fatalf("real session output is not reported by git status:\n%s", out)
	}
}
