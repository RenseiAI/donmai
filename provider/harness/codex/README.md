# `provider/codex`

Implements the `agent.Provider` contract against `codex app-server` for
the v0.5.0 Go agent runner. Per F.1.1 §3.2 + F.0.1 codex deep-dive.

## Architecture

One `codex.Provider` instance owns exactly one `codex app-server`
subprocess. Sessions are JSON-RPC `thread/start` calls that multiplex
over the same stdio pipe; each `codex.Handle` subscribes to
notifications matching its `threadId`.

```
codex.Provider
  └── child process: `codex app-server` (long-lived JSON-RPC over stdio)
        ├── thread_1 (Handle A) — independent agent.Spec
        ├── thread_2 (Handle B)
        └── thread_N
```

This mirrors the legacy TS
`packages/core/src/providers/codex-app-server-provider.ts` →
`AppServerProcessManager` + `AppServerAgentHandle` pair.

## Why no exec fallback

Per F.0.1 §6 (item 2) and the F.2.4 dispatch brief: v0.5.0 ships
**app-server only**. The legacy TS `codex exec` fallback was a band-aid
for stale codex binaries; Wave 6 requires a known-good codex on PATH.
If `codex` is missing or the JSON-RPC initialize handshake fails,
`codex.New` returns `agent.ErrProviderUnavailable` so the runner fails
fast before doing any worktree work.

## Capability matrix (F.1.1 §3.2 lock)

| Capability                              | v0.5.0 |
| --------------------------------------- | ------ |
| `SupportsMessageInjection`              | false  |
| `SupportsSessionResume`                 | true   |
| `SupportsToolPlugins`                   | true   |
| `NeedsBaseInstructions`                 | true   |
| `NeedsPermissionConfig`                 | true   |
| `SupportsCodeIntelligenceEnforcement`   | false  |
| `EmitsSubagentEvents`                   | false  |
| `SupportsReasoningEffort`               | true   |
| `ToolPermissionFormat`                  | codex  |

`SupportsMessageInjection` is `false` because `Handle.Inject` is hard-
wired to `agent.ErrUnsupported`. The legacy TS provider does support
mid-turn steering via `turn/steer`, but the v0.5.0 Go port keeps the
surface minimal — steering flows through `Provider.Resume` + a fresh
`Spec`.

## Approval bridge

The codex app-server fires JSON-RPC server-requests (`id` + `method`)
for every tool execution when the session's `approvalPolicy` is
`on-request`. The bridge in `approval.go` translates each request into
an `accept` / `decline` / `acceptForSession` reply against:

1. **Built-in safety deny patterns** (always enforced; cannot be
   overridden) — `rm -rf /`, `git worktree remove/prune`,
   `git reset --hard`, `git push --force` (without `--force-with-lease`),
   `sudo`, `curl … | bash`, recursive chmod/chown on absolute paths.
2. **`Spec.PermissionConfig.DisallowPatterns`** — user-supplied regex
   denies, evaluated in order.
3. **`Spec.PermissionConfig.AllowPatterns`** — when present, ONLY
   matching commands are accepted; everything else is declined.
4. **Default decision** — `allow` (default) → `acceptForSession`;
   `deny` / `prompt` → `decline` (autonomous mode cannot prompt).

Every approval emits a synthetic `agent.ToolUseEvent` +
`agent.ToolResultEvent` so the runner sees the call flow even when it
auto-approves. Declined approvals additionally emit a
`agent.SystemEvent{Subtype: "approval_denied"}` for observability.

The bridge ships in v0.5.0 (per F.1.1 open-question #5: ship the
bridge, not default-allow — autonomous fleets need real safety rules).

## MCP servers

Each headless Provider creates a private `CODEX_HOME` (directory mode
`0700`) with its own `config.toml` (mode `0600`). The owned home overrides
both the inherited environment and `Options.Env`; the initialize response
must confirm that exact home before the Provider is usable. The provider
does not read, link, copy, or write the operator's persistent Codex config
or auth files. If a session needs process-scoped authentication, its caller
must supply that explicitly through `Options.Env`.

Before every `thread/start` or `thread/resume`, the Provider acquires a
lease for the exact `Spec.MCPServers` set. It writes the native Codex shape
to the `mcp_servers` key with `replace`, an explicit owned `filePath`, and
`reloadUserConfig: true`, then proves activation with `config/read` at the
session working directory. The first session performs this write/read even
for an empty set, so any undeclared server visible in effective readback is
rejected before a thread starts rather than silently entering the session.
Stdio and HTTP entries are translated to Codex's native fields, including
`http_headers`.

Concurrent sessions may share an identical MCP set. An incompatible set is
denied while a lease is live; sequential sessions rewrite and re-verify it.
The last release replaces the set with empty and verifies that readback.
Failure to apply, read back, or clear is a typed MCP application denial, not
a soft degradation. A failed clear removes the private home, poisons the
Provider against later sessions, and is returned from `Handle.Stop` or
emitted as `mcp_cleanup_failed` on terminal/crash paths. Process exit and
`Shutdown` also remove the owned home. `-32601 Method not found` is therefore
a hard pre-thread failure.

## Failure modes (F.1.1 §5)

- **Transient JSON-RPC error** → `RequestWithRetry` does 3 attempts
  with 1s/2s/4s backoff. Permanent errors (parse / invalid request /
  method not found) return immediately.
- **App-server crash** → the JSON-RPC client's read loop sees EOF /
  pipe-closed, fires `onClose`, and the Provider marks every live
  Handle terminal with `agent.ErrorEvent{Code: "app_server_crashed"}`.
  If destruction of the isolated config fails, the terminal event is
  instead `agent.ErrorEvent{Code: "mcp_cleanup_failed"}`, and a later
  `Shutdown` returns the persistent cleanup error.
- **`ctx.Done()` on a Handle** → forwarder sends `turn/interrupt` +
  `thread/unsubscribe`, emits `agent.ErrorEvent{Code: "context_cancelled"}`,
  closes events.
- **Server-request with no handler** → JSON-RPC `-32601 Method not
  found` reply so codex doesn't hang on us, plus a
  `agent.SystemEvent{Subtype: "unhandled_server_request"}` for
  observability.

## Event-channel close protocol

The events channel on `Handle` has multiple potential closers — the
forwarder goroutine's defer (terminal event reached), `Stop` (caller-
initiated teardown), and `failNow` (Provider's onClientClose hook fires
when the shared app-server stream dies). Closes can race in either
direction with sends from `emit`, the synthetic `app_server_crashed`
ErrorEvent, and the forwarder's `context_cancelled` ErrorEvent.

The close protocol enforces:

1. `eventsClosed atomic.Bool` is the single source of truth for "events
   has been closed".
2. `closeEvents()` takes `eventsMu.Lock()`, flips `eventsClosed`, then
   `close(h.events)`. Idempotent under the flag check.
3. `emit()` takes `eventsMu.RLock()`, returns early when `eventsClosed`
   is set, otherwise selects on send vs `h.closed` so a slow consumer
   does not pin shutdown.
4. `signalClosed()` closes `h.closed` exactly once via a dedicated
   `closedOnce` (separated from the prior shared `closeOnce` so each
   path runs its own RPC-side cleanup independently of who wins the
   close race).

Both `Stop` and `failNow` are safe to call concurrently with each
other and with the forwarder; whichever runs first owns the user-
visible close, the others are silent no-ops via the idempotent helpers.

Additionally, `Client.Request` re-checks `pending.ch` before returning
on its `<-ctx.Done` / `<-timeoutCh` / `<-c.doneCh` cases. Without that
re-check, a response delivered in the same instant as a client-stop
broadcast was lost ~50% of the time to Go's random-select tie-break,
surfacing as a spurious `client stopped` Spawn failure.

## File layout

| File                  | Responsibility                                    |
| --------------------- | ------------------------------------------------- |
| `doc.go`              | Package overview                                  |
| `codex.go`            | `Provider` lifecycle (New / Spawn / Resume / Shutdown) |
| `config_boundary.go`  | Isolated `CODEX_HOME` ownership and cleanup            |
| `jsonrpc.go`          | Bidirectional JSON-RPC 2.0 client over stdio      |
| `handle.go`           | Per-session `Handle` + forwarder goroutine        |
| `approval.go`         | Approval bridge (Spec.PermissionConfig → decision) |
| `spec_translation.go` | `agent.Spec` → JSON-RPC param mapping             |
| `event_mapping.go`    | JSON-RPC notification → `agent.Event` mapping     |
| `signal_unix.go`      | `SIGTERM` lookup (unix)                           |
| `signal_windows.go`   | `os.Interrupt` fallback (windows; out of scope)   |

## Testing

- `*_test.go` — unit tests using a fake stdio JSON-RPC server.
- `integration_test.go` (build-tagged `codex_integration`) — smoke
  test against a real `codex app-server` if installed.

```bash
# Unit tests (default)
go test -race ./provider/harness/codex/

# Integration tests (requires codex + OPENAI_API_KEY)
go test -tags codex_integration -timeout 120s ./provider/harness/codex/
```

## What was intentionally dropped vs legacy TS

- **`codex exec` fallback** — F.0.1 §6 item 2 calls this a band-aid;
  v0.5.0 fails fast instead.
- **`turn/steer` mid-turn injection** — `Handle.Inject` returns
  `agent.ErrUnsupported`. F.5 may revisit if the runner needs it.
- **Streaming text/reasoning deltas** — the partial-message
  notifications (`item/agentMessage/delta`, `item/reasoning/textDelta`,
  `item/reasoning/summaryTextDelta`) are dropped outright, mirroring the
  Claude provider's `stream_event` drop. The full text arrives on the
  matching `item/completed`, so no content is lost. (The legacy TS
  instead buffered reasoning streams to avoid char-by-char log spam; an
  earlier Go port forwarded each delta as its own event, which surfaced
  as one-token-per-line "thought" activities on the platform topology
  view — see `runtime/activity`. Dropping the deltas removes that spam.
  Live incremental rendering is served by the separate
  `runtime/tokendelta` transport, not the activity-event stream.)
- **PID-file orphan-killing** — the legacy TS writes
  `~/.donmai/codex-app-server.pid` to detect stranded processes
  on restart. Wave 6 daemon owns subprocess lifecycle; the
  provider does not duplicate.

## See also

- `../donmai-libraries/packages/core/src/providers/codex-app-server-provider.ts`
  (read-only legacy reference, 1928 LOC)
- `../donmai-libraries/packages/core/src/providers/codex-approval-bridge.ts`
  (read-only legacy reference, 124 LOC)
- F.1.1 design doc §3.2
- F.0.1 codex deep-dive
