# `runtime/env/`

Compose the `KEY=VALUE` slice handed to `exec.Cmd.Env` for an agent provider subprocess.

## What it does

```go
c := env.NewComposer()
out := c.Compose(map[string]string{
    "PATH":              "/usr/bin",
    "ANTHROPIC_API_KEY": "operator-leak", // host shell var; will be filtered
}, agent.Spec{Env: map[string]string{
    "ANTHROPIC_API_KEY": "runner-resolved", // override is allowed
    "DONMAI_SESSION_ID": "sess-123",
}})
// out: ["DONMAI_SESSION_ID=sess-123",
//       "ANTHROPIC_API_KEY=runner-resolved",
//       "PATH=/usr/bin"]
```

## `AGENT_ENV_BLOCKLIST` (verbatim port)

```
ANTHROPIC_API_KEY
ANTHROPIC_AUTH_TOKEN
ANTHROPIC_BASE_URL
OPENCLAW_GATEWAY_TOKEN
```

Plus donmai-native entries with no legacy counterpart:

```
AMP_API_KEY                       # amp harness personal access token (provider/harness/amp)
DONMAI_GATEWAY_UPSTREAM_API_KEY   # worker-local gateway's upstream credential
DONMAI_GATEWAY_UPSTREAM_BASE_URL  # worker-local gateway's upstream route
```

The gateway entries are load-bearing for the translating-gateway host: the
harness child must receive only the gateway's per-session loopback bearer,
never the upstream provider key or route the worker dials with
(`afcli/gateway_bind.go`).

Source: `../../../donmai-libraries/packages/core/src/orchestrator/orchestrator.ts` (and `agent-spawner.ts`). The legacy TS keeps the same 4-element list verbatim across both files; `TestAgentEnvBlocklistMatchesLegacyTS` asserts the Go port stays in sync.

The blocklist applies **only to the `base` map** passed to `Compose`. `Spec.Env` is runner-set and intentionally trusted — that is how the daemon resolves `ANTHROPIC_API_KEY` from its credential store and injects it for the session.

## `DONMAI_INJECTED_ENV_KEYS` — declared injections

An embedding daemon that merges per-session credentials into the **worker process
environment** (rather than into `Spec.Env`) writes into the inherited layer, where
the blocklist strips them: the worker's own provider probe sees the key and the
harness child never does.

`DONMAI_INJECTED_ENV_KEYS` is the supervisor's declaration of which inherited
names are its own. It is a comma-separated list of variable **NAMES** — never
values — set on the worker process:

```
DONMAI_INJECTED_ENV_KEYS=GEMINI_API_KEY,OPENAI_API_KEY
```

- A declared name crosses the blocklist on the inherited layer
  (`ComposeChildEnv`, `Compose`'s `base`, and the PTY parent env).
- Everything else is unchanged: undeclared blocklisted names are still stripped,
  and the explicit layers still bypass the blocklist outright.
- The declaration re-admits `AGENT_ENV_BLOCKLIST` names **only**. `IsRunnerOnly`
  names (`ATTACH_*`, the `DONMAI_SESSION_SHIM*` launch contract) stay stripped.
- The variable is itself runner-only, so it never reaches a child: a harness
  cannot read the injected set, and cannot re-declare one of its own.
- Parsing is defensive — whitespace trimmed, empty elements ignored, exact-name
  match, no globs.

Helpers: `ParseInjectedEnvKeys(value)`, `InjectedEnvKeysFrom(entries []string)`,
`InjectedEnvKeysFromMap(map[string]string)`, and `InjectedEnvKeySet.Allows(key)`.

## Heuristic helpers

- `IsBlocked(key string) bool` — exact match against the effective blocklist.
- `LooksSensitive(key string) bool` — soft heuristic (substring match against `TOKEN`/`SECRET`/`PASSWORD`/`PASSWD`/`PRIVATE_KEY`/`API_KEY`). Not a security boundary; useful for "operator probably did not mean to set this" log warnings.

## Tests

`composer_test.go` covers: precedence, blocklist filtering, spec-overrides-blocked, custom blocklist, empty-non-nil bypasses filtering, deterministic key order, nil receiver fallback, sentinel test pinning blocklist contents.
