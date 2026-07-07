package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// makeSiblingOrigin creates a bare git repo named dirName (e.g.
// "corpus.git") seeded with the gitInit fixture commit (README.md) and
// returns (file:// URL, work-repo path). The work repo is kept so tests
// can push follow-up commits via pushSiblingCommit. Mirrors the
// makeBareRepo pattern in runner_test.go.
func makeSiblingOrigin(t *testing.T, dirName string) (url, workDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	workDir = t.TempDir()
	gitInit(t, workDir)
	bare := filepath.Join(t.TempDir(), dirName)
	siblingGit(t, filepath.Dir(bare), "clone", "--bare", workDir, bare)
	return "file://" + bare, workDir
}

// pushSiblingCommit commits relPath in workDir and pushes the current
// branch (same name) to the bare origin behind url (file:// URL from
// makeSiblingOrigin).
func pushSiblingCommit(t *testing.T, workDir, url, relPath string) {
	t.Helper()
	writeFile(t, workDir, relPath, "marker\n")
	siblingGit(t, workDir, "add", relPath)
	siblingGit(t, workDir, "commit", "-m", "add "+relPath)
	siblingGit(t, workDir, "push", url, "HEAD")
}

// siblingGit runs git in dir and fails the test on a non-zero exit.
func siblingGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	//nolint:gosec // G204: test fixture, args come from test callers.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newSessionWorktree creates a parent dir plus a session worktree dir
// inside it (the provisioned-clone layout) and returns both.
func newSessionWorktree(t *testing.T) (parent, wpath string) {
	t.Helper()
	parent = t.TempDir()
	wpath = filepath.Join(parent, "session-repo")
	if err := os.MkdirAll(wpath, 0o750); err != nil {
		t.Fatal(err)
	}
	return parent, wpath
}

func TestSiblingDirName(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/Example/docs-corpus", "docs-corpus"},
		{"https://github.com/Example/docs-corpus.git", "docs-corpus"},
		{"git@github.com:Example/notes.git", "notes"},
		{"file:///tmp/fixtures/repo.git", "repo"},
		{"https://example.com/org/repo/", "repo"},
		{"", "."}, // path.Base("") = "."; rejected by safeSiblingName.
		{"..", ".."},
	}
	for _, tt := range tests {
		if got := siblingDirName(tt.url); got != tt.want {
			t.Errorf("siblingDirName(%q) = %q; want %q", tt.url, got, tt.want)
		}
	}
}

func TestSafeSiblingName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{`a\b`, false},
		{"docs-corpus", true},
		{"repo.name", true},
	}
	for _, tt := range tests {
		if got := safeSiblingName(tt.name); got != tt.want {
			t.Errorf("safeSiblingName(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
}

func TestProvisionSiblingRepos(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the env spec; it may pre-populate the parent dir.
		setup func(t *testing.T, parent, wpath string) string
		check func(t *testing.T, parent, wpath string)
	}{
		{
			name: "single URL clones shallow sibling next to worktree",
			setup: func(t *testing.T, _, _ string) string {
				url, _ := makeSiblingOrigin(t, "corpus.git")
				return url
			},
			check: func(t *testing.T, parent, wpath string) {
				sib := filepath.Join(parent, "corpus")
				if _, err := os.Stat(filepath.Join(sib, ".git")); err != nil {
					t.Fatalf("sibling .git missing: %v", err)
				}
				if _, err := os.Stat(filepath.Join(sib, "README.md")); err != nil {
					t.Errorf("sibling content missing: %v", err)
				}
				if _, err := os.Stat(filepath.Join(sib, ".git", "shallow")); err != nil {
					t.Errorf("clone is not shallow (.git/shallow absent): %v", err)
				}
				// NEXT TO the worktree, never inside it.
				if _, err := os.Stat(filepath.Join(wpath, "corpus")); !os.IsNotExist(err) {
					t.Errorf("sibling leaked inside the worktree: stat err = %v", err)
				}
			},
		},
		{
			name: "two URLs provision two siblings",
			setup: func(t *testing.T, _, _ string) string {
				a, _ := makeSiblingOrigin(t, "corpus-a.git")
				b, _ := makeSiblingOrigin(t, "corpus-b.git")
				return a + ", " + b
			},
			check: func(t *testing.T, parent, _ string) {
				for _, name := range []string{"corpus-a", "corpus-b"} {
					if _, err := os.Stat(filepath.Join(parent, name, ".git")); err != nil {
						t.Errorf("sibling %s missing: %v", name, err)
					}
				}
			},
		},
		{
			name: "hash ref clones the named branch",
			setup: func(t *testing.T, _, _ string) string {
				url, work := makeSiblingOrigin(t, "corpus.git")
				checkout(t, work, "docs-v2")
				pushSiblingCommit(t, work, url, "V2-MARKER.md")
				return url + "#docs-v2"
			},
			check: func(t *testing.T, parent, _ string) {
				marker := filepath.Join(parent, "corpus", "V2-MARKER.md")
				if _, err := os.Stat(marker); err != nil {
					t.Errorf("branch marker missing (ref not honored): %v", err)
				}
			},
		},
		{
			name: "clone failure is non-fatal and later entries still provision",
			setup: func(t *testing.T, _, _ string) string {
				bad := "file://" + filepath.Join(t.TempDir(), "nonexistent.git")
				good, _ := makeSiblingOrigin(t, "corpus.git")
				return bad + "," + good
			},
			check: func(t *testing.T, parent, _ string) {
				if _, err := os.Stat(filepath.Join(parent, "corpus", ".git")); err != nil {
					t.Errorf("good sibling missing after earlier failure: %v", err)
				}
				if _, err := os.Stat(filepath.Join(parent, "nonexistent", ".git")); !os.IsNotExist(err) {
					t.Errorf("failed clone left a .git behind: stat err = %v", err)
				}
			},
		},
		{
			name: "unsafe names are skipped",
			setup: func(_ *testing.T, _, _ string) string {
				return "#branch-only, .., ., ,"
			},
			check: func(t *testing.T, parent, wpath string) {
				entries, err := os.ReadDir(parent)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != filepath.Base(wpath) {
					t.Errorf("parent dir polluted: %v", entries)
				}
			},
		},
		{
			name: "target colliding with session worktree is skipped",
			setup: func(t *testing.T, _, wpath string) string {
				url, _ := makeSiblingOrigin(t, filepath.Base(wpath)+".git")
				return url
			},
			check: func(t *testing.T, _, wpath string) {
				if _, err := os.Stat(filepath.Join(wpath, ".git")); !os.IsNotExist(err) {
					t.Errorf("worktree was clobbered by a sibling clone: stat err = %v", err)
				}
			},
		},
		{
			name: "existing dir without .git is left untouched",
			setup: func(t *testing.T, parent, _ string) string {
				url, _ := makeSiblingOrigin(t, "corpus.git")
				writeFile(t, filepath.Join(parent, "corpus"), "keep.txt", "keep\n")
				return url
			},
			check: func(t *testing.T, parent, _ string) {
				if _, err := os.Stat(filepath.Join(parent, "corpus", "keep.txt")); err != nil {
					t.Errorf("pre-existing content deleted: %v", err)
				}
				if _, err := os.Stat(filepath.Join(parent, "corpus", ".git")); !os.IsNotExist(err) {
					t.Errorf("non-git dir was cloned into: stat err = %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, wpath := newSessionWorktree(t)
			spec := tt.setup(t, parent, wpath)
			provisionSiblingRepos(context.Background(), discardLogger(), spec, wpath)
			tt.check(t, parent, wpath)
		})
	}
}

func TestProvisionSiblingReposFreshensExistingClone(t *testing.T) {
	parent, wpath := newSessionWorktree(t)
	url, work := makeSiblingOrigin(t, "corpus.git")

	provisionSiblingRepos(context.Background(), discardLogger(), url, wpath)
	marker := filepath.Join(parent, "corpus", "FRESH-MARKER.md")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker present before origin advanced: stat err = %v", err)
	}

	pushSiblingCommit(t, work, url, "FRESH-MARKER.md")
	provisionSiblingRepos(context.Background(), discardLogger(), url, wpath)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("existing sibling was not freshened: %v", err)
	}
}

func TestProvisionSiblingReposFreshenFailureKeepsStaleCopy(t *testing.T) {
	parent, wpath := newSessionWorktree(t)
	url, _ := makeSiblingOrigin(t, "corpus.git")

	provisionSiblingRepos(context.Background(), discardLogger(), url, wpath)

	// Remove the origin so the freshen pull fails; the stale sibling
	// must survive untouched.
	bare := url[len("file://"):]
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	provisionSiblingRepos(context.Background(), discardLogger(), url, wpath)
	if _, err := os.Stat(filepath.Join(parent, "corpus", "README.md")); err != nil {
		t.Errorf("stale sibling copy lost after failed freshen: %v", err)
	}
}

func TestProvisionSiblingsReadsProcessEnv(t *testing.T) {
	r := &Runner{logger: discardLogger()}

	t.Run("unset env is a no-op", func(t *testing.T) {
		parent, wpath := newSessionWorktree(t)
		t.Setenv(siblingReposEnv, "")
		r.provisionSiblings(context.Background(), QueuedWork{}, wpath)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("expected untouched parent dir, got %v", entries)
		}
	})

	t.Run("env value provisions siblings", func(t *testing.T) {
		parent, wpath := newSessionWorktree(t)
		url, _ := makeSiblingOrigin(t, "corpus.git")
		t.Setenv(siblingReposEnv, url)
		r.provisionSiblings(context.Background(), QueuedWork{}, wpath)
		if _, err := os.Stat(filepath.Join(parent, "corpus", ".git")); err != nil {
			t.Errorf("sibling missing: %v", err)
		}
	})
}

// TestProvisionSiblingReposConcurrentSessions drives two sessions that
// share a parent dir at the same sibling spec concurrently. The
// per-target mutex must serialize the clone/freshen; -race validates.
func TestProvisionSiblingReposConcurrentSessions(t *testing.T) {
	parent, wpathA := newSessionWorktree(t)
	wpathB := filepath.Join(parent, "session-repo-b")
	if err := os.MkdirAll(wpathB, 0o750); err != nil {
		t.Fatal(err)
	}
	url, _ := makeSiblingOrigin(t, "corpus.git")

	var wg sync.WaitGroup
	for _, wp := range []string{wpathA, wpathB} {
		wg.Add(1)
		go func(wp string) {
			defer wg.Done()
			provisionSiblingRepos(context.Background(), discardLogger(), url, wp)
		}(wp)
	}
	wg.Wait()

	if _, err := os.Stat(filepath.Join(parent, "corpus", ".git")); err != nil {
		t.Errorf("sibling missing after concurrent provisioning: %v", err)
	}
}
