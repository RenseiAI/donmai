package landing

import (
	"context"
	"fmt"
)

// PoolConfig configures a Pool. It embeds WorkerConfig and adds concurrency.
//
// Ported from MergePoolConfig in donmai-libraries merge-queue/merge-pool.ts.
type PoolConfig struct {
	WorkerConfig
	// Concurrency is the maximum number of proposals landed in parallel. <= 1
	// delegates to a single Worker.
	Concurrency int
}

// Pool orchestrates concurrent landings. It peeks all queued proposals, builds a
// conflict graph, finds independent batches of non-conflicting proposals, and
// dispatches the highest-priority batch to parallel worker slots. When
// Concurrency <= 1 it delegates to a single Worker for parity.
//
// Ported from MergePool in donmai-libraries merge-queue/merge-pool.ts.
type Pool struct {
	cfg  PoolConfig
	deps WorkerDeps
}

// NewPool returns a Pool.
func NewPool(cfg PoolConfig, deps WorkerDeps) *Pool {
	return &Pool{cfg: cfg, deps: deps}
}

// Start acquires the coordinator lock and loops processing batches until ctx is
// cancelled or Stop is called.
//
// Stub: not yet ported.
func (p *Pool) Start(ctx context.Context) error {
	_ = ctx
	return fmt.Errorf("Pool.Start: %w", ErrNotImplemented)
}

// Stop requests a graceful shutdown.
//
// Stub: not yet ported.
func (p *Pool) Stop() {}
