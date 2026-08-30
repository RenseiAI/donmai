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
If `codex` is missing, `codex.New` returns `agent.ErrProviderUnavailable`
so the runner fails fast before doing any worktree work.

Process start and the JSON-RPC initialize handshake are deferred to the
first headless `Spawn`/`Resume`; a handshake failure there surfaces as
`agent.ErrSpawnFailed` wrapping `agent.ErrProviderUnavailable`. The
deferral is what lets the child environment be composed from the
session's own `Spec.Env` (see [Environment composition](#environment-composition))
rather than from whatever the constructing process happened to inherit.

## Environment composition

The app-server child's environment is built once, at start, by
`mergeEnv` → `runtime/env.ComposeChildEnv`, in this order (later wins):

1. the inherited parent environment, minus runner-only attach controls
   and the agent-auth blocklist;
2. `Options.Env` — the construction-time layer (e.g. `OPENAI_API_KEY`);
3. the spawning session's `Spec.Env` — the runner-owned per-session
   layer, which is the canonical owner of session routing values such as
   the platform API origin;
4. the Provider's own `CODEX_HOME`, which no caller may override.

Layer 3 is why the start is lazy: composing it at construction time is
impossible, and an eagerly-started app-server would pin every session it
serves to the ambient values of the process that built the registry.

Because layer 3 is applied exactly once, one Provider serves exactly one
session's environment. The layer applied at start is pinned, and a later
`Spawn`/`Resume` whose `Spec.Env` materially differs — any key added,
removed, or changed — is refused rather than silently handed the first
session's `DONMAI_SESSION_ID` and `DONMAI_API_URL`. The refusal names the
diverging keys and never quotes a value, because this is the layer the
runner puts `WORKER_AUTH_TOKEN` and `GH_TOKEN` in. An identical layer is
always accepted, so a session resuming its own thread is unaffected, and
the interactive PTY spawn mode runs its own process and is outside the
invariant entirely. `donmai agent run` builds one Provider per session,
so this is a fail-closed guard for embedders that pool Providers, not a
constraint on normal operation.

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
never loads or writes the operator's persistent Codex configuration. By
default it also carries no host credentials; API-key callers supply those
explicitly through `Options.Env`.

For a route that has already resolved `authMode=host-session`, the caller sets
`Options.HostSessionAuth`. Construction pins Codex to its file-backed auth
store but does not deliver a credential or start app-server. After the
selected headless harness passes preparation, `Spawn`/`Resume` narrowly
hard-links the host's
`$CODEX_HOME/auth.json` (or `~/.codex/auth.json`) into the private home. The
app-server is started and initialized only after that link exists. The
credential contents never enter Donmai memory or logs, in-place token refreshes
update the same inode as the normal CLI login, and the operator's `config.toml`
remains outside the boundary. The source must be a private regular file and
share a filesystem with the isolated home; otherwise the session fails before
a thread starts. Donmai's per-session `agent run` path enables this option only
for an authoritatively selected Codex + `host-session` profile. Keyed lanes,
unknown/non-Codex explicit harnesses, and the zero-context introspection
registry retain the credential-free default.

Before every `thread/start` or `thread/resume`, the Provider acquires a
lease for the exact `Spec.MCPServers` set. It writes the native Codex shape
to the `mcp_servers` key with `replace`, an explicit owned `filePath`, and
`reloadUserConfig: true`, then proves activation with `config/read` at the
session working directory. Because Codex reloads those servers asynchronously,
readback proves the effective configuration but not client readiness. The
Provider therefore polls paginated `mcpServerStatus/list` results until the
inventory gives every requested name `serverInfo` from a completed MCP
initialize handshake. Any name removed from the preceding Provider-managed set
must also leave the inventory. Codex-owned ambient entries that are outside the
isolated `mcp_servers` readback do not block activation. This does not require a
non-empty tool list, so resource-only servers remain valid. The first session
performs the config proof even for an empty set, so an undeclared server in
effective readback is rejected before a thread starts. Stdio and HTTP entries
are translated to Codex's native fields, including `http_headers`.

Concurrent sessions may share an identical MCP set. An incompatible set is
denied while a lease is live; sequential sessions rewrite and re-verify it.
The last release replaces the set with empty and waits for both empty readback
and every Provider-managed name to leave the active inventory. Failure to
apply, read back, initialize within the RPC timeout, or clear is a typed MCP
application denial, not a soft degradation. A failed clear removes the private
home, poisons the Provider against later sessions, and is returned from
`Handle.Stop` or emitted as `mcp_cleanup_failed` on terminal/crash paths.
Process exit and `Shutdown` also remove the owned home. `-32601 Method not
found` is therefore a hard pre-thread failure.

## Model selection

Both spawn modes honor `Spec.Model` when the platform resolved one for the
work order (`QueuedWork.ResolvedProfile.Model` on the wire), and leave codex
on its own default when it did not:

- **Headless app-server lane** — `spec_translation.go`'s `resolveModel`
  always sets `thread/start`/`turn/start`'s `"model"` param, falling back
  through `Spec.Env["CODEX_MODEL_TIER"]` / `Spec.Env["CODEX_MODEL"]` /
  `DefaultCodexModel` when `Spec.Model` is empty.
- **Interactive TUI lane** — `interactive.go`'s `buildInteractiveLaunchEnv`
  seeds a process-local `--config model="<id>"` override, the same
  mechanism used for every other session-scoped knob here (approval policy,
  sandbox mode, developer instructions, MCP servers). Unlike the headless
  lane it does NOT default: an empty `Spec.Model` emits no override at all,
  so an unselected interactive session runs under whatever `model` the
  TUI's own config.toml/CLI default resolves to — a platform-side "no
  selection" and a standalone `donmai agent run --interactive` invocation
  both stay on the codex CLI's own default rather than being pinned to
  `DefaultCodexModel`.

When `Spec.SessionName` is present, the interactive lane starts a bounded
per-session Unix-socket app-server, creates the thread, applies and reads back
`thread/name/set`, then attaches the TUI with
`codex resume --remote <socket> <name>`. The server stays alive through the
first turn so the already-named thread becomes the durable resume target; the
PTY lifecycle owns its cleanup. Unnamed interactive sessions retain the bare
TUI path.

Neither lane validates a model id before spawn — codex is the sole
authority. A rejected id surfaces as codex's own nonzero exit (or, for the
headless lane, a JSON-RPC error), never a silent fallback to a different
model.

## Startup trust (interactive spawn mode)

The codex TUI holds modal startup reviews before it reads a keystroke, and a
session launched into a freshly provisioned workspace meets two of them:
its directory review ("Do you trust the contents of this directory?") and its
hook review ("N hooks are new or changed"). Neither times out, so an
unattended session parks on them until its wall clock kills it.

`trust.go` removes both by seeding process-local `--config` overrides on the
interactive launch — never by writing to the operator's own codex home:

| Seed | Value | Why it may be seeded |
| --- | --- | --- |
| `projects."<workspace>".trust_level` | `trusted` | The runner provisioned that directory. Both the given path and its symlink-resolved form are seeded, because codex matches a project entry by exact path. |
| `features.hooks` | `false` | Hooks found in the workspace are repo content, not platform-provisioned, and trusting one grants command execution outside the sandbox. This takes codex's own third option — continue without trusting, hooks do not run — deterministically. Set `DONMAI_CODEX_HOOKS=inherit` to leave codex's hook handling alone (an attended terminal can then review them; an unattended one can block). |

Requested MCP servers need no *startup* seed: codex starts every server in
effective configuration with no approval step, and one that fails to start
(including a `401`) degrades to a warning line rather than a prompt. Their
per-tool-CALL approval is a separate matter — see the next section.

The `projects` override SHADOWS the ambient projects table for the child
process, so a session's trusted set is exactly the workspace the platform
provisioned — narrower than the operator's own configuration may grant, never
broader. A workspace that cannot be resolved to an absolute path fails the
spawn with an error naming the missing trust, because hanging on the review
is the worse outcome.

The **headless app-server lane does not receive project-trust, global approval,
or sandbox seeds**: command/file approvals ride the bridge in `approval.go`,
and trusting its working directory would make codex load that directory's
`.codex/config.toml` as a project configuration layer, admitting `mcp_servers`
the isolated `CODEX_HOME` boundary exists to exclude. MCP tool approval is the
exception because Codex does not expose it on that bridge. Each requested
server receives only the scoped `default_tools_approval_mode = "approve"` key
inside the Provider-owned config.

## Approvals (interactive and headless spawn modes)

Getting past the startup reviews is not enough: a *running* session raises
three further blocking reviews, and each one parks an unattended session the
same way. `approvals_seed.go` pre-answers all three with process-local
`--config` overrides on the same launch:

| Seed | Value | Closes |
| --- | --- | --- |
| `approval_policy` | `never` | "Would you like to run the following command?" before an ordinary command. |
| `sandbox_mode` | `danger-full-access` | The sandbox's own escalation review for a command that touches the network or writes outside the workspace. This class **survives** turning approvals off from inside the TUI, which is what makes it expensive to diagnose. |
| `mcp_servers."<name>".default_tools_approval_mode` | `approve` | "Allow the `<server>` MCP server to run tool `"<tool>"`?", raised once per distinct tool name. Seeded only for the servers this spawn requested. |

Measured against codex-cli 0.146.0, with a fresh `CODEX_HOME` and a PTY:

- A `touch` outside the workspace root under the trust seeds alone stops on
  *"Would you like to run the following command? … Reason: Allow creating the
  requested file outside the writable workspace root?"*. With the two command
  seeds it runs, and `codex doctor` reports `sandbox: filesystem unrestricted ·
  network enabled`.
- An MCP tool call under the trust seeds alone stops on *"Allow the probe MCP
  server to run tool "…"?"* (Allow / Allow for this session / Always allow).
  It runs with `default_tools_approval_mode = "approve"`, and it runs with
  `approval_policy = "never"`. Note `"auto"` is **not** the auto-approve
  variant — a session configured with it still stopped on the review.

Both mechanisms independently close the MCP class today; both are seeded
because codex exposes them as separate settings and only the per-server one is
scoped to the servers the platform requested.

The headless lane uses that per-server seed as well. Its isolated config
readback must preserve the key before activation succeeds; without it, Codex
0.147 initializes and advertises the native MCP tools but cancels every call as
though the absent user had declined the review.

`DONMAI_CODEX_APPROVALS=inherit` restores codex's own approval handling for an
attended terminal. An unrecognized value fails the spawn rather than guessing
which of "cannot hang" and "may hang" was meant — the same rule
`DONMAI_CODEX_HOOKS` follows. It does not alter the headless lane's scoped MCP
seed.

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
| `interactive.go`      | Interactive PTY spawn mode and native name attach |
| `interactive_name.go` | Per-session app-server name/readback lifecycle    |
| `trust.go`            | Startup trust seeding for the interactive mode    |
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
