package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
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
		runner.plan = append(runner.plan,
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
	runner := newStubRunner(
		func(_ string, args ...string) ([]byte, error) {
			captured = append([]string(nil), args...)
			dst := args[len(args)-2] // dst, then origin/branch tail
			_ = os.MkdirAll(dst, 0o750)
			return nil, nil
		},
	)
	m, err := worktree.NewManager(worktree.Options{
		ParentDir:     dir,
		CommandRunner: runner.run,
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
