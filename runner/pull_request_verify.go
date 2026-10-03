package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
// (the checkout's HEAD). Both are read from the checkout's own remote with the
// git credential the session already pushes with — `git ls-remote origin
// refs/pull/<n>/head refs/heads/<session branch>` — so verification needs no
// GitHub CLI and works wherever the session can push. A candidate that cannot
// be confirmed is refused, so the nudge and the backstop run; the backstop's
// own `gh pr create --head <session branch>` still recovers a real pull
// request that only failed to verify.
//
// A verified pull request is the session's, but it does not always deliver
// the work. Run briefs ask for a draft pull request early, so the work
// survives a cut-off run; and a rework run continues a pull request that
// existed before the run started. So when the session's pull request is the
// only result a turn left (no turn-result manifest, and no verdict in this or
// any earlier turn), the runner re-reads it at the turn boundary (see
// undelivered):
//
//   - a rework run's pull request must have gained a commit since the run
//     started: its head now (refs/pull/<n>/head, from the same `git
//     ls-remote` the verifier makes) must differ from the head recorded at
//     run start, before the agent's first turn. No GitHub CLI is involved.
//   - the pull request must not be a draft. Git refs carry no draft flag,
//     so this is read from the runner's existing `gh pr view` query. When gh
//     cannot answer (absent, unauthenticated, offline) the draft state is
//     unknown and the pull request counts as before.
//
// A pull request that fails either check leaves the turn unfinished, so it
// is continued (turn_continuation.go); a session still in that state when
// its continuations run out ends not delivered, naming the reason.

// pullRequestLookupTimeout bounds one remote lookup.
const pullRequestLookupTimeout = 15 * time.Second

// maxPullRequestLookups bounds the remote lookups one verification pass may
// spend, so a transcript full of URLs cannot stall the session end.
const maxPullRequestLookups = 4

// maxPullRequestCandidates bounds the distinct candidate URLs a stream keeps.
const maxPullRequestCandidates = 8

// pullRequestRefLookup reads refs from the checkout's origin remote and
// returns the commit of each named ref that exists there.
type pullRequestRefLookup func(ctx context.Context, worktreePath string, refs ...string) (map[string]string, error)

// pullRequestHeadLookup reads the checkout's local HEAD commit.
type pullRequestHeadLookup func(ctx context.Context, worktreePath string) (string, error)

// localHeadSHA is the production local-HEAD read: the checkout's own HEAD
// commit via captureHeadSHA.
func localHeadSHA(ctx context.Context, worktreePath string) (string, error) {
	return captureHeadSHA(ctx, worktreePath)
}

// pullRequestDraftLookup reports whether the pull request at url is a draft.
type pullRequestDraftLookup func(ctx context.Context, worktreePath, url string) (bool, error)

// githubPullRequestDraft is the production draft read: the runner's `gh pr
// view` query, in the checkout.
func githubPullRequestDraft(ctx context.Context, worktreePath, url string) (bool, error) {
	view, err := viewGitHubPullRequest(ctx, worktreePath, url)
	if err != nil {
		return false, err
	}
	return view.IsDraft, nil
}

// originRefs is the production lookup: `git ls-remote origin <refs>` in the
// checkout, with the credential and remote the session already uses.
func originRefs(ctx context.Context, worktreePath string, refs ...string) (map[string]string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, pullRequestLookupTimeout)
	defer cancel()
	out, err := gitStdout(lookupCtx, worktreePath, nil, append([]string{"ls-remote", "origin"}, refs...)...)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		wanted[ref] = struct{}{}
	}
	found := make(map[string]string, len(refs))
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		// ls-remote matches patterns by suffix; keep exact names only.
		if _, exact := wanted[ref]; exact && headSHARE.MatchString(sha) {
			found[ref] = sha
		}
	}
	return found, nil
}

// candidateOutcome is what the verifier learned about one candidate URL.
type candidateOutcome int

const (
	// The zero value: not looked at yet (or skipped for budget).
	_ candidateOutcome = iota
	// candidateAccepted: the session's own pull request.
	candidateAccepted
	// candidateRefused: confirmed not the session's pull request, for good
	// (another repository, no such pull request): never checked again.
	candidateRefused
	// candidateHeadMismatch: a pull request on the session's repository
	// whose head is not (yet) the session's. Not cached: GitHub moves
	// refs/pull/<n>/head a few seconds after each push, so an agent that
	// pushes to its open pull request and reports it in the same turn can
	// be seen before the ref catches up. It is re-read on every later
	// check, and until it matches it does not count as the session's.
	candidateHeadMismatch
	// candidateUnconfirmed: on the session's repository, but the remote
	// could not be read to confirm or refuse it.
	candidateUnconfirmed
)

// sessionPullRequestVerifier decides which candidate URL, if any, is this
// session's pull request. One verifier lives for one session; it remembers the
// URL it accepted so a later turn that only quotes some other URL cannot
// displace a verified pull request, and what it learned about every other
// candidate.
type sessionPullRequestVerifier struct {
	// repository is the session repository's canonical lower-case
	// "owner/repo", or "" when the session's repository is not on GitHub
	// (then no GitHub pull request can be the session's).
	repository string
	// branch is the branch the runner owns for this session.
	branch string
	// worktreePath is the selected repository's checkout.
	worktreePath string
	lookup       pullRequestRefLookup
	draftLookup  pullRequestDraftLookup
	headLookup   pullRequestHeadLookup
	// startHead is, for a rework run that continues an existing pull
	// request, that pull request's head when the run started, recorded
	// before the agent's first turn (see reworkStartHead); "" for a session
	// that opens its own pull request.
	startHead string
	accepted  string
	outcomes  map[string]candidateOutcome
}

// newSessionPullRequestVerifier resolves the session's GitHub repository:
// the declared selected repository's source, else the dispatched repository,
// else the checkout's origin remote — each in any remote form (https, ssh,
// scp-like, with or without credentials in it). A repository-free workarea
// resolves to none without running git (that contract forbids any git
// invocation).
//
// startHead is the rework start head (reworkStartHead); "" for a session
// that opens its own pull request.
func (r *Runner) newSessionPullRequestVerifier(ctx context.Context, qw QueuedWork, declaration *workarea.NormalizedDeclaration, worktreePath, branch, startHead string, repositoryFree bool) *sessionPullRequestVerifier {
	v := &sessionPullRequestVerifier{
		branch: branch, worktreePath: worktreePath, startHead: startHead,
		lookup: r.pullRequestLookup, draftLookup: r.pullRequestDraftLookup,
		headLookup: r.pullRequestHeadLookup,
		outcomes:   map[string]candidateOutcome{},
	}
	if v.lookup == nil {
		v.lookup = originRefs
	}
	if v.draftLookup == nil {
		v.draftLookup = githubPullRequestDraft
	}
	if v.headLookup == nil {
		v.headLookup = localHeadSHA
	}
	switch {
	case repositoryFree:
	case declaration != nil:
		v.repository = githubRepositorySlug(declaration.Selected.Source.Repository)
	default:
		v.repository = githubRepositorySlug(qw.Repository)
	}
	if v.repository == "" && !repositoryFree && strings.TrimSpace(worktreePath) != "" {
		originCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if origin, err := gitStdout(originCtx, worktreePath, nil, "remote", "get-url", "origin"); err == nil {
			v.repository = githubRepositorySlug(origin)
		}
		cancel()
	}
	return v
}

// githubRepositorySlug returns the lower-case "owner/repo" of a GitHub
// repository address in any form git accepts — https://github.com/o/r(.git),
// the same with credentials in it, ssh://[user@]github.com[:port]/o/r(.git),
// scp-like [user@]github.com:o/r(.git), or bare github.com/o/r — or "" for
// anything that is not a GitHub repository.
func githubRepositorySlug(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	var host, path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return ""
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		host, path = u.Hostname(), u.Path
	default:
		// scp-like [user@]host:path, or a bare host/path.
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		if colon := strings.Index(s, ":"); colon > 0 && !strings.Contains(s[:colon], "/") {
			host, path = s[:colon], s[colon+1:]
		} else if slash := strings.Index(s, "/"); slash > 0 {
			host, path = s[:slash], s[slash:]
		}
	}
	switch strings.ToLower(host) {
	case "github.com", "www.github.com", "ssh.github.com":
	default:
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// verify reports what candidate is — the outcome to remember for it — with
// the reason when it is not this session's pull request, and whether it
// spent a remote lookup.
func (v *sessionPullRequestVerifier) verify(ctx context.Context, candidate string) (outcome candidateOutcome, reason string, lookedUp bool) {
	slug, number, err := parseCanonicalGitHubPullRequestURL(candidate)
	if err != nil {
		return candidateRefused, "not a canonical GitHub pull request URL", false
	}
	if v.repository == "" {
		return candidateRefused, "the session's repository is not a GitHub repository", false
	}
	if normalizeGitHubRepositorySlug(slug) != v.repository {
		return candidateRefused, "the pull request is on another repository", false
	}
	pullRef := fmt.Sprintf("refs/pull/%d/head", number)
	refs := []string{pullRef}
	branchRef := ""
	if v.branch != "" {
		branchRef = "refs/heads/" + v.branch
		refs = append(refs, branchRef)
	}
	found, err := v.lookup(ctx, v.worktreePath, refs...)
	if err != nil {
		return candidateUnconfirmed, "could not read the pull request from the session's remote: " + err.Error(), true
	}
	pullHead := found[pullRef]
	if pullHead == "" {
		return candidateRefused, "no such pull request on the session's repository", true
	}
	if branchRef != "" && found[branchRef] == pullHead {
		return candidateAccepted, "", true
	}
	shaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sha, shaErr := captureHeadSHA(shaCtx, v.worktreePath)
	cancel()
	if shaErr == nil && sha == pullHead {
		return candidateAccepted, "", true
	}
	return candidateHeadMismatch, "the pull request's head is neither the session branch nor the session commit", true
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
		if v.outcomes[candidate] == candidateRefused {
			continue
		}
		if lookups >= maxPullRequestLookups {
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "not checked: lookup budget spent"})
			continue
		}
		outcome, reason, lookedUp := v.verify(ctx, candidate)
		if lookedUp {
			lookups++
		}
		v.outcomes[candidate] = outcome
		if outcome == candidateAccepted {
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

// reportsOwnRepository reports whether turn carried a pull request URL on the
// session's own repository that the verifier has NOT found to be someone
// else's — one it could not read from the remote, or did not get to. Such a
// turn reported a pull request, so it did not stop early; the nudge and the
// backstop then settle it as they always have. A URL whose pull request has
// another head (for now) or does not exist does not count, and neither does
// the accepted pull request: whether it finished the turn is judged from the
// envelope and undelivered. A nil verifier reports none.
func (v *sessionPullRequestVerifier) reportsOwnRepository(turn streamObservation) bool {
	if v == nil || v.repository == "" {
		return false
	}
	for _, candidate := range turn.pullRequestCandidates {
		if candidate == v.accepted {
			continue
		}
		slug, _, err := parseCanonicalGitHubPullRequestURL(candidate)
		if err != nil || normalizeGitHubRepositorySlug(slug) != v.repository {
			continue
		}
		if outcome := v.outcomes[candidate]; outcome != candidateRefused && outcome != candidateHeadMismatch {
			return true
		}
	}
	return false
}

// Why the session's verified pull request does not deliver the work. They
// are receipt text: a session that ends on one names it in Result.Error.
const (
	undeliveredDraft       = "pull request still draft"
	undeliveredNoNewCommit = "no new commit"
)

// undelivered re-reads the accepted pull request and reports why it does
// not deliver the work: undeliveredNoNewCommit when this is a rework run and
// the pull request's head is still the head it had at run start, or moved
// to a commit that is not the session's own (neither the session branch's
// remote head nor the checkout's local HEAD — a push by someone else does
// not deliver the session's work), undeliveredDraft when it is a draft, ""
// when it delivers or when there is no accepted pull request. A read that
// fails is returned as err and does not count against the pull request:
// the caller logs it and the session keeps today's behaviour. A nil
// verifier reports "".
func (v *sessionPullRequestVerifier) undelivered(ctx context.Context) (reason string, err error) {
	if v == nil || v.accepted == "" {
		return "", nil
	}
	var errs []error
	if v.startHead != "" {
		_, number, parseErr := parseCanonicalGitHubPullRequestURL(v.accepted)
		if parseErr != nil {
			return "", fmt.Errorf("accepted pull request URL: %w", parseErr)
		}
		pullRef := fmt.Sprintf("refs/pull/%d/head", number)
		refs := []string{pullRef}
		branchRef := ""
		if v.branch != "" {
			branchRef = "refs/heads/" + v.branch
			refs = append(refs, branchRef)
		}
		found, lookupErr := v.lookup(ctx, v.worktreePath, refs...)
		switch {
		case lookupErr != nil:
			errs = append(errs, fmt.Errorf("read the pull request head: %w", lookupErr))
		case found[pullRef] == "" || found[pullRef] == v.startHead:
			return undeliveredNoNewCommit, nil
		case branchRef != "" && found[branchRef] != "" && found[pullRef] == found[branchRef]:
			// The new head is the session branch's remote head: the
			// session's own push. Fall through to the draft check.
		default:
			if head, headErr := v.sessionHead(ctx); headErr != nil {
				errs = append(errs, fmt.Errorf("read the session commit: %w", headErr))
			} else if head == "" || found[pullRef] != head {
				return undeliveredNoNewCommit, nil
			}
			// Else the new head is the checkout's local HEAD: the
			// session's own commit. Fall through to the draft check.
		}
	}
	if v.draftLookup != nil {
		draft, draftErr := v.draftLookup(ctx, v.worktreePath, v.accepted)
		switch {
		case draftErr != nil:
			errs = append(errs, fmt.Errorf("read the draft state: %w", draftErr))
		case draft:
			return undeliveredDraft, nil
		}
	}
	return "", errors.Join(errs...)
}

// sessionHead reads the checkout's local HEAD commit: the session's own
// commit. A nil head lookup (verifiers built by hand in tests) reads it
// through captureHeadSHA, like production.
func (v *sessionPullRequestVerifier) sessionHead(ctx context.Context) (string, error) {
	lookup := v.headLookup
	if lookup == nil {
		lookup = localHeadSHA
	}
	headCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return lookup(headCtx, v.worktreePath)
}

// reworkStartHead is the head of the existing pull request a rework run
// continues, as it was when the run started: the dispatched pull request's
// recorded head (the provisioner refuses any other tip), else, for a run
// provisioned at an existing branch, the commit the session's checkout
// started at — recorded right after provisioning, before the agent's first
// turn. "" for a run that opens its own pull request.
func reworkStartHead(qw QueuedWork, res *Result, worktreePath string) string {
	if qw.PullRequest != nil && headSHARE.MatchString(qw.PullRequest.HeadSHA) {
		return qw.PullRequest.HeadSHA
	}
	if trimRef(qw.Ref) == "" {
		return ""
	}
	for _, target := range res.rescueTargets {
		if target.path == worktreePath {
			return target.base
		}
	}
	return ""
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
