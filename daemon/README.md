# `daemon/` — long-running rensei-daemon runtime

> **Status:** Wave 6 / Phase F.2.8. Public package; the
> `af host …` CLI surface is in `afcli/host.go` (`af daemon …` is a hidden
> deprecated alias, removed in v0.58.0).
> **Architecture:** `donmai-architecture/004-sandbox-capability-matrix.md`
> §Local daemon mode + `011-local-daemon-fleet.md`.

The daemon is a single-machine, multi-project supervisor that:

1. Registers itself with the platform (`/api/workers/register`) and
   exchanges a one-time `rsp_live_*` token for a scoped runtime JWT.
2. Sends a periodic heartbeat (`/api/workers/<id>/heartbeat`) and polls
   for queued work (`/api/workers/<id>/poll`).
3. Accepts inbound `SessionSpec` payloads and spawns a worker child
   process per accepted session.
4. Exposes a localhost-only HTTP control API on `127.0.0.1:7734` for
   the `af` CLI and for the spawned worker children themselves.
5. Optionally self-updates by drain → fetch → verify → swap → restart.

## Spawn flow (F.2.8)

```
        ┌────────────────────┐
        │ platform.poll()    │  GET /api/workers/<id>/poll
        └──────────┬─────────┘
                   │ work[] item
                   ▼
        ┌────────────────────┐
        │ Daemon.AcceptWork  │
        │   WithDetail()     │
        └──────────┬─────────┘
                   │ stores SessionDetail
                   ▼
        ┌────────────────────┐
        │ WorkerSpawner.spawn│  exec.CommandContext(<af>, "agent", "run")
        │                    │  env: DONMAI_SESSION_ID=<id>,
        │                    │       DONMAI_REPOSITORY=<repo>, …
        └──────────┬─────────┘
                   │
                   ▼
        ┌────────────────────┐
        │ af agent run       │  GET 127.0.0.1:7734/api/daemon/sessions/<id>
        │   (afcli/agent_run)│  → SessionDetail with QueuedWork shape +
        │                    │     AuthToken + PlatformURL + WorkerID
        └──────────┬─────────┘
                   │ runner.Run(ctx, qw)
                   ▼
        ┌────────────────────┐
        │ runner orchestrator│  worktree → spawn provider → events →
        │                    │  tail recovery → result.Post
        └────────────────────┘
```

The `af` binary registered by `host install` doubles as both the
daemon supervisor (`af host run`) and the per-session worker
(`af agent run`) — the same binary, different subcommands. The
WorkerCommand defaults to `[<self-exe>, "agent", "run"]` resolved via
`os.Executable()`; operators rarely override this.

### `SessionDetail` lifecycle

- **Set**: `Daemon.AcceptWorkWithDetail` records the detail in an
  in-memory map keyed by session id when the poll loop dispatches a
  work item.
- **Read**: `GET /api/daemon/sessions/<id>` (handled by
  `daemon/server.go::handleSessionDetail`) returns the JSON payload
  to the spawned `af agent run` worker.
- **Delete**: the spawner emits `SessionEventEnded` when the worker
  child process exits; the daemon's listener removes the entry from
  the store so stale auth tokens do not linger in memory.

### Project admission and repository resolution

Project admission is independent of repository resources:

```yaml
projectAdmissionVersion: 2
enabledProjectIds: [project-alpha]
repositories:
  - id: repo-alpha
    projectId: project-alpha
    source: https://example.invalid/acme/alpha.git
    primary: true
```

`enabledProjectIds` is authoritative in v2, including when empty.
`SessionSpec.projectId` is checked before repository selection. When work names
a repository, the daemon verifies that resource belongs to the same project;
it never chooses the first configured repository implicitly.

#### Admission mode — the machine owner's standing consent

`projectAdmissionMode` decides how `enabledProjectIds` is read:

```yaml
projectAdmissionVersion: 2
projectAdmissionMode: all-routed
```

| Mode | Meaning |
| --- | --- |
| `enumerated` (default) | Admit exactly the projects in `enabledProjectIds`. The owner consents once **per project**. |
| `all-routed` | Admit any project the orchestrator dispatches here. The owner consents **once**, to the routing itself. |

Absent, blank, and misspelled all read as `enumerated`, so admission only ever
widens on a correctly spelled opt-in; an unrecognized value additionally fails
config validation rather than being silently narrowed in place. Set it with
`donmai project mode all-routed`, or answer the setup wizard's project-admission
question.

`all-routed` does not weaken the trust boundary. The daemon's registration token
is org-scoped, so the only work that can reach `AcceptWork` is work the
operator's own control plane routed to a pool this machine belongs to. What the
mode removes is the *second* enumeration of an intent the operator already
declared upstream when they routed the project here — not the consent itself.

The mode is hot-reloaded by the yaml watcher (`SetProjectAdmissionMode`), so a
change applies to the running daemon without a restart.

For one compatibility window, a config without `projectAdmissionVersion`
migrates legacy `projects[].{id,repository}` tuples into the enabled set and
normalized repository resources. V2 writes retain a legacy projection only for
enabled, repository-bearing projects.

#### Reporting admission upstream

Registration (`POST /api/workers/register`) carries `projectAdmissionVersion`,
`projectIds`, `daemonProjects`, and `projectAdmissionMode`.

The heartbeat carries the same admission state under one change-detector hash:
`allowlistHash` every beat, and `allowlist` + `enabledProjectIds` +
`projectAdmissionMode` only on the beats where that hash moves.

Reporting the enabled set and mode on the heartbeat — not registration alone —
is what lets an admission edit reach the orchestrator on the next beat. The
earlier contract hashed only the repository projection, so enabling a project
that had no repository resource produced an identical hash and an identical
payload; the orchestrator kept its stale copy until the daemon re-registered,
which only happens at process start. That is the "enable the project, then
restart the daemon" step, and it no longer exists.

`SessionDetail.repository` is resolved from normalized repository resources by
`PollItemToSessionDetail` (in `poll.go`). The runner uses this URL for `git clone`.

The platform's QueuedWork wire shape historically carries a
`projectName` slug (e.g. `"smoke-alpha"`) with no separate repository
URL — slugs are not clonable. When the poll item arrives the daemon
runs the same matcher as `WorkerSpawner.findProjectLocked`
by `id`, by `repository`, or by URL-suffix. The matching
resource's `source` field is substituted into
`SessionDetail.repository`, and the canonical `id` is mirrored back
into `SessionDetail.projectName` so downstream code that reads
`DONMAI_PROJECT_ID` sees a stable value.

If no repository resource matches, the daemon falls back to whatever the
platform sent (preserving prior behaviour) and emits a Warn log
`no allowlist match for projectName, falling back to as-given repo
string` so the misconfiguration is visible. Downstream
`WorkerSpawner.AcceptWork` will then reject an unconfigured repository or
project/repository mismatch, but the explicit
log makes the resolution-time failure observable separately from
the spawn-time rejection.

## HTTP control API

Localhost-only (binds 127.0.0.1). Endpoints:

| Method + Path                          | Purpose |
|----------------------------------------|---------|
| `GET    /api/daemon/status`            | Daemon lifecycle state, desired enabled IDs, applied project IDs, sessions |
| `GET    /api/daemon/stats`             | Capacity envelope, worker stats, enabled/applied project IDs |
| `POST   /api/daemon/pause`             | Stop accepting new work |
| `POST   /api/daemon/resume`            | Resume accepting work |
| `POST   /api/daemon/stop`              | Graceful stop |
| `POST   /api/daemon/drain`             | Drain in-flight work |
| `POST   /api/daemon/restart/prepare`   | Enter draining and obtain the closed planned-restart permission response |
| `POST   /api/daemon/session-shim/acceptance/<action>` | Dormant bearer-authenticated installed-artifact fault control; absent unless explicitly configured |
| `POST   /api/daemon/update`            | Trigger manual update check |
| `POST   /api/daemon/capacity`          | Update a config key (e.g. `capacity.poolMaxDiskGb`) |
| `GET    /api/daemon/pool/stats`        | Workarea pool snapshot |
| `POST   /api/daemon/pool/evict`        | Evict pool members |
| `GET    /api/daemon/sessions`          | List active session handles (incl. selected-repository `worktreePath`, additive `workareaRoot`, project, and repository enrichment) |
| `POST   /api/daemon/sessions`          | Accept a session (test entrypoint) |
| `GET    /api/daemon/sessions/<id>`     | **F.2.8** — per-session detail for the spawned worker |
| `POST   /api/daemon/sessions/<id>/stop`| Per-session kill: terminate one session + free its slot (200 on stop, 404 if unknown). Siblings unaffected — the HOL-isolation cancel path |
| `GET    /api/daemon/heartbeat`         | Most-recent heartbeat payload |
| `GET    /api/daemon/doctor`            | Aggregated health snapshot |
| `GET    /healthz`                      | Liveness probe |

`POST /api/daemon/restart/prepare` accepts no caller body or fence id. It
returns `session-shim-restart-preflight-v1` with state `prepared` only after
every non-empty authority scope durably acknowledges one immutable correlation
snapshot, or `not_required` after the drained daemon proves the snapshot is
empty. Any malformed/unknown success body, `409`, timeout, or transport failure
is a refusal: the caller must not invoke the service manager. Partial retries
reuse the daemon-minted preparation id and exact per-scope bytes. An explicit
`POST /api/daemon/resume` durably abandons only this controller's local stop
authorization before reopening admission; it never consumes external holds.

The session-shim acceptance route is disabled by default and returns the same
404 as an absent route unless `DONMAI_SESSION_SHIM_ACCEPTANCE_TOKEN_FILE` names
an absolute, private regular file. Even when enabled, each fault mutation binds
to an exact lifecycle already adopted by this daemon. The route returns no
evidence: installed acceptance must independently re-observe daemon/doctor,
heartbeat, process, and viewer-wire effects. The standalone target-owned
mutator and its closed command protocol live in
`cmd/session-shim-acceptance-mutator/`.

`POST /api/daemon/update` crosses the same preflight synchronously before it
reports that an update was initiated. Update success, failure, or absence leaves
the daemon draining until that explicit resume path runs.

Both `GET /api/daemon/status` and `GET /api/daemon/doctor` carry the same
additive `sessionShim` diagnostic: configured ownership mode, adoption
completion/time, occupied slots, every adopted shim/process/controller
correlation sourced from its authenticated live Hello, durable forwarded
sequence, and every quarantined capacity charge. The projection is deliberately
secret-free: no paths, output bytes, prompts, host/controller ids, tokens,
credentials, or opaque composing receipts are exposed.

For an externally composed session-shim, the outbound
`POST /api/workers/<id>/heartbeat` carries a separate authority-bound
`sessionShim` projection. Alongside the stable host, controller, adoption
revision, and quarantine set, it includes the five live proof-v2 readiness
facts: durable carrier acknowledgement, proof-v1 writer closure, encrypted
original-credential retention, the remaining-validity consume gate, and
adopted-candidate recovery. All five values are sampled from the configured
readiness resolver on every beat and must be true. Declaring the
`durable_carrier_proof_v2` capability is not readiness evidence. Missing,
malformed, changed, or non-exactly-echoed readiness fails the beat closed; older
JSON readers may ignore the additive keys.

## Auto-update signing

Auto-update is **opt-in and fail-closed**: the binary ships with no default
CDN (`autoUpdate` only runs when the operator configures a CDN base), and the
daemon refuses every binary swap unless signature verification passes against
an operator-pinned signer allowlist.

CDN layout per channel (`stable` | `beta` | `main`):

```
<cdnBase>/<channel>/latest.json                              # {version, sha256, releasedAt}
<cdnBase>/<channel>/<version>/donmai-daemon-<arch>-<os>      # binary
<cdnBase>/<channel>/<version>/donmai-daemon-<arch>-<os>.sigstore  # sigstore bundle JSON
```

The update flow is drain → fetch manifest → download binary + bundle →
SHA-256 integrity check against the manifest → sigstore bundle verification →
swap → restart. Verification (see `auto_update_verifier.go`) checks that:

1. the bundle's attested artifact digest matches the downloaded binary,
2. the signing certificate chains to the trust root — the embedded public
   Sigstore production root by default (shared with kit verification,
   `kit_trust.go`), or an operator-supplied root via
   `autoUpdate.trustRootPath` for private sigstore deployments,
3. the certificate identity matches one of the pinned
   `autoUpdate.signers` entries — `issuer` (exact) plus `san` (exact) or
   `sanRegex` (for CI identities whose SAN embeds the release ref).

If `autoUpdate.signers` is empty — the out-of-the-box state — every swap is
refused with `sig-rejected: no update signers configured`. There is
deliberately no identity-less mode: "any keyless signer the public trust
root validates" is not an acceptable policy for swapping the daemon binary.

Release pipelines produce the bundle with keyless signing, e.g.:

```bash
cosign sign-blob --yes --new-bundle-format \
  --bundle donmai-daemon-arm64-darwin.sigstore donmai-daemon-arm64-darwin
```

and operators pin the workflow identity in `daemon.yaml`:

```yaml
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
  signers:
    - sanRegex: ^https://github\.com/<org>/<repo>/\.github/workflows/release\.yml@refs/tags/v.+$
      issuer: https://token.actions.githubusercontent.com
```

## Kit install trust gate

Kit installs (`donmai kit install`, `POST /api/daemon/kits/<id>/install`)
prefer complete `donmai.dev/kit-package/v1` packages. The daemon authenticates
the canonical descriptor first, verifies the exact path/digest/size/mode
inventory in a private same-filesystem staging directory, and atomically
activates an immutable package digest through a durable registry generation.
Failures leave the prior generation active. Flat manifest installs remain a
compatibility path and are labeled `legacy-manifest-*`; their signature does
not authenticate payload files. The trust policy lives in `daemon.yaml`'s
top-level `trust` block:

```yaml
trust:
  mode: signed-by-allowlist        # permissive | signed-by-allowlist | attested
  issuerSet:
    - kit-publisher@example.com    # Fulcio SAN identities you trust
  actor: ops@example.com           # audit identity for trust overrides
```

**The default mode is `signed-by-allowlist`** — only kits whose signature
verifies AND whose signer identity (Fulcio SAN) appears in `trust.issuerSet`
install. Unsigned or unverified kits are rejected with HTTP 403 and an error
that spells out the remediation paths. `signed-by-allowlist` with an empty
`issuerSet` is treated as a misconfiguration: the kit surface stays readable
but installs are blocked until the operator populates the allowlist or
explicitly opts out.

Opting out (accepting that unsigned kits can execute arbitrary shell
commands):

- **Per install** — `donmai kit install <id> --allow-unsigned` sends
  `trustOverride: "allowed-this-once"`; the bypass is audit-logged with the
  kit id, signer, and configured `trust.actor`.
- **Globally** — set `trust.mode: permissive` in `daemon.yaml`, or export
  `DONMAI_KIT_TRUST_MODE=permissive` before starting the daemon. Permissive
  mode logs a prominent warning on every gated install.

`donmai kit verify <id>` shows one of `package-verified`,
`package-signed-unverified`, `legacy-manifest-verified`,
`legacy-manifest-unverified`, or `unsigned`, plus the signer and package digest
when applicable. These states are deliberately not interchangeable.

Target-aware session preflight now retains every command as the structured
`(kit id, local name, package digest)` identity and resolves generic aliases
for the exact OS, work type, and repository path scope. Multiple overlapping
claims fail before kit-controlled provisioning or command execution. An
operator can select one exact claimant with the RFC 8785 canonical lock at
`<first kit.scanPaths entry>/.composition.lock.json`; keeping this authority in
the daemon's private store prevents the target repository from selecting its
own command owner. Platform-supplied lifecycle demands remain authoritative,
but their exact `id@version` selections undergo the same local ownership
preflight. The resolved in-memory demand carries the qualified commands,
generic bindings, and canonical `compositionDigest`; structured session logs
currently emit that digest plus command and binding counts. Durable result
evidence for the full command plan remains future work. Legacy manifests use
their exact manifest-content digest for stable identity without upgrading
their trust state.

Signed owner/catalog delegation ingestion and signed catalog snapshot/TUF
synchronization remain separate fail-closed prerequisites before broader
catalog expansion. A verified package claim still covers package bytes and
atomic activation only; it does not imply catalog freshness.

## Operator runbook — debugging a stuck session

When a session appears wedged in the dashboard:

1. **Daemon log** — `af host logs --follow` (default
   `~/.rensei/daemon.log`). Look for the `worker spawner` lines
   showing `pid=…` and the matching `[child stdout sessionID=<id>]`
   (INFO) and `[child stderr sessionID=<id>]` (WARN) records from
   the spawned `af agent run` worker. Spawn output is wired to slog
   by default as of v0.5.1 — earlier daemons drained
   child stdio silently.
2. **Session detail** —
   `curl http://127.0.0.1:7734/api/daemon/sessions/<id>` to confirm
   the detail is recorded. A 404 here means the daemon never
   accepted the work (look for poll errors in the daemon log) or
   the session has already terminated and been cleaned up.
3. **`af agent run` log** — the worker child writes its own slog
   output to stderr. The daemon's spawner captures both streams
   under `[child stdout|stderr sessionID=<id>]`; the same lines
   appear inline in `af host logs` and in the control plane's
   session-activity stream.
4. **Provider logs** — when the runner reaches step 8 (`spawn
   provider`), the per-provider subprocess is the next layer
   (`claude` JSONL on stdout, `codex` JSON-RPC over stdio). The
   provider package's README explains how to capture those streams
   (`PROVIDER_DEBUG=1` for claude, `CODEX_LOG_LEVEL=debug` for
   codex).
5. **Platform-side state** —
   `curl https://platform.example.com/api/sessions/<id>` (with bearer auth)
   to confirm the platform sees the session in the expected state.
   A divergence between the daemon's view (still active) and the
   platform's view (already terminal) usually indicates a missed
   `result.Post` — re-run `af host stats` to see whether the
   poller has retried.
6. **Worktree state** — `~/.rensei/worktrees/<sessionId>/.agent/`
   contains the per-session `state.json` snapshot and the
   `events.jsonl` audit log. Look here when the agent emitted no
   visible output but the session is marked failed.

## Failure modes the daemon classifies (high-level)

| Symptom | Where it surfaces |
|---|---|
| WorkerCommand falls through to `/bin/sh` stub | `worker spawner` warn line in daemon log |
| Daemon HTTP unreachable from worker child | `af agent run preflight` error, exit code 2 |
| Session detail expired between fetch attempts | `af agent run preflight` error, exit code 2 |
| Provider probe failed at runner startup | `af agent run` Warn log "claude provider unavailable" — falls through to stub if the session asked for stub; otherwise the runner's `Resolve` fails with `FailureProviderResolve` |
| Worker child exited with non-zero | `SessionEventEnded` with `ExitErr` non-nil; daemon emits the failure to its log |

See `runner/README.md` for the runner-level failure-mode table that
the daemon receives via `result.Post` payloads.

## Tests

```bash
# Unit + smoke
go test -race ./daemon/...

# F.2.8 wire-path integration test (requires git on PATH)
go test -tags=f28_integration ./afcli/...
```
