# Pi interactive notice boundary

Interactive Pi supports durable-mailbox polling, not acknowledged runtime
injection. Its manifest's `rpc-steer` declaration describes the headless
`--mode rpc` lane. The interactive runner reports `channel-not-driven` in
`deadLetteredInjects`, never `AckInject`. Direct interactive `Handle.Inject`
returns both `agent.ErrUnsupported` and `pi.ErrInteractiveReceiptUnavailable`.
Use headless RPC workers when pushed input is required.

Transcript activity is observational. A transcript entry, extension event,
successful terminal write, or return from `sendMessage` is not a durable applied
receipt. Peer extensions execute arbitrary code in the same process; globals,
environment variables, closures, files, and sockets within that authority domain
cannot authenticate application. Same-user transcript files are writable and
cannot serve as evidence for acknowledgement.

## Required contract before enabling interactive injection

Choose an upstream awaited entry API with an explicit trusted-extension or
isolation boundary. Its receipt must follow durable SessionManager persistence.
Alternatively, a host-owned control channel must own both application and the
durable receipt outside arbitrary extension authority. A separate socket alone
does not establish that boundary.

The implementation must satisfy all of these conditions:

1. Persist an offer before accepting custody, keyed by session and delivery ID.
   Repeating the same ID and payload is idempotent; conflicting payloads fail.
2. Atomically associate the exact delivery with durable conversation persistence
   before publishing its applied receipt. Receipt identity binds the session,
   delivery ID, payload, and persisted entry.
3. Prevent arbitrary peer extensions from minting or replaying receipts. Never
   accept a proof discovered in extension-writable transcript or mailbox files.
4. Recover a crash before persistence as unapplied work. Recover a crash after
   persistence by returning the identical receipt without creating another turn.
   Persistence without recoverable delivery identity is insufficient.
5. Preserve unapplied offers across reload and restart. Historical entries do not
   acknowledge new work, and receipts cannot cross sessions or deliveries.
6. Return `AckInject` only for the exact applied delivery. Unsupported or absent
   channels continue to produce dead letters; receipt ambiguity retains work.
7. Run a real interactive Pi canary through durable append, claim, steering,
   post-persistence receipt, and the controller's applied state. Include crash,
   restart, duplicate-offer, and malicious-peer-extension controls.

These are enablement requirements, not capabilities supplied by the current
adapter. The receipt bridge below is implemented; the live success canary
is not claimed here. A Pi upstream awaited-entry API remains the cleaner
long-term primitive — until it exists, the host-owned claim channel is the
boundary.

## Host-owned claim channel (implemented)

The interactive spawn loads a second host-owned extension by explicit `-e`
path, immediately after the boundary extension and before caller
\deliveries (`extensions/donmai-notice-claim.ts`, embedded and measured
like the boundary). It exposes `/donmai-claim <deliveryId> <text>`:

* The command persists a `donmai-notice-offer` custom entry FIRST, then
  applies the notice as a `donmai-notice` custom message through
  `pi.sendMessage` — which pi persists through SessionManager before it
  emits the `message_end` the Go transcript tailer observes — and the
  `message_end` handler then persists the `donmai-notice-applied` receipt.
  The receipt therefore exists only after durable conversation
  persistence (conditions 1–2).
* A repeated claim for an applied ID reports "already applied" and writes
  nothing; a conflicting payload under the same ID is refused (conditions
  1, 4). An offer with no applied marker is unapplied work and replays;
  a crash after persistence re-reports the identical receipt without a
  duplicate turn (condition 4). Offers and receipts live in the session
  file, so reload preserves unapplied work and history never acknowledges
  new deliveries (condition 5).
* The command runs only when the spawn stamped the trust verdict
  (`DONMAI_NOTICE_CLAIM_TRUST=1`), which the spawn sets only after the
  compiled embedder's trusted set names the exact embedded digest AND no
  caller delivery shadows the claim ID — re-checked per event, never
  cached. The verdict rides the spawn's complete environment, and any
  caller-carried copy is dropped by the harness-owned env prefix. The
  `message_end` handler mints the receipt only with the verdict AND a
  matching persisted offer for that delivery (same ID, same text), and
  replays — never duplicates — when one already exists. So an arbitrary
  peer extension cannot mint the receipt without the offer, replay it
  past one per delivery, or forge it in an untrusted session (condition
  3); no proof is ever read out of a transcript or mailbox file. Exact
  authorship binding (which copy of the command acted) still awaits a pi
  upstream primitive — the fence proves the receipt never precedes the
  offer and never exceeds one per delivery, not per-action identity.
* Direct `Handle.Inject` still refuses (`ErrInteractiveReceiptUnavailable`)
  and the runtime-inject rail still dead-letters `channel-not-driven`:
  the receipt path is claimed through the session's own channel, never
  pushed over the PTY (condition 6).

Condition 7 (the real interactive canary with crash, restart,
duplicate-offer, and malicious-peer controls) remains future work and is
tracked separately; the scripted fixture
(`testdata/notice-claim-harness.mjs`, always run, no `pi` binary needed)
proves the ordering, idempotence, conflict-refusal, and trust properties
against the real production extension.
