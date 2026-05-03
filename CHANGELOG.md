# Changelog

All notable changes to `agentfactory-tui` (`af` binary) are documented here.

Format: `## vX.Y.Z — YYYY-MM-DD` with subsections `Features`, `Fixes`, `Chores`. Unreleased work goes under `## [Unreleased]`.

---

## [Unreleased]

_Placeholder for v0.4.0 work. Move items here as they merge._

### Features (v0.5.5 — REN-1485 / REN-1487 / REN-1481)

- **Phase 2 daemon-side stage-prompt scaffolding (REN-1487)** — Closes the runner-side gap left by the platform PR #154 that introduced `agent.dispatch_stage`. The daemon's `PollWorkItem` and `SessionDetail` now decode + forward five new wire fields the platform's `QueuedStageWork` extension carries: `stagePrompt` (pre-rendered user-prompt body), `stageId` (canonical stage identifier), `stageBudget` (`{maxDurationSeconds, maxSubAgents, maxTokens}`), `stageLifecycle` (opaque map for the platform to resolve native target states on success/failure), `stageSourceEventId` (CloudEvent correlation id). The runner's `prompt.Builder.Build` now SHORT-CIRCUITS the embedded user-template renderer when `qw.StagePrompt` is non-empty: the platform-rendered prompt is used verbatim with a stage-context preamble (`<stage>development</stage>` / `<stageBudget …/>` / `<stageSourceEventId>…</stageSourceEventId>`) so the agent can self-identify which stage instance it is operating in. Cardinal rule 1 holds: when `StagePrompt` is empty the renderer falls back to the legacy `PromptContext` / `Body` / per-work-type-template path (development / qa / research). New env vars `AGENTFACTORY_STAGE_ID` / `AGENTFACTORY_STAGE_MAX_*` surface the stage context to spawned sub-agents. Each `runner.Run` logs one `[runner-stage] sid=… stageId=… mode=stage|legacy` line for grep-driven correlation.

- **Sub-agent budget enforcement at runtime (REN-1487 acceptance #4)** — New `runner/budget.go` package adds a per-session `BudgetEnforcer` that tracks wall-clock, Task tool invocations, and token usage against the queue payload's `stageBudget`. Wall-clock enforcement uses a `context.WithDeadline` derived from the run start; Task counting matches `Task` and `*__Task` (MCP-namespaced) tool names case-insensitively; token counting sums `InputTokens + OutputTokens` from every per-turn `ResultEvent.Cost` (and the terminal one). On any cap breach the runner: cleanly stops the provider, classifies the failure as `FailureBudgetExceeded` (new `runner/failure.go` constant via `budget.go`), records the breach reason (`max-duration-seconds` / `max-sub-agents` / `max-tokens`) on `Result.BudgetReport`, and posts WORK_RESULT with the breach detail. `BudgetReport` is non-nil on every Run (with `.Enforced=false` for legacy work) so dashboards can distinguish "no budget" from "budget OK". Disabled-enforcer (legacy `agent.dispatch_to_queue` path with no `stageBudget`) is a no-op; cardinal rule 1 holds.

- **Runtime-token refresh probe (REN-1481)** — Closes the 5-min `401 → re-register → 404` cycle described in REN-1481. The daemon's `OnReregister` callback (used by both `HeartbeatService` and `PollService`) now routes through new `daemon/runtime_token.go::RefreshRuntimeToken` which **probes `POST /api/workers/<id>/refresh-token` first** with the registration token — preserving the workerId — and only falls back to the existing full `Register(ForceReregister=true)` call (which mints a new workerId, the original bug) when the platform returns 404 / 405 (endpoint not deployed). Once the platform-side companion ships, the daemon picks up the refresh path automatically with no further changes. New `[runtime-token] event=… workerId=… reason=…` structured log line attests which path was taken on every cycle (event values: `401` / `auth-failure-detected` / `refresh` / `refresh.unavailable` / `refresh.error` / `reregister` / `reregister.error`). 401 classification now distinguishes the platform's specific `Runtime token expired` message (`reason=runtime-token-expired`) from generic 401 (`reason=unauthorized`) and 404 (`reason=worker-not-found`) so operators see at a glance which trigger fired the cycle. `RegistrationTokenSwapped=true` flag on the refresh result surfaces when re-register burned the workerId — the operationally noisy signal originally documented in REN-1481.

- **`daemon.Version` bumped to `0.5.5`** — bundles all three above; reported in registration / status payloads.

### Features (v0.5.4 — REN-1467)

- **Runner WORK_RESULT → Linear state-transition wiring** — The Go runner now closes the Wave 6 Phase F.2.5 gap that left dev sessions in `Backlog` after a passing implementation. New `runner/sdlc.go` ports the `WORK_TYPE_COMPLETE_STATUS` / `WORK_TYPE_FAIL_STATUS` tables from `agentfactory/packages/linear/src/types.ts` and the post-session decision tree from `packages/core/src/orchestrator/event-processor.ts:300-450`. New `runner/contracts.go` ports the per-workType completion contract; development / inflight / coordination / inflight-coordination now require a `WORK_RESULT:passed|failed` marker. New `runner/post_session.go` implements step 11b of the run loop — parses the marker, resolves the target Linear status, and POSTs `updateIssueStatus` to the platform's `/api/issue-tracker-proxy` endpoint via the worker bearer token. Unknown markers post a diagnostic comment instead of stalling the issue. Failures surface as `Result.PostSessionWarnings` + `Result.LinearStatusTransition` (best-effort; a failed transition does NOT change the session's terminal status). Acceptance work continues to defer to the merge worker when a merge-queue adapter is configured (`shouldDeferAcceptanceTransition`, REN-503/REN-1153). The development prompt template now includes the WORK_RESULT marker requirement so agents emit it on every dev session (REN-1467).
- **Result poster gains `UpdateIssueStatus` + `CreateIssueComment`** — `result/issue_status.go` exposes the platform's issue-tracker-proxy via two thin methods on `Poster`. Same retry/backoff/permanent-vs-transient classification as the existing `Post` path; reuses the worker bearer token and platform URL the runner already has (no new credential surface).

### Features

- **Daemon registers against the real platform** — `daemon/registration.go` and `daemon/heartbeat.go` now target the platform's `POST /api/workers/register` and `POST /api/workers/<id>/heartbeat` endpoints (was: non-existent `/v1/daemon/register` and `/v1/daemon/heartbeat`). Registration token is sent in `Authorization: Bearer`, not in the body. Wire shape: request `{hostname, capacity, version, projects?}`; response `{workerId, heartbeatInterval (ms), pollInterval (ms), runtimeToken, runtimeTokenExpiresAt}`. Heartbeat body is `{activeCount, load?}`. Stub-vs-real switch now accepts both `rsp_live_*` (legacy) and `rsk_live_*` (REN-1351 unified mint) prefixes. Runtime-token expiry (1h TTL, no refresh endpoint) is handled by re-register-on-401/404 with credential swap inside `HeartbeatService` (REN-1422).
- **Daemon version bumped to `0.4.0-dev`** — replaces `0.3.10-sidecar` reported by the bash heartbeat shim that shipped for the 2026-05-01 demo (REN-1422).

### Fixes (v0.5.3 hotfix bucket — REN-1465)

- **Runner heartbeat sends Linear `issueId`, not empty `IssueLockID`** — `runner/loop.go` now constructs `heartbeat.Config{IssueID: qw.IssueID, ...}` instead of sourcing it from `qw.IssueLockID` — a field the platform's poll response never populated, so the runner's `/api/sessions/<id>/lock-refresh` body was always `{"workerId":"...","issueId":""}` and the platform handler returned `400 "workerId and issueId are required"`. Result: 100% of v0.5.0+ heartbeats failed; sessions tripped `LostOwnership` after 3 strikes (~90s on the default 30s interval) on every real run. v0.5.1's child-output capture (REN-1463) is what made the failure visible in `daemon-error.log`. Removed the unused `IssueLockID` wire field from `runner.QueuedWork`, `daemon.PollWorkItem`, `daemon.SessionDetail`, and the daemon→runner copy in `afcli/agent_run.go` — there is no separate "lock id" concept on the platform; `issue:lock:{linearIssueId}` is the canonical key. New `TestRunLoop_HeartbeatBodyIncludesIssueID` regression captures the bug (REN-1465).

### Fixes (v0.5.2 hotfix bucket — REN-1464)

- **Daemon resolves `projectName` to repository URL via allowlist** — When the platform's poll response carries a `projectName` slug (e.g. `"smoke-alpha"`) with no separate repository field — the canonical wire shape per the live Redis QueuedWork — the daemon's `pollItemToSessionDetail` / `pollItemToSessionSpec` now look up the matching `daemon.yaml` allowlist entry and substitute `p.Repository` (the GitHub URL) into `SessionDetail.repository`. The runner uses this URL for `git clone`. Before this fix the slug was forwarded unchanged, producing the v0.5.1 failure mode `runner.Run: git clone: exit status 128 (fatal: repository 'smoke-alpha' does not exist)` (REN-1463). Match logic mirrors `WorkerSpawner.findProjectLocked` (REN-1448) — by `id`, `repository`, or URL-suffix. When no allowlist entry matches, the daemon falls back to whatever was on the wire and emits a Warn log so operators see the misconfiguration (REN-1464).

### Fixes (v0.5.1 hotfix bucket — REN-1463 / REN-1462)

- **Spawn child stdout/stderr default to slog** — `daemon.New` now installs default `StdoutPrefixWriter` / `StderrPrefixWriter` on the spawner that emit one slog record per child line: stdout → INFO, stderr → WARN, both tagged with `sessionID` and `stream` attributes and prefixed `[child stdout|stderr sessionID=<id>]` in the message. v0.5.0 dropped child output to `io.Discard` by default, leaving operators flying blind between `runner.Run()` start and a `status=failed` post. Callers passing their own writers via `SpawnerOptions` retain priority (REN-1463).
- **`af agent run` provider probe failures are visible** — Every provider construction or registration failure now logs at WARN with `provider=<name>` and `err=<...>`. If every probe fails, an ERROR record fires (`no providers available`) so the misconfiguration surfaces instead of silently producing a session that fails resolution at runtime (REN-1462).
- **Default plist + systemd PATH includes `~/.local/bin`** — Both the macOS launchd plist (`installer/launchd`) and Linux systemd unit (`installer/systemd`) now prepend the invoking user's `~/.local/bin` to PATH so user-local installs of provider CLIs (e.g. the upstream `claude` curl|sh installer) are visible to the daemon. Resolves at install time from `os.UserHomeDir()` (or `SUDO_USER` for system-scope systemd units) (REN-1462).

---

## v0.3.0 — 2026-04-30

### Features

- **Public `installer/` package — launchd + systemd in-process** — Port of the legacy TS daemon installers to Go. `installer/launchd/` and `installer/systemd/` generate plist/unit files that register `<host-binary> daemon run` (subcommand pattern, single-binary OSS UX). Public package importable by downstream binaries (`rensei`); replaces the previous shell-out to a Node `rensei-daemon` binary (REN-1406).
- **Public `daemon/` package — full HTTP server + lifecycle ops** — Port of the legacy TS daemon runtime (~1.6K LOC across registration, heartbeat, worker-spawner, auto-update, config, setup-wizard, types). 14 HTTP endpoints (status, stats, pause, resume, stop, drain, update, capacity, pool/stats, pool/evict, sessions, heartbeat, doctor, healthz). Includes drain semantics, JWT-derived tenancy, and TTY-aware setup wizard. Importable by downstream binaries (REN-1408).
- **`af daemon run` subcommand** — Long-running daemon entry point on port 7734; replaces the deprecated `@renseiai/daemon` Node package as the canonical service binary. Inherited by `rensei daemon run` via `afcli.RegisterCommands` (REN-1408).
- **`af daemon install / uninstall / doctor` rewired in-process** — Calls into the new Go installer rather than `exec.Command("rensei-daemon", …)`. No Node.js dependency on the install path (REN-1406).

### Chores

- **Acceptance discipline: binary-distribution gate** — Hard Rule 7 added to `migration-coordinator.yaml`: any "wire / install / register a binary" issue requires fresh-machine smoke verification at Acceptance, not just CI green (REN-1407).

---

## v0.2.2 — 2026-04-30

### Features

- **`af daemon install / uninstall / doctor` wiring** — OSS mirror of the daemon shell-out work: `exec.Command` calls into the underlying `rensei-daemon` (or equivalent) binary, forwarding stdin/stdout/stderr and passthrough flags (REN-1347, REN-1348).
- **`af logs analyze`** — `af-analyze-logs` ported to Go; full pattern catalog parity with the legacy TS implementation (REN-1359).
- **`af linear`** — `af-linear` CLI ported to Go; covers issue CRUD, comments, sub-issues, relations, and deployment checks (REN-1360).
- **`af orchestrator`** — `af-orchestrator` ported to Go (REN-1361).
- **`af admin {cleanup, queue, merge-queue}`** — Admin commands ported to Go from the legacy TS CLI (REN-1362).
- **`af code` and `af arch`** — Shell-out bridges that compose with the existing `pnpm af-code` / `pnpm af-arch` toolchains, completing Phase D parity (REN-1363).

### Chores

- **README authored** — Full README with command surface map (REN-1364).
- **RELEASING.md and CHANGELOG.md established** — Tag-driven GoReleaser release flow documented; this changelog established (REN-1366).

---

## v0.2.1 — 2026-04-29

### Chores

- **CI: drop Windows target** — Remove Windows from goreleaser cross-compile matrix; the binary only targets darwin and linux (REN-1346).

---

## v0.2.0 — 2026-04 (cycle 2)

### Features

- **`af governor start` in-process** — Governor scan/dispatch loop runs inside the binary; no longer shells out to an external `agentfactory` binary. Includes PID file, daemonize, and signal-handler primitives (REN-1030, REN-1031, REN-1032, REN-1033).
- **Linear GraphQL client** — Internal Linear client for governor scan loop, porting the TypeScript reference implementation to Go (REN-1028).
- **Redis queue client** — Internal Redis client wrapper for governor dispatch (REN-1027).
- **`af daemon` command tree** — 12 subcommands covering daemon install/uninstall/start/stop/status/doctor and pool management (REN-1301, REN-1334, REN-1347, REN-1348).
- **`af project` commands** — `af project allow`, `project credentials`, `project list`, `project remove` (REN-1302).
- **`afclient` types for Machine/Daemon/Pool/Workarea/Sandbox/Kit** — Expanded API type surface for downstream consumers (REN-1333).
- **Dashboard SandboxProvider column + filter** — Dashboard now shows and filters by sandbox provider (REN-1303).
- **`RegisterRequest.CapabilitiesTyped`** — Typed capabilities field added to the worker registration protocol (REN-1282).
- **`af admin` commands** — `af admin cleanup`, `admin queue`, `admin merge-queue` ported to Go from TypeScript CLI (REN-1362).
- **`af logs analyze`** — `af-analyze-logs` ported to Go (REN-1359).
- **`af linear` commands** — Full `af-linear` CLI ported to Go (REN-1360).
- **`af code` and `af arch`** — `af-code` and `af-arch` shell-out bridges ported to Go (Phase D parity) (REN-1363).
- **`af orchestrator`** — `af-orchestrator` command ported to Go (REN-1361).
- **tui-components v0.2.0 Theme migration** — Migrated to the updated `Theme` struct (REN-1319).

### Fixes

- **gocritic / staticcheck lint cleanup** — Resolve `ifElseChain → switch`, `deprecatedComment`, `S1011` across new packages.

---

## v0.1.3 — 2026-02

_Earlier cycle-1 releases. See git log for full history._

### Features

- Initial `af dashboard` TUI with fleet status view
- `af status` inline output
- `af agent list / status / stop / chat / reconnect`
- `af fleet` subcommands
- `af queue` subcommands
- Worker protocol: register, poll, heartbeat

---

## v0.1.0 — 2026-01

### Features

- **Initial release** — `af` binary scaffolded with Cobra CLI framework, Bubble Tea TUI, and `afclient` API client. Covers `dashboard`, `status`, and `agent` commands against the AgentFactory coordinator API.
- **Public library surface** — `afclient`, `afcli`, and `worker` packages are importable by downstream consumers (e.g., `rensei-tui`).
- **Cross-platform builds** — darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 via goreleaser.
