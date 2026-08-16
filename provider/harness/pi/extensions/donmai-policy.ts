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
//   - Interactive-lane local tool policy (allowed/disallowed-tools channel,
//     agent.ToolDeliveryPiInteractiveLocalToolPolicy): the PTY lane runs no
//     RPC round trip at all (see activate() below), so it cannot ask the Go
//     side to adjudicate. It can still answer a stamped
//     AllowedTools/DisallowedTools list, though — the list is carried onto
//     the child env as DONMAI_PI_ALLOWED_TOOLS/DONMAI_PI_DISALLOWED_TOOLS
//     (interactive.go's interactiveChildEnv) and matched entirely LOCALLY,
//     in this process, against every guarded tool_call. This is narrower
//     than the full policy engine on purpose: no safety-deny regexes, no
//     path containment, no PermissionConfig regex/default-decision handling
//     — those stay Unsupported on the interactive profile because they need
//     the Go round trip this lane does not run.
//
//   - Provider pin: at load the factory registers a single "donmai" provider
//     from env (baseUrl / api / key / model, plus an optional context-window
//     size), so the session can only reach the resolved cell endpoint. The
//     key is read from process.env at runtime and never written to disk or
//     inlined in this source (so the source SHA stays stable and verifiable).
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
// policy engine before it may execute (RPC mode) or the local matcher below
// (interactive PTY mode, allowed/disallowed-tools channel only).
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

// --- Interactive-lane local tool policy (allowed/disallowed-tools channel) ---
//
// A minimal, LOCAL port of policy.go's toolPattern grammar, deliberately
// scoped to exactly the two Spec fields (AllowedTools/DisallowedTools) the
// interactive profile's NativeToolPolicyDelivery now answers
// (ToolDeliveryPiInteractiveLocalToolPolicy — manifest.go). Narrower than
// policy.go's toolPattern.matches on purpose: a pattern name that is not one
// of GUARDED_TOOLS never matches here (policy.go's "anyKind" wildcard for an
// unrecognized name is a Go-side nuance this local, no-round-trip mechanism
// does not replicate — the asymmetry is declared, not smoothed, per
// ADR-2026-08-06 D6 / ADR-2026-08-12 D3.2's "declared, not smoothed" rule for
// headless-vs-interactive evidence).
const DONMAI_ALLOWED_TOOLS_ENV = "DONMAI_PI_ALLOWED_TOOLS";
const DONMAI_DISALLOWED_TOOLS_ENV = "DONMAI_PI_DISALLOWED_TOOLS";

interface LocalToolPattern {
  raw: string;
  name: string; // lowercased tool name, e.g. "bash"
  constraint: string | null; // stripped of a trailing ":*"/"*"; null == no constraint
}

// parseLocalToolPatterns parses a JSON-encoded array of Claude-grammar tool
// designators (e.g. ["Bash(git:*)", "Read"]) — the SAME strings
// agent/tool_adaptation.go's toolDesignatorRe validates before this ever
// runs. Malformed JSON or a non-array value yields no patterns (fail
// CLOSED for DisallowedTools — nothing to match means nothing is blocked by
// this local gate — and the same emptiness for AllowedTools simply means no
// allow-gate is configured, matching parseToolPatterns' empty-list shape in
// policy.go).
function parseLocalToolPatterns(envValue: string | undefined): LocalToolPattern[] {
  if (!envValue) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(envValue);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: LocalToolPattern[] = [];
  for (const entry of parsed) {
    if (typeof entry !== "string") continue;
    const trimmed = entry.trim();
    if (!trimmed) continue;
    const match = /^([A-Za-z_][A-Za-z0-9_]*)(?:\((.*)\))?$/.exec(trimmed);
    if (!match) continue;
    let constraint: string | null = null;
    if (match[2] !== undefined) {
      constraint = match[2].replace(/:?\*$/, "");
    }
    out.push({ raw: trimmed, name: match[1].toLowerCase(), constraint });
  }
  return out;
}

// localToolPolicySubject mirrors policy.go's ToolCall.subject(): the bash
// command text for bash, the raw path/file/filename input for file ops. This
// never resolves the path against cwd (no Go round trip exists to do that
// safely from this process), so a constraint here is a plain prefix check
// against the tool's raw argument — resolved-path containment stays a
// policy.go-only, RPC-mode-only property.
function localToolPolicySubject(tool: string, input: Record<string, unknown> | undefined): string {
  if (tool === "bash") return String(input?.command ?? "").trim();
  return String(input?.path ?? input?.file ?? input?.filename ?? "");
}

function matchesLocalPattern(pattern: LocalToolPattern, tool: string, subject: string): boolean {
  if (pattern.name !== tool) return false;
  if (!pattern.constraint) return true;
  return subject.startsWith(pattern.constraint);
}

// evaluateLocalToolPolicy is the interactive-lane (PTY, no RPC round trip)
// answer on the allowed/disallowed-tools channel. Order mirrors policy.go's
// Evaluate steps 3 and 5 (DisallowedTools first, then the AllowedTools
// allow-gate), with every other step (safety-deny, containment,
// PermissionConfig, network-bash default-deny) intentionally absent — those
// channels stay Unsupported on the interactive profile. Exported for the
// scripted conformance fixture (testdata/interactive-local-tool-policy-harness.mjs),
// which imports this module directly and invokes it without a real pi
// process.
export function evaluateLocalToolPolicy(
  allowed: LocalToolPattern[],
  disallowed: LocalToolPattern[],
  tool: string,
  input: Record<string, unknown> | undefined,
): { block: true; reason: string } | undefined {
  const normalizedTool = tool.toLowerCase();
  const subject = localToolPolicySubject(normalizedTool, input);

  for (const pattern of disallowed) {
    if (matchesLocalPattern(pattern, normalizedTool, subject)) {
      return { block: true, reason: "tool call matches a disallowed-tools pattern: " + pattern.raw };
    }
  }
  if (allowed.length > 0) {
    const allowedMatch = allowed.some((pattern) => matchesLocalPattern(pattern, normalizedTool, subject));
    if (!allowedMatch) {
      return { block: true, reason: "no allow pattern matched and an allow-list is configured" };
    }
  }
  return undefined;
}

export default function activate(pi: ExtensionAPI) {
  // The harness sets DONMAI_PI_HANDSHAKE only for the headless RPC lane. Its
  // PRESENCE is what distinguishes the two spawn modes to this extension:
  //
  //   - RPC mode (token set): the Go harness drives pi over `--mode rpc` and
  //     consumes ctx.ui round-trips as extension_ui_request/response frames on
  //     stdio. The handshake + per-call adjudication below run, and the boundary
  //     is fail-closed (no tool executes until the Go side verifies us).
  //
  //   - Interactive PTY mode (token absent): the bare `pi` TUI is attached to a
  //     human, there is NO Go RPC consumer, and a ctx.ui round-trip would render
  //     a raw JSON prompt AT the human's terminal — a UI artifact — while
  //     blocking every tool on a verdict that can never arrive. So the handshake
  //     and adjudication are SKIPPED; the human at the terminal plus pi's own
  //     native approval UI is the tool authority. The pi/interactive
  //     tool-lifecycle profile declares exactly this injected-boundary gap.
  //
  // Provider registration below is UNCONDITIONAL either way — it needs no RPC,
  // and it is what points the session at the resolved cell endpoint in BOTH
  // modes.
  const token = process.env.DONMAI_PI_HANDSHAKE ?? "";
  const rpcMode = token !== "";

  // Provider pin: register the single "donmai" provider from env so the session
  // can only route to the resolved cell. Key is read at runtime, never inlined.
  const baseUrl = process.env.DONMAI_PI_BASE_URL ?? "";
  const api = process.env.DONMAI_PI_API ?? "openai-completions";
  const model = process.env.DONMAI_PI_MODEL ?? "";
  const apiKey = process.env.DONMAI_PI_KEY ?? "";
  // Context-window pin: the harness exports the resolved profile's
  // context-window size (tokens) as DONMAI_PI_CONTEXT_WINDOW when the
  // dispatch carried one (extension.go providerPinEnv). A missing or invalid
  // value falls back to the historical 200000 default, so an unpinned
  // session keeps prior behaviour while a pinned 1M-context model is no
  // longer silently clamped to it.
  const contextWindowEnv = Number(process.env.DONMAI_PI_CONTEXT_WINDOW ?? "");
  const contextWindow =
    Number.isInteger(contextWindowEnv) && contextWindowEnv > 0 ? contextWindowEnv : 200000;
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
            contextWindow,
            maxTokens: 16384,
          },
        ],
      });
    } catch {
      // A registration failure must not run the session unguarded; in RPC mode
      // the Go side still gates on the handshake, and tool calls stay blocked
      // until verified.
    }
  }

  // Interactive PTY mode: no handshake, no RPC-backed blocking adjudication
  // (see above) — but the allowed/disallowed-tools channel is still real
  // here. A stamped list is matched LOCALLY, with no round trip, against
  // every guarded tool_call (evaluateLocalToolPolicy above). The handler is
  // registered only when at least one list is actually stamped, so a session
  // with neither carries no local gate at all — same shape as RPC mode's own
  // "nothing configured, nothing blocked" default.
  if (!rpcMode) {
    const allowed = parseLocalToolPatterns(process.env[DONMAI_ALLOWED_TOOLS_ENV]);
    const disallowed = parseLocalToolPatterns(process.env[DONMAI_DISALLOWED_TOOLS_ENV]);
    if (allowed.length > 0 || disallowed.length > 0) {
      pi.on("tool_call", (event: any) => {
        const tool = String(event?.toolName ?? "").toLowerCase();
        if (!GUARDED_TOOLS.has(tool)) return;
        return evaluateLocalToolPolicy(allowed, disallowed, tool, event?.input ?? {});
      });
    }
    // Provider registration already ran, which is everything else this mode
    // needs from us — no handshake, no Go-side adjudication round trip.
    return;
  }

  const sha = selfSHA256();

  // verified flips true only once the Go side acknowledges the handshake. Until
  // then every tool call is blocked — the boundary is fail-closed even if the
  // Go side is slow to answer or the handshake is rejected.
  let verified = false;
  let handshakeSettled = false;

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
