package codeintelhost

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// runGitT runs git in dir for test fixture setup/assertions only; it is
// deliberately independent of the production runGit/buildLocalGitEnv path so
// these tests exercise GitFactory as a black box.
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git")
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newSourceRepo creates a local git repository with two commits and returns
// its directory plus both commit SHAs, oldest first.
func newSourceRepo(t *testing.T) (dir, firstSHA, secondSHA string) {
	t.Helper()
	dir = t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGitT(t, dir, "add", "a.go")
	runGitT(t, dir, "commit", "-q", "-m", "first")
	firstSHA = runGitT(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n\nfunc B() {}\n"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGitT(t, dir, "add", "b.go")
	runGitT(t, dir, "commit", "-q", "-m", "second")
	secondSHA = runGitT(t, dir, "rev-parse", "HEAD")
	return dir, firstSHA, secondSHA
}

func newTestFactory(t *testing.T, repos ...CatalogRepository) *GitFactory {
	t.Helper()
	cat, err := NewCatalog(repos)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return &GitFactory{Catalog: cat, StateDir: t.TempDir()}
}

func TestGitFactoryCreateExactRevision(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, _ := newSourceRepo(t)
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}

	caller, closer, err := factory.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if err := caller.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	workDir := filepath.Join(factory.StateDir, "workareas", binding.Fingerprint())
	if got := runGitT(t, workDir, "rev-parse", "HEAD"); got != firstSHA {
		t.Errorf("worktree HEAD = %q, want %q", got, firstSHA)
	}

	result, err := caller.Call(context.Background(), mcpserver.ToolGetRepoMap, []byte(`{}`))
	if err != nil {
		t.Fatalf("Call(af_code_get_repo_map) error = %v", err)
	}
	if result.IsError {
		t.Errorf("Call(af_code_get_repo_map) result.IsError = true: %+v", result)
	}
}

func TestGitFactoryRevisionUnavailable(t *testing.T) {
	t.Parallel()
	srcDir, _, _ := newSourceRepo(t)
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: strings.Repeat("d", 40),
	}

	_, _, err := factory.Create(context.Background(), binding)
	if !errors.Is(err, ErrRevisionUnavailable) {
		t.Errorf("Create() error = %v, want ErrRevisionUnavailable", err)
	}
}

func TestGitFactoryProjectMismatch(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, _ := newSourceRepo(t)
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-WRONG", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}

	_, _, err := factory.Create(context.Background(), binding)
	if !errors.Is(err, ErrProjectMismatch) {
		t.Errorf("Create() error = %v, want ErrProjectMismatch", err)
	}
}

func TestGitFactoryRepositoryNotFound(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, _ := newSourceRepo(t)
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "does-not-exist",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}

	_, _, err := factory.Create(context.Background(), binding)
	if !errors.Is(err, ErrRepositoryNotFound) {
		t.Errorf("Create() error = %v, want ErrRepositoryNotFound", err)
	}
}

// TestGitFactoryWorktreeRestartSafeReuseAndStaleRecreate exercises both halves
// of ensureWorktree's restart-safety contract using two separate GitFactory
// instances sharing one StateDir (simulating a process restart that finds an
// existing on-disk workarea): first, a workarea already at the exact
// requested revision is reused in place (an untracked marker file survives);
// second, a workarea whose on-disk HEAD has drifted away from the requested
// revision (simulated by manually checking out a different commit) is
// removed and recreated from scratch (the marker file is gone and HEAD is
// restored to the requested revision).
func TestGitFactoryWorktreeRestartSafeReuseAndStaleRecreate(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, secondSHA := newSourceRepo(t)
	stateDir := t.TempDir()
	repo := CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir}
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}
	workDir := filepath.Join(stateDir, "workareas", binding.Fingerprint())
	markerPath := filepath.Join(workDir, "MARKER")

	// First "process": creates the workarea fresh.
	factory1 := &GitFactory{Catalog: mustCatalog(t, repo), StateDir: stateDir}
	caller1, closer1, err := factory1.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create() [process 1] error = %v", err)
	}
	if err := caller1.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady() [process 1] error = %v", err)
	}
	if err := os.WriteFile(markerPath, []byte("marker"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Second "process": same binding, workarea already at the exact revision
	// -> must be reused in place (marker survives).
	factory2 := &GitFactory{Catalog: mustCatalog(t, repo), StateDir: stateDir}
	caller2, closer2, err := factory2.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create() [process 2, reuse] error = %v", err)
	}
	if err := caller2.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady() [process 2, reuse] error = %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker file missing after reuse (worktree was recreated instead of reused): %v", err)
	}
	// Deliberately do not Close closer2 here: it shares the same on-disk
	// workDir as closer1/closer3 (all three Create calls resolve the same
	// binding fingerprint), and Close physically removes that directory.
	// Closing it now would destroy the fixture the "stale drift" step below
	// still needs. All closers for this shared workDir are closed at the end.
	_ = closer2

	// Drift the on-disk workarea away from the requested revision, simulating
	// staleness the pool did not know about.
	runGitT(t, workDir, "checkout", "--detach", "-q", secondSHA)
	if got := runGitT(t, workDir, "rev-parse", "HEAD"); got != secondSHA {
		t.Fatalf("setup: expected drifted HEAD %q, got %q", secondSHA, got)
	}

	// Third "process": same binding (still requests firstSHA) -> stale
	// worktree must be removed and recreated at the exact requested revision.
	factory3 := &GitFactory{Catalog: mustCatalog(t, repo), StateDir: stateDir}
	caller3, closer3, err := factory3.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create() [process 3, stale] error = %v", err)
	}
	t.Cleanup(func() {
		if err := closer3.Close(); err != nil {
			t.Errorf("Close() [process 3] error = %v", err)
		}
	})
	if err := caller3.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady() [process 3, stale] error = %v", err)
	}
	if got := runGitT(t, workDir, "rev-parse", "HEAD"); got != firstSHA {
		t.Errorf("worktree HEAD after stale recreation = %q, want %q", got, firstSHA)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("marker file survived a stale-revision recreation (worktree was reused instead of recreated): err = %v", err)
	}

	if err := closer1.Close(); err != nil {
		t.Errorf("Close() [process 1, deferred] error = %v", err)
	}
}

// TestGitFactoryCloneFailureErrorNeverIncludesSource proves a failed Git
// operation's error text never includes the configured source, even though
// the source (an operator-configured clone URL) may itself carry a
// credential — mirroring the same "never echo the secret" contract
// NewCatalog's validateSource enforces at config-load time.
func TestGitFactoryCloneFailureErrorNeverIncludesSource(t *testing.T) {
	t.Parallel()
	const secretMarker = "with-secret-token-ABC123"
	secretSource := filepath.Join(t.TempDir(), "nonexistent", secretMarker, "repo.git")
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: secretSource})
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: fullObjectID("missing"),
	}

	_, _, err := factory.Create(context.Background(), binding)
	if err == nil {
		t.Fatal("Create() error = nil, want a clone failure (source does not exist)")
	}
	if strings.Contains(err.Error(), secretMarker) || strings.Contains(err.Error(), secretSource) {
		t.Errorf("Create() error = %q, must not echo the repository source", err.Error())
	}
}

// TestMirrorDirNameDistinctForCollidingSanitizedIDs proves mirrorDirName is
// collision-resistant for two ids that the former character-substituting
// sanitizeID scheme mapped to the identical directory name ("a/b" and "a_b"
// both sanitized to "a_b").
func TestMirrorDirNameDistinctForCollidingSanitizedIDs(t *testing.T) {
	t.Parallel()
	a := mirrorDirName("a/b")
	b := mirrorDirName("a_b")
	if a == b {
		t.Fatalf("mirrorDirName collided for distinct ids %q and %q: both produced %q", "a/b", "a_b", a)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Errorf("mirrorDirName() length = %d/%d, want 64 (sha256 hex)", len(a), len(b))
	}
}

// TestGitFactoryCollidingIDsProduceDistinctMirrors is the end-to-end version
// of the mirrorDirName collision-resistance property: two catalog
// repositories whose ids collided under the old sanitizeID scheme must
// resolve to distinct on-disk mirrors, each with its own repository's
// content and origin — never mixed.
func TestGitFactoryCollidingIDsProduceDistinctMirrors(t *testing.T) {
	t.Parallel()
	srcA, firstA, _ := newSourceRepo(t)
	srcB, firstB, _ := newSourceRepo(t)
	factory := newTestFactory(
		t,
		CatalogRepository{ID: "a/b", ProjectID: "proj-1", Source: srcA},
		CatalogRepository{ID: "a_b", ProjectID: "proj-1", Source: srcB},
	)
	bindingA := Binding{OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "a/b", RevisionKind: RevisionResolvedRef, Revision: firstA}
	bindingB := Binding{OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "a_b", RevisionKind: RevisionResolvedRef, Revision: firstB}

	_, closerA, err := factory.Create(context.Background(), bindingA)
	if err != nil {
		t.Fatalf("Create(a/b) error = %v", err)
	}
	t.Cleanup(func() { _ = closerA.Close() })
	_, closerB, err := factory.Create(context.Background(), bindingB)
	if err != nil {
		t.Fatalf("Create(a_b) error = %v", err)
	}
	t.Cleanup(func() { _ = closerB.Close() })

	mirrorA := filepath.Join(factory.StateDir, "mirrors", mirrorDirName("a/b"))
	mirrorB := filepath.Join(factory.StateDir, "mirrors", mirrorDirName("a_b"))
	if mirrorA == mirrorB {
		t.Fatal("colliding ids resolved to the same mirror directory")
	}
	originA := runGitT(t, mirrorA, "remote", "get-url", "origin")
	originB := runGitT(t, mirrorB, "remote", "get-url", "origin")
	if originA != srcA || originB != srcB {
		t.Errorf("mirror origins = (%q, %q), want (%q, %q) (colliding ids must not mix mirror content)",
			originA, originB, srcA, srcB)
	}
}

// TestGitFactoryExistingMirrorOriginMismatchFailsClosed proves that when a
// repository id's on-disk mirror already exists but its recorded origin no
// longer matches the catalog's currently configured source (e.g. an operator
// edited daemon.yaml without touching the mirror), Create fails closed with
// ErrMirrorOriginMismatch rather than silently reusing/serving the wrong
// remote's content — and that neither source URL is echoed in the error.
func TestGitFactoryExistingMirrorOriginMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, _ := newSourceRepo(t)
	otherSrcDir, _, _ := newSourceRepo(t)
	stateDir := t.TempDir()
	repo := CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir}
	binding := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}

	factory1 := &GitFactory{Catalog: mustCatalog(t, repo), StateDir: stateDir}
	_, closer, err := factory1.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create() [initial] error = %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	// Operator edits the catalog's source for repo-1 without touching the
	// existing on-disk mirror.
	drifted := CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: otherSrcDir}
	factory2 := &GitFactory{Catalog: mustCatalog(t, drifted), StateDir: stateDir}

	_, _, err = factory2.Create(context.Background(), binding)
	if !errors.Is(err, ErrMirrorOriginMismatch) {
		t.Errorf("Create() with drifted source error = %v, want ErrMirrorOriginMismatch", err)
	}
	if err != nil && (strings.Contains(err.Error(), srcDir) || strings.Contains(err.Error(), otherSrcDir)) {
		t.Errorf("Create() error = %q, must not echo either mirror source", err.Error())
	}
}

// TestGitFactoryLockForSerializesPerRepoNotGlobally proves lockFor returns
// the same *sync.Mutex for repeat calls with the same repository id, a
// different mutex for a different id, and that holding one repository's
// mutex never blocks another repository's.
func TestGitFactoryLockForSerializesPerRepoNotGlobally(t *testing.T) {
	t.Parallel()
	factory := &GitFactory{}
	muA1 := factory.lockFor("repo-a")
	muA2 := factory.lockFor("repo-a")
	muB := factory.lockFor("repo-b")

	if muA1 != muA2 {
		t.Error("lockFor(same id) returned different mutexes")
	}
	if muA1 == muB {
		t.Error("lockFor(different ids) returned the same mutex")
	}

	muA1.Lock()
	defer muA1.Unlock()

	// muB must be immediately acquirable while muA1 is held: a blocking
	// muB.Lock() here would hang the test forever if lockFor ever shared one
	// mutex across repositories, so TryLock (non-blocking) is both the
	// correct check and avoids an empty Lock/Unlock critical section.
	if !muB.TryLock() {
		t.Fatal("lockFor(repo-b) blocked on repo-a's held lock: different repositories must warm independently")
	}
	muB.Unlock()
}

// TestGitFactoryConcurrentWarmsSameRepoDoNotCorruptMirror races two Create
// calls for two different revisions of the SAME repository against a fresh
// (not-yet-mirrored) StateDir. Both must succeed with no clone/fetch
// corruption — proving lockFor's per-repository mutex actually serializes
// ensureMirror+ensureRevision end-to-end rather than merely existing.
func TestGitFactoryConcurrentWarmsSameRepoDoNotCorruptMirror(t *testing.T) {
	t.Parallel()
	srcDir, firstSHA, secondSHA := newSourceRepo(t)
	factory := newTestFactory(t, CatalogRepository{ID: "repo-1", ProjectID: "proj-1", Source: srcDir})

	binding1 := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: firstSHA,
	}
	binding2 := Binding{
		OrgID: "org-1", ProjectID: "proj-1", RepositoryPathID: "repo-1",
		RevisionKind: RevisionResolvedRef, Revision: secondSHA,
	}

	var wg sync.WaitGroup
	var err1, err2 error
	var closer1, closer2 io.Closer
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, closer1, err1 = factory.Create(context.Background(), binding1)
	}()
	go func() {
		defer wg.Done()
		_, closer2, err2 = factory.Create(context.Background(), binding2)
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("Create(binding1) error = %v", err1)
	}
	if err2 != nil {
		t.Fatalf("Create(binding2) error = %v", err2)
	}
	t.Cleanup(func() { _ = closer1.Close() })
	t.Cleanup(func() { _ = closer2.Close() })

	workDir1 := filepath.Join(factory.StateDir, "workareas", binding1.Fingerprint())
	workDir2 := filepath.Join(factory.StateDir, "workareas", binding2.Fingerprint())
	if got := runGitT(t, workDir1, "rev-parse", "HEAD"); got != firstSHA {
		t.Errorf("workDir1 HEAD = %q, want %q", got, firstSHA)
	}
	if got := runGitT(t, workDir2, "rev-parse", "HEAD"); got != secondSHA {
		t.Errorf("workDir2 HEAD = %q, want %q", got, secondSHA)
	}
}

func mustCatalog(t *testing.T, repos ...CatalogRepository) *Catalog {
	t.Helper()
	cat, err := NewCatalog(repos)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return cat
}
