package strategies

import (
	"context"
	"fmt"
)

// RebaseStrategy rebases the source branch onto the target branch, then
// fast-forwards the target on the remote, producing a linear history without
// merge commits. Every checkout is detached against origin/<branch> so the
// worker never takes a per-branch worktree lock.
//
// Ported from donmai-libraries merge-queue/strategies/rebase-strategy.ts.
type RebaseStrategy struct{}

// compile-time assertion.
var _ Strategy = (*RebaseStrategy)(nil)

// Name returns the strategy identifier.
func (s *RebaseStrategy) Name() string { return NameRebase }

// Prepare — stub: not yet ported.
func (s *RebaseStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	_ = ctx
	_ = c
	return PrepareResult{}, fmt.Errorf("RebaseStrategy.Prepare: %w", errNotImplemented)
}

// Execute — stub: not yet ported.
func (s *RebaseStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	_ = ctx
	_ = c
	return MergeResult{}, fmt.Errorf("RebaseStrategy.Execute: %w", errNotImplemented)
}

// Finalize — stub: not yet ported.
func (s *RebaseStrategy) Finalize(ctx context.Context, c Context) error {
	_ = ctx
	_ = c
	return fmt.Errorf("RebaseStrategy.Finalize: %w", errNotImplemented)
}
