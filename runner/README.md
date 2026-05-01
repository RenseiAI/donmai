# `runner/` — per-session orchestration loop

> **Status:** Wave 6 / Phase F.2.6 (REN-1459). Public package; importable by `rensei-tui` without depending on the rest of `agentfactory-tui` daemon plumbing.
> **Spec:** `../../../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md` §1 (layout) + §4 (orchestration) + §5 (failure modes).
> **Legacy reference:** `../../../agentfactory/packages/core/src/orchestrator/{agent-spawner,event-processor,session-backstop}.ts`.

`runner/` ties the Wave 2 + Wave 2b building blocks together into the per-session main loop. F.2.8 (daemon wire-up) calls `Runner.Run` for every claimed `QueuedWork` and the function does not return until the session is fully terminated, the result has been posted, and the worktree torn down.

## Lifecycle diagram

```
                  rensei-tui daemon (F.2.8)
                          │
                          ▼  qw QueuedWork
              ┌────────────────────────────┐
              │ runner.Run(ctx, qw)        │
              │                            │
              │ 1. Resolve provider        │
              │      registry.Resolve()    │  → agent.Provider
              │ 2. Provision worktree      │
              │      worktree.Provision()  │  → /tmp/.../wt
              │ 3. Compose env             │
              │      env.Compose()         │  blocklist applied
              │ 4. Build MCP config        │
              │      mcp.Build()           │  tmpfile + cleanup
              │ 5. Render prompt           │
              │      prompt.Build()        │  (system, user)
              │ 6. Translate spec          │
              │      translateSpec()       │  capability-gated
              │ 7. Init state.json         │
              │ 8. Spawn provider          │
              │      provider.Spawn()      │  → agent.Handle
              │ 9. Start heartbeat         │
              │      heartbeat.Start()     │
              │10. Stream events           │  → events.jsonl
              │      consumeEvents()       │  → state.json
              │11. Wait for terminal       │
              │12. Tail recovery           │
              │     a. Steering (cap-gated)│
              │     b. Backstop (det. git) │
              │13. Post Result             │
              │      poster.Post()         │  → /completion + /status
              │14. Teardown                │
              │      worktree.Teardown()   │
              └────────────────────────────┘
                          │
                          ▼  *Result
                       caller
```

## Public surface

| Symbol | Purpose |
|---|---|
| `Runner` | Long-lived per-daemon orchestrator. Build once via `New(opts)`. |
| `Options` | DI seam: registry, worktree manager, poster, env composer, MCP builder, state store, prompt builder, http client, logger, timeouts. Required: Registry, WorktreeManager, Poster. |
| `Registry` | `agent.ProviderName → agent.Provider` lookup. Built at daemon startup. |
| `QueuedWork` | Embeds `prompt.QueuedWork` plus `ResolvedProfile`, `Branch`, `WorkerID`, `AuthToken`, `PlatformURL`, `IssueLockID`. Wire shape mirrors the platform Redis session payload. |
| `ResolvedProfile` | Provider, Model, Effort, CredentialID, ProviderConfig, plus the legacy `Runner` field for transitional wire shapes. |
| `Result` | Embeds `agent.Result` plus `SessionID`, `IssueIdentifier`, `StartedAt`, `FinishedAt`, `SteeringTriggered`. |

## Failure modes

Verbatim from F.1.1 §5; classification owned by `runner/failure.go`.

| FailureMode constant | When it fires |
|---|---|
| `worktree-provision` | `runtime/worktree.Manager.Provision` failed after retry budget. |
| `prompt-render` | Prompt builder rejected the QueuedWork (empty issue context) or validation failed. |
| `provider-resolve` | Registry has no entry for `qw.ResolvedProfile.Provider`. |
| `spawn-failed` | `Provider.Spawn` returned error before events channel opened. |
| `provider-error` | An ErrorEvent arrived before any terminal ResultEvent. |
| `silent-exit` | The events channel closed without a terminal ResultEvent. |
| `lost-ownership` | Heartbeat 3-strike threshold tripped (or worktree retry detected ownership loss). |
| `timeout` | `ctx` cancelled before terminal event. |
| `backstop-failed` | Stage 2 of tail recovery ran but could not push or open a PR. |

## Tail recovery

Two-stage post-completion recovery (F.0.1 §1):

1. **Stage 1 — steering.** Fires when the provider supports `SupportsMessageInjection` *or* `SupportsSessionResume` AND the session ended successfully but without a PR URL. The runner injects a templated follow-up prompt asking the agent to commit/push/PR. Skipped via `Options.SkipSteering`.
2. **Stage 2 — backstop.** Deterministic git workflow when steering didn't produce a PR (or the provider doesn't support steering). Skipped via `Options.SkipBackstop` for tests that don't have a real remote.

The backstop's path-exclude list is ported verbatim from `agentfactory/packages/core/src/orchestrator/session-backstop.ts:57-95`. The list lives at the top of `backstop.go` as Go data tables (`excludeDirAnyDepth`, `excludeDirTopLevel`, `excludeExtensions`, `excludeBasenamePrefixes`, `excludePathPrefixes`). When the legacy TS adds an entry, port it here in the same wave.

Backstop steps:
1. `git status --porcelain` — snapshot uncommitted state.
2. `git add -A` — stage everything.
3. Enumerate staged files via `git diff --cached --name-only`; unstage anything matching the path-exclude tables.
4. Safety cap: abort when staged count > `backstopMaxFiles` (200) — likely indicates a cache or `node_modules` slipped through.
5. `git commit -m "Backstop: <session-id> (<identifier>)"` (skipped when nothing remains staged).
6. `git push -u origin <branch>` (with `--force-with-lease` retry on non-fast-forward).
7. `gh pr create --title --body` — return the URL on `BackstopReport.PRURL`.

## Telemetry

Every event the provider emits is mirrored three ways:

1. **Local audit log** — appended to `<worktree>/.agent/events.jsonl` as a JSON line decodable via `agent.UnmarshalEvent`.
2. **State snapshot** — `<worktree>/.agent/state.json` is updated on every InitEvent + on terminal events. Atomic tmpfile + rename via `runtime/state.Store`.
3. **Logger** — `slog.Default()` (or `Options.Logger`) receives Debug/Info/Warn lines describing each step.

Per-event activity streaming to the platform (`POST /api/sessions/<id>/activity`) is left to F.5; today's runner only posts the terminal Result via `result.Poster.Post`.

## Testing

```bash
# Unit + smoke (default)
go test -race ./runner/...

# Including the build-tagged integration test
go test -race -tags=runner_integration ./runner/...
```

The unit tests use the `provider/stub` package's `BehaviorSucceedWithPR`, `BehaviorMidStreamError`, `BehaviorSilentFail`, `BehaviorHangThenTimeout`, and `BehaviorInjectTest` modes to exercise every classification branch without depending on a real provider. The integration test (`runner_integration` build tag) drives a full Run() against a real bare-repo + httptest platform mock.

Tests skip when `git` is not on `PATH` so a barebones CI runner does not red-X the suite.

## Extending

- **New provider** — implement `agent.Provider` in `provider/<name>/`, register it in the registry at daemon startup. The runner's contract (capability-gated `agent.Spec`, normalized event channel) absorbs new providers without runner changes.
- **New work type** — add a template to `prompt/templates/`; the runner reads `qw.WorkType` opaquely.
- **New failure mode** — add a `Failure*` constant to `runner/failure.go` and update the classification site in `runLoop`.
- **New backstop exclusion** — add to the data tables at the top of `backstop.go`; keep order/contents byte-identical with the legacy TS source.

## What this package does NOT own

- Provider-native event mapping → `provider/{claude,codex,stub}/`.
- Worktree git ops → `runtime/worktree`.
- Prompt rendering → `prompt/`.
- Result HTTP calls → `result/`.
- Heartbeat lock-refresh → `runtime/heartbeat`.
- Session credential resolution → daemon (the runner consumes `qw.AuthToken` opaquely).
