// conformance-fixture.ts — additional-extension delivery conformance fixture
// for ADR-2026-08-12-pi-extension-delivery-seam-and-capability-pack-boundary.md
// (donmai-architecture). Delivered through Spec.AdditionalExtensions (D1),
// NEVER through the embedded donmai-policy.ts boundary path — this file
// stands in for a third-party capability pack loaded through the generic
// seam, so it deliberately knows nothing about donmai's own handshake
// protocol or UI marker.
//
// It proves, against the REAL pi binary (extension_delivery_real_binary_test.go):
//
//   - tool registration through the seam: pi.registerTool() succeeds for a
//     tool delivered by this route, exactly as it would for any other
//     extension (D1's "an injected pack's tools are native_tool_definition
//     entries" — the registration mechanism itself is ordinary pi API, only
//     the DELIVERY of the file is seam-specific).
//   - the headless-UI guarantee (D3): this extension is not the donmai
//     policy boundary, so it carries no declared answerer for a UI
//     round-trip. Attempting one anyway (ctx.ui.input with an ARBITRARY,
//     non-donmai placeholder) must resolve PROMPTLY — never hang — because
//     the runner cancels any extension_ui_request it does not recognize.
//     "Cancelled" is exactly the typed-refusal shape D3 requires: the
//     extension observes it and never blocks a tool call on it.
//
// Every observation is written to MARKER_PATH (env DONMAI_FIXTURE_MARKER) as
// one JSON object, so the Go test can assert on real subprocess behavior
// without needing to reverse-engineer a full model tool-calling round trip.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";
import { Type } from "typebox";

const MARKER_PATH = process.env.DONMAI_FIXTURE_MARKER ?? "";
const ARBITRARY_PLACEHOLDER = "conformance-fixture-arbitrary-marker-v1";

function record(entry: Record<string, unknown>) {
  if (!MARKER_PATH) return;
  try {
    writeFileSync(MARKER_PATH, JSON.stringify(entry));
  } catch {
    // The marker write is test plumbing, not extension behavior under test;
    // a failure here must never throw out of an event handler.
  }
}

export default function activate(pi: ExtensionAPI) {
  // Tool registration through the seam. A well-behaved tool delivered this
  // way completes withOUT any UI dependency at all — the D3 "complete
  // without an interactive surface" half of the contract, proven by simply
  // never touching ctx.ui.
  pi.registerTool({
    name: "donmai_fixture_tool",
    label: "Donmai Fixture Tool",
    description: "Conformance fixture tool for the additional-extension delivery seam.",
    parameters: Type.Object({}),
    async execute() {
      record({ toolExecuted: true });
      return { content: [{ type: "text", text: "fixture-tool-ok" }] };
    },
  });

  pi.on("session_start", (_event: unknown, ctx: any) => {
    const hasUI = ctx?.hasUI ?? null;
    record({ loaded: true, hasUI });
    if (!ctx?.hasUI) {
      // No UI attached at all (print/json mode): nothing to probe.
      return;
    }
    // Fire-and-forget, exactly like the boundary extension's own handshake
    // (donmai-policy.ts): awaiting inside session_start would stall pi's
    // own startup.
    void (async () => {
      const started = Date.now();
      try {
        const reply = await ctx.ui.input("conformance-fixture-probe", ARBITRARY_PLACEHOLDER);
        record({
          loaded: true,
          hasUI,
          uiRoundTripSettled: true,
          uiRoundTripMs: Date.now() - started,
          uiReply: reply ?? null,
        });
      } catch (err) {
        record({
          loaded: true,
          hasUI,
          uiRoundTripSettled: true,
          uiRoundTripMs: Date.now() - started,
          uiError: String(err),
        });
      }
    })();
  });
}
