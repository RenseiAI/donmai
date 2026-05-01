# `runtime/` — cross-cutting machinery for the agent runner

> **Status:** Wave 6 / Phase F.2.5 (REN-1456). Public package; importable by `rensei-tui` without depending on the rest of `agentfactory-tui`.
> **Spec:** `../../../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md` §1 (layout) + §5 (failure modes).
> **Legacy reference:** `../../../agentfactory/packages/core/src/orchestrator/{state-recovery,heartbeat-writer}.ts`, `../../../agentfactory/packages/core/src/workarea/git-worktree.ts`, `../../../agentfactory/packages/cli/src/lib/worker-runner.ts`.

`runtime/` owns stateful machinery shared across sessions. The runner consumes these sub-packages; provider implementations do not import them.

## Sub-packages

| Sub-package | Responsibility |
|---|---|
| [`runtime/worktree`](./worktree/) | Provision (clone or `git worktree add`) + teardown with `MAX_SPAWN_RETRIES=3` / 15s delay. Probes platform-side session ownership before each retry. |
| [`runtime/env`](./env/) | Compose `KEY=VALUE` env slice from base + spec; strips `AGENT_ENV_BLOCKLIST` keys (verbatim port from legacy TS). |
| [`runtime/mcp`](./mcp/) | Per-session MCP stdio config tmpfile; cleanup tied to session lifecycle. Wire shape consistent with `provider/claude` and `provider/codex`. |
| [`runtime/state`](./state/) | `.agent/state.json` persistence per-worktree. Atomic tmpfile + rename, per-worktree mutex, cross-issue recovery guard, malformed-recovery. |
| [`runtime/heartbeat`](./heartbeat/) | Per-session heartbeat to platform `POST /api/sessions/<id>/lock-refresh`. 3-strike rule emits `LostOwnership` event the runner consumes. Distinct from worker-level heartbeat in `daemon/heartbeat.go`. |

## Layered consumption

```
runner.Run(ctx, RunSpec)
  │
  ├─ runtime/worktree.Manager.Provision(...)        // clone+retry, ownership probe
  ├─ runtime/state.Store.Update(...)                // .agent/state.json (initialized)
  ├─ runtime/env.Composer.Compose(...)              // env for provider subprocess
  ├─ runtime/mcp.Builder.Build(...)                 // tmpfile path + cleanup closure
  ├─ runtime/heartbeat.Pulser.Start(ctx)            // begins lock-refresh loop
  │
  ├─ provider.Spawn(ctx, agent.Spec{Env:..., MCPServers:...})
  ├─ ... event loop ...
  │
  ├─ runtime/heartbeat.Pulser.Stop()
  ├─ runtime/state.Store.Update(... finalStatus ...)
  └─ runtime/worktree.Manager.Teardown(ctx, sessionID)
```

## Failure modes (per F.1.1 §5)

| Surface | Behavior |
|---|---|
| Worktree retry | 3 attempts, 15s between. Ownership probe before each retry; `ErrLostOwnership` short-circuits. |
| Heartbeat HTTP retry | 3-attempt exponential backoff (`1s`, `2s`, `4s`) inside one tick. |
| Heartbeat 3-strike | Three consecutive failed ticks (after inner retries) close `Pulser.LostOwnership()` channel. |
| State concurrency | Per-worktree mutex serializes `Update`. Different worktrees proceed in parallel. |
| State malformed | `Read` returns `ErrMalformed`; `Update` recovers by overwriting with a fresh document. |
| Cross-issue recovery | `ReadExpect` returns `ErrIdentifierMismatch` when on-disk state belongs to a different issue. |

## Tests

```bash
# Unit tests (default)
go test -race ./runtime/...

# Integration tests (gated by build tag — exercises real git binary + httptest)
go test -race -tags=runtime_integration ./runtime/...
```

The top-level `runtime/integration_test.go` exercises Provision → Compose → Build → Write state → Pulse heartbeat → Teardown end-to-end.

## What this package does NOT own

- Provider event mapping → `provider/{claude,codex,stub}/`.
- Per-session orchestration loop → `runner/` (F.2.6).
- Result posting to platform → `result/` (F.2.7).
- Prompt rendering → `prompt/` (F.2.7).
- Daemon worker-level heartbeat → `daemon/heartbeat.go` (already shipped).
