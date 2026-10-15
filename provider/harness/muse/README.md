# Muse harness (`provider/harness/muse`)

MSP client over `muse serve` (JSON-RPC 2.0 over stdio JSONL): one server
child per Handle, many turns per session.

## Wire source

`types.go` is generated from the host binary's own export — never
hand-copied:

```sh
muse schema generate-json-schema --out /tmp/muse-schema-gen
# reconcile $defs against msp.schema.json; pin the export fingerprint in
# schema_fingerprint.go
```

Handshake requires envelope schema version 1 and the granted
`sessionMcp` capability. The pinned export fingerprint is advisory (a
mismatch warns); the version is enforced.

## Spawn

`muse serve [--trust-workspace] [--disable-sandbox]`; the read-only lane
is `--disable-shell --disable-write` with approval mode `denyUnmatched`,
and no other Spec or provider option widens it. Env: `META_API_KEY` is stripped
unless the Spec sets it; `MUSE_NO_AUTO_UPDATE=1` is always set. The cwd
is the realpath of the workspace (the host rejects non-canonical roots).

## Confinement

The serve child runs under the executor OS confinement (seatbelt on
macOS, mount-namespace on Linux) exactly when requested: the host
requires it (`DONMAI_MUSE_CONFINEMENT=required` or
`Options.RequireConfinement`) or the session declares a repository
authority. A request that cannot be met (no backend, no working
directory, failed self-test) refuses the spawn — never runs unconfined.
The confinement wraps the whole serve argv after the lane flags, so it
composes with the host's own sandbox: on the read-only lane both apply;
with `--disable-sandbox` the OS profile is the only boundary. Effort
rides `turn/start.reasoningEffort` (`minimal`..`max`; unset, `default`
and `none` all leave the tier off so the host default applies).

Handshake: `initialize{clientInfo:{name: "donmai", version},
capabilities:{requestedCapabilities:["sessionMcp"]}}`, then the
`initialized` notification. Nothing is sent before the handshake
completes.

Sessions: `session/start{sessionId + commandId (client UUIDv7),
workspaceRoot, modelId, providerId:"meta", approvalMode, config}`.
Resume: `session/resume{sessionId, excludeItems:true, config}`; the
`sessionNotFound`/`notFound` error kinds map to `ErrSessionNotFound`.

Turns: `turn/start{sessionId, commandId, input, displayText,
workspaceRoots, ifBusy:"queue", reasoningEffort}` (the returned turnId
equals the commandId); `turn/interrupt{turnId}`;
`turn/steer{expectedTurnId, input}` (message injection). There is no
system-prompt channel, so instructions ride a leading text part.

## Events

Routed by `params.sessionId`. Item events are revision-aware
(replace-iff-higher; stale deltas drop; `item/completed` is always
authoritative). `view/gap` is fatal for the stream: the handle
re-anchors via `session/resume` with the last cursor, then continues.
`turn/completed{terminal: completed|cancelled|failed}` maps to the
terminal ResultEvent; cost comes from
`session/tokenUsage.cumulative.cost.usd` (absent on unpriced hosts —
then no cost is reported), overridden by `turn/completed.usage` token
counters when present.

Approvals arrive as `approval/requested` notifications plus
`approval/request` server-requests. The decision is driven once per
approvalId from the notification and sent as
`approval/decide{approvalId, requirementId, choiceId}` (decline maps to
the denial/abort choice); the server-request is answered `{}`. The
`userInput/request` server-request is answered `{}` (autonomous mode
has no user to prompt). MCP servers ride inline
(`config.mcpServers[<name>]`, streamableHttp + stdio).

## Live proof

Verified live against Muse Code 1.4.6 on 2026-10-12: a read-only turn
(`--disable-shell --disable-write`, `denyUnmatched`, effort minimal)
returned `terminal: completed` in ~7s. `model/list` reports
`muse-wonder-1.0` (default), `muse-wonder-full-1.0`, `muse-spark-1.3`,
`muse-spark-1.2`. `session/resume` on an unknown id fails
`sessionNotFound`; resume with a cursor re-anchors (`history.mode:
none`).

## Out of scope

Plan mode, fork and rollback.
