package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

type trailerCommittingProvider struct {
	agent.HarnessProvider
	commit   func(context.Context, agent.Spec)
	spawnErr error
}

func (p *trailerCommittingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.commit(ctx, spec)
	if p.spawnErr != nil {
		return nil, p.spawnErr
	}
	return p.HarnessProvider.Spawn(ctx, spec)
}

func TestRunCommitTrailerHook(t *testing.T) {
	// Isolate the real-clone fixture from host identity and hook configuration.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, name := range []string{"First Owner", "Second Owner $(touch injected) `touch injected`"} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			qw := h.queuedWork("HOOK-CREDIT")
			qw.CoAuthorName = name
			qw.CoAuthorEmail = strings.ToLower(strings.Fields(name)[0]) + "@" + "example.test"
			trailer := "Co-Authored-By: " + name + " <" + qw.CoAuthorEmail + ">"
			provider, err := stub.New()
			if err != nil {
				t.Fatal(err)
			}
			spawned := false
			if err := h.runner.registry.Register(&trailerCommittingProvider{
				HarnessProvider: provider.(agent.HarnessProvider),
				commit: func(ctx context.Context, spec agent.Spec) {
					spawned = true
					git := func(args ...string) string {
						t.Helper()
						out, gitErr := runGit(ctx, spec.Cwd, gitIdentity{Name: "Agent", Email: "agent" + "@" + "example.test"}, args...)
						if gitErr != nil {
							t.Fatalf("git %s: %v", args[0], gitErr)
						}
						return out
					}
					assertOne := func(label string) {
						t.Helper()
						message := git("log", "-1", "--format=%B")
						count := 0
						for _, line := range strings.Split(message, "\n") {
							if line == trailer {
								count++
							}
						}
						if count != 1 {
							t.Errorf("%s: co-author count = %d, want 1", label, count)
						}
					}
					gitDir := git("rev-parse", "--absolute-git-dir")
					for path, mode := range map[string]os.FileMode{
						filepath.Join(gitDir, trailerDataFile):               0o600,
						filepath.Join(gitDir, "hooks", "prepare-commit-msg"): 0o700,
					} {
						info, statErr := os.Stat(path)
						if statErr != nil {
							t.Fatal("hook metadata missing")
						}
						if info.Mode().Perm() != mode {
							t.Errorf("hook metadata mode = %o, want %o", info.Mode().Perm(), mode)
						}
					}
					base := git("rev-parse", "HEAD")
					writeFile(t, spec.Cwd, "agent-change.txt", "ordinary commit\n")
					git("add", "-A")
					staged := git("diff", "--cached", "--name-only")
					if strings.Contains(staged, trailerDataFile) || strings.Contains(staged, "prepare-commit-msg") {
						t.Error("git add -A staged private hook metadata")
					}
					git("commit", "-m", "Ordinary agent change")
					assertOne("ordinary")
					git("commit", "--amend", "--no-edit")
					assertOne("amend")
					git("commit", "--allow-empty", "-m", "Other contributor\n\nCo-Authored-By: Model <model"+"@"+"example.test>")
					assertOne("other contributor")
					if !strings.Contains(git("log", "-1", "--format=%B"), "Co-Authored-By: Model") {
						t.Error("hook removed another contributor")
					}
					git("rebase", "--force-rebase", base)
					assertOne("rebase")
					writeFile(t, spec.Cwd, "backstop-change.txt", "backstop commit\n")
					git("add", "-A")
					git(backstopCommitArgs(qw, "Backstop change")...)
					assertOne("backstop")
					if _, statErr := os.Stat(filepath.Join(spec.Cwd, "injected")); !errors.Is(statErr, os.ErrNotExist) {
						t.Error("co-author name was interpreted by a shell")
					}
				},
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil || res.Status != "completed" || !spawned {
				t.Fatalf("Run: err=%v status=%s spawned=%v", err, res.Status, spawned)
			}
			for _, path := range []string{trailerDataFile, filepath.Join("hooks", "prepare-commit-msg")} {
				if _, statErr := os.Stat(filepath.Join(res.WorktreePath, ".git", path)); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("session cleanup left hook metadata: %v", statErr)
				}
			}
		})
	}
}

func TestTrailerHookFileLifecycle(t *testing.T) {
	t.Parallel()
	email := "owner" + "@" + "example.test"
	t.Run("invalid attribution creates nothing", func(t *testing.T) {
		dir := t.TempDir()
		if err := installTrailerHook(dir, "invalid\nname", email); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid pair created metadata: %v", err)
		}
	})
	t.Run("existing hook is retained on failure", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "hooks/prepare-commit-msg", "original hook\n")
		if err := installTrailerHook(dir, "Owner", email); err == nil {
			t.Fatal("installation overwrote existing hook")
		}
		content, err := os.ReadFile(filepath.Join(dir, "hooks", "prepare-commit-msg"))
		if err != nil || string(content) != "original hook\n" {
			t.Fatal("existing hook changed")
		}
		if _, err := os.Stat(filepath.Join(dir, trailerDataFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed installation left data")
		}
	})
	t.Run("cleanup is idempotent", func(t *testing.T) {
		dir := t.TempDir()
		if err := installTrailerHook(dir, "Owner", email); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := removeTrailerHook(dir); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestRunTrailerHookProvisionFailure(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	h := newRunnerHarness(t)
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	qw := h.queuedWork("HOOK-INSTALL-FAILURE")
	qw.CoAuthorName, qw.CoAuthorEmail = "Owner", "owner"+"@"+"example.test"
	path, err := h.runner.wt.Provision(t.Context(), worktree.ProvisionSpec{
		SessionID: qw.SessionID, RepoURL: qw.Repository, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, ".git/hooks/prepare-commit-msg", "existing hook\n")
	res, err := h.runner.Run(t.Context(), qw)
	if err == nil || res.FailureMode != FailureWorktreeProvision {
		t.Fatal("Run did not reject conflicting hook at provision")
	}
	content, err := os.ReadFile(filepath.Join(path, ".git", "hooks", "prepare-commit-msg"))
	if err != nil || string(content) != "existing hook\n" {
		t.Fatal("Run overwrote existing hook")
	}
	if _, err := os.Stat(filepath.Join(path, ".git", trailerDataFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Run left private data after installation failure")
	}
}

func TestRunTrailerHookCleanupOnSpawnFailure(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	h := newRunnerHarness(t)
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	qw := h.queuedWork("HOOK-SPAWN-FAILURE")
	qw.CoAuthorName, qw.CoAuthorEmail = "Owner", "owner"+"@"+"example.test"
	provider, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	spawned := false
	if err := h.runner.registry.Register(&trailerCommittingProvider{
		HarnessProvider: provider.(agent.HarnessProvider),
		spawnErr:        errors.New("fixture spawn failure"),
		commit: func(_ context.Context, spec agent.Spec) {
			spawned = true
			if _, err := os.Stat(filepath.Join(spec.Cwd, ".git", trailerDataFile)); err != nil {
				t.Fatal("hook not installed before failed spawn")
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := h.runner.Run(t.Context(), qw)
	if err == nil || !spawned {
		t.Fatal("fixture did not reach failed spawn")
	}
	for _, path := range []string{trailerDataFile, filepath.Join("hooks", "prepare-commit-msg")} {
		if _, err := os.Stat(filepath.Join(res.WorktreePath, ".git", path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed spawn left session attribution metadata")
		}
	}
}
