package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
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

// continueBranchRef returns the fully-qualified branch ref for a
// continued head branch name, so a fetch names the branch explicitly.
// A short name would let a same-named remote tag win, because git
// matches tags before branches when resolving a bare refspec.
func continueBranchRef(headRef string) string {
	ref := strings.TrimSpace(headRef)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return "refs/heads/" + ref
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
	if _, err := runGit(ctx, worktreePath, gitIdentity{}, "fetch", "origin", continueBranchRef(headRef)); err != nil {
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

type continueRangeInspection struct {
	commitCount   int
	hasCodeChange bool
	scratchPaths  []string
}

func (i continueRangeInspection) delivers() bool {
	return i.hasCodeChange && len(i.scratchPaths) == 0
}

// inspectContinueRange classifies the commits in from..to for the continued
// -pull-request delivery gate. Delivery requires at least one non-merge commit
// in the range that changes a path outside the runner scratch set; any commit
// in the range touching a scratch path is reported separately so the caller can
// fail without rewriting history.
func inspectContinueRange(ctx context.Context, worktreePath, from, to string) (continueRangeInspection, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || strings.EqualFold(from, to) {
		return continueRangeInspection{}, nil
	}
	rng := from + ".." + to
	commits, err := continueRangeCommits(ctx, worktreePath, rng)
	if err != nil {
		return continueRangeInspection{}, err
	}
	if len(commits) == 0 {
		return continueRangeInspection{}, nil
	}
	nonMerges, err := continueRangeCommits(ctx, worktreePath, "--no-merges", rng)
	if err != nil {
		return continueRangeInspection{}, err
	}
	nonMergeSet := make(map[string]struct{}, len(nonMerges))
	for _, commit := range nonMerges {
		nonMergeSet[commit] = struct{}{}
	}
	inspection := continueRangeInspection{commitCount: len(commits)}
	scratchSet := map[string]struct{}{}
	for _, commit := range commits {
		paths, pathErr := continueCommitPaths(ctx, worktreePath, commit)
		if pathErr != nil {
			return continueRangeInspection{}, pathErr
		}
		for _, path := range paths {
			if isContinueScratchPath(path) {
				scratchSet[path] = struct{}{}
				continue
			}
			if _, ok := nonMergeSet[commit]; ok {
				inspection.hasCodeChange = true
			}
		}
	}
	if len(scratchSet) > 0 {
		inspection.scratchPaths = make([]string, 0, len(scratchSet))
		for path := range scratchSet {
			inspection.scratchPaths = append(inspection.scratchPaths, path)
		}
		sort.Strings(inspection.scratchPaths)
	}
	return inspection, nil
}

func continueRangeCommits(ctx context.Context, worktreePath string, args ...string) ([]string, error) {
	out, err := gitStdout(ctx, worktreePath, nil, append([]string{"rev-list", "--reverse", "--first-parent"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("runner: list continued pull request commits: %w", err)
	}
	return filterEmpty(strings.Split(strings.TrimSpace(out), "\n")), nil
}

func continueCommitPaths(ctx context.Context, worktreePath, commit string) ([]string, error) {
	out, err := gitStdout(ctx, worktreePath, nil, "diff-tree", "--no-commit-id", "--name-only", "-r", "-m", "--root", commit)
	if err != nil {
		return nil, fmt.Errorf("runner: list continued pull request paths for %s: %w", commit, err)
	}
	seen := map[string]struct{}{}
	paths := make([]string, 0)
	for _, path := range filterEmpty(strings.Split(strings.TrimSpace(out), "\n")) {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

func isContinueScratchPath(path string) bool {
	parts := strings.Split(strings.TrimSpace(path), "/")
	if len(parts) <= 1 {
		return false
	}
	return harnessstate.IsStateDir(parts[0])
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
	if _, err := runGit(probeCtx, worktreePath, gitIdentity{}, "fetch", "origin", continueBranchRef(headRef)); err != nil {
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
