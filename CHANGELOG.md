# Changelog

All notable changes to the `donmai` binary are documented here.

Format: `## vX.Y.Z — YYYY-MM-DD` with subsections `Features`, `Fixes`, `Chores`. Unreleased work goes under `## [Unreleased]`.

---

## v0.71.0 — 2026-08-27

### Breaking

- **`SessionShimConfig.AcquireRecoveryScopes` receives the founding primary
  receipt.** The callback is now
  `func(ctx context.Context, attestation SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt) ([]SessionShimScopeCredentialReceipt, error)`.
  `primary` is the primary scope's receipt exactly as the founding round trip
  resolved it (`Scope`, `WorkerHostID`, `AdoptionRevision`): the registration
  on the inline path, the declaring refresh on a deferred install. It is
  non-secret; embedders retain it for their own host-authority lookups instead
  of re-deriving it. Embedders that set the hook must add the third parameter.

### Fixes

- **The founding declaration resolves session-shim host authority, and nothing
  else may.** A deferred composition install demanded proof-v2 readiness inside
  the very refresh that first presented its attestation, and an embedder whose
  readiness resolver answers for the primary host could not answer before that
  refresh's receipt existed. The only other way to learn the host id was to
  present the attestation outside the credential refresher's lock, which raced
  the running lanes: the control plane saw the posture flip-flop, answered an
  attestation conflict, and revoked the losing credential. The readiness check
  is now deferred for the founding refresh alone — it runs once the primary
  receipt is retained and handed to the embedder — and every other refresh
  checks readiness before adopting, exactly as before.
- **An install abandoned mid-window no longer leaves the daemon refusing all
  work.** While a composition install is pending, the daemon already presents
  the composed attestation, so a poll tick or heartbeat projection resolve
  could consult the embedder's readiness resolver before the founding receipt
  existed and withdraw admission (spawner paused, state recovering, every
  `AcceptWork` refused). A successful install self-heals through the
  acknowledged heartbeat; a failed one rolled back the identity but left the
  withdrawal standing. The rollback now restores the admission its own window
  closed.
- **An install that fails after the control plane accepted its attestation
  withdraws it.** The rollback re-declares the stand-down as soon as the
  declaring round trip was accepted, not only once the receipts and readiness
  behind it had also succeeded. A refresher left presenting a composition for a
  daemon that is standing down was the same flip-flop from the other side.

---

## v0.70.0 — 2026-08-27

### Breaking

- **A composed session-shim configuration may now be installed after `New`.**
  `Daemon.InstallSessionShimComposition` and `Server.StartBeforeDaemon` are new
  public surface, and `SessionShimOwnsSession` reports ownership through the
  live generation rather than a captured answer. Embedders that still pass
  `Options.SessionShim` to `New` keep today's semantics exactly — the inline
  path is unchanged, including its startup safety checks.

### Features

- **Daemon readiness no longer waits on the durable-session composition.**
  Readiness used to sit behind work whose duration the host does not control: a
  registration round trip, and a startup adoption pass whose failure aborted
  `Start` outright. On a healthy host that is a second; on a slow or wedged
  control plane it is a timeout, and every caller polling for ready — service
  installers most of all — inherited the wait.

  The control listener now answers before the daemon has started, so "still
  coming up" and "nothing is there" stop being indistinguishable, and a
  composition can be installed on a daemon that is already serving. The
  sequence is install → declare → adopt → announce: the declaration comes
  first because host authority resolves from credential receipts that do not
  exist until a round trip has happened, so adopting before declaring cannot
  work.

  Nothing announces the shim as active before the adoption pass completes, and
  new sessions are not handed to a shim while a composition is still pending —
  a host that came up fast must not claim to have recovered work it has not
  counted yet.

### Fixes

- **A late-published adoption revision no longer strands the claim lane.**
  Publishing an adoption revision raises an admission fence that only an exact
  acknowledged heartbeat clears. The reopening switch handled a daemon that was
  recovering, paused, or draining, but not one that was already running —
  every prior path reached the fence through `recovering`, so the case had
  never existed. A composition installed after startup never leaves `running`,
  so the daemon came up faster and then silently stopped claiming work.

- **Declaring a session-shim attestation can no longer mint a competing worker
  identity.** The declaration went through the runtime-token refresh, which
  falls back to a full re-registration on a missing endpoint — the documented
  mutual-eviction shape — to deliver an optional capability. It now uses a
  represent-only path with no such fallback.

---

## v0.69.2 — 2026-08-27

### Fixes

- **Successful `agent stop --json` responses now retain their durable receipt.**
  The optional nested receipt preserves its version, kind, storage-session
  identity, mutation identity, intent revision, terminal disposition, and
  idempotent-replay bit while the top-level session id remains the public id.
  Older servers that omit the receipt remain compatible.

  Receipt fields use the same bounded atom and secret-rejection policy as typed
  conflict receipts. Malformed or secret-bearing success content fails decode
  without reflecting the raw response.

  This is client projection only; downstream release and live idempotency
  acceptance remain separate.

---

## v0.69.1 — 2026-08-27

### Fixes

- **`agent stop` now preserves typed conflict receipts.** JSON callers receive
  the bounded stop-refusal fields before the command returns its nonzero exit,
  including retry and reconciliation metadata. Existing callers can still use
  `errors.Is(err, ErrConflict)`, while human output names the disposition
  instead of reducing every HTTP 409 response to a generic conflict.

  Error bodies are size-limited, parsed into a strict allowlist, and rejected
  when malformed, non-JSON, or secret-bearing; arbitrary response content is
  never retained or echoed.

---

## v0.69.0 — 2026-08-26

### Breaking

- **`SessionShimHostAttestation.Supported` is now `SessionShimSupportState`,
  not `bool`.** Embedders constructing the tuple move `Supported: true` to
  `Supported: daemon.SessionShimSupported`, and read it back through
  `Supports()` / `StandsDown()` rather than comparing to a boolean. The minor
  version moves rather than the patch because this does not compile for a
  downstream embedder without that edit.

  The type exists because a host needs to say three different things, and a
  `bool` can only say two. Absent (the zero value) stays absent: it marshals to
  no key at all, so a daemon that never had a session shim sends byte-identical
  registration requests. The new third state is an explicit stand-down.

### Features

- **A host can declare that it has no session shim.** A daemon whose durable-
  session composition degrades used to register in silence, which a control
  plane that had already recorded the host as shim-enabled could not
  distinguish from a daemon too old to speak the wire. It answered the
  registration with a conflict, `daemon start` failed, the service manager
  restarted it, and the control port never bound again — a permanent crash
  loop, entered by the first degraded boot after the feature had ever worked.

  `Options.SessionShimStandDown` makes the absence explicit on both the primary
  and per-organization registration lanes, and drops a cached credential minted
  under shim support so the declaration is not bottled up until that cache
  expires.

---

## v0.68.16 — 2026-08-26

### Features

- **Public session clients now preserve the work-before-working boundary.**
  `starting` is a first-class active status for session lists and watch
  surfaces, without being rendered as `working`. `timed_out` is a first-class
  terminal status, so stream and detail watchers stop instead of polling
  forever. Human and JSON output retain both values, the detail timeline names
  them distinctly, and unknown future status strings still decode without
  being discarded.

---

## v0.68.15 — 2026-08-26

### Features

- **A lineage this daemon can no longer see is now reportable.** There are two
  facts a daemon can hold about a shim that is gone, and only one of them is a
  tombstone. A shim that exits cleanly writes its own terminal record — exit
  code, last sequence, positive proof it reaped its harness process group. A
  shim that is SIGKILLed, or whose registry record is removed underneath it,
  writes nothing, and the lineage it left behind could not be reported at all:
  not tombstoned, because manufacturing a tombstone would forge the reap proof a
  claim release depends on, and not dropped either, because a complete adoption
  batch that omits a lineage the composer still holds is refused — along with
  every batch after it. `SessionShimAbsentAttestation` reports only what was
  checked: the recorded `(pid, start time)` pair is proven not to be running,
  and the registry record is gone. It is strictly weaker than a tombstone and
  the report path enforces that — evidence carrying both proofs is refused, and
  a partial attestation is refused outright rather than downgraded. The
  acceptance quarantine clear now attests before it forgets a lineage, and keeps
  the quarantine if that report is refused.

### Fixes

- **Quarantining a session no longer takes its host out of service.** The
  platform compares a heartbeat's quarantine set against the snapshot the last
  adoption-batch commit stored, byte for byte, and demotes a host whose beat
  disagrees. Two of the four places that change this daemon's quarantine set
  published the result and two did not — including the ordinary path where an
  adopted controller's stream ends without a terminal observation. A dropped
  socket therefore added a quarantined session the platform had never been told
  about, every following heartbeat was refused, and the host sat draining with
  its claims disabled until that shim happened to be tombstoned. The projection
  is now republished wherever it changes, and a test fails if a new call site
  changes it without publishing. The republish also retains its own receipt:
  committing a batch advances the host's adoption revision, the heartbeat
  attests the revision this daemon believes it is at, and discarding the receipt
  would trade one divergence for another.

---

## v0.68.14 — 2026-08-25

### Fixes

- **A busy session no longer loses its own control channel.** Selected-v3
  acknowledged every forwarded host frame through an fsync-backed round trip to
  the shim, on the same goroutine that drained the stream — capping the daemon
  at roughly one frame per fsync. The controller's priority event queue is
  bounded and fail-closed by design, so a consumer that fell behind did not slow
  the stream down: the socket reader dropped the connection. One dense terminal
  redraw was enough. The daemon then kept publishing the session as adopted
  against a socket it could no longer write to, so every later input, resize, and
  acknowledgement failed with "use of closed network connection" while the
  harness stayed alive and unreachable. A session adopted at startup hit this
  first, because it attaches to a harness that is already producing. The durable
  cursor is now persisted off the frame path: it still advances only on the
  shim's exact receipt, and a coalesced acknowledgement replays more after a
  restart, never less.
- **Dropping an adopted controller releases ownership.** A consumer that closed
  its own connection — a durable carrier refusing a frame, a sequence that did
  not advance — returned without releasing the session, leaving a dead entry in
  the adopted map for the life of the process. Those paths now take the same
  release the ordinary disconnect takes: the session is visibly quarantined,
  still charged against capacity while its harness runs, and writes are refused
  honestly instead of failing against a closed socket. A consumer whose
  connection ended can no longer evict a replacement controller that already
  owns the same session.
- **The daemon is no longer the first thing to give up on a burst.** The
  controller bounded its undrained event backlog by FRAME COUNT (192) while the
  shim bounds its own output ring by PAYLOAD BYTES (8 MiB). Both numbers answer
  the same question — how much host output may be in flight before this system
  admits it has lost some — and they were orders of magnitude apart, so the
  controller collapsed on volume the shim absorbs by design and the Gap the ring
  exists to declare became unreachable. The backlog is now bounded in payload
  bytes by `sessionshim.EventBacklogBudget`, which is defined AS the shim's ring
  budget, with a test that fails if the two ever diverge. Past the budget the
  reader still fails closed — it is the only goroutine that can receive a
  durable heartbeat receipt and must never park behind a consumer waiting on
  one — and hosts that run many sessions at once can lower the per-session
  budget through `SessionShimConfig.EventBacklogBudget`.
- **The acceptance seam can actually evict the shim's ring.** The seam exists to
  drive the ring past eviction so the real recovery path — one declared Gap, its
  exact recovery Snapshot, a continued sequence — is observable, but its own
  volume is about 50 KB of frames against an 8 MiB ring: 0.6%, so nothing was
  ever evicted and the run proved nothing while appearing to pass. Nobody could
  see it while the daemon's controller was collapsing first. A session launched
  while the acceptance-control seam is armed now carries a ring sized from the
  seam's own guaranteed volume, through the one optional key in the launch
  contract. It is never a default: without the private token file that makes the
  acceptance route exist, the launch environment is byte-for-byte unchanged.
- **A fail-closed stream drop says why.** Every such decision in the controller's
  read loop closed the socket silently, which left the reason invisible at every
  later caller. Each one now logs the session and the exact reason before
  closing.

## v0.68.13 — 2026-08-25

### Fixes

- **A session shim adopted after startup no longer waits out a heartbeat
  interval.** Publishing an adoption dynamically raises the recovery barrier,
  and that barrier clears only on an acknowledged heartbeat — so a host that had
  already finished carrier activation kept claiming no new work, and did not
  read as adoption-complete to its control plane, until the periodic ticker
  happened to fire (up to 30s by default). The daemon now rings one immediate
  beat the moment activation completes, collapsing that window to a single round
  trip. Embedders that own a scope's own heartbeat lane can do the same through
  the new optional `SessionShimConfig.OnAdoptionActivated` hook, and
  `HeartbeatService.SendNow` sends exactly one out-of-band beat without starting
  or racing the periodic loop. (#426)

### Chores

- **The repository-free recovery guard proves a completed run.** The
  regression test now drives the provider stub through a successful
  tool-using turn rather than a silent failure, asserts the terminal status
  and the absence of a steering resume fallback, and gives each
  version-control command fixture its own marker and verified executable
  mode so neither one can mask the other. (#425)

## v0.68.12 — 2026-08-24

### Fixes

- **Composed Snapshot emission can bind exact shim authority.** Embedders can
  request one active emitting Snapshot through the complete session-shim
  identity, shim, process-epoch, and controller-generation reference while the
  existing identity-only API remains compatible. (#420)

## v0.68.11 — 2026-08-24

### Fixes

- **Dynamic session-shim publication is globally ordered and heartbeat-gated.**
  Concurrent launches serialize adoption, scope-batch publication, and exact
  candidate activation; already-active carriers are retained without a second
  activation, and local work admission reopens only after every changed scope
  echoes its current adoption revision on heartbeat.
- **Attach-v2 separates durable activation from local mutation authority.** A
  composing publisher can collect the complete remote `carrier_active` set
  before explicitly releasing Input, Resize, and Kill callbacks, while existing
  one-call activation remains compatible. Session-shim carrier callbacks can
  also bind mutations to the exact current shim incarnation and controller
  generation rather than an identity alone.

## v0.68.10 — 2026-08-24

### Features

- **Daemon embedders can supply one orchestrator HTTP client.** The exact
  caller-owned client now carries registration, runtime-token refresh and
  re-registration, heartbeat, and poll traffic without imposing transport or
  TLS policy; nil retains every existing OSS client default.

## v0.68.9 — 2026-08-24

### Features

- **Composed poll services can share the daemon's exact claim gate.** The
  public `PollClaimGate` callback delegates to the daemon's live host-status and
  proof-readiness authority, so independently constructed poll loops suspend
  before requesting work and reopen only through the existing recovery path.

## v0.68.8 — 2026-08-24

### Features

- **Attach-v2 fresh candidate composition is claims-bound.** A public candidate
  helper derives the carrier boundary and resolved boundary only from the
  authenticated proof, emits the exact optional `controller_unforwarded` gap,
  and sends the mandatory Snapshot without requiring callers to parse the
  bearer or duplicate proof fields.
- **Adopted attach-v2 recovery can use a Relay-retained candidate.** The explicit
  `server_retained` resume state carries no raw Snapshot; it retains the exact
  original proof-v2 bearer, PTY/carrier epochs, and bounded cursor/gap evidence,
  and fails closed unless both the proof and Relay's candidate cursor match.

## v0.68.7 — 2026-08-24

_v0.68.6 published its versioned worker image and E2B target, but the binary
publisher stopped in mandatory smoke before creating a GitHub release or
updating Homebrew. This patch carries the binary release forward with the
repository-free workarea, live readiness, and installed-service acceptance
support required by the corrected release lane._

### Features

- **Installed-artifact session-shim restart acceptance has a dormant,
  authenticated target control.** A Linux/systemd-user mutator can prepare the
  private bearer, drive real gap, incompatible-quarantine, and one-shot restart
  fence conditions, and clean up its target-owned state. The daemon route stays
  indistinguishable from absent unless an absolute private token file is
  configured, binds every mutation to an adopted lifecycle, and returns no
  acceptance evidence; external fixtures must independently re-observe every
  effect. (#409)

### Fixes

- **Hosted heartbeats carry live proof-v2 readiness.** The authority-bound
  `sessionShim` projection now includes the independently resolved durable
  acknowledgement, proof-v1 closure, retained-credential, remaining-validity,
  and adopted-candidate recovery facts. Missing or changed facts fail closed;
  a capability declaration is never treated as readiness evidence. (#402)
- **Repository-free sessions receive a deterministic empty workarea.** The
  local runner creates an isolated flat directory without invoking Git, while
  repository-backed clones and negotiated shared/declaration layouts keep
  their existing strategies. (#410)

## v0.68.6 — 2026-08-24

_v0.68.5 published its versioned worker image and E2B target, but the binary
publisher stopped in mandatory smoke before creating a GitHub release or
updating Homebrew. This patch repairs that startup failure and carries the
binary release forward._

### Features

- **Linear issues can move between projects without replacement records.** The
  update command resolves a destination project by name, slug, or UUID within
  the issue team, composes project and status in one mutation, and reports the
  resulting project while preserving every unspecified issue field. (#387)
- **Negotiated session-root workareas are active.** Versioned workarea client
  projections expose the root, selected repository working directory, and
  repository metadata for multi-repository workareas while preserving existing
  flat layouts and released client types. (#384)
- **Durable session-shim recovery requires proof-v2 authority.** New admission
  binds the exact proof capability, bearer, receipt, PTY epoch, and cursor.
  Consumed recovery reuses the original bearer, candidate, and staged Snapshot
  without issuing a second one. (#395)

### Fixes

- **Daemon startup handles missing and invalid config before recovery
  authority.** A missing config uses the documented empty archive setting,
  while read, parse, or validation errors return before lease recovery, replay,
  or reaper authority is constructed. (#401, #404)
- **Release tags require authorized creation, immutable refs, and verified
  signatures.** Binary, worker-image, and E2B-template publishers fail before
  build unless the required protection rules are active and GitHub verifies
  the annotated tag signature at its exact commit. (#388)
- **Restored archived workareas validate repository metadata.** Restore derives
  selected and repository paths from the copied, digest-verified declaration.
  Unsafe leaves, paths outside the restored session root, and manifest or
  declaration mismatches are rejected before publication. (#392)
- **Attach v2 preserves the released omitted-field resume shape.** Callers that
  omit both proof schema and authority retain the frozen proof-v1
  `same_handoff` behavior. Supplying only one field, or an unknown explicit
  value, remains a closed validation error. (#398)
- **Proof-v2 readiness is resolved at every new-work edge.** Claim polling,
  direct admission, and carrier activation each re-read all five durable
  readiness facts. A withdrawal enters the existing recovering, zero-capacity,
  heartbeat-acknowledged reopen path. (#398)
- **Selected-v3 recovery releases buffered PTY progress only after a durable
  acknowledgement advances.** Equal, ahead, failed-persistence, and timed-out
  Heartbeats cannot open the Hello output barrier. A successful durable put
  opens it before the reply write so a lost response cannot deadlock locally
  committed recovery. (#398)

### Chores

- **Parallel poll tests no longer race on process-global log capture.** The
  daemon fixture serializes temporary default-logger use and restores the prior
  logger after each control. (#385)
- **GitHub workflows declare least-privilege permissions.** Workflow and job
  token grants are explicit for code scanning, publishing, and read-only
  verification lanes. (#386)
- **Release preparation verifies the configured SSH signing key before tag
  push.** The preflight confirms that the key is registered to the active
  GitHub account and verifies the candidate tag with a pinned OpenSSH verifier.
  (#394)

## v0.68.5 — 2026-08-23

_v0.68.4 was tagged, but its publishers stopped before build and no release
artifacts were created. This patch carries those changes forward._

### Features

- **Linear issues can move between projects without replacement records.** The
  update command resolves a destination project by name, slug, or UUID within
  the issue team, composes project and status in one mutation, and reports the
  resulting project while preserving every unspecified issue field. (#387)
- **Negotiated session-root workareas are active.** Versioned workarea client
  projections expose the root, selected repository working directory, and
  repository metadata for multi-repository workareas while preserving existing
  flat layouts and released client types. (#384)
- **Durable session-shim recovery requires proof-v2 authority.** New admission
  binds the exact proof capability, bearer, receipt, PTY epoch, and cursor.
  Consumed recovery reuses the original bearer, candidate, and staged Snapshot
  without issuing a second one. (#395)

### Fixes

- **Release tags require authorized creation, immutable refs, and verified
  signatures.** Binary, worker-image, and E2B-template publishers fail before
  build unless the required protection rules are active and GitHub verifies
  the annotated tag signature at its exact commit. (#388)
- **Restored archived workareas validate repository metadata.** Restore derives
  selected and repository paths from the copied, digest-verified declaration.
  Unsafe leaves, paths outside the restored session root, and manifest or
  declaration mismatches are rejected before publication. (#392)
- **Attach v2 preserves the released omitted-field resume shape.** Callers that
  omit both proof schema and authority retain the frozen proof-v1
  `same_handoff` behavior. Supplying only one field, or an unknown explicit
  value, remains a closed validation error. (#398)
- **Proof-v2 readiness is resolved at every new-work edge.** Claim polling,
  direct admission, and carrier activation each re-read all five durable
  readiness facts. A withdrawal enters the existing recovering, zero-capacity,
  heartbeat-acknowledged reopen path. (#398)
- **Selected-v3 recovery releases buffered PTY progress only after a durable
  acknowledgement advances.** Equal, ahead, failed-persistence, and timed-out
  Heartbeats cannot open the Hello output barrier. A successful durable put
  opens it before the reply write so a lost response cannot deadlock locally
  committed recovery. (#398)

### Chores

- **Parallel poll tests no longer race on process-global log capture.** The
  daemon fixture serializes temporary default-logger use and restores the prior
  logger after each control. (#385)
- **GitHub workflows declare least-privilege permissions.** Workflow and job
  token grants are explicit for code scanning, publishing, and read-only
  verification lanes. (#386)
- **Release preparation verifies the configured SSH signing key before tag
  push.** The preflight confirms that the key is registered to the active
  GitHub account and verifies the candidate tag with a pinned OpenSSH verifier.
  (#394)

## v0.68.4 — 2026-08-23

### Features

- **Linear issues can move between projects without replacement records.** The
  update command resolves a destination project by name, slug, or UUID within
  the issue team, composes project and status in one mutation, and reports the
  resulting project while preserving every unspecified issue field. (#387)

### Fixes

- **Release tags now require authorized creation, immutable refs, and verified
  signatures.** All binary, worker-image, and E2B publishers fail before build
  unless separate creation and no-bypass immutability rulesets are active and
  GitHub verifies the annotated tag signature at the exact commit. (#388)

### Chores

- **Parallel poll tests no longer race on process-global log capture.** The
  daemon test fixture serializes its temporary default logger and restores the
  prior logger after each control. (#385)
- **GitHub workflows declare least-privilege permissions.** Workflow and job
  token grants are explicit for code scanning, publishing, and read-only
  verification lanes. (#386)

## v0.68.3 — 2026-08-23

### Features

- **Selected-v3 recovery binds a local ACK floor to carrier-owned proof.** The
  shim fsyncs an incarnation-bound mode-`0600` acknowledgement sidecar before
  confirming durable Heartbeat progress. Cold preparation exposes exact
  `LocalResumeFrom` and authenticated `LastHostSeq`; a proof-resolved
  `PreparedAdoption.ResumeFrom` may raise but never regress that local floor.
  Selected v2 ignores the sidecar completely, including corrupt bytes. (#383)
- **Attach v2 carries exact durable-carrier proof correlation.** Its strict host
  claim parser now requires stable store authority, proof revision/digest,
  distinct carrier boundary N and resolved boundary/last host K, reservation
  request identity/digest, and the reserved candidate epoch. The closed gap
  vocabulary adds proof-only `controller_unforwarded` while the existing helper
  retains `ring_evicted` as its source-compatible default. (#383)

### Fixes

- **Pre-active recovery cannot replay ordinary frames or lose the Hello tail.**
  Selected-v3 proof preparation holds the PTY host sequence allocator at K.
  The mandatory Snapshot is allocated atomically as K+1 before queued Output,
  Resize, Marker, or Exit can advance. When K>N, attach v2 accepts only Gap
  N+1..K followed by Snapshot K+1/atSeq K; K=N accepts no invented gap. (#383)
- **Pending and active carrier reconnects retain different truthful cursors.**
  A receipt-stored reconnect retains pre-stage AckSeq N plus its exact optional
  gap and staged Snapshot, without requesting another Snapshot. An active
  reconnect may validate current persisted AckSeq L after ordinary progress
  without freezing at the original proof boundary. (#383)

## v0.68.2 — 2026-08-23

### Features

- **Session-shim controllers have immutable process-scoped identity.** Each
  daemon resolves one high-entropy controller id exactly once at construction,
  or accepts an explicit exact override. The correlation stays independent of
  registration, credential refresh, stable host authority, and every shim's
  own controller generation. (#381)
- **Selected local-wire v2 proxies fresh authoritative PTY snapshots.** The
  read-only operation returns exact shim-owned encoded screen bytes and
  `at_seq` without allocating output sequence. The emitting operation calls the
  shim-owned PTY host once and returns the exact interactive-attach frame:
  delivered once in host-stream order before Exit, or sequence-zero with
  `at_seq == Exit.seq` after Exit. Request ids, modes, generations, immutable
  retries, and the bounded per-connection ledger remain explicit. This release
  ships the OSS protocol and composition boundary; it does not claim downstream
  carrier activation. (#381)
- **Selected-v1 shims remain locally owned through the v2 overlap.** A newer
  daemon still adopts and controls a released v1 shim without sending a v2
  message. A carrier requiring fresh authoritative snapshots instead receives
  typed `authoritative_snapshot_unsupported` evidence while the live shim
  remains capacity-charged; no daemon cache or reconstructed screen substitutes
  for the missing capability. (#381)

### Fixes

- **Controller identity cannot alias credentials.** Stable host ids, worker
  registration ids, the comparison-only runtime-token `jti`, and the literal
  `daemon` are refused as controller sources. Initial, cached, refreshed, and
  re-registered credentials are checked before they become visible to daemon
  state, service lanes, callbacks, or the credential cache; decoded token claims
  never become authentication authority. (#381)
- **Snapshot results fail closed unless their exact disposition is proven.**
  The controller now verifies request id, mode, generation, screen format and
  encoding, emitted frame bytes, live sequence law, and ordered immutable Exit
  before accepting a result. Changed replay, duplicate delivery, stale
  generation, malformed bytes, and live/direct Exit-order disagreement close
  instead of fabricating continuity. The durable event consumer starts before
  carrier takeover, so replay beyond its bounded event buffer and an emitted
  snapshot advance one monotonic durable cursor before adoption completes.
  (#381)
- **Quarantine keeps the trustworthy controller generation.** Selected-v1
  carrier incompatibility reports the committed generation, and a pre-commit
  refusal after authenticated Hello reports that Hello generation. Explicit
  zero is reserved for discovery with no trustworthy Hello. The value is
  preserved through status, heartbeat, adoption batch, and restart-fence
  projections. (#381)

## v0.68.1 — 2026-08-22

### Features

- **Planned restarts have one daemon-owned permission edge.** The localhost
  control API now exposes a no-body restart preflight that enters draining,
  closes admission, settles in-progress launches, refuses uncovered
  direct-owned sessions, and freezes one server-identified correlation snapshot
  across partial retries. Only the closed `prepared` or `not_required` response
  permits a service-manager action; callers never supply a fence or preparation
  id. A matching public client method lets embedding binaries consume the same
  fail-closed contract. (#379)
- **Session-shim state is visible without exposing session content.** Status and
  doctor now report ownership mode, adoption completion and time, occupied
  slots, adopted shim/process/controller correlations, durable forwarded
  sequence, and every quarantined capacity charge. Paths, quarantine detail,
  output, prompts, credentials, tokens, host/controller ids, and opaque
  composing receipts are excluded. (#379)

### Fixes

- **Restart permission cannot outlive its durable hold.** Every initial or
  cached permission revalidates the complete frozen scope set immediately before
  success. Exact stores retain byte-exact requests and a non-empty immutable
  revision; legacy stores keep their source-compatible semantic acknowledgement
  and optional revision; standalone daemons keep a revision-less local held
  intent. Missing, changed, reconciled, or expired authority refuses the restart
  without resnapshotting, reminting, or consuming an external hold. (#379)
- **Updates cannot race or bypass restart preparation.** The update endpoint
  transfers one lifecycle lease from synchronous preflight into update
  initiation, so resume cannot abandon authorization in between. A failed,
  unavailable, or no-op update remains draining until explicit resume durably
  abandons only the controller-local stop authorization; external holds remain
  intact. The bounded local audit marker contains no fence, cursor, host
  authority, session correlation, or credential material and is never used as
  recovery authority by a replacement controller. (#379)

## v0.68.0 — 2026-08-22

### Features

- **Restart fences can carry exact durable acknowledgements.** Composing
  stores may opt into an additive exact-fence interface that receives one
  immutable ordered request and must echo its bytes with a non-empty durable
  revision. Reordered, partial, changed, or revision-less acknowledgements are
  refused. The v0.67 fence-store interface and standalone nil-store behavior
  remain available for existing embedders. (#376)
- **Fence and launch identity can be composed per organization.** Session specs
  carry their organization identity, host authority may be resolved per
  organization, and hosted callers can request one exact fence per organization
  without collapsing duplicate shim incarnations. Every covered session carries
  process epoch, controller generation, and the last durably carrier-forwarded
  output sequence, plus shim id when available; malformed quarantine may omit
  the shim id. Partial retries reuse the original exact request bytes. The
  legacy single-fence method remains available. (#376, #378)
- **Adoption exposes fail-closed durable composition points.** A per-session
  preparation hook runs only after authenticated Hello supplies the exact
  shim/process/current-generation correlation; the shim must echo the exact
  proposed generation and extensions in Adopted. Optional per-session callbacks
  can rehydrate durable carriers, followed by one complete per-organization
  adopted/quarantined/tombstoned batch. When `PrepareAdoptionBatch` is
  configured it must return a non-empty expected revision; when
  `OnAdoptionBatch` is configured it must return a non-empty durable receipt.
  When both are configured both conditions hold before adoption is reported
  complete. Nil hooks preserve standalone behavior. (#378)

### Fixes

- **Observed output is no longer mistaken for durable carriage.** The existing
  session-event callback remains observation-only. A separate acknowledged
  carrier hook advances the forwarded cursor only after successful durable
  handoff; a failed handoff closes controller authority before a later frame can
  leap over the gap. (#376)
- **Duplicate shim incarnations keep independent terminal proof.** Tombstones
  are persisted by lifecycle identity, shim id, and process epoch while retaining
  safe legacy reads when only one proof exists. A terminal incarnation no longer
  hides a different live duplicate, and claim release requires positive proof
  for every correlation covered by the fence. Tombstone publication must also
  finish withdrawing the matching live record before proof disposal. (#378)
- **Ambiguous control and restart paths fail closed.** A bare session stop that
  matches multiple organization-scoped shims cannot fall through to a colliding
  direct child; malformed unknown-organization quarantine cannot be relabelled
  as controller-owned; and a tombstone for the wrong correlation cannot release
  even a single covered session. (#378)

## v0.67.1 — 2026-08-22

### Fixes

- **Session-shim selection now happens before pre-spawn resource acquisition.**
  Default-off and non-interactive sessions no longer invoke an embedder's
  credential/resource hook once for the shim offer and again for the direct
  fallback. A selected session with no launcher or no returned shim handle
  fails closed instead of silently becoming a daemon-owned child. Custom
  launchers retain their previous combined-decision behavior unless they opt
  into the stable selector. (#374)
- **Shim-owned sessions deliver an ordinary, exactly-once lifecycle.** A shim
  launched by the daemon emits one start after controller publication and one
  end only after the immutable shim exit observation, allowing existing
  credential and session-detail cleanup listeners to release their resources.
  Controller disconnect alone never fabricates a terminal event. (#374)
- **Controller gaps stay capacity-visible until exact terminal proof.** An
  unexpected controller-stream close moves the live shim into a deduplicated,
  capacity-consuming quarantine. Only a group-reaped tombstone matching the
  lifecycle identity, shim id, and process epoch removes that charge. The same
  process-epoch correlation now rides quarantine/status/heartbeat and restart-
  fence projections, and mismatched fence acknowledgements are refused. (#374)

## v0.67.0 — 2026-08-22

### Features

- **Per-session shim ownership and restart adoption foundation.** Interactive
  sessions can be owned by a separate, versioned `session-shim-v1` process
  while the daemon acts as a generation-fenced controller. The foundation
  includes the bounded secret-free registry, adoption/quarantine accounting,
  replay-gap honesty, terminal tombstones, orphan bounds, restart-fence seam,
  daemon/worker production wiring, and post-adoption stop/input/resize/output
  paths. Adoption and ownership remain default-off until the installed-service
  restart smoke and composing-plane fence integration pass. (#372)
- **Ref-bearing runs provision the requested existing branch.** A queued base
  `Ref` is validated and checked out directly; the runner scopes `GH_TOKEN` to
  ref-bearing runs and skips creating a duplicate pull request, allowing an
  autonomous repair session to add commits to an existing review branch. (#368)
- **Heartbeat load averages.** Daemon host telemetry now reports the one-,
  five-, and fifteen-minute load averages alongside existing CPU and memory
  samples. (#369)

### Fixes

- **An ambient `DONMAI_API_URL` can no longer outrank the runner's canonical
  platform origin.** The headless `codex app-server` child was started at
  `Provider` construction, from the ambient `os.Environ`, before any session
  existed — so a host- or credential-snapshot-injected origin beat the
  runner-owned one for every session that `Provider` served. Process start and
  the initialize handshake now defer to the first headless `Spawn`/`Resume`, and
  the child environment is composed with that session's `Spec.Env` as an
  explicit layer above the inherited parent. `DONMAI_API_URL` is additionally
  reserved on the canonical agent-env blocklist, so a credential snapshot can
  never author the platform origin. (#370)

### Chores

- **Keep `session-root-v1` dormant until the full protocol is negotiated.** A
  runner-level V16 control now prevents legacy work items from activating the
  session-owned multi-repository layout before producer/executor negotiation,
  the durable declaration record, refusal semantics, and enforceable read-only
  authority exist. (#371)

### Operator-visible behaviour changes

- **Plaintext HTTP to a non-loopback platform origin is now rejected.**
  `internal/linear.NewProxiedClient` validates and canonicalizes its origin in
  the constructor, before any request is built: userinfo, a path, a query, a
  fragment, a trailing delimiter, an empty or out-of-range port, and `http://`
  against anything other than a loopback host all fail closed with
  `ErrInvalidPlatformURL`. `http://127.0.0.1:3010` and friends still work — the
  local `donmai host` loopback and test servers are unaffected — but an operator
  who was pointing `donmai linear` at a **remote** origin over plaintext `http`
  must switch it to `https`. The request carries a platform bearer, so that
  origin was never safe to use unencrypted. The rejection names the *source*
  that supplied the origin and never echoes the value, because an origin can
  carry a bearer in its userinfo or query.
- **Provider registry construction no longer spawns a `codex app-server`
  process.** Daemon-startup provider introspection and any other registry build
  now only probe for the `codex` binary; nothing is executed. A missing binary
  is still `agent.ErrProviderUnavailable` at construction, so an uninstalled
  codex is skipped exactly as before, but a *failing initialize handshake* now
  surfaces later — at the first `Spawn`, as `agent.ErrSpawnFailed` wrapping
  `agent.ErrProviderUnavailable` — instead of at registry build. Operators
  reading startup logs to confirm codex health should read the first session's
  spawn result instead.
- **One `codex` Provider serves one session's environment.** The child's
  environment layer is frozen at start, so a later `Spawn`/`Resume` whose
  `Spec.Env` materially differs is refused with a value-free error naming the
  diverging keys, rather than silently served the first session's
  `DONMAI_SESSION_ID` and `DONMAI_API_URL`. `donmai agent run` builds one
  Provider per session, so this does not fire in normal operation; it is a
  fail-closed guard for embedders that pool Providers. Same-environment resume
  and the interactive PTY spawn mode are unaffected.

---

## v0.66.0 — 2026-08-20

### Features

- **W3C trace-context propagation through the dispatch pipeline.** `QueuedWork` now carries `traceparent`/`tracestate`/`sessionStorageId`/`sessionPublicId`/`trackerSessionId` from the platform's dispatch chokepoint through `PollWorkItem` → `SessionDetail` → `prompt.QueuedWork`; the runner's span processor reuses a valid incoming `traceparent` (strict 00- trace-id/parent-id validation, non-zero, lowercase) and emits the three session-id join keys as span attributes, joining a dispatch receipt, runner spans, activity rows, and the relay room in one trace. Older platforms omit the fields and older runners ignore them. (#355 follow-on)

### Fixes

- **MCP gateway bearer is seeded before spawn and the harness falls back when the live file is absent.** The pre-spawn rail injected `MCP_GATEWAY_TOKEN_FILE` with no seed and the harness removed the static `Authorization` header when the env was set, so every platform tool call ENOENT'd from spawn until the first refresh (~7h). `SessionSpec` now carries `mcpAuthToken`/`mcpAuthTokenExpiresAt` and the daemon atomically seeds the token file before env injection, starting the refresher from the real expiry; the `clijsonl` MCP config emits a `headersHelper` that tries the live file first and falls back to the baked spawn-time bearer when absent or empty.
- **Worktree golangci-lint cache isolated per worktree.** Parallel lint runs no longer share one cache directory and poison each other; `go.work` trap documented. (#364)
- **Linear sub-issue pagination.** Paginates beyond one upstream page while preserving order. (#359)

### Chores

- **Deps:** bump `go-git` security patch (#361).
- **Lint:** enforce allowlist import boundaries with `depguard` (#363).
- **Release:** harness-smoke gate required before publish (#360).
- **Formatting:** `gofumpt` clean.

---

## v0.65.0 — 2026-08-17

### Features

- **Headless sandbox runs can request full network/metadata access via a new
  wire field.** `QueuedWork` gains an optional `permissionProfile` field
  (`"autonomous"` or `"workspace-write"`); absent, empty, or
  `"workspace-write"` preserves the existing hardcoded sandbox behavior
  byte-for-byte. `"autonomous"` resolves to the sandbox's existing
  full-access tier, previously unreachable from the wire, letting headless
  autonomous runs perform git network/metadata operations (fetch,
  cherry-pick) without hitting an unanswerable sandbox-escalation review. An
  unrecognized value is fail-safe: it logs a warning and falls back to
  workspace-write rather than failing closed. The field also flows into the
  admission receipt's exact-adaptation digest. (#355)

### Fixes

- **A relay restart no longer permanently silences an attached session.** The
  interactive-attach protocol treats a relay's `ring-miss` control as a safe,
  designed repair path — reset to a fresh snapshot and resume — but the
  host-side attach client was instead classifying `ring-miss` (and an
  internal degraded-lane rewind failure against its own retained ring) as a
  fatal error, ending the host run and silencing viewing/driver-pen input for
  the rest of the session after a single relay bounce. A new error type
  reclassifies both call sites as reset-and-retry: the client drops its
  resume position and re-attaches on a dedicated, slower backoff (default
  ceiling 60s) separate from normal reconnect backoff. Genuinely fatal cases
  (auth, stale epoch) are unchanged. Adds a test helper for simulating a
  relay restart. (#356)

## v0.64.0 — 2026-08-16

### Features

- **The gateway MCP auth header can follow a live token file.** When the
  session environment carries `MCP_GATEWAY_TOKEN_FILE`, the CLI-harness MCP
  config resolves the gateway `Authorization` header from that file at use
  time instead of baking the spawn-time bearer forever; absent the variable,
  the config is byte-identical to before. A control-plane daemon that
  refreshes the token file can now keep a session's MCP surface alive for
  the session's whole life (#353).

### Chores

- Raise the Go toolchain floor to 1.26.6 — six reachable standard-library
  vulnerabilities fixed; every independent pin site (go.mod, release
  dry-run, both worker image stages) moves together (#352).

## v0.63.0 — 2026-08-16

### Fixes

- **A control-plane `contextWindow` on the resolved profile now reaches the
  pi extension instead of being silently clamped to the built-in default.**
  The resolved-profile wire type gains an additive `contextWindow` field;
  the dispatch bridge injects it into `ProviderConfig["contextWindow"]`
  (explicit provider config wins, a model-profile `context` supersedes
  both); the pi provider exports it to the extension environment as
  `DONMAI_PI_CONTEXT_WINDOW` for both the headless and interactive lanes;
  and the embedded policy extension reads it — falling back to its previous
  hardcoded 200000 only when unset. A 1M-context model driven through pi
  now runs at its native window. (#350)

## v0.62.0 — 2026-08-15

### Fixes

- **Advisory extension deliveries no longer fail spawns on harnesses that
  cannot load them.** A `Spec.AdditionalExtensions` batch where every
  `ExtensionDelivery.Required` is false now projects a non-required
  tool-lifecycle entry: on a harness profile whose `tool_plugin` channel is
  unsupported, the batch is recorded on the receipt (`Outcome: denied`,
  `Required: false`), stripped from the adapted Spec, and the spawn proceeds
  degraded-but-honest instead of being refused with `cannot apply required
  entry "additional-extensions"`. A batch carrying any load-bearing
  (`Required: true`) delivery still denies closed exactly as before — the
  fail-closed rule for granted capabilities applies unchanged.
  `ApplyPreparedHarness` reproduces the identical receipt-keyed drop on the
  child side, so host and worker can never disagree about what was dropped.
  (#348)

## v0.61.0 — 2026-08-15

### Features

- **`Daemon.UpdateSessionRuntimeCredentials(prevWorkerID, workerID, authToken)`
  exposes the scoped session-credential update to embedding binaries.** A
  binary that runs more than one worker identity on a single daemon process
  owns re-registration for the identities the daemon itself does not hold, and
  had no way to reach those identities' stored session details. The new method
  re-stamps exactly the sessions attributed to `prevWorkerID` and reports how
  many it updated; passing both the superseded and the settled worker id is
  what carries sessions onto a new identity when a refresh falls back to a full
  re-registration instead of orphaning them on the retired one. The daemon's
  own registration is already wired to the same scoped update internally.

### Fixes

- **A runtime-credential refresh no longer overwrites the sessions of other
  worker identities.** One daemon process can serve several worker identities
  — a host admitted to more than one organisation holds a registration per
  organisation, each refreshing its runtime token on its own schedule while
  every identity's sessions share the daemon's one session-detail store. The
  refresh swept that store unconditionally, so one identity's routine refresh
  stamped its own worker id and bearer onto every other identity's sessions;
  those children then presented the wrong identity on each subsequent platform
  call and were rejected for the rest of their lives, while the refreshing
  identity's own sessions looked healthy. The refresh is now scoped to the
  identity being refreshed.

### Chores

- **A red harness smoke now blocks the release publish.** `release.yml` ran
  GoReleaser with nothing between a `v*` tag push and a published Homebrew
  cask, so a broken harness protocol could ride a tag straight to
  `brew upgrade donmai`. A `harness-smoke` job the release job `needs:` checks
  out the exact tagged ref, builds the binary from it, and runs the full
  donmai-smokes suite; guard steps assert the daemon-lifecycle and pi
  real-binary lanes actually report PASS rather than quietly SKIP.

- **Worker-image PR builds are isolated from the release layer cache.** Both
  the untrusted `pull_request` build and the trusted tag build shared one
  persistent BuildKit sticky disk under the same implicit cache key, so a PR
  build could seed layers the release build would silently reuse. PR builds now
  use an ephemeral builder that keeps no state, the release build is pinned to
  its own never-before-used cache key, and `worker/Dockerfile` pins its npm
  globals to exact versions so the resolved version is part of the cache key.

---

## v0.57.12 — 2026-08-11

### Fixes

- **Headless Codex host-session routes now inherit the authenticated CLI
  session without weakening MCP isolation.** The app-server intentionally
  runs under a private `CODEX_HOME`, but that also hid the host's ChatGPT login
  and made every subscription-backed model call fail with a 401. A
  per-session constructor hint now enables host auth only when the
  authoritative harness is Codex and `authMode=host-session`; the private home
  pins file auth but defers both the host credential link and app-server start
  until the selected headless spawn path, while retaining its own
  `config.toml` and exact MCP activation proof. Keyed routes,
  unknown/non-Codex explicit
  harnesses, and daemon introspection keep the credential-free default.

- **Codex 0.147 lifecycle synchronization no longer floods agent activity.**
  Routine MCP startup and thread-settings notifications, plus echoed user
  message items, are consumed as protocol bookkeeping. Failed or cancelled
  MCP startups remain visible as bounded system events without publishing the
  app-server's free-form error text.

- **Headless Codex waits for capability MCPs before the first turn.** Codex
  reloads `mcp_servers` asynchronously, so a successful config write/read could
  race `thread/start` and leave A2A, memory, knowledge-graph, and code-intel
  tools unavailable for that turn. Donmai now polls Codex's paginated MCP
  inventory and requires every requested server's completed initialize metadata
  before opening or resuming a thread. A server removed from the preceding
  Donmai-managed set must also leave the inventory; Codex-owned ambient entries
  outside the isolated config do not block activation. Startup timeout or
  inventory failure is a bounded, typed pre-thread denial.

- **Headless Codex MCP calls no longer cancel for lack of a user response.**
  MCP tool approval is internal to Codex rather than part of the app-server's
  command/file JSON-RPC approval bridge. Each requested server now carries
  `default_tools_approval_mode = "approve"` in the isolated config, and exact
  config readback must preserve that seed before the first turn.

- **Code-intel MCP warm-up no longer creates cache-only backstop PRs.** The
  generated `.donmai/code-index/` tree is excluded from deterministic
  backstop commits while source-owned `.donmai` configuration remains eligible.

---

## v0.57.11 — 2026-08-10

### Fixes

- **MCP tool-name requirement now follows mount-channel deliverability.**
  Tool names narrow the tool surface of the mounted servers. When a harness
  delivers the mount boundary itself but auto-discovers tools (so a name
  filter is undeliverable), the adaptation walk previously fail-closed the
  whole spawn with `delivery_unsupported` — blocking headless dispatch on
  such harnesses one gate after the v0.57.10 designator fix. Such harnesses
  now record a truthful denied entry on the adaptation receipt and proceed:
  the surface stays bounded by the mounts. Harnesses that cannot deliver the
  mount channel either (external-attach providers, where nothing is
  controlled) keep the fatal denial — an unapplicable name policy there
  means zero MCP control.

---

## v0.57.10 — 2026-08-10

### Fixes

- **Headless codex sessions no longer die at spawn when an agent card's
  permissions use tool designators.** Codex consumes a structured
  `PermissionConfig` rather than a flat allowlist, so the runner bridges card
  `AllowedTools`/`DisallowedTools` designators (`Bash(git *)`, `Bash(*)`,
  `Read`) into `AllowPatterns`/`DisallowPatterns` verbatim — and the codex
  approval bridge consumes that grammar natively. Tool/lifecycle adaptation
  validation, however, required every pattern to compile as a raw regular
  expression; `Bash(*)` does not (`*` immediately after `(`), so adaptation
  fail-closed EVERY headless codex spawn with `malformed_tool_lifecycle_plan`
  before delivery (measured 5/5 on a fleet host, 2026-08-10). Validation now
  recognizes the tool-designator grammar ahead of its regex fallback —
  accepting exactly what the consumer accepts. Input that is neither a
  designator nor a valid regex still fails closed. (#309)

---

## v0.57.9 — 2026-08-09

### Features

- **A pool can now admit "every project routed here" as a single consent
  decision, and admission state travels on every heartbeat.** Admission is
  enforced once, in `WorkerSpawner.isProjectAllowedLocked`
  (`resolveProjectForSpecLocked` → `AcceptWork`); until now the only knob was
  `enabledProjectIds`, a pure enumeration with no "all" semantics, so an
  operator who wanted a pool to serve every one of their projects still had to
  retype that project list a second time, per machine, forever — invisible
  until dispatch landed on a host that had not enumerated it, silently
  re-queued, and reported the daemon as offline. A new `daemon.yaml`
  `projectAdmissionMode` adds `all-routed` alongside the default `enumerated`:
  absent, blank, or misspelled all still read as `enumerated`, and an
  unrecognized value now fails config validation outright rather than
  silently deny-all. `all-routed` does not widen the trust boundary — the
  daemon's registration token is already org-scoped, so only work the
  operator's own control plane routed to a pool this machine belongs to can
  reach `AcceptWork`; the mode only removes the second enumeration of an
  intent already declared upstream.

  Separately, admission state used to reach the orchestrator only at
  registration: the heartbeat hashed just the repository projection, so
  enabling a project with no repository resource left that hash
  byte-identical, and the orchestrator kept a stale admission set until the
  daemon's next process restart — the "enable the project, then restart the
  daemon" step operators kept hitting. `admissionHash` now covers mode +
  `enabledProjectIds` + entries under one change detector and travels on
  every heartbeat. SMOKE-GAP: the all-routed admission path has no coverage
  in `donmai-smokes` yet.

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
