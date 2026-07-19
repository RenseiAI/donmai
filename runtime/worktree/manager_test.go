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
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "s1",
		TerminalResultID: "result-1",
		Policy: workarea.LeasePolicy{
			SettlementBudget: time.Minute,
			SafetyMargin:     time.Second,
			LeaseDuration:    2 * time.Minute,
			MaxLeaseDuration: 5 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("AcquireTerminalLease: %v", err)
	}
	persisted, err := leaseStore.Get(lease.LeaseID)
	if err != nil || persisted.State != workarea.LeaseActive {
		t.Fatalf("lease not durable before teardown: lease=%+v err=%v", persisted, err)
	}

	if err := m.Teardown(context.Background(), "s1"); err != nil {
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
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	}); !errors.Is(err, workarea.ErrWorkareaLeased) {
		t.Fatalf("retained workarea was reusable after restart: %v", err)
	}

	claimManagerLease(t, m, lease, "invocation-1", "claim-1")
	ack := managerAcknowledgement(lease, "invocation-1", "claim-1")
	for i := 0; i < 2; i++ {
		released, ackErr := m.AcknowledgeTerminalResult(context.Background(), ack)
		if ackErr != nil {
			t.Fatalf("AcknowledgeTerminalResult %d: %v", i+1, ackErr)
		}
		if released.State != workarea.LeaseReleased {
			t.Fatalf("acknowledgement %d state = %q", i+1, released.State)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged leaf still exists: %v", err)
	}
	if _, err := m.Path("s1"); !errors.Is(err, worktree.ErrUnknownSession) {
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
		SessionID: "s1",
		RepoURL:   "x",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := first.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID:        "s1",
		TerminalResultID: "result-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Teardown(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}

	recovered, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner.run})
	if err != nil {
		t.Fatal(err)
	}
	claimManagerLease(t, recovered, lease, "invocation-1", "claim-1")
	_, err = recovered.AcknowledgeTerminalResult(context.Background(), managerAcknowledgement(lease, "invocation-1", "claim-1"))
	if err != nil {
		t.Fatalf("recovered acknowledgement: %v", err)
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
	for k, v := range m {
		if !strings.HasPrefix(k, "GIT_CONFIG_KEY_") || v != key {
			continue
		}
		n := strings.TrimPrefix(k, "GIT_CONFIG_KEY_")
		val, ok := m["GIT_CONFIG_VALUE_"+n]
		return val, ok
	}
	return "", false
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
	if v, ok := configValueForKey(gotEnv, "http.extraHeader"); !ok || v != header {
		t.Errorf("http.extraHeader = %q present=%v, want %q", v, ok, header)
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
