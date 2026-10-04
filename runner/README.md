# `runner/` — per-session orchestration loop

> **Status:** Wave 6 / Phase F.2.6. Public package; importable by `rensei-tui` without depending on the rest of `donmai` daemon plumbing.
> **Spec:** `../../../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md` §1 (layout) + §4 (orchestration) + §5 (failure modes).
> **Legacy reference:** `../../../donmai-libraries/packages/core/src/orchestrator/{agent-spawner,event-processor,session-backstop}.ts`.

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
| `QueuedWork` | Embeds `prompt.QueuedWork` plus `ResolvedProfile`, `Branch`, `WorkerID`, `AuthToken`, `PlatformURL`. Wire shape mirrors the platform Redis session payload. |
| `ResolvedProfile` | Provider, Model, Effort, CredentialID, ProviderConfig, plus the legacy `Runner` field for transitional wire shapes. |
| `Result` | Embeds `agent.Result` plus `SessionID`, `IssueIdentifier`, `StartedAt`, `FinishedAt`, `SteeringTriggered`. |

## Failure modes

Verbatim from F.1.1 §5; classification owned by `runner/failure.go`. The
interactive rows (`interactive-*`) are the runner's own extension for the
PTY-hosted dispatch (`dispatchInteractive`).

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
| `continuations-unproductive` | A turn still ended unfinished (no turn-result manifest, no PR, no verdict) after `TurnContinuationLimit` consecutive continuation prompts whose turns made no tool call. |
| `continuations-ceiling` | A turn still ended unfinished after `TurnContinuationCeiling` continuation prompts in total, productive or not. |
| `continue-pr-diverged` | A continue-mode run's push to the continued pull request's head branch was refused as a non-fast-forward: the head moved after dispatch, so the session's commits were not published. |
| `interactive-input` | The interactive initial prompt exceeded the harness-declared ceiling before spawn (`interactiveInitialPromptLimit` in `interactive_loop.go`): 1,023 bytes for harnesses that type the seed into the live PTY (the kernel canonical-mode bound), 32 KiB for harnesses that carry the seed as a spawned argv element. Counts and the limit travel on the session record; prompt content never does. |
| `interactive-unsupported` | A mode:`interactive` dispatch spawned a non-interactive handle (no PTY transport). |
| `interactive-config` | Exactly one of the relay-attach variables was set (both or neither required). |

## Interactive initial prompts

An interactive seed reaches its harness by one of two native paths, and the
pre-spawn byte ceiling follows the path the harness declares — never the
harness's name (`interactiveInitialPromptLimit` reads the live manifest's
human-controlled user delivery):

- PTY-typed seeds (1,023 bytes): the seed is written into the live terminal
after spawn, where the kernel canonical-mode line discipline truncates an
unterminated line past `MAX_CANON`. A larger seed would arrive corrupted,
so the runner fails the session before spawn instead.
- Argv-carried seeds (32 KiB): the seed rides a spawned argv element (the
positional prompt argument), which never crosses the line discipline. The
ceiling only stops an unbounded caller payload from becoming an unbounded
spawn argument, with wide headroom for real coordinator briefs.

Keep seeds small even under the ceiling: a short pointer-prompt that names
where the full brief already waits (the session's durable message inbox,
fetched at session start) stays well under either bound, keeps the full
brief durable when the terminal scrolls, and avoids re-sending kilobytes on
every retry.

## Tail recovery

A pull request URL in the conversation is only a candidate. For work that owes
a pull request, the runner accepts one only when it exists on the session's own
repository with the session's branch or commit as its head, read from the
checkout's own remote with the session's git credential
(`git ls-remote origin refs/pull/<n>/head refs/heads/<branch>`; no GitHub CLI).
The session's repository is recognised in any remote form (https, ssh,
scp-like, with credentials). A quoted example URL, or a pull request on another
repository or branch, leaves the session without one, so the stages below still
run. See `pull_request_verify.go`.

Before either stage, the runner reads how the latest turn ended
(`turn_continuation.go`), for work that owes a pull request:

- A turn that stopped early — a clean end with no turn-result manifest, no
  verified pull request and no verdict — gets a short "continue the task"
  prompt instead of the pull request nudge.
- A turn that ended on a model provider error (`agent.SystemSubtypeProviderError`,
  which the pi harness emits for an assistant message with stopReason
  `error`) is retried after a short wait, never nudged.
- A blocked or failed verdict is never continued; a passed verdict without a
  pull request gets the steering nudge, once.

Continuations are bounded by progress. A turn is productive when its event
stream carried at least one tool call (`agent.ToolUseEvent`); whether the call
changed the workspace is not judged, because read or write semantics are
harness-specific.

- `Options.TurnContinuationLimit` (default 3) bounds the *consecutive*
  continuation prompts whose turn made no tool call. Any productive turn
  (continuation, retry or nudge) resets the streak, so an agent that ends its
  turns early but keeps working is not cut off. Exhausted:
  `continuations-unproductive`.
- `Options.TurnContinuationCeiling` (default 50) bounds continuation prompts in
  total, productive or not. Exhausted: `continuations-ceiling`.
- `Options.TurnContinuationUndeliveredLimit` (default 10) bounds the
  continuation prompts sent in a row while the session's pull request does not
  deliver the work (still a draft, or a rework with no new commit) and gains no
  commit of the session's own. A turn that pushes to the pull request starts
  the count again; continuations for any other reason never count. Exhausted:
  `continuations-ceiling`.
- Provider-error retries keep a total bound of `TurnContinuationLimit` with no
  progress reset. Exhausted: `provider-error`.

For each option zero is the default; a negative limit disables continuations
and retries, a negative ceiling removes the ceiling, and a negative undelivered
limit removes that separate bound. The session's own
duration and token budgets cover every follow-up turn. At an exhausted bound no
nudge follows, but the backstop still makes its open-PR attempt, so the work is
pushed and a real pull request the verifier could not confirm is recovered; the
session stays failed. `Result.TurnContinuations` carries the counts (total
continuations, the unproductive streak, retries) and both bounds onto the
terminal status.

Two-stage post-completion recovery (F.0.1 §1):

1. **Stage 1 — steering.** Fires when the provider supports `SupportsMessageInjection` *or* `SupportsSessionResume` AND the session ended successfully but without a PR URL. The runner delivers a templated follow-up prompt asking the agent to commit/push/PR — via `Handle.Inject` when the live handle accepts it, falling back to a stop-and-resume (`Handle.Stop` then `Provider.Resume` with the prompt riding the resumed `Spec.Prompt`) when the handle rejects the inject as unsupported and the harness declares `SupportsSessionResume` (Codex; OpenCode's one-shot lane). `Result.SteeringResumeFallback` records when the fallback path fired. Skipped via `Options.SkipSteering`.
2. **Stage 2 — backstop.** Deterministic git workflow when steering didn't produce a PR (or the provider doesn't support steering). Skipped via `Options.SkipBackstop` for tests that don't have a real remote.

The backstop's path-exclude list is ported verbatim from `donmai-libraries/packages/core/src/orchestrator/session-backstop.ts:57-95`. The list lives at the top of `backstop.go` as Go data tables (`excludeDirAnyDepth`, `excludeDirTopLevel`, `excludeExtensions`, `excludeBasenamePrefixes`, `excludePathPrefixes`). When the legacy TS adds an entry, port it here in the same wave.

Backstop steps:
1. `git status --porcelain` — snapshot uncommitted state.
2. `git add -A` — stage everything.
3. Enumerate staged files via `git diff --cached --name-only`; unstage anything matching the path-exclude tables.
4. Safety cap: abort when staged count > `backstopMaxFiles` (200) — likely indicates a cache or `node_modules` slipped through.
5. `git commit -m "Backstop: <session-id> (<identifier>)"` (skipped when nothing remains staged).
6. `git push -u origin <branch>` (with `--force-with-lease` retry on non-fast-forward).
7. `gh pr create --title --body` — return the URL on `BackstopReport.PRURL`.

## Teardown never deletes unpublished work

Before teardown deletes the workarea, every git checkout in it is checked for
work that exists nowhere else: uncommitted changes (including untracked files)
and commits no remote holds. That work is archived as a patch against the
commit the checkout started at, at
`<RescueDir>/<session>/<time>-<id>/<repository>.patch` with a JSON sidecar, and the
log names the file. Paths the backstop never commits (dependency and build
output, runner and harness state) are left out. The checkout itself is not
touched. The patch is written with plumbing (`git diff-tree` with every output
option pinned), so no diff configuration can alter it, and it must reproduce the
working state when applied to its base in a scratch index. When the archive
cannot be written or proven, the workarea is kept instead of deleted. Each
session keeps its last 5 archives. `Options.RescueDir` defaults to a `rescue`
directory beside the worktree parent. See `workarea_rescue.go`.

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
