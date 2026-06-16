package landing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/RenseiAI/donmai/landing/strategies"
	"github.com/RenseiAI/donmai/landing/vcs"
	"github.com/redis/go-redis/v9"
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
	// WorktreePath, when set, is the dedicated working tree the strategy runs in
	// for this proposal. The Pool sets it per in-flight proposal so concurrent
	// batch members never share one working tree (which would let them clobber
	// each other's index/checkout/lock-file regen). When empty, the strategy runs
	// directly in RepoPath — the single-flight (Concurrency<=1) Worker path, whose
	// behavior is unchanged.
	WorktreePath string
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
	// DelIfMatches atomically deletes key only if its current value equals value,
	// returning whether it deleted. This is the compare-and-delete used to release
	// the coordinator lock: an unconditional Del could free a DIFFERENT worker's
	// lock if this worker had already lost it (its TTL expired and another worker
	// re-acquired). The check + delete must be atomic, so it is a Lua script.
	DelIfMatches(ctx context.Context, key, value string) (bool, error)
	// Get returns the value of a key, or "" if absent.
	Get(ctx context.Context, key string) (string, error)
	// Set sets key=value with no expiry.
	Set(ctx context.Context, key, value string) error
	// Expire sets a TTL on a key.
	Expire(ctx context.Context, key string, ttl time.Duration) error
	// ExpireIfMatches atomically extends the TTL of key only when its current
	// value equals the caller's token, returning whether the extension took
	// effect. Used by heartbeat loops to avoid extending a lock that this
	// instance no longer owns (TOCTOU guard). Backed by a Lua script so the
	// check and the PEXPIRE run in a single Redis round trip.
	ExpireIfMatches(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
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

// lockTTL is the coordinator single-instance lock lifetime; the heartbeat
// re-extends it while the worker runs.
const lockTTL = 5 * time.Minute

// heartbeatInterval is how often the heartbeat re-extends the lock TTL.
const heartbeatInterval = 60 * time.Second

// Worker is the single-instance landing processor: it acquires a per-(orgId,
// repoId) lock and loops prepare -> execute -> resolve -> regenerate -> test ->
// finalize over the queue.
//
// Ported from MergeWorker in donmai-libraries merge-queue/merge-worker.ts.
type Worker struct {
	cfg  WorkerConfig
	deps WorkerDeps

	// running is flipped false by Stop to request a graceful shutdown after the
	// current proposal finishes.
	running bool

	// Test seams — production defaults are installed by NewWorker.
	//
	// runner runs the test command (and the file-reservation diff). Tests inject
	// a fake to avoid spawning processes.
	runner commandRunner
	// newStrategy builds a landing strategy; tests inject a fake strategy.
	newStrategy func(name string) (strategies.Strategy, error)
	// newResolver builds a conflict resolver; tests inject one driven by a fake
	// runner.
	newResolver func(cfg ConflictResolverConfig) conflictResolver
	// newLockHandler builds a lock-file regenerator; tests inject one driven by a
	// fake runner.
	newLockHandler func() lockHandler
	// sleep is the cancellable sleep used for poll/backoff; tests stub it.
	sleep func(ctx context.Context, d time.Duration)
	// now supplies the marker timestamp; tests stub it for determinism.
	now func() time.Time
}

// conflictResolver is the worker's view of a ConflictResolver (one method) so
// tests can supply a fake.
type conflictResolver interface {
	Resolve(ctx context.Context, c ConflictContext) (ResolutionResult, error)
}

// lockHandler is the worker's view of a LockFileRegeneration so tests can supply
// a fake.
type lockHandler interface {
	ShouldRegenerate(pm PackageManager, lockFileRegenerate bool) bool
	Regenerate(ctx context.Context, worktreePath string, pm PackageManager) (RegenerationResult, error)
}

// NewWorker returns a Worker wired to the production strategy/resolver/lock
// handlers and command runner.
func NewWorker(cfg WorkerConfig, deps WorkerDeps) *Worker {
	return &Worker{
		cfg:         cfg,
		deps:        deps,
		runner:      defaultRunner,
		newStrategy: strategies.New,
		newResolver: func(c ConflictResolverConfig) conflictResolver { return NewConflictResolver(c) },
		newLockHandler: func() lockHandler {
			return NewLockFileRegeneration()
		},
		sleep: sleepCtx,
		now:   time.Now,
	}
}

// Start acquires the coordinator lock and loops processing the queue until ctx
// is cancelled or Stop is called. Returns an error if another worker already
// holds the lock for this (orgId, repoId).
//
// Ported from MergeWorker.start.
func (w *Worker) Start(ctx context.Context) error {
	if err := validateKey(w.cfg.Key); err != nil {
		return err
	}
	lockKey := w.cfg.Key.lockKey()
	// A unique per-Start token lets release be a compare-and-delete: if this
	// worker's lock TTL expired and another worker re-acquired the lock, our
	// release must NOT free the new holder's lock.
	lockToken, err := newLockToken()
	if err != nil {
		return fmt.Errorf("Worker.Start mint lock token: %w", err)
	}
	acquired, err := w.deps.Redis.SetNX(ctx, lockKey, lockToken, lockTTL)
	if err != nil {
		return fmt.Errorf("Worker.Start acquire lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("another landing worker is already running for %s", w.cfg.Key)
	}

	w.running = true
	stopHeartbeat := w.startHeartbeat(ctx, lockKey, lockToken)
	defer func() {
		stopHeartbeat()
		// Release the lock on shutdown via compare-and-delete so we never free a
		// lock another worker re-acquired after ours expired. Use a detached
		// context so a cancelled ctx still frees our own lock for the next worker.
		if _, delErr := w.deps.Redis.DelIfMatches(context.WithoutCancel(ctx), lockKey, lockToken); delErr != nil {
			slog.Warn("landing worker: failed to release lock", "key", lockKey, "err", delErr)
		}
		w.running = false
	}()

	for w.running && ctx.Err() == nil {
		paused, perr := w.deps.Redis.Get(ctx, w.cfg.Key.pausedKey())
		if perr != nil {
			slog.Warn("landing worker: paused-flag read failed", "err", perr)
		}
		if paused != "" {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}

		entry, derr := w.deps.Storage.Dequeue(ctx, w.cfg.Key)
		if derr != nil {
			return fmt.Errorf("Worker.Start dequeue: %w", derr)
		}
		if entry == nil {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}

		result, perr := w.ProcessEntry(ctx, *entry)
		if perr != nil {
			// ProcessEntry already normalizes most failures into a ProcessError
			// result; a non-nil error here is a programming/infra fault. Mark the
			// proposal failed so the queue advances rather than wedging.
			result = ProcessResult{Proposal: entry.Proposal, Status: ProcessError, Message: perr.Error()}
		}
		if err := w.handleResult(ctx, *entry, result); err != nil {
			return fmt.Errorf("Worker.Start handle result: %w", err)
		}
	}
	return nil
}

// Stop requests a graceful shutdown after the current proposal finishes.
//
// Ported from MergeWorker.stop.
func (w *Worker) Stop() { w.running = false }

// handleResult records the disposition in storage and (when wired) bubbles it
// back. Each side failing is logged but does not block the queue from advancing,
// matching the TS source's independence of storage state and bubble-up.
func (w *Worker) handleResult(ctx context.Context, e Entry, result ProcessResult) error {
	key := w.cfg.Key
	switch result.Status {
	case ProcessMerged:
		if err := w.deps.Storage.MarkCompleted(ctx, key, result.Proposal); err != nil {
			return fmt.Errorf("mark completed: %w", err)
		}
		w.markRecentlyMerged(ctx, result.Proposal)
	case ProcessNoop:
		// Stale duplicate — original run already completed. Silently advance.
		if err := w.deps.Storage.MarkCompleted(ctx, key, result.Proposal); err != nil {
			return fmt.Errorf("mark completed (noop): %w", err)
		}
	case ProcessConflict:
		reason := result.Message
		if reason == "" {
			reason = "Merge conflict"
		}
		if err := w.deps.Storage.MarkBlocked(ctx, key, result.Proposal, reason); err != nil {
			return fmt.Errorf("mark blocked: %w", err)
		}
	case ProcessTestFailure:
		reason := result.Message
		if reason == "" {
			reason = "Tests failed"
		}
		if w.cfg.Escalation.OnTestFailure == "park" {
			if err := w.deps.Storage.MarkBlocked(ctx, key, result.Proposal, reason); err != nil {
				return fmt.Errorf("mark blocked (test-failure park): %w", err)
			}
		} else {
			if err := w.deps.Storage.MarkFailed(ctx, key, result.Proposal, reason); err != nil {
				return fmt.Errorf("mark failed (test-failure): %w", err)
			}
		}
	case ProcessError:
		reason := result.Message
		if reason == "" {
			reason = "Unknown error"
		}
		if err := w.deps.Storage.MarkFailed(ctx, key, result.Proposal, reason); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
	}
	_ = e // entry retained for a future issue-tracker bubble-up (wired in a later stage).
	return nil
}

// ProcessEntry lands a single queue entry: pre-flight skip -> prepare -> execute
// -> resolve conflicts -> regenerate lock files -> test -> finalize.
//
// Ported from MergeWorker.processEntry. The optional issue-tracker / PR-labeler
// hooks from the TS source are not wired here; pre-flight skip relies on the
// local recently-landed marker (the GitHub-state path lived in those hooks).
func (w *Worker) ProcessEntry(ctx context.Context, e Entry) (ProcessResult, error) {
	strategy, err := w.newStrategy(w.cfg.Strategy)
	if err != nil {
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: err.Error()}, nil
	}
	resolver := w.newResolver(ConflictResolverConfig{
		MergirafEnabled:    w.cfg.Mergiraf,
		EscalationStrategy: w.cfg.Escalation.OnConflict,
	})
	lockHandler := w.newLockHandler()

	targetBranch := e.TargetBranch
	if targetBranch == "" {
		targetBranch = w.cfg.TargetBranch
	}
	remote := w.cfg.Remote
	if remote == "" {
		remote = "origin"
	}
	// A dedicated per-proposal worktree (set by the Pool) isolates concurrent
	// batch members; the single-flight Worker path leaves WorktreePath empty and
	// runs directly in RepoPath.
	worktreePath := w.cfg.WorktreePath
	if worktreePath == "" {
		worktreePath = w.cfg.RepoPath
	}
	sctx := strategies.Context{
		RepoPath:     w.cfg.RepoPath,
		WorktreePath: worktreePath,
		SourceBranch: e.SourceBranch,
		TargetBranch: targetBranch,
		Proposal:     e.Proposal,
		Remote:       remote,
	}

	// 0. Pre-flight: skip if a prior run already landed this proposal. The local
	// marker is authoritative regardless of any provider-side state propagation.
	if marker, merr := w.deps.Redis.Get(ctx, w.cfg.Key.completedKey(e.Proposal)); merr == nil && marker != "" {
		return ProcessResult{Proposal: e.Proposal, Status: ProcessNoop, Message: "already landed (local marker)"}, nil
	}

	// 1. Prepare, retrying transient (retryable) failures with backoff.
	backoffs := w.cfg.RetryablePrepareBackoffs
	if backoffs == nil {
		backoffs = DefaultRetryablePrepareBackoffs
	}
	prep, err := strategy.Prepare(ctx, sctx)
	if err != nil {
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: err.Error()}, nil
	}
	for i := 0; !prep.Success && prep.Retryable && i < len(backoffs); i++ {
		slog.Info("landing worker: retrying prepare",
			"proposal", e.Proposal, "attempt", i+2, "backoff", backoffs[i], "error", prep.Error)
		w.sleep(ctx, backoffs[i])
		prep, err = strategy.Prepare(ctx, sctx)
		if err != nil {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: err.Error()}, nil
		}
	}
	if !prep.Success {
		if prep.AlreadyMerged {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessNoop, Message: "source branch missing on remote (already landed)"}, nil
		}
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: "Prepare failed: " + prep.Error}, nil
	}

	// 2. Execute the landing strategy.
	merge, err := strategy.Execute(ctx, sctx)
	if err != nil {
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: err.Error()}, nil
	}

	// 3. Handle conflicts / errors.
	switch merge.Status {
	case strategies.StatusConflict:
		issueID := e.IssueID
		if issueID == "" {
			issueID = "PR-" + strconv.Itoa(e.Proposal)
		}
		resolution, rerr := resolver.Resolve(ctx, ConflictContext{
			RepoPath:        sctx.RepoPath,
			WorktreePath:    sctx.WorktreePath,
			SourceBranch:    sctx.SourceBranch,
			TargetBranch:    sctx.TargetBranch,
			Proposal:        e.Proposal,
			IssueID:         issueID,
			ConflictFiles:   merge.ConflictFiles,
			ConflictDetails: merge.ConflictDetails,
		})
		if rerr != nil {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: rerr.Error()}, nil
		}
		if resolution.Status != ResolutionResolved {
			msg := resolution.Message
			if msg == "" {
				msg = "Unresolved conflicts"
			}
			return ProcessResult{Proposal: e.Proposal, Status: ProcessConflict, Message: msg}, nil
		}
	case strategies.StatusError:
		msg := merge.Error
		if msg == "" {
			msg = "Landing strategy failed"
		}
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: msg}, nil
	}

	// 4. Regenerate lock files if configured.
	if lockHandler.ShouldRegenerate(w.cfg.PackageManager, w.cfg.LockFileRegenerate) {
		regen, rerr := lockHandler.Regenerate(ctx, sctx.WorktreePath, w.cfg.PackageManager)
		if rerr != nil {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: rerr.Error()}, nil
		}
		if !regen.Success {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: "Lock file regeneration failed: " + regen.Error}, nil
		}
	}

	// 5. Run the test suite.
	if w.cfg.TestCommand != "" {
		passed, output := w.runTests(ctx, sctx.WorktreePath)
		if !passed {
			return ProcessResult{Proposal: e.Proposal, Status: ProcessTestFailure, Message: output}, nil
		}
	}

	// 6. Finalize: push / land.
	if err := strategy.Finalize(ctx, sctx); err != nil {
		return ProcessResult{Proposal: e.Proposal, Status: ProcessError, Message: err.Error()}, nil
	}

	// 7. Delete the source branch if configured (best-effort; non-fatal).
	if w.cfg.DeleteBranchOnMerge {
		if _, derr := w.runner.run(ctx, sctx.WorktreePath, nil, "git", "push", remote, "--delete", e.SourceBranch); derr != nil {
			slog.Info("landing worker: source-branch delete failed (non-fatal)", "branch", e.SourceBranch, "err", derr)
		}
	}

	return ProcessResult{Proposal: e.Proposal, Status: ProcessMerged}, nil
}

// runTests runs the configured test command in worktreePath, returning whether
// it passed and its combined output.
func (w *Worker) runTests(ctx context.Context, worktreePath string) (bool, string) {
	runCtx := ctx
	if w.cfg.TestTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, w.cfg.TestTimeout)
		defer cancel()
	}
	out, err := w.runner.run(runCtx, worktreePath, nil, "sh", "-c", w.cfg.TestCommand)
	if err != nil {
		return false, out
	}
	return true, out
}

// markRecentlyMerged writes the short-lived local marker so a duplicate dequeue
// short-circuits in pre-flight. Best-effort: a Redis write failure here does not
// fail the landing (it already happened).
func (w *Worker) markRecentlyMerged(ctx context.Context, proposal int) {
	ttl := w.cfg.RecentlyMergedTTL
	if ttl <= 0 {
		ttl = DefaultRecentlyMergedTTL
	}
	key := w.cfg.Key.completedKey(proposal)
	if _, err := w.deps.Redis.SetNX(ctx, key, strconv.FormatInt(w.now().UnixNano(), 10), ttl); err != nil {
		slog.Warn("landing worker: failed to write recently-landed marker", "proposal", proposal, "err", err)
	}
}

// heartbeatExtend is the per-tick extend action shared by Worker and Pool
// heartbeat loops. It calls ExpireIfMatches with the caller's lock token so the
// TTL is only refreshed when this instance still holds the lock. Returns the
// matched bool and any Redis error. Callers are responsible for logging.
func heartbeatExtend(ctx context.Context, rc RedisClient, key, token string, ttl time.Duration) (bool, error) {
	return rc.ExpireIfMatches(ctx, key, token, ttl)
}

// startHeartbeat extends the lock TTL on an interval until the returned stop
// func is called. The interval ticker is owned by a goroutine that exits on stop
// or ctx cancellation. lockToken is the value stored under lockKey: the
// extension is a compare-and-extend (ExpireIfMatches) so we never refresh a
// lock another worker re-acquired after ours expired.
func (w *Worker) startHeartbeat(ctx context.Context, lockKey, lockToken string) func() {
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
				matched, err := heartbeatExtend(ctx, w.deps.Redis, lockKey, lockToken, lockTTL)
				if err != nil {
					slog.Debug("landing worker: heartbeat expire failed", "key", lockKey, "err", err)
				} else if !matched {
					slog.Warn("landing worker: lock lost — heartbeat will stop extending", "key", lockKey)
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

// newLockToken mints a random per-Start lock token. The token is the value
// stored under the coordinator lock key so release can compare-and-delete: only
// the worker that owns the current value frees it.
func newLockToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first. A
// non-positive duration is a no-op (yields to the scheduler once).
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// redisClientAdapter adapts a *redis.Client to the minimal RedisClient surface
// the worker and pool need. Get maps a missing key to "" (not an error).
type redisClientAdapter struct {
	rdb *redis.Client
}

// NewRedisClient adapts a go-redis client to the RedisClient interface used by
// Worker and Pool for locking, pausing, and the recently-landed marker.
func NewRedisClient(rdb *redis.Client) RedisClient {
	return &redisClientAdapter{rdb: rdb}
}

func (a *redisClientAdapter) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	ok, err := a.rdb.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx %s: %w", key, err)
	}
	return ok, nil
}

// delIfMatchesScript is the atomic compare-and-delete: delete the key only if its
// value matches the caller's token. Returns 1 if deleted, 0 otherwise. The check
// and delete run in one Redis round trip so they cannot interleave with another
// worker re-acquiring the lock.
var delIfMatchesScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`)

func (a *redisClientAdapter) DelIfMatches(ctx context.Context, key, value string) (bool, error) {
	res, err := delIfMatchesScript.Run(ctx, a.rdb, []string{key}, value).Int64()
	if err != nil {
		return false, fmt.Errorf("redis compare-and-delete %s: %w", key, err)
	}
	return res == 1, nil
}

// expireIfMatchesScript atomically extends the TTL of a key only when its
// current value matches the caller's token. Returns 1 if the PEXPIRE was
// applied, 0 otherwise. ARGV[2] is the TTL in milliseconds so sub-second
// precision is preserved (matching PEXPIRE semantics).
var expireIfMatchesScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
	return 0
end
`)

func (a *redisClientAdapter) ExpireIfMatches(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	res, err := expireIfMatchesScript.Run(ctx, a.rdb, []string{key}, value, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("redis compare-and-expire %s: %w", key, err)
	}
	return res == 1, nil
}

func (a *redisClientAdapter) Get(ctx context.Context, key string) (string, error) {
	v, err := a.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("redis get %s: %w", key, err)
	}
	return v, nil
}

func (a *redisClientAdapter) Set(ctx context.Context, key, value string) error {
	if err := a.rdb.Set(ctx, key, value, 0).Err(); err != nil {
		return fmt.Errorf("redis set %s: %w", key, err)
	}
	return nil
}

func (a *redisClientAdapter) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if err := a.rdb.Expire(ctx, key, ttl).Err(); err != nil {
		return fmt.Errorf("redis expire %s: %w", key, err)
	}
	return nil
}
