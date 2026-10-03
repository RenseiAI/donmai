package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestRunSkippedReadOnlyContextWarnsAndContinues is the regression for a
// declared read-only context repository that fails to clone: the session
// must keep running with a warning, not fail worktree provisioning.
func TestRunSkippedReadOnlyContextWarnsAndContinues(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("SKIPPED-CONTEXT")
	qw.WorkType = "acceptance"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "file:///nonexistent-context-repo.git"}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q); want completed with a warning", res.Status, res.FailureMode, res.Error)
	}
	found := false
	for _, warning := range res.PostSessionWarnings {
		if strings.Contains(warning, "context") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PostSessionWarnings = %q; want a warning naming the skipped context repository", res.PostSessionWarnings)
	}
	provider.mu.Lock()
	policy := provider.observed.RepositoryAuthority
	provider.mu.Unlock()
	if policy == nil {
		t.Fatal("provider observed no repository authority policy")
	}
	for _, path := range policy.ReadOnlyPaths {
		if strings.HasSuffix(path, "context") {
			t.Fatalf("skipped repository leaked into read-only policy paths: %v", policy.ReadOnlyPaths)
		}
	}
}

// TestRunFailedWritablePrimaryStillFails pins the other half of the
// contract: a writable primary repository that fails to clone must still
// fail the session.
func TestRunFailedWritablePrimaryStillFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	qw := h.queuedWork("FAILED-PRIMARY")
	qw.Repository = "file:///nonexistent-primary-repo.git"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	if res.Status != "failed" || res.FailureMode != FailureWorktreeProvision {
		t.Fatalf("result = status %q mode %q err %q; want failed/worktree-provision", res.Status, res.FailureMode, res.Error)
	}
	if res.WorktreePath != "" {
		t.Fatalf("failed provision left WorktreePath = %q; want empty", res.WorktreePath)
	}
}

// namedBareRepo is a cloneable bare repository at <tmp>/<name>.git.
func namedBareRepo(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", makeBareRepo(t), path).CombinedOutput(); err != nil { //nolint:gosec // test-owned paths
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return path
}

// TestRun_LegacySiblingEnvClonesNothing pins the removal of the legacy
// sibling environment-variable path. The legacy hook ran only for a work
// item without a repository declaration and cloned each listed repository
// next to the session worktree, as <worktree-parent>/<name>. Here the
// variable lists a repository that clones cleanly, and the work item has no
// declaration: nothing may appear next to the worktree.
func TestRun_LegacySiblingEnvClonesNothing(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("LEGACY-SIBLING-ENV")
	if qw.RepositoryDeclaration != nil {
		t.Fatal("the work item must carry no repository declaration: the legacy hook ran only without one")
	}
	t.Setenv("DONMAI_SIBLING_REPOS", namedBareRepo(t, "legacy-corpus"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" || res.WorktreePath == "" {
		t.Fatalf("Status = %q WorktreePath = %q (FailureMode=%q, Error=%q)", res.Status, res.WorktreePath, res.FailureMode, res.Error)
	}
	sibling := filepath.Join(filepath.Dir(res.WorktreePath), "legacy-corpus")
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Fatalf("legacy sibling clone at %s exists (stat err %v); the variable must clone nothing", sibling, err)
	}
}

// TestRepositoryAuthorityPolicy pins which declared repository may be
// missing a path when the sandbox authority policy is built: only one the
// worktree manager reported skipped, and only a non-selected read-only
// context repository. Any other missing path fails loudly instead of
// dropping that repository from the policy.
func TestRepositoryAuthorityPolicy(t *testing.T) {
	declaration, err := (&workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/web.git"}, Name: "web", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/corpus.git"}, Name: "corpus", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
			{Source: workarea.RepositorySource{Repository: "https://example.test/acme/docs.git"}, Name: "docs", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryReadOnly},
		},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"web": "/w/web", "corpus": "/w/corpus", "docs": "/w/docs"}
	without := func(name string) map[string]string {
		paths := map[string]string{}
		for k, v := range all {
			if k != name {
				paths[k] = v
			}
		}
		return paths
	}
	skippedSet := func(names ...string) map[string]struct{} {
		set := map[string]struct{}{}
		for _, name := range names {
			set[name] = struct{}{}
		}
		return set
	}
	cases := []struct {
		name         string
		paths        map[string]string
		skipped      map[string]struct{}
		wantReadOnly []string
		wantErrRepo  string
	}{
		{name: "every repository provisioned", paths: all, wantReadOnly: []string{"/w/corpus", "/w/docs"}},
		{name: "a recorded skip of a read-only context repository", paths: without("corpus"), skipped: skippedSet("corpus"), wantReadOnly: []string{"/w/docs"}},
		{name: "a read-only context repository missing without a recorded skip", paths: without("corpus"), wantErrRepo: "corpus"},
		{name: "a read-only secondary repository missing even when reported skipped", paths: without("docs"), skipped: skippedSet("docs"), wantErrRepo: "docs"},
		{name: "a mutable repository missing even when reported skipped", paths: without("web"), skipped: skippedSet("web"), wantErrRepo: "web"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := repositoryAuthorityPolicy(declaration, tc.paths, tc.skipped, "/w", "/w/web", "isolated-read-only-v1")
			if tc.wantErrRepo != "" {
				var contractErr *workarea.RepositoryContractError
				if !errors.As(err, &contractErr) || contractErr.Repository != tc.wantErrRepo || policy != nil {
					t.Fatalf("policy = %+v, err = %v; want a contract error naming %q and no policy", policy, err, tc.wantErrRepo)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !slices.Equal(policy.ReadOnlyPaths, tc.wantReadOnly) || !slices.Equal(policy.MutablePaths, []string{"/w/web"}) {
				t.Fatalf("policy read-only %v mutable %v; want read-only %v mutable [/w/web]", policy.ReadOnlyPaths, policy.MutablePaths, tc.wantReadOnly)
			}
		})
	}
}

// TestRun_PassingPathsUnchanged pins that the skip rule leaves the passing
// paths alone: a single-repository session, and a declared session whose
// read-only context repository clones, complete with no skip warning, and
// the declared session's policy holds every repository.
func TestRun_PassingPathsUnchanged(t *testing.T) {
	noSkipWarning := func(t *testing.T, res *Result) {
		t.Helper()
		for _, warning := range res.PostSessionWarnings {
			if strings.Contains(warning, "failed to clone") {
				t.Fatalf("PostSessionWarnings = %q; want no skip warning", res.PostSessionWarnings)
			}
		}
	}
	t.Run("single repository", func(t *testing.T) {
		h := newRunnerHarness(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, h.queuedWork("SINGLE-REPO"))
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run = %+v, %v; want completed", res, err)
		}
		noSkipWarning(t, res)
	})
	t.Run("declared session whose context clone succeeds", func(t *testing.T) {
		h := newRunnerHarness(t)
		provider := &workareaGateProvider{}
		if err := h.runner.registry.Register(provider); err != nil {
			t.Fatal(err)
		}
		qw := h.queuedWork("DECLARED-CONTEXT-OK")
		qw.WorkType = "acceptance"
		qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
				{Source: workarea.RepositorySource{Repository: namedBareRepo(t, "context-corpus")}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, qw)
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
		}
		noSkipWarning(t, res)
		if skipped, err := h.runner.wt.SkippedRepositories(qw.SessionID); err != nil || len(skipped) != 0 {
			t.Fatalf("skipped = %v, %v; want none", skipped, err)
		}
		provider.mu.Lock()
		policy := provider.observed.RepositoryAuthority
		provider.mu.Unlock()
		if policy == nil || len(policy.ReadOnlyPaths) != 1 || filepath.Base(policy.ReadOnlyPaths[0]) != "context" ||
			len(policy.MutablePaths) != 1 || filepath.Base(policy.MutablePaths[0]) != "primary" {
			t.Fatalf("policy = %+v; want the context repository read-only and the primary mutable", policy)
		}
	})
}
