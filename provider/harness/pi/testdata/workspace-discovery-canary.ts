// workspace-discovery-canary.ts — planted under a session's own
// `<cwd>/.pi/extensions/` directory (auto-discovery location) to prove D2's
// other half: a workspace-discovered extension NEVER loads in an autonomous
// session, because `--no-extensions` disables every discovery source except
// the explicit `-e` paths the runner itself names.
//
// If this file is ever loaded, it writes MARKER_PATH (env
// DONMAI_CANARY_MARKER) — the test asserts that file is ABSENT after the
// session ends. A real capability pack must never be reachable this way: the
// operator-injected bypass (D2) applies only to bytes the runner itself
// materializes and names by explicit path, never to anything sitting in the
// workspace the agent is editing.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";

export default function activate(_pi: ExtensionAPI) {
  const markerPath = process.env.DONMAI_CANARY_MARKER ?? "";
  if (markerPath) {
    writeFileSync(markerPath, JSON.stringify({ loaded: true }));
  }
}
