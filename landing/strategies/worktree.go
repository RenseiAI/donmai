package strategies

import "context"

// CleanWorktreeState resets a landing worktree to a clean state at the start of
// every Prepare. The worktree is reused across proposals and any number of steps
// between Prepare and Finalize can leave it dirty (staged lock files, untracked
// install side effects, an aborted rebase). The reset aborts any in-progress
// rebase/merge/cherry-pick, resets to HEAD, and removes untracked files — but it
// does NOT clean ignored files (no "-x"), so dependency caches survive.
//
// Best-effort: every step's failure is swallowed because the corresponding
// precondition may not apply on a given run (no rebase in progress, HEAD unset,
// etc.). The subsequent Prepare checkout surfaces any genuine failure.
//
// Ported from donmai-libraries merge-queue/strategies/worktree-cleanup.ts.
func CleanWorktreeState(ctx context.Context, worktreePath string) error {
	return cleanWorktreeState(ctx, defaultRunner, worktreePath)
}

// cleanWorktreeState is the runner-injectable implementation.
func cleanWorktreeState(ctx context.Context, r commandRunner, worktreePath string) error {
	// Abort anything in-flight so the reset can take effect. Each is swallowed
	// because the corresponding operation is usually not in progress.
	_, _ = r.run(ctx, worktreePath, nil, "git", "rebase", "--abort")
	_, _ = r.run(ctx, worktreePath, nil, "git", "merge", "--abort")
	_, _ = r.run(ctx, worktreePath, nil, "git", "cherry-pick", "--abort")

	// Clear the index and working tree to match HEAD. For the (typically
	// detached) landing worktree this is enough to discard staged changes from
	// the previous proposal; the detached checkout in Prepare then moves HEAD.
	_, _ = r.run(ctx, worktreePath, nil, "git", "reset", "--hard", "HEAD")

	// Remove untracked files but NOT ignored files (no "-x"), so caches such as
	// node_modules survive and Prepare stays fast.
	_, _ = r.run(ctx, worktreePath, nil, "git", "clean", "-fd")

	return nil
}
