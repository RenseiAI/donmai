# `daemon/` — long-running rensei-daemon runtime

> **Status:** Wave 6 / Phase F.2.8 (REN-1461). Public package; the
> `af daemon …` CLI surface is in `afcli/daemon.go`.
> **Architecture:** `rensei-architecture/004-sandbox-capability-matrix.md`
> §Local daemon mode + `011-local-daemon-fleet.md`.

The daemon is a single-machine, multi-project supervisor that:

1. Registers itself with the platform (`/api/workers/register`) and
   exchanges a one-time `rsp_live_*` token for a scoped runtime JWT.
2. Sends a periodic heartbeat (`/api/workers/<id>/heartbeat`) and polls
   for queued work (`/api/workers/<id>/poll`).
3. Accepts inbound `SessionSpec` payloads and spawns a worker child
   process per accepted session.
4. Exposes a localhost-only HTTP control API on `127.0.0.1:7734` for
   the `af` CLI and for the spawned worker children themselves.
5. Optionally self-updates by drain → fetch → verify → swap → restart.

## Spawn flow (F.2.8)

```
        ┌────────────────────┐
        │ platform.poll()    │  GET /api/workers/<id>/poll
        └──────────┬─────────┘
                   │ work[] item
                   ▼
        ┌────────────────────┐
        │ Daemon.AcceptWork  │
        │   WithDetail()     │
        └──────────┬─────────┘
                   │ stores SessionDetail
                   ▼
        ┌────────────────────┐
        │ WorkerSpawner.spawn│  exec.CommandContext(<af>, "agent", "run")
        │                    │  env: RENSEI_SESSION_ID=<id>,
        │                    │       RENSEI_REPOSITORY=<repo>, …
        └──────────┬─────────┘
                   │
                   ▼
        ┌────────────────────┐
        │ af agent run       │  GET 127.0.0.1:7734/api/daemon/sessions/<id>
        │   (afcli/agent_run)│  → SessionDetail with QueuedWork shape +
        │                    │     AuthToken + PlatformURL + WorkerID
        └──────────┬─────────┘
                   │ runner.Run(ctx, qw)
                   ▼
        ┌────────────────────┐
        │ runner orchestrator│  worktree → spawn provider → events →
        │                    │  tail recovery → result.Post
        └────────────────────┘
```

The `af` binary registered by `daemon install` doubles as both the
daemon supervisor (`af daemon run`) and the per-session worker
(`af agent run`) — the same binary, different subcommands. The
WorkerCommand defaults to `[<self-exe>, "agent", "run"]` resolved via
`os.Executable()`; operators rarely override this.

### `SessionDetail` lifecycle

- **Set**: `Daemon.AcceptWorkWithDetail` records the detail in an
  in-memory map keyed by session id when the poll loop dispatches a
  work item.
- **Read**: `GET /api/daemon/sessions/<id>` (handled by
  `daemon/server.go::handleSessionDetail`) returns the JSON payload
  to the spawned `af agent run` worker.
- **Delete**: the spawner emits `SessionEventEnded` when the worker
  child process exits; the daemon's listener removes the entry from
  the store so stale auth tokens do not linger in memory.

### Repository URL resolution (REN-1464 / v0.5.2)

`SessionDetail.repository` is **resolved from the `daemon.yaml`
project allowlist** by `pollItemToSessionDetail` (in `poll.go`). The
runner uses this URL for `git clone`.

The platform's QueuedWork wire shape historically carries a
`projectName` slug (e.g. `"smoke-alpha"`) with no separate repository
URL — slugs are not clonable. When the poll item arrives the daemon
runs the same matcher as `WorkerSpawner.findProjectLocked`
(REN-1448): by `id`, by `repository`, or by URL-suffix. The matching
entry's `repository` field is substituted into
`SessionDetail.repository`, and the canonical `id` is mirrored back
into `SessionDetail.projectName` so downstream code that reads
`RENSEI_PROJECT_ID` sees a stable value.

If no allowlist entry matches, the daemon falls back to whatever the
platform sent (preserving prior behaviour) and emits a Warn log
`no allowlist match for projectName, falling back to as-given repo
string` so the misconfiguration is visible. Downstream
`WorkerSpawner.AcceptWork` will then reject the spec with
`repository ... is not in the project allowlist`, but the explicit
log makes the resolution-time failure observable separately from
the spawn-time rejection.

## HTTP control API

Localhost-only (binds 127.0.0.1). Endpoints:

| Method + Path                          | Purpose |
|----------------------------------------|---------|
| `GET    /api/daemon/status`            | Daemon lifecycle state, version, uptime, sessions |
| `GET    /api/daemon/stats`             | Capacity envelope, worker stats, allowed projects |
| `POST   /api/daemon/pause`             | Stop accepting new work |
| `POST   /api/daemon/resume`            | Resume accepting work |
| `POST   /api/daemon/stop`              | Graceful stop |
| `POST   /api/daemon/drain`             | Drain in-flight work |
| `POST   /api/daemon/update`            | Trigger manual update check |
| `POST   /api/daemon/capacity`          | Update a config key (e.g. `capacity.poolMaxDiskGb`) |
| `GET    /api/daemon/pool/stats`        | Workarea pool snapshot |
| `POST   /api/daemon/pool/evict`        | Evict pool members |
| `GET    /api/daemon/sessions`          | List active session handles |
| `POST   /api/daemon/sessions`          | Accept a session (test entrypoint) |
| `GET    /api/daemon/sessions/<id>`     | **F.2.8** — per-session detail for the spawned worker |
| `GET    /api/daemon/heartbeat`         | Most-recent heartbeat payload |
| `GET    /api/daemon/doctor`            | Aggregated health snapshot |
| `GET    /healthz`                      | Liveness probe |

## Operator runbook — debugging a stuck session

When a session appears wedged in the dashboard:

1. **Daemon log** — `af daemon logs --follow` (default
   `~/.rensei/daemon.log`). Look for the `worker spawner` lines
   showing `pid=…` and the matching `[child stdout sessionID=<id>]`
   (INFO) and `[child stderr sessionID=<id>]` (WARN) records from
   the spawned `af agent run` worker. Spawn output is wired to slog
   by default as of v0.5.1 (REN-1463) — earlier daemons drained
   child stdio silently.
2. **Session detail** —
   `curl http://127.0.0.1:7734/api/daemon/sessions/<id>` to confirm
   the detail is recorded. A 404 here means the daemon never
   accepted the work (look for poll errors in the daemon log) or
   the session has already terminated and been cleaned up.
3. **`af agent run` log** — the worker child writes its own slog
   output to stderr. The daemon's spawner captures both streams
   under `[child stdout|stderr sessionID=<id>]`; the same lines
   appear inline in `af daemon logs` and in the platform's
   session-activity stream.
4. **Provider logs** — when the runner reaches step 8 (`spawn
   provider`), the per-provider subprocess is the next layer
   (`claude` JSONL on stdout, `codex` JSON-RPC over stdio). The
   provider package's README explains how to capture those streams
   (`PROVIDER_DEBUG=1` for claude, `CODEX_LOG_LEVEL=debug` for
   codex).
5. **Platform-side state** —
   `curl http://app.rensei.ai/api/sessions/<id>` (with bearer auth)
   to confirm the platform sees the session in the expected state.
   A divergence between the daemon's view (still active) and the
   platform's view (already terminal) usually indicates a missed
   `result.Post` — re-run `af daemon stats` to see whether the
   poller has retried.
6. **Worktree state** — `~/.rensei/worktrees/<sessionId>/.agent/`
   contains the per-session `state.json` snapshot and the
   `events.jsonl` audit log. Look here when the agent emitted no
   visible output but the session is marked failed.

## Failure modes the daemon classifies (high-level)

| Symptom | Where it surfaces |
|---|---|
| WorkerCommand falls through to `/bin/sh` stub | `worker spawner` warn line in daemon log |
| Daemon HTTP unreachable from worker child | `af agent run preflight` error, exit code 2 |
| Session detail expired between fetch attempts | `af agent run preflight` error, exit code 2 |
| Provider probe failed at runner startup | `af agent run` Warn log "claude provider unavailable" — falls through to stub if the session asked for stub; otherwise the runner's `Resolve` fails with `FailureProviderResolve` |
| Worker child exited with non-zero | `SessionEventEnded` with `ExitErr` non-nil; daemon emits the failure to its log |

See `runner/README.md` for the runner-level failure-mode table that
the daemon receives via `result.Post` payloads.

## Tests

```bash
# Unit + smoke
go test -race ./daemon/...

# F.2.8 wire-path integration test (requires git on PATH)
go test -tags=f28_integration ./afcli/...
```
