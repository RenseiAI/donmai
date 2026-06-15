package strategies

import (
	"context"
	"fmt"
)

// SquashStrategy squash-merges all commits from the source branch into a single
// commit on the target, producing a clean linear history with one commit per
// proposal. All checkouts are detached against origin/<branch> refs (see rebase.go
// for the full rationale).
//
// Ported from donmai-libraries merge-queue/strategies/squash-strategy.ts.
type SquashStrategy struct {
	runner commandRunner
}

// compile-time assertion.
var _ Strategy = (*SquashStrategy)(nil)

// NewSquashStrategy returns a SquashStrategy backed by the production runner.
func NewSquashStrategy() *SquashStrategy {
	return &SquashStrategy{runner: defaultRunner}
}

func (s *SquashStrategy) r() commandRunner {
	if s.runner == nil {
		return defaultRunner
	}
	return s.runner
}

// Name returns the strategy identifier.
func (s *SquashStrategy) Name() string { return NameSquash }

// Prepare cleans the worktree, fetches both branches, and detaches HEAD at the
// target tip, capturing the HEAD SHA.
func (s *SquashStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	if err := cleanWorktreeState(ctx, s.r(), c.WorktreePath); err != nil {
		return PrepareResult{}, fmt.Errorf("SquashStrategy.Prepare clean worktree: %w", err)
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

// Execute squash-merges the source then commits, capturing the squash SHA. On
// conflict it aborts and reports the conflicting files; on any other failure it
// aborts and reports the error.
func (s *SquashStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	_, err := s.r().run(ctx, c.WorktreePath, nil, "git", "merge", "--squash", c.Remote+"/"+c.SourceBranch)
	if err == nil {
		msg := fmt.Sprintf("Squash merge PR #%d from %s", c.Proposal, c.SourceBranch)
		if _, cErr := s.r().run(ctx, c.WorktreePath, nil, "git", "commit", "-m", msg); cErr != nil {
			err = cErr
		} else {
			mergedSHA, rpErr := s.r().run(ctx, c.WorktreePath, nil, "git", "rev-parse", "HEAD")
			if rpErr != nil {
				return MergeResult{Status: StatusError, Error: errMessage(rpErr)}, nil
			}
			return MergeResult{Status: StatusSuccess, MergedSHA: mergedSHA}, nil
		}
	}

	if conflictFiles := collectConflictFiles(ctx, s.r(), c.WorktreePath); len(conflictFiles) > 0 {
		_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "merge", "--abort")
		return MergeResult{
			Status:          StatusConflict,
			ConflictFiles:   conflictFiles,
			ConflictDetails: fmt.Sprintf("Squash merge conflict in %d file(s)", len(conflictFiles)),
		}, nil
	}

	_, _ = s.r().run(ctx, c.WorktreePath, nil, "git", "merge", "--abort")
	return MergeResult{Status: StatusError, Error: errMessage(err)}, nil
}

// Finalize pushes the squash commit (HEAD) to the target branch via an explicit
// refspec — a fast-forward because HEAD descends directly from origin/<target>.
func (s *SquashStrategy) Finalize(ctx context.Context, c Context) error {
	if _, err := s.r().run(ctx, c.WorktreePath, nil,
		"git", "push", c.Remote, "HEAD:"+c.TargetBranch); err != nil {
		return fmt.Errorf("SquashStrategy.Finalize push target: %w", err)
	}
	return nil
}
