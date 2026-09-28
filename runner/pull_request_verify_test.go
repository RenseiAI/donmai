package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The example URL a task prompt quoted in the incident: it looks like a pull
// request URL but belongs to no session.
const quotedExamplePR = "https://github.com/org/repo/pull/42"

// githubRepositoryFixture returns https://github.com/<slug> and makes that URL
// resolve, for every git subprocess of this test, to a fresh local bare
// repository (git's url.<base>.insteadOf, carried in GIT_CONFIG_* environment
// variables). A Run then provisions from disk while its session repository is
// a GitHub one, so the session pull request verifier resolves a real identity.
// It uses t.Setenv, so it cannot serve a parallel test.
func githubRepositoryFixture(t *testing.T, slug string) string {
	t.Helper()
	bare := makeBareRepo(t)
	repoURL := "https://github.com/" + slug
	n := 0
	if existing := os.Getenv("GIT_CONFIG_COUNT"); existing != "" {
		parsed, err := strconv.Atoi(existing)
		if err != nil {
			t.Fatalf("GIT_CONFIG_COUNT=%q: %v", existing, err)
		}
		n = parsed
	}
	t.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", n), "url."+bare+".insteadOf")
	t.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), repoURL)
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
	return repoURL
}

// fakePullRequestLookup answers pull request lookups from a table; any other
// URL fails the way `gh pr view` fails for a pull request that does not exist.
type fakePullRequestLookup struct {
	mu    sync.Mutex
	heads map[string]pullRequestHead
	calls []string
}

func (f *fakePullRequestLookup) lookup(_ context.Context, _, prURL string) (pullRequestHead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, prURL)
	head, ok := f.heads[prURL]
	if !ok {
		return pullRequestHead{}, errors.New("could not resolve to a pull request")
	}
	return head, nil
}

func (f *fakePullRequestLookup) called(prURL string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call == prURL {
			return true
		}
	}
	return false
}

// stubGhPullRequestView shadows `gh` on PATH with a stub that answers
// `gh pr view <url> --json <the verifier's fields>` from views (keyed by URL)
// and fails like the real gh for any other pull request.
func stubGhPullRequestView(t *testing.T, views map[string]string) {
	t.Helper()
	dir := t.TempDir()
	var cases strings.Builder
	for prURL, view := range views {
		file := filepath.Join(dir, strconv.Itoa(len(cases.String()))+".json")
		if err := os.WriteFile(file, []byte(view), 0o600); err != nil {
			t.Fatalf("write gh view fixture: %v", err)
		}
		fmt.Fprintf(&cases, "  %q) cat %q; exit 0 ;;\n", prURL, file)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -ne 5 ] || [ "$1" != "pr" ] || [ "$2" != "view" ] || [ "$4" != "--json" ] || [ "$5" != "number,url,headRefName,headRefOid,isCrossRepository" ]; then
  echo "unexpected gh argv: $*" 1>&2
  exit 1
fi
case "$3" in
%s  *) echo "GraphQL: Could not resolve to a PullRequest" 1>&2; exit 1 ;;
esac
`, cases.String())
	//nolint:gosec // G306: a stub executable must carry the exec bit.
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSessionPullRequestVerifier_AcceptsOnlyTheSessionsOwnPullRequest pins the
// acceptance rule through the production `gh pr view` lookup (a stub gh):
// the pull request must exist on the session's repository with the session's
// branch or commit as its head.
func TestSessionPullRequestVerifier_AcceptsOnlyTheSessionsOwnPullRequest(t *testing.T) {
	checkout := t.TempDir()
	gitInit(t, checkout)
	head := gitRun(t, checkout, "rev-parse", "HEAD")
	const branch = "agent/session-1"
	const (
		byBranch  = "https://github.com/acme/widgets/pull/7"
		byCommit  = "https://github.com/acme/widgets/pull/8"
		otherHead = "https://github.com/acme/widgets/pull/9"
		fork      = "https://github.com/acme/widgets/pull/10"
		renamed   = "https://github.com/acme/widgets/pull/11"
		missing   = "https://github.com/acme/widgets/pull/404"
		otherRepo = "https://github.com/acme/gadgets/pull/7"
	)
	other := strings.Repeat("b", 40)
	view := func(prURL string, number int, headRef, oid string, cross bool) string {
		return fmt.Sprintf(`{"number":%d,"url":%q,"headRefName":%q,"headRefOid":%q,"isCrossRepository":%t}`, number, prURL, headRef, oid, cross)
	}
	stubGhPullRequestView(t, map[string]string{
		byBranch:  view(byBranch, 7, branch, other, false),
		byCommit:  view(byCommit, 8, "feature/own-name", head, false),
		otherHead: view(otherHead, 9, "feature/someone-else", other, false),
		fork:      view(fork, 10, branch, other, true),
		renamed:   view("https://github.com/acme/other/pull/11", 11, branch, head, false),
		otherRepo: view(otherRepo, 7, branch, head, false),
	})

	cases := []struct {
		name       string
		repository string
		url        string
		wantOK     bool
		wantReason string
	}{
		{name: "the prompt's quoted example URL", repository: "acme/widgets", url: quotedExamplePR, wantReason: "another repository"},
		{name: "own repository, head is the session branch", repository: "acme/widgets", url: byBranch, wantOK: true},
		{name: "own repository, head is the session commit", repository: "acme/widgets", url: byCommit, wantOK: true},
		{name: "own repository, another branch and commit", repository: "acme/widgets", url: otherHead, wantReason: "neither the session branch nor the session commit"},
		{name: "a fork's branch that shares the session branch name", repository: "acme/widgets", url: fork, wantReason: "neither the session branch nor the session commit"},
		{name: "lookup answers for a different pull request", repository: "acme/widgets", url: renamed, wantReason: "different pull request"},
		{name: "a pull request that does not exist", repository: "acme/widgets", url: missing, wantReason: "could not confirm the pull request exists"},
		{name: "another repository", repository: "acme/widgets", url: otherRepo, wantReason: "another repository"},
		{name: "session repository is not on GitHub", repository: "", url: byBranch, wantReason: "not a GitHub repository"},
		{name: "not a canonical pull request URL", repository: "acme/widgets", url: byBranch + "?tab=files", wantReason: "not a canonical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &sessionPullRequestVerifier{repository: tc.repository, branch: branch, worktreePath: checkout, lookup: ghPullRequestHead}
			ok, reason, _ := v.verify(context.Background(), tc.url)
			if ok != tc.wantOK {
				t.Fatalf("verify(%s) = %v (%q); want %v", tc.url, ok, reason, tc.wantOK)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q; want it to contain %q", reason, tc.wantReason)
			}
		})
	}
}

// TestSessionPullRequestVerifier_SettleNewestVerifiedCandidateWins pins how a
// turn's candidates are settled: the newest verified candidate wins even when
// the agent quoted the example URL after it, a later turn that only quotes the
// example keeps the verified pull request, and a turn with no verified
// candidate leaves the envelope without one.
func TestSessionPullRequestVerifier_SettleNewestVerifiedCandidateWins(t *testing.T) {
	const own = "https://github.com/acme/widgets/pull/7"
	fake := &fakePullRequestLookup{heads: map[string]pullRequestHead{
		own: {Number: 7, URL: own, HeadRefName: "agent/s"},
	}}
	v := &sessionPullRequestVerifier{repository: "acme/widgets", branch: "agent/s", worktreePath: t.TempDir(), lookup: fake.lookup}

	// Turn 1: the agent opens its PR, then quotes the prompt's example.
	var turn1 streamObservation
	turn1.pullRequestCandidates = appendPullRequestCandidates(nil, "Opened "+own)
	turn1.pullRequestCandidates = appendPullRequestCandidates(turn1.pullRequestCandidates, "Following "+quotedExamplePR)
	res := &Result{}
	res.PullRequestURL = quotedExamplePR // the scraped latest URL
	obs := &streamObservation{pullRequestURL: quotedExamplePR}
	rejected := v.settle(context.Background(), res, obs, turn1)
	if res.PullRequestURL != own || obs.pullRequestURL != own {
		t.Fatalf("after turn 1: envelope %q, observation %q; want %q", res.PullRequestURL, obs.pullRequestURL, own)
	}
	if len(rejected) != 1 || rejected[0].url != quotedExamplePR {
		t.Errorf("rejected = %+v; want only the quoted example", rejected)
	}
	if fake.called(quotedExamplePR) {
		t.Errorf("the example URL on another repository was looked up; want it refused without a lookup")
	}

	// Turn 2 only quotes the example again: the verified PR stands.
	var turn2 streamObservation
	turn2.pullRequestCandidates = appendPullRequestCandidates(nil, quotedExamplePR)
	res.PullRequestURL = quotedExamplePR
	v.settle(context.Background(), res, obs, turn2)
	if res.PullRequestURL != own {
		t.Fatalf("after turn 2: envelope %q; want the verified %q", res.PullRequestURL, own)
	}

	// A fresh session whose only candidate is the example has no PR.
	fresh := &sessionPullRequestVerifier{repository: "acme/widgets", branch: "agent/s", worktreePath: t.TempDir(), lookup: fake.lookup}
	res = &Result{}
	res.PullRequestURL = quotedExamplePR
	obs = &streamObservation{pullRequestURL: quotedExamplePR}
	fresh.settle(context.Background(), res, obs, turn2)
	if res.PullRequestURL != "" || obs.pullRequestURL != "" {
		t.Fatalf("example-only session: envelope %q, observation %q; want no pull request", res.PullRequestURL, obs.pullRequestURL)
	}
}

func TestAppendPullRequestCandidates_KeepsLatestPositionAndBound(t *testing.T) {
	a := "https://github.com/o/r/pull/1"
	b := "https://github.com/o/r/pull/2"
	got := appendPullRequestCandidates(nil, a+" then "+b+" then "+a)
	if strings.Join(got, ",") != b+","+a {
		t.Fatalf("candidates = %v; want [%s %s]", got, b, a)
	}
	var many []string
	for i := 0; i < maxPullRequestCandidates+3; i++ {
		many = appendPullRequestCandidates(many, fmt.Sprintf("https://github.com/o/r/pull/%d", i+10))
	}
	if len(many) != maxPullRequestCandidates || !strings.HasSuffix(many[len(many)-1], "/pull/"+strconv.Itoa(maxPullRequestCandidates+12)) {
		t.Fatalf("candidates = %v; want the newest %d", many, maxPullRequestCandidates)
	}
}

// runPullRequestScenario runs one development session on a GitHub-identified
// repository whose single turn is turn. Steering and the backstop are off, so
// the session's pull request is exactly what the runner accepted from the
// conversation, and teardown runs on success.
func runPullRequestScenario(t *testing.T, lookup *fakePullRequestLookup, rescueDir string, turn verdictScriptTurn) (*Result, *Runner) {
	t.Helper()
	res, r := runScriptedSession(t, scriptedSession{
		workType:     "development",
		skipSteering: true,
		github:       "acme/widgets",
		lookup:       lookup,
		rescueDir:    rescueDir,
		teardown:     true,
		turns:        []verdictScriptTurn{turn},
	})
	return res, r
}

// TestRun_QuotedExamplePullRequestURLIsNotTheSessionsPR is the incident: the
// agent's only pull request URL is the example its prompt quoted, and its work
// is still uncommitted. The run must not report that URL, and teardown must
// not delete the uncommitted work without archiving it.
func TestRun_QuotedExamplePullRequestURLIsNotTheSessionsPR(t *testing.T) {
	lookup := &fakePullRequestLookup{}
	rescueDir := t.TempDir()
	res, _ := runPullRequestScenario(t, lookup, rescueDir, verdictScriptTurn{
		files: map[string]string{"notes/copied-prompt.md": "the work the agent was doing\n"},
		text:  "Copied the prompt, which links " + quotedExamplePR + " as the example.",
	})
	if res.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q; want none (the quoted example is not the session's pull request)", res.PullRequestURL)
	}
	if lookup.called(quotedExamplePR) {
		t.Errorf("the example URL on another repository was looked up; want it refused without a lookup")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after a completed run (stat err %v); the scenario needs teardown to run", res.WorktreePath, err)
	}
	patch := onlyRescuePatch(t, rescueDir)
	if body := readFile(t, patch); !strings.Contains(body, "notes/copied-prompt.md") || !strings.Contains(body, "the work the agent was doing") {
		t.Fatalf("rescue patch %s does not carry the uncommitted work:\n%s", patch, body)
	}
}

// TestRun_OwnPullRequestIsAcceptedOverAQuotedExample pins the other side: a
// pull request that exists on the session's repository with the session
// branch as its head is the session's pull request, even when the agent also
// quoted the example URL after it.
func TestRun_OwnPullRequestIsAcceptedOverAQuotedExample(t *testing.T) {
	const own = "https://github.com/acme/widgets/pull/7"
	lookup := &fakePullRequestLookup{heads: map[string]pullRequestHead{
		own: {Number: 7, URL: own, HeadRefName: "agent/test-session-MANIFEST-FOLLOWUP"},
	}}
	res, _ := runPullRequestScenario(t, lookup, t.TempDir(), verdictScriptTurn{
		text: "Opened " + own + " following the example at " + quotedExamplePR + ".",
	})
	if res.PullRequestURL != own {
		t.Fatalf("PullRequestURL = %q; want the session's own %q", res.PullRequestURL, own)
	}
	if res.Status != "completed" {
		t.Errorf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
}

// TestRun_PullRequestOnAnotherBranchIsRejected pins that a pull request on the
// session's own repository whose head is neither the session branch nor the
// session commit is not the session's pull request.
func TestRun_PullRequestOnAnotherBranchIsRejected(t *testing.T) {
	const theirs = "https://github.com/acme/widgets/pull/9"
	lookup := &fakePullRequestLookup{heads: map[string]pullRequestHead{
		theirs: {Number: 9, URL: theirs, HeadRefName: "feature/someone-else", HeadRefOid: strings.Repeat("c", 40)},
	}}
	res, _ := runPullRequestScenario(t, lookup, t.TempDir(), verdictScriptTurn{
		text: "Related work is in " + theirs + ".",
	})
	if res.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q; want none", res.PullRequestURL)
	}
	if !lookup.called(theirs) {
		t.Errorf("the same-repository candidate was never looked up")
	}
}
