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
- `StrategyWorktreeAdd` — `git worktree add -B <b> <dst> origin/<b>` off an existing parent clone. Cheaper for many concurrent sessions.
- `StrategyEmpty` — create a fresh flat session directory without invoking Git. Used only for work whose accepted session detail has no repository; versioned declarations and shared participants retain their existing strategies.

## Retry contract (verbatim port from legacy TS)

- `MaxSpawnRetries = 3`
- `SpawnRetryDelay = 15s`
- Retriable errors: `already checked out`, `Agent already running`, `Agent is still running`, `already exists`. Anything else fails fast.
- Before each retry: `OwnershipProber` is consulted. If the platform reports another worker now owns this session, `Provision` returns `ErrLostOwnership` immediately — no further retries, no further git work.

## Tests

- `manager_test.go` — unit tests with stub `CommandRunner` (no real git). Covers happy path, retry-then-succeed, lost-ownership, non-retriable, exhausted retries, ctx-cancel, both strategies.
- `nested_layout_test.go` — declaration selection, concurrent root isolation,
  root-bound cleanup and leases, retained-flat coexistence, and generation identity.
- `integration_test.go` (build tag `runtime_integration`) — bare-repo fixture exercises real `git clone` against a temp repo.

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
