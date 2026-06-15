package strategies

import (
	"context"
	"fmt"
)

// MergeCommitStrategy performs a standard merge commit (--no-ff) from the source
// branch into the target, preserving full branch history with an explicit merge
// commit. All checkouts are detached against origin/<branch> refs.
//
// Ported from donmai-libraries merge-queue/strategies/merge-commit-strategy.ts.
type MergeCommitStrategy struct{}

// compile-time assertion.
var _ Strategy = (*MergeCommitStrategy)(nil)

// Name returns the strategy identifier.
func (s *MergeCommitStrategy) Name() string { return NameMerge }

// Prepare — stub: not yet ported.
func (s *MergeCommitStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	_ = ctx
	_ = c
	return PrepareResult{}, fmt.Errorf("MergeCommitStrategy.Prepare: %w", errNotImplemented)
}

// Execute — stub: not yet ported.
func (s *MergeCommitStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	_ = ctx
	_ = c
	return MergeResult{}, fmt.Errorf("MergeCommitStrategy.Execute: %w", errNotImplemented)
}

// Finalize — stub: not yet ported.
func (s *MergeCommitStrategy) Finalize(ctx context.Context, c Context) error {
	_ = ctx
	_ = c
	return fmt.Errorf("MergeCommitStrategy.Finalize: %w", errNotImplemented)
}
