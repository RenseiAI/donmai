package landing

import (
	"context"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/landing/vcs"
)

// ProcessStatus is the terminal disposition of processing a single proposal.
//
// Ported from MergeProcessResult.status in
// donmai-libraries merge-queue/merge-worker.ts.
type ProcessStatus string

const (
	// ProcessMerged — the proposal landed.
	ProcessMerged ProcessStatus = "merged"
	// ProcessNoop — a stale duplicate entry; the original run already landed
	// and bubbled the result. Distinct from ProcessMerged to avoid
	// double-posting a "landed" comment.
	ProcessNoop ProcessStatus = "noop"
	// ProcessConflict — an unresolved conflict.
	ProcessConflict ProcessStatus = "conflict"
	// ProcessTestFailure — tests failed against the landed code.
	ProcessTestFailure ProcessStatus = "test-failure"
	// ProcessError — an unexpected error.
	ProcessError ProcessStatus = "error"
)

// ProcessResult is the result of processing a single proposal.
type ProcessResult struct {
	Proposal int
	Status   ProcessStatus
	Message  string
}

// EscalationConfig controls how conflict and test-failure outcomes escalate.
type EscalationConfig struct {
	// OnConflict is one of "reassign", "notify", "park".
	OnConflict EscalationStrategy
	// OnTestFailure is one of "notify", "park", "retry".
	OnTestFailure string
}

// WorkerConfig configures a Worker.
//
// Ported from MergeWorkerConfig in
// donmai-libraries merge-queue/merge-worker.ts, with Key replacing the bare
// repoId (FD-4).
type WorkerConfig struct {
	// Key is the (orgId, repoId) tenant-isolation key.
	Key Key
	// RepoPath is the path to the local repository.
	RepoPath string
	// Strategy is one of "rebase", "merge", "squash".
	Strategy string
	// TestCommand is run after landing to validate the result.
	TestCommand string
	// TestTimeout bounds the test run.
	TestTimeout time.Duration
	// LockFileRegenerate enables lock-file regeneration after landing.
	LockFileRegenerate bool
	// Mergiraf enables the mergiraf auto-resolution pass.
	Mergiraf bool
	// PollInterval is how long to wait when the queue is empty.
	PollInterval time.Duration
	// MaxRetries bounds in-process retries.
	MaxRetries int
	// Escalation controls conflict / test-failure escalation.
	Escalation EscalationConfig
	// DeleteBranchOnMerge deletes the source branch after a successful landing.
	DeleteBranchOnMerge bool
	// PackageManager is used for lock-file regeneration.
	PackageManager PackageManager
	// Remote is the git remote name (default "origin").
	Remote string
	// TargetBranch is the branch to land into (default "main").
	TargetBranch string
	// AcceptedStatus / RejectedStatus are issue-tracker status names used when
	// bubbling results back (only relevant when an issue tracker is wired).
	AcceptedStatus string
	RejectedStatus string
	// RetryablePrepareBackoffs are the in-process backoffs used when a strategy's
	// Prepare returns Retryable.
	RetryablePrepareBackoffs []time.Duration
	// MergeRecordedTimeout bounds how long the worker waits for the provider to
	// record the merge before deleting the source branch.
	MergeRecordedTimeout time.Duration
	// RecentlyMergedTTL is the lifetime of the short-lived "recently landed"
	// marker.
	RecentlyMergedTTL time.Duration
}

// DefaultRetryablePrepareBackoffs are the default in-process backoffs for
// retryable Prepare failures (~50s total).
var DefaultRetryablePrepareBackoffs = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// DefaultRecentlyMergedTTL is the default lifetime of the recently-landed marker.
const DefaultRecentlyMergedTTL = 10 * time.Minute

// RedisClient is the minimal Redis surface the worker and pool need for locking,
// pausing, and the recently-landed marker. Satisfied by an adapter over
// *redis.Client.
type RedisClient interface {
	// SetNX sets key=value with a TTL only if it does not already exist,
	// returning whether it was set.
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Del deletes a key.
	Del(ctx context.Context, key string) error
	// Get returns the value of a key, or "" if absent.
	Get(ctx context.Context, key string) (string, error)
	// Set sets key=value with no expiry.
	Set(ctx context.Context, key, value string) error
	// Expire sets a TTL on a key.
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

// WorkerDeps are the injected dependencies of a Worker.
//
// Ported from MergeWorkerDeps in
// donmai-libraries merge-queue/merge-worker.ts. The optional issue-tracker /
// PR-labeler hooks from the TS source are intentionally omitted from stage-1
// scaffolding; they are wired in a later stage.
type WorkerDeps struct {
	// Storage is the queue backing store.
	Storage Storage
	// Redis is the lock / pause / marker store.
	Redis RedisClient
	// VCS, when set, gates serialization on capabilities (commutative VCS skips
	// the queue). When nil, git semantics are assumed.
	VCS vcs.Provider
}

// Worker is the single-instance landing processor: it acquires a per-(orgId,
// repoId) lock and loops prepare -> execute -> resolve -> regenerate -> test ->
// finalize over the queue.
//
// Ported from MergeWorker in donmai-libraries merge-queue/merge-worker.ts.
type Worker struct {
	cfg  WorkerConfig
	deps WorkerDeps
}

// NewWorker returns a Worker.
func NewWorker(cfg WorkerConfig, deps WorkerDeps) *Worker {
	return &Worker{cfg: cfg, deps: deps}
}

// Start acquires the coordinator lock and loops processing the queue until ctx
// is cancelled or Stop is called.
//
// Stub: not yet ported.
func (w *Worker) Start(ctx context.Context) error {
	_ = ctx
	return fmt.Errorf("Worker.Start: %w", ErrNotImplemented)
}

// Stop requests a graceful shutdown after the current proposal finishes.
//
// Stub: not yet ported.
func (w *Worker) Stop() {}

// ProcessEntry lands a single queue entry: pre-flight skip -> prepare -> execute
// -> resolve conflicts -> regenerate lock files -> test -> finalize.
//
// Stub: not yet ported.
func (w *Worker) ProcessEntry(ctx context.Context, e Entry) (ProcessResult, error) {
	_ = ctx
	return ProcessResult{Proposal: e.Proposal}, fmt.Errorf("Worker.ProcessEntry: %w", ErrNotImplemented)
}
