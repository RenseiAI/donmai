//go:build runtime_integration

package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
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
