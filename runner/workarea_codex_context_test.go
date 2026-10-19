package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	codexprovider "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// This file pins the declared public-context contract against the harness
// that attests read-only enforcement in production: a primary repository
// plus a read-only context repository must provision the nested session
// layout with the context beside the selected repository, hold the context
// read-only in the authority policy and in the harness sandbox translation,
// and never fail the session when the context clone fails.
//
// All three run hermetically through Runner.Run with git file URLs and a
// Spawn recorder carrying the harness's real production manifest. No live
// binary, network, or platform is required.

func codexContextHarness(t *testing.T) *manifestAttestingProvider {
	t.Helper()
	return &manifestAttestingProvider{
		workareaGateProvider: &workareaGateProvider{},
		manifest:             (&codexprovider.Provider{}).Manifest(),
	}
}

// TestRunCodexReadOnlyContextBindsAuthorityAndLayout drives a declared
// primary plus read-only context session through the production Run path:
// the nested layout is provisioned with the context beside the selected
// repository, the authority policy holds the context read-only, and the
// harness sandbox translation grants only the mutable root.
//
// RED: remove the manifest's read-only enforcement attestation and Run
// refuses with the enforcement-missing contract error before provisioning;
// mark the context mutable and the read-only assertions fail.
func TestRunCodexReadOnlyContextBindsAuthorityAndLayout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := codexContextHarness(t)
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Setenv(siblingReposEnv, "")
	qw := h.queuedWork("CODEX-CONTEXT-OK")
	qw.WorkType = "acceptance"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: makeBareRepo(t)}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q); want completed", res.Status, res.FailureMode, res.Error)
	}
	for _, warning := range res.PostSessionWarnings {
		if strings.Contains(warning, "failed to clone") {
			t.Fatalf("PostSessionWarnings = %q; want no skip warning when every clone succeeds", res.PostSessionWarnings)
		}
	}
	if res.WorkareaRoot == "" || res.WorkareaRoot == res.WorktreePath ||
		filepath.Base(res.WorktreePath) != "primary" || filepath.Dir(res.WorktreePath) != res.WorkareaRoot {
		t.Fatalf("result paths = root %q cwd %q; want the selected primary a leaf of the session root", res.WorkareaRoot, res.WorktreePath)
	}
	sibling := filepath.Join(res.WorktreePath, "..", "context")
	if _, err := os.Stat(filepath.Join(sibling, ".git")); err != nil {
		t.Fatalf("../context from the selected repository is not a clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(res.WorkareaRoot), "context")); !os.IsNotExist(err) {
		t.Fatalf("a context copy landed in the shared parent (stat err %v); want it only under the session root", err)
	}
	if skipped, err := h.runner.wt.SkippedRepositories(qw.SessionID); err != nil || len(skipped) != 0 {
		t.Fatalf("skipped = %v, %v; want none when every clone succeeds", skipped, err)
	}
	if _, err := workarea.ReadDeclaration(workarea.RootPath(res.WorkareaRoot)); err != nil {
		t.Fatalf("ReadDeclaration: %v", err)
	}

	provider.mu.Lock()
	observed := provider.observed
	provider.mu.Unlock()
	policy := observed.RepositoryAuthority
	if observed.Cwd != res.WorktreePath || policy == nil {
		t.Fatalf("provider observed CWD/policy = %q/%#v; want the selected leaf and a policy", observed.Cwd, observed.RepositoryAuthority)
	}
	if observed.SandboxLevel != agent.SandboxWorkspaceWrite {
		t.Fatalf("sandbox = %q; want workspace-write for a declared session", observed.SandboxLevel)
	}
	if policy.Protocol != string(workarea.ProtocolSessionRootV1) ||
		policy.Enforcement != string(workarea.RepositoryAuthorityIsolatedReadOnlyV1) ||
		policy.WorkareaRoot != res.WorkareaRoot || policy.SelectedPath != res.WorktreePath {
		t.Fatalf("policy binding = %#v; want session-root-v1 with isolated read-only enforcement bound to the session root", policy)
	}
	if len(policy.MutablePaths) != 1 || filepath.Base(policy.MutablePaths[0]) != "primary" {
		t.Fatalf("mutable paths = %v; want exactly the primary repository", policy.MutablePaths)
	}
	if len(policy.ReadOnlyPaths) != 1 || filepath.Base(policy.ReadOnlyPaths[0]) != "context" {
		t.Fatalf("read-only paths = %v; want exactly the context repository", policy.ReadOnlyPaths)
	}
	for _, path := range policy.MutablePaths {
		if filepath.Base(path) == "context" {
			t.Fatalf("context repository entered mutable paths %v; a write into it must fail", policy.MutablePaths)
		}
	}

	plan := codexprovider.NewSpawnPlan(observed)
	raw, ok := plan.TurnStart["sandboxPolicy"]
	if !ok {
		t.Fatalf("spawn plan has no sandboxPolicy; want the workspace-write policy with narrowed writable roots")
	}
	translated, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("sandboxPolicy = %T; want the workspace-write object", raw)
	}
	roots, ok := translated["writableRoots"].([]string)
	if !ok || len(roots) != 1 || roots[0] != policy.MutablePaths[0] {
		t.Fatalf("writableRoots = %#v; want only the declared mutable repository", raw)
	}
	for _, root := range roots {
		if root == policy.ReadOnlyPaths[0] {
			t.Fatal("read-only context entered the harness writable roots")
		}
	}
}

// TestRunCodexUnreachableContextWarnsAndContinues drives the same declared
// session with a context source that cannot be cloned: the session must
// still complete with a warning naming the skipped context repository, the
// skip must be recorded durably, and the authority policy must exclude the
// missing leaf instead of failing the session.
//
// RED: make a failed context clone fail provisioning and this Run returns
// failed/worktree-provision instead of completed with a warning.
func TestRunCodexUnreachableContextWarnsAndContinues(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := codexContextHarness(t)
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Setenv(siblingReposEnv, "")
	qw := h.queuedWork("CODEX-CONTEXT-SKIP")
	qw.WorkType = "acceptance"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "file:///nonexistent-context-repository.git"}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q); want completed with a warning, not a failed session", res.Status, res.FailureMode, res.Error)
	}
	found := false
	for _, warning := range res.PostSessionWarnings {
		if strings.Contains(warning, "context") && strings.Contains(warning, "failed to clone") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PostSessionWarnings = %q; want a warning naming the skipped context repository", res.PostSessionWarnings)
	}
	if res.WorkareaRoot == "" || res.WorkareaRoot == res.WorktreePath ||
		filepath.Base(res.WorktreePath) != "primary" || filepath.Dir(res.WorktreePath) != res.WorkareaRoot {
		t.Fatalf("result paths = root %q cwd %q; want the selected primary a leaf of the session root", res.WorkareaRoot, res.WorktreePath)
	}
	if _, err := os.Stat(filepath.Join(res.WorkareaRoot, "primary", ".git")); err != nil {
		t.Fatalf("primary leaf has no clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.WorkareaRoot, "context")); !os.IsNotExist(err) {
		t.Fatalf("partial skipped checkout left behind (stat err %v); want it removed", err)
	}
	skipped, err := h.runner.wt.SkippedRepositories(qw.SessionID)
	if err != nil || len(skipped) != 1 || skipped[0] != "context" {
		t.Fatalf("skipped = %v, %v; want [context]", skipped, err)
	}
	record, err := workarea.ReadDeclaration(workarea.RootPath(res.WorkareaRoot))
	if err != nil {
		t.Fatalf("ReadDeclaration: %v", err)
	}
	if got := record.SkippedRepositoryNames(); len(got) != 1 || got[0] != "context" {
		t.Fatalf("durable skipped set = %v; want [context]", got)
	}
	for _, repository := range record.Repositories {
		if repository.Name == "context" {
			t.Fatalf("skipped repository persisted in the declaration record: %#v", record.Repositories)
		}
	}

	provider.mu.Lock()
	policy := provider.observed.RepositoryAuthority
	provider.mu.Unlock()
	if policy == nil {
		t.Fatal("provider observed no repository authority policy")
	}
	if len(policy.MutablePaths) != 1 || filepath.Base(policy.MutablePaths[0]) != "primary" {
		t.Fatalf("mutable paths = %v; want exactly the primary repository", policy.MutablePaths)
	}
	if len(policy.ReadOnlyPaths) != 0 {
		t.Fatalf("read-only paths = %v; want the skipped context excluded, not failed", policy.ReadOnlyPaths)
	}
	for _, path := range append(append([]string(nil), policy.MutablePaths...), policy.ReadOnlyPaths...) {
		if filepath.Base(path) == "context" {
			t.Fatalf("skipped repository leaked into policy paths %v", append(policy.MutablePaths, policy.ReadOnlyPaths...))
		}
	}
}
