package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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

// inspectContinueRange classifies the commits the session added between from
// and to for the continued-pull-request delivery gate: every commit reachable
// from to but not from from, nor from any remote-tracking branch other than
// branch's own (the pull request's head branch, which the session pushes to).
// A side branch the session committed on and merged in is its work; the base
// branch, or any other published branch it merged, is someone else's.
// Delivery requires at least one non-merge commit of the session's that
// changes a path outside the runner scratch set; any of its commits adding or
// modifying a scratch path is reported separately so the caller can fail
// without rewriting history. Removing a scratch path is cleanup, not a scratch
// commit, and a merge counts only for what it changed against every parent.
func inspectContinueRange(ctx context.Context, worktreePath, branch, from, to string) (continueRangeInspection, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || strings.EqualFold(from, to) {
		return continueRangeInspection{}, nil
	}
	commits, err := continueRangeCommits(ctx, worktreePath, branch, from, to)
	if err != nil {
		return continueRangeInspection{}, err
	}
	if len(commits) == 0 {
		return continueRangeInspection{}, nil
	}
	nonMerges, err := continueRangeCommits(ctx, worktreePath, branch, from, to, "--no-merges")
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
		changes, pathErr := continueCommitChanges(ctx, worktreePath, commit)
		if pathErr != nil {
			return continueRangeInspection{}, pathErr
		}
		for _, change := range changes {
			switch {
			case !isContinueScratchPath(change.path):
				if _, ok := nonMergeSet[commit]; ok {
					inspection.hasCodeChange = true
				}
			case !change.deleted:
				scratchSet[change.path] = struct{}{}
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

// continueRangeCommits lists the session's commits between from and to (see
// inspectContinueRange). With no branch every remote-tracking branch is
// excluded, so a pushed head reads as nothing new: the check fails closed.
func continueRangeCommits(ctx context.Context, worktreePath, branch, from, to string, flags ...string) ([]string, error) {
	args := append([]string{"rev-list", "--reverse"}, flags...)
	args = append(args, to, "--not", from)
	if branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"); branch != "" {
		args = append(args, "--exclude=origin/"+branch)
	}
	args = append(args, "--remotes")
	out, err := gitStdout(ctx, worktreePath, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("runner: list continued pull request commits: %w", err)
	}
	return filterEmpty(strings.Split(strings.TrimSpace(out), "\n")), nil
}

// continueChange is one path a commit changed, and whether the commit
// deleted it.
type continueChange struct {
	path    string
	deleted bool
}

// continueCommitChanges lists the paths commit itself changed. --cc keeps a
// non-merge commit's diff as is and narrows a merge to the paths it changed
// against every parent (its own conflict resolution or additions).
func continueCommitChanges(ctx context.Context, worktreePath, commit string) ([]continueChange, error) {
	out, err := gitStdout(ctx, worktreePath, nil, "diff-tree", "--no-commit-id", "--name-status", "-z", "-r", "--cc", "--root", commit)
	if err != nil {
		return nil, fmt.Errorf("runner: list continued pull request paths for %s: %w", commit, err)
	}
	fields := strings.Split(strings.Trim(out, "\x00\n"), "\x00")
	changes := make([]continueChange, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := strings.TrimSpace(fields[i]), fields[i+1]
		if status == "" || path == "" {
			continue
		}
		changes = append(changes, continueChange{path: path, deleted: strings.Trim(status, "D") == ""})
	}
	return changes, nil
}

// continueScratchDirs are the top-level directories a continued pull
// request must never gain a commit in: the runner's own state, the pi
// harness's session storage and the seat scratch. harnessstate also lists
// the Claude Code and Codex CLI directories, which the backstop never
// auto-commits; a repository may track those as project content (agent
// settings, skills), so a commit the agent made there is delivered work.
var continueScratchDirs = []string{harnessstate.RunnerStateDir, harnessstate.PiStateDir, harnessstate.SeatScratchDir}

// isContinueScratchPath reports whether path sits under one of
// continueScratchDirs. Only the first path component decides, so a nested
// directory that merely shares the name stays ordinary project content.
func isContinueScratchPath(path string) bool {
	top, rest, nested := strings.Cut(strings.TrimSpace(path), "/")
	return nested && rest != "" && slices.Contains(continueScratchDirs, top)
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
