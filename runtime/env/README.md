# `runtime/env/`

Compose the `KEY=VALUE` slice handed to `exec.Cmd.Env` for an agent provider subprocess.

## What it does

```go
c := env.NewComposer()
out := c.Compose(map[string]string{
    "PATH":              "/usr/bin",
    "ANTHROPIC_API_KEY": "operator-leak", // host shell var; will be filtered
}, agent.Spec{Env: map[string]string{
    "ANTHROPIC_API_KEY":      "runner-resolved", // override is allowed
    "AGENTFACTORY_SESSION_ID": "sess-123",
}})
// out: ["AGENTFACTORY_SESSION_ID=sess-123",
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

Source: `../../../agentfactory/packages/core/src/orchestrator/orchestrator.ts` (and `agent-spawner.ts`). The legacy TS keeps the same 4-element list verbatim across both files; `TestAgentEnvBlocklistMatchesLegacyTS` asserts the Go port stays in sync.

The blocklist applies **only to the `base` map** passed to `Compose`. `Spec.Env` is runner-set and intentionally trusted — that is how the daemon resolves `ANTHROPIC_API_KEY` from its credential store and injects it for the session.

## Heuristic helpers

- `IsBlocked(key string) bool` — exact match against the effective blocklist.
- `LooksSensitive(key string) bool` — soft heuristic (substring match against `TOKEN`/`SECRET`/`PASSWORD`/`PASSWD`/`PRIVATE_KEY`/`API_KEY`). Not a security boundary; useful for "operator probably did not mean to set this" log warnings.

## Tests

`composer_test.go` covers: precedence, blocklist filtering, spec-overrides-blocked, custom blocklist, empty-non-nil bypasses filtering, deterministic key order, nil receiver fallback, sentinel test pinning blocklist contents.
