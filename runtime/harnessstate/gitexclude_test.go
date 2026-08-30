package harnessstate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NewGitCheckout initializes a throwaway git repository and returns its path.
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
		cmd.Env = append(GitLocationNeutralEnv(os.Environ()),
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
	cmd.Env = GitLocationNeutralEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	return string(out)
}

func readExcludeFile(t *testing.T, dir string) string {
	t.Helper()
	path, ok := InfoExcludePath(dir)
	if !ok {
		t.Fatalf("InfoExcludePath(%s) reported no checkout", dir)
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

// TestEnsureExcluded_SilencesEveryStateDir is the control for the provision
// half of the fix, and it is deliberately written against Dirs() rather than
// a literal list: adding a row to the table must extend the guarantee
// automatically, not require someone to remember this test.
//
// RED (excludeDirTopLevel-era behaviour, only `.agent` excluded):
//
//	git status --porcelain = "?? .claude/\n?? .codex/\n?? .pi/"
//
// GREEN: empty.
func TestEnsureExcluded_SilencesEveryStateDir(t *testing.T) {
	t.Parallel()
	dir := newGitCheckout(t)

	if err := EnsureExcluded(dir); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	// Create every state dir with a file inside, exactly as a live session
	// would.
	for _, name := range Dirs() {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(sub, "state"), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write into %s: %v", name, err)
		}
	}
	if status := gitStatusPorcelain(t, dir); strings.TrimSpace(status) != "" {
		t.Fatalf("checkout is dirty with harness state present:\n%s", status)
	}
	// A directory that merely shares a prefix with a state dir is NOT
	// excluded — the entries must not over-match.
	if err := os.MkdirAll(filepath.Join(dir, ".pi-cache"), 0o700); err != nil {
		t.Fatalf("create .pi-cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pi-cache", "blob"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write .pi-cache/blob: %v", err)
	}
	if status := gitStatusPorcelain(t, dir); !strings.Contains(status, ".pi-cache") {
		t.Fatalf("a prefix-sharing directory was silently excluded too:\n%s", status)
	}
	// And nothing tracked was touched: the project's own .gitignore is not
	// ours to edit.
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore was created in the checkout (err=%v) — the exclude belongs in git's own exclude file", err)
	}
}

// TestEnsureExcluded_Idempotent proves repeated session starts leave exactly
// one line per entry, for every entry.
func TestEnsureExcluded_Idempotent(t *testing.T) {
	t.Parallel()
	dir := newGitCheckout(t)

	for i := 0; i < 3; i++ {
		if err := EnsureExcluded(dir); err != nil {
			t.Fatalf("EnsureExcluded (call %d): %v", i+1, err)
		}
	}
	body := readExcludeFile(t, dir)
	for _, entry := range ExcludeEntries() {
		if got := strings.Count(body, entry); got != 1 {
			t.Errorf("entry %q appears %d times after 3 calls, want 1:\n%s", entry, got, body)
		}
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
	path, ok := InfoExcludePath(dir)
	if !ok {
		t.Fatal("no exclude path for a fresh checkout")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("# operator's own entry\nscratch/"), 0o600); err != nil {
		t.Fatalf("seed exclude: %v", err)
	}

	if err := EnsureExcluded(dir); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	body := readExcludeFile(t, dir)
	if !strings.Contains(body, "# operator's own entry") || !strings.Contains(body, "scratch/") {
		t.Fatalf("pre-existing exclude content lost:\n%s", body)
	}
	// The seeded file had no trailing newline; the appended content must be on
	// its own line rather than glued onto "scratch/".
	if strings.Contains(body, "scratch/#") {
		t.Fatalf("content was glued onto the previous line:\n%s", body)
	}
	for _, entry := range ExcludeEntries() {
		if got := strings.Count(body, entry); got != 1 {
			t.Errorf("entry %q appears %d times, want 1:\n%s", entry, got, body)
		}
	}
}

// TestEnsureGitExcluded_NotAGitCheckout proves the silent no-op: a workarea
// that is not a git checkout is an ordinary way to run a session, not an
// error, and nothing is created on disk.
func TestEnsureGitExcluded_NotAGitCheckout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := EnsureExcluded(dir); err != nil {
		t.Fatalf("EnsureExcluded in a non-git dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("non-git directory was modified: %v", entries)
	}

	// A missing directory is likewise not an error.
	if err := EnsureExcluded(filepath.Join(dir, "nope")); err != nil {
		t.Fatalf("EnsureExcluded on a missing dir: %v", err)
	}
	// And neither is being asked to exclude nothing.
	if err := EnsureGitExcluded(dir); err != nil {
		t.Fatalf("EnsureGitExcluded with no entries: %v", err)
	}
	if err := EnsureGitExcluded("", ".pi/"); err != nil {
		t.Fatalf("EnsureGitExcluded with no dir: %v", err)
	}
}

// TestInfoExcludePath_LinkedWorktree pins the reason this uses
// `rev-parse --git-path` instead of assuming `<dir>/.git/info/exclude`: in a
// linked worktree `<dir>/.git` is a FILE, and the exclude file git actually
// reads lives in the shared git directory.
func TestInfoExcludePath_LinkedWorktree(t *testing.T) {
	t.Parallel()
	main := newGitCheckout(t)
	linked := filepath.Join(t.TempDir(), "linked")

	cmd := exec.Command("git", "-C", main, "worktree", "add", "--quiet", "--detach", linked) //nolint:gosec // G204: test fixture; both paths are t.TempDir().
	cmd.Env = GitLocationNeutralEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add unavailable here: %v\n%s", err, out)
	}

	path, ok := InfoExcludePath(linked)
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

	if err := EnsureExcluded(linked); err != nil {
		t.Fatalf("EnsureExcluded in a linked worktree: %v", err)
	}
	for _, name := range Dirs() {
		if err := os.MkdirAll(filepath.Join(linked, name), 0o700); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if status := gitStatusPorcelain(t, linked); strings.TrimSpace(status) != "" {
		t.Fatalf("linked worktree is dirty after excluding harness state:\n%s", status)
	}
}

// TestEnsureExcluded_SpawnsNoGitOutsideACheckout pins a contract this helper
// must not break: a repository-free session invokes NO git at all (the runner
// enforces that with its own marker test). A workarea with no .git anywhere
// above it is answered structurally, without a subprocess.
//
// RED (with the hasGitAncestor short-circuit removed from InfoExcludePath):
//
//	EnsureExcluded spawned git in a repository-free workarea: -C <dir> rev-parse --git-path info/exclude
//
// GREEN: no marker written.
func TestEnsureExcluded_SpawnsNoGitOutsideACheckout(t *testing.T) {
	// Not parallel: t.Setenv.
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-was-invoked")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	workarea := t.TempDir()
	if err := EnsureExcluded(workarea); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	if body, err := os.ReadFile(marker); err == nil { //nolint:gosec // test-local path
		t.Fatalf("EnsureExcluded spawned git in a repository-free workarea: %s", body)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read marker: %v", err)
	}
}
