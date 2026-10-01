package afcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestRunWorktreeCleanupPreservesLeaseStateAndRetainedWorkarea(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	leaseDir := filepath.Join(root, ".terminal-leases")
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(root, "session-1")
	if err := os.MkdirAll(worktreePath, 0o750); err != nil {
		t.Fatal(err)
	}
	workareaID, err := workarea.NewWorkareaID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), workarea.AcquireSpec{
		SessionID: "11111111-1111-4111-8111-111111111111", TerminalResultID: "tr_11111111111111111111111111111111",
		WorkareaID: workareaID, WorkareaPath: worktreePath, Policy: workarea.DefaultLeasePolicy(),
		ReleaseRequested: true, ReleaseDisposition: "destroy",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := runWorktreeCleanup(context.Background(), root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || result.Cleaned != 0 {
		t.Fatalf("cleanup result = %+v", result)
	}
	if _, err := os.Stat(leaseDir); err != nil {
		t.Fatalf("lease state directory removed: %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("retained workarea removed: %v", err)
	}
}

// The helper runs the actual cleanup function with a private Git repository as
// its process cwd. Neither test case can address the source tree's common Git
// directory, even when the Go test binary was built from a linked worktree.
func TestCleanupDoesNotPruneUnrelatedWorktrees(t *testing.T) {
	if action := os.Getenv("DONMAI_TEST_PRIVATE_CLEANUP_ACTION"); action != "" {
		switch action {
		case "worktrees":
			result, err := runWorktreeCleanup(context.Background(), os.Getenv("DONMAI_TEST_PRIVATE_CLEANUP_TARGET"), false, true)
			if err != nil {
				t.Fatal(err)
			}
			if result.Scanned != 1 || result.Skipped != 1 || result.Cleaned != 0 {
				t.Fatalf("retained target cleanup = %+v", result)
			}
		case "branches-dry-run":
			if _, err := runBranchCleanup(context.Background(), true, false); err != nil {
				t.Fatal(err)
			}
		case "main-dotgit":
			result, err := runWorktreeCleanup(context.Background(), os.Getenv("DONMAI_TEST_PRIVATE_CLEANUP_TARGET"), false, true)
			if err != nil {
				t.Fatal(err)
			}
			if result.Scanned != 2 || result.Skipped != 2 || result.Cleaned != 0 || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error, ".git is a directory") {
				t.Fatalf("main .git directory cleanup = %+v", result)
			}
		case "escaping-lease":
			result, err := runWorktreeCleanup(context.Background(), os.Getenv("DONMAI_TEST_PRIVATE_CLEANUP_TARGET"), false, true)
			if err == nil || !strings.Contains(err.Error(), "stat terminal lease store") || result.Cleaned != 0 {
				t.Fatalf("escaping lease cleanup = result %+v, error %v", result, err)
			}
		case "escaping-dotgit":
			result, err := runWorktreeCleanup(context.Background(), os.Getenv("DONMAI_TEST_PRIVATE_CLEANUP_TARGET"), false, true)
			if err != nil || result.Scanned != 2 || result.Skipped != 2 || result.Cleaned != 0 || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error, ".git metadata check failed") {
				t.Fatalf("escaping .git cleanup = result %+v, error %v", result, err)
			}
		default:
			t.Fatalf("unknown private cleanup action %q", action)
		}
		return
	}

	for _, action := range []string{"worktrees", "branches-dry-run", "main-dotgit", "escaping-lease", "escaping-dotgit"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + root,
				"TMPDIR=" + root,
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "empty-gitconfig"),
				"GIT_TERMINAL_PROMPT=0",
				"GORACE=atexit_sleep_ms=0",
			}
			runPrivate := func(path string, argv, processEnv []string) {
				t.Helper()
				outputFile, err := os.CreateTemp(root, "process-output-")
				if err != nil {
					t.Fatal(err)
				}
				inputFile, err := os.Open(os.DevNull)
				if err != nil {
					_ = outputFile.Close()
					t.Fatal(err)
				}
				process, err := os.StartProcess(path, argv, &os.ProcAttr{
					Dir: repo, Env: processEnv, Files: []*os.File{inputFile, outputFile, outputFile},
				})
				_ = inputFile.Close()
				_ = outputFile.Close()
				if err != nil {
					t.Fatalf("start private fixture process: %v", err)
				}
				state, err := process.Wait()
				if err != nil || !state.Success() {
					output, _ := os.ReadFile(outputFile.Name())
					t.Fatalf("private fixture process failed: state=%v err=%v output=%s", state, err, output)
				}
			}
			gitBinary, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			privateGit := func(args ...string) {
				t.Helper()
				argv := []string{gitBinary}
				argv = append(argv, args...)
				runPrivate(gitBinary, argv, env)
			}
			privateGit("init", "-b", "main")
			privateGit("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")

			target := filepath.Join(root, "target")
			retained := filepath.Join(target, "retained")
			foreign := filepath.Join(root, "foreign")
			privateGit("worktree", "add", "-b", "retained", retained)
			privateGit("worktree", "add", "-b", "foreign", foreign)
			admin := filepath.Join(repo, ".git", "worktrees", "foreign")
			if _, err := os.Stat(admin); err != nil {
				t.Fatalf("private unrelated registration missing before test: %v", err)
			}

			leaseDir := filepath.Join(target, ".terminal-leases")
			store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: leaseDir})
			if err != nil {
				t.Fatal(err)
			}
			workareaID, err := workarea.NewWorkareaID()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Acquire(context.Background(), workarea.AcquireSpec{
				SessionID: "11111111-1111-4111-8111-111111111111", TerminalResultID: "tr_11111111111111111111111111111111",
				WorkareaID: workareaID, WorkareaPath: retained, Policy: workarea.DefaultLeasePolicy(),
				ReleaseRequested: true, ReleaseDisposition: "destroy",
			}); err != nil {
				t.Fatal(err)
			}
			if action == "main-dotgit" {
				if err := os.MkdirAll(filepath.Join(target, "main", ".git"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if action == "escaping-lease" {
				outsideLease := filepath.Join(root, "outside-lease")
				if err := os.Rename(leaseDir, outsideLease); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideLease, leaseDir); err != nil {
					t.Fatal(err)
				}
			}
			if action == "escaping-dotgit" {
				escapingEntry := filepath.Join(target, "escape")
				if err := os.Mkdir(escapingEntry, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(escapingEntry, ".git")); err != nil {
					t.Fatal(err)
				}
			}

			// A container may see the Git common directory but not a foreign
			// linked worktree mount. Hide only this fixture directory, then age
			// only its private administrative record past Git's prune expiry.
			hidden := filepath.Join(root, "hidden")
			if err := os.Mkdir(hidden, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(foreign, filepath.Join(hidden, "foreign")); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * 365 * 24 * time.Hour)
			if err := os.Chtimes(admin, old, old); err != nil {
				t.Fatal(err)
			}

			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			helperEnv := env
			helperEnv = append(helperEnv, "DONMAI_TEST_PRIVATE_CLEANUP_ACTION="+action, "DONMAI_TEST_PRIVATE_CLEANUP_TARGET="+target)
			runPrivate(executable, []string{executable, "-test.run=^TestCleanupDoesNotPruneUnrelatedWorktrees$"}, helperEnv)
			if _, err := os.Stat(admin); err != nil {
				t.Fatalf("%s removed an unrelated Git registration: %v", action, err)
			}
			if _, err := os.Stat(filepath.Join(hidden, "foreign")); err != nil {
				t.Fatalf("private unrelated worktree bytes lost: %v", err)
			}
			if action == "main-dotgit" {
				if info, err := os.Stat(filepath.Join(target, "main", ".git")); err != nil || !info.IsDir() {
					t.Fatalf("private main .git directory lost: info=%v err=%v", info, err)
				}
			}
			if action == "escaping-lease" {
				if _, err := os.Stat(retained); err != nil {
					t.Fatalf("retained worktree lost after escaping lease refusal: %v", err)
				}
			}
			if action == "escaping-dotgit" {
				if info, err := os.Lstat(filepath.Join(target, "escape", ".git")); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("escaping .git symlink changed: info=%v err=%v", info, err)
				}
			}
		})
	}
}
