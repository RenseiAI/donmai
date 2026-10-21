# Outage hold for healthy harnesses

A healthy local harness is never reaped on silence. Reaping on a timer applies
only when the host has positive evidence the session should end, not merely
because the control plane could not be reached.

## Contract reference

This document is the OSS-canonical text proposed for
`donmai-architecture/ADR-2026-10-21-outage-hold.md` (status: proposed,
boundary: shared, synchronized-mirror; platform mirror:
`rensei-architecture/ADR-2026-10-21-outage-hold.md`, status Mirrored,
canonical pointer to the OSS file). The ADR has not been published yet — a
donmai session cannot publish to the architecture corpus — so reviewers check
the contract here: field names, sentinel wrapping rules, and the platform
mirror placeholder below are the exact terms the ADR will carry verbatim.

## Unreachable versus released

The daemon distinguishes two facts that used to share one outcome:

- **Control plane unreachable or unhealthy**: timeouts, 5xx, connection
  refused, relay draining, or any refusal the carrier never examined. These
  carry no fact about the lineage behind them.
- **Control plane says the lineage is released or ended**: a decisive,
  authenticated answer for this lineage (`ErrSessionShimLineageReleased`
  returned wrapped with `%w` from `OnAdoption`/`OnAdoptionV2`), the composing
  layer releasing it through `LineageLive` or `ReadoptionHeldPastWindow`, the
  discovery record going away or a terminal tombstone appearing, an explicit
  stop or shutdown, a session duration quota expiring, or a credential the
  hold must never extend expiring on its own schedule
  (`ErrSessionShimCredentialExpired`, likewise wrapped).

Only the second list, a requested stop, or a quota ends the harness. The
first list keeps it running.

## During an outage

While the hold is armed (`SessionShimConfig.ReadoptionHeldPastWindow` plus
`SessionShimConfig.LineageLive`) and the lineage stays observable and held:

- keepalive continues, extending the shim's orphan clock for one further
  deadline per honoured exchange, gated on the same held-past-window
  conjunction — the moment the composing layer lets go the extensions stop;
- re-adoption retries past the bounded window or attempt budget that would
  otherwise quarantine: a lineage-live window continues in place with the
  keepalive still armed and the backoff still capped, and a spent
  fixed-attempt budget holds with the keepalive running and capped backoff;
- harness output stays buffered on the host in the shim ring and the
  controller backlog, both bounded by payload bytes; overflow evicts oldest
  first and declares an explicit gap, and crossing the stall deadline
  publishes the carrier as degraded while holding rather than dropping;
- the session stays adopted and locally observable through the local session
  list (`AdoptedSessionShims`, `AdoptedSessionShimBindings`) and host watch,
  and remains locally attachable.

No authority is loosened while held: an unreachable control plane never grants
anything new, and credentials still expire on their own schedule.

## On return

When the carrier returns, the next re-adoption replays from the durable
resume cursor, so no buffered output is lost. The platform reconciles state
from the complete adoption projection the daemon publishes.

## Transition table

Every state times every event to the next state; the code that closes each
row is named. Lineage-live continuation and the fixed-attempt hold share one
authority (`ReadoptionHeldPastWindow` with `LineageLive`) and one set of
decisive exits.

| State | Event | Next state | Code |
|---|---|---|---|
| adopted, carrier bound | carrier fault, shim observable and held past bound | holding (continued window / fixed hold), keepalive running, retrying | `readoptSessionShimAfterCarrierLoss`, `holdFixedAttemptsPastBudget` |
| holding | carrier returns, re-adoption commits | adopted, carrier bound | `readoptSessionShimOnce` |
| holding | shim unobservable (record gone or tombstone) | withdrawn, quarantined under dead-shim reason | `readoptSessionShimAttempt`, `sessionShimIncarnationStillLive` |
| holding | composing layer stops holding (`LineageLive`/`ReadoptionHeldPastWindow` false) | withdrawn under the ordinary bound outcome (window exhaustion with hook, or spent budget), keepalive stops at once | `readoptSessionShimAttempt`, `startSessionShimOrphanKeepaliveWhile`, `sessionShimWindowExhausted` |
| holding | decisive released answer | withdrawn, quarantined at once | `isSessionShimDecisiveRelease` |
| holding | explicit stop or daemon shutdown | released by the stop path / `readoptionShutdown`, keepalive stopped | `StopAdoptedSessionShim`, `readoptionShutdown` |
| holding | credential expiry | withdrawn, quarantined at once | `isSessionShimDecisiveRelease` |
| holding | duration quota reached (signalled via release or stop) | withdrawn, harness reaps on ordinary deadline | composing layer release or stop path |
| holding | outage persists | holding | `readoptSessionShimWithinLivenessWindow` window continuation, `holdFixedAttemptsPastBudget` |

## Configuration

The hold is opt-in and inert by default: a nil `ReadoptionHeldPastWindow`
keeps the bounded behaviour exactly. Arming it requires both
`ReadoptionHeldPastWindow` and `LineageLive` on the session-shim config, in
either re-adoption mode. A fixed-attempt hold runs on the default backoff cap
and the derived keepalive interval (`BackoffCap`/`KeepaliveInterval` stay
refused in fixed mode). Declaring an external release threshold with the hold
(in either mode) requires `LineageLive`, so the extension continues only while
the daemon still holds the lineage.

## Platform mirror (placeholder)

The platform extension — the composing control plane's wiring of
`ReadoptionHeldPastWindow` (no stop requested, no duration quota spent, no
external release while the answer stays true), the mapping of the platform's
released/ended answer onto `ErrSessionShimLineageReleased`, and downstream
reconciliation of the complete adoption projection — lives in
`rensei-architecture/ADR-2026-10-21-outage-hold.md` (Mirrored stub pointing at
the OSS-canonical ADR above). Until that stub is published, this section is
its verbatim source.
