# Landing serializer (FD-4) — status & remaining work

Status of the Go-native landing serializer (`landing/`, `landing/strategies/`,
`landing/vcs/`) as of the Stage-5 finalize. This is the precise "what's done vs
what isn't" record; the conceptual design lives in `DESIGN.md`.

## Verification snapshot (Stage 5 + deferred-hardening pass)

Run from the `donmai` module root with `GOWORK=off` (CI / donmai-smokes
convention — the org `go.work` `use`s `./donmai`, not this worktree):

| Gate                                   | Result |
| -------------------------------------- | ------ |
| `GOWORK=off go build ./...`            | PASS   |
| `GOWORK=off go vet ./landing/...`      | PASS   |
| `GOWORK=off go test -race ./landing/...` | PASS (3/3 packages) |
| `GOWORK=off golangci-lint run ./landing/...` | 0 issues |
| `bash scripts/leak-guard.sh --all`     | OK — no closed-source content (779 files) |
| Private-ref sweep over `git diff origin/main` | clean (only `github.com/RenseiAI/donmai/...` import paths) |

Test functions: `landing` 79, `landing/strategies` 24, `landing/vcs` 34
(137 total). Statement coverage: `landing` 79.9%, `strategies` 78.0%,
`vcs` 83.1% — all above the repo's 70% minimum.

> Note: plain `go build ./...` (workspace mode) fails with "directory prefix .
> does not contain modules listed in go.work" because the org-level go.work
> references `./donmai`, not this linked worktree. That is a worktree/workspace
> membership artifact, not a code defect; `GOWORK=off` builds and tests the
> module standalone exactly as donmai CI does. No workspace dependency was
> edited.

## Fully ported + tested

These have real bodies (no stubs) and table-driven `-race` tests:

### package `landing`
- `conflictgraph.go` — undirected file-overlap graph + greedy independent
  batching (`IndependentBatches`).
- `filemanifest.go` — `git diff --name-only target...source` per proposal.
- `manifest.go` — queue status/state value types + the `Adapter` contract.
- `queue.go` `RedisStorage` — Redis sorted-set queue keyed by `(orgId, repoId)`.
  Score packs `(priority, seq)` into one float (`encodeScore`, FIFO on ties): the
  tiebreaker is a **strictly increasing per-key sequence** (a Redis `INCR` on
  `:seq`), not a wall-clock fraction, so sub-second same-priority enqueues never
  collide on one float64 score and lose FIFO ordering. The fractional part is
  `seq/(1+seq)` clamped strictly below 1 (`maxScoreFrac`) so it can never bleed
  into the next integer priority band. Per-proposal metadata in a sibling hash;
  `DequeueBatch` runs ZREM+DEL in a `MULTI/EXEC` pipeline and returns only
  proposals whose ZREM reported a removal (no double-dispatch); every method
  `validateKey`s first. `(orgId,repoId)` keying intact. `ExtractIssueID` (pure).
- `worker.go` `Worker` — single-instance processor: lock acquire + TTL
  heartbeat + pause-flag honoring + dequeue→`ProcessEntry`→`handleResult` loop.
  The lock is acquired with a unique per-`Start` token and released by
  **compare-and-delete** (`DelIfMatches`, a Lua `GET==token then DEL` script) on a
  detached ctx, so a worker whose TTL expired and was re-acquired by another
  worker can never free the new holder's lock. `ProcessEntry` is the full pipeline
  (pre-flight local-marker noop → prepare w/ retryable backoff → execute →
  resolve conflicts → regenerate lock files → run tests → finalize → optional
  source-branch delete), running in `WorkerConfig.WorktreePath` when set (the
  Pool's per-proposal worktree) and otherwise directly in `RepoPath` (single
  flight).
- `pool.go` `Pool` — concurrent coordinator: `Concurrency<=1` delegates to one
  `Worker`; otherwise peek-all → fetch refs → build manifests → conflict graph →
  first independent batch (capped by concurrency) → atomic `DequeueBatch` →
  parallel process → record results. Each in-flight batch member gets its **own
  dedicated git worktree** (`strategies.AddWorktree`/`RemoveWorktree`, a
  `worktreeManager` seam) created before processing and removed after via `defer`
  (always, even on error), so parallel members never share one working tree and
  clobber each other's index/checkout/lock-regen. The coordinator lock uses the
  same unique-token compare-and-delete release as the Worker. Single-flight
  behavior is unchanged (it delegates to a `Worker` that runs in `RepoPath`).
- `adapter_local.go` `LocalAdapter` — self-hosted `Adapter` over `Storage`,
  carries `OrgID` (FD-4); eligibility/merged probed via injectable `gh` runner.
- `conflictresolver.go` `ConflictResolver` — mergiraf marker-check pass (stage
  resolved files, `git rebase --continue` when all clean) + escalation
  (reassign/notify/park).
- `lockfile.go` `LockFileRegeneration` — npm/pnpm/yarn lock regen +
  `EnsureGitAttributes` union merge driver; injectable runner + filesystem seam.
- `runner.go` — `commandRunner` exec seam; tests inject a fake.

### package `landing/strategies`
- `strategy.go` — `Strategy` interface + `Context`/`PrepareResult`/`MergeResult`.
- `rebase.go` / `mergecommit.go` / `squash.go` — git rebase / `merge --no-ff` /
  `merge --squash` Prepare/Execute/Finalize bodies.
- `worktree.go` — `CleanWorktreeState` plus the `AddWorktree`/`RemoveWorktree`
  `git worktree add --detach … / remove --force` lifecycle helpers the Pool uses
  to give each in-flight proposal its own isolated working tree.
- `runner.go` — exec seam mirroring the `landing` one; fake in tests.

### package `landing/vcs`
- `provider.go` — `Provider` interface, `Capabilities`, all value types,
  `UnsupportedOperationError`, `AssertCapability`.
- `github.go` (+ `github_graphql.go`) `GitHubProvider` — git CLI for
  clone/recordChange/push/pull; `gh` CLI for openProposal/mergeProposal/
  enqueueForMerge; attestation via commit trailers
  (`BuildCommitMessageWithTrailers`, provider-neutral `X-Donmai-*` keys).
  Gated behind `GitHubOpts.Available` (default-OFF, skew-safe).
- `atomic.go` `AtomicProvider` — commutative VCS; clone/recordChange/push/pull
  real; the PR/queue verbs (`OpenProposal`/`MergeProposal`/`EnqueueForMerge`)
  return `*UnsupportedOperationError` by capability, which is the intended
  behavior, not a stub.
- `runner.go` — `commandRunner` exec seam; fake in tests.

## Intentionally NOT ported / out of scope (by design, not "unfinished")

- **GitHub-native server-side merge-queue adapter**
  (`merge-queue/adapters/github-native.ts`). That delegates to GitHub's own
  hosted merge queue (an external provider's server feature) and does not belong
  in the OSS execution-layer serializer. Left out; documented in `DESIGN.md`.
- **Issue-tracker / PR-labeler bubble-up hooks.** The TS worker optionally pushed
  the landing result back to an issue tracker / labeled the PR. The extension
  point now exists: `WorkerDeps.ResultPoster` (a `ResultPoster` interface with a
  single `PostResult(ctx, Entry, ProcessResult)` method) is called best-effort
  from `handleResult` after the queue state is recorded — a poster error is
  logged and swallowed so the queue still advances. The **concrete
  implementation lives in the embedding binary** (transport-agnostic by design);
  with no poster wired (`ResultPoster == nil`) this is a no-op, so it is not
  load-bearing for serialization correctness. The `AcceptedStatus`/
  `RejectedStatus` config fields remain available for a poster to consume.

## Hardening landed (was "Partial / deferred wiring")

These three deferred-hardening items are now fully wired + `-race`-tested:

- **Per-proposal dedicated worktree.** The `Pool` now hands each in-flight batch
  member its own isolated git worktree via `strategies.AddWorktree`/
  `RemoveWorktree` (new `git worktree add --detach … / remove --force` helpers)
  behind a `worktreeManager` seam. `Pool.processOne` creates the worktree, sets
  `WorkerConfig.WorktreePath`, processes, and removes the worktree in a `defer`
  (always, even on error / on a `ProcessError` result). `Worker.ProcessEntry`
  honors `WorktreePath` when set and falls back to `RepoPath` otherwise, so the
  single-flight (`Concurrency<=1`) path is byte-for-byte unchanged. Tests:
  two concurrent batch members get distinct worktree paths; cleanup runs on
  success and on error; a worktree-add failure becomes a `ProcessError` with no
  dangling remove; `proposalWorktreePath` is distinct-per-proposal and stable.
- **Sub-second FIFO tiebreak.** `encodeScore` now packs `(priority, seq)` where
  `seq` is a strictly increasing per-key Redis `INCR` sequence (`:seq`) rather
  than a wall-clock fraction. Sub-second same-priority enqueues no longer collide
  on one float64 score; the `seq/(1+seq)` fraction is clamped strictly below 1
  (`maxScoreFrac`) so it never bleeds into the next priority band, and
  `(orgId,repoId)` keying is intact. Tests: 20 same-priority proposals at one
  identical instant dequeue in strict enqueue order; interleaved priorities still
  honor priority-then-FIFO; `encodeScore` is strictly monotonic in `seq` and the
  tiebreaker stays in-band for huge `seq`.
- **Non-token lock release (compare-and-delete).** The coordinator lock is now
  acquired with a unique per-`Start` token and released via `DelIfMatches` — an
  atomic Lua `GET==token then DEL` — in both `Worker.Start` and `Pool.Start`. A
  worker whose TTL expired and was re-acquired by another worker can no longer
  free the new holder's lock. Tests (against miniredis): a release with a
  non-matching token is a no-op and leaves the lock untouched; the matching token
  deletes; a stale worker cannot free a re-acquired lock.

## DEFERRED to a separate platform PR — do NOT do in this donmai PR

These two items belong to the closed `platform` repo (server side) and the
deprecating `donmai-libraries` TS, not to OSS `donmai`. They are explicitly out
of scope for this PR:

1. **Platform coordinator keying on `(orgId, repoId)`.** This Go package already
   keys every Redis structure by the `(orgId, repoId)` composite (see
   `DESIGN.md` "(orgId, repoId) keying decision"). The platform-side coordinator
   that drives the queue must be updated to pass/consume the same composite key
   so the tenant-isolation property holds end-to-end. That is a `platform` change.
2. **TS merge-queue hard-delete.** Remove the legacy
   `donmai-libraries/packages/core/src/merge-queue/` + `vcs/` TS now that this Go
   port supersedes it — **only after** the Go path is green in smokes. Do this in
   a separate platform/donmai-libraries PR, not here.
