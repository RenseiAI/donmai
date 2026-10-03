package runner

import (
	"context"
	"os/exec"
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

// TestProvisionSiblingsEnvRemoved pins the removal of the legacy sibling
// environment-variable path: setting it must not materialize anything next
// to the worktree, and the runner no longer reads the variable at all.
func TestProvisionSiblingsEnvRemoved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	qw := h.queuedWork("LEGACY-SIBLING-ENV")
	t.Setenv("DONMAI_SIBLING_REPOS", "file:///nonexistent-sibling.git")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	for _, warning := range res.PostSessionWarnings {
		if strings.Contains(warning, "sibling") {
			t.Fatalf("legacy sibling path produced a warning: %q", res.PostSessionWarnings)
		}
	}
}
