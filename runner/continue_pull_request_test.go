package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestIsContinueDivergence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "fast-forward rejection", output: "! [rejected] HEAD -> feature/x (non-fast-forward)", want: true},
		{name: "fetch-first hint", output: "! [rejected] HEAD -> feature/x (fetch first)", want: true},
		{name: "transport failure", output: "error: failed to push some refs: connection reset", want: false},
		{name: "empty", output: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isContinueDivergence(tc.output); got != tc.want {
				t.Fatalf("isContinueDivergence(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
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

	// gh must never run in continue mode: fail loudly if it does.
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho 'gh must not run in continue mode' 1>&2\nexit 99\n"
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
}
