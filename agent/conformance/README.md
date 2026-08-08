# `agent/conformance` — the harness certification suite

A harness adapter is only as trustworthy as what has been **proven** about it.
A manifest is a set of claims. This package turns the claims a harness can be
held to into executable checks, and awards a capability **tier** only when every
check for that tier passed.

Nothing here reads a tier off a manifest. Declaring `noticeDelivery` earns
nothing; delivering a message into a running session earns the live-notice tier.

## Two layers

**Pure event-sequence checks.** `CheckSingleInit`, `CheckTerminalContract`,
`CheckCompleteAssistantTexts` and the `CheckEventContract` composite take a
fully drained `[]agent.Event`. No process, no I/O. Any provider test package can
drain a `Handle` and assert against them.

```go
events := drain(t, handle)
if err := conformance.CheckEventContract(events); err != nil {
    t.Fatal(err)
}
```

**The runnable suite.** `Run` drives a `Subject` — your adapter plus the small
amount of harness-specific glue only you can write — through every check and
returns a `Report`.

## Running it against your adapter

The suite imports `agent` and the standard library, reaches no network service,
and needs no credentials beyond whatever your own harness binary already
requires.

```go
func TestMyHarnessConformance(t *testing.T) {
    provider, err := myharness.New()
    if err != nil {
        // NOT t.Skipf. `go test` prints `ok` for a package whose tests all
        // skipped, so a certification test that skips when the binary is
        // missing reports the same thing as one that certified — which is the
        // skip-reads-as-success failure this whole package exists to prevent.
        t.Fatalf("harness binary unavailable, so NOTHING was certified: %v", err)
    }

    report, err := conformance.Run(t.Context(), conformance.Subject{
        Provider: provider,
        BaseSpec: agent.Spec{Cwd: t.TempDir(), Model: "…"},
        EchoPrompt: func(nonce string) string {
            return "Reply with exactly this token: " + nonce +
                "\nThen wait for further instructions."
        },
        Adaptation: &conformance.AdaptationFixture{
            Spec:         mySpecWithASecretInIt,
            SecretValues: []string{"…the secret's literal value…"},
        },
    })
    if err != nil {
        t.Fatal(err) // a malformed Subject, not a non-conformant harness
    }
    t.Log("\n" + report.Text())
    if err := report.Err(); err != nil {
        t.Fatal(err)
    }
    t.Logf("tiers earned: %v", report.EarnedTiers())
}
```

`Run`'s error return is reserved for a malformed `Subject`. A harness that fails
checks yields a nil error and a `Report` whose `Err()` lists the failures — so
"you configured the suite wrong" stays distinct from "your harness is not
conformant".

### Do not make the certification test skippable

A lane where the harness binary is never installed and the certification test
skips is a lane that prints `ok` and certifies nothing. If you need a fast unit
lane that does not install binaries, put the certification test behind a build
tag or an explicit opt-in (`CERTIFY=1`) and make the certifying lane assert it
**ran** — count the skips, or check for the tier line in the log. Never let the
absence of a run render as a pass. `go test` gives a skipped test and a passing
test the same one-word summary; the only defence is a lane that fails when the
test did not execute.

### `EchoPrompt` is the one thing you must write

The suite proves a message **arrived** rather than trusting that `Inject`
returned nil, and it can only do that if your agent reproduces a token it was
given. Two obligations:

1. The agent must echo the nonce verbatim somewhere in its output.
2. For the live-notice probe, the prompt must leave the session open long
   enough to accept a follow-up — typically "echo, then wait for further
   instructions". A session that already terminated cannot receive a notice,
   and the suite cannot tell that apart from an adapter that dropped one.

If your manifest declares `pty-notice`, `EchoPrompt` renders a **command** the
session runs (`"echo " + nonce`), your `BaseSpec` must set `Spec.Interactive`,
and the proof is the nonce appearing on the terminal *twice* — once as the line
being echoed back at you, once as the session's own output. See the rail table
below.

## Tiers

| Tier | Earned by |
|---|---|
| `event-contract` | one `InitEvent` first, complete (never per-token) assistant texts, one terminal event last, the channel closes, `Stop` after close is idempotent |
| `live-notice` | a message pushed into a running session over the rail that carries the **declared** channel actually arrives |
| `resume` | a resumed session re-announces and re-terminates conformantly |
| `adaptation-receipt` | the manifest declares an adaptation profile for every mode it claims, the pre-spawn authority compiles and validates ready, and no declared secret survives into it |

A tier is earned only when **every** check in it passed.

### The live-notice rails

Driven-ness is asked **per channel**, because
`Capabilities().SupportsMessageInjection` is a fact about one rail —
`Handle.Inject` — and is evidence about a declared channel only when that rail
is what carries it.

| Declared `noticeDelivery` | Rail | How the suite certifies it |
|---|---|---|
| `mcp-rpc`, `http-session`, `acp`, `rpc-steer`, `in-box-loop` | `Handle.Inject` | injects a nonce at the `InitEvent` and requires it back out of the event stream |
| `pty-notice` | `InteractiveCapable` → `InteractiveNotifier.TryWriteNotice` | writes the notice at the live terminal (the seam the runner's interactive supervisor uses) and requires the session's own output back on screen |
| `hook` | none in this build | the injection point is the harness calling out to a host-side responder, which this build does not have — reported **unproven**, never passed on the strength of `Handle.Inject` |
| `none`, `resume-inject` | not live | honest declarations with no live channel to certify — reported not-applicable, never failed |

A mechanism that is declarable but missing from the rail table is a **failure**,
not a default: the suite says it cannot judge the channel rather than guessing.

## Honest output

Three rules are mechanized, because a skip that reads as a pass is the failure
mode this program keeps rediscovering:

1. **A not-applicable must carry a reason.** A result claiming
   `StatusNotApplicable` with no reason is rewritten into `StatusFail`. The
   suite cannot emit a silent skip even by mistake. `Subject.NotApplicable`
   entries are held to the same rule — an empty reason is a `Run` error.
2. **A not-applicable never earns its tier.** "We could not test it" and "it
   works" can never render the same. Declaring a check inapplicable buys an
   honest report, not a pass.
3. **Every report names what it did not check.** `Report.Unverified` lists the
   checklist rows and manifest claims this suite has no check for, so a green
   report cannot be mistaken for full certification. The first entry is this
   suite's own central blind spot: on the `Handle.Inject` rail it proves a nonce
   came back out of the event stream, **not** that it came back over the
   declared channel — from in-process the two are indistinguishable.

`CheckResult.Decider` records whether a skip was derived by the suite from the
manifest or requested by the harness author — a reader can tell the two apart.

## Checklist coverage

Grounded in the harness-addition checklist
(`donmai-architecture/ADR-2026-07-24-harness-addition-v2-checklist.md`).

**Implemented**

- **Row 6 (event-contract conformance)** — in full: one `InitEvent`, complete
  assistant texts, one terminal event, channel close. Plus `Stop` idempotence
  and resume continuity from the `Handle` contract.
- **Row 9/10 (notice-delivery declaration)** — the manifest must answer the
  notice-delivery question, and a declared live channel must be driven by this
  build *on the rail that carries that channel* and demonstrably deliver over
  it. Channel attribution on the `Handle.Inject` rail is out of reach from
  in-process and is named in `Report.Unverified`.
- **Row 10 (applied receipt fixtures)** — the part observable from the adapter
  and its host-compiled authority: profiles for every claimed session mode, an
  authority that compiles and validates ready, and secret-ref-only
  serialization.

**Deliberately not implemented**

- **Row 11 (child conformance).** Proving native-child identity/event/cancel/
  terminal mapping needs a live child spawn against an independently admitted
  execution cell and its own `SessionRef`. A check that cannot observe a child
  would be exactly the silent skip rule 1 exists to prevent, so none is
  registered and the gap is named in `Report.Unverified`.
- **Rows 1–5, 7, 8, and the materialization half of 10.** Binary pins,
  pin-bump protocol, policy enforcement, fail-closed boundaries, endpoint
  lockout, the per-harness smoke set, router tier entry, and what an adapter
  writes to config/argv/environment are all outside what an in-process suite
  can see. Each is named in `Report.Unverified` rather than faked.
- **Three properties of row 10** — idempotent re-application, refusal of a
  drifted Spec, and named downgrades — are enforced by the shared compiler in
  the `agent` package, which no adapter can override and every adapter
  inherits. A check here could not be made to fail by any subject, and a check
  nothing can fail is not a check. They are covered where they live.

## Adding a check

A check earns its place when a **non-conformant subject can make it fail**.
`fakeharness_test.go` holds a configurable fake adapter; every check in the
registry has a fake broken in exactly that one way, and a table row naming the
check that must catch it. Add both, or the check is decoration.
