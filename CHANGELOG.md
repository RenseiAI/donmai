# Changelog

All notable changes to the `donmai` binary are documented here.

Format: `## vX.Y.Z — YYYY-MM-DD` with subsections `Features`, `Fixes`, `Chores`. Unreleased work goes under `## [Unreleased]`.

---

## v0.57.0 — 2026-08-03

### Features

- **New top-level `host` noun — the commands for *this machine*.** `host` owns
  the local daemon's lifecycle (`install`, `uninstall`, `setup`, `run`,
  `status`, `logs`, `doctor`, `pause`, `resume`, `update`, `drain`, `stop`,
  `stats`, `evict`, `set`) alongside this machine's workarea pool, the
  providers and kits installed on it, project admission, and the local
  live-session dashboard (`provider`, `kit`, `workarea`, `project`, `watch`) —
  20 leaves, pinned by test so a future silent drop fails the build.
- **`afcli.NewHostCmd(ds, cfg)` is exported.** A composing downstream binary
  consumes one factory instead of hand-assembling an equivalent tree, and
  later additions to `host` reach it without a second edit. Each parent gets a
  fresh tree built from the same constructors: a `cobra.Command` carries a
  single parent pointer, so one shared instance would corrupt `CommandPath()`
  and every usage string rendered from it.
- **`fleet-watch` is now `host watch`** — it has always been a this-host
  dashboard. `fleet-watch` is retained as a hidden alias.

### Chores

- **`daemon` is now a hidden deprecated alias of `host`**, carrying the
  identical lifecycle leaves, so every existing `daemon <verb>` invocation
  keeps working. Its `Deprecated` string names a concrete removal version,
  **v0.58.0**, rather than "the next release". Alias descendants are
  deliberately not cobra-`Deprecated` — that would empty `daemon --help`;
  instead each prints the notice to **stderr** naming the exact replacement
  path, so `daemon status --json | jq` still parses.
- **Upgrade hazard for the v0.58.0 alias removal.**
  `installer/{launchd,systemd}.DaemonSubcommand` now registers
  `<host-binary> host run` instead of `daemon run`. Service units written by
  earlier builds still invoke `daemon run` and keep working for the whole alias
  window, but a unit already on disk is only rewritten by a re-run of
  `host install` — so the removal release must force (or verify) a re-install
  first, or the service stops on every machine that has not re-installed. The
  precondition is documented at the removal-version constant.
- User-facing strings swept to the new noun: install/uninstall/pause/stop help,
  the doctor "service is not installed" error, the daemon-unreachable hint
  (which also pointed at a `daemon start` subcommand that has never existed),
  the agent worker's provider-probe hint, `README.md`, `daemon/README.md`, and
  the standalone credentials doc.

---

## v0.56.6 — 2026-08-03

### Features

- **Daemon honours control-plane host status in the claim path.** The daemon
  already received and stored the host-status signal on every heartbeat
  response, but nothing consulted it before claiming new work. The poll loop
  now suspends claiming the moment the control plane reports the bound pool
  as paused, draining, disabled, or deleted; in-flight sessions and the
  heartbeat loop are untouched, and claiming resumes automatically once
  status returns to ok. An unrecognised status (including one that carries
  its own re-registration path) is deliberately not treated as a claim gate,
  so a newer server can never take a working daemon offline.
- **Daemon reports its own lifecycle status on the heartbeat.** The daemon
  computed its idle/busy/draining status on every heartbeat, but the value
  was dropped before reaching the wire — the struct actually marshalled onto
  the request body had no field for it. The heartbeat body now carries an
  optional `status` field.
- **`pool.deleted` is now handled** instead of falling through to the
  unsupported-operation default and NACKing on every heartbeat. It now
  records the host status immediately, so claiming suspends without waiting
  for the next heartbeat, and ACKs applied.

### Fixes

- **Interactive sessions honour `Spec.Autonomous`.** The interactive REPL
  spawn path discarded every `agent.Spec` field except the prompt, so it
  silently fell back to the CLI's default permission mode instead of mapping
  `Autonomous` to the same permission flag the headless spawn path has always
  used. Interactive and headless sessions built from an identical `Spec` now
  agree on permission posture. **Behaviour change:** interactive sessions
  dispatched with `Autonomous: true` now run under the same widened
  permission mode headless sessions already used.
- **Git auth headers are scoped to the remote they authenticate.**
  `internal/gitexec.HardenedEnv` injected the per-invocation credential as a
  bare `http.extraHeader`, which git applies to *every* HTTP(S) remote — and,
  because it travels via `GIT_CONFIG_*`, to every descendant process too
  (submodule fetches, `go mod download`, npm/pnpm git dependencies, pip VCS
  installs, SwiftPM). Besides offering the credential to unrelated hosts, this
  broke operations that should have succeeded: attaching *any* credential to an
  anonymous clone of a *public* repository makes the forge authenticate it, so a
  stale token turned a working clone into `remote: Invalid username or token`.
  The header is now emitted as `http.<remote>.extraHeader`, and an inherited
  unscoped `http.extraHeader` is reset to the empty list on every invocation so
  a stale ambient credential can no longer outrank the one an operation intends
  to use. A header that cannot be scoped (no remote, an SSH/scp-style remote, a
  local path) is dropped and logged rather than broadcast. `HardenedEnv` now
  takes a `gitexec.Auth{Header, RemoteURL}` in place of a bare header string;
  the package is module-private, so no downstream embedder API changes.

---

## v0.56.5 — 2026-07-31

### Features

- **Repository warm host (`donmai code host`).** A long-lived HTTP process mode
  serving the frozen six-tool code-intelligence contract at
  `POST /v1/tools/call` through the existing native engine. Workareas are keyed
  by exact immutable bindings (`orgId`, `projectId`, `repositoryPathId`,
  `revisionKind`, `revision`); the bounded pool single-flights warming, issues
  ref-counted leases, and evicts only idle LRU/TTL entries. Opaque repository
  IDs resolve solely through an operator-owned repository catalogue — clone
  sources and Git credentials never arrive in request JSON or JWT claims, and
  embedded HTTP(S) URL credentials are rejected. HS256 bearer JWTs are verified
  fail-closed against configured issuer, audience, expiry, signature,
  invocation subject, and every binding claim, with a defense-in-depth
  held-lease binding equality check. `CODE_INTEL_HOST_JWT_SECRET` is blocked
  from forwarded agent and Git child-process environments (#243).
- **Path-ID warm-host catalogs and a Git auth seam.** Generated daemon
  repository entries now carry `pathId`, and warm-host catalogs are required
  and indexed by `pathId` — the database row `id` is retained as metadata only.
  A new `GitFactory.GitAuth` seam forwards `afcli.Config.CodeHostGitAuth` into
  clone and fetch operations, injecting credentials only as an in-memory
  `http.extraHeader`; static credential-helper and SSH-key configuration
  remains supported. Composing binaries must populate `RepositoryEntry.PathID`
  and set `CodeHostGitAuth` from their runtime-JWT token resolver (#244).
- **Outbound host relay tunnel client.** New public
  `github.com/RenseiAI/donmai/hostrelay` host-relay-v1 wire codec plus a strict
  outbound-only WSS client for a single workload connection, with bounded
  forwarding, deadlines, cancellation, liveness pings, and no replay on
  disconnect. Optionally wired into `donmai code host` behind identity flags and
  exactly one of `--relay-token-env` or `--relay-token-file`; local host
  signing-secret env names are rejected as tunnel-token inputs. No inbound
  listener is introduced (#245).

### Chores

- Dependency maintenance: Go module `go-minor-patch` group bumped across 10
  updates (#183) and the GitHub Actions group across 9 updates (#201).

---

## v0.56.0 — 2026-07-27

### Features

- **Reusable prompt-experiment driver.** New OSS-pure `eval/experiment`
  package: bounded arm/experiment identity, SHA-256-bound process-local
  prompt variants, balanced-matrix preplanning, deterministic shared
  perturbations, and a fail-closed context-reset seam. `eval/codeintel`
  becomes a consumer of the shared matrix (#226).

### Fixes

- **Interactive sessions never receive the batch work prompt.** An
  interactive-mode session with no seed prompt used to fall through to the
  development work-type template ("Start work on …") against its synthetic
  session anchor, so the hosted agent immediately self-reported blocked.
  Interactive builds now produce an empty user prompt (harness starts idle
  awaiting attached input); a provided seed prompt is delivered verbatim;
  batch/interview prompts are byte-identical to before (#228).
- **Interactive heartbeat loss degrades instead of killing (F1).** For
  `sessionClass=interactive`, heartbeat strike-out and post-wake
  `refreshed=false` no longer terminate the session: the pulser logs a loud
  degrade, keeps ticking through the outage, and resumes cleanly on
  recovery. Non-interactive sessions keep fail-fast behavior. The launchd
  installer also emits the durability keys (`ExitTimeOut` covering the
  drain window, `AbandonProcessGroup`, `KeepAlive{Crashed}`,
  `LegacyTimers`) (#222).
- **opencode ETXTBSY flake closed at the root**: the fake CLI fixture is
  built once in `TestMain`, eliminating the fork/execve descriptor window
  (#225).

### Chores

- CI: worker-image PR builds cut from ~7m35s to ~49s (cross-compile from
  `BUILDPLATFORM`, docs path filters) (#224); skip counts reported and the
  duplicate govulncheck run removed (#227).

---

## v0.55.0 — 2026-07-26

### Features

- **Worker-local translating gateway (M1).** A `ModelEndpoint` host that
  presents an openai-chat surface on a loopback-only bind (#212), now bound as
  a per-agent-run gateway session by the worker (#219). The gateway executes a
  chosen capability cell; it never chooses one. Upstream credentials stay with
  the worker — `DONMAI_GATEWAY_UPSTREAM_API_KEY` and
  `DONMAI_GATEWAY_UPSTREAM_BASE_URL` are blocklisted from child environments.
- **pi harness.** Greenfield pi adapter with an explicit trust boundary (#206),
  registered in the agent-run constructor list (#214).
- **opencode Lane B.** Serve/HTTP adapter (#205) with the resolved-profile
  `preferServer` hint threaded into the constructor (#209).
- **Canonical credential blocklist.** `blocklist.json` is generated as the
  cross-repo single source of truth for worker-only env names (#210).
- **Matrix integrity.** `binaryPins` section with opencode version-pin
  enforcement (#203); terminal-event conformance contract wired onto the
  claude/codex/stub harnesses (#204).
- **Cells smoked.** The opencode `openai×direct` (#218) and
  `openai×gateway` (#221) capability cells flipped `smoked: true` on accrued
  green smoke history.

### Fixes

- **Gateway hardening.** Upstream base-URL validation, inherited-environment
  blocklist filtering at every `ComposeChildEnv` site, and classified upstream
  error responses instead of raw transport/provider detail (#220).
- **pi protocol.** Speak the real pi 0.80.10 RPC + extension protocol (#215);
  thread the resolved endpoint into the runner spec (#216).
- **opencode.** Lane-B SSE frames wrap payloads in `data` (#211).
- **Prompt hardening.** Injection guards + stale runner-line removal in
  `system_base.tmpl` (#207) and `system_base.yaml` (#208).

### Chores

- Linear command family extracted into `afcli/linearcmd` (#213); docs path
  updates (#217).

---

## v0.54.1 — 2026-07-23

### Fixes

- **Truthful stop acknowledgement during natural cleanup.** `StopSession` now
  returns an idempotent acknowledgement for a generation still owned in the
  registry during natural cleanup, instead of a false `404 session not found`
  from the daemon and HTTP stop endpoint, without signaling, canceling, or
  altering classification. Truly absent or released generations still return
  not-found. (#198)
- **opencode adapter contract and flag fixes.** Successful `opencode run`
  sessions no longer emit a spurious `spawn_no_result` error after the terminal
  result event; the unsupported permission-skip flag was replaced with
  `--auto`; the default endpoint port now matches opencode's actual default
  (4096). Adds a shared terminal-event conformance check. (#199)

### Chores

- CI: every job now sets `timeout-minutes`. (#200)
- Security: bump indirect `google.golang.org/grpc` to 1.82.1
  (GHSA xDS RBAC / HTTP/2 fixes; dependabot alert 18).

---

## v0.54.0 — 2026-07-20

### Features

- **Durable terminal-workarea leases.** Successful sessions can retain their
  exact workarea under a bounded, crash-recoverable lease until semantic result
  acknowledgement or expiry. The daemon persists canonical lease, receiver,
  immutable status-outbox, release, reaper, and quarantine authority; status
  payloads expose only a path-free four-field projection, and restart replay
  resolves fresh ephemeral authorization without persisting bearer tokens.

---

## v0.53.0 — 2026-07-20

### Features

- **Per-session credential-socket capabilities.** The Go and TypeScript
  credential clients can send an optional `HELLO.capability`, resolved from an
  explicit option or `DONMAI_CREDENTIAL_CAPABILITY`, while continuing to omit
  the field for legacy clients and servers. The capability control variable is
  blocklisted from credential snapshots and updates.
- **Credential requirement metadata.** Daemon poll, resolved-profile,
  session-detail, and spawn-spec wires now carry ordered, non-secret
  `credentialRequirements` groups plus the resolved `harness` and
  `servingHost`. All fields are additive and omitted when absent; explicit
  empty requirement lists and group/name ordering remain distinguishable while
  decoding and are forwarded without normalization.
- **Interactive session initial prompts.** Carries the optional `initialPrompt`
  field through daemon polling into the runner and writes it exactly once as the
  first PTY input for interactive sessions. Delivery remains cancellable on
  ownership loss and does not change headless or interview prompt construction.
- **Classified interactive occupancy.** Registration and heartbeat payloads can
  report additive `activeInteractiveCount` alongside total active sessions,
  counting both current interactive mode and its legacy interview alias while
  preserving absent-versus-zero compatibility for older embedders.

### Fixes

- **Aborted spawn and session-detail cleanup.** Adds the public
  `SpawnerOptions.OnSpawnAborted` rollback hook with exact ownership semantics:
  pre-spawn failures retain their own cleanup, process-start failures roll back
  once with the returned wrapped error, and successful starts transfer cleanup
  to `SessionEventEnded`. `AcceptWorkWithDetail` validates matching session ids,
  rejects duplicate active deliveries before replacing their detail, and uses
  generation-owned rollback so a rejected attempt cannot delete a live or later
  session's detail.
- **Data-free credential frame diagnostics.** Wrong handshake frames and
  unknown post-handshake frames now produce fixed Go and TypeScript diagnostics
  that never interpolate peer-controlled frame types or reflected capability
  material.
- **Interactive attach reconnection.** The runner can re-read a rotating attach
  token from `ATTACH_TOKEN_FILE` for every relay dial attempt, so a reconnect is
  not stranded after the initial short-lived token expires. A missing or
  unreadable file may fall back to the static token during startup or atomic
  refresh races, with deduplicated warnings. An existing empty, malformed, or
  oversized file fails explicitly instead of silently presenting stale auth,
  and degraded auth refresh retries are bounded.
- **Interactive attach child-environment isolation.** Runner-owned `ATTACH_URL`,
  `ATTACH_TOKEN`, and `ATTACH_TOKEN_FILE` controls are removed from every
  provider process, PTY child, and model-invoked tool subprocess, including
  explicit per-session environment overrides, while remaining available to the
  runner's host-side attach leg.

### Chores

- **Adaptive PTY firehose race gate.** Sizes the race-mode backpressure volume
  from a bounded warmup throughput probe instead of a machine-specific fixed
  byte count, retaining all existing pressure and memory assertions.
- **Secret-safe credential metadata tests.** Verifies ordered credential
  requirements directly instead of serializing session-detail fixtures that
  also contain secret-bearing authentication fields.
- **Faster hosted CI.** Migrates repository workflows to Blacksmith runners
  without changing their test, security, release, or image-build responsibilities.
- **Reproducible release automation.** Pins the Blacksmith worker-image actions
  to reviewed immutable commits; makes every publisher share one strict SemVer
  tag grammar and prove a detached checkout at the tag's peeled commit; passes
  the verified tag explicitly to GoReleaser; prevents manual retries and
  prerelease tags from moving GitHub Latest, the GHCR `latest` image, or E2B
  `default`; skips the Homebrew publisher for every prerelease and manual retry
  so older tags cannot roll back the stable cask; publishes an E2B
  `donmai-worker:<version>` target for every release; and anchors GoReleaser
  publication to the exact commit it built.
- **Go 1.25.12 release floor.** Raises the module and worker-image toolchain to
  Go 1.25.12 and makes every Go-using workflow consume the canonical `go.mod`
  version instead of carrying independent floating or stale versions.

---

## v0.52.1 — 2026-07-15

### Chores

- **Interactive-attach viewertest harness.** Adds an OSS viewer-side test
  harness for the interactive attach path: a `viewertest` package with a
  screen-decode driver and Screen-assert helpers over the attach VT feed, plus a
  `vtfixture` fixture TUI and viewer-driven `snapshot_request` support in the
  attach test client. Test infrastructure only — no changes to shipped runtime
  behavior.

---

## v0.52.0 — 2026-07-14

### Features

- **Outbound interactive attach and PTY hosting.** Adds the
  versioned attach wire, stateful terminal sanitizer and snapshots, bounded PTY
  host, outbound attach client, interactive runner loop, and registry-declared
  Claude Code, Codex, and shell PTY harness modes without introducing an inbound
  listener into the OSS daemon.

### Fixes

- **Deterministic interactive terminal environment.** PTY children now receive
  `TERM=xterm-256color` and `COLORTERM=truecolor` regardless of the launching
  process's terminal environment, while explicit per-session overrides remain
  authoritative.

---

## v0.51.0 — 2026-07-12

### Features

- **Hard per-session daemon mutation.** Heartbeats now apply `session.kill`
  mutations by sending `SIGKILL` to the daemon-owned worker process group,
  preserving sibling sessions and reporting applied or failed mutation ACKs on
  the next heartbeat. Repeated kills of a known, already-ended session are
  idempotent; unknown session IDs fail closed instead of signaling arbitrary
  processes.

---

## v0.50.3 — 2026-07-10

### Features

- **Explicit project admission.** Daemon configuration now keeps the set of
  enabled projects separate from repository resources, allowing a project to
  be admitted before any repository is configured and allowing multiple
  repositories to belong to one project. Existing configurations migrate to
  the versioned contract while preserving repository credentials and stable
  project identifiers.

### Chores

- **Reliable release snapshots.** Local release dry-runs now skip the
  production signing pipe explicitly, matching GoReleaser v2 behavior while
  leaving tag-pushed release signing unchanged.

---

## v0.39.0 — 2026-06-11

### Features

- **Durable CI wait — runner half (ADR-2026-06-10-durable-ci-wait.md).** Agent
  sessions no longer wait for remote CI; the orchestration layer owns the CI
  wait. The runner now captures the worktree's head commit at envelope-build
  time (after tail recovery and the backstop) into the already-typed
  `Result.CommitSHA`, and the terminal status post carries additive
  `commitSha` + `pullRequestUrl` fields so the platform can correlate CI
  completion events to the session's pushed head. The development prompt
  contract is redefined: `WORK_RESULT:passed` means local verification green
  (tests/typecheck/lint) + branch pushed + PR open — remote CI is explicitly
  excluded, and a new hard rule forbids in-process wake-ups that expect to
  outlive the final message. Additive wire fields; old platforms ignore them.

### Security

- **Kit installs now default to `signed-by-allowlist` (BREAKING).** The
  daemon's kit trust gate no longer defaults to `permissive`: with no
  `trust:` block in `daemon.yaml`, only kits whose sigstore signature
  verifies AND whose signer (Fulcio SAN) appears in `trust.issuerSet`
  install. Unsigned/unverified kits are rejected (HTTP 403) with an error
  spelling out every remediation path; `signed-by-allowlist` with an empty
  `issuerSet` fail-closes installs (the kit surface stays readable) instead
  of silently accepting any validly-signed kit. Operators who knowingly
  accept unsigned-kit risk can opt out per install
  (`donmai kit install --allow-unsigned`, audit-logged) or globally via
  `trust.mode: permissive` / `DONMAI_KIT_TRUST_MODE=permissive` (logs a
  prominent warning per gated install). Also fixed: SAN-only
  `trust.issuerSet` entries were dropped as malformed (sigstore-go requires
  issuer criteria), so a configured allowlist could never match — entries
  now match the SAN exactly with the OIDC issuer wildcarded; and a verifier
  construction failure now keeps the configured trust mode (fail-closed)
  instead of downgrading to a permissive verifier. The trust-gate 403
  detail now reaches `donmai kit install` output, which appends the
  allowlist/override/permissive guidance. Closes the "operator installs
  attacker kit → arbitrary shell execution" gap (CWE-494). See
  `daemon/README.md` § "Kit install trust gate".

- **dmk_ machine token no longer travels in a URL query string.** The
  dashboard claim link printed on `daemon install` now carries the token in
  the URL fragment (`<dashboard>/claim#token=…`) instead of
  `…/api/auth/claim?token=…`. Fragments are never sent over the wire, keeping
  the secret out of server access logs, proxy logs, and Referer headers.
  Requires the paired dashboard release that serves the `/claim`
  fragment-exchange page.

### Fixes

- **Daemon runtime-token churn is now quiet.** Three-part fix for the
  unbounded daemon-error.log growth: (1) a proactive token refresher re-mints
  the runtime JWT before expiry (lead 5m, retry 1m), so the hourly reactive
  401→refresh cycle on BOTH the heartbeat and poll paths becomes a rare
  backstop; (2) any refresh now fans fresh credentials out to both loops
  (`SetCredentials`), eliminating the duplicate second refresh per expiry;
  routine "rejected — refreshing" lines demoted Warn→Info and the
  `[runtime-token]` trigger event renamed `401`→`refresh-requested`;
  (3) `daemon run` rotates `daemon.log`/`daemon-error.log` at 10 MiB
  (copy-truncate, one `.1` generation) at boot and every 6h.
- **Activity toolOutput truncation no longer drops trailing PR URLs.** The
  forwarded tool-output cap is raised 500 → 2000 chars
  (`DefaultMaxToolOutputChars`), switches from head-only to middle-elision
  truncation (head + tail survive — `gh pr create` prints the PR URL last),
  and is now overridable per poster via `Config.MaxToolOutputChars`
  (negative disables the cap).
- **`arch assess` explains diff-fetch degrades.** When the GitHub CLI is
  missing (or the PR diff fetch fails), the metadata-only fallback now prints
  WHY to stderr — including the `gh` install instructions — instead of silently
  emitting zero observations.

### Changes

- **Arch-drift seam is contract-only.** Removed the `LaneAdapter` reference
  implementation from `afclient/codeintel` — per
  ADR-2026-06-07-intelligence-implementation-is-platform, OSS ships the
  `ModelAdapter` interface, request/response types, and `DriftVerdictSchema`
  only; drift implementations live with the intelligence owner.
- **Governor queue: legacy `agentfactory:governor:queue` dual-write/dual-read
  removed.** The debrand transition window is over — `Enqueue` writes only the
  canonical `donmai:governor:queue` key and `Peek` no longer falls back to the
  legacy key.

---

## v0.13.0 — 2026-06-02

### Changes

- **Memory inject is now platform-gated per project — no worker config.**
  Removed the `MEMORY_INJECT_ENABLED` env gate. The runner wires the heartbeat
  `OnInject` hook whenever the provider supports message injection (claude);
  whether a block is delivered is decided entirely by the platform (it only
  returns an `inject` on the lock-refresh response when the project's memory
  config has runtime-inject enabled). Daemons/TUIs need zero local config —
  a tenant toggles memory in project settings on the platform. Pairs with
  platform PR #203.

---

## v0.12.0 — 2026-06-02

### Features

- **Agent memory runtime inject (Wave 3).** Deliver platform-computed memory
  blocks into a running session via the per-session lock-refresh heartbeat →
  `handle.Inject` (claude `--resume`, between-turn). Adds `refreshResponse.inject`
  + `refreshRequest.ackedInject` echo to the pulser; the runner drains injects at
  the post-terminal seam through a shared `injectDirective` helper. Env-gated by
  `MEMORY_INJECT_ENABLED` (default off). Pairs with platform PR #202.
- **Dispatch-time memory fold.** New `memoryBlock` field threaded through
  PollWorkItem → SessionDetail → QueuedWork → prompt builder, folded under an
  `# Agent Memory` heading. Works for all providers (not just claude).

---

## v0.7.7 — 2026-05-12

Patch — schema fix for `ListWorkflowStates`.

### Fixes

- **`queryListWorkflowStates` $teamId — `String!` → `ID!`.** Linear's
  schema rejects `String!` at the `filter: { team: { id: { eq: $teamId } } }`
  position with `Variable $teamId of type String! used in position
  expecting type ID.`. The error surfaced for the first time once
  v0.7.6's CLI Linear proxy started normalizing Linear-side GraphQL
  errors back to the caller — every `rensei linear update-issue --state`
  (which resolves a state name → ID via this query) was failing as
  "server error" before the normalization. Sibling queries
  (`queryListSubIssues`, `queryListBacklogIssues`) already used `ID!`;
  this one was the outlier.

---

## v0.7.6 — 2026-05-12

CLI Linear proxy support — `linear` subcommands can now authenticate via
a downstream-embedder's platform login session instead of requiring a
`LINEAR_API_KEY` env var. Per
donmai-architecture/ADR-2026-05-12-cli-linear-proxy.

### Features

- **`linear.Client.ProxyMode` + `linear.NewProxiedClient(baseURL, rskToken)`** —
  the Linear GraphQL client gains a proxy-mode toggle. When `ProxyMode`
  is true the `Authorization` header switches from raw `<APIKey>` (Linear-
  direct semantics) to `Bearer <APIKey>` (platform proxy semantics), and
  `NewProxiedClient` composes a `BaseURL` that targets the embedder's
  `/api/cli/linear/graphql` proxy route. All query/mutation strings and
  response decoders are unchanged — `linear.Linear` callers don't care
  which mode they got.
- **`afcli.newLinearCmd(ds)` accepts a DataSource factory.** Matches every
  other afcli command that touches an embedder's API. Resolution order
  inside `newLinearClient`:
    1. `LINEAR_API_KEY` / `LINEAR_ACCESS_TOKEN` env → direct path.
    2. Authenticated `*afclient.Client` from `ds()` (rsk_ token + base URL)
       → `linear.NewProxiedClient`.
    3. Neither → actionable error pointing the user at the env var or
       `rensei login` + `rensei project trackers connect-linear`.
- **`afclient.CredentialsFromDataSource(ds)`** — pure helper that recovers
  `(baseURL, token, ok)` from a DataSource. Returns ok=false for
  unauthenticated or non-Client DataSources; callers degrade gracefully.

### Chores

- `afcli/commands.go:76` now passes `ds` to `newLinearCmd`. This is the
  one-line closure of the structural bypass that motivated the ADR.

---

## v0.7.5 — 2026-05-12

Activity-poster wire-format extension that lets the platform reconstruct
Layer 6 hook events from daemon-emitted tool calls (per
donmai-architecture/ADR-2026-05-12-cross-process-hook-bus-bridge),
plus a daemon-readiness fix and a launchd KeepAlive change.

### Features

- **`runtime/activity` carries toolUseId / isError / durationMs / providerName** —
  the wire payload to `POST /api/sessions/<id>/activity` now includes the
  four fields the platform-side bridge needs to reconstruct
  `pre-tool-use` / `post-tool-use` / `tool-use-error` hook events. The
  daemon-internal `agent.ToolUseEvent` / `agent.ToolResultEvent` already
  carried `ToolUseID` and `IsError` — `mapEvent` was stripping them. The
  poster tracks per-tool-use start times in a `sync.Map` keyed on
  `ToolUseID` so paired result events get a real wall-clock `DurationMs`.
  `ProviderName` is threaded from `qw.resolvedProvider()` at poster
  construction.

### Fixes

- **Daemon: never report ready while the spawner is silently NACKing** —
  fixes a window where a host install would report `af daemon stats` as
  healthy/idle even though every claimed session was being NACK'd
  immediately because of an allowlist mismatch. The readiness probe now
  observes the spawner's NACK rate and surfaces a degraded status.
- **Installer: KeepAlive only on failure** — launchd was respawning the
  daemon on user-initiated `af host stop`. The plist now sets
  `KeepAlive: { SuccessfulExit: false }` so a clean stop sticks; a crash
  still relaunches.

---

## v0.7.2 — 2026-05-08

Bug-fix roll-up after v0.7.1: three afclient corrections for the platform's
`/api/public/session-activities` endpoint (so unauthenticated CLI calls reach
the public branch and authenticated ones don't double-feed `sessionHash`),
plus daemon hardening — install/uninstall now wipes `~/.rensei/daemon.jwt`
to break re-register loops on stale tokens, and rejected work is NACK'd
instead of silently dropped.

### Fixes

- **`afclient.GetActivities` targets `/api/public/session-activities`** — the
  previous path was `/api/sessions/<id>/activities`, which routes through the
  authenticated tree and 404s for the public CLI flow.
- **`sessionHash` only on unauthenticated GetActivities** — when the caller
  has a `rsk_` token, omitting `sessionHash` lets the platform take the
  `rsk_`-scoped read path. Authenticated callers were sending both, so the
  server applied the public-hash filter inside the rsk_ branch and silently
  returned empty.
- **Unauthenticated GetActivities passes `sessionHash`** — the inverse of
  the above for the no-`rsk_` branch. Public CLI calls without a token now
  carry the hash so the platform's public hash-keyed lookup succeeds.
- **Daemon NACKs rejected work + silences misleading allowlist WARN** —
  when the daemon refuses a session for allowlist reasons it now NACKs the
  work back to the queue (instead of leaving it claimed-but-unrun) and the
  WARN that read like a fatal error has been clarified.
- **Daemon wipes cached JWT on install/uninstall** — removes
  `~/.rensei/daemon.jwt` so a re-install on a stale machine doesn't loop on
  a JWT minted for a worker_id the platform no longer knows. rensei-tui's
  `host install` already had this behaviour; the upstream `af daemon
  install` now matches.

### Tests

- **`test(afcli): stop tests from clobbering the developer's launchd
  domain`** — `go test ./...` on macOS could previously `bootout` the
  developer's real `dev.rensei.daemon` LaunchAgent because install/uninstall
  tests took the ServiceManager path. Tests now pass
  `--skip-service-manager` (the v0.7.1 hidden flag) explicitly and only the
  ServiceManager-specific suite exercises the real launchd domain.

### Docs

- **`af provider --help` no longer leaks roadmap commentary** — the Long
  help text was carrying a "TODO: per-Provider-Family registries" note that
  surfaced in user-visible CLI output.

---

## v0.7.0 — 2026-05-07

### Features

- **Daemon HTTP control API for the four operator surfaces (Wave 9)** — Provider, Kit, Workarea, and Routing surfaces now ship as canonical OSS endpoints under `/api/daemon/*`, joining the pre-existing seven daemon lifecycle routes. New endpoints: `GET /api/daemon/providers`, `GET /api/daemon/providers/<id>`, `GET /api/daemon/kits`, `GET /api/daemon/kits/<id>`, `GET /api/daemon/kits/<id>/verify-signature`, `POST /api/daemon/kits/<id>/{install,enable,disable}`, `GET /api/daemon/kit-sources`, `POST /api/daemon/kit-sources/<name>/{enable,disable}`, `GET /api/daemon/workareas`, `GET /api/daemon/workareas/<id>`, `POST /api/daemon/workareas/<archiveID>/restore`, `GET /api/daemon/workareas/<idA>/diff/<idB>`, `GET /api/daemon/routing/config`, `GET /api/daemon/routing/explain/<sessionID>`. Localhost-only auth model (no bearer). Contract locked in `donmai-architecture/ADR-2026-05-07-daemon-http-control-api.md`.
- **`af provider` / `af kit` / `af workarea` / `af routing` Cobra command trees** — First-class top-level commands on the `af` binary, sourced from the new daemon HTTP surface. `provider list/show`, `kit list/show/install/enable/disable/verify/sources`, `workarea list/show/restore/diff`, `routing show/explain`. Each delegates to the local daemon at `127.0.0.1:7734` (overridable via `RENSEI_DAEMON_URL`) and renders through the new `afview/` package.
- **New public package `afview/`** — Houses surface-specific composed renderers (`afview/provider`, `afview/kit`, `afview/workarea`, `afview/routing`). Joins `afclient`/`afcli`/`worker` as the fourth public package. Both binaries (af and rensei) import the same renderers; no forks. Plain-text fallbacks for each surface's list/detail views are what `rensei-smokes` pins against.
- **21 new `afclient` types** — Provider/Kit/Workarea/Routing wire types live in `afclient/{provider,kit,workarea,routing}_types.go` matching the daemon's `/api/daemon/*` namespace. Notable shapes: `ListProvidersResponse.PartialCoverage` flag (honest about agent-runtime-only coverage in this wave), `WorkareaSummary.Kind` discriminating active pool members vs on-disk archives, structured `WorkareaDiffEntry` with per-path SHA-256 hashes + size + mode + symlink target, `RoutingDecision` + `RoutingTraceStep` for per-session decision explain.
- **8 new exported `afcli` factories** — `NewProviderCmd`, `NewKitCmd`, `NewWorkareaCmd`, `NewRoutingCmd` and their backing private helpers, exported via `afcli/exports.go` so downstream binaries can graft the canonical command trees under their own parent commands. The rensei binary uses these to expose `rensei host {provider,kit,workarea}` and `rensei routing` without forking.
- **Workarea diff streaming** — `GET /api/daemon/workareas/<idA>/diff/<idB>` switches between a single JSON envelope and NDJSON streaming based on entry count vs the daemon's configured threshold (default 1000, `daemon.yaml` key `workarea.diffStreamingThreshold`). Both shapes carry the same `WorkareaDiffEntry` per-path payload; consumers discriminate via `Content-Type`. The afclient method consumes both into a unified `WorkareaDiffResult`.
- **Workarea archive restore** — `POST /api/daemon/workareas/<archiveID>/restore` materialises an archive into a fresh active pool member with a NEW id (archives are immutable). Conflicts on `IntoSessionID` → 409; pool saturation → 503 with Retry-After; corrupted archive → 400.
- **Daemon-side Kit registry** — Minimal in-process Kit registry that scans `~/.rensei/kits/*.kit.toml` per `005-kit-manifest-spec.md`'s § Daemon kit registry. Multi-path support via `daemon.yaml` `kit.scanPaths` override. Malformed manifests log warnings and are excluded; empty registry returns `{kits: []}` with HTTP 200. Enable/disable state persisted to `<scanPath>/.state.json`.
- **Daemon-side routing trace store** — In-memory ring buffer (default 50) of recent routing decisions plus per-session lookup keyed on `SessionID` for `routing explain`. Hookable `RecordDecision` seam for the future cross-provider scheduler. Default Thompson-Sampling weights `{Cost: 0.7, Latency: 0.3}` per `004-sandbox-capability-matrix.md`.
- **`runner.ProviderView` adapter** — Widens `runner.Registry` for HTTP surface use. Surfaces the in-process AgentRuntime registry as the `agent-runtime` Provider Family entries; documented `partialCoverage: true` flag honestly reports the other seven Provider Families return empty until per-family registries land.

### Fixes

- **None specific to this release.** Wave 9 was a structural refactor; earlier observability bug (`auditClientFromConfig` delegating to the daemon-targeted client) was caught in rensei-tui's parallel cleanup and is fixed there.

### Chores

- **`daemon.Version` bumped to `0.7.0`** — reported in registration and status payloads; was last bumped to `0.5.5` and drifted out of sync with the git tag during v0.6.0.
- **New error sentinels** — `ErrConflict`, `ErrUnavailable`, `ErrBadRequest`, `ErrUnimplemented` in `afclient/errors.go` for HTTP 409 / 503 / 400 / staged-implementation cases.

---

## v0.6.0 — 2026-05-06

### Features

- **Ollama agent-runtime provider (full real impl)** — Local-first provider against `http://localhost:11434/api/chat` with `stream=true`. Stateless: `Resume` and `Inject` return `ErrUnsupported`; `Stop` cancels the in-flight request via context. Probe is `GET /api/tags`; missing daemon → registry warn-and-skip. 25 unit/integration tests via httptest fake. Capabilities conservative (no tool plugins, no resume, no MCP).
- **Gemini native agent-runtime provider (full real impl)** — Against Google's `generativelanguage.googleapis.com/v1beta/models/<model>:streamGenerateContent?alt=sse`. Spec translation builds `Contents` + `systemInstruction` + `GenerationConfig`; event mapping handles text parts, finishReason/usageMetadata terminals, promptFeedback.blockReason errors. Auth via `GEMINI_API_KEY` or `GOOGLE_API_KEY`. 26 tests.
- **Amp + OpenCode providers (registration-only)** — Both upstream APIs lack the stability today for a real runner. Constructors probe their env/endpoint and register cleanly; `Spawn` returns `ErrSpawnFailed` with a clear "runner not yet shipped" message. The contract has the architectural hooks so real impls drop in without contract change once Sourcegraph/SST stabilize. 20 tests across the two.
- **Daemon protocol: `machineId` on register, `maxSessions` on heartbeat** — Aligns with the platform's `worker_hosts` schema. `RegisterRequest.MachineID` populated from daemon config; `HeartbeatPayload.MaxSessions` populated via new `Daemon.maxConcurrentSessions()` getter that reads `Config.Capacity.MaxConcurrentSessions` under `d.mu.RLock()`.
- **Tool-use capability surface declared on every provider** — Adds `AcceptsAllowedToolsList` and `AcceptsMcpServerSpec` to `agent.Capabilities`. Each provider declares the flags honestly: claude (true/true), codex (false/true — MCP via `config/batchWrite`), stub (true/true — exercises gating), ollama / gemini / amp / opencode (false/false). Runner `spec_translation` strips `Spec.MCPServers` and `Spec.AllowedTools` for providers that declare false (warn-and-ignore). New `runner/spec_translation_test.go` and `afcli/tooluse_matrix_test.go`.

### Fixes

- **Data race: `handleSetCapacity` vs heartbeat read of `Capacity.MaxConcurrentSessions`** — The local control API's `POST /api/daemon/capacity` writes the field under `d.mu.Lock()`, but the heartbeat closure captured the underlying pointer and read it without the lock. Routed through new `Daemon.maxConcurrentSessions()` getter. Caught by CI's `-race` detector — the race was real (operator-driven capacity change racing with the next heartbeat tick).

### Features (foundation-tool-use)

- **v2 contract tool-use surface declared on every provider** — Adds the two forward-declared flags from `donmai-architecture/002-provider-base-contract.md` §"Tool-use surface" to `agent.Capabilities`: `AcceptsAllowedToolsList` (provider honors `Spec.AllowedTools` end-to-end) and `AcceptsMcpServerSpec` (provider honors `Spec.MCPServers` end-to-end). Each of the seven providers declares the flags **honestly against what its impl already delivers**: claude (true/true — already wired via `--allowedTools` + `--mcp-config`), codex (false/true — MCP via `config/batchWrite`, AllowedTools dropped because codex routes per-tool permission through the approval bridge), stub (true/true — exercises every gating branch), ollama / gemini / amp / opencode (false/false). Runner spec-translation strips `MCPServers` when `!SupportsToolPlugins || !AcceptsMcpServerSpec` and `AllowedTools` when `!AcceptsAllowedToolsList`, matching the existing warn-and-ignore semantics for capability-gated fields. New tests: `runner/spec_translation_test.go` (gating matrix), `afcli/tooluse_matrix_test.go` (registry-level capability table guard), per-provider capability assertions extended in claude/codex/gemini/ollama/amp/opencode/stub. No upstream API change — Claude already passes the equivalent of `tools[]` via its CLI's `--allowedTools` + MCP stdio bridge; the contract addition is the explicit declaration that providers cannot lie about it.

### Features (v0.5.5)

- **Phase 2 daemon-side stage-prompt scaffolding** — Closes the runner-side gap left by the platform PR #154 that introduced `agent.dispatch_stage`. The daemon's `PollWorkItem` and `SessionDetail` now decode + forward five new wire fields the platform's `QueuedStageWork` extension carries: `stagePrompt` (pre-rendered user-prompt body), `stageId` (canonical stage identifier), `stageBudget` (`{maxDurationSeconds, maxSubAgents, maxTokens}`), `stageLifecycle` (opaque map for the platform to resolve native target states on success/failure), `stageSourceEventId` (CloudEvent correlation id). The runner's `prompt.Builder.Build` now SHORT-CIRCUITS the embedded user-template renderer when `qw.StagePrompt` is non-empty: the platform-rendered prompt is used verbatim with a stage-context preamble (`<stage>development</stage>` / `<stageBudget …/>` / `<stageSourceEventId>…</stageSourceEventId>`) so the agent can self-identify which stage instance it is operating. Cardinal rule 1 holds: when `StagePrompt` is empty the renderer falls back to the legacy `PromptContext` / `Body` / per-work-type-template path (development / qa / research). New env vars `AGENTFACTORY_STAGE_ID` / `AGENTFACTORY_STAGE_MAX_*` surface the stage context to spawned sub-agents. Each `runner.Run` logs one `[runner-stage] sid=… stageId=… mode=stage|legacy` line for grep-driven correlation.

- **Sub-agent budget enforcement at runtime** — New `runner/budget.go` package adds a per-session `BudgetEnforcer` that tracks wall-clock, Task tool invocations, and token usage against the queue payload's `stageBudget`. Wall-clock enforcement uses a `context.WithDeadline` derived from the run start; Task counting matches `Task` and `*__Task` (MCP-namespaced) tool names case-insensitively; token counting sums `InputTokens + OutputTokens` from every per-turn `ResultEvent.Cost` (and the terminal one). On any cap breach the runner: cleanly stops the provider, classifies the failure as `FailureBudgetExceeded` (new `runner/failure.go` constant via `budget.go`), records the breach reason (`max-duration-seconds` / `max-sub-agents` / `max-tokens`) on `Result.BudgetReport`, and posts WORK_RESULT with the breach detail. `BudgetReport` is non-nil on every Run (with `.Enforced=false` for legacy work) so dashboards can distinguish "no budget" from "budget OK". Disabled-enforcer (legacy `agent.dispatch_to_queue` path with no `stageBudget`) is a no-op; cardinal rule 1 holds.

- **Runtime-token refresh probe** — Closes the 5-min `401 → re-register → 404` cycle described. The daemon's `OnReregister` callback (used by both `HeartbeatService` and `PollService`) now routes through new `daemon/runtime_token.go::RefreshRuntimeToken` which **probes `POST /api/workers/<id>/refresh-token` first** with the registration token — preserving the workerId — and only falls back to the existing full `Register(ForceReregister=true)` call (which mints a new workerId, the original bug) when the platform returns 404 / 405 (endpoint not deployed). Once the platform-side companion ships, the daemon picks up the refresh path automatically with no further changes. New `[runtime-token] event=… workerId=… reason=…` structured log line attests which path was taken on every cycle (event values: `401` / `auth-failure-detected` / `refresh` / `refresh.unavailable` / `refresh.error` / `reregister` / `reregister.error`). 401 classification now distinguishes the platform's specific `Runtime token expired` message (`reason=runtime-token-expired`) from generic 401 (`reason=unauthorized`) and 404 (`reason=worker-not-found`) so operators see at a glance which trigger fired the cycle. `RegistrationTokenSwapped=true` flag on the refresh result surfaces when re-register burned the workerId — the operationally noisy signal originally documented.

- **`daemon.Version` bumped to `0.5.5`** — bundles all three above; reported in registration / status payloads.

### Features (v0.5.4)

- **Runner WORK_RESULT → Linear state-transition wiring** — The Go runner now closes the Wave 6 Phase F.2.5 gap that left dev sessions in `Backlog` after a passing implementation. New `runner/sdlc.go` ports the `WORK_TYPE_COMPLETE_STATUS` / `WORK_TYPE_FAIL_STATUS` tables from `donmai-libraries/packages/linear/src/types.ts` and the post-session decision tree from `packages/core/src/orchestrator/event-processor.ts:300-450`. New `runner/contracts.go` ports the per-workType completion contract; development / inflight / coordination / inflight-coordination now require a `WORK_RESULT:passed|failed` marker. New `runner/post_session.go` implements step 11b of the run loop — parses the marker, resolves the target Linear status, and POSTs `updateIssueStatus` to the platform's `/api/issue-tracker-proxy` endpoint via the worker bearer token. Unknown markers post a diagnostic comment instead of stalling the issue. Failures surface as `Result.PostSessionWarnings` + `Result.LinearStatusTransition` (best-effort; a failed transition does NOT change the session's terminal status). Acceptance work continues to defer to the merge worker when a merge-queue adapter is configured (`shouldDeferAcceptanceTransition`). The development prompt template now includes the WORK_RESULT marker requirement so agents emit it on every dev session.
- **Result poster gains `UpdateIssueStatus` + `CreateIssueComment`** — `result/issue_status.go` exposes the platform's issue-tracker-proxy via two thin methods on `Poster`. Same retry/backoff/permanent-vs-transient classification as the existing `Post` path; reuses the worker bearer token and platform URL the runner already has (no new credential surface).

### Features

- **Daemon registers against the real platform** — `daemon/registration.go` and `daemon/heartbeat.go` now target the platform's `POST /api/workers/register` and `POST /api/workers/<id>/heartbeat` endpoints (was: non-existent `/v1/daemon/register` and `/v1/daemon/heartbeat`). Registration token is sent in `Authorization: Bearer`, not in the body. Wire shape: request `{hostname, capacity, version, projects?}`; response `{workerId, heartbeatInterval (ms), pollInterval (ms), runtimeToken, runtimeTokenExpiresAt}`. Heartbeat body is `{activeCount, load?}`. Stub-vs-real switch now accepts both `rsp_live_*` (legacy) and `rsk_live_*` prefixes. Runtime-token expiry (1h TTL, no refresh endpoint) is handled by re-register-on-401/404 with credential swap inside `HeartbeatService`.
- **Daemon version bumped to `0.4.0-dev`** — replaces `0.3.10-sidecar` reported by the bash heartbeat shim that shipped for the 2026-05-01 demo.

### Fixes (v0.5.3 hotfix bucket)

- **Runner heartbeat sends Linear `issueId`, not empty `IssueLockID`** — `runner/loop.go` now constructs `heartbeat.Config{IssueID: qw.IssueID, ...}` instead of sourcing it from `qw.IssueLockID` — a field the platform's poll response never populated, so the runner's `/api/sessions/<id>/lock-refresh` body was always `{"workerId":"...","issueId":""}` and the platform handler returned `400 "workerId and issueId are required"`. Result: 100% of v0.5.0+ heartbeats failed; sessions tripped `LostOwnership` after 3 strikes (~90s on the default 30s interval) on every real run. v0.5.1's child-output capture is what made the failure visible in `daemon-error.log`. Removed the unused `IssueLockID` wire field from `runner.QueuedWork`, `daemon.PollWorkItem`, `daemon.SessionDetail`, and the daemon→runner copy in `afcli/agent_run.go` — there is no separate "lock id" concept on the platform; `issue:lock:{linearIssueId}` is the canonical key. New `TestRunLoop_HeartbeatBodyIncludesIssueID` regression captures the bug.

### Fixes (v0.5.2 hotfix bucket)

- **Daemon resolves `projectName` to repository URL via allowlist** — When the platform's poll response carries a `projectName` slug (e.g. `"smoke-alpha"`) with no separate repository field — the canonical wire shape per the live Redis QueuedWork — the daemon's `pollItemToSessionDetail` / `pollItemToSessionSpec` now look up the matching `daemon.yaml` allowlist entry and substitute `p.Repository` (the GitHub URL) into `SessionDetail.repository`. The runner uses this URL for `git clone`. Before this fix the slug was forwarded unchanged, producing the v0.5.1 failure mode `runner.Run: git clone: exit status 128 (fatal: repository 'smoke-alpha' does not exist)`. Match logic mirrors `WorkerSpawner.findProjectLocked` — by `id`, `repository`, or URL-suffix. When no allowlist entry matches, the daemon falls back to whatever was on the wire and emits a Warn log so operators see the misconfiguration.

### Fixes (v0.5.1 hotfix bucket)

- **Spawn child stdout/stderr default to slog** — `daemon.New` now installs default `StdoutPrefixWriter` / `StderrPrefixWriter` on the spawner that emit one slog record per child line: stdout → INFO, stderr → WARN, both tagged with `sessionID` and `stream` attributes and prefixed `[child stdout|stderr sessionID=<id>]` in the message. v0.5.0 dropped child output to `io.Discard` by default, leaving operators flying blind between `runner.Run()` start and a `status=failed` post. Callers passing their own writers via `SpawnerOptions` retain priority.
- **`af agent run` provider probe failures are visible** — Every provider construction or registration failure now logs at WARN with `provider=<name>` and `err=<...>`. If every probe fails, an ERROR record fires (`no providers available`) so the misconfiguration surfaces instead of silently producing a session that fails resolution at runtime.
- **Default plist + systemd PATH includes `~/.local/bin`** — Both the macOS launchd plist (`installer/launchd`) and Linux systemd unit (`installer/systemd`) now prepend the invoking user's `~/.local/bin` to PATH so user-local installs of provider CLIs (e.g. the upstream `claude` curl|sh installer) are visible to the daemon. Resolves at install time from `os.UserHomeDir()` (or `SUDO_USER` for system-scope systemd units).

---

## v0.3.0 — 2026-04-30

### Features

- **Public `installer/` package — launchd + systemd in-process** — Port of the legacy TS daemon installers to Go. `installer/launchd/` and `installer/systemd/` generate plist/unit files that register `<host-binary> daemon run` (subcommand pattern, single-binary OSS UX). Public package importable by downstream binaries (`rensei`); replaces the previous shell-out to a Node `rensei-daemon` binary.
- **Public `daemon/` package — full HTTP server + lifecycle ops** — Port of the legacy TS daemon runtime (~1.6K LOC across registration, heartbeat, worker-spawner, auto-update, config, setup-wizard, types). 14 HTTP endpoints (status, stats, pause, resume, stop, drain, update, capacity, pool/stats, pool/evict, sessions, heartbeat, doctor, healthz). Includes drain semantics, JWT-derived tenancy, and TTY-aware setup wizard. Importable by downstream binaries.
- **`af daemon run` subcommand** — Long-running daemon entry point on port 7734; replaces the deprecated `@renseiai/daemon` Node package as the canonical service binary. Inherited by `rensei daemon run` via `afcli.RegisterCommands`.
- **`af daemon install / uninstall / doctor` rewired in-process** — Calls into the new Go installer rather than `exec.Command("rensei-daemon", …)`. No Node.js dependency on the install path.

### Chores

- **Acceptance discipline: binary-distribution gate** — Hard Rule 7 added to `migration-coordinator.yaml`: any "wire / install / register a binary" issue requires fresh-machine smoke verification at Acceptance, not just CI green.

---

## v0.2.2 — 2026-04-30

### Features

- **`af daemon install / uninstall / doctor` wiring** — OSS mirror of the daemon shell-out work: `exec.Command` calls into the underlying `rensei-daemon` (or equivalent) binary, forwarding stdin/stdout/stderr and passthrough flags.
- **`af logs analyze`** — `af-analyze-logs` ported to Go; full pattern catalog parity with the legacy TS implementation.
- **`af linear`** — `af-linear` CLI ported to Go; covers issue CRUD, comments, sub-issues, relations, and deployment checks.
- **`af orchestrator`** — `af-orchestrator` ported to Go.
- **`af admin {cleanup, queue, merge-queue}`** — Admin commands ported to Go from the legacy TS CLI.
- **`af code` and `af arch`** — Shell-out bridges that compose with the existing `pnpm af-code` / `pnpm af-arch` toolchains, completing Phase D parity.

### Chores

- **README authored** — Full README with command surface map.
- **RELEASING.md and CHANGELOG.md established** — Tag-driven GoReleaser release flow documented; this changelog established.

---

## v0.2.1 — 2026-04-29

### Chores

- **CI: drop Windows target** — Remove Windows from goreleaser cross-compile matrix; the binary only targets darwin and linux.

---

## v0.2.0 — 2026-04 (cycle 2)

### Features

- **`af governor start` in-process** — Governor scan/dispatch loop runs inside the binary; no longer shells out to an external `agentfactory` binary. Includes PID file, daemonize, and signal-handler primitives.
- **Linear GraphQL client** — Internal Linear client for governor scan loop, porting the TypeScript reference implementation to Go.
- **Redis queue client** — Internal Redis client wrapper for governor dispatch.
- **`af daemon` command tree** — 12 subcommands covering daemon install/uninstall/start/stop/status/doctor and pool management.
- **`af project` commands** — `af project allow`, `project credentials`, `project list`, `project remove`.
- **`afclient` types for Machine/Daemon/Pool/Workarea/Sandbox/Kit** — Expanded API type surface for downstream consumers.
- **Dashboard SandboxProvider column + filter** — Dashboard now shows and filters by sandbox provider.
- **`RegisterRequest.CapabilitiesTyped`** — Typed capabilities field added to the worker registration protocol.
- **`af admin` commands** — `af admin cleanup`, `admin queue`, `admin merge-queue` ported to Go from TypeScript CLI.
- **`af logs analyze`** — `af-analyze-logs` ported to Go.
- **`af linear` commands** — Full `af-linear` CLI ported to Go.
- **`af code` and `af arch`** — `af-code` and `af-arch` shell-out bridges ported to Go (Phase D parity).
- **`af orchestrator`** — `af-orchestrator` command ported to Go.
- **tui-components v0.2.0 Theme migration** — Migrated to the updated `Theme` struct.

### Fixes

- **gocritic / staticcheck lint cleanup** — Resolve `ifElseChain → switch`, `deprecatedComment`, `S1011` across new packages.

---

## v0.1.3 — 2026-02

_Earlier cycle-1 releases. See git log for full history._

### Features

- Initial `af dashboard` TUI with fleet status view
- `af status` inline output
- `af agent list / status / stop / chat / reconnect`
- `af fleet` subcommands
- `af queue` subcommands
- Worker protocol: register, poll, heartbeat

---

## v0.1.0 — 2026-01

### Features

- **Initial release** — `af` binary scaffolded with Cobra CLI framework, Bubble Tea TUI, and `afclient` API client. Covers `dashboard`, `status`, and `agent` commands against the AgentFactory coordinator API.
- **Public library surface** — `afclient`, `afcli`, and `worker` packages are importable by downstream consumers (e.g., `rensei-tui`).
- **Cross-platform builds** — darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 via goreleaser.
