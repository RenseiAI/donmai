package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

func continueFixtureSHA() string {
	return "0123456789abcdef0123456789abcdef01234567"
}

func TestValidateContinuePullRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		record  *prompt.ContinuePullRequest
		wantErr bool
	}{
		{name: "nil is ordinary work", record: nil},
		{name: "valid", record: &prompt.ContinuePullRequest{Number: 7, HeadRef: "feature/x", HeadSha: continueFixtureSHA()}},
		{name: "zero number", record: &prompt.ContinuePullRequest{Number: 0, HeadRef: "feature/x", HeadSha: continueFixtureSHA()}, wantErr: true},
		{name: "negative number", record: &prompt.ContinuePullRequest{Number: -3, HeadRef: "feature/x", HeadSha: continueFixtureSHA()}, wantErr: true},
		{name: "empty branch", record: &prompt.ContinuePullRequest{Number: 7, HeadRef: "", HeadSha: continueFixtureSHA()}, wantErr: true},
		{name: "branch traversal", record: &prompt.ContinuePullRequest{Number: 7, HeadRef: "../evil", HeadSha: continueFixtureSHA()}, wantErr: true},
		{name: "short sha", record: &prompt.ContinuePullRequest{Number: 7, HeadRef: "feature/x", HeadSha: "abc123"}, wantErr: true},
		{name: "empty sha", record: &prompt.ContinuePullRequest{Number: 7, HeadRef: "feature/x"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateContinuePullRequest(tc.record); (err != nil) != tc.wantErr {
				t.Fatalf("validateContinuePullRequest() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestContinuePullRequestBranch(t *testing.T) {
	t.Parallel()
	if got := continuePullRequestBranch(nil); got != "" {
		t.Fatalf("continuePullRequestBranch(nil) = %q, want empty", got)
	}
	got := continuePullRequestBranch(&prompt.ContinuePullRequest{Number: 7, HeadRef: "feature/x", HeadSha: continueFixtureSHA()})
	if got != "feature/x" {
		t.Fatalf("continuePullRequestBranch() = %q, want feature/x", got)
	}
}

// TestCheckoutContinuePullRequest_ChecksOutHeadAtPinnedSHA proves the
// checkout: the branch the session owns is checked out at exactly the
// dispatched head commit, not at whatever the remote currently serves.
func TestCheckoutContinuePullRequest_ChecksOutHeadAtPinnedSHA(t *testing.T) {
	remote, repo := backstopRemoteFixture(t)
	// The pull request's head branch exists only on the remote, two commits
	// ahead of main — the same shape as a real PR head the clone never saw.
	gitRun(t, repo, "push", "-q", "origin", "origin/develop:refs/heads/continued/pr-12")
	head := gitRun(t, remote, "rev-parse", "refs/heads/continued/pr-12")
	// Move the local clone somewhere else first so the checkout must move it.
	gitRun(t, repo, "checkout", "-q", "main")

	cpr := &prompt.ContinuePullRequest{Number: 12, HeadRef: "continued/pr-12", HeadSha: head}
	if err := checkoutContinuePullRequest(context.Background(), repo, cpr); err != nil {
		t.Fatalf("checkoutContinuePullRequest: %v", err)
	}
	if got := gitRun(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %q, want dispatched head %q", got, head)
	}
	if got := gitRun(t, repo, "branch", "--show-current"); got != "continued/pr-12" {
		t.Fatalf("checked out branch = %q, want continued/pr-12", got)
	}
}

// TestCheckoutContinuePullRequest_RefusesWrongSHA proves the checkout fails
// when the pinned commit is not the branch's head: running against a moved
// head would attribute work to commits nobody chose.
func TestCheckoutContinuePullRequest_RefusesWrongSHA(t *testing.T) {
	remote, repo := backstopRemoteFixture(t)
	gitRun(t, repo, "push", "-q", "origin", "origin/develop:refs/heads/continued/pr-12")
	_ = remote
	head := gitRun(t, remote, "rev-parse", "refs/heads/continued/pr-12")
	other := gitRun(t, remote, "rev-parse", "refs/heads/main")
	if head == other {
		t.Fatal("fixture setup: heads must differ")
	}
	cpr := &prompt.ContinuePullRequest{Number: 12, HeadRef: "continued/pr-12", HeadSha: other}
	if err := checkoutContinuePullRequest(context.Background(), repo, cpr); err == nil {
		t.Fatal("checkoutContinuePullRequest with a stale head sha succeeded")
	}
}

// TestCheckoutContinuePullRequest_IgnoresSameNamedTag proves the checkout
// fetches the branch explicitly: a same-named remote tag pointing at a
// different commit must not satisfy the head pin.
func TestCheckoutContinuePullRequest_IgnoresSameNamedTag(t *testing.T) {
	remote, repo := backstopRemoteFixture(t)
	gitRun(t, repo, "push", "-q", "origin", "origin/develop:refs/heads/continued/pr-12")
	head := gitRun(t, remote, "rev-parse", "refs/heads/continued/pr-12")
	other := gitRun(t, remote, "rev-parse", "refs/heads/main")
	if head == other {
		t.Fatal("fixture setup: heads must differ")
	}
	// Point a same-named tag at the wrong commit: a checkout that fetches
	// the short name resolves the tag and compares against the wrong head.
	gitRun(t, repo, "-c", "tag.gpgsign=false", "tag", "continued/pr-12", other)
	gitRun(t, repo, "push", "-q", "origin", "tag", "continued/pr-12")
	if got := gitRun(t, remote, "rev-parse", "refs/tags/continued/pr-12"); got != other {
		t.Fatalf("remote tag = %q, want %q", got, other)
	}
	gitRun(t, repo, "checkout", "-q", "main")
	cpr := &prompt.ContinuePullRequest{Number: 12, HeadRef: "continued/pr-12", HeadSha: head}
	if err := checkoutContinuePullRequest(context.Background(), repo, cpr); err != nil {
		t.Fatalf("checkoutContinuePullRequest with a same-named tag: %v", err)
	}
	if got := gitRun(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %q, want dispatched head %q", got, head)
	}
}

// installProtectedBranchHook arms a pre-receive hook on the bare repository
// that rejects every push to branch with a policy message deliberately
// worded to trip the old diagnostics-text matcher ("non-fast-forward",
// "fetch first"): a policy rejection of an otherwise fast-forward push
// must not read as divergence.
func installProtectedBranchHook(t *testing.T, bare, branch string) {
	t.Helper()
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\n" +
		"while read old new ref; do\n" +
		"  if [ \"$ref\" = \"refs/heads/" + branch + "\" ]; then\n" +
		"    echo \"remote: error: refusing update to protected branch " + branch + " (non-fast-forward policy check: fetch first)\" 1>&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n" +
		"exit 0\n"
	//nolint:gosec // G306: a hook stub must carry the exec bit.
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
}

func TestContinueHeadDiverged(t *testing.T) {
	const branch = "continued/pr-12"

	t.Run("moved head is diverged", func(t *testing.T) {
		remote, repo := backstopRemoteFixture(t)
		gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
		gitRun(t, repo, "fetch", "-q", "origin", branch)
		gitRun(t, repo, "checkout", "-q", branch)
		writeFile(t, repo, "src/fix.go", "package fix\n")
		// Someone else pushes to the continued branch after dispatch.
		other := t.TempDir()
		//nolint:gosec // G204: test fixture, paths come from t.TempDir.
		if out, err := exec.Command("git", "clone", "-q", remote, other).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		gitRun(t, other, "config", "user.email", "test@example.com")
		gitRun(t, other, "config", "user.name", "test")
		gitRun(t, other, "config", "commit.gpgsign", "false")
		gitRun(t, other, "fetch", "-q", "origin", branch)
		gitRun(t, other, "checkout", "-q", branch)
		writeFile(t, other, "src/other.go", "package other\n")
		gitRun(t, other, "add", "-A")
		gitRun(t, other, "commit", "-q", "-m", "someone else")
		gitRun(t, other, "push", "-q", "origin", branch)
		if !continueHeadDiverged(context.Background(), repo, branch) {
			t.Fatal("continueHeadDiverged = false after the head moved; want true")
		}
	})

	t.Run("fast-forwardable head is not diverged", func(t *testing.T) {
		_, repo := backstopRemoteFixture(t)
		gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
		gitRun(t, repo, "fetch", "-q", "origin", branch)
		gitRun(t, repo, "checkout", "-q", branch)
		writeFile(t, repo, "src/fix.go", "package fix\n")
		if continueHeadDiverged(context.Background(), repo, branch) {
			t.Fatal("continueHeadDiverged = true with the remote head behind the session; want false")
		}
	})

	t.Run("policy rejection is not diverged", func(t *testing.T) {
		remote, repo := backstopRemoteFixture(t)
		gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
		installProtectedBranchHook(t, remote, branch)
		gitRun(t, repo, "fetch", "-q", "origin", branch)
		gitRun(t, repo, "checkout", "-q", branch)
		writeFile(t, repo, "src/fix.go", "package fix\n")
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", "session work")
		// The hook rejects the push with the old matcher's trigger words;
		// the ancestry probe must still say the head did not move.
		if out, err := runGit(context.Background(), repo, gitIdentity{}, "push", "origin", "HEAD:refs/heads/"+branch); err == nil {
			t.Fatal("hook setup: push succeeded, want the policy rejection")
		} else if !strings.Contains(out, "non-fast-forward") {
			t.Fatalf("push output = %q; want the hook's misleading wording present", out)
		}
		if continueHeadDiverged(context.Background(), repo, branch) {
			t.Fatal("continueHeadDiverged = true for a policy rejection; want false")
		}
	})

	t.Run("same-named tag does not shadow the branch", func(t *testing.T) {
		remote, repo := backstopRemoteFixture(t)
		// The continued branch matches the session checkout: the fetched
		// remote head is an ancestor of the session's HEAD, so the probe
		// must report no divergence.
		gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
		gitRun(t, repo, "fetch", "-q", "origin", branch)
		gitRun(t, repo, "checkout", "-q", branch)
		writeFile(t, repo, "src/fix.go", "package fix\n")
		// A tag with the same name as the branch points at a commit the
		// session does not carry: a probe that fetches the short name
		// resolves the tag first and misreads the branch as diverged.
		seedTree := gitRun(t, repo, "rev-parse", "HEAD^{tree}")
		tagTarget := gitRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit-tree", seedTree, "-m", "same-named tag")
		gitRun(t, repo, "-c", "tag.gpgsign=false", "tag", branch, tagTarget)
		gitRun(t, repo, "push", "-q", "origin", "tag", branch)
		if got := gitRun(t, remote, "rev-parse", "refs/tags/"+branch); got != tagTarget {
			t.Fatalf("remote tag = %q, want %q", got, tagTarget)
		}
		if continueHeadDiverged(context.Background(), repo, branch) {
			t.Fatal("continueHeadDiverged = true with a same-named tag; want false: the probe must fetch the branch, not the tag")
		}
	})

	t.Run("empty ref is not diverged", func(t *testing.T) {
		_, repo := backstopRemoteFixture(t)
		if continueHeadDiverged(context.Background(), repo, "") {
			t.Fatal("continueHeadDiverged with an empty ref = true; want false")
		}
	})

	t.Run("failed fetch is not diverged", func(t *testing.T) {
		// A probe that cannot read the remote proves nothing: an unknown
		// ref fails the fetch, and the failure must read as "not
		// diverged", never as divergence.
		_, repo := backstopRemoteFixture(t)
		if out, err := runGit(context.Background(), repo, gitIdentity{}, "fetch", "origin", continueBranchRef("continued/does-not-exist")); err == nil {
			t.Fatalf("fixture setup: fetch of a missing ref succeeded: %q", out)
		}
		if continueHeadDiverged(context.Background(), repo, "continued/does-not-exist") {
			t.Fatal("continueHeadDiverged = true for a failed fetch; want false")
		}
	})

	t.Run("unreadable ancestry is not diverged", func(t *testing.T) {
		// merge-base exits 128 when the ancestry question cannot even be
		// asked (here an unborn HEAD): a usage-level git failure is not
		// proof the remote head moved.
		_, repo := backstopRemoteFixture(t)
		gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/continued/pr-12")
		const orphan = "orphan-probe-branch"
		gitRun(t, repo, "checkout", "-q", "--orphan", orphan)
		gitRun(t, repo, "rm", "-q", "-rf", ".")
		if out, err := runGit(context.Background(), repo, gitIdentity{}, "merge-base", "--is-ancestor", "HEAD", "HEAD"); err == nil {
			t.Fatalf("fixture setup: merge-base on an unborn HEAD succeeded: %q", out)
		} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 128 {
			t.Fatalf("fixture setup: merge-base err = %v (%q); want exit 128", err, out)
		}
		if continueHeadDiverged(context.Background(), repo, "continued/pr-12") {
			t.Fatal("continueHeadDiverged = true with an unreadable HEAD; want false")
		}
	})
}

func TestContinueDivergedFlag(t *testing.T) {
	t.Parallel()
	if continueDiverged(nil) {
		t.Fatal("continueDiverged(nil) = true; want false")
	}
	if continueDiverged(&agent.BackstopReport{}) {
		t.Fatal("continueDiverged(empty) = true; want false")
	}
	// Diagnostics text alone — the old signal — must not count.
	if continueDiverged(&agent.BackstopReport{Diagnostics: ErrContinuePullRequestDiverged.Error() + ": something moved"}) {
		t.Fatal("continueDiverged(diagnostics-only) = true; want false: the flag decides, not text")
	}
	if !continueDiverged(&agent.BackstopReport{ContinueDiverged: true}) {
		t.Fatal("continueDiverged(flag) = false; want true")
	}
	if !continueDiverged(&agent.BackstopReport{Repositories: []agent.RepositoryBackstopReport{{Name: "primary", Report: agent.BackstopReport{ContinueDiverged: true}}}}) {
		t.Fatal("continueDiverged(aggregate) = false; want true: a declared session folds the flag")
	}
}

func TestContinuePullRequestURL(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: queuedWorkBase("CONT-URL")}
	qw.Repository = "https://github.com/example/project.git"
	qw.ContinuePullRequest = &prompt.ContinuePullRequest{Number: 7, HeadRef: "feature/x", HeadSha: continueFixtureSHA()}
	if got := continuePullRequestURL(context.Background(), qw, nil, ""); got != "https://github.com/example/project/pull/7" {
		t.Fatalf("continuePullRequestURL() = %q", got)
	}
	qw.Repository = "/tmp/not-a-forge"
	if got := continuePullRequestURL(context.Background(), qw, nil, ""); got != "" {
		t.Fatalf("continuePullRequestURL(non-github) = %q, want empty", got)
	}
}

func continueFixtureBranch(t *testing.T, repo, branch string) string {
	t.Helper()
	gitRun(t, repo, "checkout", "-q", "-b", branch, "origin/main")
	return gitRun(t, repo, "rev-parse", "HEAD")
}

func commitFixtureFiles(t *testing.T, repo string, files map[string]string, message string) string {
	t.Helper()
	for path, body := range files {
		writeFile(t, repo, path, body)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", message)
	return gitRun(t, repo, "rev-parse", "HEAD")
}

func advanceMainAndMerge(t *testing.T, cwd string, pushRefs ...string) string {
	t.Helper()
	remote := strings.TrimSpace(gitRun(t, cwd, "remote", "get-url", "origin"))
	other := t.TempDir()
	//nolint:gosec // G204: test fixture, paths come from t.TempDir.
	if out, err := exec.Command("git", "clone", "-q", remote, other).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	gitRun(t, other, "config", "user.email", "test@example.com")
	gitRun(t, other, "config", "user.name", "test")
	gitRun(t, other, "config", "commit.gpgsign", "false")
	writeFile(t, other, filepath.Join("upstream", filepath.Base(other)+".txt"), "main moved\n")
	gitRun(t, other, "add", "-A")
	gitRun(t, other, "commit", "-q", "-m", "advance main")
	gitRun(t, other, "push", "-q", "origin", "main")

	// The merge below creates a commit in cwd, so cwd needs its own
	// identity: CI has no global git identity to fall back on.
	gitRun(t, cwd, "config", "user.email", "test@example.com")
	gitRun(t, cwd, "config", "user.name", "test")
	gitRun(t, cwd, "config", "commit.gpgsign", "false")
	gitRun(t, cwd, "fetch", "-q", "origin", "main")
	gitRun(t, cwd, "merge", "--no-ff", "-m", "merge origin/main", "origin/main")
	if len(pushRefs) > 0 {
		args := []string{"push", "-q", "origin"}
		for _, ref := range pushRefs {
			args = append(args, "HEAD:"+ref)
		}
		gitRun(t, cwd, args...)
	}
	return gitRun(t, cwd, "rev-parse", "HEAD")
}

func TestInspectContinueRange(t *testing.T) {
	const branch = "continued/pr-12"
	cases := []struct {
		name             string
		arrange          func(t *testing.T) (repo, startHead, newHead string)
		wantCommitCount  int
		wantDelivers     bool
		wantScratchPaths []string
	}{
		{
			name: "merge only head",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				gitRun(t, repo, "merge", "--no-ff", "-m", "merge develop", "origin/develop")
				return repo, startHead, gitRun(t, repo, "rev-parse", "HEAD")
			},
			wantCommitCount: 1,
		},
		{
			name: "scratch only commit",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				newHead = commitFixtureFiles(t, repo, map[string]string{".scratch/comments.json": "{}\n"}, "scratch only")
				return repo, startHead, newHead
			},
			wantCommitCount:  1,
			wantScratchPaths: []string{".scratch/comments.json"},
		},
		{
			name: "code commit plus merge",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				commitFixtureFiles(t, repo, map[string]string{"fix.go": "package fix\n"}, "code change")
				gitRun(t, repo, "merge", "--no-ff", "-m", "merge develop", "origin/develop")
				return repo, startHead, gitRun(t, repo, "rev-parse", "HEAD")
			},
			wantCommitCount: 2,
			wantDelivers:    true,
		},
		{
			name: "code commit with scratch",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				newHead = commitFixtureFiles(t, repo, map[string]string{
					"fix.go":                "package fix\n",
					".scratch/gate-full.md": "notes\n",
				}, "code and scratch")
				return repo, startHead, newHead
			},
			wantCommitCount:  1,
			wantScratchPaths: []string{".scratch/gate-full.md"},
		},
		{
			name: "nested scratch named dir is code",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				newHead = commitFixtureFiles(t, repo, map[string]string{"docs/.scratch/notes.md": "notes\n"}, "nested same-named dir")
				return repo, startHead, newHead
			},
			wantCommitCount: 1,
			wantDelivers:    true,
		},
		{
			name: "tracked agent configuration is code",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				newHead = commitFixtureFiles(t, repo, map[string]string{
					".claude/settings.json":           "{}\n",
					".claude/skills/release/SKILL.md": "# release\n",
					".codex/config.toml":              "model = \"x\"\n",
				}, "agent configuration")
				return repo, startHead, newHead
			},
			wantCommitCount: 1,
			wantDelivers:    true,
		},
		{
			name: "pi session state only",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				newHead = commitFixtureFiles(t, repo, map[string]string{".pi/session.jsonl": "{}\n"}, "pi state")
				return repo, startHead, newHead
			},
			wantCommitCount:  1,
			wantScratchPaths: []string{".pi/session.jsonl"},
		},
		{
			name: "removing earlier scratch is cleanup",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				continueFixtureBranch(t, repo, branch)
				startHead = commitFixtureFiles(t, repo, map[string]string{".scratch/old.md": "earlier notes\n"}, "earlier scratch")
				gitRun(t, repo, "rm", "-q", ".scratch/old.md")
				newHead = commitFixtureFiles(t, repo, map[string]string{"fix.go": "package fix\n"}, "fix and drop the scratch")
				return repo, startHead, newHead
			},
			wantCommitCount: 1,
			wantDelivers:    true,
		},
		{
			name: "merge does not re-attribute earlier scratch",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				continueFixtureBranch(t, repo, branch)
				startHead = commitFixtureFiles(t, repo, map[string]string{".scratch/old.md": "earlier notes\n"}, "earlier scratch")
				commitFixtureFiles(t, repo, map[string]string{"fix.go": "package fix\n"}, "code change")
				gitRun(t, repo, "merge", "--no-ff", "-m", "merge develop", "origin/develop")
				return repo, startHead, gitRun(t, repo, "rev-parse", "HEAD")
			},
			wantCommitCount: 2,
			wantDelivers:    true,
		},
		{
			name: "merge adding its own scratch",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				commitFixtureFiles(t, repo, map[string]string{"fix.go": "package fix\n"}, "code change")
				gitRun(t, repo, "merge", "--no-ff", "--no-commit", "origin/develop")
				writeFile(t, repo, ".scratch/merge.md", "notes\n")
				gitRun(t, repo, "add", "-f", ".scratch/merge.md")
				gitRun(t, repo, "commit", "-q", "-m", "merge develop with notes")
				return repo, startHead, gitRun(t, repo, "rev-parse", "HEAD")
			},
			wantCommitCount:  2,
			wantScratchPaths: []string{".scratch/merge.md"},
		},
		{
			name: "no new commits",
			arrange: func(t *testing.T) (repo, startHead, newHead string) {
				_, repo = backstopRemoteFixture(t)
				startHead = continueFixtureBranch(t, repo, branch)
				return repo, startHead, startHead
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, startHead, newHead := tc.arrange(t)
			got, err := inspectContinueRange(context.Background(), repo, startHead, newHead)
			if err != nil {
				t.Fatalf("inspectContinueRange: %v", err)
			}
			if got.commitCount != tc.wantCommitCount {
				t.Fatalf("commitCount = %d, want %d", got.commitCount, tc.wantCommitCount)
			}
			if got.delivers() != tc.wantDelivers {
				t.Fatalf("delivers = %v, want %v (%+v)", got.delivers(), tc.wantDelivers, got)
			}
			if strings.Join(got.scratchPaths, ",") != strings.Join(tc.wantScratchPaths, ",") {
				t.Fatalf("scratchPaths = %v, want %v", got.scratchPaths, tc.wantScratchPaths)
			}
		})
	}
}

// TestRunBackstop_ContinueModePushesHeadWithoutNewPR proves the push
// target and the no-new-PR rule: the backstop pushes HEAD to the
// continued branch and reports the envelope URL without invoking gh.
func TestRunBackstop_ContinueModePushesHeadWithoutNewPR(t *testing.T) {
	remote, repo := backstopRemoteFixture(t)
	const branch = "continued/pr-12"
	gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
	head := gitRun(t, remote, "rev-parse", "refs/heads/"+branch)
	gitRun(t, repo, "fetch", "-q", "origin", branch)
	gitRun(t, repo, "checkout", "-q", branch)
	writeFile(t, repo, "src/fix.go", "package fix\n")

	// gh must never open a PR in continue mode: fail loudly if it tries
	// anything but the read-only visibility probe (`repo view` answers
	// PRIVATE so the commit keeps its identifier; every other gh
	// invocation exits 99).
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nif [ \"$1\" = \"repo\" ]; then echo PRIVATE; exit 0; fi\necho 'gh must not run in continue mode' 1>&2\nexit 99\n"
	//nolint:gosec // G306: a stub executable must carry the exec bit.
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write gh guard stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qw := QueuedWork{QueuedWork: queuedWorkBase("CONT-PUSH")}
	qw.Repository = "https://github.com/example/project.git"
	qw.ContinuePullRequest = &prompt.ContinuePullRequest{Number: 12, HeadRef: branch, HeadSha: head}
	res := &Result{}
	res.WorktreePath = repo
	res.PullRequestURL = "https://github.com/example/project/pull/12"

	report := minimalRunner(t).runBackstop(context.Background(), qw, branch, res, nil)
	if report.Diagnostics != "" {
		t.Fatalf("backstop diagnostics = %q", report.Diagnostics)
	}
	if !report.Pushed {
		t.Fatal("expected Pushed=true")
	}
	if report.PRCreated {
		t.Fatal("continue mode must never create a pull request")
	}
	if report.PRURL != res.PullRequestURL {
		t.Fatalf("report.PRURL = %q, want envelope %q", report.PRURL, res.PullRequestURL)
	}
	if got := gitRun(t, remote, "rev-parse", "refs/heads/"+branch); got != gitRun(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("remote head %q is not the committed work", got)
	}
}

// TestRunBackstop_ContinueModeRefusesDivergence proves the divergence
// refusal: when the continued branch moved on the remote, the backstop
// reports the typed reason instead of forcing the push.
func TestRunBackstop_ContinueModeRefusesDivergence(t *testing.T) {
	remote, repo := backstopRemoteFixture(t)
	const branch = "continued/pr-12"
	gitRun(t, repo, "push", "-q", "origin", "origin/main:refs/heads/"+branch)
	head := gitRun(t, remote, "rev-parse", "refs/heads/"+branch)
	gitRun(t, repo, "fetch", "-q", "origin", branch)
	gitRun(t, repo, "checkout", "-q", branch)
	writeFile(t, repo, "src/fix.go", "package fix\n")
	// Someone else pushes to the continued branch after dispatch.
	other := t.TempDir()
	//nolint:gosec // G204: test fixture, paths come from t.TempDir.
	if out, err := exec.Command("git", "clone", "-q", remote, other).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	gitRun(t, other, "config", "user.email", "test@example.com")
	gitRun(t, other, "config", "user.name", "test")
	gitRun(t, other, "config", "commit.gpgsign", "false")
	gitRun(t, other, "fetch", "-q", "origin", branch)
	gitRun(t, other, "checkout", "-q", branch)
	writeFile(t, other, "src/other.go", "package other\n")
	gitRun(t, other, "add", "-A")
	gitRun(t, other, "commit", "-q", "-m", "someone else")
	gitRun(t, other, "push", "-q", "origin", branch)

	qw := QueuedWork{QueuedWork: queuedWorkBase("CONT-DIVERGE")}
	qw.Repository = "https://github.com/example/project.git"
	qw.ContinuePullRequest = &prompt.ContinuePullRequest{Number: 12, HeadRef: branch, HeadSha: head}
	res := &Result{}
	res.WorktreePath = repo

	report := minimalRunner(t).runBackstop(context.Background(), qw, branch, res, nil)
	if report.Pushed {
		t.Fatal("diverged continue mode must not push")
	}
	if !strings.Contains(report.Diagnostics, ErrContinuePullRequestDiverged.Error()) {
		t.Fatalf("diagnostics = %q, want typed divergence reason", report.Diagnostics)
	}
	if !report.ContinueDiverged {
		t.Fatal("report.ContinueDiverged = false; want the typed divergence flag set")
	}
}

// TestRun_ContinueModeAcceptsMatchingAmendRef proves the cross-PR contract
// through the production entry point: the platform keeps sending the ref
// pin alongside the continued record, so a ref naming the continued head
// branch runs to completion on that branch.
func TestRun_ContinueModeAcceptsMatchingAmendRef(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		continueRef:    true,
		backstop:       true,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if res.PullRequestURL != "https://github.com/example/repo/pull/12" {
		t.Fatalf("PullRequestURL = %q; want the continued pull request", res.PullRequestURL)
	}
	if res.BackstopReport == nil || !res.BackstopReport.Pushed {
		t.Fatalf("BackstopReport = %+v; want the backstop to have pushed", res.BackstopReport)
	}
	remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
	if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("continued head %q does not carry the session commit", remoteHead)
	}
	if got := gitRun(t, res.WorktreePath, "branch", "--show-current"); got != "continued/pr-12" {
		t.Fatalf("checked out branch = %q; want the continued head branch", got)
	}
}

// TestRun_ContinueModeRefusesMismatchedAmendRef proves the typed refusal
// through the production entry point: a ref naming a different branch than
// the continued head fails worktree provisioning.
func TestRun_ContinueModeRefusesMismatchedAmendRef(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		ref:            "some-other-branch",
		turns:          []verdictScriptTurn{{text: "WORK_RESULT:passed"}},
	})
	if res.Status != "failed" || res.FailureMode != FailureWorktreeProvision {
		t.Fatalf("Status = %q (%s: %s); want failed worktree-provision", res.Status, res.FailureMode, res.Error)
	}
	if !strings.Contains(res.Error, "differs from the dispatched amend ref") {
		t.Fatalf("Error = %q; want the typed amend-ref refusal", res.Error)
	}
}

// TestRun_ContinueModePushesToTheContinuedBranch proves the push target
// through the production entry point: the session's commit lands on the
// continued head branch, never on a fresh agent/<session> branch.
func TestRun_ContinueModePushesToTheContinuedBranch(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		backstop:       true,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	refs := gitRun(t, res.WorktreePath, "ls-remote", "origin")
	if strings.Contains(refs, "refs/heads/agent/test-session-MANIFEST-FOLLOWUP") {
		t.Fatalf("a fresh session branch was created; want the push on the continued branch only:\n%s", refs)
	}
	remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
	if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("continued head %q does not carry the session commit", remoteHead)
	}
}

// TestRun_ContinueModeNoNewCommitIsNotDelivered proves the delivery rule
// through the production entry point: an agent that changes nothing ends
// unfinished against the continued pull request instead of completed.
func TestRun_ContinueModeNoNewCommitIsNotDelivered(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns:          []verdictScriptTurn{{text: "nothing to do"}},
	})
	if res.Status == "completed" && res.PullRequestURL == "https://github.com/example/repo/pull/12" {
		t.Fatalf("a no-new-commit continue run was reported delivered: %+v", res)
	}
	if res.PullRequestURL != "https://github.com/example/repo/pull/12" {
		t.Fatalf("PullRequestURL = %q; want the continued pull request under test", res.PullRequestURL)
	}
}

func TestRun_ContinueModeMergeOnlyHeadHasNoCodeChange(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			during: func(t *testing.T, cwd string) {
				advanceMainAndMerge(t, cwd, "refs/heads/continued/pr-12")
			},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureBackstop)
	}
	want := "continued pull request #12 has no code change since dispatch (only merges or scratch files)"
	if res.Error != want {
		t.Fatalf("Error = %q, want %q", res.Error, want)
	}
	remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
	if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("continued head %q does not carry the merge-only head", remoteHead)
	}
}

func TestRun_ContinueModeScratchCommitsFailByPath(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantPaths string
	}{
		{
			name:      "scratch only commit",
			files:     map[string]string{".scratch/comments.json": "{}\n"},
			wantPaths: ".scratch/comments.json",
		},
		{
			name: "code and scratch commit",
			files: map[string]string{
				"fix.go":                "package fix\n",
				".scratch/gate-full.md": "notes\n",
			},
			wantPaths: ".scratch/gate-full.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := runScriptedSession(t, scriptedSession{
				workType:       "development",
				skipSteering:   true,
				repository:     "https://github.com/example/repo",
				continueNumber: 12,
				turns: []verdictScriptTurn{{
					manifest: passedManifest,
					text:     "WORK_RESULT:passed",
					during: func(t *testing.T, cwd string) {
						for path, body := range tc.files {
							writeFile(t, cwd, path, body)
						}
						// The commit below runs in a fresh clone without
						// a global identity on CI; pass it inline.
						gitRun(t, cwd, "add", "-f", "-A")
						gitRun(t, cwd, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "scratch commit")
						gitRun(t, cwd, "push", "-q", "origin", "HEAD:refs/heads/continued/pr-12")
					},
				}},
			})
			if res.Status != "failed" || res.FailureMode != FailureBackstop {
				t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureBackstop)
			}
			want := "continued pull request #12 commits scratch paths since dispatch: .agent/state.json, .agent/turn-result.json, " + tc.wantPaths
			if res.Error != want {
				t.Fatalf("Error = %q, want %q", res.Error, want)
			}
			remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
			if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
				t.Fatalf("continued head %q does not carry the committed scratch path", remoteHead)
			}
		})
	}
}

// TestRun_ContinueModeDraftIsNotDelivered proves the draft rule through the
// production entry point: a draft continued pull request never reports the
// run delivered. The session does deliver a new commit onto the continued
// head, so the no-new-commit rule passes and only the draft rule can fail
// the run.
func TestRun_ContinueModeDraftIsNotDelivered(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		backstop:       true,
		draft:          func(context.Context, string, string) (bool, error) { return true, nil },
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed backstop-failed", res.Status, res.FailureMode, res.Error)
	}
	if !strings.Contains(res.Error, "is still a draft") {
		t.Fatalf("Error = %q; want the draft refusal", res.Error)
	}
	// The commit really landed on the continued head: the draft rule, not
	// a missing commit, is what failed the run.
	remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
	if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("continued head %q does not carry the session commit", remoteHead)
	}
}

// TestRun_ContinueModeDivergedHeadIsTyped proves the divergence outcome
// through the production entry point: the agent commits, then someone else
// pushes to the continued head branch mid-session. The push is refused as
// a non-fast-forward, and the terminal result — and the status the
// platform receives — carry the typed divergence failure, not a
// "no new commit" reading of the unpushed head.
func TestRun_ContinueModeDivergedHeadIsTyped(t *testing.T) {
	const branch = "continued/pr-12"
	var moved string
	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		backstop:       true,
		platform:       platform,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
			during: func(t *testing.T, cwd string) {
				// The agent commits its work locally.
				gitRun(t, cwd, "add", "fix.go")
				gitRun(t, cwd, "-c", "user.name=agent", "-c", "user.email=agent@example.com", "commit", "-q", "-m", "agent work")
				// Someone else then pushes a sibling of the session's
				// commit onto the continued head branch.
				tree := gitRun(t, cwd, "rev-parse", "HEAD~1^{tree}")
				other := gitRun(t, cwd, "-c", "user.name=other", "-c", "user.email=other@example.com", "commit-tree", tree, "-p", "HEAD~1", "-m", "someone else")
				gitRun(t, cwd, "push", "-q", "origin", other+":refs/heads/"+branch)
				moved = other
			},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureContinuePullRequestDiverged {
		t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureContinuePullRequestDiverged)
	}
	if !strings.Contains(res.Error, ErrContinuePullRequestDiverged.Error()) {
		t.Fatalf("Error = %q; want the typed divergence reason", res.Error)
	}
	if strings.Contains(res.Error, "no new commit") {
		t.Fatalf("Error = %q; the session committed, so it must not read as no new commit", res.Error)
	}
	if res.BackstopReport == nil || !res.BackstopReport.ContinueDiverged {
		t.Fatalf("BackstopReport = %+v; want the typed divergence flag set", res.BackstopReport)
	}
	// Nothing was forced: the other author's commit is still the head.
	if got := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/"+branch); !strings.HasPrefix(got, moved) {
		t.Fatalf("continued head = %q; want the other author's commit %s left in place", got, moved)
	}
	status := platform.terminalStatus(t)
	if got := string(status["failureMode"]); got != `"`+FailureContinuePullRequestDiverged+`"` {
		t.Fatalf("posted failureMode = %s; want %s", got, FailureContinuePullRequestDiverged)
	}
	if !strings.Contains(string(status["error"]), ErrContinuePullRequestDiverged.Error()) {
		t.Fatalf("posted error = %s; want the typed divergence reason", status["error"])
	}
}

// TestRun_ContinueModeProtectedRejectionIsNotDiverged proves the typed
// signal through the production entry point: the continued head never
// moves, but a server-side policy hook rejects the push with the old
// text matcher's trigger words. The run must fail as a backstop failure
// — never as divergence — because the fetched head is still an ancestor
// of the session's work.
func TestRun_ContinueModeProtectedRejectionIsNotDiverged(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:        "development",
		skipSteering:    true,
		repository:      "https://github.com/example/repo",
		continueNumber:  12,
		backstop:        true,
		protectContinue: true,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureBackstop)
	}
	if res.FailureMode == FailureContinuePullRequestDiverged {
		t.Fatalf("FailureMode = %q; a policy rejection must not read as divergence", res.FailureMode)
	}
	if res.BackstopReport == nil {
		t.Fatal("BackstopReport is nil; want the push failure recorded")
	}
	if res.BackstopReport.ContinueDiverged {
		t.Fatal("BackstopReport.ContinueDiverged = true; want false for a policy rejection")
	}
	if got := res.BackstopReport.Diagnostics; !strings.Contains(got, "non-fast-forward") {
		t.Fatalf("diagnostics = %q; want the hook's wording present so the test proves text alone would mislabel it", got)
	}
}

// TestBuildSteeringPrompt_ContinueModePushesToHeadBranch proves the steering
// entry point: the continue prompt names the head-branch push and never a
// new pull request.
func TestBuildSteeringPrompt_ContinueModePushesToHeadBranch(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: queuedWorkBase("CONT-STEER")}
	qw.ContinuePullRequest = &prompt.ContinuePullRequest{Number: 12, HeadRef: "continued/pr-12", HeadSha: continueFixtureSHA()}
	got := buildSteeringPrompt(qw, streamObservation{}, backstopVisibilityPrivate)
	if !strings.Contains(got, "git push origin HEAD:refs/heads/continued/pr-12") {
		t.Fatalf("steering prompt does not push to the head branch:\n%s", got)
	}
	if strings.Contains(got, "gh pr create") {
		t.Fatalf("steering prompt must never open a new pull request:\n%s", got)
	}
}
