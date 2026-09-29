package runner

import (
	"context"
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
		{name: "head is someone else's commit", url: "https://github.com/acme/widgets/pull/9", want: candidateRefused, wantReason: "neither the session branch nor the session commit"},
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
