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

// AddWorktree creates a dedicated linked git worktree at worktreePath, checked
// out at targetBranch, so a single in-flight proposal gets its own isolated
// working tree. With a concurrent landing pool, sharing one working tree across
// parallel proposals lets them clobber each other's staged index, checkout, and
// lock-file regen; a dedicated worktree per proposal isolates them.
//
// `--detach` keeps the worktree off any branch ref so two concurrent worktrees
// can both start from the same targetBranch without git's "branch already
// checked out" guard rejecting the second.
//
// Ported/extended from the worktree lifecycle in
// donmai-libraries merge-queue/strategies/worktree-cleanup.ts (which only kept
// the cleanup half).
func AddWorktree(ctx context.Context, repoPath, worktreePath, targetBranch string) error {
	return addWorktree(ctx, defaultRunner, repoPath, worktreePath, targetBranch)
}

// addWorktree is the runner-injectable implementation.
func addWorktree(ctx context.Context, r commandRunner, repoPath, worktreePath, targetBranch string) error {
	// Prune before add: if a previous run crashed after creating the worktree
	// but before the defer-remove could run, the path is still registered in
	// git's worktree list and "git worktree add" will refuse to reuse it.
	// Force-remove the stale registration first (ignored when absent), then
	// prune dangling metadata, so the subsequent add always starts clean.
	_, _ = r.run(ctx, repoPath, nil, "git", "worktree", "remove", "--force", worktreePath)
	_, _ = r.run(ctx, repoPath, nil, "git", "worktree", "prune")

	if _, err := r.run(ctx, repoPath, nil, "git", "worktree", "add", "--detach", worktreePath, targetBranch); err != nil {
		return fmt.Errorf("git worktree add %s: %w", worktreePath, err)
	}
	return nil
}

// RemoveWorktree tears down a dedicated worktree created by AddWorktree.
// `--force` is used because the worktree may carry staged or untracked changes
// from a failed landing; the caller always wants it gone regardless.
//
// Best-effort by contract: callers run it in a defer so the worktree is removed
// even when the landing errored. A removal failure is returned so the caller can
// log it, but it never aborts the surrounding flow.
func RemoveWorktree(ctx context.Context, repoPath, worktreePath string) error {
	return removeWorktree(ctx, defaultRunner, repoPath, worktreePath)
}

// removeWorktree is the runner-injectable implementation.
func removeWorktree(ctx context.Context, r commandRunner, repoPath, worktreePath string) error {
	if _, err := r.run(ctx, repoPath, nil, "git", "worktree", "remove", "--force", worktreePath); err != nil {
		return fmt.Errorf("git worktree remove %s: %w", worktreePath, err)
	}
	return nil
}
