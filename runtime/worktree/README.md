# `runtime/worktree/`

Per-session git worktree provisioning + teardown for the agent runner.

## What it does

```go
m, _ := worktree.NewManager(worktree.Options{
    ParentDir:       "/var/lib/rensei/wt",
    OwnershipProber: afclient.GetSessionOwnership,
})
path, err := m.Provision(ctx, worktree.ProvisionSpec{
    SessionID:      "sess-123",
    RepoURL:        "git@github.com:org/repo.git",
    Branch:         "main",
    Strategy:       worktree.StrategyClone,
    RepositoryLeaf: workarea.RepositoryLeaf("git@github.com:org/repo.git"),
})
// path == /var/lib/rensei/wt/sess-123/repo — the agent runs HERE.
layout, _ := m.Layout("sess-123")
// layout.Root       == /var/lib/rensei/wt/sess-123   (session-owned)
// layout.Repository == path                          (mutable git authority)
m.Teardown(ctx, "sess-123") // removes layout.Root, atomically
```

## Session-owned workarea namespace

A session owns a workspace that CONTAINS repositories; it is not synonymous with one git repository:

```text
<ParentDir>/<session-id>/               ← workarea root (workarea.RootPath)
<ParentDir>/<session-id>/<repo-leaf>/   ← selected repository (workarea.RepositoryPath), the agent CWD
<ParentDir>/<session-id>/<other-repo>/  ← this session's read-only context repositories
```

- `Provision` returns, and `Path` reports, the **repository** path — semantics unchanged from before nesting, so mixed-version readers of `worktreePath` stay correct. `Layout` is the typed accessor for both paths; `workareaRoot` is the additive wire field.
- Cleanup, terminal leases, archives and disk accounting own the **root** atomically. Completion, branch, landing and every mutable git authority stay scoped to the **repository**.
- Omitting `RepositoryLeaf` keeps the retained **flat** layout (`<ParentDir>/<session-id>` IS the repository), so pre-nesting callers and already-provisioned workareas keep working. `workarea.DiscoverLayout` finds either shape on disk.

## Strategies

- `StrategyClone` — `git clone --branch <b> <repo> <dst>`. Fully isolated session; one full repo per session.
- `StrategyWorktreeAdd` — `git worktree add -B <b> <dst> origin/<b>` off an existing parent clone. Cheaper for many concurrent sessions.

## Retry contract (verbatim port from legacy TS)

- `MaxSpawnRetries = 3`
- `SpawnRetryDelay = 15s`
- Retriable errors: `already checked out`, `Agent already running`, `Agent is still running`, `already exists`. Anything else fails fast.
- Before each retry: `OwnershipProber` is consulted. If the platform reports another worker now owns this session, `Provision` returns `ErrLostOwnership` immediately — no further retries, no further git work.

## Tests

- `manager_test.go` — unit tests with stub `CommandRunner` (no real git). Covers happy path, retry-then-succeed, lost-ownership, non-retriable, exhausted retries, ctx-cancel, both strategies.
- `nested_layout_test.go` — the session-owned namespace: nesting under the session root, the retained flat layout, root-atomic teardown that leaves a concurrent session's workarea untouched, and root-scoped terminal-lease retention.
- `integration_test.go` (build tag `runtime_integration`) — bare-repo fixture exercises real `git clone` against a temp repo.

## Failure modes

| Scenario | Behavior |
|---|---|
| Branch already checked out | Cleanup conflict + retry up to `MaxSpawnRetries`. |
| Ownership lost mid-retry | `ErrLostOwnership`; runner halts. |
| Repo URL invalid | Non-retriable; fail-fast on first attempt. |
| ctx cancelled during retry wait | `ctx.Err()` propagated. |
| Teardown on unknown session | No-op (idempotent). |

## Source

- `manager.go` — `Manager`, `Provision`, `Teardown`, `Path`.
- Legacy reference: `../../../donmai-libraries/packages/core/src/workarea/git-worktree.ts` + `../../../donmai-libraries/packages/cli/src/lib/worker-runner.ts:884-1000`.
