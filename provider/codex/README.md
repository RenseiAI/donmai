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

`Spec.MCPServers` is pushed to the app-server via JSON-RPC
`config/batchWrite` (`mcpServers` keyPath, `replace` merge strategy)
exactly once per Provider lifetime, on the first `Spawn`. The
app-server then runs each MCP server as its own subprocess; the codex
side discovers and routes tools without further help from this
provider.

If the codex version returns `-32601 Method not found` for
`config/batchWrite`, the call is treated as a soft failure — the
provider still works, sessions just run without tool plugins.

## Failure modes (F.1.1 §5)

- **Transient JSON-RPC error** → `RequestWithRetry` does 3 attempts
  with 1s/2s/4s backoff. Permanent errors (parse / invalid request /
  method not found) return immediately.
- **App-server crash** → the JSON-RPC client's read loop sees EOF /
  pipe-closed, fires `onClose`, and the Provider marks every live
  Handle terminal with `agent.ErrorEvent{Code: "app_server_crashed"}`.
- **`ctx.Done()` on a Handle** → forwarder sends `turn/interrupt` +
  `thread/unsubscribe`, emits `agent.ErrorEvent{Code: "context_cancelled"}`,
  closes events.
- **Server-request with no handler** → JSON-RPC `-32601 Method not
  found` reply so codex doesn't hang on us, plus a
  `agent.SystemEvent{Subtype: "unhandled_server_request"}` for
  observability.

## File layout

| File                  | Responsibility                                    |
| --------------------- | ------------------------------------------------- |
| `doc.go`              | Package overview                                  |
| `codex.go`            | `Provider` lifecycle (New / Spawn / Resume / Shutdown) |
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
go test -race ./provider/codex/

# Integration tests (requires codex + OPENAI_API_KEY)
go test -tags codex_integration -timeout 120s ./provider/codex/
```

## What was intentionally dropped vs legacy TS

- **`codex exec` fallback** — F.0.1 §6 item 2 calls this a band-aid;
  v0.5.0 fails fast instead.
- **`turn/steer` mid-turn injection** — `Handle.Inject` returns
  `agent.ErrUnsupported`. F.5 may revisit if the runner needs it.
- **Reasoning-delta coalescing** — the legacy TS buffers reasoning
  text streams to avoid char-by-char log spam. Out of scope for the
  provider; the runner can coalesce its own log output.
- **PID-file orphan-killing** — the legacy TS writes
  `~/.agentfactory/codex-app-server.pid` to detect stranded processes
  on restart. Wave 6 daemon owns subprocess lifecycle (REN-1408+); the
  provider does not duplicate.

## See also

- `../agentfactory/packages/core/src/providers/codex-app-server-provider.ts`
  (read-only legacy reference, 1928 LOC)
- `../agentfactory/packages/core/src/providers/codex-approval-bridge.ts`
  (read-only legacy reference, 124 LOC)
- F.1.1 design doc §3.2
- F.0.1 codex deep-dive
