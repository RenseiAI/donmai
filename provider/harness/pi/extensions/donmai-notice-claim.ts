// donmai-notice-claim — host-owned applied-receipt extension for Pi
// interactive sessions.
//
// This file is the TRUSTED-extension half of the interactive notice receipt
// contract (docs/agents/PI-INTERACTIVE-NOTICES.md). The host loads it
// through the SAME explicit `-e` path as the embedded policy extension —
// never through workspace discovery (`--no-extensions` stays on) — and
// only when its measured identity matches the compiled embedder's
// trusted set (Options.TrustedExtensions): untrusted sessions never
// register `/donmai-claim` at all. An arbitrary peer
// AdditionalExtension cannot mint its receipts: the applied marker is
// written only when the same session holds this extension's own persisted
// offer for that delivery (same ID, same text) AND the spawn proved the
// trust verdict for that event — and the session file is read through the
// extension runtime's own session manager, not through a transcript or
// mailbox file the peer can write.
//
// Mechanism (verified against the real pinned binary):
//
//   - Claim: the `/donmai-claim <deliveryId> <text>` slash command persists
//     a `donmai-notice-offer` custom entry binding (delivery, text) FIRST —
//     so a crash before application replays as unapplied work — then
//     applies the notice into the conversation as a `donmai-notice` custom
//     message via `pi.sendMessage`, which pi persists through
//     SessionManager before it emits the `message_end` the Go tailer
//     observes. The `message_end` handler then persists the
//     `donmai-notice-applied` receipt binding the delivery to the
//     persisted entry. The receipt therefore exists only after durable
//     conversation persistence, never before it. Only a non-empty text
//     binds: a textless offer entry — plantable through the same-user
//     session file — is skipped by both halves, so it can neither mint a
//     receipt nor wedge the claim into a conflicting-payload refusal.
//   - Idempotence: a repeated claim for an already-applied delivery ID
//     reports "already applied" and writes nothing — no duplicate turn.
//     A conflicting payload under the same ID is refused. A reclaim that
//     finds the offer AND the persisted `donmai-notice` message but no
//     applied marker — the crash-between-apply-and-marker window —
//     appends the receipt only and reports "already applied": the
//     receipt still follows durable persistence, because the persisted
//     message IS the persistence it attests.
//   - Recovery: an offer with no applied marker and no persisted message
//     is unapplied work (reclaim it); a persisted message with no marker
//     replays the receipt (apply nothing); an offer with a marker is
//     complete (replay the receipt, apply nothing).
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
  // Entries are same-user writable session state: a malformed entry (no
  // .data, non-object .data) is skipped, never trusted and never fatal.
  // A peer that wedges a dataless entry into the session file gets the
  // normal claim/refusal path, not a TypeError that wedges the channel.
  return entries.find(
    (e: any) =>
      e && e.type === "custom" && e.customType === customType && e.data && e.data.delivery === deliveryId,
  );
}

function appliedReceipt(entries: any, deliveryId: string) {
  return findEntry(entries, APPLIED_TYPE, deliveryId);
}

function offerTextOf(entry: any) {
  const text = entry && entry.data && entry.data.text;
  return typeof text === "string" ? text : "";
}

// An offer binds (delivery, text) only when the text is actually persisted.
// A textless entry — plantable through the same-user session file or the
// shared appendEntry surface — is inert: skipped by both halves, so it can
// neither satisfy the receipt half nor wedge the claim half into a
// conflicting-payload refusal that blocks the legitimate claim.
function findValidOffer(entries: any, deliveryId: string) {
  return entries.find(
    (e: any) =>
      e && e.type === "custom" && e.customType === OFFER_TYPE && e.data && e.data.delivery === deliveryId && offerTextOf(e) !== "",
  );
}

// The persisted text of an applied message. The runtime normalizes a
// missing/null content to [] at ingestion, so a contentless event arrives
// as a non-string: it normalizes to "" and can never satisfy the receipt.
function messageTextOf(m: any) {
  if (!m) return "";
  if (typeof m.content === "string") return m.content;
  if (Array.isArray(m.content)) {
    let out = "";
    for (const part of m.content) {
      if (part && part.type === "text" && typeof part.text === "string") out += part.text;
    }
    return out;
  }
  return "";
}

// A persisted application of (delivery, text): the custom_message entry
// pi's own sendMessage path wrote before emitting message_end. Crash
// recovery keys on it: offer plus message without an applied marker means
// the application survived and only the receipt is missing — replay the
// receipt, never re-apply.
function findAppliedMessage(entries: any, deliveryId: string, text: string) {
  return entries.find(
    (e: any) =>
      e && e.type === "custom_message" && e.customType === MESSAGE_TYPE && e.details && e.details.deliveryId === deliveryId && messageTextOf(e) === text,
  );
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
  // The trust verdict is read per event, never cached at activation: every
  // AdditionalExtension shares this process, so a peer can mutate
  // process.env after load, and a session activated before the spawn
  // stamped the verdict must not inherit authority it never proved. Both
  // halves below — the claim command and the receipt writer — consult it
  // on every invocation.
  function isTrusted() {
    return process.env.DONMAI_NOTICE_CLAIM_TRUST === "1";
  }
  function claim(deliveryId: string, text: string, ctx: any) {
    const entries = entriesOf(ctx);
    const already = appliedReceipt(entries, deliveryId);
    if (already) return Promise.resolve("already applied " + deliveryId);
    const offer = findValidOffer(entries, deliveryId);
    if (offer) {
      if (offerTextOf(offer) !== text) {
        return Promise.reject(new Error("delivery " + deliveryId + " already offered with a different payload"));
      }
    } else {
      // Persist the offer BEFORE applying: a crash between the two replays
      // as unapplied work, never as a lost application.
      pi.appendEntry(OFFER_TYPE, { delivery: deliveryId, text });
    }
    // Crash between application and the applied marker: the message is
    // already durably persisted, so exact-replay the receipt instead of
    // re-applying a duplicate turn. The receipt still follows durable
    // persistence — the persisted message IS the persistence it attests.
    if (findAppliedMessage(entries, deliveryId, text)) {
      pi.appendEntry(APPLIED_TYPE, { delivery: deliveryId, kind: "applied" });
      return Promise.resolve("already applied " + deliveryId);
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
      if (!isTrusted()) {
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
  // persisted (message_end for the custom message this extension sent),
  // not when the claim was issued — and only when the event carries the
  // same non-empty text the persisted offer binds, so a contentless event
  // mints nothing. A crash between application and this marker replays
  // the receipt without a duplicate turn: the message is already in the
  // conversation, so reclaiming finds the applied marker missing but the
  // message present and re-reports the receipt instead of re-applying.
  //
  // The receipt half consults the same trust verdict per event AND
  //  requires a matching offer this extension's own claim step persisted
  //  (same delivery ID, same non-empty text) AND non-empty text on the
  //  event itself: a peer sharing this process and the pi.sendMessage
  //  surface can emit its own donmai-notice message, but without an
  //  offer for that delivery the persistence event mints nothing —
  //  and without the verdict, with a mismatched payload, with a textless
  //  planted offer, or with a contentless event, it mints nothing either. A redelivered message_end
  // for an already-applied delivery replays the existing receipt instead
  // of stamping a second, so the receipt stream stays exactly-one per
  // delivery. The receipt therefore attests persistence of an offered
  // (delivery, text), not authorship identity: a peer sharing this
  // process can still invoke the claim command itself while the verdict
  // holds, so per-action authorship binding awaits a pi upstream
  // primitive. What this fence does prove — and the fixtures pin — is
  // that no peer can mint or replay the receipt WITHOUT the persisted
  // offer and the spawn-stamped verdict: the receipt never precedes the
  // offer, never exceeds one per delivery, and never issues untrusted.
  pi.on("message_end", async (event: any, ctx: any) => {
    if (!isTrusted()) return;
    const m = (event && event.message) || null;
    if (!m || m.role !== "custom" || m.customType !== MESSAGE_TYPE) return;
    const details = m.details || {};
    const deliveryId = String(details.deliveryId ?? "");
    if (!deliveryId) return;
    // Both sides must carry persisted content: the event must carry
    // non-empty text AND a valid offer must bind the same non-empty text.
    // A textless planted offer is never honored, and a contentless event
    // mints nothing — the receipt attests an offered (delivery, text) the
    // session actually persisted, on both sides of the gate.
    const messageText = messageTextOf(m);
    if (!messageText) return;
    const entries = entriesOf(ctx);
    const offer = findValidOffer(entries, deliveryId);
    if (!offer) return;
    if (offerTextOf(offer) !== messageText) return;
    if (appliedReceipt(entries, deliveryId)) return;
    pi.appendEntry(APPLIED_TYPE, { delivery: deliveryId, kind: "applied" });
  });
}
