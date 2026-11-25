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
DONMAI_GATEWAY_UPSTREAM_API_KEY   # worker-local gateway's upstream credential
DONMAI_GATEWAY_UPSTREAM_BASE_URL  # worker-local gateway's upstream route
DONMAI_PI_KEY                     # resolved cell key for the pi harness's injected provider; blocked from the parent env
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
- The declaration re-admits **shell-leak** `AGENT_ENV_BLOCKLIST` names only.
  Three classes are refused regardless:
  - `IsRunnerOnly` names (`ATTACH_*`, the `DONMAI_SESSION_SHIM*` launch
    contract, and the two declaration variables themselves);
  - `AgentEnvIsolationInvariants` — `DONMAI_GATEWAY_UPSTREAM_API_KEY` and
    `DONMAI_GATEWAY_UPSTREAM_BASE_URL`, whose blocking is the gateway cell's
    isolation invariant rather than a shell-leak heuristic;
  - whatever the gateway named for **this session** through
    `DONMAI_GATEWAY_UPSTREAM_ENV_KEYS`. The gateway falls back to
    `OPENAI_API_KEY` as its upstream credential, and that name is ordinarily
    declarable — so the refusal is published per session by
    `DeclareGatewayUpstreamEnvKeys`, not inferred from the name.
- **Provenance.** The declaration is trusted from a supervising parent process
  only. `daemon.composeEnv` strips runner-owned names from every caller-supplied
  map (`SessionSpec.Env` is copied verbatim from an orchestrator work item), and
  the dotenv loader skips them, so a work item or a repository `.env` cannot
  author one. The only writer left is the `OnPreSpawn` hook.
- The variable is itself runner-only, so it never reaches a child: a harness
  cannot read the injected set, and cannot re-declare one of its own.
- Parsing is defensive — whitespace trimmed, empty elements ignored, exact-name
  match, no globs.

Helpers: `ParseInjectedEnvKeys(value)`, `InjectedEnvKeysFrom(entries []string)`,
`InjectedEnvKeysFromMap(map[string]string)`, and `InjectedEnvKeySet.Allows(key)`.

## `DONMAI_PROVIDER_HTTP_HEADERS` — provider request headers

A session env var, not blocklisted, that declares extra HTTP headers a harness
sends on every request to its model provider (app attribution for a model
gateway, say). The value is newline-separated `Name: Value` lines, the format
the Claude Code CLI reads from `ANTHROPIC_CUSTOM_HEADERS`:

```
DONMAI_PROVIDER_HTTP_HEADERS=http-referer: https://example.com
x-title: Example App
```

`agent.ParseProviderHTTPHeaders` validates it: lines are trimmed, blank lines
skipped, each splits on its first `:`. Names must be unique RFC 7230 tokens;
values printable ASCII. At most 16 headers. Names that would override auth or
transport are refused (`authorization`, `proxy-authorization`, `cookie`,
`host`, `content-length`, `content-type`, `x-api-key`, `anthropic-api-key`,
`api-key`, `transfer-encoding`, `connection`, `te`, `upgrade`, `x-goog-api*`).
A malformed value refuses the session with `*agent.ProviderHTTPHeadersError`;
headers are never silently dropped. Absent or blank changes nothing.

| Harness | Where the headers go |
|---|---|
| claude | merged into `ANTHROPIC_CUSTOM_HEADERS`: existing lines kept, names not already present appended |
| codex | `http_headers` on the gateway `model_providers` block (sessions with an endpoint base URL) |
| pi | `headers` on the injected provider, and a header-only override on a natively pinned built-in provider; values escaped so pi's `$NAME` and `!command` syntax stays literal. The values ride the owner-only session credential file, never the exec environment |
| opencode | `options.headers` on the injected provider; values holding `{env:` or `{file:` are refused |

On claude the declared values reach the CLI child's exec environment through
`ANTHROPIC_CUSTOM_HEADERS`, because that child receives `Spec.Env` there, so a
same-user process listing can read them: header values are not secret-safe on
claude.

## Heuristic helpers

- `IsBlocked(key string) bool` — exact match against the effective blocklist.
- `LooksSensitive(key string) bool` — soft heuristic (substring match against `TOKEN`/`SECRET`/`PASSWORD`/`PASSWD`/`PRIVATE_KEY`/`API_KEY`). Not a security boundary; useful for "operator probably did not mean to set this" log warnings.

## Tests

`composer_test.go` covers: precedence, blocklist filtering, spec-overrides-blocked, custom blocklist, empty-non-nil bypasses filtering, deterministic key order, nil receiver fallback, sentinel test pinning blocklist contents.
