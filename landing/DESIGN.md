# Landing Serializer — Go-native design (FD-4)

The landing serializer is the Go-native port of the legacy TypeScript "merge
queue" + "VCS provider" subsystem. It serializes the landing of proposed changes
(pull requests / proposals) onto a trunk so that concurrent landings never
corrupt the target branch, while letting non-conflicting changes land in
parallel.

The legacy TS lives in:

- `donmai-libraries/packages/core/src/merge-queue/` — queue, worker, pool,
  strategies, conflict graph, file manifest, conflict resolver, adapters.
- `donmai-libraries/packages/core/src/vcs/` — `VersionControlProvider` interface
  + GitHub and Atomic adapters.

`donmai-libraries` is deprecating (legacy Node CLI → Go binaries). This package
re-homes the execution-layer serializer as Go-native code inside the OSS `donmai`
module so the Go binaries can serialize landings without the TS runtime. It is a
client/execution-layer concern (no platform server dependency), which is why it
belongs in OSS `donmai`, not `platform`.

## Naming

"Merge queue" is git-specific framing. The corpus (008-version-control-providers)
generalizes over commutative VCS (Atomic) where there is no queue. This package
uses provider-neutral names:

| Legacy TS term      | Go-native term            |
| ------------------- | ------------------------- |
| merge queue         | landing serializer        |
| merge worker        | `Worker`                  |
| merge pool          | `Pool`                    |
| PR / proposal       | proposal (`ProposalRef`)  |
| merge strategy      | landing strategy          |
| VCS provider        | `vcs.Provider`            |

## TS → Go file map

| TS source                                       | Go file                                  | Notes |
| ----------------------------------------------- | ---------------------------------------- | ----- |
| `merge-queue/conflict-graph.ts`                 | `landing/conflictgraph.go`               | undirected file-overlap graph + greedy batching |
| `merge-queue/file-manifest.ts`                  | `landing/filemanifest.go`                | `git diff --name-only target...source` |
| `merge-queue/types.ts` (queue adapter)          | `landing/manifest.go`                    | queue status / state types + adapter contract |
| `merge-queue/branch-conflict.ts`                | `landing/branchconflict.go`              | git error-string classifiers |
| `merge-queue/conflict-resolver.ts`              | `landing/conflictresolver.go`            | mergiraf marker-check + escalation (reassign/notify/park) |
| `merge-queue/lock-file-regeneration.ts`         | `landing/lockfile.go`                    | lock-file regen (npm/pnpm/yarn) + git-attributes union driver |
| `merge-queue/merge-worker.ts`                   | `landing/worker.go`                      | prepare→execute→resolve→test→finalize loop |
| `merge-queue/merge-pool.ts`                     | `landing/pool.go`                        | conflict-graph batching over `Worker` |
| `merge-queue/strategies/types.ts`               | `landing/strategies/strategy.go`         | `Strategy` interface + context/results |
| `merge-queue/strategies/index.ts`               | `landing/strategies/strategies.go`       | `New(name)` factory |
| `merge-queue/strategies/rebase-strategy.ts`     | `landing/strategies/rebase.go`           | git rebase onto target |
| `merge-queue/strategies/merge-commit-strategy.ts` | `landing/strategies/mergecommit.go`    | git merge --no-ff |
| `merge-queue/strategies/squash-strategy.ts`     | `landing/strategies/squash.go`           | git merge --squash |
| `merge-queue/strategies/worktree-cleanup.ts`    | `landing/strategies/worktree.go`         | `CleanWorktreeState` + `AddWorktree`/`RemoveWorktree` (git worktree add --detach / remove --force) lifecycle helpers |
| `merge-queue/adapters/local.ts` (storage iface) | `landing/queue.go`                       | Redis sorted-set storage |
| `merge-queue/adapters/local.ts` (gh eligibility)| `landing/queue.go` (`extractIssueID`)    | issue-id extraction (pure, ported) |
| `merge-queue/adapters/github-native.ts`         | (not ported — GitHub-native queue)       | external provider; out of scope for FD-4 stage 1 |
| `vcs/types.ts`                                   | `landing/vcs/provider.go`                | `Provider` interface + value types |
| `vcs/github.ts`                                  | `landing/vcs/github.go` (+ `github_graphql.go`) | `GitHubProvider`, gated behind `GitHubOpts.Available` (default-off, skew-safe) |
| `vcs/atomic.ts`                                  | `landing/vcs/atomic.go`                  | `AtomicProvider` (commutative; PR/queue verbs return `*UnsupportedOperationError`) |
| (CLI exec seam)                                  | `landing/vcs/runner.go`                  | `commandRunner` seam for git/gh/atomic; tests inject a fake |

## Public API (package `landing`)

```go
// conflictgraph.go
type ConflictGraph struct { /* unexported */ }
func NewConflictGraph() *ConflictGraph
func BuildConflictGraph(manifests []FileManifest) *ConflictGraph
func (g *ConflictGraph) AddProposal(proposal int, files []string)
func (g *ConflictGraph) Conflicts(a, b int) bool
func (g *ConflictGraph) SharedFiles(a, b int) []string
func (g *ConflictGraph) IndependentBatches(maxBatchSize int) [][]int // maxBatchSize<=0 ⇒ unlimited
func (g *ConflictGraph) Size() int

// filemanifest.go
type FileManifest struct {
    Proposal     int
    SourceBranch string
    Files        []string
    ComputedAt   time.Time
}
type ManifestEntry struct { Proposal int; SourceBranch string }
func BuildFileManifest(ctx, repoPath, sourceBranch, targetBranch, remote string) ([]string, error)
func BuildFileManifests(ctx, repoPath string, entries []ManifestEntry, targetBranch, remote string) ([]FileManifest, error)

// manifest.go (queue status types + adapter contract)
type State string            // queued|merging|merged|failed|blocked|not-queued
type CheckStatus struct { Name string; Status string } // pass|fail|pending
type Status struct { State State; Position int; FailureReason string; Checks []CheckStatus }
type Adapter interface {
    Name() string
    CanEnqueue(ctx, owner, repo string, proposal int) (bool, error)
    Enqueue(ctx, owner, repo string, proposal int) (Status, error)
    GetStatus(ctx, owner, repo string, proposal int) (Status, error)
    Dequeue(ctx, owner, repo string, proposal int) error
    IsEnabled(ctx, owner, repo string) (bool, error)
}

// queue.go (Redis sorted-set storage; org+repo scoped — FD-4)
type Key struct { OrgID, RepoID string }
type Entry struct {
    OrgID, RepoID  string
    Proposal       int
    ProposalURL    string
    IssueID        string
    Priority       int
    SourceBranch   string
    TargetBranch   string
    EnqueuedAt     time.Time
}
type Storage interface {
    Enqueue(ctx, e Entry) error
    Dequeue(ctx, key Key) (*Entry, error)
    PeekAll(ctx, key Key) ([]Entry, error)
    DequeueBatch(ctx, key Key, proposals []int) ([]Entry, error)
    QueueDepth(ctx, key Key) (int, error)
    IsEnqueued(ctx, key Key, proposal int) (bool, error)
    Position(ctx, key Key, proposal int) (int, error) // 0 ⇒ not queued (1-based otherwise)
    Remove(ctx, key Key, proposal int) error
    MarkCompleted(ctx, key Key, proposal int) error
    MarkFailed(ctx, key Key, proposal int, reason string) error
    MarkBlocked(ctx, key Key, proposal int, reason string) error
    FailedReason(ctx, key Key, proposal int) (string, error)
    BlockedReason(ctx, key Key, proposal int) (string, error)
}
type RedisStorage struct { /* wraps *redis.Client */ }
func NewRedisStorage(rdb *redis.Client) *RedisStorage
func ExtractIssueID(input string) string // "" if none

// worker.go
type ProcessStatus string // merged|noop|conflict|test-failure|error
type ProcessResult struct { Proposal int; Status ProcessStatus; Message string }
type WorkerConfig struct { Key Key; RepoPath string; Strategy string; TestCommand string; ... }
type WorkerDeps struct { Storage Storage; Redis RedisClient; VCS vcs.Provider; ... }
type Worker struct { /* ... */ }
func NewWorker(cfg WorkerConfig, deps WorkerDeps) *Worker
func (w *Worker) Start(ctx) error
func (w *Worker) Stop()
func (w *Worker) ProcessEntry(ctx, e Entry) (ProcessResult, error)

// pool.go
type PoolConfig struct { WorkerConfig; Concurrency int }
type Pool struct { /* ... */ }
func NewPool(cfg PoolConfig, deps WorkerDeps) *Pool
func (p *Pool) Start(ctx) error
func (p *Pool) Stop()
```

```go
// package landing/strategies
type Context struct { RepoPath, WorktreePath, SourceBranch, TargetBranch, Remote string; Proposal int }
type PrepareResult struct { Success bool; Error string; HeadSHA string; Retryable, AlreadyMerged bool }
type MergeResult struct { Status string; MergedSHA string; ConflictFiles []string; ConflictDetails, Error string }
type Strategy interface {
    Name() string
    Prepare(ctx context.Context, c Context) (PrepareResult, error)
    Execute(ctx context.Context, c Context) (MergeResult, error)
    Finalize(ctx context.Context, c Context) error
}
func New(name string) (Strategy, error) // rebase|merge|squash
```

```go
// package landing/vcs
type Provider interface {
    Name() string
    Capabilities() Capabilities
    Clone(ctx, uri, dst string, opts CloneOpts) (Workspace, error)
    RecordChange(ctx, ws Workspace, change ChangeRequest) (ChangeRef, error)
    Push(ctx, ws Workspace, target PushTarget) (PushResult, error)
    Pull(ctx, ws Workspace, source PullSource) (MergeResult, error)
    // optional verbs gated by capabilities; return UnsupportedOperationError
    OpenProposal(ctx, ws Workspace, opts ProposalOpts) (ProposalRef, error)
    MergeProposal(ctx, ref ProposalRef, strategy string) (MergeResult, error)
    EnqueueForMerge(ctx, ref ProposalRef, opts QueueOpts) (QueueTicket, error)
    Attest(ctx, ws Workspace, meta SessionAttestation) (AttestationRef, error)
}
type UnsupportedOperationError struct { Capability, ProviderID string }
func AssertCapability(caps Capabilities, capability, providerID string) error
```

## (orgId, repoId) keying decision — FD-4 tenant isolation

The legacy TS Redis keyspace is **repoId-only**:

```
merge:lock:<repoId>
merge:paused:<repoId>
merge:completed:<repoId>:<prNumber>
```

with `repoId = "<owner>/<repo>"`. A single-tenant deployment can get away with
that because every repo id is globally unique to that tenant. In a multi-tenant
deployment two organizations that both have access to (or fork) the same upstream
`owner/repo` would collide on the same Redis keys — one org's landing lock,
queue, and "recently merged" markers would be visible to and mutable by the
other. That is a tenant-isolation defect.

**Decision (FD-4):** the Go port keys every landing-serializer Redis structure by
the composite `(orgId, repoId)` via the `Key{OrgID, RepoID}` value. The key
prefix becomes:

```
landing:<orgId>:<repoId>:<...>
```

Concretely:

| Concern             | Legacy key                          | Go-native key                                  |
| ------------------- | ----------------------------------- | ---------------------------------------------- |
| coordinator lock    | `merge:lock:<repoId>`               | `landing:<orgId>:<repoId>:lock`                |
| pause flag          | `merge:paused:<repoId>`             | `landing:<orgId>:<repoId>:paused`              |
| recently-merged     | `merge:completed:<repoId>:<pr>`     | `landing:<orgId>:<repoId>:completed:<pr>`      |
| queue (sorted set)  | (server-side, repoId-only)          | `landing:<orgId>:<repoId>:queue`               |
| entry hash          | (server-side)                       | `landing:<orgId>:<repoId>:entry:<pr>`          |
| failed reason       | (server-side)                       | `landing:<orgId>:<repoId>:failed:<pr>`         |
| blocked reason      | (server-side)                       | `landing:<orgId>:<repoId>:blocked:<pr>`        |

`OrgID` is required (empty `OrgID` is a programming error and yields the literal
prefix `landing::<repoId>:…`, which still namespaces away from any future
orgId-bearing key but should be rejected by callers). `Key.String()` centralizes
prefix construction so no call site hand-builds a key — a single place to audit
for the isolation property.

Rationale for org+repo (not just org, not a hash):
- repoId alone collides across tenants (the defect above).
- orgId alone collides across repos within a tenant (every repo would share one
  queue/lock).
- The pair is the natural isolation boundary: one serialized landing stream per
  repository per organization. It mirrors the platform's
  `execution_provider_pool.org_id == project.org_id` binding invariant.

## Redis storage shape (queue.go)

The queue is a Redis **sorted set** per `(orgId, repoId)`, score = priority
(lower = higher priority; ties broken by enqueue order encoded into the score's
fractional part). The tiebreaker is a strictly increasing per-key sequence
number (a Redis `INCR` on `landing:<orgId>:<repoId>:seq`), **not** a wall-clock
timestamp: a float64 score cannot resolve fine-grained sub-second timestamps, so
several proposals enqueued in the same second at one priority could collide on a
single score and lose FIFO ordering. A small monotonic integer sequence is
exactly representable and strictly increasing under `seq/(1+seq)`, which is
clamped strictly below 1 so it never bleeds into the next integer priority band.
Members are proposal numbers. Per-proposal metadata (`Entry`) lives in a sibling
hash so the sorted set stays small and `PeekAll`/`Position` are cheap.
`DequeueBatch` uses a `MULTI/EXEC` transaction to remove a set of proposals
atomically (so the `Pool` never double-dispatches a proposal across concurrent
coordinators). All operations take a `context.Context` for cancellation/deadlines.

### Coordinator lock release (compare-and-delete)

The single-instance coordinator lock (`landing:<orgId>:<repoId>:lock`) is
acquired with a unique per-`Start` token and released by **compare-and-delete**:
an atomic Lua `GET == token then DEL` (`RedisClient.DelIfMatches`). An
unconditional `DEL` could free a *different* worker's lock — if this worker's
lock TTL expired and another worker re-acquired it, an unconditional release on
shutdown would delete the new holder's lock. Comparing the stored token to this
worker's own token before deleting closes that race; both `Worker.Start` and
`Pool.Start` use it.

### Per-proposal dedicated worktree (concurrent landing isolation)

When `Concurrency > 1`, the `Pool` lands non-conflicting proposals in parallel.
Each in-flight batch member runs the landing strategy in its **own** dedicated
git worktree (`strategies.AddWorktree` → `git worktree add --detach`; torn down
by `strategies.RemoveWorktree` → `git worktree remove --force` in a `defer`).
Sharing one working tree across parallel proposals would let them clobber each
other's index, checkout, and lock-file regeneration. `--detach` lets two
worktrees start from the same target branch without git's "branch already checked
out" guard. The single-flight (`Concurrency <= 1`) path delegates to a `Worker`
that runs directly in `RepoPath` (`WorkerConfig.WorktreePath` empty) — unchanged.

## Build-out history

The port landed in stages. Stage 1 was design + scaffold that compiles
(interfaces, value types, the `Key` composite + `Key.String()`, the conflict
graph and issue-id extraction real; everything else a stub). Subsequent stages
filled the bodies and ported the table-driven tests from the TS `*.test.ts`
suites. **As of Stage 5 there are no stubs left** — `landing.ErrNotImplemented`
was removed because nothing returns it.

## Stage-3 scope — pool + worker + (orgId,repoId)-keyed Redis queue (DONE)

Real, fully-ported and table-driven `-race`-tested:

- `queue.go` `RedisStorage` — Redis sorted-set queue per `(orgId, repoId)`.
  Score packs `(priority, seq)` into one float so the queue orders by priority
  ascending (lower = higher precedence), FIFO on ties; the tie-breaker is a
  strictly increasing per-key `INCR` sequence (not a timestamp) mapped through
  `seq/(1+seq)` and clamped strictly below 1 so it never bleeds into the next
  priority band (`encodeScore`). Per-proposal metadata lives in a sibling hash so
  the ZSET stays small. `DequeueBatch` runs ZREM+DEL in a `MULTI/EXEC` pipeline and
  returns only the proposals whose ZREM reported a removal — so concurrent
  coordinators never double-dispatch. Every method validates the key first
  (`validateKey`) so an unscoped key can never silently share tenant state.
- `worker.go` `Worker` — single-instance processor. `Start` acquires the
  per-`(orgId,repoId)` lock (errors on contention), runs a heartbeat goroutine
  to re-extend the TTL, honors the pause flag, and loops dequeue → `ProcessEntry`
  → `handleResult` until `Stop`/ctx-cancel; the lock is always released (detached
  ctx). `ProcessEntry` is the full pipeline: pre-flight local-marker noop →
  prepare (with retryable backoff) → execute → resolve conflicts → regenerate
  lock files → run tests → finalize → optional source-branch delete; every leg
  maps to a `ProcessStatus`. `handleResult` records the disposition in storage
  (`MarkCompleted` + recently-landed marker / `MarkBlocked` / `MarkFailed`,
  honoring `onTestFailure: park`). `NewRedisClient` adapts `*redis.Client` to
  the minimal `RedisClient` surface.
- `pool.go` `Pool` — concurrent coordinator. `Concurrency <= 1` delegates to a
  single `Worker`. Otherwise: acquire lock → peek all → fetch refs → build
  manifests → conflict graph → first independent batch (size-capped by
  concurrency) → atomic `DequeueBatch` → process the batch in parallel → record
  each result. Non-conflicting proposals land together; conflicting ones split
  across batches.
- `adapter_local.go` `LocalAdapter` — self-hosted `Adapter` over `Storage`,
  carrying `OrgID` (FD-4). Proposal eligibility / merged-state probed via the
  `gh` CLI (injectable runner). `Enqueue` resolves the issue id from branch then
  title then `PR-N`; `GetStatus` walks position → failed → blocked → merged →
  not-queued.

Test seams: `Worker`/`Pool` expose unexported `newStrategy`/`newResolver`/
`newLockHandler`/`newWorker`/`buildManifests`/`fetchRefs`/`sleep`/`now` fields
so tests drive the pipeline with fakes and never spawn git or sleep wall-clock.
`RedisStorage` is tested against an in-process miniredis (`redis_test.go`).

Stage 4 replaced the `vcs.github`/`vcs.atomic` adapter-verb stubs with full
git/gh/atomic-CLI implementations (see the vcs section below). The optional
issue-tracker / PR-labeler bubble-up hooks from the TS source are intentionally
not wired (the `Entry` is threaded through `handleResult` for a later stage); the
GitHub-native server-side queue adapter is out of scope (external provider).

## OSS hygiene

No ticket IDs, no private-repo URLs, no internal SHAs, no developer absolute
paths are carried over from the TS source comments. The git commit-trailer
attestation keys are named provider-neutrally (`X-Donmai-*`) so the OSS package
ships no brand-coupled wire tokens.
