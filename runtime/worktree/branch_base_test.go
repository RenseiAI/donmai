package worktree_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/worktree"
)

type branchGitFixture struct {
	root, remote, parent, baseSHA string
	env                           []string
}

func (f branchGitFixture) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "git" {
		return nil, fmt.Errorf("unexpected fixture command")
	}
	args = append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	command := exec.CommandContext(ctx, "git")
	command.Args = append(command.Args, args...)
	command.Env = f.env
	return command.CombinedOutput()
}

func (f branchGitFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.run(context.Background(), "git", args...)
	if err != nil {
		t.Fatalf("owned git fixture: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func newBranchGitFixture(t *testing.T) branchGitFixture {
	t.Helper()
	root := t.TempDir()
	f := branchGitFixture{root: root, remote: filepath.Join(root, "remote.git"), parent: filepath.Join(root, "parent")}
	f.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
	f.git(t, "init", "--bare", f.remote)
	seed := filepath.Join(root, "seed")
	f.git(t, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "-C", seed, "add", "source.txt")
	f.git(t, "-C", seed, "commit", "-m", "main source")
	f.git(t, "-C", seed, "checkout", "-b", "release/next")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("selected base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "-C", seed, "commit", "-am", "base source")
	f.baseSHA = f.git(t, "-C", seed, "rev-parse", "HEAD")
	f.git(t, "-C", seed, "remote", "add", "origin", f.remote)
	f.git(t, "-C", seed, "push", "origin", "main", "release/next")
	f.git(t, "--git-dir", f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	f.git(t, "clone", f.remote, f.parent)
	return f
}

func branchManager(t *testing.T, f branchGitFixture) *worktree.Manager {
	t.Helper()
	m, err := worktree.NewManager(worktree.Options{ParentDir: filepath.Join(f.root, "workareas"), CommandRunner: f.run})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRequiredBranchBaseCloneAndWorktreeAdd(t *testing.T) {
	t.Parallel()
	for _, strategy := range []worktree.CloneStrategy{worktree.StrategyClone, worktree.StrategyWorktreeAdd} {
		t.Run(strategy.String(), func(t *testing.T) {
			t.Parallel()
			f := newBranchGitFixture(t)
			m := branchManager(t, f)
			branch := "release/next"
			if strategy == worktree.StrategyWorktreeAdd {
				branch = "work/session"
			}
			path, err := m.Provision(context.Background(), worktree.ProvisionSpec{SessionID: "owned", RepoURL: f.remote, Strategy: strategy, ParentRepoPath: f.parent, Branch: branch, BaseRef: "release/next", RequireBranchBase: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := m.Teardown(context.Background(), "owned"); err != nil {
					t.Error(err)
				}
			})
			if got := f.git(t, "-C", path, "rev-parse", "HEAD"); got != f.baseSHA {
				t.Fatal("provision fell back to default HEAD")
			}
			if got := f.git(t, "-C", path, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
				t.Fatalf("wrong worktree branch %q", got)
			}
		})
	}
}

func TestRequiredBranchBaseRejectsTagFallbackAndStaleTracking(t *testing.T) {
	t.Parallel()
	for _, strategy := range []worktree.CloneStrategy{worktree.StrategyClone, worktree.StrategyWorktreeAdd} {
		t.Run(strategy.String(), func(t *testing.T) {
			t.Parallel()
			f := newBranchGitFixture(t)
			m := branchManager(t, f)
			f.git(t, "--git-dir", f.remote, "tag", "release/next", f.baseSHA)
			f.git(t, "--git-dir", f.remote, "update-ref", "-d", "refs/heads/release/next")
			// The old tracking ref intentionally remains. A tag of the same name must
			// not turn an absent remote branch into successful new-branch authority.
			if f.git(t, "-C", f.parent, "show-ref", "--verify", "--hash", "refs/remotes/origin/release/next") != f.baseSHA {
				t.Fatal("stale tracking fixture missing")
			}
			branch := "release/next"
			if strategy == worktree.StrategyWorktreeAdd {
				branch = "work/session"
			}
			if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{SessionID: "refused", RepoURL: f.remote, Strategy: strategy, ParentRepoPath: f.parent, Branch: branch, BaseRef: "release/next", RequireBranchBase: true}); err == nil || !errors.Is(err, worktree.ErrInvalidBaseRef) {
				t.Fatalf("tag/stale fallback was not refused: %v", err)
			}
			if strategy == worktree.StrategyClone {
				path, err := m.Provision(context.Background(), worktree.ProvisionSpec{SessionID: "legacy-tag", RepoURL: f.remote, Strategy: strategy, Branch: "release/next"})
				if err != nil {
					t.Fatalf("legacy tag behavior changed: %v", err)
				}
				t.Cleanup(func() {
					if err := m.Teardown(context.Background(), "legacy-tag"); err != nil {
						t.Error(err)
					}
				})
				if f.git(t, "-C", path, "rev-parse", "HEAD") != f.baseSHA {
					t.Fatal("legacy tag did not retain its commit")
				}
			}
		})
	}
}
