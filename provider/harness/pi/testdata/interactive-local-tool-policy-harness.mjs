// interactive-local-tool-policy-harness.mjs — scripted (no real pi binary)
// conformance fixture for the interactive-lane allowed/disallowed-tools
// channel (agent.ToolDeliveryPiInteractiveLocalToolPolicy — manifest.go,
// agent/tool_adaptation.go). Proves ADR-2026-08-12 D3.1's "the fixture is
// written on its input" for THIS lane's new delivery: a stamped list must
// actually reach the policy extension's enforcement.
//
// This harness imports the REAL production extension
// (extensions/donmai-policy.ts) directly, never a copy, activates it with a
// stub ExtensionAPI that captures registered event handlers exactly the way
// the real pi runtime would (pi.on(event, handler)), then invokes the
// captured "tool_call" handler with one synthetic event built from argv —
// under the SAME env shape interactive.go's interactiveChildEnv sets
// (DONMAI_PI_ALLOWED_TOOLS / DONMAI_PI_DISALLOWED_TOOLS, DONMAI_PI_HANDSHAKE
// deliberately absent so rpcMode is false, same as the real interactive
// spawn). The verdict is printed as one JSON line on stdout so the Go test
// can assert on real extension behavior without spawning the real `pi`
// process — the real-binary fixture stays optional per this channel's scope
// (real-binary fixture optional/skip-clean; scripted fixture required).
//
// Usage: node interactive-local-tool-policy-harness.mjs <extensionPath> <toolName> <inputJSON>
// Env: DONMAI_PI_ALLOWED_TOOLS / DONMAI_PI_DISALLOWED_TOOLS (JSON arrays, may
// be absent or "[]"); DONMAI_PI_HANDSHAKE must NOT be set by the caller.

import { pathToFileURL } from "node:url";

const [, , extensionPath, toolName, inputJSON] = process.argv;

if (!extensionPath || !toolName) {
  console.error("usage: node interactive-local-tool-policy-harness.mjs <extensionPath> <toolName> <inputJSON>");
  process.exit(2);
}

const handlers = {};
const stubPi = {
  registerProvider() {},
  registerTool() {},
  on(event, handler) {
    handlers[event] = handler;
  },
};

const mod = await import(pathToFileURL(extensionPath).href);
mod.default(stubPi);

const handler = handlers["tool_call"];
if (!handler) {
  console.log(JSON.stringify({ registered: false, verdict: null }));
  process.exit(0);
}

let input = {};
if (inputJSON) {
  try {
    input = JSON.parse(inputJSON);
  } catch {
    input = {};
  }
}

const verdict = await handler({ toolName, input }, {});
console.log(JSON.stringify({ registered: true, verdict: verdict ?? null }));
