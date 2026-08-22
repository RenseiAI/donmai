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

	"github.com/RenseiAI/donmai/runtime/workarea"
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

// newSessionLayout creates a worktree root plus the session-owned workarea
// <worktree-root>/<sessionID>/<repo-leaf> and returns the worktree root and
// the nested layout. This is the provisioned shape the runner hands to
// provisionSiblings.
func newSessionLayout(t *testing.T, sessionID string) (worktreeRoot string, layout workarea.Layout) {
	t.Helper()
	worktreeRoot = t.TempDir()
	layout, err := workarea.NewLayout(worktreeRoot, sessionID, "session-repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Repository.String(), 0o750); err != nil {
		t.Fatal(err)
	}
	return worktreeRoot, layout
}

func TestProvisionSiblingRepos(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the env spec; it may pre-populate the session root.
		setup func(t *testing.T, layout workarea.Layout) string
		check func(t *testing.T, worktreeRoot string, layout workarea.Layout)
	}{
		{
			name: "single URL clones shallow sibling inside the session root",
			setup: func(t *testing.T, _ workarea.Layout) string {
				url, _ := makeSiblingOrigin(t, "corpus.git")
				return url
			},
			check: func(t *testing.T, worktreeRoot string, layout workarea.Layout) {
				sib := filepath.Join(layout.Root.String(), "corpus")
				if _, err := os.Stat(filepath.Join(sib, ".git")); err != nil {
					t.Fatalf("sibling .git missing: %v", err)
				}
				if _, err := os.Stat(filepath.Join(sib, "README.md")); err != nil {
					t.Errorf("sibling content missing: %v", err)
				}
				if _, err := os.Stat(filepath.Join(sib, ".git", "shallow")); err != nil {
					t.Errorf("clone is not shallow (.git/shallow absent): %v", err)
				}
				// NEXT TO the selected repository, never inside it.
				if _, err := os.Stat(filepath.Join(layout.Repository.String(), "corpus")); !os.IsNotExist(err) {
					t.Errorf("sibling leaked inside the repository worktree: stat err = %v", err)
				}
				// And never as a GLOBAL peer under the worktree root, where a
				// second session would share it.
				if _, err := os.Stat(filepath.Join(worktreeRoot, "corpus")); !os.IsNotExist(err) {
					t.Errorf("sibling materialized as a global peer under the worktree root: stat err = %v", err)
				}
			},
		},
		{
			name: "two URLs provision two siblings",
			setup: func(t *testing.T, _ workarea.Layout) string {
				a, _ := makeSiblingOrigin(t, "corpus-a.git")
				b, _ := makeSiblingOrigin(t, "corpus-b.git")
				return a + ", " + b
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				for _, name := range []string{"corpus-a", "corpus-b"} {
					if _, err := os.Stat(filepath.Join(layout.Root.String(), name, ".git")); err != nil {
						t.Errorf("sibling %s missing: %v", name, err)
					}
				}
			},
		},
		{
			name: "hash ref clones the named branch",
			setup: func(t *testing.T, _ workarea.Layout) string {
				url, work := makeSiblingOrigin(t, "corpus.git")
				checkout(t, work, "docs-v2")
				pushSiblingCommit(t, work, url, "V2-MARKER.md")
				return url + "#docs-v2"
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				marker := filepath.Join(layout.Root.String(), "corpus", "V2-MARKER.md")
				if _, err := os.Stat(marker); err != nil {
					t.Errorf("branch marker missing (ref not honored): %v", err)
				}
			},
		},
		{
			name: "clone failure is non-fatal and later entries still provision",
			setup: func(t *testing.T, _ workarea.Layout) string {
				bad := "file://" + filepath.Join(t.TempDir(), "nonexistent.git")
				good, _ := makeSiblingOrigin(t, "corpus.git")
				return bad + "," + good
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				root := layout.Root.String()
				if _, err := os.Stat(filepath.Join(root, "corpus", ".git")); err != nil {
					t.Errorf("good sibling missing after earlier failure: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "nonexistent", ".git")); !os.IsNotExist(err) {
					t.Errorf("failed clone left a .git behind: stat err = %v", err)
				}
			},
		},
		{
			name: "unsafe names are skipped",
			setup: func(_ *testing.T, _ workarea.Layout) string {
				return "#branch-only, .., ., ,"
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				entries, err := os.ReadDir(layout.Root.String())
				if err != nil {
					t.Fatal(err)
				}
				want := filepath.Base(layout.Repository.String())
				if len(entries) != 1 || entries[0].Name() != want {
					t.Errorf("session root polluted: %v", entries)
				}
			},
		},
		{
			name: "target colliding with the selected repository is skipped",
			setup: func(t *testing.T, layout workarea.Layout) string {
				url, _ := makeSiblingOrigin(t, filepath.Base(layout.Repository.String())+".git")
				return url
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				if _, err := os.Stat(filepath.Join(layout.Repository.String(), ".git")); !os.IsNotExist(err) {
					t.Errorf("selected repository was clobbered by a sibling clone: stat err = %v", err)
				}
			},
		},
		{
			name: "existing dir without .git is left untouched",
			setup: func(t *testing.T, layout workarea.Layout) string {
				url, _ := makeSiblingOrigin(t, "corpus.git")
				writeFile(t, filepath.Join(layout.Root.String(), "corpus"), "keep.txt", "keep\n")
				return url
			},
			check: func(t *testing.T, _ string, layout workarea.Layout) {
				root := layout.Root.String()
				if _, err := os.Stat(filepath.Join(root, "corpus", "keep.txt")); err != nil {
					t.Errorf("pre-existing content deleted: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "corpus", ".git")); !os.IsNotExist(err) {
					t.Errorf("non-git dir was cloned into: stat err = %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktreeRoot, layout := newSessionLayout(t, "sess-fixture")
			spec := tt.setup(t, layout)
			provisionSiblingRepos(context.Background(), discardLogger(), spec, layout)
			tt.check(t, worktreeRoot, layout)
		})
	}
}

func TestProvisionSiblingReposFreshensExistingClone(t *testing.T) {
	_, layout := newSessionLayout(t, "sess-freshen")
	url, work := makeSiblingOrigin(t, "corpus.git")

	provisionSiblingRepos(context.Background(), discardLogger(), url, layout)
	marker := filepath.Join(layout.Root.String(), "corpus", "FRESH-MARKER.md")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker present before origin advanced: stat err = %v", err)
	}

	pushSiblingCommit(t, work, url, "FRESH-MARKER.md")
	provisionSiblingRepos(context.Background(), discardLogger(), url, layout)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("existing sibling was not freshened: %v", err)
	}
}

func TestProvisionSiblingReposFreshenFailureKeepsStaleCopy(t *testing.T) {
	_, layout := newSessionLayout(t, "sess-stale")
	url, _ := makeSiblingOrigin(t, "corpus.git")

	provisionSiblingRepos(context.Background(), discardLogger(), url, layout)

	// Remove the origin so the freshen pull fails; the stale sibling
	// must survive untouched.
	bare := url[len("file://"):]
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	provisionSiblingRepos(context.Background(), discardLogger(), url, layout)
	if _, err := os.Stat(filepath.Join(layout.Root.String(), "corpus", "README.md")); err != nil {
		t.Errorf("stale sibling copy lost after failed freshen: %v", err)
	}
}

func TestProvisionSiblingsReadsProcessEnv(t *testing.T) {
	r := &Runner{logger: discardLogger()}

	t.Run("unset env is a no-op", func(t *testing.T) {
		_, layout := newSessionLayout(t, "sess-env-unset")
		t.Setenv(siblingReposEnv, "")
		r.provisionSiblings(context.Background(), QueuedWork{}, layout)
		entries, err := os.ReadDir(layout.Root.String())
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("expected untouched session root, got %v", entries)
		}
	})

	t.Run("env value provisions siblings", func(t *testing.T) {
		_, layout := newSessionLayout(t, "sess-env-set")
		url, _ := makeSiblingOrigin(t, "corpus.git")
		t.Setenv(siblingReposEnv, url)
		r.provisionSiblings(context.Background(), QueuedWork{}, layout)
		if _, err := os.Stat(filepath.Join(layout.Root.String(), "corpus", ".git")); err != nil {
			t.Errorf("sibling missing: %v", err)
		}
	})
}

// TestProvisionSiblingReposConcurrentSessionsGetDistinctPaths is the V16
// control for the session-owned ownership boundary: two concurrent sessions sharing a
// worktree root and naming the SAME secondary repo must each materialize it
// inside their OWN session root, never as one global peer they share.
//
// RED without the nesting change: both sessions resolve
// <worktree-root>/corpus, so the two roots hold no clone at all and the
// per-session assertions fail.
func TestProvisionSiblingReposConcurrentSessionsGetDistinctPaths(t *testing.T) {
	worktreeRoot := t.TempDir()
	url, _ := makeSiblingOrigin(t, "corpus.git")

	layouts := make([]workarea.Layout, 0, 2)
	for _, sessionID := range []string{"session-a", "session-b"} {
		layout, err := workarea.NewLayout(worktreeRoot, sessionID, "session-repo")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(layout.Repository.String(), 0o750); err != nil {
			t.Fatal(err)
		}
		layouts = append(layouts, layout)
	}

	var wg sync.WaitGroup
	for _, layout := range layouts {
		wg.Add(1)
		go func(l workarea.Layout) {
			defer wg.Done()
			provisionSiblingRepos(context.Background(), discardLogger(), url, l)
		}(layout)
	}
	wg.Wait()

	seen := make(map[string]bool, len(layouts))
	for _, layout := range layouts {
		sib := filepath.Join(layout.Root.String(), "corpus")
		if _, err := os.Stat(filepath.Join(sib, ".git")); err != nil {
			t.Errorf("session %s has no session-owned sibling clone: %v", layout.Root, err)
		}
		if seen[sib] {
			t.Errorf("two sessions resolved the same sibling path %s", sib)
		}
		seen[sib] = true
	}
	// The shared worktree root must hold only the two session roots.
	if _, err := os.Stat(filepath.Join(worktreeRoot, "corpus")); !os.IsNotExist(err) {
		t.Errorf("sibling materialized as a global peer shared across sessions: stat err = %v", err)
	}
}

// TestProvisionSiblingReposFlatLayoutStillWorks pins that a retained FLAT
// workarea — where the repository clone IS the session directory — still
// materializes its context repos next to itself, so a session adopted from a
// pre-nesting binary keeps finding its corpus at ../<name>.
func TestProvisionSiblingReposFlatLayoutStillWorks(t *testing.T) {
	worktreeRoot := t.TempDir()
	layout, err := workarea.FlatLayout(worktreeRoot, "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Repository.String(), 0o750); err != nil {
		t.Fatal(err)
	}
	url, _ := makeSiblingOrigin(t, "corpus.git")

	provisionSiblingRepos(context.Background(), discardLogger(), url, layout)

	// Flat root == flat repository, so ../corpus is the worktree root peer —
	// the pre-nesting shape, unchanged.
	if _, err := os.Stat(filepath.Join(worktreeRoot, "legacy-session", "corpus", ".git")); err != nil {
		t.Errorf("flat-layout sibling missing: %v", err)
	}
}
