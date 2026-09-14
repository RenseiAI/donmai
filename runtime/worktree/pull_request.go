package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// originRemote is the remote `git clone` creates. Fetching by remote NAME
// rather than by URL is what makes "the same credentialed remote the clone
// used" literally true: the URL stored under it is the one the clone
// succeeded with, including the userinfo-stripped rewrite the credential
// seam performs (see provisionOnceWithReference).
const originRemote = "origin"

// Sentinel errors for dispatched-pull-request provisioning. All three are
// deterministic — isRetriable refuses them, so a session fails once with a
// stated reason instead of burning three spawn attempts on an outcome that
// cannot change.
var (
	// ErrPullRequestFetch is returned when the pull request's head could not
	// be brought into the clone at all: the ref does not exist (closed and
	// pruned, wrong number, a forge that does not publish refs/pull/*), the
	// remote refused the fetch, or the fetched ref will not resolve.
	ErrPullRequestFetch = errors.New("runtime/worktree: fetch pull request head")

	// ErrPullRequestRepositoryMismatch is returned when the record is bound to
	// a different repository than the one this session clones. It is raised
	// BEFORE any git command: the fetch would otherwise be the first thing to
	// notice, by which point the session has already reached out with a
	// credential on behalf of a record it should have refused.
	//
	// It is the comparison's own sentinel rather than a second one wrapping
	// it, so a caller can classify with either package's name and the stated
	// reason says the thing once.
	ErrPullRequestRepositoryMismatch = workarea.ErrPullRequestRepositoryMismatch

	// ErrPullRequestHeadMismatch is returned when the fetched tip is a
	// different commit from the one the dispatch recorded — the pull request
	// moved between dispatch and provisioning. It is NOT a transport failure
	// and must never be retried into success: the session is refused so no
	// review, approval or follow-up commit is attributed to a head nobody
	// chose.
	ErrPullRequestHeadMismatch = errors.New("runtime/worktree: pull request head moved after dispatch")
)

// fetchPullRequestHead materializes spec.PullRequest into a freshly cloned
// workarea and proves the result is the exact commit the dispatch recorded.
// It is a no-op when the spec carries no pull-request record, which keeps every
// pre-existing clone byte-identical.
//
// The sequence is deliberate:
//
//  1. Validate the record. A malformed number, head commit or local ref, or a
//     record with no owner/repo, is refused before any git invocation, so
//     nothing attacker-shaped can reach a refspec.
//  2. Check the record's owner/repo against the repository this session
//     clones. A record naming a different repository is refused here, still
//     before any git command — the session never reaches out on its behalf,
//     and there is no path by which another remote or another credential
//     could be used: step 3 fetches from `origin` and nothing else.
//  3. Fetch refs/pull/<n>/head into the record's local ref, over `origin`.
//     The forge serves that ref read-only from the BASE repository, so it
//     resolves with the credential the clone already used even when the head
//     lives on a fork. No token is minted, and none is handed to the agent.
//  4. Resolve the local ref and compare it byte-for-byte with the recorded
//     head. A difference fails the session naming BOTH commits.
//  5. Require the recorded base commit to be reachable. It is not fetched
//     separately and there is no fallback to baseRef.
//
// Errors are wrapped so the runner's existing clone-failure channel carries
// them verbatim into the session's stated failure reason. No credential is
// ever formatted into a message: git output is included, but the remote is
// named as "origin" and the repository URL — which may embed userinfo — is
// not.
func (m *Manager) fetchPullRequestHead(ctx context.Context, dst string, spec ProvisionSpec) error {
	pr := spec.PullRequest
	if pr == nil {
		return nil
	}
	if err := pr.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrPullRequestFetch, err)
	}
	if err := pr.BindsToRepository(spec.RepoURL); err != nil {
		return fmt.Errorf("runtime/worktree: refusing to provision pull request #%d: %w", pr.Number, err)
	}
	localRef := pr.ResolvedLocalRef()
	remoteRef := pr.RemoteHeadRef()

	if out, err := m.runGit(ctx, spec.RepoURL, "-C", dst, "fetch", "--no-tags", originRemote,
		"+"+remoteRef+":"+localRef); err != nil {
		return fmt.Errorf("%w: pull request #%d %s into %s: %w (%s)",
			ErrPullRequestFetch, pr.Number, remoteRef, localRef, err, gitDetail(out))
	}

	out, err := m.runGit(ctx, "", "-C", dst, "rev-parse", "--verify", "--end-of-options", localRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("%w: resolve %s after fetching pull request #%d: %w (%s)",
			ErrPullRequestFetch, localRef, pr.Number, err, gitDetail(out))
	}
	fetched := strings.TrimSpace(string(out))
	if !strings.EqualFold(fetched, pr.HeadSHA) {
		return fmt.Errorf("%w: pull request #%d dispatched at head %s but %s now resolves to %s; refusing to run against a head the dispatch did not choose",
			ErrPullRequestHeadMismatch, pr.Number, pr.HeadSHA, localRef, fetched)
	}

	if err := m.requirePullRequestBase(ctx, dst, pr); err != nil {
		return err
	}
	m.logger.Debug("dispatched pull request head fetched and verified",
		"sessionId", spec.SessionID, "pullRequest", pr.Number, "localRef", localRef, "headSha", pr.HeadSHA)
	return nil
}

// requirePullRequestBase requires the recorded base commit to be reachable in
// the clone once the head has been fetched. It is NOT fetched separately and
// there is deliberately no fallback to baseRef: a base commit the clone cannot
// see at this point means the base was rewritten since dispatch, and fetching
// the base branch's CURRENT tip instead would silently substitute a different
// comparison point — a diff, and any review resting on it, against something
// nobody chose. Failing names the commit so the dispatch can be redone.
//
// A record with no BaseSHA skips this entirely: the base is context for a diff,
// and a work source that does not name one is not asking for it.
func (m *Manager) requirePullRequestBase(ctx context.Context, dst string, pr *workarea.PullRequestV1) error {
	if pr.BaseSHA == "" {
		return nil
	}
	if m.hasCommit(ctx, dst, pr.BaseSHA) {
		return nil
	}
	return fmt.Errorf("%w: pull request #%d base commit %s (%s) is not reachable in the clone after fetching the head; the base moved after dispatch",
		ErrPullRequestFetch, pr.Number, pr.BaseSHA, baseRefLabel(pr))
}

// baseRefLabel names the base branch in a failure reason, or says plainly that
// the record did not carry one.
func baseRefLabel(pr *workarea.PullRequestV1) string {
	if ref := strings.TrimSpace(pr.BaseRef); ref != "" {
		return "baseRef " + ref
	}
	return "no baseRef recorded"
}

// hasCommit reports whether an object id names a commit already present in the
// clone. It is a pure local lookup — no remote, no credential.
func (m *Manager) hasCommit(ctx context.Context, dst, commit string) bool {
	_, err := m.runGit(ctx, "", "-C", dst, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// gitDetail trims git's combined output for inclusion in an error message and
// bounds it, so a pathological remote cannot push the session's stated failure
// reason past what a human or a log line can read.
func gitDetail(out []byte) string {
	detail := strings.TrimSpace(string(out))
	const maxDetail = 512
	if len(detail) > maxDetail {
		return detail[:maxDetail] + "…"
	}
	return detail
}
