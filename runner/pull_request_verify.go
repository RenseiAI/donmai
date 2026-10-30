package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/prompt"
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

// pullRequestLookupTimeout bounds all remote lookups in one verification
// pass. One pass shares a single deadline so a transcript full of URLs
// cannot stall the session end.
const pullRequestLookupTimeout = 15 * time.Second

// pullRequestLookupChunkSize bounds the pull refs in one ls-remote call:
// one call with tens of thousands of refs fails with "argument list too
// long" and would mark every candidate unconfirmed. Reads go in priority
// order in chunks of this size (the session branch ref rides along in each
// chunk), verified chunk by chunk, stopping at the first accepted pull
// request. A failed chunk marks only its own candidates unconfirmed.
const pullRequestLookupChunkSize = 256

// Candidate URLs are kept without a bound: verification reads them in
// chunked batched remote lookups, so a transcript full of URLs costs
// ceil(candidates/chunkSize) ls-remote calls sharing one overall deadline
// (bounded by pullRequestLookupTimeout) instead of stalling the session
// end. Retention is bounded by the transcript itself — every candidate
// consumes input bytes — and every hint the stream carried must still be
// present when the verifier runs: the session pull request is verified, not
// voted on. A bounded window let a flood of unrelated URLs evict the
// genuine session pull request before verification (a narrative hint oldest
// under tool-result chatter, or an earlier inject turn's hint after union),
// flipping a landed session to agent-blocked.

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
	// inspectRange classifies the session's commits between from and to on
	// branch for the delivery progress check. It defaults to inspectContinueRange; tests stub it
	// so a re-read that only exercises remote lookups never shells out
	// to git in a directory that is not a checkout.
	inspectRange func(ctx context.Context, worktreePath, branch, from, to string) (continueRangeInspection, error)
	// startHead is, for a rework run that continues an existing pull
	// request, that pull request's head when the run started, recorded
	// before the agent's first turn (see reworkStartHead); "" for a session
	// that opens its own pull request.
	startHead string
	// lastHead is the accepted pull request's head at the latest re-read
	// (undelivered); it starts at startHead.
	lastHead string
	// delivery is the dispatch-declared delivery policy for the run's
	// pull request (see prompt.DeliveryPolicy). Nil is today's
	// behaviour: a draft is undelivered, merges alone do not deliver.
	delivery *prompt.DeliveryPolicy
	accepted string
	outcomes map[string]candidateOutcome
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
		branch: branch, worktreePath: worktreePath, startHead: startHead, lastHead: startHead,
		delivery: qw.Delivery,
		lookup:   r.pullRequestLookup, draftLookup: r.pullRequestDraftLookup,
		headLookup: r.pullRequestHeadLookup, inspectRange: inspectContinueRange,
		outcomes: map[string]candidateOutcome{},
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

	// Distinct candidates in priority order: envelope first, then the
	// turn's candidates newest-first, then the previously accepted URL.
	distinct := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		distinct = append(distinct, c)
	}
	// A previously accepted URL short-circuits: everything after its
	// first occurrence is never evaluated, so it is never fetched.
	acceptedPos := -1
	for i, c := range distinct {
		if c == v.accepted && v.accepted != "" {
			acceptedPos = i
			break
		}
	}
	eval := distinct
	if acceptedPos >= 0 {
		eval = distinct[:acceptedPos+1]
	}
	// Collect the same-repository pull refs a batched remote read must
	// cover. Other-repository and malformed URLs are refused without a
	// lookup; already-refused URLs are skipped without a lookup or a
	// rejection, exactly like the per-candidate path they replace.
	type need struct {
		url    string
		number int
	}
	var needs []need
	needSet := map[string]struct{}{}
	branchRef := ""
	if v.branch != "" {
		branchRef = "refs/heads/" + v.branch
	}
	for _, c := range eval {
		if c == v.accepted && v.accepted != "" {
			continue
		}
		if v.outcomes[c] == candidateRefused {
			continue
		}
		slug, number, err := parseCanonicalGitHubPullRequestURL(c)
		if err != nil || v.repository == "" || normalizeGitHubRepositorySlug(slug) != v.repository {
			continue
		}
		if _, dup := needSet[c]; dup {
			continue
		}
		needSet[c] = struct{}{}
		needs = append(needs, need{url: c, number: number})
	}
	lookup := v.lookup
	if lookup == nil {
		lookup = originRefs
	}
	// Chunked remote reads in priority order: needs is already envelope
	// first, then newest candidate first. Each chunk carries its own
	// branch ref so every chunk can accept on the session branch head.
	// Chunks are fetched lazily in evaluation order and share one overall
	// deadline; a failed chunk marks only its own candidates unconfirmed.
	var lookupCtx context.Context
	var lookupCancel context.CancelFunc
	if len(needs) > 0 {
		lookupCtx, lookupCancel = context.WithTimeout(ctx, pullRequestLookupTimeout)
		defer lookupCancel()
	}
	needIndex := make(map[string]int, len(needs))
	for i, n := range needs {
		needIndex[n.url] = i
	}
	numChunks := (len(needs) + pullRequestLookupChunkSize - 1) / pullRequestLookupChunkSize
	foundChunks := make([]map[string]string, numChunks)
	errChunks := make([]error, numChunks)
	fetched := make([]bool, numChunks)
	fetchChunk := func(chunk int) {
		if chunk < 0 || chunk >= numChunks || fetched[chunk] {
			return
		}
		fetched[chunk] = true
		start := chunk * pullRequestLookupChunkSize
		end := start + pullRequestLookupChunkSize
		if end > len(needs) {
			end = len(needs)
		}
		refs := make([]string, 0, end-start+1)
		for _, n := range needs[start:end] {
			refs = append(refs, fmt.Sprintf("refs/pull/%d/head", n.number))
		}
		if branchRef != "" {
			refs = append(refs, branchRef)
		}
		found, err := lookup(lookupCtx, v.worktreePath, refs...)
		if found == nil {
			found = map[string]string{}
		}
		foundChunks[chunk] = found
		errChunks[chunk] = err
	}
	// The checkout's own commit is read once, only when a candidate's
	// pull head is present but is not the session branch's head.
	var localHEAD string
	var localErr error
	localRead := false
	readLocal := func() (string, error) {
		if !localRead {
			localRead = true
			shaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			localHEAD, localErr = captureHeadSHA(shaCtx, v.worktreePath)
		}
		return localHEAD, localErr
	}
	accepted := ""
	for _, candidate := range eval {
		if candidate == v.accepted && v.accepted != "" {
			accepted = candidate
			break
		}
		if v.outcomes[candidate] == candidateRefused {
			continue
		}
		slug, number, err := parseCanonicalGitHubPullRequestURL(candidate)
		if err != nil {
			v.outcomes[candidate] = candidateRefused
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "not a canonical GitHub pull request URL"})
			continue
		}
		if v.repository == "" {
			v.outcomes[candidate] = candidateRefused
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "the session's repository is not a GitHub repository"})
			continue
		}
		if normalizeGitHubRepositorySlug(slug) != v.repository {
			v.outcomes[candidate] = candidateRefused
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "the pull request is on another repository"})
			continue
		}
		idx, ok := needIndex[candidate]
		if !ok {
			// Unreachable: same-repository candidates are collected into
			// needs above; anything else was refused as malformed,
			// non-GitHub, or foreign. Treat as unconfirmed rather than
			// accepting on a missing read.
			v.outcomes[candidate] = candidateUnconfirmed
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "could not read the pull request from the session's remote: no batched read covers this candidate"})
			continue
		}
		chunk := idx / pullRequestLookupChunkSize
		fetchChunk(chunk)
		if errChunks[chunk] != nil {
			v.outcomes[candidate] = candidateUnconfirmed
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "could not read the pull request from the session's remote: " + errChunks[chunk].Error()})
			continue
		}
		found := foundChunks[chunk]
		pullRef := fmt.Sprintf("refs/pull/%d/head", number)
		pullHead := found[pullRef]
		if pullHead == "" {
			v.outcomes[candidate] = candidateRefused
			rejected = append(rejected, pullRequestRejection{url: candidate, reason: "no such pull request on the session's repository"})
			continue
		}
		if branchRef != "" && found[branchRef] == pullHead && found[branchRef] != "" {
			v.outcomes[candidate] = candidateAccepted
			accepted = candidate
			break
		}
		if sha, shaErr := readLocal(); shaErr == nil && sha == pullHead {
			v.outcomes[candidate] = candidateAccepted
			accepted = candidate
			break
		}
		v.outcomes[candidate] = candidateHeadMismatch
		rejected = append(rejected, pullRequestRejection{url: candidate, reason: "the pull request's head is neither the session branch nor the session commit"})
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
// envelope and undelivered. A rework or continued run whose pull request is
// accepted reports none: it delivers onto that pull request, so no other URL
// the turn carried (a sibling quoted in review comments, one settle never
// looked up because the accepted one came first) can be its result. A nil
// verifier reports none.
func (v *sessionPullRequestVerifier) reportsOwnRepository(turn streamObservation) bool {
	if v == nil || v.repository == "" {
		return false
	}
	if v.startHead != "" && v.accepted != "" {
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
// when it delivers or when there is no accepted pull request.
//
// Under a dispatch-declared delivery policy (see prompt.DeliveryPolicy)
// the draft check is skipped when the policy allows drafts, and the
// delivery inspection counts merge resolutions when the policy allows
// merges; every other check still applies, so a draft without the run's
// own new commit still ends undelivered, naming the missing commit.
//
// moved reports that the pull request gained a deliverable commit of the
// session's own since the previous re-read: a non-merge commit outside the
// scratch set (or, under the merge allowance, a merge carrying the run's
// own resolution). A draft or rework that keeps gaining only merges or scratch
// commits is not making delivery progress, so the caller keeps counting its
// continuations against the undelivered bound
// (turnFollowUps.undeliveredSent).
//
// A read that fails is returned as err and does not count against the pull
// request: the caller logs it and the session keeps today's behaviour. A nil
// verifier reports "".
func (v *sessionPullRequestVerifier) undelivered(ctx context.Context) (reason string, moved bool, err error) {
	if v == nil || v.accepted == "" {
		return "", false, nil
	}
	var errs []error
	inspect := v.inspectRange
	if inspect == nil {
		inspect = inspectContinueRange
	}
	delivers := func(inspection continueRangeInspection) bool {
		return inspection.deliversUnder(v.delivery)
	}
	readProgress := func(head string, own bool) {
		previous := v.lastHead
		if head != "" {
			v.lastHead = head
		}
		if !own || previous == "" || head == "" || head == previous {
			return
		}
		inspection, inspectErr := inspect(ctx, v.worktreePath, v.branch, previous, head)
		if inspectErr != nil {
			errs = append(errs, fmt.Errorf("inspect continued pull request progress: %w", inspectErr))
			return
		}
		moved = delivers(inspection)
	}
	if v.startHead != "" {
		head, own, readErr := v.readHead(ctx)
		readProgress(head, own)
		switch {
		case readErr != nil && head == "":
			errs = append(errs, readErr)
		case head == "" || head == v.startHead:
			return undeliveredNoNewCommit, moved, nil
		case readErr != nil:
			// The session commit could not be read, so whose commit the
			// new head is stays unknown, and an unknown does not count
			// against the pull request. Fall through to the draft check.
			errs = append(errs, readErr)
		case !own:
			return undeliveredNoNewCommit, moved, nil
		default:
			inspection, inspectErr := inspect(ctx, v.worktreePath, v.branch, v.startHead, head)
			if inspectErr != nil {
				errs = append(errs, fmt.Errorf("inspect continued pull request delivery: %w", inspectErr))
			} else if !delivers(inspection) {
				return undeliveredNoNewCommit, moved, nil
			}
		}
	}
	if !v.delivery.AllowsDraft() && v.draftLookup != nil {
		draft, draftErr := v.draftLookup(ctx, v.worktreePath, v.accepted)
		switch {
		case draftErr != nil:
			errs = append(errs, fmt.Errorf("read the draft state: %w", draftErr))
		case draft:
			if v.startHead == "" {
				// A draft the session opened: read its head as well, so
				// only a deliverable push to it counts as progress.
				head, own, readErr := v.readHead(ctx)
				readProgress(head, own)
				if readErr != nil {
					errs = append(errs, readErr)
				}
			}
			return undeliveredDraft, moved, errors.Join(errs...)
		}
	}
	return "", moved, errors.Join(errs...)
}

// readHead reads the accepted pull request's head with the verifier's own
// remote query (refs/pull/<n>/head beside the session branch), and whether
// that head is a commit of the session's own: the session branch's remote
// head, or the checkout's local HEAD. err reports a read that failed: with
// head "" the remote could not be read; with a head, the session commit
// could not be, and own is false.
func (v *sessionPullRequestVerifier) readHead(ctx context.Context) (head string, own bool, err error) {
	_, number, parseErr := parseCanonicalGitHubPullRequestURL(v.accepted)
	if parseErr != nil {
		return "", false, fmt.Errorf("accepted pull request URL: %w", parseErr)
	}
	pullRef := fmt.Sprintf("refs/pull/%d/head", number)
	refs := []string{pullRef}
	branchRef := ""
	if v.branch != "" {
		branchRef = "refs/heads/" + v.branch
		refs = append(refs, branchRef)
	}
	found, lookupErr := v.lookup(ctx, v.worktreePath, refs...)
	if lookupErr != nil {
		return "", false, fmt.Errorf("read the pull request head: %w", lookupErr)
	}
	head = found[pullRef]
	switch {
	case head == "":
		return "", false, nil
	case branchRef != "" && found[branchRef] == head:
		return head, true, nil
	case head == v.lastHead && (v.startHead == "" || head == v.startHead):
		// The head has not moved, and no rework judgement turns on whose
		// commit it is: the local HEAD is not read.
		return head, false, nil
	}
	local, headErr := v.sessionHead(ctx)
	if headErr != nil {
		return head, false, fmt.Errorf("read the session commit: %w", headErr)
	}
	return head, local == head, nil
}

// sessionHead reads the checkout's local HEAD commit: the session's own
// commit. The runner's verifier reads it through captureHeadSHA; a verifier
// built without a head lookup reads none, rather than running git in
// whatever directory the process happens to be in.
func (v *sessionPullRequestVerifier) sessionHead(ctx context.Context) (string, error) {
	if v.headLookup == nil {
		return "", errors.New("no session head lookup")
	}
	headCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return v.headLookup(headCtx, v.worktreePath)
}

// reworkStartHead is the head of the existing pull request a rework run
// continues, as it was when the run started: the dispatched pull request's
// recorded head (the provisioner refuses any other tip), else, for a run
// provisioned at an existing branch, the commit the session's checkout
// started at — recorded right after provisioning, before the agent's first
// turn. "" for a run that opens its own pull request.
func reworkStartHead(qw QueuedWork, res *Result, worktreePath string) string {
	if qw.ContinuePullRequest != nil && headSHARE.MatchString(qw.ContinuePullRequest.HeadSha) {
		return qw.ContinuePullRequest.HeadSha
	}
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

// seedContinuedPullRequest accepts the continued pull request as the
// run's own before tail recovery: the seed goes through the same settle
// path as a pull request the agent reported, so verification (same
// repository, head at the session branch or commit) and the no-new-commit
// and draft delivery rules apply unchanged. A nil verifier (work that owes
// no pull request) leaves the envelope untouched.
func (r *Runner) seedContinuedPullRequest(v *sessionPullRequestVerifier, continuedURL string, res *Result, obs *streamObservation) {
	if v == nil || strings.TrimSpace(continuedURL) == "" {
		return
	}
	seed := streamObservation{pullRequestURL: continuedURL, pullRequestCandidates: []string{continuedURL}}
	for _, rejection := range v.settle(context.Background(), res, obs, seed) {
		r.logger.Warn("continued pull request is not this session's pull request; ignored",
			"url", rejection.url,
			"reason", rejection.reason,
		)
	}
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
// in order, keeping each URL once at its latest position. The list is
// unbounded: dropping an older hint under a flood of newer URLs is what
// flipped landed sessions to agent-blocked, so no hint is ever evicted.
// Retention stays proportional to the transcript that carried the URLs.
func appendPullRequestCandidates(candidates []string, text string) []string {
	for _, u := range prURLRE.FindAllString(text, -1) {
		for i, existing := range candidates {
			if existing == u {
				candidates = append(candidates[:i], candidates[i+1:]...)
				break
			}
		}
		candidates = append(candidates, u)
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
