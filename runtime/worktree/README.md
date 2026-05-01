# `runtime/worktree/`

Per-session git worktree provisioning + teardown for the agent runner.

## What it does

```go
m, _ := worktree.NewManager(worktree.Options{
    ParentDir:       "/var/lib/rensei/wt",
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
- Legacy reference: `../../../agentfactory/packages/core/src/workarea/git-worktree.ts` + `../../../agentfactory/packages/cli/src/lib/worker-runner.ts:884-1000`.
