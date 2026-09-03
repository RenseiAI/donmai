package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/worktree"
)

// requireGit skips the test when no git binary is on PATH. The build
// tag keeps this file out of CI by default; opt-in via -tags=runtime_integration.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// initBareRepo creates a temporary bare repo seeded with one commit on
// branch "main". Returns the bare repo path.
func initBareRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	cmd := exec.Command("git", "clone", "--bare", work, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone --bare: %v\n%s", err, out)
	}
	return bare
}

func TestIntegrationProvisionTeardownClone(t *testing.T) {
	requireGit(t)

	bare := initBareRepo(t)
	parent := t.TempDir()
	m, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	path, err := m.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "sess-1",
		RepoURL:   bare,
		Branch:    "main",
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("expected README.md in worktree: %v", err)
	}

	if err := m.Teardown(ctx, "sess-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree gone, stat=%v", err)
	}
}

func TestIntegrationWorktreeBaseRefreshesBetweenLaunches(t *testing.T) {
	requireGit(t)

	bare := initBareRepo(t)
	parent := filepath.Join(t.TempDir(), "parent")
	clone := exec.Command("git", "clone", "--branch", "main", bare, parent)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone parent: %v\n%s", err, out)
	}
	advance := t.TempDir()
	clone = exec.Command("git", "clone", "--branch", "main", bare, advance)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone advance: %v\n%s", err, out)
	}
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run(advance, "config", "user.email", "test@example.com")
	run(advance, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(advance, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(advance, "add", "new.txt")
	run(advance, "commit", "-m", "advance")
	newTip := strings.TrimSpace(run(advance, "rev-parse", "HEAD"))
	run(advance, "push", "origin", "main")

	m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "first", Branch: "session-first", Strategy: worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent, BaseRef: "refs/heads/main",
	}); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if err := m.Teardown(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "second", Branch: "session-second", Strategy: worktree.StrategyWorktreeAdd,
		ParentRepoPath: parent, BaseRef: "origin/main",
	}); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	result, err := m.Result("second")
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseRef != "main" || result.BaseSHA != newTip || !result.BaseFetched {
		t.Fatalf("second base receipt = %#v, want tip %s", result, newTip)
	}
}
