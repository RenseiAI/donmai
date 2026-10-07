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

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// The example URL a task prompt quoted in the incident: it looks like a pull
// request URL but belongs to no session.
const quotedExamplePR = "https://github.com/org/repo/pull/42"

// Markers for scriptedSession.pulls: where refs/pull/<n>/head points.
const (
	// pullAtSessionCommit: the commit the session's checkout is at (the
	// bare repository's main, which a scripted session never moves).
	pullAtSessionCommit = "session"
	// pullAtOtherCommit: a commit the session never had.
	pullAtOtherCommit = "other"
)

// githubRepositoryFixture makes repoURL — a GitHub repository address in any
// form — resolve, for every git subprocess of this test, to a fresh local bare
// repository (git's url.<base>.insteadOf, carried in GIT_CONFIG_* environment
// variables), and returns that bare repository. A Run then provisions, pushes
// and ls-remotes against disk while its session repository is a GitHub one.
// It uses t.Setenv, so it cannot serve a parallel test.
func githubRepositoryFixture(t *testing.T, repoURL string) string {
	t.Helper()
	bare := makeBareRepo(t)
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
	return bare
}

// setPullRef points refs/pull/<number>/head of a bare repository at target:
// a sha, pullAtSessionCommit or pullAtOtherCommit.
func setPullRef(t *testing.T, bare string, number int, target string) {
	t.Helper()
	sha := target
	switch target {
	case pullAtSessionCommit:
		sha = gitRun(t, bare, "rev-parse", "main")
	case pullAtOtherCommit:
		sha = bareCommit(t, bare, "someone else's work")
	}
	gitRun(t, bare, "update-ref", fmt.Sprintf("refs/pull/%d/head", number), sha)
}

// bareCommit creates a commit on top of main in a bare repository, on no
// branch, and returns it.
func bareCommit(t *testing.T, bare, message string) string {
	t.Helper()
	tree := gitRun(t, bare, "rev-parse", "main^{tree}")
	out, err := runGit(context.Background(), bare, gitIdentity{Name: "test", Email: "test@example.com"},
		"commit-tree", tree, "-p", "main", "-m", message)
	if err != nil {
		t.Fatalf("commit-tree: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// recordingRefLookup is the production originRefs, recording every ref
// looked up.
type recordingRefLookup struct {
	mu   sync.Mutex
	refs []string
}

func (l *recordingRefLookup) lookup(ctx context.Context, worktreePath string, refs ...string) (map[string]string, error) {
	l.mu.Lock()
	l.refs = append(l.refs, refs...)
	l.mu.Unlock()
	return originRefs(ctx, worktreePath, refs...)
}

func (l *recordingRefLookup) lookedUp(ref string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, got := range l.refs {
		if got == ref {
			return true
		}
	}
	return false
}

// sentinelGh shadows gh on PATH with a stub that records any call and fails.
func sentinelGh(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "gh-calls.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\necho 'gh is not available' 1>&2\nexit 1\n", calls)
	//nolint:gosec // G306: a stub executable must carry the exec bit.
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestGithubRepositorySlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Acme/Widgets":                         "acme/widgets",
		"https://github.com/acme/widgets.git":                     "acme/widgets",
		"https://github.com/acme/widgets/":                        "acme/widgets",
		"https://x-access-token:secret@github.com/acme/widgets":   "acme/widgets",
		"http://github.com/acme/widgets":                          "acme/widgets",
		"ssh://git@github.com/acme/widgets.git":                   "acme/widgets",
		"ssh://git@ssh.github.com:443/acme/widgets.git":           "acme/widgets",
		"git+ssh://git@github.com/acme/widgets":                   "acme/widgets",
		"git@github.com:acme/widgets.git":                         "acme/widgets",
		"github.com:acme/widgets":                                 "acme/widgets",
		"github.com/acme/widgets":                                 "acme/widgets",
		"https://gitlab.com/acme/widgets":                         "",
		"git@gitlab.com:acme/widgets.git":                         "",
		"https://github.com/acme":                                 "",
		"https://github.com/acme/widgets/pull/7":                  "",
		"https://github.com/acme/widgets?x=1":                     "",
		"file:///srv/git/widgets.git":                             "",
		"/srv/git/widgets.git":                                    "",
		"":                                                        "",
		"https://github.com/acme/widgets and more":                "",
		"https://github.com.attacker.example/acme/widgets":        "",
		"ssh://git@github.com.attacker.example/acme/widgets.git":  "",
		"git@github.com.attacker.example:acme/widgets.git":        "",
		"https://attacker.example/github.com/acme/widgets":        "",
		"https://user@attacker.example@github.com/acme/widgets":   "acme/widgets",
		"https://github.com@attacker.example/acme/widgets":        "",
		"https://github.com:8443@attacker.example/acme/widgets":   "",
		"ssh://github.com@attacker.example/acme/widgets":          "",
		"git@attacker.example:github.com/acme/widgets":            "",
		"attacker.example:github.com/acme/widgets":                "",
		"https://github.com/acme/widgets.git/":                    "acme/widgets",
		"https://GitHub.com/acme/widgets":                         "acme/widgets",
		"https://github.com/acme/widgets#frag":                    "",
		"https://github.com/acme//widgets":                        "",
		"https://github.com/acme/widgets/extra":                   "",
		"ftp://github.com/acme/widgets":                           "",
		"ssh://git@github.com:22/acme/widgets":                    "acme/widgets",
		"https://github.com:443/acme/widgets":                     "acme/widgets",
		"https://token@github.com/acme/widgets.git":               "acme/widgets",
		"git@github.com:/acme/widgets.git":                        "acme/widgets",
		"git@github.com:acme/widgets.git/":                        "acme/widgets",
		"git://github.com/acme/widgets.git":                       "acme/widgets",
		"https://www.github.com/acme/widgets":                     "acme/widgets",
		"github.com/acme/widgets.git":                             "acme/widgets",
		"user@github.com/acme/widgets":                            "acme/widgets",
		"https://github.com/acme/widgets\n":                       "acme/widgets",
		"https://github.com/acme/widgets\nhttps://github.com/x/y": "",
	}
	for in, want := range cases {
		if got := githubRepositorySlug(in); got != want {
			t.Errorf("githubRepositorySlug(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestSessionPullRequestVerifier_AcceptsOnlyTheSessionsOwnPullRequest pins the
// acceptance rule through the production lookup, `git ls-remote origin`, on a
// real remote: the pull request must exist on the session's repository with
// the session branch's remote head or the session commit as its head.
func TestSessionPullRequestVerifier_AcceptsOnlyTheSessionsOwnPullRequest(t *testing.T) {
	const branch = "agent/session-1"
	clone, remote, _ := cloneForRescue(t)
	gitRun(t, clone, "checkout", "-q", "-b", branch)
	writeFile(t, clone, "work.txt", "pushed work\n")
	gitRun(t, clone, "add", "work.txt")
	gitRun(t, clone, "commit", "-q", "-m", "pushed work")
	pushed := gitRun(t, clone, "rev-parse", "HEAD")
	gitRun(t, clone, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	writeFile(t, clone, "more.txt", "later work\n")
	gitRun(t, clone, "add", "more.txt")
	gitRun(t, clone, "commit", "-q", "-m", "later work")
	local := gitRun(t, clone, "rev-parse", "HEAD")
	gitRun(t, clone, "push", "-q", "origin", "HEAD:refs/heads/feature/own-name")
	setPullRef(t, remote, 7, pushed)            // head: the session branch's remote head
	setPullRef(t, remote, 8, local)             // head: the session commit, another branch name
	setPullRef(t, remote, 9, pullAtOtherCommit) // someone else's work

	unreachable := filepath.Join(t.TempDir(), "unreachable")
	gitRun(t, filepath.Dir(unreachable), "clone", "-q", remote, unreachable)
	gitRun(t, unreachable, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	cases := []struct {
		name       string
		repository string
		checkout   string
		url        string
		want       candidateOutcome
		wantReason string
	}{
		{name: "the prompt's quoted example URL", url: quotedExamplePR, want: candidateRefused, wantReason: "another repository"},
		{name: "head is the session branch's remote head", url: "https://github.com/acme/widgets/pull/7", want: candidateAccepted},
		{name: "head is the session commit on another branch", url: "https://github.com/Acme/Widgets/pull/8", want: candidateAccepted},
		{name: "head is someone else's commit", url: "https://github.com/acme/widgets/pull/9", want: candidateHeadMismatch, wantReason: "neither the session branch nor the session commit"},
		{name: "no such pull request", url: "https://github.com/acme/widgets/pull/404", want: candidateRefused, wantReason: "no such pull request"},
		{name: "another repository", url: "https://github.com/acme/gadgets/pull/7", want: candidateRefused, wantReason: "another repository"},
		{name: "session repository is not on GitHub", repository: "-", url: "https://github.com/acme/widgets/pull/7", want: candidateRefused, wantReason: "not a GitHub repository"},
		{name: "not a canonical pull request URL", url: "https://github.com/acme/widgets/pull/7?tab=files", want: candidateRefused, wantReason: "not a canonical"},
		{name: "remote cannot be read", checkout: unreachable, url: "https://github.com/acme/widgets/pull/7", want: candidateUnconfirmed, wantReason: "could not read the pull request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repository := "acme/widgets"
			if tc.repository == "-" {
				repository = ""
			}
			checkout := clone
			if tc.checkout != "" {
				checkout = tc.checkout
			}
			v := &sessionPullRequestVerifier{repository: repository, branch: branch, worktreePath: checkout, lookup: originRefs, outcomes: map[string]candidateOutcome{}}
			got, reason, _ := v.verify(context.Background(), tc.url)
			if got != tc.want {
				t.Fatalf("verify(%s) = %d (%q); want %d", tc.url, got, reason, tc.want)
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
	head := strings.Repeat("a", 40)
	var looked []string
	lookup := func(_ context.Context, _ string, refs ...string) (map[string]string, error) {
		looked = append(looked, refs...)
		return map[string]string{"refs/pull/7/head": head, "refs/heads/agent/s": head}, nil
	}
	newVerifier := func() *sessionPullRequestVerifier {
		return &sessionPullRequestVerifier{repository: "acme/widgets", branch: "agent/s", worktreePath: t.TempDir(), lookup: lookup, outcomes: map[string]candidateOutcome{}}
	}
	v := newVerifier()

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
	for _, ref := range looked {
		if ref == "refs/pull/42/head" {
			t.Errorf("the example URL on another repository was looked up")
		}
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
	fresh := newVerifier()
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

// runPullRequestScenario runs one development session on a GitHub repository
// (addressed as repoURL) whose single turn is turn. Steering and the backstop
// are off, so the session's pull request is exactly what the runner accepted
// from the conversation, and teardown runs on success.
func runPullRequestScenario(t *testing.T, repoURL string, pulls map[int]string, lookup *recordingRefLookup, rescueDir string, turn verdictScriptTurn) *Result {
	t.Helper()
	cfg := scriptedSession{
		workType:     "development",
		skipSteering: true,
		repository:   repoURL,
		pulls:        pulls,
		rescueDir:    rescueDir,
		teardown:     true,
		turns:        []verdictScriptTurn{turn},
	}
	if lookup != nil {
		cfg.lookup = lookup.lookup
	}
	res, _ := runScriptedSession(t, cfg)
	return res
}

// TestRun_QuotedExamplePullRequestURLIsNotTheSessionsPR is the incident: the
// agent's only pull request URL is the example its prompt quoted, and its work
// is still uncommitted. The run must not report that URL, and teardown must
// not delete the uncommitted work without archiving it.
func TestRun_QuotedExamplePullRequestURLIsNotTheSessionsPR(t *testing.T) {
	lookup := &recordingRefLookup{}
	rescueDir := t.TempDir()
	res := runPullRequestScenario(t, "https://github.com/acme/widgets", nil, lookup, rescueDir, verdictScriptTurn{
		files: map[string]string{"notes/copied-prompt.md": "the work the agent was doing\n"},
		text:  "Copied the prompt, which links " + quotedExamplePR + " as the example.",
	})
	if res.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q; want none (the quoted example is not the session's pull request)", res.PullRequestURL)
	}
	if lookup.lookedUp("refs/pull/42/head") {
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

// TestRun_OwnPullRequestIsAcceptedInEveryRemoteForm pins acceptance of the
// session's real pull request — on its repository, head at the session
// commit — whatever form the dispatched repository address takes, through
// `git ls-remote` alone: a gh that fails every call is never asked.
func TestRun_OwnPullRequestIsAcceptedInEveryRemoteForm(t *testing.T) {
	const own = "https://github.com/acme/widgets/pull/7"
	for _, repoURL := range []string{
		"https://github.com/acme/widgets",
		"https://x-access-token:secret@github.com/acme/widgets",
		"ssh://git@github.com/acme/widgets.git",
		"git@github.com:acme/widgets.git",
	} {
		t.Run(repoURL, func(t *testing.T) {
			ghCalls := sentinelGh(t)
			res := runPullRequestScenario(t, repoURL, map[int]string{7: pullAtSessionCommit}, nil, t.TempDir(), verdictScriptTurn{
				text: "Opened " + own + " following the example at " + quotedExamplePR + ".",
			})
			if res.PullRequestURL != own {
				t.Fatalf("PullRequestURL = %q; want the session's own %q", res.PullRequestURL, own)
			}
			if res.Status != "completed" {
				t.Errorf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
			}
			if _, err := os.Stat(ghCalls); !os.IsNotExist(err) {
				t.Errorf("gh was called (%s): verification must not need it", readFile(t, ghCalls))
			}
		})
	}
}

// TestRun_PullRequestOnAnotherBranchIsRejected pins that a pull request on the
// session's own repository whose head is neither the session branch nor the
// session commit is not the session's pull request.
func TestRun_PullRequestOnAnotherBranchIsRejected(t *testing.T) {
	const theirs = "https://github.com/acme/widgets/pull/9"
	lookup := &recordingRefLookup{}
	res := runPullRequestScenario(t, "https://github.com/acme/widgets", map[int]string{9: pullAtOtherCommit}, lookup, t.TempDir(), verdictScriptTurn{
		text: "Related work is in " + theirs + ".",
	})
	if res.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q; want none", res.PullRequestURL)
	}
	if !lookup.lookedUp("refs/pull/9/head") {
		t.Errorf("the same-repository candidate was never looked up")
	}
}

// TestRun_PullRefThatLagsAPushIsReverified pins that a head mismatch is not
// remembered: the agent pushes to its open pull request and reports it in the
// same turn, GitHub's refs/pull/<n>/head still shows the previous head at the
// first check, and once the ref catches up the next check accepts the pull
// request instead of refusing it for the rest of the session.
func TestRun_PullRefThatLagsAPushIsReverified(t *testing.T) {
	var mu sync.Mutex
	lookups := 0
	lookup := func(ctx context.Context, worktreePath string, refs ...string) (map[string]string, error) {
		mu.Lock()
		lookups++
		n := lookups
		mu.Unlock()
		switch n {
		case 1:
			// The agent's push: a new commit on the session branch, while
			// refs/pull/7/head still shows the previous head.
			writeFile(t, worktreePath, "feature.go", "package feature\n")
			gitRun(t, worktreePath, "add", "feature.go")
			if out, err := runGit(ctx, worktreePath, gitIdentity{Name: "test", Email: "test@example.com"}, "commit", "-q", "-m", "feature"); err != nil {
				t.Fatalf("commit: %v\n%s", err, out)
			}
			gitRun(t, worktreePath, "push", "-q", "origin", "HEAD:refs/heads/"+scriptedSessionBranch)
		case 2:
			// GitHub has synced the pull request to the pushed head.
			gitRun(t, worktreePath, "push", "-q", "-f", "origin", "HEAD:refs/pull/7/head")
		}
		return originRefs(ctx, worktreePath, refs...)
	}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		lookup:     lookup,
		turns: []verdictScriptTurn{
			{text: "Pushed the fix to " + followUpPR},
			{text: "The PR is open: " + followUpPR},
			{text: "The PR is open: " + followUpPR},
			{text: "The PR is open: " + followUpPR},
			{text: "The PR is open: " + followUpPR},
		},
	})
	if res.PullRequestURL != followUpPR || res.Status != "completed" {
		t.Fatalf("PullRequestURL=%q Status=%q (%s: %s); want the session's own PR accepted once its ref caught up",
			res.PullRequestURL, res.Status, res.FailureMode, res.Error)
	}
	if len(provider.prompts) != 1 || provider.prompts[0] != continuePrompt {
		t.Errorf("prompts = %q; want one continuation, then acceptance", provider.prompts)
	}
}

// scriptedSessionBranch is the branch the runner owns for a scripted session.
const scriptedSessionBranch = "agent/test-session-MANIFEST-FOLLOWUP"

// TestSessionPullRequestVerifier_Undelivered pins the re-read of the accepted
// pull request: a rework whose head has not moved to a commit of the
// session's own since run start has no new commit (and is not asked about its
// draft state), a draft is a draft, a head that moved to a commit of the
// session's own since the previous re-read is progress, and a read that fails
// does not count against the pull request. Every read is stubbed: an
// unexpected one fails the case, and none runs git.
func TestSessionPullRequestVerifier_Undelivered(t *testing.T) {
	const own = "https://github.com/acme/widgets/pull/7"
	start := strings.Repeat("a", 40)
	moved := strings.Repeat("b", 40)
	// The stubbed re-reads below exercise only remote lookups: range
	// inspection is stubbed to a delivering range so the cases never
	// shell out to git in a directory that is not a checkout.
	delivering := func(context.Context, string, string, string) (continueRangeInspection, error) {
		return continueRangeInspection{commitCount: 1, hasCodeChange: true}, nil
	}
	refsAt := func(head string) pullRequestRefLookup {
		return func(_ context.Context, _ string, refs ...string) (map[string]string, error) {
			if len(refs) != 1 || refs[0] != "refs/pull/7/head" {
				return nil, fmt.Errorf("unexpected refs %q", refs)
			}
			return map[string]string{"refs/pull/7/head": head}, nil
		}
	}
	refsAtBranch := func(pullHead, branchHead string) pullRequestRefLookup {
		return func(_ context.Context, _ string, refs ...string) (map[string]string, error) {
			if len(refs) != 2 || refs[0] != "refs/pull/7/head" || refs[1] != "refs/heads/agent/s" {
				return nil, fmt.Errorf("unexpected refs %q", refs)
			}
			return map[string]string{"refs/pull/7/head": pullHead, "refs/heads/agent/s": branchHead}, nil
		}
	}
	headIs := func(head string) pullRequestHeadLookup {
		return func(context.Context, string) (string, error) { return head, nil }
	}
	headUnreadable := func(context.Context, string) (string, error) { return "", errors.New("not a git repository") }
	unreadable := func(context.Context, string, ...string) (map[string]string, error) {
		return nil, errors.New("remote unreachable")
	}
	draftIs := func(draft bool) pullRequestDraftLookup {
		return func(_ context.Context, _ string, url string) (bool, error) {
			if url != own {
				return false, fmt.Errorf("unexpected url %q", url)
			}
			return draft, nil
		}
	}
	ghDown := func(context.Context, string, string) (bool, error) { return false, errors.New("gh: not authenticated") }
	cases := []struct {
		name      string
		accepted  string
		branch    string
		startHead string
		lastHead  string
		lookup    pullRequestRefLookup
		head      pullRequestHeadLookup
		draft     pullRequestDraftLookup
		want      string
		wantMoved bool
		wantErr   string
	}{
		{name: "no accepted pull request", draft: draftIs(true)},
		{name: "ready pull request the session opened", accepted: own, draft: draftIs(false)},
		{name: "draft the session opened, first re-read", accepted: own, branch: "agent/s", lookup: refsAtBranch(moved, moved), draft: draftIs(true), want: undeliveredDraft},
		{name: "draft the session pushed to since the last re-read", accepted: own, branch: "agent/s", lastHead: start, lookup: refsAtBranch(moved, moved), draft: draftIs(true), want: undeliveredDraft, wantMoved: true},
		{name: "draft whose head is the session commit on another branch", accepted: own, branch: "agent/s", lastHead: start, lookup: refsAtBranch(moved, start), head: headIs(moved), draft: draftIs(true), want: undeliveredDraft, wantMoved: true},
		{name: "draft whose head moved to someone else's commit", accepted: own, branch: "agent/s", lastHead: start, lookup: refsAtBranch(moved, start), head: headIs(start), draft: draftIs(true), want: undeliveredDraft},
		{name: "draft that did not move", accepted: own, branch: "agent/s", lastHead: moved, lookup: refsAtBranch(moved, moved), draft: draftIs(true), want: undeliveredDraft},
		{name: "draft whose head is unreadable", accepted: own, branch: "agent/s", lastHead: start, lookup: unreadable, draft: draftIs(true), want: undeliveredDraft, wantErr: "remote unreachable"},
		{name: "rework with no new commit", accepted: own, startHead: start, lastHead: start, lookup: refsAt(start), draft: ghDown, want: undeliveredNoNewCommit},
		{name: "rework with a new commit, ready", accepted: own, branch: "agent/s", startHead: start, lastHead: start, lookup: refsAtBranch(moved, moved), draft: draftIs(false), wantMoved: true},
		{name: "rework with a new commit, still draft", accepted: own, branch: "agent/s", startHead: start, lastHead: start, lookup: refsAtBranch(moved, moved), draft: draftIs(true), want: undeliveredDraft, wantMoved: true},
		{name: "rework whose head moved to someone else's commit", accepted: own, branch: "agent/s", startHead: start, lastHead: start, lookup: refsAtBranch(moved, start), head: headIs(start), draft: draftIs(false), want: undeliveredNoNewCommit},
		{name: "rework whose session commit is unreadable", accepted: own, branch: "agent/s", startHead: start, lastHead: start, lookup: refsAtBranch(moved, start), head: headUnreadable, draft: draftIs(false), wantErr: "read the session commit"},
		{name: "draft state unknown", accepted: own, draft: ghDown, wantErr: "not authenticated"},
		{name: "rework head unreadable", accepted: own, startHead: start, lastHead: start, lookup: unreadable, draft: draftIs(false), wantErr: "remote unreachable"},
		{name: "rework head unreadable, draft", accepted: own, startHead: start, lastHead: start, lookup: unreadable, draft: draftIs(true), want: undeliveredDraft, wantErr: "remote unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := tc.lookup
			if lookup == nil {
				lookup = func(context.Context, string, ...string) (map[string]string, error) {
					t.Fatal("read the pull request head from the remote; this case must not")
					return nil, nil
				}
			}
			head := tc.head
			if head == nil {
				head = func(context.Context, string) (string, error) {
					t.Fatal("read the checkout's HEAD; this case must not")
					return "", nil
				}
			}
			v := &sessionPullRequestVerifier{
				repository: "acme/widgets", branch: tc.branch, worktreePath: t.TempDir(), accepted: tc.accepted,
				startHead: tc.startHead, lastHead: tc.lastHead, lookup: lookup, draftLookup: tc.draft, headLookup: head,
				inspectRange: delivering,
			}
			got, gotMoved, err := v.undelivered(context.Background())
			if got != tc.want || gotMoved != tc.wantMoved {
				t.Fatalf("undelivered = %q, moved %v (err %v); want %q, moved %v", got, gotMoved, err, tc.want, tc.wantMoved)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v; want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v; want it to contain %q", err, tc.wantErr)
			}
		})
	}
	var none *sessionPullRequestVerifier
	if got, moved, err := none.undelivered(context.Background()); got != "" || moved || err != nil {
		t.Errorf("nil verifier: undelivered = %q, %v, %v; want none", got, moved, err)
	}
	// A verifier built without a head lookup reads no HEAD, even in a real
	// checkout where git would answer.
	checkout, _, _ := cloneForRescue(t)
	unwired := &sessionPullRequestVerifier{worktreePath: checkout}
	if head, err := unwired.sessionHead(context.Background()); head != "" || err == nil {
		t.Errorf("verifier without a head lookup: sessionHead = %q, %v; want an error and no git run", head, err)
	}
}

// TestGithubPullRequestDraft pins the production draft read: the runner's
// one gh pr view query, with isDraft decoded, and a gh failure surfaced as an
// error rather than as "not a draft".
func TestGithubPullRequestDraft(t *testing.T) {
	const prURL = "https://github.com/acme/widgets/pull/7"
	cases := []struct {
		name     string
		exitCode int
		output   string
		want     bool
		wantErr  bool
	}{
		{name: "draft", output: `{"number":7,"url":"` + prURL + `","baseRefName":"main","headRefName":"agent/s","isDraft":true}`, want: true},
		{name: "ready", output: `{"number":7,"url":"` + prURL + `","baseRefName":"main","headRefName":"agent/s","isDraft":false}`},
		{name: "gh fails", exitCode: 1, output: "gh: To get started with GitHub CLI, please run: gh auth login", wantErr: true},
		{name: "not JSON", output: "unexpected", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGhOnPathExpectingPullRequestView(t, prURL, tc.exitCode, tc.output)
			got, err := githubPullRequestDraft(context.Background(), t.TempDir(), prURL)
			if (err != nil) != tc.wantErr {
				t.Fatalf("githubPullRequestDraft err = %v; want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("githubPullRequestDraft = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestReworkStartHead pins which runs carry a start head, and where it comes
// from: the dispatched pull request's recorded head, else the commit the
// checkout of a run provisioned at an existing branch started at.
func TestReworkStartHead(t *testing.T) {
	recorded := strings.Repeat("c", 40)
	checkoutStart := strings.Repeat("d", 40)
	started := &Result{rescueTargets: []rescueTarget{{path: "/work/other", base: strings.Repeat("e", 40)}, {path: "/work/repo", base: checkoutStart}}}
	cases := []struct {
		name string
		qw   QueuedWork
		want string
	}{
		{name: "a run that opens its own pull request"},
		{name: "dispatched pull request", qw: QueuedWork{PullRequest: &workarea.PullRequestV1{Number: 7, HeadSHA: recorded}}, want: recorded},
		{name: "existing branch", qw: func() QueuedWork { var qw QueuedWork; qw.Ref = "feature/rework"; return qw }(), want: checkoutStart},
		{name: "dispatched pull request with no usable head, existing branch", qw: func() QueuedWork {
			var qw QueuedWork
			qw.Ref = "feature/rework"
			qw.PullRequest = &workarea.PullRequestV1{Number: 7, HeadSHA: "not-a-sha"}
			return qw
		}(), want: checkoutStart},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reworkStartHead(tc.qw, started, "/work/repo"); got != tc.want {
				t.Fatalf("reworkStartHead = %q; want %q", got, tc.want)
			}
		})
	}
}
