package strategies

import (
	"context"
	"fmt"
)

// RebaseStrategy rebases the source branch onto the target branch, then
// fast-forwards the target on the remote, producing a linear history without
// merge commits.
//
// Detached-HEAD checkouts: every checkout uses --detach against a remote
// tracking ref (origin/<branch>) rather than the local branch name. Git enforces
// per-branch exclusivity across worktrees, so a plain `git checkout <branch>`
// fails when another worktree holds <branch>. Detached checkouts bypass that
// lock, and updates go through refspec pushes (HEAD:<branch>) so no local branch
// ref is ever needed.
//
// Ported from donmai-libraries merge-queue/strategies/rebase-strategy.ts.
type RebaseStrategy struct {
	runner commandRunner
	// rebasedSha is captured during Execute so Finalize can push without a
	// local branch ref. The strategy is single-use per proposal (the factory
	// returns a fresh instance), replacing the legacy per-context WeakMap.
	rebasedSha string
}

// compile-time assertion.
var _ Strategy = (*RebaseStrategy)(nil)

// NewRebaseStrategy returns a RebaseStrategy backed by the production runner.
func NewRebaseStrategy() *RebaseStrategy {
	return &RebaseStrategy{runner: defaultRunner}
}

func (s *RebaseStrategy) r() commandRunner {
	if s.runner == nil {
		return defaultRunner
	}
	return s.runner
}

// Name returns the strategy identifier.
func (s *RebaseStrategy) Name() string { return NameRebase }

// Prepare cleans the worktree, fetches both branches, and detaches HEAD at the
// source tip, capturing the HEAD SHA.
func (s *RebaseStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	if err := cleanWorktreeState(ctx, s.r(), c.WorktreePath); err != nil {
		return PrepareResult{}, fmt.Errorf("RebaseStrategy.Prepare clean worktree: %w", err)
	}
	if _, err := s.r().run(ctx, c.WorktreePath, nil, "git", "fetch", c.Remote, c.TargetBranch, c.SourceBranch); err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	// Detached checkout at the source tip — bypasses the per-branch worktree lock.
	if _, err := s.r().run(ctx, c.WorktreePath, nil, "git", "checkout", "--detach", c.Remote+"/"+c.SourceBranch); err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	headSHA, err := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	return PrepareResult{Success: true, HeadSHA: headSHA}, nil
}

// Execute rebases the detached HEAD onto the target. On conflict it aborts and
// reports the conflicting files; on any other failure it aborts and reports the
// error.
func (s *RebaseStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	_, err := s.r().run(ctx, c.WorktreePath, nil, "git", "rebase", c.Remote+"/"+c.TargetBranch)
	if err == nil {
		mergedSHA, rpErr := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
		if rpErr != nil {
			return MergeResult{Status: StatusError, Error: errMessage(rpErr)}, nil
		}
		s.rebasedSha = mergedSHA
		return MergeResult{Status: StatusSuccess, MergedSHA: mergedSHA}, nil
	}

	// Determine whether the failure was a conflict.
	if conflictFiles := collectConflictFiles(ctx, s.r(), c.WorktreePath); len(conflictFiles) > 0 {
		_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "rebase", "--abort")
		return MergeResult{
			Status:          StatusConflict,
			ConflictFiles:   conflictFiles,
			ConflictDetails: fmt.Sprintf("Rebase conflict in %d file(s)", len(conflictFiles)),
		}, nil
	}

	// Not a conflict — abort and surface the original error.
	_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "rebase", "--abort")
	return MergeResult{Status: StatusError, Error: errMessage(err)}, nil
}

// Finalize pushes the rebased commits back to the source branch (force-with-lease)
// then fast-forwards the target on the remote to the rebased SHA. Both use
// explicit refspecs so no local branch ref is needed from the detached HEAD.
func (s *RebaseStrategy) Finalize(ctx context.Context, c Context) error {
	rebasedSHA := s.rebasedSha
	if rebasedSHA == "" {
		out, err := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("RebaseStrategy.Finalize resolve HEAD: %w", err)
		}
		rebasedSHA = out
	}

	// Push the rebased commits to the source branch. --force-with-lease guards
	// against another writer advancing the branch since our fetch.
	if _, err := s.r().run(ctx, c.WorktreePath, nil,
		"git", "push", c.Remote, "HEAD:"+c.SourceBranch, "--force-with-lease="+c.SourceBranch); err != nil {
		return fmt.Errorf("RebaseStrategy.Finalize push source: %w", err)
	}

	// Fast-forward the target on the remote to the rebased SHA.
	if _, err := s.r().run(ctx, c.WorktreePath, nil,
		"git", "push", c.Remote, rebasedSHA+":"+c.TargetBranch); err != nil {
		return fmt.Errorf("RebaseStrategy.Finalize push target: %w", err)
	}
	return nil
}

// classifyPrepareError maps a git failure during Prepare to a PrepareResult,
// flagging branch-conflict failures retryable and missing-source-ref failures
// as already-merged. Shared by every strategy.
//
// Ported from the catch block shared by the three TS strategies' prepare().
func classifyPrepareError(err error, sourceBranch string) PrepareResult {
	msg := errMessage(err)
	if isBranchConflictError(msg) {
		return PrepareResult{Success: false, Error: msg, Retryable: true}
	}
	if isMissingRemoteRefError(msg, sourceBranch) {
		return PrepareResult{Success: false, Error: msg, AlreadyMerged: true}
	}
	return PrepareResult{Success: false, Error: msg}
}
