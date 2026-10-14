// notice-claim-harness.mjs — scripted conformance fixture for the
// host-owned applied-receipt extension (extensions/donmai-notice-claim.ts).
//
// It activates the REAL production extension directly (never a copy) with a
// stub ExtensionAPI that captures the registered command handler and the
// message_end handler exactly the way the real pi runtime would
// (pi.on(event, handler)), drives one claim through the captured
// `/donmai-claim` handler under a caller-supplied session entry list, and
// prints the resulting appends/sends/notifies as JSON.
//
// Usage:
//   node notice-claim-harness.mjs <extensionPath> <entriesJSON> <claimArgs>
// Env:
//   DONMAI_NOTICE_CLAIM_TRUST=1 enables the claim command (the verdict the
//   Go spawn stamps only after the trusted-set check); absent, every claim
//   is refused.
//
// The TUI canary (the Go test driving the real `pi` TUI) owns the
// end-to-end evidence; this fixture owns the fast, always-run contract:
// offer-before-apply ordering, idempotent reclaim, conflicting-payload
// refusal, untrusted refusal, and the applied marker following the applied
// message through message_end.

import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

// --- stripErasableTypeScript: reuse the policy harness's verified eraser
// rather than restating it. The eraser's scope contract (type-only
// imports, interface blocks, parameter/return annotations, `as`
// assertions) is the same subset donmai-notice-claim.ts commits to in its
// header. ---
const POLICY_HARNESS = new URL(
  "./interactive-local-tool-policy-harness.mjs",
  import.meta.url,
);
const harnessSource = readFileSync(POLICY_HARNESS, "utf8");
const stripStart = harnessSource.indexOf("function stripErasableTypeScript(src)");
const stripEnd = harnessSource.indexOf("// --- Harness proper ---");
if (stripStart < 0 || stripEnd < 0 || stripEnd <= stripStart) {
  console.error("notice-claim harness: cannot locate the stripper in the policy harness");
  process.exit(2);
}
const stripErasableTypeScript = new Function(
  harnessSource.slice(stripStart, stripEnd) + "\nreturn stripErasableTypeScript;",
)();

const [, , extensionPath, entriesJSON, claimArgs] = process.argv;
if (!extensionPath) {
  console.error("usage: node notice-claim-harness.mjs <extensionPath> <entriesJSON> <claimArgs>");
  process.exit(2);
}

const source = readFileSync(extensionPath, "utf8");
const stripped = stripErasableTypeScript(source);
try {
  execFileSync(process.execPath, ["--input-type=module", "--check"], {
    input: stripped,
    stdio: ["pipe", "pipe", "pipe"],
  });
} catch (err) {
  const detail = err.stderr ? err.stderr.toString() : String(err);
  throw new Error("stripped notice-claim extension is not valid JavaScript:\n" + detail);
}
const dataURL = "data:text/javascript;base64," + Buffer.from(stripped, "utf8").toString("base64");

let entries = [];
if (entriesJSON) {
  try {
    entries = JSON.parse(entriesJSON);
  } catch {
    entries = [];
  }
}

const appended = [];
const sent = [];
const notified = [];
const handlers = {};
const tools = {};
const stubPi = {
  appendEntry(customType, data) {
    appended.push({ customType, data });
    entries.push({ type: "custom", customType, data });
  },
  sendMessage(message, options) {
    sent.push({ message, options });
  },
  registerCommand(name, options) {
    handlers["command:" + name] = options.handler;
  },
  registerTool(tool) {
    tools[tool.name] = tool;
  },
  on(event, handler) {
    handlers[event] = handler;
  },
};

const mod = await import(dataURL);
await mod.default(stubPi);

const claim = handlers["command:donmai-claim"];
if (!claim) {
  console.log(JSON.stringify({ registered: false }));
  process.exit(0);
}
const ctx = {
  ui: {
    notify(msg, kind) {
      notified.push({ msg: String(msg), kind: String(kind ?? "") });
    },
  },
  sessionManager: {
    getEntries() {
      return entries;
    },
  },
};
await claim(claimArgs ?? "", ctx);

// Fire the captured message_end handler for every applied custom message
// the claim sent, modelling pi persisting the message and reporting it.
const endHandler = handlers["message_end"];
const appliedMarkers = [];
if (endHandler) {
  for (const s of sent) {
    const m = s.message;
    if (m && m.customType === "donmai-notice") {
      const before = appended.length;
      await endHandler(
        { message: { role: "custom", customType: "donmai-notice", details: m.details } },
        ctx,
      );
      appliedMarkers.push(appended.slice(before));
    }
  }
}

console.log(
  JSON.stringify({ registered: true, appended, sent, notified, appliedMarkers }),
);
