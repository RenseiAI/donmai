# `runtime/worktree/`

Per-session repository or repository-free directory provisioning and root-scoped teardown for the agent runner.

## What it does

```go
m, _ := worktree.NewManager(worktree.Options{
    ParentDir:       "/var/lib/donmai/wt",
    OwnershipProber: afclient.GetSessionOwnership,
})
path, err := m.Provision(ctx, worktree.ProvisionSpec{
    SessionID: "sess-123",
    RepoURL:   "git@github.com:org/repo.git",
    Branch:    "main",
    Strategy:  worktree.StrategyClone,
})
// ... agent runs in `path` ...
m.Teardown(ctx, "sess-123")
```

With no `RepositoryDeclaration`, this keeps the released flat layout and
`path` is `<parent>/<session-id>`. A negotiated `session-root-v1` declaration
materializes a fresh session-owned root containing one leaf per declared
repository. `Provision` and `Path` still return the selected repository CWD;
`Layout` separately exposes the lifecycle root. Cleanup, terminal leases,
archive disposition, and disk reporting bind the root.

The versioned path requires an exact executor attestation. A declaration with
any `read-only` leaf additionally requires `isolated-read-only-v1`; an absent
protocol list is `[]` and absent enforcement is `none`. Unsupported peers keep
the flat singular path and never receive a declaration.

## Strategies

- `StrategyClone` — `git clone --branch <b> <repo> <dst>`. Fully isolated session; one full repo per session.
- `StrategyWorktreeAdd` — `git worktree add --no-track -B <b> <dst> origin/<b>` off an existing parent clone. The session branch has no automatic upstream; configure one explicitly before a bare `git push`. Cheaper for many concurrent sessions.
- `StrategyEmpty` — create a fresh flat session directory without invoking Git. Used only for work whose accepted session detail has no repository; versioned declarations and shared participants retain their existing strategies.

## Shared-parent concurrency contract (`StrategyWorktreeAdd`)

A single parent clone is shared by every session that branches off it, so
provisioning coordinates on two axes. Both invariants are pinned by tests that
go red when the mechanism is removed — see `base_refresh_test.go`.

| Concern | Rule | Why |
|---|---|---|
| Base fetch, same `(parent, ref)` | Coalesced onto **one** in-flight `git fetch`; the others wait on it. | Concurrent `git fetch` of one ref contend for `refs/remotes/origin/<ref>` and Git fails the losers (`cannot lock ref ...: is at X but expected Y`). Measured on real Git: 70 of 80 concurrent same-ref fetches failed. |
| Base fetch, different refs, one parent | Deliberately **not** serialized. | Git locks remote-tracking refs individually. Measured on real Git: 0 of 60 concurrent distinct-ref fetches failed. Serializing them would make provision latency linear in fan-out for no correctness gain. |
| Fetch context | The shared fetch runs detached from any single caller, bounded by `BaseFetchTimeout`. | One session cancelling its launch must not fail the sessions waiting on the same fetch. Each caller still observes its own context while waiting. |
| `worktree add` / `worktree remove` / conflict cleanup | Serialized per parent. | These mutate the parent's worktree registry. |
| Lock key | `EvalSymlinks`-canonical parent path. | Absolute, relative, and symlinked spellings of one parent must take one lock. The receipt's `ParentRepoPath` keeps the caller's spelling; canonicalization is a lock-key concern only. |

`BaseFetchDuration` and `BaseFetched` describe the coalesced fetch, which a
concurrent caller may have performed. Both registries are process-wide: two
processes sharing one parent clone still race.

## Retry contract (verbatim port from legacy TS)

- `MaxSpawnRetries = 3`
- `SpawnRetryDelay = 15s`
- Retriable errors: `already checked out`, `Agent already running`, `Agent is still running`, `already exists`. Anything else fails fast.
- Before each retry: `OwnershipProber` is consulted. If the platform reports another worker now owns this session, `Provision` returns `ErrLostOwnership` immediately — no further retries, no further git work.

## Tests

- `manager_test.go` — unit tests with stub `CommandRunner` (no real git). Covers happy path, retry-then-succeed, lost-ownership, non-retriable, exhausted retries, ctx-cancel, both strategies.
- `nested_layout_test.go` — declaration selection, concurrent root isolation,
  root-bound cleanup and leases, retained-flat coexistence, and generation identity.
- `base_refresh_test.go` — the shared-parent concurrency contract above. Stub-runner tests for fetch coalescing, distinct-ref overlap, leader cancellation, ref normalization and teardown-on-verification-failure; real-Git tests that wrap the `CommandRunner` in an overlap counter to prove the parent lock serializes `worktree add`/`remove`/cleanup, that three spellings of one parent take one lock, and that concurrent launches against a stale parent all succeed.
- `integration_test.go` — bare-repo fixture exercises real Git clone and worktree refreshes against a temp repo.

## Failure modes

| Scenario | Behavior |
|---|---|
| Branch already checked out | Cleanup conflict + retry up to `MaxSpawnRetries`. |
| Ownership lost mid-retry | `ErrLostOwnership`; runner halts. |
| Repo URL invalid | Non-retriable; fail-fast on first attempt. |
| ctx cancelled during retry wait | `ctx.Err()` propagated. |
| Teardown on unknown session | No-op (idempotent). |
| Declaration on an unattested executor | Typed refusal before filesystem mutation. |
| Duplicate or unsafe repository leaf | Typed declaration refusal; never auto-renamed. |
| Retained flat workarea plus new multi-repository work | Fresh encoded root beside the flat workarea; the flat directory is not moved or extended. |

## Source

- `manager.go` — `Manager`, `Provision`, `Teardown`, `Path`, `Layout`, and root-bound lease release.
- `../workarea/declaration.go` — the closed declaration/filter grammar and atomic secret-free record.
- `../workarea/layout.go` — typed root/CWD layout plus declared and legacy-flat discovery.
- Legacy reference: `../../../donmai-libraries/packages/core/src/workarea/git-worktree.ts` + `../../../donmai-libraries/packages/cli/src/lib/worker-runner.ts:884-1000`.
