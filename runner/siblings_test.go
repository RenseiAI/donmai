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
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
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

// TestSiblingContextEligible is the gating table for the derived sibling
// declaration: only a plain single-repository, non-full-access headless work
// item derives. Every other shape keeps its current provisioning, so the
// declaration never changes where the worktree lands or which sandbox the
// session carries. Each row exercises the real siblingContextEligible, not a
// reimplementation: removing any gate (for example `qw.Mode == ""`)
// turns its row RED.
func TestSiblingContextEligible(t *testing.T) {
	t.Setenv(siblingReposEnv, "")
	siblingSpec := "https://example.test/acme/corpus.git"
	plain := func() QueuedWork {
		qw := QueuedWork{Env: map[string]string{siblingReposEnv: siblingSpec}}
		qw.QueuedWork = queuedWorkBase("sibling-eligible")
		return qw
	}
	withRepository := func(mutate func(*QueuedWork)) QueuedWork {
		qw := plain()
		qw.Repository = "https://example.test/acme/web.git"
		if mutate != nil {
			mutate(&qw)
		}
		return qw
	}
	tests := []struct {
		name  string
		build func() QueuedWork
		want  bool
	}{
		{name: "plain derives", build: func() QueuedWork { return withRepository(nil) }, want: true},
		{name: "ref-bearing derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.Ref = "main" })
		}, want: true},
		{name: "own declaration never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) {
				qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
					Protocol: workarea.ProtocolSessionRootV1,
					Repositories: []workarea.DeclaredRepositoryV1{
						{
							Source:    workarea.RepositorySource{Repository: "https://example.test/acme/web.git"},
							Name:      "web",
							Role:      workarea.RepositoryRolePrimary,
							Authority: workarea.RepositoryMutable,
						},
					},
				}
			})
		}, want: false},
		{name: "missing repository never derives", build: plain, want: false},
		{name: "interactive never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.Mode = interactiveRunMode })
		}, want: false},
		{name: "shared workarea never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.WorkareaMode = worktree.ModeShared })
		}, want: false},
		{name: "cache-seeded never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.CacheSeedID = "seed-1" })
		}, want: false},
		{name: "base-ref never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.BaseRef = "main" })
		}, want: false},
		{name: "pull-request never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) {
				qw.PullRequest = &workarea.PullRequestV1{Owner: "acme", Repo: "web", Number: 7, HeadSHA: "abc123"}
			})
		}, want: false},
		{name: "full-access never derives", build: func() QueuedWork {
			return withRepository(func(qw *QueuedWork) { qw.PermissionProfile = PermissionProfileAutonomous })
		}, want: false},
		{name: "missing sibling variable never derives", build: func() QueuedWork {
			qw := withRepository(nil)
			qw.Env = nil
			return qw
		}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := siblingContextEligible(tt.build()); got != tt.want {
				t.Errorf("siblingContextEligible = %v; want %v", got, tt.want)
			}
		})
	}
}

// TestSiblingContextDeclarationNeedsAnAttestingExecutor pins the second half
// of the derivation gate: even an eligible work item derives nothing on an
// executor that cannot hold a declared read-only leaf, and a full-access
// item derives nothing even on one that can.
func TestSiblingContextDeclarationNeedsAnAttestingExecutor(t *testing.T) {
	t.Setenv(siblingReposEnv, "")
	eligible := QueuedWork{Env: map[string]string{siblingReposEnv: "https://example.test/acme/corpus.git"}}
	eligible.QueuedWork = queuedWorkBase("sibling-attesting")
	eligible.Repository = "https://example.test/acme/web.git"
	if _, derived := effectiveRepositoryDeclaration(eligible, &workareaGateProvider{}); !derived {
		t.Fatal("eligible item on an attesting executor derived nothing; want a declaration")
	}
	if _, derived := effectiveRepositoryDeclaration(eligible, &stubWithoutWorkareaAttestation{}); derived {
		t.Fatal("eligible item on a non-attesting executor derived a declaration; want the original placement")
	}
	fullAccess := eligible
	fullAccess.PermissionProfile = PermissionProfileAutonomous
	if _, derived := effectiveRepositoryDeclaration(fullAccess, &workareaGateProvider{}); derived {
		t.Fatal("full-access item on an attesting executor derived a declaration; want the original placement")
	}
}

// TestRun_FullAccessSiblingEnvKeepsSandboxAndPlacement pins B1 end to end:
// the same autonomous full-access item, with and without the sibling
// variable, keeps full-access sandboxing and the original placement beside
// the worktree; a workspace-write item still derives the declaration.
func TestRun_FullAccessSiblingEnvKeepsSandboxAndPlacement(t *testing.T) {
	t.Setenv(siblingReposEnv, "")
	siblingURL := "file://" + namedBareRepo(t, "full-access-corpus")
	runWithSpec := func(t *testing.T, spec string, profile PermissionProfile) (*Result, agent.Spec) {
		t.Helper()
		h := newRunnerHarness(t)
		provider := &workareaGateProvider{}
		if err := h.runner.registry.Register(provider); err != nil {
			t.Fatal(err)
		}
		qw := h.queuedWork("FULLACCESS-SIBLING")
		qw.WorkType = "acceptance"
		qw.PermissionProfile = profile
		if spec != "" {
			qw.Env = map[string]string{siblingReposEnv: spec}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, qw)
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
		}
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return res, provider.observed
	}
	plainRes, plainSpec := runWithSpec(t, "", PermissionProfileAutonomous)
	siblingRes, siblingSpec := runWithSpec(t, siblingURL, PermissionProfileAutonomous)
	if siblingSpec.SandboxLevel != agent.SandboxFullAccess || plainSpec.SandboxLevel != agent.SandboxFullAccess {
		t.Fatalf("sandbox = %q without and %q with the variable; want full-access in both", plainSpec.SandboxLevel, siblingSpec.SandboxLevel)
	}
	for _, res := range []*Result{plainRes, siblingRes} {
		if res.WorkareaRoot != res.WorktreePath {
			t.Fatalf("WorkareaRoot = %q WorktreePath = %q; want the legacy layout (no declaration derived)", res.WorkareaRoot, res.WorktreePath)
		}
	}
	if siblingSpec.RepositoryAuthority != nil {
		t.Fatalf("authority = %+v; want none (no declaration derived)", siblingSpec.RepositoryAuthority)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(siblingRes.WorktreePath), "full-access-corpus", ".git")); err != nil {
		t.Fatalf("sibling beside the worktree missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plainRes.WorktreePath, "..", "full-access-corpus")); !os.IsNotExist(err) {
		t.Fatalf("sibling leaked into the plain run (stat err %v); want identical placement with and without the variable", err)
	}
	derivedRes, derivedSpec := runWithSpec(t, siblingURL, PermissionProfileWorkspaceWrite)
	if derivedRes.WorkareaRoot == derivedRes.WorktreePath || filepath.Dir(derivedRes.WorktreePath) != derivedRes.WorkareaRoot {
		t.Fatalf("WorkareaRoot = %q WorktreePath = %q; want the selected repository a leaf of the session root", derivedRes.WorkareaRoot, derivedRes.WorktreePath)
	}
	if derivedSpec.SandboxLevel != agent.SandboxWorkspaceWrite {
		t.Fatalf("sandbox = %q; want workspace-write for the derived declaration", derivedSpec.SandboxLevel)
	}
}
