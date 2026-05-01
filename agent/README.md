# `agent/` — multi-provider agent-runner contract

This package defines the provider-agnostic types every agent provider
implementation and the runner orchestrator depend on. It is the
foundation block of the v0.5.0 multi-provider agent-runner subsystem.

## What lives here

| File | Purpose |
|---|---|
| `doc.go` | Package overview + the 9-capability matrix |
| `types.go` | Enums, struct types: `ProviderName`, `Capability`, `Capabilities`, `SandboxLevel`, `EffortLevel`, `MCPServerConfig`, `PermissionConfig`, `CodeIntelEnforcement`, `Spec`, `CostData`, `Result`, `BackstopReport`, `QualityReport`, plus the `IsSupported` capability gate |
| `provider.go` | `Provider` interface (5 methods: `Name`, `Capabilities`, `Spawn`, `Resume`, `Shutdown`) |
| `handle.go` | `Handle` interface (4 methods: `SessionID`, `Events`, `Inject`, `Stop`) |
| `event.go` | `Event` sealed-interface + 8 variant structs + `MarshalEvent` / `UnmarshalEvent` polymorphic JSON helpers |
| `errors.go` | Sentinel errors: `ErrUnsupported`, `ErrNoProvider`, `ErrSessionNotFound`, `ErrSpawnFailed`, `ErrProviderUnavailable` |

## What does NOT live here

- Provider implementations (`provider/claude`, `provider/codex`,
  `provider/stub`) — they import this package.
- The runner orchestration loop — `runner/` imports this package.
- HTTP clients, process spawning, filesystem I/O, MCP-stdio bridging,
  worktree management, prompt rendering, result posting.

This package is **pure types + interfaces**. No business logic, no I/O,
no dependencies beyond the Go standard library and `log/slog`.

## Relationship to the legacy TypeScript surface

This package is the verbatim Go translation of the legacy TS contract at:

```
../agentfactory/packages/core/src/providers/types.ts
```

JSON tags on every struct use **camelCase** to match the TS wire format
verbatim. Readers of `QueuedWork.resolvedProfile` JSON across the fleet
(daemon, platform-side workflow nodes, rensei-tui) depend on this.

## Public package boundary

This package is exported at the top level of the module
(`github.com/RenseiAI/agentfactory-tui/agent`). Downstream consumers
including `rensei-tui` import it directly without depending on the rest
of `agentfactory-tui`. F.1.1 §1 ratifies this boundary. Do not move or
rename without an ADR.

## The 9 capabilities

A provider declares a `Capabilities` struct describing which optional
behaviors it supports. The runner gates on these flags via
`IsSupported(caps, capability)` rather than try-catching unsupported
operations. The 9 named capabilities (each maps 1:1 to a flag on
`Capabilities`):

| `Capability` constant | `Capabilities` field | Meaning |
|---|---|---|
| `CapMessageInjection` | `SupportsMessageInjection` | `Handle.Inject` can deliver mid-session user messages |
| `CapSessionResume` | `SupportsSessionResume` | `Provider.Resume` can continue a prior session |
| `CapToolPlugins` | `SupportsToolPlugins` | Provider accepts MCP stdio tool plugins (af_linear, af_code) |
| `CapBaseInstructions` | `NeedsBaseInstructions` | Provider consumes `Spec.BaseInstructions` |
| `CapPermissionConfig` | `NeedsPermissionConfig` | Provider consumes `Spec.PermissionConfig` |
| `CapCodeIntelEnforcement` | `SupportsCodeIntelligenceEnforcement` | Provider supports the Grep/Glob → af_code redirect |
| `CapSubagentEvents` | `EmitsSubagentEvents` | Provider emits Anthropic-style subagent (Task) events |
| `CapReasoningEffort` | `SupportsReasoningEffort` | Provider honors `Spec.Effort` (low/medium/high/xhigh) |
| `CapToolPermissionFormatClaude` | `ToolPermissionFormat == "claude"` | Provider uses Claude's `Bash(prefix:glob)` tool-permission grammar |

### v0.5.0 provider matrix (per F.1.1 §3 and locked coordinator decisions)

| Provider | Inject | Resume | ToolPlugins | BaseInstr | PermConfig | CodeIntel | Subagent | Effort | PermFmt |
|---|---|---|---|---|---|---|---|---|---|
| `claude` | **false** | true | true | false | false | **false** | true | true | claude |
| `codex` | false | true | true | true | true | false | false | true | codex |
| `stub` | configurable per-test | configurable per-test |

The bolded `claude` flags reflect the v0.5.0 CLI shell-out approach:
the Claude CLI does not expose mid-session injection in JSON-stream
mode, and the runtime `canUseTool` callback is not yet exposed by the
CLI either. Both flip to `true` in F.5 if/when a wrapper sidecar lands.

## Sealed Event variants

The `Event` interface is sealed: it requires both `Kind() EventKind`
and an unexported `isAgentEvent()` marker. External packages cannot
satisfy the interface, which keeps the discriminated union closed to
the 8 variants defined in `event.go`:

```
InitEvent → SystemEvent* → (AssistantTextEvent | ToolUseEvent | ToolResultEvent | ToolProgressEvent)* → (ResultEvent | ErrorEvent)
```

To decode an `Event` polymorphically from JSON (e.g. the events.jsonl
file the runner persists per session), use:

```go
ev, err := agent.UnmarshalEvent(jsonBytes)
```

`UnmarshalEvent` reads a `"kind"` discriminator field and dispatches to
the matching variant struct. Use `MarshalEvent` to encode in the same
shape; the wire format is `{"kind": "<EventKind>", ...variant fields...}`.

## Wire-format compatibility rule

If you change a wire shape (add/remove/rename a JSON field, change a
JSON tag, add an enum value), you must update the F.1.1 design doc in
the same change:

```
../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md
```

The doc is the contract source-of-truth — keeping it single-sourced
prevents drift between implementations.

## Testing

```bash
go test -race ./agent/...
```

Tests cover:

- JSON round-trip for `Spec`, `Capabilities`, `Result` (verifies
  camelCase tags + `omitempty` behavior).
- Round-trip for every `Event` variant + `MarshalEvent` /
  `UnmarshalEvent` polymorphic dispatch + error paths.
- `IsSupported` capability gate (table-driven over all 9 capabilities).
- Sentinel errors are distinct (no accidental aliasing).
- `Provider` and `Handle` interfaces are implementable from outside the
  package (compile-time check via a no-op type).
