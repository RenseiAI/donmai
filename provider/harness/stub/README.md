# `provider/harness/stub` — the deterministic fake harness

This package has **two spawn modes**, and the difference between them is
the whole point.

| `Spec.Interactive` | Mode | What runs |
|---|---|---|
| `nil` | **headless** | a scripted, in-process `agent.Event` sequence — no child process at all |
| set | **interactive** | a real PTY child running the deterministic fake **agent** |

The headless mode is the original one: it fabricates events so the runner's
event handling can be unit-tested without a model. It is the right tool for
that and the wrong tool for anything else, because nothing about it is a real
session — no PTY, no process group, no signal delivery, no shim adoption, no
exit status.

The interactive mode is the other half. It spawns a real child through the
shared `provider/harness/ptycli` driver, exactly as `claude`, `codex`, `pi`
and `shell` are spawned: same `ptyhost.Spawn` plumbing, same session-shim
adoption, same `SIGTERM` → grace → `SIGKILL` stop escalation, same coarse
`Init`/`Result` event mapping. What it does not have is a model — its
behaviour is a **scenario script**. That lets an integration environment
drive every session-lifecycle branch (clean exit, failing exit, hang,
stop-ignoring, chatty output, agent-to-agent traffic) with no provider
credential, no network, and no dependence on a model's mood.

The child is [`stubagent`](stubagent/), invoked as the hidden `stub-agent`
subcommand of **this very binary** — not a separate artifact, so an
integration environment cannot end up running a fake agent from one build
against a daemon from another.

## Interactive mode

### How the harness is selected

`stub` is a canonical harness id, so nothing new is needed to select it: a
`ResolvedProfile.Harness` of `"stub"` is recognized by
`runner/harness_selection.go` and resolved against the live manifest registry
like any other harness. `afcli`'s `agent run` registers the stub provider
unconditionally (it is the one provider with no probe that can fail), so the
harness is available on every host.

The manifest declares `SupportsInteractivePTY: true`. `Spawn` reads that flag
off the live manifest rather than branching on a hardcoded `true`, so flipping
the declaration back off returns the provider to headless-only behaviour
instead of leaving a PTY spawn the manifest no longer claims.

Prompt delivery in this mode is `stub_pty_seed`: the terminal is the child's
only input channel, so the user prompt, initial context and amendments are
seeded into the PTY, and the channels it cannot honour natively are declared
`unsupported` (a caller that needs one through anyway authorizes the
documented downgrade to the user turn) rather than mapped onto a sink that
would drop them while the receipt said "delivered".

### The scenario

The child reads its scenario from the environment:

| Variable | Meaning |
|---|---|
| `DONMAI_STUB_SCENARIO` | inline JSON scenario (wins) |
| `DONMAI_STUB_SCENARIO_FILE` | path to a JSON scenario |
| `DONMAI_STUB_EXIT_CODE` | override the final exit code |
| `DONMAI_STUB_HANG_FOR` | override the post-script idle (`"30s"`, or `"forever"`) |
| `DONMAI_STUB_STOP_MODE` | override the stop policy (`respond` / `ignore` / `slow`) |
| `DONMAI_STUB_OUTPUT_RATE` | throttle stdout, in bytes per second |
| `DONMAI_STUB_SEED` | override the scenario seed |
| `DONMAI_STUB_AGENT_BIN` | run a different donmai build as the child |

With none of them set the child runs the default scenario: announce, idle
briefly, exit 0. The override variables apply on top of whichever source
supplied the base, so one scenario file covers a family of fault cases without
being rewritten, and the result is revalidated — an override is not a second
chance to be invalid.

Unlike the scenario variables, `DONMAI_STUB_AGENT_BIN` is read **once, at
`New()`** — the same place codex reads `$CODEX_BIN` and pi reads `$PI_BIN` — and
never from a `Spec`. A provider handed out by a shared registry must execute the
same program for every caller. Because the override names a *donmai binary*, the
`stub-agent` subcommand is appended to it; without that the child would run a
bare `donmai`, print help and exit 0, which every layer above reads as a clean
session that ran no scenario at all. A test that must run something else uses
the `WithStubAgentCommand` option, which supplies its own argv. The value is
handed to exec as-is, so a bare name is resolved by `LookPath` and a path is
not — and that lookup uses the **spawning process's** `$PATH`, not the child's:
`ptyhost.Spawn` calls `exec.Command`, which resolves the name at construction
and only afterwards assigns the composed `cmd.Env`, so a `$PATH` placed in
`Spec.Env` has no bearing on which binary is found.

Callers holding a `Spec` can pass the scenario through the typed
`ProviderConfig` instead of composing `Env` themselves:
`stub.scenario` (a JSON string or a decoded object) and `stub.scenarioFile`.
Both are validated at spawn — the file is read and parsed in the parent, not
forwarded on trust — because a child that exits on a malformed or missing
scenario is indistinguishable, at the session layer, from one that was asked to
exit. The check applies to whichever form the child will actually read: an
inline scenario wins, and when one is present the file is never opened. An
explicit `Spec.Env` entry always wins over the `ProviderConfig` form.

That read is bounded, because it happens inside `Spawn`. The path must be a
**regular file** — `os.ReadFile` on a FIFO blocks until a writer appears, which
for a pipe nobody opens is forever, and an unbounded read there is an unbounded
`Spawn` — and it must be at most `MaxScenarioFileBytes` (1 MiB), enforced both
by the stat and by the read itself, since a file can grow between the two.
Every refusal names the path it refused.

```json
{
  "version": 1,
  "name": "c1-clean-session",
  "seed": 7,
  "steps": [
    {"print": "stub agent up"},
    {"awaitInput": {"timeout": "20s", "echo": true}},
    {"a2a": {"text": "work started", "contextId": "ctx-7"}},
    {"idle": "2s"},
    {"exit": 0}
  ],
  "stop": {"mode": "respond", "exitCode": 0},
  "exitCode": 0,
  "hangFor": "",
  "outputRate": 0
}
```

A step sets **exactly one** action; setting none or several is refused at
parse time, because a step that silently picks one of two requested actions is
a scenario that does not mean what it says. Unknown fields are refused for the
same reason.

| Step | Effect |
|---|---|
| `print` | write one line to the terminal |
| `idle` | wait (a Go duration string — a JSON number is refused, since it would read as nanoseconds) |
| `a2a` | emit one agent-to-agent line (see below) |
| `awaitInput` | block until a line arrives on stdin; `echo` prints it back. A wait that expires fails the scenario |
| `exit` | exit now with this code, skipping every later step |
| `hang` | a boolean; `true` idles until stopped, `false` is a deliberate no-op step |

The throttle paces the stream **between** lines: it never fragments one, and
never holds the writer's lock while it waits. Pacing inside the lock is the
natural way to write it and it is wrong — a 100-byte line at 20 bytes/second
would hold the lock for the whole five-second stop grace window, starving the
notice below past the deadline and making a refused stop look like one that was
never delivered.

### Cooperative stop

`stop.mode` is what separates a harness that shuts down when asked from one
that has to be killed:

- `respond` — acknowledge and exit with `stop.exitCode` (the default);
- `slow` — acknowledge, wait `stop.delay`, then exit;
- `ignore` — acknowledge and **keep running**, so the parent's grace window
  expires and escalates to `SIGKILL`.

`ignore` is the RED control for any assertion that a cooperative stop
succeeded. Every mode, `ignore` included, prints its notice when the signal is
observed, so a transcript can tell "refused the stop" apart from "never
received the stop" — without that line, a stop assertion also passes against a
stop path that does nothing at all.

### Agent-to-agent lines

An `a2a` step writes one line of the form

```
DONMAI-A2A {"messageId":"stub-msg-…","role":"ROLE_AGENT","parts":[…]}
```

The body is a real `a2a.Message`, so a consumer asserting on it is asserting
against the protocol type rather than a shape invented here. Use
`stubagent.ParseA2ALine` to read one back rather than restating the format;
it tolerates the `\r\n` a PTY produces.

The message id is **derived** from (scenario name, seed, step index), not
drawn from a random source or a clock. That is what makes two runs of one
scenario byte-identical — the property the whole harness exists for.

## Why the headless mode exists

- **F.4 smoke harness** drives the full runner end-to-end without
  invoking the `claude` or `codex` binaries. The harness asserts on
  the byte-exact event sequence emitted by `BehaviorSucceedWithPR`
  per F.1.1 §3.3.
- **Runner unit tests (F.2.5)** use this provider to drive every
  failure-mode branch deterministically — clone failure, mid-stream
  error, silent fail, hang/timeout — without spinning up real agents.
- **Demo and development** — an embedding binary can select the stub
  harness to exercise its own surfaces (dashboards, session views) with
  no provider credential and no network.

## Behavior matrix

The behavior name is read from (in order):

1. `Spec.ProviderConfig["stub.behavior"]` (typed v0.5.0 knob)
2. `Spec.Env["DONMAI_STUB_MODE"]` (legacy F.1.1 §3.3 knob)
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

## Stop semantics (headless mode)

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
go test -race ./provider/harness/stub/...          # both modes, unit level
go test -race -run TestStubAgent ./afcli/          # the PTY end-to-end suite
```

The end-to-end suite lives in `afcli` because that is where the hidden
`stub-agent` command is registered: it re-executes the test binary as the
child, so a real process on a real PTY runs the production command with no
build step. It covers a clean exit, a scripted failure reaching the terminal
`ResultEvent`, the PTY seed arriving at the child, the marked line parsing
back off the terminal, and both halves of the cooperative-stop pair. Those
last two wait for the child's own readiness line before signalling —
signalling a process that has not yet installed its handler proves only the
default disposition, which kills it in every mode and would make both cases
pass for the wrong reason.

The headless test suite covers:

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

And for the interactive mode:

- `interactive_test.go` — the manifest declaration, child-binary resolution
  order, the `ProviderConfig` → environment projection (including that it
  copies rather than mutates the caller's map), the refusal of a malformed
  scenario, and that a headless Spawn is unchanged by the mode switch.
- `stubagent/scenario_test.go` — parse, validate, load precedence, the
  environment overrides and their revalidation.
- `stubagent/run_test.go` — every step kind, the three stop modes, output
  throttling, byte-identical repeat runs, and the defer-ordering regression.
- `stubagent/a2aline_test.go` — encode/parse round trip, determinism as a
  function of identity, and the rejections.
