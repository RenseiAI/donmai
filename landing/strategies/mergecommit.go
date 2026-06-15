package strategies

import (
	"context"
	"fmt"
)

// MergeCommitStrategy performs a standard merge commit (--no-ff) from the source
// branch into the target, preserving full branch history with an explicit merge
// commit. All checkouts are detached against origin/<branch> refs so the worker
// never takes a local branch ref (see rebase.go for the full rationale).
//
// Ported from donmai-libraries merge-queue/strategies/merge-commit-strategy.ts.
type MergeCommitStrategy struct {
	runner commandRunner
}

// compile-time assertion.
var _ Strategy = (*MergeCommitStrategy)(nil)

// NewMergeCommitStrategy returns a MergeCommitStrategy backed by the production runner.
func NewMergeCommitStrategy() *MergeCommitStrategy {
	return &MergeCommitStrategy{runner: defaultRunner}
}

func (s *MergeCommitStrategy) r() commandRunner {
	if s.runner == nil {
		return defaultRunner
	}
	return s.runner
}

// Name returns the strategy identifier.
func (s *MergeCommitStrategy) Name() string { return NameMerge }

// Prepare cleans the worktree, fetches both branches, and detaches HEAD at the
// target tip, capturing the HEAD SHA.
func (s *MergeCommitStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	if err := cleanWorktreeState(ctx, s.r(), c.WorktreePath); err != nil {
		return PrepareResult{}, fmt.Errorf("MergeCommitStrategy.Prepare clean worktree: %w", err)
	}
	if _, err := s.r().run(ctx, c.WorktreePath, nil, "git", "fetch", c.Remote, c.TargetBranch, c.SourceBranch); err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	if _, err := s.r().run(ctx, c.WorktreePath, nil, "git", "checkout", "--detach", c.Remote+"/"+c.TargetBranch); err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	headSHA, err := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return classifyPrepareError(err, c.SourceBranch), nil
	}
	return PrepareResult{Success: true, HeadSHA: headSHA}, nil
}

// Execute merges the source into the detached target HEAD with --no-ff. On
// conflict it aborts and reports the conflicting files; on any other failure it
// aborts and reports the error.
func (s *MergeCommitStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	msg := fmt.Sprintf("Merge PR #%d from %s", c.Proposal, c.SourceBranch)
	_, err := s.r().run(ctx, c.WorktreePath, nil,
		"git", "merge", "--no-ff", c.Remote+"/"+c.SourceBranch, "-m", msg)
	if err == nil {
		mergedSHA, rpErr := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
		if rpErr != nil {
			return MergeResult{Status: StatusError, Error: errMessage(rpErr)}, nil
		}
		return MergeResult{Status: StatusSuccess, MergedSHA: mergedSHA}, nil
	}

	if conflictFiles := collectConflictFiles(ctx, s.r(), c.WorktreePath); len(conflictFiles) > 0 {
		_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "merge", "--abort")
		return MergeResult{
			Status:          StatusConflict,
			ConflictFiles:   conflictFiles,
			ConflictDetails: fmt.Sprintf("Merge conflict in %d file(s)", len(conflictFiles)),
		}, nil
	}

	_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "merge", "--abort")
	return MergeResult{Status: StatusError, Error: errMessage(err)}, nil
}

// Finalize pushes the merge commit (HEAD) to the target branch via an explicit
// refspec — a fast-forward because HEAD has origin/<target> as its first parent.
func (s *MergeCommitStrategy) Finalize(ctx context.Context, c Context) error {
	if _, err := s.r().run(ctx, c.WorktreePath, nil,
		"git", "push", c.Remote, "HEAD:"+c.TargetBranch); err != nil {
		return fmt.Errorf("MergeCommitStrategy.Finalize push target: %w", err)
	}
	return nil
}
