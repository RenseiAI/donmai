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
//   - Refusal registration: pi emits a tool_execution_end for a BLOCKED call
//     exactly as it does for an executed one, so a refusal this extension
//     reached on its own — an unverified boundary, or an adjudication
//     round-trip that threw — would reach the Go monitor as a call with no
//     recorded outcome, indistinguishable from a bypass. Each such refusal is
//     therefore registered first, over a bounded best-effort round-trip
//     (KIND_REFUSAL), and blocked afterwards. The block never depends on the
//     registration landing; a Go side that cannot answer must not be able to
//     wedge a blocking hook.
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
//   - Sequential shell and file-write tools: bash, write and edit are
//     re-registered under their own names with executionMode "sequential",
//     so a batch of tool calls from one assistant message that includes any
//     of them runs one call at a time, in order. Ordering only, layered under
//     the boundary: see registerSequentialTools below.
//
//   - Provider pin: at load the factory registers a single "donmai" provider
//     from env (baseUrl / api / model, plus an optional context-window
//     size) and the session credential file (the key). The key is read from
//     the file named by DONMAI_PI_CREDENTIALS_FILE at runtime and never
//     written to disk or inlined in this source (so the source SHA stays
//     stable and verifiable); the file path rides env, never the key.
//
//   - Session environment: the harness execs pi with an allowlisted
//     environment only, because pi renames its process and a same-user
//     process listing then renders the exec-time environment as its command
//     line. The session's other bindings (a VCS or tracker token, a toolchain
//     home) ride the same owner-only credential file, in its "environment"
//     section. The factory restores them into this process's environment
//     FIRST, before anything else loads, so every tool pi starts inherits
//     them, while the listing — which shows only the exec-time block — never
//     does. The model credentials in the "credentials" section are never
//     restored: the provider pin reads the one key it needs directly.
//
// This file is intentionally dependency-free (node builtins + a type-only
// import) and brand-neutral. The one exception is a guarded dynamic import of
// the host pi package's own tool factories for the sequential-tools override;
// it runs after every policy handler is registered, and nothing in the
// boundary depends on it resolving (see registerSequentialTools).

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";
import { basename, isAbsolute, join, normalize, sep } from "node:path";

// The wire marker that identifies this extension's UI round-trips to the Go
// side (carried in the extension_ui_request `placeholder`). The Go handler
// dispatches only requests carrying it and cancels anything else.
const DONMAI_UI_MARKER = "donmai-policy-v1";

// Discriminators inside the JSON payload the Go side reads from the request
// `title`.
const KIND_HANDSHAKE = "handshake";
const KIND_ADJUDICATE = "adjudicate";
// KIND_REFUSAL carries a refusal this extension reached ON ITS OWN — before a
// verdict could be asked for, or because asking failed. The tool is blocked
// either way; the round-trip exists so the Go side records the refusal as that
// call's outcome instead of meeting the tool_execution_end with no record at
// all (policy.go / handle.go, doc.go "The fail-safe fence").
const KIND_REFUSAL = "refusal";

// REFUSAL_REGISTER_TIMEOUT_MS bounds the best-effort refusal round-trip. The
// refusal itself never depends on it: the tool is blocked whether or not the
// Go side answers, and a Go side that is gone (or was never listening) must
// not be able to wedge a blocking tool_call hook forever.
const REFUSAL_REGISTER_TIMEOUT_MS = 2000;

// --- Bounded shell-tool execution (one long tool call must not end the session) ---
//
// A dispatched seat ran a whole-filesystem search; the command ran past the
// session's no-progress watchdog and the healthy session was ended, not just
// the command. Two rails bound that failure here, at the last point that runs
// before every guarded execution:
//
//   - DEFAULT_TOOL_CALL_TIMEOUT_SECONDS (300): a bash call with no timeout,
//     an invalid timeout, or a timeout above the bound runs with the bound
//     instead. pi's bash tool kills the command at the timeout and reports
//     "Command timed out after N seconds" as a tool ERROR — the agent sees
//     the failure and continues, and the session never waits on one call long
//     enough to trip its own idle timer. 300 stays below the runner's
//     12-minute no-progress window with margin for model round trips around
//     the call.
//   - withPipefailPrelude: a piped gate (`gate 2>&1 | tail -5`) otherwise
//     reports tail's exit code, not the gate's. The prelude is idempotent and
//     carries a `2>/dev/null` guard so a non-bash fallback shell runs the
//     command unchanged instead of failing on an unknown option.
//
// Both are applied by mutating event.input in place after an allow verdict;
// pi's extension docs guarantee input mutations affect the actual execution.
// Exported for the scripted conformance fixture
// (testdata/tool-call-bounds-harness.mjs), which invokes them against this
// exact source without a running pi process.
const DEFAULT_TOOL_CALL_TIMEOUT_SECONDS = 300;

export function resolveBashTimeoutSeconds(timeout: unknown): number {
  if (typeof timeout !== "number" || !Number.isFinite(timeout) || timeout <= 0) {
    return DEFAULT_TOOL_CALL_TIMEOUT_SECONDS;
  }
  return Math.min(timeout, DEFAULT_TOOL_CALL_TIMEOUT_SECONDS);
}

const PIPEFAIL_PRELUDE = "set -o pipefail 2>/dev/null; ";

export function withPipefailPrelude(command: unknown): string {
  const text = String(command ?? "");
  if (text.trimStart().startsWith("set -o pipefail")) {
    return text;
  }
  return PIPEFAIL_PRELUDE + text;
}

// Built-in tools this extension guards. Every one routes through the Go-side
// policy engine before it may execute (RPC mode) or the local matcher below
// (interactive PTY mode, allowed/disallowed-tools channel only).
const GUARDED_TOOLS = new Set(["read", "write", "edit", "bash", "grep", "find", "ls"]);

// toolCallIdOf reads a tool call's id from a pi event. It accepts the SAME
// spellings, in the same order, that the Go side accepts when it reads the id
// back off tool_execution_end (policy.go callIDFieldNames). Keeping one rule
// on both sides is load-bearing: a writer that only ever serialized
// `toolCallId` while the reader also accepted `callId`/`call_id`/`id` would
// leave an honoured ruling unrecorded the moment the runtime spelled the
// field any other way.
function toolCallIdOf(event: any): string {
  const candidates = [event?.toolCallId, event?.callId, event?.call_id, event?.id];
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate !== "") return candidate;
    if (typeof candidate === "number") return String(candidate);
  }
  return "";
}

// registerRefusal tells the Go side that this extension refused one call, so
// the refusal is recorded as that call's adjudication outcome. Best effort by
// construction: it is used exactly on the paths where the normal adjudication
// round-trip is unavailable or has already failed, so it must never throw and
// never block past REFUSAL_REGISTER_TIMEOUT_MS. The caller blocks the tool
// regardless of whether this lands.
async function registerRefusal(ctx: any, token: string, tool: string, callId: string, reason: string): Promise<void> {
  if (!ctx?.hasUI || typeof ctx?.ui?.input !== "function") return;
  let timer: any;
  try {
    const payload = JSON.stringify({
      donmai: KIND_REFUSAL,
      token,
      toolName: tool,
      toolCallId: callId,
      reason,
      cwd: ctx?.cwd ?? "",
    });
    await Promise.race([
      ctx.ui.input(payload, DONMAI_UI_MARKER),
      new Promise((resolve) => {
        timer = setTimeout(resolve, REFUSAL_REGISTER_TIMEOUT_MS);
        timer?.unref?.();
      }),
    ]);
  } catch {
    // Unreachable Go side, no UI channel, a rejected round-trip: the refusal
    // stands either way. The Go side records the call as unproven instead,
    // which is non-fatal there.
  } finally {
    if (timer) clearTimeout(timer);
  }
}

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
// DONMAI_CREDENTIALS_FILE_ENV names the session credential file the Go
// harness writes before spawn: a versioned JSON envelope of {env, value}
// entries (credential_file.go). The path rides the child env; the key bytes
// never do. readSessionCredential returns the entry filed under the injected
// provider's env name, or "" when the file is absent, unreadable, or holds
// no entry for it — a keyless session registers the provider with an empty
// key, exactly as it did when the key rode env unset.
const DONMAI_CREDENTIALS_FILE_ENV = "DONMAI_PI_CREDENTIALS_FILE";
const DONMAI_INJECTED_KEY_ENV = "DONMAI_PI_KEY";

// readSessionCredentialEnvelope parses the session credential file named by
// DONMAI_CREDENTIALS_FILE_ENV. Any failure — no path, unreadable file,
// malformed JSON — reads as null, never throws: delivery is best-effort at
// this layer, and the Go side already refused the spawn when writing the
// file failed.
function readSessionCredentialEnvelope(): any {
  try {
    const path = (process.env[DONMAI_CREDENTIALS_FILE_ENV] ?? "").trim();
    if (!path) return null;
    return JSON.parse(readFileSync(path, "utf8"));
  } catch {
    return null;
  }
}

// readSessionCredential returns the session key filed under the injected
// provider's env name in the envelope's credentials section, or "" when the
// envelope is absent or holds no entry for it — a keyless session registers
// the provider with an empty key, exactly as it did when the key rode env
// unset.
function readSessionCredential(envelope: any): string {
  const entries = envelope?.credentials;
  if (!Array.isArray(entries)) return "";
  for (const entry of entries) {
    if (entry?.env === DONMAI_INJECTED_KEY_ENV && typeof entry?.value === "string") {
      return entry.value;
    }
  }
  return "";
}

// restoreSessionEnvironment assigns the envelope's environment section into
// this process's environment, so the tools pi starts inherit the session's
// bindings even though the exec environment is allowlisted. An in-process
// assignment never reaches the exec-time environment block a process
// listing renders. A name already set is never overridden (the exec
// environment, and anything pi set itself, wins), a malformed name is
// skipped, and the extension's and pi's own control namespaces are never
// written from the file.
function restoreSessionEnvironment(envelope: any): void {
  const entries = envelope?.environment;
  if (!Array.isArray(entries)) return;
  for (const entry of entries) {
    const name = entry?.env;
    const value = entry?.value;
    if (typeof name !== "string" || typeof value !== "string") continue;
    if (name === "" || name.includes("=") || name.includes("\u0000")) continue;
    if (name.startsWith("DONMAI_PI_") || name.startsWith("PI_")) continue;
    if (process.env[name] !== undefined) continue;
    process.env[name] = value;
  }
}
// DONMAI_STATE_DIR_ENV carries the relocated per-session state root
// onto the interactive child. The Go engine guards the same root;
// the local interactive guard must cover it too, or deleting live
// session state from an interactive seat stays allowed.
const DONMAI_STATE_DIR_ENV = "DONMAI_PI_STATE_DIR";

// The interactive lane cannot round-trip to policy.go, but it MUST keep this
// one non-negotiable local safety rail. The state directory is created by the
// harness and pi keeps appending its own session JSONL beneath it; deleting it
// strands an otherwise-live session. Keep the refusal bytes in lock-step with
// statedir_guard.go: the model sees the same reason on either launch mode.
const PI_STATE_DIR = ".pi";
const STATE_DIR_GUARD_REASON_PREFIX = "refusing to delete the pi harness state directory";
const STATE_DIR_GUARD_EXPLANATION =
  " — " +
  PI_STATE_DIR +
  " holds this session's own storage (session transcript, the loaded policy extension, and the per-session agent home). " +
  "The harness created it before the session started and reads it for the session's whole life; " +
  "removing it does not fail loudly, it silently strands the run. " +
  "It is harness state, not project output — leave it in place.";

function shellTokens(segment: string): string[] {
  const out: string[] = [];
  let current = "";
  let quote = "";
  let escaped = false;
  let started = false;
  const flush = () => {
    if (started) {
      out.push(current);
      current = "";
      started = false;
    }
  };
  for (const ch of segment) {
    if (escaped) {
      current += ch;
      escaped = false;
      started = true;
    } else if (ch === "\\" && quote !== "'") {
      escaped = true;
      started = true;
    } else if (quote) {
      if (ch === quote) quote = "";
      else current += ch;
    } else if (ch === "'" || ch === '"') {
      quote = ch;
      started = true;
    } else if (/\s/.test(ch)) {
      flush();
    } else {
      current += ch;
      started = true;
    }
  }
  flush();
  return out;
}

function stateDirRoot(cwd: string): string {
  return cwd ? join(normalize(cwd), PI_STATE_DIR) : PI_STATE_DIR;
}

// stateDirRoots returns every guarded root: the legacy directory plus
// the relocated per-session root when the child env carries it. Anything
// unparsable is ignored so a stray value cannot widen the guard.
function stateDirRoots(cwd: string): string[] {
  const roots = [stateDirRoot(cwd)];
  const extra = (process.env[DONMAI_STATE_DIR_ENV] ?? "").trim();
  if (extra) {
    const resolved = isAbsolute(extra)
      ? normalize(extra)
      : cwd
        ? normalize(join(normalize(cwd), extra))
        : normalize(extra);
    if (resolved && !roots.includes(resolved)) roots.push(resolved);
  }
  return roots;
}

function firstStateDirPathInRoots(args: string[], cwd: string, roots: string[]): string | undefined {
  return pathOperands(args).find((arg) => roots.some((root) => resolvesIntoStateDir(arg, cwd, root)));
}

function stateDirReasonInRoots(args: string[], cwd: string, roots: string[]): string | undefined {
  for (const root of roots) {
    const reason = gitCleanStateDirReason(args, cwd, root);
    if (reason) return reason;
  }
  return undefined;
}

function resolvesIntoStateDir(operand: string, cwd: string, root: string): boolean {
  const trimmed = operand.trim();
  if (!trimmed || trimmed === "~" || trimmed.startsWith("~/")) return false;
  const resolved = isAbsolute(trimmed)
    ? normalize(trimmed)
    : cwd
      ? normalize(join(normalize(cwd), trimmed))
      : normalize(trimmed);
  return resolved === root || resolved.startsWith(root + sep);
}

function pathOperands(args: string[]): string[] {
  const out: string[] = [];
  let skipNext = false;
  for (const arg of args) {
    if (skipNext) {
      skipNext = false;
    } else if (!arg || arg === "--") {
      continue;
    } else if (/^[0-9&]*(>>?|<)$/.test(arg)) {
      skipNext = true;
    } else if (/^[0-9&]*(>>?|<)/.test(arg) || arg.startsWith("-")) {
      continue;
    } else {
      out.push(arg);
    }
  }
  return out;
}

function firstStateDirPath(args: string[], cwd: string, root: string): string | undefined {
  return pathOperands(args).find((arg) => resolvesIntoStateDir(arg, cwd, root));
}

function stateDirRefusal(command: string, path: string): string {
  return STATE_DIR_GUARD_REASON_PREFIX + " via `" + command + "` (" + path + ")" + STATE_DIR_GUARD_EXPLANATION;
}

function findDeletes(args: string[]): boolean {
  return args.some((arg, index) =>
    arg === "-delete" ||
    ((arg === "-exec" || arg === "-execdir" || arg === "-ok" || arg === "-okdir") &&
      args.slice(index + 1).some((rest) => ["rm", "rmdir", "unlink", "shred"].includes(basename(rest)))),
  );
}

function findSearchRoots(args: string[]): string[] {
  const roots: string[] = [];
  for (const arg of args) {
    if (arg.startsWith("-")) break;
    roots.push(arg);
  }
  return roots;
}

function gitCleanStateDirReason(args: string[], cwd: string, root: string): string | undefined {
  let commandIndex = -1;
  for (let index = 0; index < args.length; index++) {
    const arg = args[index];
    if (["-C", "-c", "--git-dir", "--work-tree", "--namespace"].includes(arg)) index++;
    else if (!arg.startsWith("-")) {
      if (arg !== "clean") return undefined;
      commandIndex = index;
      break;
    }
  }
  if (commandIndex < 0) return undefined;
  const rest = args.slice(commandIndex + 1);
  let forced = false;
  for (const arg of rest) {
    if (arg === "--dry-run" || (arg.startsWith("-") && !arg.startsWith("--") && arg.includes("n"))) return undefined;
    if (arg === "--force" || (arg.startsWith("-") && !arg.startsWith("--") && arg.includes("f"))) forced = true;
  }
  if (!forced) return undefined;
  const paths = pathOperands(rest);
  if (paths.length === 0) {
    return STATE_DIR_GUARD_REASON_PREFIX + " via an unrestricted `git clean` (it sweeps the whole worktree, " + PI_STATE_DIR + " included)" + STATE_DIR_GUARD_EXPLANATION;
  }
  const hit = firstStateDirPath(paths, cwd, root);
  return hit ? stateDirRefusal("git clean", hit) : undefined;
}

// interactiveStateDirDeletionReason is the local !rpcMode counterpart to
// stateDirDeletionReasonForRoots in statedir_guard.go. It deliberately
// covers only the session state rail; the richer policy/containment engine
// remains RPC. The relocated root travels on DONMAI_PI_STATE_DIR.
export function interactiveStateDirDeletionReason(command: string, cwd: string): string | undefined {
  if (!command.trim()) return undefined;
  const roots = stateDirRoots(cwd);
  for (const segment of command.split(/&&|\|\||;|\||&|\n/)) {
    let tokens = shellTokens(segment);
    while (tokens.length > 0 && /^[A-Za-z_][A-Za-z0-9_]*=/.test(tokens[0])) tokens = tokens.slice(1);
    if (tokens.length === 0) continue;
    const commandName = basename(tokens[0]);
    const args = tokens.slice(1);
    if (["rm", "rmdir", "unlink", "shred"].includes(commandName)) {
      const hit = firstStateDirPathInRoots(args, cwd, roots);
      if (hit) return stateDirRefusal(commandName, hit);
    } else if (commandName === "mv") {
      const paths = pathOperands(args);
      const hit = firstStateDirPathInRoots(paths.slice(0, -1), cwd, roots);
      if (hit) return stateDirRefusal(commandName, hit);
    } else if (commandName === "find" && findDeletes(args)) {
      const hit = firstStateDirPathInRoots(findSearchRoots(args), cwd, roots);
      if (hit) return stateDirRefusal(commandName, hit);
    } else if (commandName === "git") {
      const reason = stateDirReasonInRoots(args, cwd, roots);
      if (reason) return reason;
    }
  }
  return undefined;
}

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

// --- Sequential shell and file-write tools (ordering, not a trust layer) ---
//
// pi executes the tool calls of one assistant message concurrently by
// default: it runs every call's tool_call hook first, in order, and only then
// starts all the executions together. A batch of dependent shell calls (git
// add, git status, git commit) therefore races. The runtime's own switch is a
// per-tool executionMode: when ANY call in a batch names a tool registered
// with executionMode "sequential", the whole batch runs one call at a time in
// the order the model wrote it, each call's tool_call hook running right
// before its own execution (verified against the pinned agent-core's
// executeToolCalls). pi offers no other knob for this, and the tool_call hook
// cannot provide it: in a parallel batch every hook has returned before the
// first execution starts, so a queue held inside the hook would deadlock.
//
// So bash, write and edit are re-registered under their own names, with
// executionMode "sequential" and pi's own implementation. The override sits
// UNDER the boundary, never around it:
//
//   - pi fires tool_call for an overriding tool exactly as for the built-in,
//     keyed on the unchanged tool name, so adjudication, the bounds rail
//     (timeout clamp, pipefail prelude) and the state-dir guard all still
//     run, and their in-place input mutations are the params execute gets.
//   - execute delegates to pi's own factory for that tool, built for the
//     session's working directory (ctx.cwd), so the tool behaves as the
//     built-in does apart from ordering. Residual: the built-in bash also
//     receives the shellPath / shellCommandPrefix settings, which an
//     extension cannot read; the override runs pi's default shell resolution.
//
// The factories come from the host pi package through a dynamic import that
// runs only after every policy handler is registered. A static import would
// make the whole boundary fail to load wherever that module does not resolve
// (the scripted fixtures load this file from a data: URL; a later pi could
// rename an export), and in the interactive lane a failed load runs the
// session with no local rail at all. Here a failed import or an unusable
// factory changes nothing: the built-ins stay registered, unordered, and the
// boundary is untouched. Ordering is best-effort; the boundary is not.
const SEQUENTIAL_TOOL_FACTORIES = [
  ["bash", "createBashToolDefinition"],
  ["write", "createWriteToolDefinition"],
  ["edit", "createEditToolDefinition"],
];

// sequentialToolOverride builds the sequential override for one tool from
// pi's own factory, or returns undefined when the factory is unusable (not a
// function, or it built a definition for some other tool). Exported for the
// scripted conformance fixture (testdata/tool-call-bounds-harness.mjs).
export function sequentialToolOverride(factory: any, toolName: string, fallbackCwd: string): any {
  if (typeof factory !== "function") return undefined;
  const template = factory(fallbackCwd);
  if (!template || template.name !== toolName || typeof template.execute !== "function") return undefined;
  return {
    ...template,
    executionMode: "sequential",
    execute(toolCallId: string, params: any, signal: any, onUpdate: any, ctx: any) {
      const cwd = typeof ctx?.cwd === "string" && ctx.cwd !== "" ? ctx.cwd : fallbackCwd;
      return factory(cwd).execute(toolCallId, params, signal, onUpdate, ctx);
    },
  };
}

// registerSequentialTools registers the overrides. Called LAST in both lanes,
// after every tool_call handler is in place; it never throws.
async function registerSequentialTools(pi: ExtensionAPI): Promise<void> {
  try {
    const host: any = await import("@earendil-works/pi-coding-agent");
    const fallbackCwd = process.cwd();
    for (const [toolName, factoryName] of SEQUENTIAL_TOOL_FACTORIES) {
      const override = sequentialToolOverride(host?.[factoryName], toolName, fallbackCwd);
      if (override) pi.registerTool(override as never);
    }
  } catch {
    // The host package did not resolve or a factory threw: the built-ins
    // stay as they are and the boundary above is unaffected.
  }
}

export default async function activate(pi: ExtensionAPI) {
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
  //
  // Restore the session's deferred environment bindings before anything
  // else runs: every extension loaded after this one, and every tool pi
  // starts, then sees them. See the "Session environment" note above.
  const sessionCredentials = readSessionCredentialEnvelope();
  restoreSessionEnvironment(sessionCredentials);

  const token = process.env.DONMAI_PI_HANDSHAKE ?? "";
  const rpcMode = token !== "";

  // Provider pin: register the single "donmai" provider from env so the session
  // can only route to the resolved cell. The key is read from the session
  // credential file (DONMAI_PI_CREDENTIALS_FILE) at runtime, never from env
  // and never inlined.
  const baseUrl = process.env.DONMAI_PI_BASE_URL ?? "";
  const api = process.env.DONMAI_PI_API ?? "openai-completions";
  const model = process.env.DONMAI_PI_MODEL ?? "";
  const apiKey = readSessionCredential(sessionCredentials);
  // Context-window pin: the harness exports the resolved profile's
  // context-window size (tokens) as DONMAI_PI_CONTEXT_WINDOW when the
  // dispatch carried one (extension.go providerPinEnv). A missing or invalid
  // value falls back to the historical 200000 default, so an unpinned
  // session keeps prior behaviour while a pinned 1M-context model is no
  // longer silently clamped to it.
  const contextWindowEnv = Number(process.env.DONMAI_PI_CONTEXT_WINDOW ?? "");
  const contextWindow =
    Number.isInteger(contextWindowEnv) && contextWindowEnv > 0 ? contextWindowEnv : 200000;
  // Output-token pin: the harness exports the resolved profile's per-response
  // output limit as DONMAI_PI_MAX_TOKENS only when the dispatch carried one
  // (extension.go providerPinEnv). There is deliberately NO fallback value:
  // an output limit is configuration, not something this extension invents.
  // Unset (or invalid), the model is registered without maxTokens, so pi
  // requests no output cap and the serving endpoint's own limit applies. A
  // fixed cap here once cut long reasoning turns off mid tool call.
  const maxTokensEnv = Number(process.env.DONMAI_PI_MAX_TOKENS ?? "");
  const maxTokens = Number.isInteger(maxTokensEnv) && maxTokensEnv > 0 ? maxTokensEnv : undefined;
  // Thinking-level pin: the harness exports the session's configured
  // reasoning effort as DONMAI_PI_THINKING_LEVEL (extension.go
  // providerPinEnv) — the same level it pins with `--model <id>:<level>`.
  // pi treats the xhigh and max levels as opt-in per model: without a
  // thinkingLevelMap entry it clamps a request for either down to high. The
  // configured level is the one this session runs at, so it is registered as
  // supported and mapped to the provider's own value of the same name, which
  // pi sends as the protocol's native effort parameter (reasoning_effort on
  // Chat Completions, reasoning.effort on Responses). On the Anthropic
  // Messages protocol those two tiers exist only as the adaptive-thinking
  // effort parameter — a thinking token budget cannot express them — so the
  // model is marked adaptive for them. Lower levels need no entry: pi passes
  // them through under their own names.
  //
  // Unset means no effort is configured, and none may be invented: pi would
  // otherwise request its own default level (medium) on the session's behalf.
  // On Chat Completions the request can omit the parameter entirely, so the
  // model is registered without reasoning-effort support and the serving
  // endpoint's own default applies. The other protocols give pi no way to
  // omit it; there the session record states that no effort was configured.
  const thinkingLevel = process.env.DONMAI_PI_THINKING_LEVEL ?? "";
  const extendedThinking = thinkingLevel === "xhigh" || thinkingLevel === "max";
  const thinkingLevelMap = extendedThinking ? { [thinkingLevel]: thinkingLevel } : undefined;
  const compat =
    extendedThinking && api === "anthropic-messages"
      ? { forceAdaptiveThinking: true }
      : thinkingLevel === "" && api === "openai-completions"
        ? { supportsReasoningEffort: false }
        : undefined;
  // Per-token price pin: the runner exports the endpoint binding's
  // optional per-token prices (USD per million tokens) on
  // DONMAI_PI_PRICE_{INPUT,OUTPUT,CACHE_READ,CACHE_WRITE} plus the
  // DONMAI_PI_PRICES_BOUND presence flag (extension.go unitPricePinEnv).
  // Bound prices register as the model's cost table so pi computes the
  // real per-turn cost. Unbound, a zero cost table is registered (pi
  // requires the cost field, and omitting it crashes its cost
  // computation) while the Go mapper suppresses the reported cost, so
  // the session reads as cost-absent, not $0. A non-nil but all-zero
  // binding is an explicit zero price, not an absent one.
  const pricesBound = process.env.DONMAI_PI_PRICES_BOUND === "1";
  const priceEnv = (name) => {
    const raw = process.env[name] ?? "";
    const value = Number(raw);
    return Number.isFinite(value) && value >= 0 ? value : 0;
  };
  const modelCost = pricesBound
    ? {
        input: priceEnv("DONMAI_PI_PRICE_INPUT"),
        output: priceEnv("DONMAI_PI_PRICE_OUTPUT"),
        cacheRead: priceEnv("DONMAI_PI_PRICE_CACHE_READ"),
        cacheWrite: priceEnv("DONMAI_PI_PRICE_CACHE_WRITE"),
      }
    : { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
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
            cost: modelCost,
            contextWindow,
            ...(maxTokens === undefined ? {} : { maxTokens }),
            ...(thinkingLevelMap === undefined ? {} : { thinkingLevelMap }),
            ...(compat === undefined ? {} : { compat }),
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
    pi.on("tool_call", (event: any, ctx: any) => {
      const tool = String(event?.toolName ?? "").toLowerCase();
      if (!GUARDED_TOOLS.has(tool)) return;
      if (tool === "bash") {
        const reason = interactiveStateDirDeletionReason(String(event?.input?.command ?? ""), String(ctx?.cwd ?? ""));
        if (reason) return { block: true, reason };
      }
      if (allowed.length > 0 || disallowed.length > 0) {
        return evaluateLocalToolPolicy(allowed, disallowed, tool, event?.input ?? {});
      }
    });
    // Provider registration already ran; the sequential overrides are the
    // last step. No handshake, no Go-side adjudication round trip.
    await registerSequentialTools(pi);
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
  //
  // Every refusal below is REGISTERED before it is returned. pi emits a
  // tool_execution_end for a blocked call exactly as it does for an executed
  // one (verified against the pinned binary: a blocked call is finalized with
  // isError:true and the reason as its result text), so a refusal this
  // extension reached alone used to arrive at the Go monitor as a call with no
  // record — indistinguishable from a bypass. Registering it first closes
  // that gap on the three paths that never reach the adjudication round-trip.
  pi.on("tool_call", async (event: any, ctx: any) => {
    const tool = String(event?.toolName ?? "");
    if (!GUARDED_TOOLS.has(tool)) return;
    const callId = toolCallIdOf(event);

    // Fail closed: an unverified boundary blocks every guarded tool. If the
    // handshake has not even settled yet, block too — the Go side gates the
    // prompt on the handshake, so this only guards against races.
    if (!verified) {
      const reason = handshakeSettled
        ? "donmai policy boundary not verified"
        : "donmai policy boundary still initializing";
      await registerRefusal(ctx, token, tool, callId, reason);
      return { block: true, reason };
    }
    if (!ctx?.hasUI) {
      // No UI channel means no round-trip at all, so this refusal cannot be
      // registered; registerRefusal returns immediately and the Go monitor
      // records the call as unproven instead.
      const reason = "donmai policy boundary requires a UI channel";
      await registerRefusal(ctx, token, tool, callId, reason);
      return { block: true, reason };
    }

    try {
      const payload = JSON.stringify({
        donmai: KIND_ADJUDICATE,
        token,
        toolName: tool,
        toolCallId: callId,
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
      // allow: bound the shell call before it executes, then let it run.
      // The mutation must land before the Go adjudication record is written:
      // a deny clamps nothing, an allow runs exactly this bounded input.
      // NOTE (residual): mutating input here means the Go-side
      // permission_decision audit event for this call still carries the
      // pre-mutation command text, while pi executes the bounded form —
      // the timeout clamp and the pipefail prelude are deterministic and
      // visible on the wire (timeout field, command prefix), not in the
      // audit message. A future change can re-emit the bounded text.
      if (tool === "bash" && event?.input && typeof event.input === "object") {
        event.input.timeout = resolveBashTimeoutSeconds((event.input as any).timeout);
        event.input.command = withPipefailPrelude((event.input as any).command);
      }
      // allow: returning undefined lets the tool execute.
      return;
    } catch (err) {
      const reason = "donmai policy adjudication failed: " + String(err);
      await registerRefusal(ctx, token, tool, callId, reason);
      return { block: true, reason };
    }
  });

  await registerSequentialTools(pi);
}
