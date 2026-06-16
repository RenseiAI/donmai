package landing

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/landing/strategies"
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

	running bool

	// Test seams — production defaults are installed by NewPool.
	//
	// newWorker builds a Worker for processing one proposal in a batch; tests
	// inject a fake.
	newWorker func(cfg WorkerConfig, deps WorkerDeps) batchWorker
	// buildManifests computes file manifests for the queued proposals; tests
	// stub it to drive the conflict graph without a real repo.
	buildManifests func(ctx context.Context, repoPath string, entries []ManifestEntry, targetBranch, remote string) ([]FileManifest, error)
	// fetchRefs fetches latest refs before manifest building (best-effort).
	fetchRefs func(ctx context.Context, repoPath, remote string)
	// sleep is the cancellable poll sleep; tests stub it.
	sleep func(ctx context.Context, d time.Duration)
	// worktrees creates and removes the per-proposal isolated worktrees so
	// concurrent batch members never share one working tree; tests inject a fake.
	worktrees worktreeManager
}

// worktreeManager creates and removes a dedicated git worktree for one in-flight
// proposal. The production implementation shells out to `git worktree
// add/remove` (see strategies.AddWorktree / strategies.RemoveWorktree); tests
// inject a fake that records add/remove calls without touching git.
type worktreeManager interface {
	// add creates an isolated worktree at worktreePath checked out at
	// targetBranch.
	add(ctx context.Context, repoPath, worktreePath, targetBranch string) error
	// remove tears the worktree down (always run in a defer, even on error).
	remove(ctx context.Context, repoPath, worktreePath string) error
}

// gitWorktreeManager is the production worktreeManager backed by the strategies
// git-worktree helpers.
type gitWorktreeManager struct{}

func (gitWorktreeManager) add(ctx context.Context, repoPath, worktreePath, targetBranch string) error {
	return strategies.AddWorktree(ctx, repoPath, worktreePath, targetBranch)
}

func (gitWorktreeManager) remove(ctx context.Context, repoPath, worktreePath string) error {
	return strategies.RemoveWorktree(ctx, repoPath, worktreePath)
}

// batchWorker is the Pool's view of a Worker (one method) so tests can supply a
// fake that records dispatch order without running git.
type batchWorker interface {
	ProcessEntry(ctx context.Context, e Entry) (ProcessResult, error)
}

// NewPool returns a Pool wired to production Worker construction and manifest
// building.
func NewPool(cfg PoolConfig, deps WorkerDeps) *Pool {
	return &Pool{
		cfg:  cfg,
		deps: deps,
		newWorker: func(c WorkerConfig, d WorkerDeps) batchWorker {
			return NewWorker(c, d)
		},
		buildManifests: BuildFileManifests,
		fetchRefs: func(ctx context.Context, repoPath, remote string) {
			if _, err := defaultRunner.run(ctx, repoPath, nil, "git", "fetch", remote); err != nil {
				slog.Debug("landing pool: git fetch failed (non-fatal)", "err", err)
			}
		},
		sleep:     sleepCtx,
		worktrees: gitWorktreeManager{},
	}
}

// Start acquires the coordinator lock and loops processing batches until ctx is
// cancelled or Stop is called. When Concurrency <= 1 it delegates to a single
// Worker for parity with the legacy single-instance path.
//
// Ported from MergePool.start.
func (p *Pool) Start(ctx context.Context) error {
	if err := validateKey(p.cfg.Key); err != nil {
		return err
	}
	if p.cfg.Concurrency <= 1 {
		return p.newSingleWorker().Start(ctx)
	}

	lockKey := p.cfg.Key.lockKey()
	// Unique per-Start token so release is a compare-and-delete (never frees a
	// lock another coordinator re-acquired after ours expired).
	lockToken, err := newLockToken()
	if err != nil {
		return fmt.Errorf("Pool.Start mint lock token: %w", err)
	}
	acquired, err := p.deps.Redis.SetNX(ctx, lockKey, lockToken, lockTTL)
	if err != nil {
		return fmt.Errorf("Pool.Start acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("another landing worker/pool is already running for %s", p.cfg.Key)
	}

	p.running = true
	stopHeartbeat := p.startHeartbeat(ctx, lockKey, lockToken)
	defer func() {
		stopHeartbeat()
		if _, delErr := p.deps.Redis.DelIfMatches(context.WithoutCancel(ctx), lockKey, lockToken); delErr != nil {
			slog.Warn("landing pool: failed to release lock", "key", lockKey, "err", delErr)
		}
		p.running = false
	}()

	for p.running && ctx.Err() == nil {
		paused, perr := p.deps.Redis.Get(ctx, p.cfg.Key.pausedKey())
		if perr != nil {
			slog.Warn("landing pool: paused-flag read failed", "err", perr)
		}
		if paused != "" {
			p.sleep(ctx, p.cfg.PollInterval)
			continue
		}

		results, berr := p.processBatch(ctx)
		if berr != nil {
			return fmt.Errorf("Pool.Start process batch: %w", berr)
		}
		if len(results) == 0 {
			p.sleep(ctx, p.cfg.PollInterval)
		}
	}
	return nil
}

// Stop requests a graceful shutdown after the current batch finishes.
//
// Ported from MergePool.stop.
func (p *Pool) Stop() { p.running = false }

// newSingleWorker builds the delegate Worker used for the Concurrency <= 1 path.
func (p *Pool) newSingleWorker() *Worker {
	return NewWorker(p.cfg.WorkerConfig, p.deps)
}

// processBatch processes one batch of non-conflicting proposals concurrently. It
// returns the per-proposal results (empty when the queue is empty or no batch
// could be formed). Each result is recorded in storage / bubbled via a Worker.
func (p *Pool) processBatch(ctx context.Context) ([]ProcessResult, error) {
	// 1. Peek all queued proposals.
	entries, err := p.deps.Storage.PeekAll(ctx, p.cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("peek all: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	// 2. Fetch latest refs (best-effort).
	remote := p.cfg.Remote
	if remote == "" {
		remote = "origin"
	}
	p.fetchRefs(ctx, p.cfg.RepoPath, remote)

	// 3. Build file manifests for all queued proposals.
	manifestEntries := make([]ManifestEntry, 0, len(entries))
	for _, e := range entries {
		manifestEntries = append(manifestEntries, ManifestEntry{Proposal: e.Proposal, SourceBranch: e.SourceBranch})
	}
	targetBranch := p.cfg.TargetBranch
	if targetBranch == "" {
		targetBranch = "main"
	}
	manifests, err := p.buildManifests(ctx, p.cfg.RepoPath, manifestEntries, targetBranch, remote)
	if err != nil {
		return nil, fmt.Errorf("build manifests: %w", err)
	}

	// 4. Build conflict graph and find independent batches (size-capped by
	// concurrency).
	graph := BuildConflictGraph(manifests)
	batches := graph.IndependentBatches(p.cfg.Concurrency)
	if len(batches) == 0 {
		return nil, nil
	}

	// 5. Take the first (highest-priority non-conflicting) batch.
	batch := batches[0]

	// 6. Dequeue the batch atomically so concurrent coordinators never double-
	// dispatch a proposal.
	dequeued, err := p.deps.Storage.DequeueBatch(ctx, p.cfg.Key, batch)
	if err != nil {
		return nil, fmt.Errorf("dequeue batch: %w", err)
	}
	if len(dequeued) == 0 {
		return nil, nil
	}

	// 7. Process every proposal in the batch concurrently. The proposals are
	// non-conflicting by construction so parallel landing is safe.
	results := make([]ProcessResult, len(dequeued))
	var wg sync.WaitGroup
	for i, entry := range dequeued {
		wg.Add(1)
		go func(i int, entry Entry) {
			defer wg.Done()
			results[i] = p.processOne(ctx, entry)
		}(i, entry)
	}
	wg.Wait()

	// 8. Record each disposition via a Worker (markCompleted/markFailed/etc.).
	for i, entry := range dequeued {
		w := p.newSingleWorker()
		if err := w.handleResult(ctx, entry, results[i]); err != nil {
			slog.Warn("landing pool: failed to record result", "proposal", entry.Proposal, "err", err)
		}
	}
	return results, nil
}

// processOne lands a single proposal via a fresh Worker in its OWN dedicated
// worktree, normalizing any error into a ProcessError result so one failed
// proposal never aborts the batch. The per-proposal worktree isolates concurrent
// batch members: without it they would all run the strategy in p.cfg.RepoPath and
// clobber each other's index, checkout, and lock-file regeneration. The worktree
// is always removed (defer), even when creation succeeds but processing errors.
func (p *Pool) processOne(ctx context.Context, entry Entry) ProcessResult {
	cfg := p.cfg.WorkerConfig

	targetBranch := cfg.TargetBranch
	if targetBranch == "" {
		targetBranch = "main"
	}
	worktreePath := p.proposalWorktreePath(entry.Proposal)
	if err := p.worktrees.add(ctx, cfg.RepoPath, worktreePath, targetBranch); err != nil {
		// Without an isolated worktree we cannot safely land in parallel, so fail
		// this proposal rather than fall back to the shared tree.
		return ProcessResult{Proposal: entry.Proposal, Status: ProcessError, Message: fmt.Errorf("create worktree: %w", err).Error()}
	}
	defer func() {
		if err := p.worktrees.remove(context.WithoutCancel(ctx), cfg.RepoPath, worktreePath); err != nil {
			slog.Warn("landing pool: failed to remove proposal worktree", "proposal", entry.Proposal, "path", worktreePath, "err", err)
		}
	}()
	cfg.WorktreePath = worktreePath

	w := p.newWorker(cfg, p.deps)
	result, err := w.ProcessEntry(ctx, entry)
	if err != nil {
		return ProcessResult{Proposal: entry.Proposal, Status: ProcessError, Message: err.Error()}
	}
	return result
}

// proposalWorktreePath derives a stable, per-proposal worktree directory beside
// the repo. Two proposals always get distinct paths, so concurrent batch members
// never collide on one working tree.
func (p *Pool) proposalWorktreePath(proposal int) string {
	dir := ".landing-worktrees/" + p.cfg.Key.OrgID + "-" + sanitizeRepoID(p.cfg.Key.RepoID) + "-" + strconv.Itoa(proposal)
	return filepath.Join(p.cfg.RepoPath, dir)
}

// sanitizeRepoID makes a repoId (often "owner/repo") safe to embed in a directory
// name by replacing path separators.
func sanitizeRepoID(repoID string) string {
	return filepath.Base(repoID)
}

// startHeartbeat extends the lock TTL on an interval until the returned stop
// func is called. lockToken is the value stored under lockKey: the extension
// is a compare-and-extend (heartbeatExtend) so we never refresh a lock another
// coordinator re-acquired after ours expired.
func (p *Pool) startHeartbeat(ctx context.Context, lockKey, lockToken string) func() {
	ticker := time.NewTicker(heartbeatInterval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				matched, err := heartbeatExtend(ctx, p.deps.Redis, lockKey, lockToken, lockTTL)
				if err != nil {
					slog.Debug("landing pool: heartbeat expire failed", "key", lockKey, "err", err)
				} else if !matched {
					slog.Warn("landing pool: lock lost — heartbeat will stop extending", "key", lockKey)
				}
			}
		}
	}()
	var stopped bool
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
	}
}
