package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

type branchWritingProvider struct {
	agent.Provider
	calls                        atomic.Int32
	expectedBase, expectedBranch string
	expectedPromptBase           string
}

func (p *branchWritingProvider) Manifest() agent.HarnessManifest {
	return p.Provider.(agent.HarnessProvider).Manifest()
}

func (p *branchWritingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.calls.Add(1)
	// Like the shipped headless stub, this in-process event fixture has no
	// native prompt-adaptation channel. Real git and Runner.Run remain active.
	prepared := spec
	if p.expectedPromptBase != "" && !strings.Contains(spec.Prompt, `the base branch is "`+p.expectedPromptBase+`"`) {
		return nil, fmt.Errorf("provider prompt omitted explicit PR base")
	}
	head, err := runGit(ctx, prepared.Cwd, gitIdentity{}, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != p.expectedBase {
		return nil, fmt.Errorf("provider did not start from configured base")
	}
	branch, err := runGit(ctx, prepared.Cwd, gitIdentity{}, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(branch) != "refs/heads/"+p.expectedBranch {
		return nil, fmt.Errorf("provider did not start on new work branch")
	}
	if err = os.WriteFile(filepath.Join(prepared.Cwd, "branch-proof.md"), []byte("Owned branch proof.\n"), 0o600); err != nil {
		return nil, err
	}
	handle := newScriptedHandle()
	handle.pushTurn(agent.InitEvent{SessionID: "owned-branch-fixture"}, agent.AssistantTextEvent{Text: "WORK_RESULT:passed\n"}, agent.ResultEvent{Success: true, Message: "done"})
	close(handle.events)
	return handle, nil
}

func TestBaseRefActualRunnerCreatesBranchAndExplicitBasePR(t *testing.T) {
	// No parallel: subprocess PATH/git config are scoped to this actual-runner fixture.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GIT_AUTHOR_NAME", "Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.invalid")
	h := newRunnerHarness(t)
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", "-c", "core.hooksPath=/dev/null")
		command.Args = append(command.Args, args...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned git: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mainBefore := git("--git-dir", h.bareRepo, "rev-parse", "refs/heads/main")
	seed := filepath.Join(t.TempDir(), "seed")
	git("clone", h.bareRepo, seed)
	git("-C", seed, "checkout", "-b", "release/next")
	if err := os.WriteFile(filepath.Join(seed, "base-only.md"), []byte("non-default base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("-C", seed, "add", "base-only.md")
	git("-C", seed, "commit", "-m", "non-default base")
	git("-C", seed, "push", "origin", "release/next")
	base := git("-C", seed, "rev-parse", "HEAD")
	bin := t.TempDir()
	script := `#!/bin/sh
dir="${0%/*}"
printf '%s\n' "$@" >> "$dir/calls"
if [ "$1" = 'pr' ] && [ "$2" = 'create' ]; then
 : > "$dir/created"
 printf 'https://github.com/example/project/pull/1\n'
 exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(filepath.Join(bin, "gh"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: RuntimeTransportLocalV2})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	provider := &branchWritingProvider{Provider: inner, expectedBase: base, expectedBranch: "work/session", expectedPromptBase: "release/next"}
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = registry
	h.runner.skipBackstop = false
	work := h.queuedWork("base-source")
	work.AuthToken = ""
	work.BaseRef = "release/next"
	work.Ref = ""
	work.Branch = "work/session"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := h.runner.Run(ctx, work)
	if err != nil {
		t.Fatalf("actual runner: %v (%+v)", err, result)
	}
	if provider.calls.Load() != 1 || result.Status != "completed" || result.PullRequestURL != "https://github.com/example/project/pull/1" || result.BackstopReport == nil || !result.BackstopReport.PRCreated {
		t.Fatalf("new-branch completion did not run: %+v", result)
	}
	calls, err := os.ReadFile(filepath.Join(bin, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--base\nrelease/next\n") || !strings.Contains(string(calls), "--head\nwork/session\n") {
		t.Fatal("actual PR backstop omitted explicit base/head")
	}
	if got := git("--git-dir", h.bareRepo, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatal("new-branch run changed remote main")
	}
	head := git("--git-dir", h.bareRepo, "rev-parse", "refs/heads/work/session")
	if head == base || head != result.CommitSHA {
		t.Fatal("new work was not pushed to the session branch")
	}
}

func TestBaseRefAbsentPreservesLegacyAmendRunner(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	h := newRunnerHarness(t)
	base, err := runGit(context.Background(), h.bareRepo, gitIdentity{}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	provider := &branchWritingProvider{Provider: inner, expectedBase: strings.TrimSpace(base), expectedBranch: "main"}
	registry := NewRegistry()
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = registry
	h.runner.skipBackstop = false
	work := h.queuedWork("legacy-amend")
	work.Ref = "main"
	work.Branch = "metadata-is-not-a-new-branch"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := h.runner.Run(ctx, work)
	if err != nil || result.Status != "completed" || provider.calls.Load() != 1 {
		t.Fatalf("legacy amend: err=%v result=%+v", err, result)
	}
	if result.PullRequestURL != "" || result.BackstopReport != nil {
		t.Fatal("legacy amend unexpectedly entered new-PR backstop")
	}
	after, err := runGit(ctx, h.bareRepo, gitIdentity{}, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(after) != strings.TrimSpace(base) {
		t.Fatal("legacy amend fixture changed remote base")
	}
}
