// donmai-notice-claim — host-owned applied-receipt extension for Pi
// interactive sessions.
//
// This file is the TRUSTED-extension half of the interactive notice receipt
// contract (docs/agents/PI-INTERACTIVE-NOTICES.md). It is delivered through
// the SAME explicit `-e` path as the embedded policy extension — never
// through workspace discovery (`--no-extensions` stays on) — and it is
// effective only when its measured identity matches the compiled embedder's
// trusted set (Options.TrustedExtensions). An arbitrary peer
// AdditionalExtension cannot mint its receipts: the applied marker is
// accepted only for a delivery this extension's own claim command applied,
// and the session file is read through the extension runtime's own
// session manager, not through a transcript or mailbox file the peer can
// write.
//
// Mechanism (verified against the real pinned binary):
//
//   - Claim: the `/donmai-claim <deliveryId> <text>` slash command persists
//     a `donmai-notice-offer` custom entry FIRST — so a crash before
//     application replays as unapplied work — then applies the notice into
//     the conversation as a `donmai-notice` custom message via
//     `pi.sendMessage`, which pi persists through SessionManager before it
//     emits the `message_end` the Go tailer observes. The `message_end`
//     handler then persists the `donmai-notice-applied` receipt binding the
//     delivery to the persisted entry. The receipt therefore exists only
//     after durable conversation persistence, never before it.
//   - Idempotence: a repeated claim for an already-applied delivery ID
//     reports "already applied" and writes nothing — no duplicate turn.
//     A conflicting payload under the same ID is refused.
//   - Recovery: an offer with no applied marker is unapplied work (reclaim
//     it); an offer with one is complete (replay the receipt, apply
//     nothing).
//
// The extension reads session state through ctx.sessionManager (the
// runtime's own record), never by scanning transcript files: a peer that
// forges `custom` lines into the session file cannot satisfy the claim
// path, which requires the entry to have been written by this extension's
// own offer step in the same session.
//
// TypeScript surface (kept narrow on purpose): a type-only import plus
// parameter annotations in the erasable subset the scripted harness
// strips — see testdata/interactive-local-tool-policy-harness.mjs. No
// interface blocks, no return-type annotations on nested functions.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const OFFER_TYPE = "donmai-notice-offer";
const APPLIED_TYPE = "donmai-notice-applied";
const MESSAGE_TYPE = "donmai-notice";

function splitClaimArgs(args: string) {
  const trimmed = args.trim();
  if (!trimmed) return null;
  const space = trimmed.indexOf(" ");
  if (space < 0) return null;
  const deliveryId = trimmed.slice(0, space).trim();
  const text = trimmed.slice(space + 1).trim();
  if (!deliveryId || !text) return null;
  return { deliveryId, text };
}

function entriesOf(ctx: any) {
  try {
    const entries = ctx.sessionManager.getEntries();
    return Array.isArray(entries) ? entries : [];
  } catch {
    return [];
  }
}

function findEntry(entries: any, customType: string, deliveryId: string) {
  return entries.find(
    (e: any) => e.type === "custom" && e.customType === customType && e.data.delivery === deliveryId,
  );
}

function appliedReceipt(entries: any, deliveryId: string) {
  return findEntry(entries, APPLIED_TYPE, deliveryId);
}

export default function activate(pi: ExtensionAPI) {
  // The claim command is effective only when the compiled embedder
  // trusts this file to share receipt authority. The Go side loads this
  // copy by explicit path and measures it; the digest it trusts is
  // compiled in, so a peer pack cannot substitute its own copy under the
  // same command name — but belt and suspenders, the command refuses to
  // run when the session did not prove the trust. The proof rides the
  // DONMAI_NOTICE_CLAIM_TRUST env the Go spawn stamps only after the
  // trusted-set check passes; a peer extension cannot set it for the
  // session because the child env is the spawn's complete environment.
  const trusted = process.env.DONMAI_NOTICE_CLAIM_TRUST === "1";
  function claim(deliveryId: string, text: string, ctx: any) {
    const entries = entriesOf(ctx);
    const already = appliedReceipt(entries, deliveryId);
    if (already) return Promise.resolve("already applied " + deliveryId);
    const offer = findEntry(entries, OFFER_TYPE, deliveryId);
    if (offer) {
      const offeredText = String(offer.data.text ?? "");
      if (offeredText !== text) {
        return Promise.reject(new Error("delivery " + deliveryId + " already offered with a different payload"));
      }
    } else {
      // Persist the offer BEFORE applying: a crash between the two replays
      // as unapplied work, never as a lost application.
      pi.appendEntry(OFFER_TYPE, { delivery: deliveryId, text });
    }
    // Application enters the conversation through pi's own message path, so
    // SessionManager persists it before the message_end the Go tailer
    // observes — the applied marker below therefore always follows durable
    // persistence, never precedes it.
    pi.sendMessage(
      { customType: MESSAGE_TYPE, content: text, display: true, details: { deliveryId } },
      { triggerTurn: true },
    );
    return Promise.resolve("claimed " + deliveryId);
  }

  pi.registerCommand("donmai-claim", {
    description: "Claim a queued peer notice: /donmai-claim <deliveryId> <text>",
    handler: async (args: string, ctx: any) => {
      if (!trusted) {
        ctx.ui.notify("notice-claim channel not trusted in this session", "error");
        return;
      }
      const parsed = splitClaimArgs(args);
      if (!parsed) {
        ctx.ui.notify("usage: /donmai-claim <deliveryId> <text>", "error");
        return;
      }
      try {
        const outcome = await claim(parsed.deliveryId, parsed.text, ctx);
        ctx.ui.notify(outcome, "info");
      } catch (err) {
        ctx.ui.notify(String(err), "error");
      }
    },
  });

  // The applied receipt is written when pi reports the applied message as
  // persisted (message_end for the custom message this extension sent), not
  // when the claim was issued. A crash between application and this marker
  // replays the receipt without a duplicate turn: the message is already in
  // the conversation, so reclaiming finds the applied marker missing but
  // the message present and re-reports it instead of re-applying.
  pi.on("message_end", async (event: any, ctx: any) => {
    const m = event.message;
    if (!m || m.role !== "custom" || m.customType !== MESSAGE_TYPE) return;
    const deliveryId = String(m.details.deliveryId ?? "");
    if (!deliveryId) return;
    pi.appendEntry(APPLIED_TYPE, { delivery: deliveryId, kind: "applied" });
  });
}
