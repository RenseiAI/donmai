package landing

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeWorktrees is a worktreeManager that records add/remove calls and the
// per-proposal worktree path each Worker was handed. Safe for concurrent use so
// the parallel-batch tests pass under -race.
type fakeWorktrees struct {
	mu      sync.Mutex
	added   []string // worktree paths passed to add, in call order
	removed []string // worktree paths passed to remove, in call order
	live    map[string]bool
	addErr  error // when set, add returns it (and must skip remove)
}

func newFakeWorktrees() *fakeWorktrees {
	return &fakeWorktrees{live: map[string]bool{}}
}

func (f *fakeWorktrees) add(_ context.Context, _, worktreePath, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, worktreePath)
	f.live[worktreePath] = true
	return nil
}

func (f *fakeWorktrees) remove(_ context.Context, _, worktreePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, worktreePath)
	delete(f.live, worktreePath)
	return nil
}

func TestPoolProcessOneUsesDedicatedWorktreePerProposal(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	for _, p := range []int{1, 2} {
		_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: p, SourceBranch: "b"})
	}
	rd := newFakeRedis()
	manifests := map[int][]string{1: {"a.go"}, 2: {"b.go"}}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake", RepoPath: "/repo"}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, st, rd, manifests, nil)

	wt := newFakeWorktrees()
	p.worktrees = wt
	// Capture the dedicated WorktreePath each per-proposal Worker was configured
	// with, proving the Pool hands each batch member its own working tree.
	var mu sync.Mutex
	var workerPaths []string
	p.newWorker = func(c WorkerConfig, _ WorkerDeps) batchWorker {
		mu.Lock()
		workerPaths = append(workerPaths, c.WorktreePath)
		mu.Unlock()
		return recordingWorker{mu: &mu, processed: &[]int{}, status: nil}
	}

	if _, err := p.processBatch(context.Background()); err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	// Two proposals → two distinct worktrees created and two removed.
	if len(wt.added) != 2 {
		t.Fatalf("added %d worktrees, want 2 (one per proposal)", len(wt.added))
	}
	if wt.added[0] == wt.added[1] {
		t.Errorf("concurrent batch members got the SAME worktree path %q; must be distinct", wt.added[0])
	}
	if len(wt.removed) != 2 {
		t.Errorf("removed %d worktrees, want 2 (cleanup must run for each)", len(wt.removed))
	}
	if len(wt.live) != 0 {
		t.Errorf("worktrees left live after batch: %v", wt.live)
	}
	// Every Worker was handed a non-empty, dedicated worktree path (not the bare
	// repo), and the two paths are distinct.
	mu.Lock()
	defer mu.Unlock()
	if len(workerPaths) != 2 {
		t.Fatalf("configured %d workers, want 2", len(workerPaths))
	}
	for _, path := range workerPaths {
		if path == "" || path == "/repo" {
			t.Errorf("worker ran in %q, want a dedicated worktree (not the bare repo)", path)
		}
	}
	if workerPaths[0] == workerPaths[1] {
		t.Errorf("both workers configured with the same worktree path %q", workerPaths[0])
	}
}

func TestPoolProcessOneRemovesWorktreeOnProcessError(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 7, SourceBranch: "b"})
	rd := newFakeRedis()
	manifests := map[int][]string{7: {"a.go"}}
	status := map[int]ProcessStatus{7: ProcessError}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake", RepoPath: "/repo"}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, st, rd, manifests, status)
	wt := newFakeWorktrees()
	p.worktrees = wt

	if _, err := p.processBatch(context.Background()); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	// Even though processing errored, the worktree must be cleaned up (defer).
	if len(wt.added) != 1 || len(wt.removed) != 1 {
		t.Errorf("added=%v removed=%v; cleanup must run on error too", wt.added, wt.removed)
	}
	if len(wt.live) != 0 {
		t.Errorf("worktree leaked after errored landing: %v", wt.live)
	}
}

func TestPoolProcessOneWorktreeAddFailureIsProcessError(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 3, SourceBranch: "b"})
	rd := newFakeRedis()
	manifests := map[int][]string{3: {"a.go"}}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake", RepoPath: "/repo"}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, st, rd, manifests, nil)
	wt := newFakeWorktrees()
	wt.addErr = errors.New("worktree add failed")
	p.worktrees = wt

	results, err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(results) != 1 || results[0].Status != ProcessError {
		t.Fatalf("worktree add failure should yield a ProcessError result; got %+v", results)
	}
	// No worktree was created, so none should be removed.
	if len(wt.removed) != 0 {
		t.Errorf("remove called %d times after add failed, want 0", len(wt.removed))
	}
}

func TestProposalWorktreePathsAreDistinct(t *testing.T) {
	p := NewPool(PoolConfig{
		WorkerConfig: WorkerConfig{Key: Key{OrgID: "org1", RepoID: "owner/repo"}, RepoPath: "/repo"},
		Concurrency:  2,
	}, WorkerDeps{Storage: newFakeStorage(), Redis: newFakeRedis()})
	a := p.proposalWorktreePath(1)
	b := p.proposalWorktreePath(2)
	if a == b {
		t.Errorf("worktree paths for distinct proposals collide: %q == %q", a, b)
	}
	if a == "/repo" || b == "/repo" {
		t.Errorf("worktree path must not be the bare repo path: a=%q b=%q", a, b)
	}
	// Stable: same proposal → same path.
	if p.proposalWorktreePath(1) != a {
		t.Errorf("worktree path for proposal 1 is not stable")
	}
}

// recordingWorker is a batchWorker that records which proposals it processed and
// returns a configured status per proposal.
type recordingWorker struct {
	mu        *sync.Mutex
	processed *[]int
	status    map[int]ProcessStatus
}

func (w recordingWorker) ProcessEntry(_ context.Context, e Entry) (ProcessResult, error) {
	w.mu.Lock()
	*w.processed = append(*w.processed, e.Proposal)
	w.mu.Unlock()
	st := ProcessMerged
	if s, ok := w.status[e.Proposal]; ok {
		st = s
	}
	return ProcessResult{Proposal: e.Proposal, Status: st}, nil
}

// newTestPool wires a Pool with injectable manifests and a recording worker.
func newTestPool(t *testing.T, cfg PoolConfig, st *fakeStorage, rd *fakeRedis, manifests map[int][]string, status map[int]ProcessStatus) (*Pool, *[]int) {
	t.Helper()
	processed := &[]int{}
	var mu sync.Mutex
	p := NewPool(cfg, WorkerDeps{Storage: st, Redis: rd})
	p.sleep = func(context.Context, time.Duration) {}
	p.fetchRefs = func(context.Context, string, string) {}
	p.buildManifests = func(_ context.Context, _ string, entries []ManifestEntry, _, _ string) ([]FileManifest, error) {
		out := make([]FileManifest, 0, len(entries))
		for _, e := range entries {
			out = append(out, FileManifest{Proposal: e.Proposal, SourceBranch: e.SourceBranch, Files: manifests[e.Proposal]})
		}
		return out, nil
	}
	p.newWorker = func(WorkerConfig, WorkerDeps) batchWorker {
		return recordingWorker{mu: &mu, processed: processed, status: status}
	}
	// Default to a no-op worktree manager so existing tests never shell out to
	// git; tests that assert on worktree behavior override p.worktrees.
	p.worktrees = newFakeWorktrees()
	return p, processed
}

func TestPoolProcessBatchNonConflicting(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	for _, p := range []int{1, 2, 3} {
		_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: p, SourceBranch: "b"})
	}
	rd := newFakeRedis()
	// Disjoint file sets → all three land together.
	manifests := map[int][]string{
		1: {"a.go"},
		2: {"b.go"},
		3: {"c.go"},
	}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake"}, Concurrency: 3}
	p, processed := newTestPool(t, cfg, st, rd, manifests, nil)

	results, err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	got := append([]int(nil), *processed...)
	sort.Ints(got)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("processed = %v, want all of [1 2 3]", got)
	}
	// All landed → all marked completed.
	if len(st.completed) != 3 {
		t.Errorf("completed = %v, want 3 entries", st.completed)
	}
	// Queue drained.
	depth, _ := st.QueueDepth(context.Background(), key)
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0", depth)
	}
}

func TestPoolProcessBatchConflictingSplitsBatches(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	for _, p := range []int{1, 2} {
		_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: p, SourceBranch: "b"})
	}
	rd := newFakeRedis()
	// Both touch shared.go → conflict; only the first batch (proposal 1) processes.
	manifests := map[int][]string{
		1: {"shared.go"},
		2: {"shared.go"},
	}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake"}, Concurrency: 2}
	p, processed := newTestPool(t, cfg, st, rd, manifests, nil)

	results, err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(results) != 1 || (*processed)[0] != 1 {
		t.Errorf("conflicting proposals should land one at a time; processed = %v", *processed)
	}
	// Proposal 2 remains queued for the next batch.
	depth, _ := st.QueueDepth(context.Background(), key)
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1 (proposal 2 deferred)", depth)
	}
}

func TestPoolProcessBatchEmptyQueue(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake"}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, newFakeStorage(), newFakeRedis(), nil, nil)
	results, err := p.processBatch(context.Background())
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 on empty queue", len(results))
	}
}

func TestPoolProcessBatchRecordsFailures(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	for _, p := range []int{1, 2} {
		_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: p, SourceBranch: "b"})
	}
	rd := newFakeRedis()
	manifests := map[int][]string{1: {"a.go"}, 2: {"b.go"}}
	status := map[int]ProcessStatus{2: ProcessTestFailure}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake", Escalation: EscalationConfig{OnTestFailure: "notify"}}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, st, rd, manifests, status)

	if _, err := p.processBatch(context.Background()); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(st.completed) != 1 || st.completed[0] != 1 {
		t.Errorf("completed = %v, want [1]", st.completed)
	}
	if st.failed[2] == "" {
		t.Errorf("proposal 2 should be marked failed; failed = %v", st.failed)
	}
}

func TestPoolStartDelegatesToWorkerWhenConcurrencyOne(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	rd := newFakeRedis()
	// Held lock proves the single-worker path runs (Worker.Start acquires the
	// same lock key and must error on contention).
	rd.lockHeld[key.lockKey()] = true
	p := NewPool(PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake"}, Concurrency: 1},
		WorkerDeps{Storage: newFakeStorage(), Redis: rd})
	err := p.Start(context.Background())
	if err == nil {
		t.Fatal("Concurrency<=1 Start should delegate to a Worker, which errors on a held lock")
	}
}

func TestPoolStartLockContention(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	rd := newFakeRedis()
	rd.lockHeld[key.lockKey()] = true
	p := NewPool(PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake"}, Concurrency: 4},
		WorkerDeps{Storage: newFakeStorage(), Redis: rd})
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Pool.Start should error when the lock is held")
	}
}

func TestPoolStartRejectsInvalidKey(t *testing.T) {
	p := NewPool(PoolConfig{WorkerConfig: WorkerConfig{Key: Key{RepoID: "owner/repo"}}, Concurrency: 2},
		WorkerDeps{Storage: newFakeStorage(), Redis: newFakeRedis()})
	if err := p.Start(context.Background()); err == nil {
		t.Error("Pool.Start with empty OrgID should error")
	}
}

func TestPoolStartLoopStops(t *testing.T) {
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	st := newFakeStorage()
	for _, p := range []int{1, 2} {
		_ = st.Enqueue(context.Background(), Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: p, SourceBranch: "b"})
	}
	rd := newFakeRedis()
	manifests := map[int][]string{1: {"a.go"}, 2: {"b.go"}}
	cfg := PoolConfig{WorkerConfig: WorkerConfig{Key: key, Strategy: "fake", PollInterval: time.Millisecond}, Concurrency: 2}
	p, _ := newTestPool(t, cfg, st, rd, manifests, nil)
	// Stop on the first empty poll (after the queue drains).
	p.sleep = func(context.Context, time.Duration) { p.Stop() }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(st.completed) != 2 {
		t.Errorf("completed = %v, want both proposals landed", st.completed)
	}
	if v, _ := rd.Get(context.Background(), key.lockKey()); v != "" {
		t.Error("lock should be released after Start returns")
	}
}
