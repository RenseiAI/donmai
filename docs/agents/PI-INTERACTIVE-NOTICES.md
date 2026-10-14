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
adapter. No receipt bridge or success canary is claimed here. A Pi upstream
implementation and a verified isolation boundary remain necessary before the
interactive runner can drive this channel.
