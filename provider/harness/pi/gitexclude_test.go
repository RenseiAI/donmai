package pi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitCheckout initializes a throwaway git repository and returns its path.
// It skips (rather than fails) when git is unavailable, since the behaviour
// under test is defined as "no-op when git cannot answer".
func newGitCheckout(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	// A committed file gives `git status --porcelain` a real baseline; an
	// empty repo would report clean for reasons unrelated to the exclude.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "README.md"},
		{"commit", "--quiet", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture; dir is t.TempDir() and args are literals.
		cmd.Env = append(gitLocationNeutralEnv(os.Environ()),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain") //nolint:gosec // G204: test fixture; dir is t.TempDir().
	cmd.Env = gitLocationNeutralEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	return string(out)
}

func readExcludeFile(t *testing.T, dir string) string {
	t.Helper()
	path, ok := gitInfoExcludePath(dir)
	if !ok {
		t.Fatalf("gitInfoExcludePath(%s) reported no checkout", dir)
	}
	b, err := os.ReadFile(path) //nolint:gosec // test-local path from git itself
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read exclude file: %v", err)
	}
	return string(b)
}

// TestMaterializeExtension_LeavesCheckoutClean is the control for the
// motive half of the fix. Materializing a session's state into a git checkout
// must not make that checkout look dirty: `?? .pi/` in `git status` is what
// made a live session's storage read as deletable junk.
//
// RED (with the ensureGitExcluded call removed from materializeExtension):
//
//	git status --porcelain = "?? .pi/"
//
// GREEN: empty.
func TestMaterializeExtension_LeavesCheckoutClean(t *testing.T) {
	t.Parallel()
	dir := newGitCheckout(t)

	if _, err := materializeExtension(dir); err != nil {
		t.Fatalf("materializeExtension: %v", err)
	}
	if status := gitStatusPorcelain(t, dir); strings.TrimSpace(status) != "" {
		t.Fatalf("checkout is dirty after materializing session state:\n%s", status)
	}
	// The state dir really is there — a clean status because nothing was
	// created would be a false pass.
	if _, err := os.Stat(filepath.Join(dir, piStateDir)); err != nil {
		t.Fatalf("state dir missing after materialize: %v", err)
	}
	// And nothing tracked was touched: the project's own .gitignore is not
	// ours to edit.
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore was created in the checkout (err=%v) — the exclude belongs in git's own exclude file", err)
	}
}

// TestEnsureGitExcluded_Idempotent proves repeated session starts leave
// exactly one line per entry.
func TestEnsureGitExcluded_Idempotent(t *testing.T) {
	t.Parallel()
	dir := newGitCheckout(t)

	for i := 0; i < 3; i++ {
		if err := ensureGitExcluded(dir, piGitExcludeEntries()...); err != nil {
			t.Fatalf("ensureGitExcluded (call %d): %v", i+1, err)
		}
	}
	body := readExcludeFile(t, dir)
	if got := strings.Count(body, piStateDir+"/"); got != 1 {
		t.Fatalf("exclude entry appears %d times after 3 calls, want 1:\n%s", got, body)
	}
	if got := strings.Count(body, gitExcludeHeader); got != 1 {
		t.Fatalf("exclude header appears %d times after 3 calls, want 1:\n%s", got, body)
	}
}

// TestEnsureGitExcluded_PreservesExistingContent proves an existing exclude
// file (including one that does not end in a newline) is appended to, never
// rewritten or corrupted.
func TestEnsureGitExcluded_PreservesExistingContent(t *testing.T) {
	t.Parallel()
	dir := newGitCheckout(t)
	path, ok := gitInfoExcludePath(dir)
	if !ok {
		t.Fatal("no exclude path for a fresh checkout")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("# operator's own entry\nscratch/"), 0o600); err != nil {
		t.Fatalf("seed exclude: %v", err)
	}

	if err := ensureGitExcluded(dir, piGitExcludeEntries()...); err != nil {
		t.Fatalf("ensureGitExcluded: %v", err)
	}
	body := readExcludeFile(t, dir)
	if !strings.Contains(body, "# operator's own entry") || !strings.Contains(body, "scratch/") {
		t.Fatalf("pre-existing exclude content lost:\n%s", body)
	}
	// The seeded file had no trailing newline; the appended entry must be on
	// its own line rather than glued onto "scratch/".
	if strings.Contains(body, "scratch/"+piStateDir) {
		t.Fatalf("entry was glued onto the previous line:\n%s", body)
	}
	if got := strings.Count(body, piStateDir+"/"); got != 1 {
		t.Fatalf("entry appears %d times, want 1:\n%s", got, body)
	}
}

// TestEnsureGitExcluded_NotAGitCheckout proves the silent no-op: a workarea
// that is not a git checkout is an ordinary way to run a session, not an
// error, and nothing is created on disk.
func TestEnsureGitExcluded_NotAGitCheckout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := ensureGitExcluded(dir, piGitExcludeEntries()...); err != nil {
		t.Fatalf("ensureGitExcluded in a non-git dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("non-git directory was modified: %v", entries)
	}

	// A missing directory is likewise not an error.
	if err := ensureGitExcluded(filepath.Join(dir, "nope"), piGitExcludeEntries()...); err != nil {
		t.Fatalf("ensureGitExcluded on a missing dir: %v", err)
	}
	// And neither is being asked to exclude nothing.
	if err := ensureGitExcluded(dir); err != nil {
		t.Fatalf("ensureGitExcluded with no entries: %v", err)
	}
}

// TestGitInfoExcludePath_LinkedWorktree pins the reason this uses
// `rev-parse --git-path` instead of assuming `<dir>/.git/info/exclude`: in a
// linked worktree `<dir>/.git` is a FILE, and the exclude file git actually
// reads lives in the shared git directory.
func TestGitInfoExcludePath_LinkedWorktree(t *testing.T) {
	t.Parallel()
	main := newGitCheckout(t)
	linked := filepath.Join(t.TempDir(), "linked")

	cmd := exec.Command("git", "-C", main, "worktree", "add", "--quiet", "--detach", linked) //nolint:gosec // G204: test fixture; both paths are t.TempDir().
	cmd.Env = gitLocationNeutralEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable here: %v\n%s", err, out)
	}

	path, ok := gitInfoExcludePath(linked)
	if !ok {
		t.Fatal("no exclude path resolved for a linked worktree")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("exclude path is not absolute: %q", path)
	}
	naive := filepath.Join(linked, ".git", "info", "exclude")
	if path == naive {
		t.Fatalf("resolved the naive path %q — a linked worktree's .git is a file, not a directory", naive)
	}

	if err := ensureGitExcluded(linked, piGitExcludeEntries()...); err != nil {
		t.Fatalf("ensureGitExcluded in a linked worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(linked, piStateDir), 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if status := gitStatusPorcelain(t, linked); strings.TrimSpace(status) != "" {
		t.Fatalf("linked worktree is dirty after excluding the state dir:\n%s", status)
	}
}
