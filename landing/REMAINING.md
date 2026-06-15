# Landing serializer (FD-4) — status & remaining work

Status of the Go-native landing serializer (`landing/`, `landing/strategies/`,
`landing/vcs/`) as of the Stage-5 finalize. This is the precise "what's done vs
what isn't" record; the conceptual design lives in `DESIGN.md`.

## Verification snapshot (Stage 5)

Run from the `donmai` module root with `GOWORK=off` (CI / donmai-smokes
convention — the org `go.work` `use`s `./donmai`, not this worktree):

| Gate                                   | Result |
| -------------------------------------- | ------ |
| `GOWORK=off go build ./...`            | PASS   |
| `GOWORK=off go vet ./landing/...`      | PASS   |
| `GOWORK=off go test -race ./landing/...` | PASS (3/3 packages) |
| `GOWORK=off golangci-lint run ./landing/...` | 0 issues |
| `bash scripts/leak-guard.sh --all`     | OK — no closed-source content (778 files) |
| Private-ref sweep over `git diff origin/main` | clean (only `github.com/RenseiAI/donmai/...` import paths) |

Test functions: `landing` 70, `landing/strategies` 20, `landing/vcs` 34
(124 total). Statement coverage: `landing` 78.6%, `strategies` 78.1%,
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
  Score packs `(priority, enqueuedAt)` into one float (`encodeScore`, FIFO on
  ties, fractional tie-breaker bounded in `[0,1)`); per-proposal metadata in a
  sibling hash; `DequeueBatch` runs ZREM+DEL in a `MULTI/EXEC` pipeline and
  returns only proposals whose ZREM reported a removal (no double-dispatch);
  every method `validateKey`s first. `ExtractIssueID` (pure).
- `worker.go` `Worker` — single-instance processor: lock acquire + TTL
  heartbeat + pause-flag honoring + dequeue→`ProcessEntry`→`handleResult` loop;
  lock always released on a detached ctx. `ProcessEntry` is the full pipeline
  (pre-flight local-marker noop → prepare w/ retryable backoff → execute →
  resolve conflicts → regenerate lock files → run tests → finalize → optional
  source-branch delete).
- `pool.go` `Pool` — concurrent coordinator: `Concurrency<=1` delegates to one
  `Worker`; otherwise peek-all → fetch refs → build manifests → conflict graph →
  first independent batch (capped by concurrency) → atomic `DequeueBatch` →
  parallel process → record results.
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
- `worktree.go` — `git worktree add/remove` helpers.
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
  the landing result back to an issue tracker / labeled the PR. The `Entry` is
  threaded through `handleResult` (see `worker.go` `_ = e`) and the
  `AcceptedStatus`/`RejectedStatus` config fields exist, but no tracker call is
  wired. This is a thin add-on for a later stage, not load-bearing for
  serialization correctness.

## Partial / deferred wiring (works today, hardening later)

- **Per-proposal dedicated worktree.** `Worker.ProcessEntry` currently runs the
  strategy with `WorktreePath == RepoPath` (`worker.go` ~L356). The
  `strategies.worktree.go` add/remove helpers exist; wiring the host/`Pool` to
  hand each in-flight proposal its own isolated worktree (so parallel batch
  members don't share a working tree) is the natural next step. Single-flight
  (`Concurrency<=1`) is unaffected.

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
