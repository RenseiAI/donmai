# `provider/stub` — deterministic test stub for the agent runner

This package is a fully in-process implementation of `agent.Provider`
that emits a pre-scripted sequence of `agent.Event` values. It has no
external runtime dependency — no CLI, no network, no filesystem — and
is the foundation of the F.4 smoke harness in
`runs/2026-05-01-wave-6-fleet-iteration/`.

## Why this package exists

- **F.4 smoke harness** drives the full runner end-to-end without
  invoking the `claude` or `codex` binaries. The harness asserts on
  the byte-exact event sequence emitted by `BehaviorSucceedWithPR`
  per F.1.1 §3.3.
- **Runner unit tests (F.2.5)** use this provider to drive every
  failure-mode branch deterministically — clone failure, mid-stream
  error, silent fail, hang/timeout — without spinning up real agents.
- **`rensei-tui` dev mode** can wire the stub provider as a
  `--provider stub` flag to demo the dashboard without API keys.

## Behavior matrix

The behavior name is read from (in order):

1. `Spec.ProviderConfig["stub.behavior"]` (typed v0.5.0 knob)
2. `Spec.Env["RENSEI_STUB_MODE"]` (legacy F.1.1 §3.3 knob)
3. The `WithDefaultBehavior` provider option, default `succeed-with-pr`.

| Behavior | Source | Event sequence | Use case |
|---|---|---|---|
| `succeed-with-pr` | F.1.1 §3.3 | Init → System → AssistantText → ToolUse → ToolResult → AssistantText(`WORK_RESULT:passed`) → Result(success) | Smoke happy-path |
| `fail-on-clone` | F.1.1 §3.3 | Init → ErrorEvent(`clone_failed`) → close | Spawn-failure path |
| `hang-then-timeout` | F.1.1 §3.3 | Init → System → wait on ctx | MaxDuration timeout |
| `silent-fail` | F.1.1 §3.3 | Init → System → close (no Result) | Synthetic-error path |
| `slow-tool` | F.1.1 §3.3 | Init → System → ToolUse → N×ToolProgress → ToolResult → Result | Progress UI |
| `cost-overrun` | F.1.1 §3.3 | Init → System → AssistantText → Result(cost=$999.99) | Cost-cap warnings |
| `mid-stream-error` | F.2.2 add-on | Init → System → AssistantText → ErrorEvent → close | Mid-session crash |
| `inject-test` | F.2.2 add-on | Init → System → block on Inject → echo → Result | `Handle.Inject` path |

The `slow-tool` tick count is configurable via
`Spec.ProviderConfig["stub.progressTicks"]` (default 3).

## Capability matrix

All optional capability flags default to `true` so the runner exercises
every gating branch when wired against the stub. Tests can override
via `New(WithCapabilities(...))`. The exposed `HumanLabel` is
`"Test Stub"`.

To exercise the unsupported-Inject path without flipping the whole
capability matrix, set
`Spec.ProviderConfig["stub.injectUnsupported"] = true` — `Handle.Inject`
will return `agent.ErrUnsupported` while every other behavior remains
on.

## Stop semantics

`Handle.Stop` signals the scripting goroutine to bail out, emit a
terminal `ResultEvent{Success: false, ErrorSubtype: "stopped"}`, and
close the events channel. Stop is idempotent and safe to call after
the channel has closed naturally.

## Adding a new behavior

1. Add the constant to `behaviors.go` and to `IsKnown`.
2. Add a `script<Name>` function in `handle.go` that emits the
   sequence and returns when done.
3. Wire the constant into the switch in `(*handle).run`.
4. Add a row to the `Test_Behaviors` table in `unit_test.go`.
5. Update the matrix above.
6. If the new behavior becomes part of the locked smoke contract,
   mirror the change to F.1.1 §3.3.

## Testing

```bash
go test -race ./provider/stub/...
```

The test suite covers:

- `compile_test.go` — compile-time `agent.Provider` / `agent.Handle`
  conformance plus a non-nil check on `New`.
- `unit_test.go` — table-driven event-kind sequence assertion for
  every behavior, plus capability/option/Resume/Spawn-context tests.
- `roundtrip_test.go` — full Spawn → drain assertion on the canonical
  `succeed-with-pr` event-by-event shape (tool ID pairing, WORK_RESULT
  marker, cost data, PR URL).
- `inject_test.go` — `inject-test` flow plus the
  `stub.injectUnsupported` gate.
- `stop_test.go` — Stop emits the terminal stopped Result, idempotency,
  Stop-after-natural-close.
