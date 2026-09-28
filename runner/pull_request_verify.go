package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// A pull request URL that merely appears in a session's conversation is a
// CANDIDATE, never the session's pull request. The prompt can quote an example
// (`https://github.com/org/repo/pull/42`), a tool can print an unrelated PR,
// and the agent can mention one it read about. Accepting such a URL skipped
// the "open a pull request" nudge and the backstop, reported the run as
// completed, and let teardown delete the worktree with the real work still
// uncommitted in it.
//
// So for work whose completion contract owes a pull request, the runner
// accepts a URL only when the pull request EXISTS on the session's own
// repository and its head is the session's branch or the session's commit
// (the checkout's HEAD). Existence and head are read through the same GitHub
// access the runner already uses for pull request facts (`gh pr view`). A
// candidate that cannot be confirmed — including when that lookup fails — is
// rejected, so the nudge and the backstop run: the backstop's own
// `gh pr create --head <session branch>` recovers a real pull request that
// only failed to verify.

// pullRequestLookupTimeout bounds one `gh pr view` lookup.
const pullRequestLookupTimeout = 10 * time.Second

// maxPullRequestLookups bounds the network lookups one verification pass may
// spend, so a transcript full of URLs cannot stall the session end.
const maxPullRequestLookups = 4

// maxPullRequestCandidates bounds the distinct candidate URLs a stream keeps.
const maxPullRequestCandidates = 8

// pullRequestHead is the GitHub projection the verifier reads.
type pullRequestHead struct {
	Number            int    `json:"number"`
	URL               string `json:"url"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	IsCrossRepository bool   `json:"isCrossRepository"`
}

// pullRequestHeadLookup reads one pull request's head. worktreePath is the
// directory the lookup runs in.
type pullRequestHeadLookup func(ctx context.Context, worktreePath, prURL string) (pullRequestHead, error)

// ghPullRequestHead is the production lookup: `gh pr view <url>`, the same
// GitHub access lookupGitHubPullRequest uses.
func ghPullRequestHead(ctx context.Context, worktreePath, prURL string) (pullRequestHead, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, pullRequestLookupTimeout)
	defer cancel()
	out, err := runGh(lookupCtx, worktreePath, "pr", "view", prURL,
		"--json", "number,url,headRefName,headRefOid,isCrossRepository")
	if err != nil {
		return pullRequestHead{}, fmt.Errorf("gh pr view: %w: %s", err, firstLine(out))
	}
	var head pullRequestHead
	if err := json.Unmarshal([]byte(out), &head); err != nil {
		return pullRequestHead{}, fmt.Errorf("decode gh pr view output: %w", err)
	}
	return head, nil
}

// sessionPullRequestVerifier decides which candidate URL, if any, is this
// session's pull request. One verifier lives for one session; it remembers the
// URL it accepted so a later turn that only quotes some other URL cannot
// displace a verified pull request.
type sessionPullRequestVerifier struct {
	// repository is the session repository's canonical lower-case
	// "owner/repo", or "" when the session's repository is not on GitHub
	// (then no GitHub pull request can be the session's).
	repository string
	// branch is the branch the runner owns for this session.
	branch string
	// worktreePath is the selected repository's checkout.
	worktreePath string
	lookup       pullRequestHeadLookup
	accepted     string
}

// newSessionPullRequestVerifier resolves the session's GitHub repository:
// the declared selected repository's source, else the dispatched repository,
// else the checkout's origin remote. A repository-free workarea resolves to
// none without running git (that contract forbids any git invocation).
func (r *Runner) newSessionPullRequestVerifier(ctx context.Context, qw QueuedWork, declaration *workarea.NormalizedDeclaration, worktreePath, branch string, repositoryFree bool) *sessionPullRequestVerifier {
	v := &sessionPullRequestVerifier{branch: branch, worktreePath: worktreePath, lookup: r.pullRequestLookup}
	if v.lookup == nil {
		v.lookup = ghPullRequestHead
	}
	switch {
	case repositoryFree:
	case declaration != nil:
		v.repository = workarea.CanonicalGitHubRepositorySource(declaration.Selected.Source.Repository)
	default:
		v.repository = workarea.CanonicalGitHubRepositorySource(qw.Repository)
		if v.repository == "" && strings.TrimSpace(worktreePath) != "" {
			originCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if origin, err := runGit(originCtx, worktreePath, gitIdentity{}, "remote", "get-url", "origin"); err == nil {
				v.repository = workarea.CanonicalGitHubRepositorySource(strings.TrimSpace(origin))
			}
			cancel()
		}
	}
	return v
}

// verify reports whether candidate is this session's pull request, with the
// reason when it is not.
func (v *sessionPullRequestVerifier) verify(ctx context.Context, candidate string) (bool, string, bool) {
	slug, number, err := parseCanonicalGitHubPullRequestURL(candidate)
	if err != nil {
		return false, "not a canonical GitHub pull request URL", false
	}
	if v.repository == "" {
		return false, "the session's repository is not a GitHub repository", false
	}
	if normalizeGitHubRepositorySlug(slug) != v.repository {
		return false, "the pull request is on another repository", false
	}
	head, err := v.lookup(ctx, v.worktreePath, candidate)
	if err != nil {
		return false, "could not confirm the pull request exists: " + err.Error(), true
	}
	viewSlug, viewNumber, err := parseCanonicalGitHubPullRequestURL(head.URL)
	if err != nil || normalizeGitHubRepositorySlug(viewSlug) != v.repository || viewNumber != number || head.Number != number {
		return false, "the pull request lookup returned a different pull request", true
	}
	if !head.IsCrossRepository && v.branch != "" && head.HeadRefName == v.branch {
		return true, "", true
	}
	if oid := strings.ToLower(strings.TrimSpace(head.HeadRefOid)); oid != "" {
		shaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		sha, shaErr := captureHeadSHA(shaCtx, v.worktreePath)
		cancel()
		if shaErr == nil && sha == oid {
			return true, "", true
		}
	}
	return false, "the pull request's head is neither the session branch nor the session commit", true
}

// settle picks the session's pull request from the candidates a turn offered,
// newest first: the URL now on the envelope (a manifest's, or the latest one
// the stream scraped), then every URL the turn's stream carried, then the URL
// accepted earlier. The first candidate that verifies is written to
// res.PullRequestURL and obs.pullRequestURL; when none does, both are cleared
// so the nudge and the backstop treat the session as having no pull request.
func (v *sessionPullRequestVerifier) settle(ctx context.Context, res *Result, obs *streamObservation, turn streamObservation) (rejected []pullRequestRejection) {
	candidates := make([]string, 0, len(turn.pullRequestCandidates)+2)
	candidates = append(candidates, res.PullRequestURL)
	for i := len(turn.pullRequestCandidates) - 1; i >= 0; i-- {
		candidates = append(candidates, turn.pullRequestCandidates[i])
	}
	candidates = append(candidates, v.accepted)

	seen := make(map[string]struct{}, len(candidates))
	lookups := 0
	accepted := ""
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		if candidate == v.accepted {
			accepted = candidate
			break
		}
		if lookups >= maxPullRequestLookups {
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "not checked: lookup budget spent"})
			continue
		}
		ok, reason, lookedUp := v.verify(ctx, candidate)
		if lookedUp {
			lookups++
		}
		if ok {
			accepted = candidate
			break
		}
		rejected = append(rejected, pullRequestRejection{url: candidate, reason: reason})
	}
	v.accepted = accepted
	res.PullRequestURL = accepted
	obs.pullRequestURL = accepted
	return rejected
}

// pullRequestRejection is one candidate the verifier refused, and why.
type pullRequestRejection struct {
	url    string
	reason string
}

// acceptSessionPullRequest runs the verifier over one turn and logs every
// refused candidate. A nil verifier (work that owes no pull request) leaves
// the envelope untouched.
func (r *Runner) acceptSessionPullRequest(ctx context.Context, v *sessionPullRequestVerifier, qw QueuedWork, res *Result, obs *streamObservation, turn streamObservation) {
	if v == nil {
		return
	}
	for _, rejection := range v.settle(ctx, res, obs, turn) {
		r.logger.Warn("pull request URL is not this session's pull request; ignored",
			"sessionId", qw.SessionID,
			"url", rejection.url,
			"reason", rejection.reason,
		)
	}
}

// appendPullRequestCandidates records every GitHub pull request URL in text,
// in order, keeping each URL once at its latest position and at most
// maxPullRequestCandidates of them.
func appendPullRequestCandidates(candidates []string, text string) []string {
	for _, u := range prURLRE.FindAllString(text, -1) {
		for i, existing := range candidates {
			if existing == u {
				candidates = append(candidates[:i], candidates[i+1:]...)
				break
			}
		}
		candidates = append(candidates, u)
		if len(candidates) > maxPullRequestCandidates {
			candidates = candidates[len(candidates)-maxPullRequestCandidates:]
		}
	}
	return candidates
}

// firstLine returns the first non-empty line of s, for compact diagnostics.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
