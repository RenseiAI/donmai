# Claude Code against a third-party Anthropic-compatible endpoint — decision

Status: decision recorded. Implementation is pending in the follow-up
slices at the end of this note.

## Scope

This note covers only non-first-party models served over an endpoint
that speaks the Anthropic Messages protocol but is not the first-party
API (for example a vendor's own Anthropic-compatible path). First-party
models reached through a gateway already run through the CLI's
documented base-URL plus token pair with the company attribution intact
— established by the earlier gateway spike — and are out of scope here.

## What exists today

- The harness endpoint projection applies a binding only when the
  endpoint company is the first-party one; any other company fails
  loudly instead of routing.
- A gateway projection exists for the first-party company: the
  gateway's base URL plus the CLI's unattended token pair, with the
  modal-raising key name kept out of the child environment.
- The harness manifest drives the anthropic-messages protocol on the
  host-login, direct, and cloud-host cells — not on the gateway host.
- The hand-authored capability-cell list has no harness × third-party ×
  gateway cell.
- Admission checks a binding against the hand-authored cells and the
  harness manifest's driven protocols and hosts, so a third-party
  binding is refused on both grounds today.
- Execution receipts already separate the spoken surface (company) from
  the model author, so a truthful third-party cell is representable in
  receipts without any format change.

## Options compared

(a) A qualified `(harness, company, host, protocol)` cell for the
third-party company speaking anthropic-messages.

(b) An environment-only override that keeps the company set to the
first-party value.

| Axis | (a) qualified cell | (b) env-only override |
|---|---|---|
| Receipts | Truthful: the company names the surface spoken to and the model author names who authored the model; both are bound into the receipt digests. | Misstates: receipts bind the first-party company while traffic goes to a third party. |
| Admission | A hand-authored cell plus a manifest drive entry admit exactly this tuple; everything else still refuses. | No cell exists to admit it, so receipt-bearing checks compare against the admitted cell and mismatch. |
| Narrowing | Cell validation is narrowing-only by construction: protocol intersection, host membership, auth-mode subset, capability narrowing. | Bypasses validation entirely — any base URL rides with no cell to bound it. |
| Credential names | The vendor declares its own key names; the projection translates the resolved bearer onto the CLI's base-URL plus token pair, keeping the modal-raising name out of the child environment. | Works mechanically today (a raw base URL in the session environment, pinned by test) but carries the wrong company's credential shape. |
| Model mapping | The binding's model wins over the session model on both spawn lanes, and native vendor ids ride the `--model` flag verbatim — the earlier spike showed catalog slugs confuse the CLI's model recognition, cost basis, and tool loading. | Same pass-through, but with no binding there is nothing to prefer over the session model and nothing recording which id was intended. |

## Recommendation

Option (a). Option (b) stays valid only where the company attribution
is already truthful — first-party models through a gateway, which is
the already-shipped environment route a test pins. For non-first-party
models, (b) would launder third-party traffic under first-party
receipts.

Note: the explicit qualified-protocol view from the October 2026
endpoint-protocol architecture decision is not implemented in this
repository yet, so the new cell follows the existing hand-authored cell
convention. The protocol is unambiguous because the vendor speaks only
anthropic-messages.

## Follow-up slices

1. Admit the vendor cell (2 points): new endpoint manifest speaking
   anthropic-messages with a gateway host row under its own key names,
   harness manifest drives the gateway host, one hand-authored qualified
   cell, admission plus parity coverage.
2. Project the vendor cell (1 point): generalize the endpoint
   projection's company gate and bearer-name mapping onto the CLI token
   pair, with routing-pin coverage on both spawn lanes.
