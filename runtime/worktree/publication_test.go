package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestInteractivePublicationInspectionSerializesLeaseAheadOfTeardown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	statusStarted := make(chan struct{})
	allowStatus := make(chan struct{})
	var once sync.Once
	var observedStatusArgs []string
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "status" {
				once.Do(func() {
					observedStatusArgs = append([]string(nil), args...)
					close(statusStarted)
				})
				select {
				case <-allowStatus:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				break
			}
		}
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed git binary and temp inputs
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "77777777-7777-4777-8777-777777777777"
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{
		SessionID: sessionID,
		RepoURL:   remote,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(path, "dirty.go"), []byte("package dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type leaseResult struct {
		lease      *workarea.TerminalLease
		assessment worktree.PublicationAssessment
		err        error
	}
	leaseDone := make(chan leaseResult, 1)
	go func() {
		lease, assessment, err := manager.AcquireInteractiveTerminalLease(
			context.Background(),
			worktree.InteractivePublicationSpec{
				SessionID: sessionID,
				Repositories: []worktree.PublicationTarget{{
					Name: "remote", Repository: remote, Branch: branch, AllowRemoteHEAD: true,
				}},
			},
			workarea.AcquireSpec{
				SessionID: sessionID, TerminalResultID: "tr_77777777777777777777777777777777",
				Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true,
			},
			false,
		)
		leaseDone <- leaseResult{lease: lease, assessment: assessment, err: err}
	}()
	<-statusStarted
	teardownDone := make(chan error, 1)
	go func() { teardownDone <- manager.Teardown(context.Background(), sessionID) }()
	close(allowStatus)

	retained := <-leaseDone
	if retained.err != nil || retained.lease == nil || !retained.assessment.Retain || retained.assessment.Reason != worktree.PublicationReasonDirty {
		t.Fatalf("lease result=%+v", retained)
	}
	if err := <-teardownDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("teardown crossed committed retention lease: %v", err)
	}
	if retained.lease.ReleaseDisposition != "archive" {
		t.Fatalf("release disposition=%q", retained.lease.ReleaseDisposition)
	}
	statusCommand := strings.Join(observedStatusArgs, " ")
	for _, required := range []string{"core.hooksPath=/dev/null", "core.fsmonitor=false", "protocol.ext.allow=never", "--untracked-files=all"} {
		if !strings.Contains(statusCommand, required) {
			t.Errorf("publication status command omitted %q: %s", required, statusCommand)
		}
	}
}

func TestInteractivePublicationRefusesAdmittedRemoteHelperWithoutExecutingIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	var unsafeCalls int
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "ext::") {
			unsafeCalls++
		}
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed git binary and temp inputs
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "88888888-8888-4888-8888-888888888888"
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	lease, assessment, err := manager.AcquireInteractiveTerminalLease(
		t.Context(),
		worktree.InteractivePublicationSpec{
			SessionID: sessionID,
			Repositories: []worktree.PublicationTarget{{
				Name: "remote", Repository: "ext::sh -c 'exit 0'", Branch: branch,
			}},
		},
		workarea.AcquireSpec{
			SessionID: sessionID, TerminalResultID: "tr_88888888888888888888888888888888",
			Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true,
		},
		false,
	)
	if err != nil || lease == nil || !assessment.Retain || assessment.Reason != worktree.PublicationReasonUncertain {
		t.Fatalf("lease=%+v assessment=%+v err=%v", lease, assessment, err)
	}
	if unsafeCalls != 0 {
		t.Fatalf("unsafe remote helper reached git %d time(s)", unsafeCalls)
	}
}

func publicationBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	publicationGit(t, "", "init", "--bare", "--quiet", remote)
	publicationGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt")
	publicationGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	publicationGit(t, seed, "remote", "add", "origin", remote)
	publicationGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	publicationGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func publicationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test binary and temp inputs
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
