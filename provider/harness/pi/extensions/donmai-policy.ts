// donmai policy extension — the trust boundary for the pi harness.
//
// pi ships NO permission system, no sandbox, no MCP: tools run with the full
// permissions of the spawning user. This extension is the ENTIRE trust
// boundary, owned by the orchestrator. It is shipped INSIDE the donmai binary
// via go:embed and loaded per session with `pi --mode rpc -e <path>` (a CLI
// extension, which loads regardless of project trust — a project-local
// `.pi/extensions` copy would NOT load in an untrusted worktree, so the
// harness passes the materialized path with `-e`). Its bytes are SHA-verified
// against the embedded payload at handshake time; a mismatch fails the session
// closed.
//
// Mechanism (verified against the real pinned binary @earendil-works/
// pi-coding-agent@0.80.10, docs/rpc.md + docs/extensions.md):
//
//   - Handshake: at `session_start` the extension reads a per-session secret
//     token from the DONMAI_PI_HANDSHAKE env var the harness set on the child,
//     hashes its own on-disk source (import.meta.url -> sha256), and sends both
//     back to the Go side over a `ctx.ui.input` round-trip (which pi turns
//     into an extension_ui_request / extension_ui_response exchange on stdio).
//     The Go side verifies the token (per-session liveness/identity) AND the
//     SHA (integrity vs the embedded payload) and replies. Until it replies
//     "ok" the extension refuses every tool call. The Go side, for its part,
//     never sends the prompt until it observes and verifies this round-trip —
//     so a missing / stale / tampered extension fails the session closed.
//
//   - Tool adjudication: the `tool_call` event fires before every built-in
//     tool executes and can block. For each guarded tool the handler sends the
//     intended call to the Go side over the same `ctx.ui.input` channel; the
//     Go policy engine (policy.go) answers allow / deny+reason; on deny the
//     handler returns { block: true, reason } so pi blocks the tool and the
//     model sees WHY.
//
//   - Provider pin: at load the factory registers a single "donmai" provider
//     from env (baseUrl / api / key / model), so the session can only reach
//     the resolved cell endpoint. The key is read from process.env at runtime
//     and never written to disk or inlined in this source (so the source SHA
//     stays stable and verifiable).
//
// This file is intentionally dependency-free (node builtins + a type-only
// import) and brand-neutral.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";

// The wire marker that identifies this extension's UI round-trips to the Go
// side (carried in the extension_ui_request `placeholder`). The Go handler
// dispatches only requests carrying it and cancels anything else.
const DONMAI_UI_MARKER = "donmai-policy-v1";

// Discriminators inside the JSON payload the Go side reads from the request
// `title`.
const KIND_HANDSHAKE = "handshake";
const KIND_ADJUDICATE = "adjudicate";

// Built-in tools this extension guards. Every one routes through the Go-side
// policy engine before it may execute.
const GUARDED_TOOLS = new Set(["read", "write", "edit", "bash", "grep", "find", "ls"]);

// selfSHA256 hashes this extension's own on-disk source so the Go side can
// verify the exact bytes it materialized are the bytes that loaded.
function selfSHA256(): string {
  try {
    const path = fileURLToPath(import.meta.url);
    return createHash("sha256").update(readFileSync(path)).digest("hex");
  } catch {
    return "";
  }
}

export default function activate(pi: ExtensionAPI) {
  const token = process.env.DONMAI_PI_HANDSHAKE ?? "";
  const sha = selfSHA256();

  // verified flips true only once the Go side acknowledges the handshake. Until
  // then every tool call is blocked — the boundary is fail-closed even if the
  // Go side is slow to answer or the handshake is rejected.
  let verified = false;
  let handshakeSettled = false;

  // Provider pin: register the single "donmai" provider from env so the session
  // can only route to the resolved cell. Key is read at runtime, never inlined.
  const baseUrl = process.env.DONMAI_PI_BASE_URL ?? "";
  const api = process.env.DONMAI_PI_API ?? "openai-completions";
  const model = process.env.DONMAI_PI_MODEL ?? "";
  const apiKey = process.env.DONMAI_PI_KEY ?? "";
  if (baseUrl && model) {
    try {
      pi.registerProvider("donmai", {
        baseUrl,
        apiKey,
        api: api as never,
        models: [
          {
            id: model,
            name: model,
            reasoning: true,
            input: ["text"],
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
            contextWindow: 200000,
            maxTokens: 16384,
          },
        ],
      });
    } catch {
      // A registration failure must not run the session unguarded; the Go side
      // still gates on the handshake, and tool calls stay blocked until
      // verified.
    }
  }

  // Handshake at session_start. Fire-and-forget: awaiting a ctx.ui round-trip
  // inside the awaited session_start handler stalls pi's startup, so the
  // round-trip runs on its own microtask and session_start resolves at once.
  pi.on("session_start", (_event: unknown, ctx: any) => {
    if (!ctx?.hasUI) return;
    void (async () => {
      try {
        const payload = JSON.stringify({ donmai: KIND_HANDSHAKE, token, sha });
        const reply = await ctx.ui.input(payload, DONMAI_UI_MARKER);
        verified = String(reply ?? "") === "ok";
        handshakeSettled = true;
        if (!verified) {
          // The Go side rejected us — do not run unguarded.
          ctx.ui.notify?.("donmai policy handshake rejected", "error");
        }
      } catch {
        handshakeSettled = true;
        verified = false;
      }
    })();
  });

  // Tool adjudication. tool_call can block; we await the Go verdict.
  pi.on("tool_call", async (event: any, ctx: any) => {
    const tool = String(event?.toolName ?? "");
    if (!GUARDED_TOOLS.has(tool)) return;

    // Fail closed: an unverified boundary blocks every guarded tool. If the
    // handshake has not even settled yet, block too — the Go side gates the
    // prompt on the handshake, so this only guards against races.
    if (!verified) {
      return {
        block: true,
        reason: handshakeSettled
          ? "donmai policy boundary not verified"
          : "donmai policy boundary still initializing",
      };
    }
    if (!ctx?.hasUI) {
      return { block: true, reason: "donmai policy boundary requires a UI channel" };
    }

    try {
      const payload = JSON.stringify({
        donmai: KIND_ADJUDICATE,
        token,
        toolName: tool,
        toolCallId: String(event?.toolCallId ?? ""),
        input: event?.input ?? {},
        cwd: ctx?.cwd ?? "",
      });
      const verdict = await ctx.ui.input(payload, DONMAI_UI_MARKER);
      const decision = JSON.parse(String(verdict ?? "{}")) as {
        allow?: boolean;
        reason?: string;
      };
      if (!decision.allow) {
        return { block: true, reason: decision.reason || "denied by donmai policy" };
      }
      // allow: returning undefined lets the tool execute.
      return;
    } catch (err) {
      return { block: true, reason: "donmai policy adjudication failed: " + String(err) };
    }
  });
}
