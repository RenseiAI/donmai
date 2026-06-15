package strategies

import (
	"context"
	"fmt"
)

// SquashStrategy squash-merges all commits from the source branch into a single
// commit on the target, producing a clean linear history with one commit per
// proposal. All checkouts are detached against origin/<branch> refs.
//
// Ported from donmai-libraries merge-queue/strategies/squash-strategy.ts.
type SquashStrategy struct{}

// compile-time assertion.
var _ Strategy = (*SquashStrategy)(nil)

// Name returns the strategy identifier.
func (s *SquashStrategy) Name() string { return NameSquash }

// Prepare — stub: not yet ported.
func (s *SquashStrategy) Prepare(ctx context.Context, c Context) (PrepareResult, error) {
	_ = ctx
	_ = c
	return PrepareResult{}, fmt.Errorf("SquashStrategy.Prepare: %w", errNotImplemented)
}

// Execute — stub: not yet ported.
func (s *SquashStrategy) Execute(ctx context.Context, c Context) (MergeResult, error) {
	_ = ctx
	_ = c
	return MergeResult{}, fmt.Errorf("SquashStrategy.Execute: %w", errNotImplemented)
}

// Finalize — stub: not yet ported.
func (s *SquashStrategy) Finalize(ctx context.Context, c Context) error {
	_ = ctx
	_ = c
	return fmt.Errorf("SquashStrategy.Finalize: %w", errNotImplemented)
}
