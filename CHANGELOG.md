# Changelog

All notable changes to the `donmai` binary are documented here.

Format: `## vX.Y.Z — YYYY-MM-DD` with subsections `Features`, `Fixes`, `Chores`. Unreleased work goes under `## [Unreleased]`.

---

## [Unreleased]

### Fixes

- **KG extraction no longer boots an agent to produce one JSON object.** The
  fleet's constrained triple extraction ran on the harness's AGENTIC invocation
  — `claude -p --output-format stream-json --verbose
  --dangerously-skip-permissions --add-dir <cwd> --permission-mode
  bypassPermissions --append-system-prompt` — which loads the full built-in tool
  set, starts every MCP server the host has configured, discovers project memory
  up from the working directory, and keeps the agent's own system prompt with
  the extraction instructions merely appended to it. `--max-turns 1` capped the
  loop; it did not avoid paying to stand it up. In the fleet that cold start
  exceeded the 120-second per-observation deadline on EVERY observation: each
  batch ended `succeeded=0 failed=N` with `emit context: context deadline
  exceeded`, and no graph nodes were ever written.

  The claude harness now implements the one-shot completion lane directly
  (`agent.OneShotProvider`), and the emitter routes through `agent.Complete` —
  the strategy resolver — instead of calling `agent.SpawnComplete`, the
  agent-harness projection, by hand. A one-shot is now one process, one
  completion: `claude -p --output-format json --max-turns 1 --strict-mcp-config
  --no-session-persistence --system-prompt <text> --tools ""`, run in a
  temporary directory. No tools (so no agent step is reachable), no MCP servers,
  no project memory, and the caller's text REPLACES the agent system prompt
  rather than appending to it. Measured end to end against the real CLI: **~7–8s
  per observation**, versus a blown 120s deadline before.

  Three details are load-bearing beyond the speed:

  - **The timeout was not the bug and was not raised.** 120s stays, now as a
    ceiling for a stuck process rather than a budget the happy path approaches.
  - **No new credential requirement.** The lane passes no `--bare` (which would
    force an API key and refuse to read a login) and no `--setting-sources`
    override, so a host authenticated by its own subscription — the reason
    extraction runs on the fleet at all — keeps working unchanged.
  - **Success is read from `is_error`, nothing else.** A completion against a
    nonexistent model exits 0 with `subtype:"success"` and `is_error:true`; a
    reader trusting the exit code or the subtype would hand the caller an
    apology sentence as a successful extraction.

- **A malformed extraction emit now gets one bounded repair retry instead of
  failing straight to a hole in the graph.** An emit the parser cannot read is
  re-asked ONCE, with the parse error and the offending output quoted back —
  showing a model its own output is what makes the second attempt converge. A
  second failure is terminal for that observation and carries BOTH attempts'
  parse errors into the posted result, so the reason travels with the count. The
  retry is bounded at one on purpose: a model failing twice is failing
  structurally, and looping would spend the whole batch's budget on one
  observation. A first-attempt EMIT error (provider fault, deadline) is not
  retried at all — a repair retry fixes output shape, not transport.

- **A dispatched codex interactive session no longer stops on approval
  prompts.** Seeding startup trust got the session past its opening modals; it
  then parked on the next one. Three review classes were still live, and a
  session with nobody at the keyboard cannot answer any of them: the command
  approval ("Would you like to run the following command?"), the sandbox's own
  escalation review for a command that touches the network or writes outside
  the workspace, and one MCP review per distinct tool name ("Allow the
  `<server>` MCP server to run tool `"<tool>"`?"). The second is the one that
  cost the most to find — it survives switching the session to full access from
  inside the TUI, because the SANDBOX, not the approval policy, is what raises
  it.

  The interactive launch now seeds all three as process-local `--config`
  overrides, alongside the existing trust seeds and on the same terms — nothing
  is read from or written to the operator's codex home:

  - `approval_policy = "never"`,
  - `sandbox_mode = "danger-full-access"`,
  - `mcp_servers."<name>".default_tools_approval_mode = "approve"`, for the
    servers this spawn itself requested and no others.

  Config keys rather than the equivalent `--yolo` /
  `--dangerously-bypass-approvals-and-sandbox` flags, for the reason the hooks
  seed avoids `--disable`: a renamed flag would turn into a harness that cannot
  spawn at all. Note `"auto"` is not the auto-approving variant of
  `default_tools_approval_mode` — a session configured with it still stopped on
  the tool review; `"approve"` is the one that pre-answers it.

  Isolation for a dispatched session belongs to the environment the platform
  provisioned around it, not to a second sandbox inside the CLI that only knows
  how to ask a human. `DONMAI_CODEX_APPROVALS=inherit` restores codex's own
  prompting for an attended terminal; an unrecognized value fails the spawn
  rather than silently guessing. The headless app-server lane, whose approvals
  ride the JSON-RPC bridge, is untouched. Measured against codex-cli 0.146.0
  under a PTY with a fresh codex home.

## v0.57.8 — 2026-08-09

### Fixes

- **The kg-extraction executor now speaks contract v2, so staged work actually
  runs.** The coordinator has staged `contractVersion: 2` items since
  2026-06-13; this worker still declared 1 and refused every item at the version
  gate before doing any work. Nothing noticed for two months because no worker
  advertised the capability, so no item was ever handed over — the first host to
  advertise it rejected every batch it claimed. The worker now decodes the real
  v2 shape end to end:

  - edges carry the optional `confidence` label and discrete `confidenceScore`
    through to the posted result instead of dropping them on unmarshal (both are
    passthrough — the coordinator owns that vocabulary and its defaulting);
  - the `Convention` and `Deviation` node types are accepted, so nodes the emit
    schema asks the model for are no longer silently dropped by the worker's
    closed-set validation;
  - `semantically_similar_to` needs no change here — the relation name is a free
    string, and the emit prompt and JSON-Schema ride on the wire.

  A real work item, generated from the coordinator's own dispatcher, is checked
  in as `kgextract/testdata/platform_v2_work_item.json` and decoded by the test
  suite, so the next contract move fails in CI rather than in one host's log.

- **A refused kg-extraction item now becomes a visible failed result instead of
  disappearing.** On a contract-version rejection the executor returned an error
  and POSTed nothing, so the coordinator's row for that batch stayed `pending`
  forever while its claim key suppressed every re-stage of the same stable batch
  id for an hour — the failure was visible only in the host's log file. The
  executor now POSTs a terminal `status:"error"` result before returning, so the
  refusal is reported where it can be seen. The cross-tenant org-claim rejection
  deliberately still posts nothing: that guard fires precisely because the item's
  result-auth token does not claim the item's org, so it must not be used.

## v0.57.7 — 2026-08-09

### Features

- **The resident daemon can now claim and run kg-extraction work.** Until now
  only the standalone `donmai worker start` process advertised the
  kg-extraction capability and ran its executor; the daemon poll path did
  neither, so an operator running the daemon had no way to serve that lane. The
  daemon now decodes the `kgExtractWork[]` lane, executes each claimed item off
  the poll goroutine (bounded concurrency, joined on shutdown), and advertises
  the capability at registration.

  Advertisement and execution are welded together: the capability tag and the
  handler come from one value (`kgextract.NewLane`), `NewPollService` fills a
  nil handler with the default lane, and the registration tag list is derived
  from the lanes the poll service runs. A worker can no longer advertise a lane
  it cannot execute — which would be worse than silence, since claiming pops
  the item off the coordinator's queue and no other worker would ever see it.

### Fixes

- **An interactive session no longer reports its exit line as a summary.**
  `Result.Summary` carried a synthesized "interactive session ended (exit N)"
  for every interactive session — a lifecycle fact in a field consumers read as
  the agent's account of the work, which downstream readers then had to
  recognise and discard. The exit detail still travels on `Result.Error`, the
  session-ended activity event, and the log line; a session with nothing
  substantive to say now leaves `Summary` empty.
- **A codex interactive session no longer parks on startup reviews nobody is
  there to answer.** The codex TUI holds modal reviews before it reads a
  keystroke — "Do you trust the contents of this directory?" for a workspace
  it has not seen, and "N hooks are new or changed" for hooks the checked-out
  repo ships — and neither times out, so an unattended session sat on them
  until its wall clock killed it, having produced nothing that explained why.
  The interactive launch now seeds the answers the platform is entitled to
  give as process-local `--config` overrides: the session workspace is
  pre-trusted (both the given path and its symlink-resolved form, since codex
  matches a project entry by exact path), and the hooks feature is turned off
  for the process. Hooks are deliberately NOT marked trusted — they are repo
  content, not platform-provisioned, and trusting one grants command execution
  outside the sandbox — so this takes codex's own third option, continue
  without trusting, deterministically instead of by an unmade keystroke. Set
  `DONMAI_CODEX_HOOKS=inherit` to restore codex's hook handling for an
  attended terminal. Nothing writes to the operator's codex home, and the
  seeded `projects` table shadows rather than widens ambient trust, so a
  session's trusted set is exactly the workspace it was given. A workspace
  that cannot be resolved to an absolute path now fails the spawn with an
  error naming the missing trust rather than hanging on the review. Requested
  MCP servers need no seed (codex starts configured servers with no approval
  step), and the headless app-server lane is deliberately untouched: it has no
  modal to block on, and trusting its working directory would admit a
  project-level `.codex/config.toml` into the effective configuration the
  isolated `CODEX_HOME` boundary exists to keep exclusive.

## v0.57.6 — 2026-08-08

### Features

- **Every harness now DECLARES how a message reaches a session that is already
  running.** `agent.NoticeDelivery` names the mechanism per harness — `hook`,
  `mcp-rpc`, `http-session`, `acp`, `rpc-steer`, `resume-inject`,
  `in-box-loop`, `pty-notice`, or `none` — and every in-tree manifest answers,
  each verified against that harness's own CLI rather than assumed. The zero
  value is UNDECLARED, deliberately not `none`, so a new harness cannot inherit
  an answer by omission in either direction. A session that something upstream
  must be able to reach (`Spec.RequiresLiveNotice`) is now REFUSED at admission
  on a harness that declares `none` or declares nothing, with a typed
  `SpecAdmissionError`, instead of launching an agent nobody can reach whose
  messages a lower layer drops while reporting success. The PTY notice was
  retargeted onto the same axis: `TryWriteNotice` is permitted only where the
  harness declared `pty-notice`, acks fire when bytes reach the terminal rather
  than on buffer, and an un-placeable payload now dead-letters observably
  (rides back on the lock refresh with a reason token) instead of holding the
  single in-flight slot for the life of the session.
- **Messages can be delivered into a LIVE Claude turn, over the harness's
  declared `hook` channel.** Until now live delivery was implemented for
  exactly one harness — `shell`, via the PTY notice — and `shell` has no agent
  behind its terminal, so for the harness interactive sessions actually run,
  live delivery was dead by construction and every runtime inject aimed at it
  was dead-lettered as channel-not-driven. The harness now gets a private drop
  directory and a five-line POSIX hook installed via `--settings` as a JSON
  string, so nothing lands in a tree the agent could read, edit, or commit; the
  claim on a pending message is a rename, and that rename is itself the
  forced-continuation loop guard. Routing is keyed on the manifest's declared
  mechanism, never on a harness name.

  **What is and is not proven.** The transport is deterministic and was
  re-verified independently: `{"decision":"block"}` forces a continuation on
  the real CLI, the reason text enters the conversation, the re-fire carries
  `stop_hook_active=true`, and the loop guard suppresses it. What is NOT
  deterministic is what the agent then does with the delivered text — that is
  model behaviour, and an independent re-run of the live acceptance assertion
  passed 1 of 2. Treat this as a working channel, not as a guarantee that an
  injected instruction is acted on.

  Two further limits, stated rather than papered over. A Stop hook fires when a
  turn ENDS, so this reaches a session that is WORKING; a session idle at an
  empty prompt has already fired its last Stop and is reached by the durable
  mailbox instead. And the mailbox remains the floor — a message offered and
  never collected, or offered to a session that exposed no channel, dead-letters
  with an actionable reason, while one held when the session ends is left
  unacked for requeue. `agent.NoticeChannel.Consumed` answers from the
  recipient's own transcript record, so a hook that overruns its timeout (whose
  output the CLI discards silently) is never reported as delivered.
- **`agent/conformance` is now a runnable certification suite, not a 63-line
  seed.** A harness author can run their own adapter against it: it imports
  `agent` plus the standard library, needs no network and no credentials beyond
  the author's own harness binary. The pure event-sequence checks
  (`CheckSingleInit`, `CheckTerminalContract`, `CheckCompleteAssistantTexts`,
  and the `CheckEventContract` composite) still take a drained `[]agent.Event`
  and are unchanged for existing callers; on top sits `Run(ctx, Subject)
  -> Report`, which drives a live adapter and awards capability tiers.

  Tiers are earned, never declared, and honesty is mechanized rather than
  documented: a not-applicable result without a reason is rewritten into a
  failure, a not-applicable never earns its tier, and every report carries
  `Report.Unverified` naming the rows the suite has no check for — so a green
  report cannot be read as full certification. Driven-ness is asked PER CHANNEL
  against the seam that actually carries it, so a harness cannot be credited
  for a channel it does not drive: the `Handle.Inject` rail is evidence for
  `mcp-rpc`/`http-session`/`acp`/`rpc-steer`/`in-box-loop` only, `pty-notice`
  is proven through the same interactive seam the runner drives, and a
  declarable mechanism missing from the table is a failure rather than a
  default.
- **The runner prefers a session-scoped bearer for the platform MCP gateway.**
  That gateway's `Authorization` header is written ONCE into an MCP config file
  at spawn and nothing ever rewrites it — not the daemon's runtime-credential
  refresh, not the harness — so when the chosen bearer's lifetime is shorter
  than the session's, the harness's platform tools silently vanish mid-session
  with no error surfaced. The work item now carries optional `mcpAuthToken` /
  `mcpAuthTokenExpiresAt` fields (additive and `omitempty` on both
  `daemon.PollWorkItem` and `daemon.SessionDetail`; the daemon is a pure
  forwarder and never parses, validates, or logs the value), and the gateway
  header prefers that token when present.

  **What this version alone changes: nothing, unless the server you connect to
  stamps the field.** The worker bearer remains the fallback, that fallback is
  PERMANENT rather than a migration shim — it is what keeps a self-hosted
  platform that mints no session token working — and a platform that sends
  nothing gets byte-identical behaviour to v0.57.5. Equally, a downstream
  binary that embeds `donmai` only starts honouring the field once it ships a
  build embedding THIS version; until then its sessions keep receiving the
  worker bearer. The expiry hint is advisory only: the runner logs it once at
  spawn when a gateway is actually mounted and never branches on it. The token
  is used for the gateway header and nothing else — heartbeat, result-post,
  activity-post, and the session preflight fetch still use `AuthToken`.

### Fixes

- **Three harnesses could not spawn at all in a platform-connected session, and
  WHICH harnesses mount the implicit MCP gateway has changed.** The runner
  injects a per-session platform MCP gateway on the caller's behalf whenever
  platform credentials are present. Since the unified-admission work, an MCP
  entry a harness cannot deliver DENIES the spawn rather than being silently
  stripped — correct for a server the caller asked for, wrong for one the
  runner added by itself — and the gateway's exemption was written against a
  hardcoded harness NAME (`shell`). Every other harness whose tool/lifecycle
  profile declares no MCP delivery was therefore handed a gateway its own
  adapter then refused: `pi`, `ollama`, and `agy-cli` failed with `spawn
  failed: tool/lifecycle adaptation denied (delivery_unsupported,
  channel=mcp_server)`.

  The exemption is now the DECLARED `MCPDelivery` of the harness's mode-scoped
  tool/lifecycle profile, read off the live manifest — the same field the
  adapter consults to admit or deny the `mcp-servers` requirement, so the runner
  can no longer ask for a channel the adapter will refuse. Measured across the
  whole fleet in autonomous mode, the gateway is now emitted for **claude,
  codex, gemini, amp, opencode and stub**, and omitted for **agy-cli, ollama,
  pi and shell**. Read this as a change in gateway coverage, not only as a
  crash fix. Deliberately not over-corrected: an MCP server the CALLER
  requested — an agent-card entry, or the code-intel plugin — is still never
  filtered; it stays in the spec and denies loudly on a harness that cannot
  deliver it.

### Chores

- **`worker` and `fleet` are now marked deprecated, and `fleet scale` is
  gone.** Both legacy process-supervision command trees (only ever registered
  in the standalone binary, never by an embedder) now carry a Cobra
  `Deprecated` marker naming `host` as the replacement and a concrete removal
  version (`v0.59.0`) rather than an unfalsifiable "next release" promise.
  `fleet scale` — a subcommand that has only ever returned an error — is
  deleted outright rather than deprecated.
- **The host-watch dashboard now has an exported constructor,
  `afcli.NewHostWatchCmd`.** A composing downstream binary can construct it
  directly instead of relocating the alias-registered `fleet-watch` command
  off root, which previously dragged that command's `Hidden` and
  `Deprecated` fields along with it.
- **The hidden `daemon` and `fleet-watch` aliases are still present and still
  work.** Their removal remains scheduled for **v0.58.0** and this patch
  release does not perform it, because the precondition recorded at
  `hostAliasRemovalVersion` is still unmet: service units written by earlier
  builds invoke `<host-binary> daemon run`, a unit already on disk is only
  rewritten by re-running `host install`, and no release path yet forces or
  verifies that re-install. Deleting the aliases first would stop the service
  on every machine that has not re-installed.
- **The closed-source-reference guard is now the vendored `guard-b` linter.**
  It replaces `scripts/leak-guard.sh`, whose own self-test fixtures were
  literal banned strings (the file tripped its own rules and needed a
  whole-file allowlist entry). `scripts/guard-b-lint.sh` and its self-test are
  vendored byte-for-byte from the architecture corpus with a provenance header
  pinning the source commit, and `scripts/check-guard-b-vendor-drift.sh` fails
  CI if this copy drifts. The blocking scans are scoped to NEW content only
  (staged changes, a pull request's commits, the squash message, the push
  range); the full tracked-tree `--all` scan runs separately and NON-BLOCKING
  as `make guard-report`, because the rules surface a large pre-existing
  residue across dozens of files that predates the guard — 209 hits when it
  was vendored, 201 as of this release. `.guard-allowlist`'s header documents
  that residue and the path to curating it to zero.
- **Five closed-source references were curated out of this file's own
  history.** Because `--staged` scans whole staged files, `CHANGELOG.md`'s
  pre-guard residue failed `make guard` the moment the file was touched at
  all, so no release edit could pass the gate until they were dealt with. A
  closed control-plane endpoint path, a closed environment-variable name, and
  three references to a closed downstream repository are rewritten to describe
  the behaviour instead. None is allowlisted: an allowlist entry asserts the
  hit is legitimate, and these are exactly the content this repository must
  not carry.
- **Build-tag-gated test files can no longer rot unnoticed.** Six `_test.go`
  files sat behind `//go:build` tags no target ever supplied, which excludes
  them from `go build ./...` and `go test ./...` outright — not run, not
  compiled, not even syntax-checked. `internal/testregistration` now walks
  every `_test.go` for custom build tags and fails `make test` unless each tag
  appears in a literal `-tags` flag in the Makefile or a workflow (literal, not
  behind a variable, because the guard must read the text the toolchain really
  receives), and `make test-tagged` type-checks all six. `tparallel` is also
  enabled, catching a parent test whose `defer` teardown fires while a
  `t.Parallel()` subtest is still paused; its style half is excluded because it
  fires on correct tests and a guard people learn to scroll past stops
  guarding.
- **Stop-hook steering of an interactive Claude session is now measured rather
  than assumed.** A build-tagged PTY spike drives the same `ptyhost.Spawn` path
  interactive sessions use, with the negative control wired in as a peer test:
  an identical session whose hook returns nothing must produce no artefact and
  must still show the hook fired, so the green run is believable. It records
  that `decision:block` does force a continuation, that `stop_hook_active`
  flips true on the re-fire so the loop is guardable, that a per-hook `timeout`
  is enforced and the block discarded silently, that `--settings` accepts a
  JSON string and merges with the base sources, and that the workspace-trust
  modal still parks a session on an untrusted directory even under
  `--dangerously-skip-permissions`.

## v0.57.5 — 2026-08-07

### Fixes

- **A machine now has one stable identity.** The daemon resolves the
  `machineId` it registers with from an operator override, else a
  domain-separated hash of the OS-native machine identifier, else a value
  persisted in the machine-local state directory — never from the hostname.
  The hostname travels as a human-readable label only. A machine whose name
  resolves differently depending on the network it is attached to previously
  presented itself as a separate host per name form, splitting its capacity
  across duplicate registrations; it now presents one identity that survives
  renames, network changes, and reinstalls.
- **Concurrent worker registrations no longer evict one another.** A rejected
  heartbeat or poll (HTTP 401 or 404) now refreshes credentials and
  re-presents the EXISTING registration wherever the orchestrator still has
  it, instead of treating "worker not found" as proof the registration is gone
  and minting a replacement. A daemon serving several organizations had each
  lane's fresh registration retire the previous one, producing a new
  registration every heartbeat interval and stranding sessions owned by the
  retired one. Full re-registration is now a last resort and is rate-limited
  by a minimum interval; transport failures, timeouts, and 5xx responses are
  retried and never re-register.
- **Embedded command examples name the binary that is running.** Twelve leaf
  commands hardcoded a literal binary name in their `Example` blocks while
  their descriptions correctly used the templated name each factory is given,
  so a CLI embedding these commands printed copy-pasteable examples naming a
  binary its users do not have.

### Chores

- **A scheduler concurrency test no longer depends on wall-clock luck.** It
  inferred concurrency from whether two sleeps overlapped in real time and
  failed 20 of 50 isolated runs; it now synchronizes on a rendezvous the
  scheduler actually guarantees, and additionally asserts the in-flight bound
  and exactly-once attempt accounting. No production code changed.

---

## v0.57.4 — 2026-08-06

### Features

- **Harness admission is authoritative and fail closed.** Execution cells now
  name an exact supported harness before work begins, split raw Gemini and
  Ollama harness identities from provider endpoint selection, and pin OpenCode
  to the selected generic endpoint.
- **Harness adaptation follows the whole session lifecycle.** Tool, MCP,
  prompt, and policy configuration is refreshed truthfully across interactive
  and headless execution while preserving prompt safeguards and PTY command
  authority.

### Fixes

- **Codex and OpenCode configuration is isolated per owned session.** Managed
  configuration is created, refreshed, and removed without overwriting user
  state or another concurrent session.
- **Generated harness capability artifacts match the authoritative manifests.**
  Admission, selection, and published matrix data now describe the same
  supported harness contracts.

---

## v0.57.3 — 2026-08-05

### Fixes

- **Linear project discovery stays within the upstream query complexity
  budget.** Project catalogs now paginate in bounded batches while preserving
  complete catalog traversal and team membership data. Structured GraphQL
  errors returned with non-success HTTP statuses also retain their actionable
  server message.

---

## v0.57.2 — 2026-08-05

### Fixes

- **Reconnect snapshots no longer display terminal-title control payloads.**
  Donmai now filters OSC 0, 1, and 2 title controls before bytes enter the
  headless snapshot emulator, while preserving byte-exact live output,
  recordings, non-title OSC controls, terminal query responses, Unicode, and
  behavior across chunk boundaries.

---

## v0.57.1 — 2026-08-04

### Features

- **Keyless release signatures and build provenance for every shipped
  archive.** Release artifacts and `checksums.txt` now receive Sigstore
  keyless signatures with published certificates, while separate build
  attestations record the source commit and workflow that built each artifact.
- **Linear routing catalogs.** `linear list-teams` and `linear list-projects`
  provide deterministic, exhaustively paginated routing data, including stable
  membership rows for projects shared by multiple teams.

### Fixes

- **Blocker resolution fails closed.** Relation reads now reject malformed,
  incomplete, unknown, or truncated data instead of treating an issue as
  unblocked.
- **Linear label operations are scoped and safe.** Unknown requested labels
  stop create and update operations before mutation; team-scoped label catalogs
  include only applicable team and workspace labels; and `apply-label --create`
  creates an applicable team label, handles a concurrent duplicate safely, and
  uses additive label updates.
- **Interactive Claude sessions retain their composed system-prompt append,**
  while content-as-data safety instructions are applied after base or override
  prompt selection so a system override cannot replace them.
- **Linear issue listings paginate beyond one upstream page** while preserving
  output order and rejecting malformed, cyclic, oversized, or truncated page
  data.
- **Daemon control listeners reject non-loopback binds before opening a
  socket,** including through direct library use.
- **Release signing now operates on final artifacts deterministically.**
  Developer ID signing and notarization happen before archive and checksum
  creation, and signing certificates are registered for release publication.

### Chores

- Retired the obsolete AF-TUI and AgentFactory names from user-facing text,
  while preserving compatibility-only identifiers and historical references
  where they remain part of an existing contract.

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
  own GraphQL proxy route. All query/mutation strings and
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
  a JWT minted for a worker_id the platform no longer knows. A downstream
  embedder's `host install` already had this behaviour; the upstream `af daemon
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
- **`af provider` / `af kit` / `af workarea` / `af routing` Cobra command trees** — First-class top-level commands on the `af` binary, sourced from the new daemon HTTP surface. `provider list/show`, `kit list/show/install/enable/disable/verify/sources`, `workarea list/show/restore/diff`, `routing show/explain`. Each delegates to the local daemon at `127.0.0.1:7734` (overridable via the daemon-URL environment variable) and renders through the new `afview/` package.
- **New public package `afview/`** — Houses surface-specific composed renderers (`afview/provider`, `afview/kit`, `afview/workarea`, `afview/routing`). Joins `afclient`/`afcli`/`worker` as the fourth public package. Both binaries (af and rensei) import the same renderers; no forks. Plain-text fallbacks for each surface's list/detail views are what `rensei-smokes` pins against.
- **21 new `afclient` types** — Provider/Kit/Workarea/Routing wire types live in `afclient/{provider,kit,workarea,routing}_types.go` matching the daemon's `/api/daemon/*` namespace. Notable shapes: `ListProvidersResponse.PartialCoverage` flag (honest about agent-runtime-only coverage in this wave), `WorkareaSummary.Kind` discriminating active pool members vs on-disk archives, structured `WorkareaDiffEntry` with per-path SHA-256 hashes + size + mode + symlink target, `RoutingDecision` + `RoutingTraceStep` for per-session decision explain.
- **8 new exported `afcli` factories** — `NewProviderCmd`, `NewKitCmd`, `NewWorkareaCmd`, `NewRoutingCmd` and their backing private helpers, exported via `afcli/exports.go` so downstream binaries can graft the canonical command trees under their own parent commands. The rensei binary uses these to expose `rensei host {provider,kit,workarea}` and `rensei routing` without forking.
- **Workarea diff streaming** — `GET /api/daemon/workareas/<idA>/diff/<idB>` switches between a single JSON envelope and NDJSON streaming based on entry count vs the daemon's configured threshold (default 1000, `daemon.yaml` key `workarea.diffStreamingThreshold`). Both shapes carry the same `WorkareaDiffEntry` per-path payload; consumers discriminate via `Content-Type`. The afclient method consumes both into a unified `WorkareaDiffResult`.
- **Workarea archive restore** — `POST /api/daemon/workareas/<archiveID>/restore` materialises an archive into a fresh active pool member with a NEW id (archives are immutable). Conflicts on `IntoSessionID` → 409; pool saturation → 503 with Retry-After; corrupted archive → 400.
- **Daemon-side Kit registry** — Minimal in-process Kit registry that scans `~/.rensei/kits/*.kit.toml` per `005-kit-manifest-spec.md`'s § Daemon kit registry. Multi-path support via `daemon.yaml` `kit.scanPaths` override. Malformed manifests log warnings and are excluded; empty registry returns `{kits: []}` with HTTP 200. Enable/disable state persisted to `<scanPath>/.state.json`.
- **Daemon-side routing trace store** — In-memory ring buffer (default 50) of recent routing decisions plus per-session lookup keyed on `SessionID` for `routing explain`. Hookable `RecordDecision` seam for the future cross-provider scheduler. Default Thompson-Sampling weights `{Cost: 0.7, Latency: 0.3}` per `004-sandbox-capability-matrix.md`.
- **`runner.ProviderView` adapter** — Widens `runner.Registry` for HTTP surface use. Surfaces the in-process AgentRuntime registry as the `agent-runtime` Provider Family entries; documented `partialCoverage: true` flag honestly reports the other seven Provider Families return empty until per-family registries land.

### Fixes

- **None specific to this release.** Wave 9 was a structural refactor; earlier observability bug (`auditClientFromConfig` delegating to the daemon-targeted client) was caught in a downstream embedder's parallel cleanup and is fixed there.

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
- **Public library surface** — `afclient`, `afcli`, and `worker` packages are importable by downstream consumers.
- **Cross-platform builds** — darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 via goreleaser.
