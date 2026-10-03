package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

// Child-process inputs for TestEnsureSiblingSerializesAcrossProcesses.
const (
	siblingCloneTargetEnv = "DONMAI_RUNNER_TEST_SIBLING_TARGET"
	siblingCloneURLEnv    = "DONMAI_RUNNER_TEST_SIBLING_URL"
)

// siblingCloneChild is the "sibling-clone" test role: one process provisioning
// a legacy sibling, holding its clone open for a moment so two processes
// started together overlap.
func siblingCloneChild() {
	siblingCloneHook = func() { time.Sleep(750 * time.Millisecond) }
	if err := provisionOneSibling(); err != nil {
		fmt.Fprintln(os.Stderr, "ensureSibling:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func provisionOneSibling() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return ensureSibling(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)), os.Getenv(siblingCloneTargetEnv), os.Getenv(siblingCloneURLEnv), "")
}

// TestEnsureSiblingSerializesAcrossProcesses pins the legacy placement's
// lock across processes: each session runs as its own process, so two
// sessions provisioning the same sibling first-time must not both clone into
// it. Two processes start together; both succeed, and the sibling is one
// clean clone.
func TestEnsureSiblingSerializesAcrossProcesses(t *testing.T) {
	target := filepath.Join(t.TempDir(), "shared-corpus")
	url := "file://" + namedBareRepo(t, "shared-corpus")
	processes := make([]*exec.Cmd, 2)
	outputs := make([]*strings.Builder, 2)
	for i := range processes {
		cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), testRoleEnv+"=sibling-clone", siblingCloneTargetEnv+"="+target, siblingCloneURLEnv+"="+url)
		outputs[i] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outputs[i], outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		processes[i] = cmd
	}
	for i, cmd := range processes {
		if err := cmd.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", i, err, outputs[i])
		}
	}
	if out, err := exec.Command("git", "-C", target, "rev-parse", "--is-shallow-repository").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" { //nolint:gosec // test-owned path
		t.Fatalf("sibling %s is not one clean shallow clone: %v %s", target, err, out)
	}
}

// TestRun_SiblingEnvBecomesContextLeavesOnAnAttestingExecutor pins the
// DONMAI_SIBLING_REPOS mapping for an executor that attests session-root-v1
// and isolated-read-only-v1: each entry of a single-repository work item
// becomes a read-only context leaf under the session root, beside the
// selected repository, so ../<name> resolves from it. The leaf is held
// read-only by the authority policy and nothing lands in the shared parent.
// An entry that cannot be cloned is skipped with a warning and recorded in
// the durable declaration; the session completes.
func TestRun_SiblingEnvBecomesContextLeavesOnAnAttestingExecutor(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Setenv(siblingReposEnv, "")
	qw := h.queuedWork("SIBLING-CONTEXT-LEAVES")
	qw.WorkType = "acceptance"
	qw.Env = map[string]string{siblingReposEnv: namedBareRepo(t, "ctx-corpus") + ", file:///nonexistent-runner-test/ctx-missing.git"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
	}
	if res.WorkareaRoot == "" || res.WorkareaRoot == res.WorktreePath || filepath.Dir(res.WorktreePath) != res.WorkareaRoot {
		t.Fatalf("WorkareaRoot = %q WorktreePath = %q; want the selected repository a leaf of the session root", res.WorkareaRoot, res.WorktreePath)
	}
	sibling := filepath.Join(res.WorktreePath, "..", "ctx-corpus")
	if _, err := os.Stat(filepath.Join(sibling, ".git")); err != nil {
		t.Fatalf("../ctx-corpus from the selected repository is not a clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(res.WorkareaRoot), "ctx-corpus")); !os.IsNotExist(err) {
		t.Fatalf("a sibling landed in the shared parent (stat err %v); want it only under the session root", err)
	}
	if skipped, err := h.runner.wt.SkippedRepositories(qw.SessionID); err != nil || !slices.Equal(skipped, []string{"ctx-missing"}) {
		t.Fatalf("skipped = %v, %v; want [ctx-missing]", skipped, err)
	}
	record, err := workarea.ReadDeclaration(workarea.RootPath(res.WorkareaRoot))
	if err != nil {
		t.Fatal(err)
	}
	if got := record.SkippedRepositoryNames(); !slices.Equal(got, []string{"ctx-missing"}) {
		t.Fatalf("durable skipped set = %v; want [ctx-missing]", got)
	}
	if !slices.ContainsFunc(res.PostSessionWarnings, func(w string) bool { return strings.Contains(w, `"ctx-missing" failed to clone`) }) {
		t.Fatalf("PostSessionWarnings = %q; want the skipped context repository named", res.PostSessionWarnings)
	}
	provider.mu.Lock()
	policy := provider.observed.RepositoryAuthority
	provider.mu.Unlock()
	if policy == nil || len(policy.ReadOnlyPaths) != 1 || filepath.Base(policy.ReadOnlyPaths[0]) != "ctx-corpus" {
		t.Fatalf("policy = %+v; want ctx-corpus held read-only", policy)
	}
}

// TestRun_SiblingEnvKeepsTheOriginalPlacementOnOtherExecutors pins the
// placement for an executor that cannot hold a declared read-only leaf (the
// stub attests neither session-root-v1 nor isolated-read-only-v1): the
// sibling is a shallow clone beside the session worktree, as the original
// ADR placement — and the end-to-end smoke — expect.
func TestRun_SiblingEnvKeepsTheOriginalPlacementOnOtherExecutors(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("SIBLING-LEGACY-PLACEMENT")
	t.Setenv(siblingReposEnv, "file://"+namedBareRepo(t, "legacy-corpus"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
	}
	sibling := filepath.Join(filepath.Dir(res.WorktreePath), "legacy-corpus")
	if out, err := exec.Command("git", "-C", sibling, "rev-parse", "--is-shallow-repository").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" { //nolint:gosec // test-owned path
		t.Fatalf("sibling beside the worktree at %s is not a shallow clone: %v %s", sibling, err, out)
	}
}

// TestRun_DeclarationIgnoresSiblingEnv pins precedence: a work item that
// declares its own repositories is authoritative. DONMAI_SIBLING_REPOS adds
// no context leaf to it and clones nothing beside it, and the session says
// the variable was ignored.
func TestRun_DeclarationIgnoresSiblingEnv(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Setenv(siblingReposEnv, "")
	qw := h.queuedWork("DECLARATION-OVER-SIBLING-ENV")
	qw.WorkType = "acceptance"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}
	qw.Env = map[string]string{siblingReposEnv: namedBareRepo(t, "ignored-corpus")}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
	}
	for _, place := range []string{res.WorkareaRoot, filepath.Dir(res.WorkareaRoot), filepath.Dir(res.WorktreePath)} {
		if _, err := os.Stat(filepath.Join(place, "ignored-corpus")); !os.IsNotExist(err) {
			t.Fatalf("ignored-corpus appeared in %s (stat err %v); want the variable ignored", place, err)
		}
	}
	if !slices.ContainsFunc(res.PostSessionWarnings, func(w string) bool { return strings.Contains(w, siblingReposEnv+" is ignored") }) {
		t.Fatalf("PostSessionWarnings = %q; want the ignored variable named", res.PostSessionWarnings)
	}
}

// TestSiblingContextDeclarationIsDerivedIdentically pins that the daemon
// preflight and the runner derive byte-identical declarations from the same
// work item, so the host-compiled authority digest matches what the runner
// materializes: the preflight decodes the work item from its operational
// payload, the runner holds it decoded, and both resolve the declaration with
// resolveRepositoryWorkarea. The variable rides the work item's env, which the
// operational payload carries.
func TestSiblingContextDeclarationIsDerivedIdentically(t *testing.T) {
	t.Setenv(siblingReposEnv, "")
	var qw QueuedWork
	qw.SessionID = "derive-identically"
	qw.WorkerID = "worker-1" // runner-only: the operational payload excludes it
	qw.Repository = "https://example.test/acme/web.git"
	qw.Ref = "main"
	qw.Env = map[string]string{siblingReposEnv: "https://example.test/acme/corpus.git#v1, https://example.test/acme/docs.git,https://example.test/acme/web.git"}

	payload, err := json.Marshal(ProjectOperationalPayload(qw))
	if err != nil {
		t.Fatal(err)
	}
	var preflightWork QueuedWork
	if err := json.Unmarshal(payload, &preflightWork); err != nil {
		t.Fatal(err)
	}
	provider := &workareaGateProvider{}
	preflight, preflightCaps, err := resolveRepositoryWorkarea(preflightWork, provider)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	runner, provisioned, runnerCaps, err := resolveRepositoryWorkareaDeclaration(qw, provider)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if preflight == nil || runner == nil || provisioned == nil {
		t.Fatalf("declarations: preflight %v runner %v provisioned %v; want all derived", preflight, runner, provisioned)
	}
	preflightBytes, _ := json.Marshal(preflight)
	runnerBytes, _ := json.Marshal(runner)
	if string(preflightBytes) != string(runnerBytes) {
		t.Fatalf("declarations differ:\npreflight %s\nrunner    %s", preflightBytes, runnerBytes)
	}
	if !reflect.DeepEqual(preflightCaps, runnerCaps) {
		t.Fatalf("capabilities differ: %+v vs %+v", preflightCaps, runnerCaps)
	}
	if !reflect.DeepEqual(ReconcileRepositorySandbox(agent.Spec{}, preflight), ReconcileRepositorySandbox(agent.Spec{}, runner)) {
		t.Fatal("the sandbox reconciliation differs between preflight and runner")
	}
	var names []string
	for _, repository := range runner.Repositories {
		names = append(names, fmt.Sprintf("%s:%s:%s:%s", repository.Name, repository.Role, repository.Authority, repository.Source.Ref))
	}
	want := []string{"web:primary:mutable:main", "corpus:context:read-only:v1", "docs:context:read-only:"}
	if !slices.Equal(names, want) {
		t.Fatalf("declared repositories = %v; want %v (the entry repeating the primary's leaf left out)", names, want)
	}

	if declaration, _, _, err := resolveRepositoryWorkareaDeclaration(qw, &stubWithoutWorkareaAttestation{}); err != nil || declaration != nil {
		t.Fatalf("non-attesting executor: declaration %v, err %v; want none (the original placement applies)", declaration, err)
	}
}

// stubWithoutWorkareaAttestation is an executor that attests no workarea
// protocol and no read-only enforcement.
type stubWithoutWorkareaAttestation struct{ workareaGateProvider }

func (*stubWithoutWorkareaAttestation) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{Name: agent.HarnessStub, HumanLabel: "No attestation", Family: agent.FamilyHarness, ContractABI: "workarea-gate/v1"}
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
