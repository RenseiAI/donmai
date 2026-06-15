package strategies

import (
	"context"
	"fmt"
)

// CleanWorktreeState resets a landing worktree to a clean state at the start of
// every Prepare. The worktree is reused across proposals and any number of steps
// between Prepare and Finalize can leave it dirty (staged lock files, untracked
// install side effects, an aborted rebase). The reset aborts any in-progress
// rebase/merge/cherry-pick, resets to HEAD, and removes untracked files — but it
// does NOT clean ignored files, so dependency caches survive.
//
// Best-effort: every step's failure is swallowed because the corresponding
// precondition may not apply on a given run. The subsequent Prepare checkout
// surfaces any genuine failure.
//
// Ported from donmai-libraries merge-queue/strategies/worktree-cleanup.ts.
//
// Stub: not yet ported.
func CleanWorktreeState(ctx context.Context, worktreePath string) error {
	_ = ctx
	_ = worktreePath
	return fmt.Errorf("CleanWorktreeState: %w", errNotImplemented)
}
