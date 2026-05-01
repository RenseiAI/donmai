# `provider/claude` — Anthropic Claude Code CLI provider

This package implements the v0.5.0 production `agent.Provider` for the
agent-runner subsystem. It shells out to the `claude` CLI (the
Anthropic Claude Code binary) and translates its stream-json output
into the unified `agent.Event` taxonomy.

The implementation follows F.1.1 §3.1 of the multi-provider runner
contract design doc and the locked coordinator decisions captured in
F.1.1 §10.

## What this package does

| File | Purpose |
|---|---|
| `doc.go` | Package overview + capability table + failure-mode notes |
| `claude.go` | `Provider` struct, `New(Options)` constructor, fail-fast probe |
| `cli_args.go` | `Spec → []string` translator covering all 19 Spec fields |
| `mcp.go` | Per-session MCP `--mcp-config` JSON tmpfile writer + cleanup |
| `jsonl.go` | JSONL → `agent.Event` mapper (port of legacy `mapSDKMessage`) |
| `handle.go` | `Handle` struct: subprocess lifecycle, events channel, Stop |
| `process_unix.go` | Unix process-group helpers for clean SIGTERM/SIGKILL propagation |
| `process_other.go` | Stub for non-unix builds (kept compileable) |
| `testdata/` | Canned JSONL fixtures for the parser tests |

## How it shells out to the CLI

The provider invokes the `claude` binary (probed via `exec.LookPath`
at `New()` time) with these required flags every session:

```
claude -p \
   --output-format stream-json \
   --verbose \
   --dangerously-skip-permissions \
   --add-dir <Spec.Cwd> \
   [optional: --model, --max-turns, --effort,
              --append-system-prompt, --permission-mode bypassPermissions,
              --allowedTools, --disallowedTools,
              --mcp-config /tmp/agentfactory-claude-mcp-*.json --strict-mcp-config,
              --resume <session-id>]
```

The prompt is delivered via **stdin** so it never appears in the
process listing. The CLI reads to EOF, then produces a JSONL stream
on stdout — each line is one event the provider parses with
`jsonl.go::mapLine` and forwards on the events channel.

The subprocess runs in its own process group (`Setpgid=true`) so
`Stop()` and ctx cancellation can SIGTERM/SIGKILL the whole group at
once — necessary because `claude` may fork shell helpers and stdio
MCP children that inherit stdout.

## v0.5.0 capability matrix

| Capability | v0.5.0 | Notes |
|---|---|---|
| `SupportsMessageInjection` | **true** | Between-turn injection via `--resume`. `Handle.Inject(text)` spawns a fresh `claude --resume <session-id> -p <text>` subprocess and forwards its JSONL stream onto the parent events channel. Same semantic level as the legacy TS Agent SDK. |
| `SupportsSessionResume` | **false** | Runner doesn't yet exercise resume. CLI flag (`--resume`) is wired in `cli_args.go` for v0.5.+ flip. |
| `SupportsToolPlugins` | true | MCP stdio servers via `--mcp-config`. |
| `NeedsBaseInstructions` | false | Codex-only contract field. |
| `NeedsPermissionConfig` | false | Codex-only contract field. |
| `SupportsCodeIntelligenceEnforcement` | **false** | The `canUseTool` callback is not yet exposed by the CLI. Re-enable in F.5 when a wrapper sidecar lands. |
| `EmitsSubagentEvents` | true | Mirrors legacy provider. |
| `SupportsReasoningEffort` | true | `--effort` flag. |
| `ToolPermissionFormat` | `claude` | `Bash(prefix:glob)` grammar. |

The remaining bolded flags reflect the v0.5.0 CLI shell-out approach.
They flip to `true` in v0.5.+ once option C lands per **REN-1451**.

### Between-turn injection (`Inject` semantics)

`v0.5.0` ships `SupportsMessageInjection=true` via `--resume`-based
injection (REN-1455 / F.2.3-cap-flip). Each `Inject(ctx, text)` call:

1. Reads the session id captured from the parent's `system.init`
   event. Pre-init calls error with `ErrSessionNotReady`.
2. Acquires the per-Handle inject mutex. Concurrent injects error
   immediately with `ErrInjectInFlight` — callers must consume events
   from `Events()` until they observe a terminal `ResultEvent` for
   the prior inject before calling again.
3. Spawns `claude --resume <session-id> --output-format stream-json
   --verbose --dangerously-skip-permissions -p <text>` and streams
   its JSONL output onto the same `Events()` channel.
4. Returns when the resume subprocess exits cleanly, or with
   `ctx.Err` on cancellation.

The events channel close ownership shifted with this flip: the parent
reader no longer closes on EOF (Inject would have nowhere to send).
`Stop()` is the sole closer; the spawn ctx's cancellation triggers an
internal Stop via a watcher goroutine.

### Path to the option C upgrade (REN-1451)

The `--resume` mechanism is between-turn — same level as the legacy
TS Agent SDK in v0.5.0. Option C replaces the subprocess shell-out
with the Anthropic Go SDK + a Go-native agent loop, enabling true
mid-turn injection and lower per-turn overhead. When option C lands:

- Replace `Inject`'s subprocess spawn with the Go SDK's stream-input
  channel.
- Flip `SupportsSessionResume` to `true` and wire `Provider.Resume`.
- Capability semantics stay the same; observable behavior improves.

## MCP server attachment

Per coordinator decision #10 in F.1.1 §10, MCP stdio servers are
attached via a **per-session JSON tmpfile** passed to `--mcp-config`.

`mcp.go::writeMCPConfig` serializes `Spec.MCPServers` to:

```json
{
  "mcpServers": {
    "af_linear": {
      "type": "stdio",
      "command": "node",
      "args": ["dist/stdio.js", "--plugin", "linear"],
      "env": {"LINEAR_API_KEY": "..."}
    }
  }
}
```

The file lives under `os.TempDir()` with the prefix
`agentfactory-claude-mcp-`. `Handle.Stop()` removes the tmpfile as
part of cleanup (idempotent). `--strict-mcp-config` is passed
alongside so the CLI ignores any other host-wide MCP configurations.

## JSONL → Event mapping

Port of the legacy `mapSDKMessage` from
`../agentfactory/packages/core/src/providers/claude-provider.ts`.

| CLI line `type` | Variant emitted |
|---|---|
| `system.subtype="init"` | `InitEvent{SessionID}` |
| `system.subtype="*"` | `SystemEvent{Subtype, Message}` |
| `assistant` (text block) | `AssistantTextEvent{Text}` |
| `assistant` (tool_use block) | `ToolUseEvent{ToolName, ToolUseID, Input}` |
| `user` (tool_result block) | `ToolResultEvent{ToolUseID, Content, IsError}` |
| `user` (no tool_result) | `SystemEvent{Subtype: "user_message"}` |
| `tool_progress` | `ToolProgressEvent{ToolName, ElapsedSeconds}` |
| `result.subtype="success"` | `ResultEvent{Success: true, Message, Cost}` |
| `result` (any other subtype) | `ResultEvent{Success: false, Errors, ErrorSubtype, Cost}` |
| `auth_status` (no error) | `SystemEvent{Subtype: "auth_status", Message}` |
| `auth_status` (error) | `ErrorEvent{Message, Code: "auth_status"}` |
| `stream_event` | dropped (high-frequency partial frames) |
| `rate_limit_event` | `SystemEvent{Subtype: "rate_limit"}` |
| unknown / missing `type` | `SystemEvent{Subtype: "unknown"}` or `ErrorEvent` |

Each emitted event carries the original raw line in its `Raw` field
as `json.RawMessage` so the runner can persist provider-native
events to `<worktree>/.agent/events.jsonl` per F.1.1 §4 step 9.

If the stdout stream EOFs without a terminal `ResultEvent`, the
provider synthesizes a final `ErrorEvent{Code: "spawn_no_result"}`
(with the captured stderr tail when available) so the runner observes
the failure rather than blocking.

## Failure-mode protocol

Per F.1.1 §5:

- **Spawn failure** (binary missing, tmpfile error, exec start fail) →
  return error wrapping `agent.ErrSpawnFailed`. Caller short-circuits
  before any worktree work.
- **Subprocess failure** (non-zero exit, EOF without terminal) →
  synthetic `ErrorEvent` followed by close of the events channel. No
  panics.
- **`ctx.Done()`** → SIGTERM the process group, wait up to 5 seconds,
  SIGKILL, then close the events channel.
- **API retry** (3-attempt exponential backoff `2^(n-1)*1s`) does
  **not** apply at this layer — the CLI shell-out makes no HTTP calls
  directly. The runner-level result poster is responsible for retries.

## Probe / fail-fast at construction

`New(Options{})` calls `exec.LookPath("claude")` and returns an error
wrapping `agent.ErrProviderUnavailable` when the binary is missing.
The runner is expected to short-circuit on this error so no worktree
provisioning happens before a usable provider is confirmed.

Tests inject a fake binary path via `Options.Binary` +
`Options.LookPath` so the probe runs against a deterministic stub.

## Testing

```bash
go test -race ./provider/claude/...                    # unit + handle (fake CLI)
go test -race -tags=integration ./provider/claude/...  # against the real claude binary
```

Test inventory (per cardinal rule 10):

- `claude_test.go` — `New()` happy/binary-missing, capability matrix,
  Resume/Inject unsupported, Shutdown no-op.
- `cli_args_test.go` — `buildArgs` covers all 19 Spec fields,
  Linear MCP block list gating on `Autonomous`, deduplication, env
  composition determinism.
- `mcp_test.go` — tmpfile write happy path, idempotent cleanup,
  validation, no aliasing of caller's slices/maps.
- `jsonl_test.go` — table-driven over every CLI event type using
  fixtures in `testdata/`. Includes malformed-JSON / unknown-type /
  user-without-tool-result edge cases.
- `handle_test.go` — fake `/bin/sh` CLI scripts drive end-to-end
  Handle behavior: happy path, no-terminal synthetic error, Stop
  idempotency, ctx cancel propagation, MCP tmpfile cleanup,
  bounded-buffer behavior.
- `integration_test.go` — gated on `-tags=integration` AND `claude`
  binary on PATH; runs a no-op `--max-turns 1` smoke against the
  real CLI. Skipped in CI by default to avoid token spend.

## Legacy TS reference

| Concern | Legacy path |
|---|---|
| Provider class + capabilities + spawn loop | `../agentfactory/packages/core/src/providers/claude-provider.ts` |
| `mapSDKMessage` translation | same file, `mapSDKMessage` / `mapAssistantMessage` / `mapUserMessage` / `mapResultMessage` |
| `canUseTool` factory | same file, `createAutonomousCanUseTool` (not ported in v0.5.0; CLI has no callback surface) |
| Linear MCP disallow list | same file, `disallowedTools` array under `agentQuery.options` |
| MCP server config shape | same file, `Object.fromEntries(config.mcpStdioServers.map(...))` block |

## Wire-format compatibility

Any change to the JSONL→Event mapping or the MCP config tmpfile shape
must be reflected in
`../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md`.
That doc is the contract source-of-truth — keeping it single-sourced
prevents drift between provider implementations.
