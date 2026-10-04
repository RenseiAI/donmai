package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// ErrContinuePullRequestDiverged marks a continue-mode push the remote
// refused as a non-fast-forward: the pull request's head moved after
// dispatch, so pushing the session's commits would overwrite work the
// session cannot vouch for. The caller surfaces the typed reason instead
// of forcing the push.
var ErrContinuePullRequestDiverged = errors.New("runner: continued pull request head moved after dispatch")

// validateContinuePullRequest reports whether the session-spec record can
// drive a continue-mode run. Nil is ordinary branch work and is valid.
// A non-nil record requires a positive number, a usable branch name, and
// a full-length hex head commit.
func validateContinuePullRequest(cpr *prompt.ContinuePullRequest) error {
	if cpr == nil {
		return nil
	}
	if cpr.Number <= 0 {
		return fmt.Errorf("runner: continued pull request number must be positive, got %d", cpr.Number)
	}
	if err := validateRef(strings.TrimSpace(cpr.HeadRef)); err != nil {
		return fmt.Errorf("runner: continued pull request head ref: %w", err)
	}
	if !headSHARE.MatchString(strings.TrimSpace(cpr.HeadSha)) {
		return fmt.Errorf("runner: continued pull request head sha must be a full commit id")
	}
	return nil
}

// continuePullRequestBranch resolves the working branch for a continue-mode
// run: the pull request's head branch. Empty when not continuing.
func continuePullRequestBranch(cpr *prompt.ContinuePullRequest) string {
	if cpr == nil {
		return ""
	}
	return strings.TrimSpace(cpr.HeadRef)
}

// continuePullRequestURL builds the run's pull request URL from the session
// repository and the continued number. It resolves the repository the same
// way the verifier does — the declared selected source, else the dispatched
// repository, else the checkout's origin remote — and returns "" when the
// repository is not on GitHub, so a non-GitHub checkout never fabricates a
// github.com URL.
func continuePullRequestURL(ctx context.Context, qw QueuedWork, declaration *workarea.NormalizedDeclaration, worktreePath string) string {
	if qw.ContinuePullRequest == nil || qw.ContinuePullRequest.Number <= 0 {
		return ""
	}
	repository := ""
	if declaration != nil {
		repository = githubRepositorySlug(declaration.Selected.Source.Repository)
	} else {
		repository = githubRepositorySlug(qw.Repository)
	}
	if repository == "" && strings.TrimSpace(worktreePath) != "" {
		originCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if origin, err := gitStdout(originCtx, worktreePath, nil, "remote", "get-url", "origin"); err == nil {
			repository = githubRepositorySlug(origin)
		}
		cancel()
	}
	if repository == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repository, qw.ContinuePullRequest.Number)
}

// checkoutContinuePullRequest checks out the pull request's head branch at
// exactly the dispatched head commit. The worktree is cloned at the remote
// default branch, so the branch may exist only on the remote: fetch it
// first, then check it out at the pinned commit. A checkout whose HEAD is
// not the pinned commit fails the session rather than running against a
// head nobody chose.
func checkoutContinuePullRequest(ctx context.Context, worktreePath string, cpr *prompt.ContinuePullRequest) error {
	headRef := strings.TrimSpace(cpr.HeadRef)
	headSha := strings.TrimSpace(cpr.HeadSha)
	if _, err := runGit(ctx, worktreePath, gitIdentity{}, "fetch", "origin", headRef); err != nil {
		return fmt.Errorf("runner: fetch continued pull request branch %q: %w", headRef, err)
	}
	// Prove the fetched head IS the dispatched head before checking it
	// out: a stale HeadSha would otherwise check out a commit the remote
	// no longer serves as the branch head while looking like success.
	remoteHead, err := runGit(ctx, worktreePath, gitIdentity{}, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("runner: resolve continued pull request branch %q: %w", headRef, err)
	}
	if !strings.EqualFold(strings.TrimSpace(remoteHead), headSha) {
		return fmt.Errorf("runner: continued pull request branch %q is %s, want dispatched head %s; refusing to run against a moved head", headRef, strings.TrimSpace(remoteHead), headSha)
	}
	if _, err := runGit(ctx, worktreePath, gitIdentity{}, "checkout", "-B", headRef, headSha); err != nil {
		return fmt.Errorf("runner: checkout continued pull request branch %q at %s: %w", headRef, headSha, err)
	}
	head, err := captureHeadSHA(ctx, worktreePath)
	if err != nil {
		return fmt.Errorf("runner: read continued pull request checkout: %w", err)
	}
	if !strings.EqualFold(head, headSha) {
		return fmt.Errorf("runner: continued pull request checkout is %s, want dispatched head %s", head, headSha)
	}
	return nil
}

// continueDelivered reports whether a continue-mode run delivered work
// onto the continued pull request: the session's checkout moved past the
// dispatched head commit. A run that changed nothing — local HEAD still
// the dispatched head, or a local HEAD the remote head does not carry —
// is not delivered, mirroring the verifier's no-new-commit rule.
func continueDelivered(localHead, remoteHead, startHead string) bool {
	localHead = strings.TrimSpace(localHead)
	remoteHead = strings.TrimSpace(remoteHead)
	startHead = strings.TrimSpace(startHead)
	if localHead == "" || startHead == "" {
		return false
	}
	if strings.EqualFold(localHead, startHead) {
		return false
	}
	if remoteHead == "" {
		return false
	}
	return strings.EqualFold(remoteHead, localHead)
}

// continueDiverged reports whether a backstop report carries the typed
// continue-mode divergence signal (see runBackstop). A declared session's
// aggregate report folds every repository's flag into its own, so the
// top-level flag is enough.
func continueDiverged(report *agent.BackstopReport) bool {
	if report == nil {
		return false
	}
	if report.ContinueDiverged {
		return true
	}
	for _, repository := range report.Repositories {
		if repository.Report.ContinueDiverged {
			return true
		}
	}
	return false
}

// continueHeadDiverged probes whether the continued head branch moved on
// the remote after dispatch: it fetches the head ref, then asks whether
// the fetched head is an ancestor of the session's HEAD. A remote head
// that is not an ancestor means the push would not fast-forward, so the
// refusal is divergence; a remote head that IS an ancestor means some
// other policy refused an otherwise fast-forward push. A failed probe
// (fetch or ancestry check erroring for any other reason) reports false:
// divergence must be proven, never assumed from an unreadable remote.
func continueHeadDiverged(ctx context.Context, worktreePath, headRef string) bool {
	if strings.TrimSpace(headRef) == "" || strings.TrimSpace(worktreePath) == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := runGit(probeCtx, worktreePath, gitIdentity{}, "fetch", "origin", headRef); err != nil {
		return false
	}
	_, err := runGit(probeCtx, worktreePath, gitIdentity{}, "merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true
	}
	return false
}
