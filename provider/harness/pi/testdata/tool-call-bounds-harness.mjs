// tool-call-bounds-harness.mjs — scripted (no pi binary) conformance
// fixture for the bounded shell-tool execution rail: the policy extension's
// exported resolveBashTimeoutSeconds / withPipefailPrelude helpers, run
// against the REAL production extensions/donmai-policy.ts.
//
// This harness imports the real production extension directly, never a copy,
// strips its narrow TypeScript surface to plain JavaScript with the same
// stripErasableTypeScript routine the interactive-local-tool-policy fixture
// uses (duplicated here so this fixture stays self-contained; the two must
// stay in lock-step — if the extension gains a TypeScript shape this copy
// does not handle, the stripped source fails node's own --check loudly,
// never a silent skip), then calls the two exported helpers and prints one
// JSON line on stdout so the Go test can assert on real extension behavior
// without spawning pi.
//
// It also proves the sequential shell and file-write overrides
// (sequentialToolOverride / registerSequentialTools) without a pi process:
// the extension is activated in both lanes against a stub ExtensionAPI, once
// with its host-package import pointed at a stub module of tool factories
// (the import specifier is rewritten in this harness's copy of the source,
// and the rewrite must hit exactly once — a renamed or removed import fails
// loudly here, never silently), and once unrewritten, where the import
// cannot resolve from a data: URL, standing in for a host that lacks the
// factories.
//
// Usage: node tool-call-bounds-harness.mjs <extensionPath>
// Env: none.

import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

// --- stripErasableTypeScript: duplicated from
// interactive-local-tool-policy-harness.mjs (see its header for scope and
// the loud-failure contract). Kept as a copy so each fixture reads
// standalone; extend both together when the extension gains new syntax. ---

function stripErasableTypeScript(src) {
  let s = src;
  s = s.replace(/^import type .*;\r?\n/gm, "");
  s = s.replace(/^interface\s+\w+\s*\{[\s\S]*?\n\}\r?\n/gm, "");
  s = stripAsAssertions(s);
  s = stripParamAndReturnTypes(s);
  s = stripVarAnnotations(s);
  return s;
}

function scanRegions(src) {
  const codeMask = new Uint8Array(src.length);
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    const c2 = src[i + 1];
    if (c === "/" && c2 === "/") {
      const end = src.indexOf("\n", i);
      i = end === -1 ? n : end;
      continue;
    }
    if (c === "/" && c2 === "*") {
      const end = src.indexOf("*/", i + 2);
      i = end === -1 ? n : end + 2;
      continue;
    }
    if (c === '"' || c === "'") {
      const quote = c;
      let j = i + 1;
      while (j < n && src[j] !== quote) {
        if (src[j] === "\\") j++;
        j++;
      }
      i = j + 1;
      continue;
    }
    if (c === "`") {
      let j = i + 1;
      while (j < n) {
        if (src[j] === "\\") {
          j += 2;
          continue;
        }
        if (src[j] === "`") {
          j++;
          break;
        }
        if (src[j] === "$" && src[j + 1] === "{") {
          let depth = 1;
          let k = j + 2;
          for (; k < n && depth > 0; k++) {
            if (src[k] === "{") depth++;
            else if (src[k] === "}") depth--;
          }
          for (let m = j + 2; m < k - 1; m++) codeMask[m] = 1;
          j = k;
          continue;
        }
        j++;
      }
      for (let m = i; m < j && m < n; m++) {
        if (codeMask[m] !== 1) codeMask[m] = 0;
      }
      i = j;
      continue;
    }
    codeMask[i] = 1;
    i++;
  }
  return codeMask;
}

function stripAsAssertions(s) {
  const codeMask = scanRegions(s);
  const n = s.length;
  let out = "";
  let i = 0;
  while (i < n) {
    if (codeMask[i] && s.startsWith("as", i) && isWordBoundary(s, i, i + 2)) {
      let j = i + 2;
      while (j < n && codeMask[j] && /\s/.test(s[j])) j++;
      if (s[j] === "{" && codeMask[j]) {
        let depth = 0;
        let k = j;
        for (; k < n; k++) {
          if (!codeMask[k]) continue;
          if (s[k] === "{") depth++;
          else if (s[k] === "}") {
            depth--;
            if (depth === 0) {
              k++;
              break;
            }
          }
        }
        i = k;
        continue;
      }
      const rest = /^[\w.]+(\[\])?/.exec(s.slice(j));
      if (rest && codeMask[j]) {
        i = j + rest[0].length;
        continue;
      }
    }
    out += s[i];
    i++;
  }
  return out;
}

function isWordBoundary(s, start, end) {
  const before = start > 0 ? s[start - 1] : " ";
  const after = end < s.length ? s[end] : " ";
  return !/\w/.test(before) && !/\w/.test(after);
}

function stripParamAndReturnTypes(s) {
  const codeMask = scanRegions(s);
  const n = s.length;
  let out = "";
  let i = 0;
  const stack = [];
  while (i < n) {
    if (!codeMask[i]) {
      out += s[i];
      i++;
      continue;
    }
    const c = s[i];
    if (c === "(" || c === "{" || c === "[") {
      stack.push(c);
      out += c;
      i++;
      continue;
    }
    if (c === ")" || c === "}" || c === "]") {
      stack.pop();
      out += c;
      i++;
      if (c === ")" && stack.length === 0) {
        let j = i;
        while (j < n && codeMask[j] && /\s/.test(s[j])) j++;
        if (codeMask[j] && s[j] === ":") {
          j++;
          while (j < n && codeMask[j] && /\s/.test(s[j])) j++;
          if (s[j] === "{" && codeMask[j]) {
            let depth = 0;
            let k = j;
            for (; k < n; k++) {
              if (!codeMask[k]) continue;
              if (s[k] === "{") depth++;
              else if (s[k] === "}") {
                depth--;
                if (depth === 0) {
                  k++;
                  break;
                }
              }
            }
            j = k;
          }
          const localStack = [];
          let k = j;
          for (; k < n; k++) {
            if (!codeMask[k]) continue;
            const ch = s[k];
            if (localStack.length === 0 && ch === "{") break;
            if (localStack.length === 0 && ch === "=" && s[k + 1] === ">") break;
            if (ch === "(" || ch === "[" || ch === "{" || ch === "<") localStack.push(ch);
            else if (ch === ")" || ch === "]" || ch === "}") localStack.pop();
            else if (ch === ">" && localStack[localStack.length - 1] === "<") localStack.pop();
          }
          i = k;
        }
      }
      continue;
    }
    if (stack[stack.length - 1] === "(" && /[A-Za-z_$]/.test(c)) {
      const identMatch = /^[\w$]+/.exec(s.slice(i));
      const identEnd = i + identMatch[0].length;
      let j = identEnd;
      while (j < n && codeMask[j] && /\s/.test(s[j])) j++;
      if (codeMask[j] && s[j] === ":") {
        out += s.slice(i, identEnd);
        j++;
        const innerStack = [];
        let k = j;
        for (; k < n; k++) {
          if (!codeMask[k]) continue;
          const ch = s[k];
          if (ch === "(" || ch === "[" || ch === "{" || ch === "<") innerStack.push(ch);
          else if (ch === ")" || ch === "]" || ch === "}") {
            if (innerStack.length === 0) break;
            innerStack.pop();
          } else if (ch === ">" && innerStack[innerStack.length - 1] === "<") {
            innerStack.pop();
          } else if (ch === "," && innerStack.length === 0) break;
        }
        i = k;
        continue;
      }
      out += s.slice(i, identEnd);
      i = identEnd;
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

function stripVarAnnotations(s) {
  const codeMask = scanRegions(s);
  return s.replace(/\b(let|const|var)\s+(\w+)\s*:\s*[^=;\n]+(?=[=;])/g, (m, kw, name, offset) => {
    for (let k = offset; k < offset + m.length; k++) {
      if (!codeMask[k]) return m;
    }
    return `${kw} ${name}`;
  });
}

// --- Harness proper ---

const [, , extensionPath] = process.argv;

if (!extensionPath) {
  console.error("usage: node tool-call-bounds-harness.mjs <extensionPath>");
  process.exit(2);
}

const source = readFileSync(extensionPath, "utf8");
const stripped = stripErasableTypeScript(source);

function checkStrippedSyntax(code) {
  try {
    execFileSync(process.execPath, ["--input-type=module", "--check"], { input: code, stdio: ["pipe", "pipe", "pipe"] });
  } catch (err) {
    const detail = err.stderr ? err.stderr.toString() : String(err);
    throw new Error(
      "stripErasableTypeScript produced invalid JavaScript from " +
        extensionPath +
        " — the extension gained a TypeScript shape this harness's stripper does not handle yet; update stripErasableTypeScript " +
        "(tool-call-bounds-harness.mjs, and its twin in interactive-local-tool-policy-harness.mjs) to cover it. node --check said:\n" +
        detail,
    );
  }
}
checkStrippedSyntax(stripped);

const dataURL = "data:text/javascript;base64," + Buffer.from(stripped, "utf8").toString("base64");
const mod = await import(dataURL);

if (
  typeof mod.resolveBashTimeoutSeconds !== "function" ||
  typeof mod.withPipefailPrelude !== "function" ||
  typeof mod.sequentialToolOverride !== "function"
) {
  console.log(JSON.stringify({ ok: false, reason: "helpers not exported" }));
  process.exit(0);
}

const { resolveBashTimeoutSeconds, withPipefailPrelude } = mod;
const timeoutCases = {
  missing: resolveBashTimeoutSeconds(undefined),
  zero: resolveBashTimeoutSeconds(0),
  negative: resolveBashTimeoutSeconds(-5),
  nan: resolveBashTimeoutSeconds(NaN),
  string: resolveBashTimeoutSeconds("60"),
  small: resolveBashTimeoutSeconds(60),
  atBound: resolveBashTimeoutSeconds(300),
  aboveBound: resolveBashTimeoutSeconds(900),
};
const preludeCases = {
  plain: withPipefailPrelude("false | tail -1; echo $?"),
  alreadyPrefixed: withPipefailPrelude("set -o pipefail 2>/dev/null; echo hi"),
  empty: withPipefailPrelude(""),
};
// --- Sequential overrides ---

const HOST_IMPORT = 'import("@earendil-works/pi-coding-agent")';
const stubHostURL =
  "data:text/javascript;base64," +
  Buffer.from(
    `export const calls = [];
const make = (name) => (cwd) => ({
  name,
  label: name,
  description: "stub " + name,
  promptSnippet: "snippet " + name,
  parameters: { type: "object" },
  async execute(toolCallId, params, signal, onUpdate, ctx) {
    calls.push({ name, cwd, toolCallId, params });
    return { content: [{ type: "text", text: name + " ran" }], details: { cwd } };
  },
});
export const createBashToolDefinition = make("bash");
export const createWriteToolDefinition = make("write");
export const createEditToolDefinition = make("edit");
export const createReadToolDefinition = make("read");
`,
    "utf8",
  ).toString("base64");

const hostImports = stripped.split(HOST_IMPORT).length - 1;
if (hostImports !== 1) {
  console.log(JSON.stringify({ ok: false, reason: "expected exactly one " + HOST_IMPORT + " in the extension, found " + hostImports }));
  process.exit(0);
}
const wired = stripped.replace(HOST_IMPORT, "import(" + JSON.stringify(stubHostURL) + ")");
const wiredMod = await import("data:text/javascript;base64," + Buffer.from(wired, "utf8").toString("base64"));
const stubHost = await import(stubHostURL);

// activateLane runs the extension's default export against a stub
// ExtensionAPI and records, in order, every handler and tool it registers.
async function activateLane(module, handshakeToken) {
  if (handshakeToken) process.env.DONMAI_PI_HANDSHAKE = handshakeToken;
  else delete process.env.DONMAI_PI_HANDSHAKE;
  const sequence = [];
  const tools = [];
  const handlers = {};
  const stubPi = {
    registerProvider() {},
    registerTool(definition) {
      sequence.push("tool:" + definition.name);
      tools.push(definition);
    },
    on(event, handler) {
      sequence.push("on:" + event);
      handlers[event] = handler;
    },
  };
  await module.default(stubPi);
  delete process.env.DONMAI_PI_HANDSHAKE;
  return { sequence, tools, handlers };
}

function describeLane(lane) {
  return {
    sequence: lane.sequence,
    tools: lane.tools.map((tool) => ({
      name: tool.name,
      executionMode: tool.executionMode ?? null,
      promptSnippet: tool.promptSnippet ?? null,
    })),
  };
}

const interactiveLane = await activateLane(wiredMod, "");
const rpcLane = await activateLane(wiredMod, "fixture-handshake-token");
const unresolvedLane = await activateLane(mod, "fixture-handshake-token");

// Delegation: the override runs pi's own factory, built for the session's
// cwd, with the very params object the tool_call hook mutated.
const bash = rpcLane.tools.find((tool) => tool.name === "bash");
const params = { command: "set -o pipefail 2>/dev/null; true", timeout: 300 };
let delegation = null;
if (bash) {
  const result = await bash.execute("fixture-call", params, undefined, undefined, { cwd: "/fixture/session-cwd" });
  const call = stubHost.calls[stubHost.calls.length - 1] ?? {};
  delegation = {
    cwd: call.cwd ?? null,
    toolCallId: call.toolCallId ?? null,
    sameParams: call.params === params,
    resultCwd: result?.details?.cwd ?? null,
  };
}

const goodFactory = (cwd) => ({ name: "bash", execute: async () => ({ content: [], details: { cwd } }) });
const rejects = {
  notAFunction: mod.sequentialToolOverride(undefined, "bash", "/fixture") === undefined,
  wrongTool: mod.sequentialToolOverride(goodFactory, "write", "/fixture") === undefined,
  noExecute: mod.sequentialToolOverride(() => ({ name: "bash" }), "bash", "/fixture") === undefined,
  accepted: mod.sequentialToolOverride(goodFactory, "bash", "/fixture")?.executionMode ?? null,
};

const sequential = {
  lanes: { interactive: describeLane(interactiveLane), rpc: describeLane(rpcLane) },
  unresolvedHost: describeLane(unresolvedLane),
  delegation,
  rejects,
  fenceProbe: await probeFenceWhileUnverified(rpcLane),
};

// probeFenceWhileUnverified drives the REAL rpc-lane tool_call handler for
// every overridden tool while the boundary is still unverified (no handshake
// round-trip has run, so verified is false). A fail-closed fence blocks each
// one — proving the calls the sequential overrides re-enable under the
// unchanged names still reach adjudication. Returns one entry per tool:
// { block: boolean, reason: string|null }.
async function probeFenceWhileUnverified(lane) {
  const out = {};
  const handler = lane.handlers["tool_call"];
  for (const tool of ["bash", "write", "edit"]) {
    if (typeof handler !== "function") {
      out[tool] = { block: false, reason: "no tool_call handler registered" };
      continue;
    }
    try {
      const verdict = await handler(
        { toolName: tool, toolCallId: "probe-" + tool, input: {} },
        { cwd: "/fixture" },
      );
      out[tool] = {
        block: verdict?.block === true,
        reason: typeof verdict?.reason === "string" ? verdict.reason : null,
      };
    } catch (err) {
      out[tool] = { block: false, reason: "threw: " + String(err) };
    }
  }
  return out;
};

// --- Output limit fitted to the context window ---
//
// An OpenAI-compatible gateway rejected a request because its input plus the
// requested output exceeded the model's context window, and rejected the
// identical retry again. pi sized the output limit from a characters/4 input
// estimate and a fixed 4096-token margin; with the model's maxTokens equal to
// its window, that left more requested output than the window had room for.
// The incident's numbers, reproduced here: a 1048576-token window and
// ceiling, content pi estimated at 81680 tokens (326720 characters), which a
// real tokenizer counted as 87109, and the 962800-token output pi requested
// (1048576 - 81680 - 4096). The Go test asserts on the fitted limits.

const FIT_WINDOW = 1048576;
const FIT_CONTENT_CHARS = 81680 * 4;
const PI_REQUESTED_OUTPUT = FIT_WINDOW - 81680 - 4096;

function proseOfLength(n) {
  const words = "the quick brown fox jumps over a lazy dog while reading source files ";
  return words.repeat(Math.ceil(n / words.length)).slice(0, n);
}

function incidentPayload(field, extra) {
  const system = proseOfLength(4000);
  return {
    model: "fixture-model",
    messages: [
      { role: "system", content: system },
      { role: "user", content: proseOfLength(FIT_CONTENT_CHARS - system.length) },
    ],
    tools: [{ type: "function", function: { name: "bash", description: "run a shell command", parameters: { type: "object" } } }],
    [field]: PI_REQUESTED_OUTPUT,
    ...(extra ?? {}),
  };
}

function fittedField(payload, field) {
  if (payload === undefined || payload === null) return null;
  const value = payload[field];
  return typeof value === "number" ? value : null;
}

async function fitReport() {
  if (typeof mod.fitOutputLimitToWindow !== "function") return null;
  const fit = mod.fitOutputLimitToWindow;
  const original = incidentPayload("max_tokens");
  const cases = {
    maxTokens: fittedField(fit(original, FIT_WINDOW, FIT_WINDOW), "max_tokens"),
    maxCompletionTokens: fittedField(fit(incidentPayload("max_completion_tokens"), FIT_WINDOW, FIT_WINDOW), "max_completion_tokens"),
    maxOutputTokens: fittedField(fit(incidentPayload("max_output_tokens"), FIT_WINDOW, FIT_WINDOW), "max_output_tokens"),
    registeredCeiling: fittedField(fit(incidentPayload("max_tokens"), FIT_WINDOW, 200000), "max_tokens"),
    alreadyFits: fit({ ...incidentPayload("max_tokens"), max_tokens: 32000 }, FIT_WINDOW, FIT_WINDOW) ?? null,
    noRoom: fit(incidentPayload("max_tokens"), 100000, 100000) ?? null,
    noOutputField: fit({ model: "fixture-model", messages: [] }, FIT_WINDOW, FIT_WINDOW) ?? null,
    noWindow: fit(incidentPayload("max_tokens"), 0, FIT_WINDOW) ?? null,
  };
  const thinking = fit(
    incidentPayload("max_tokens", { thinking: { type: "enabled", budget_tokens: PI_REQUESTED_OUTPUT - 1024 } }),
    FIT_WINDOW,
    FIT_WINDOW,
  );
  cases.thinkingMax = fittedField(thinking, "max_tokens");
  cases.thinkingBudget = thinking?.thinking?.budget_tokens ?? null;
  cases.originalUntouched = original.max_tokens === PI_REQUESTED_OUTPUT;

  // Headroom the fit leaves beyond a bytes/3 bound over the WHOLE serialized
  // payload (computed here, independently of the extension's helper), for a
  // large input, a small one, and a tool-heavy one whose bytes sit mostly in
  // the tool definitions. pi sized each of these as window - chars/4 of the
  // messages - 4096. The Go test asserts the headroom is at least the
  // window-scaled margin, so the fit neither drops the margin nor counts only
  // the messages.
  const reserved = (payload, field) => {
    const fitted = fittedField(fit(payload, FIT_WINDOW, FIT_WINDOW), field);
    if (fitted === null) return null;
    const bound = Math.ceil(Buffer.byteLength(JSON.stringify(payload), "utf8") / 3);
    return FIT_WINDOW - bound - fitted;
  };
  const small = { model: "fixture-model", messages: [{ role: "user", content: proseOfLength(4000) }], max_completion_tokens: FIT_WINDOW - 1000 - 4096 };
  const toolHeavy = {
    model: "fixture-model",
    messages: [{ role: "user", content: proseOfLength(4000) }],
    tools: Array.from({ length: 300 }, (_, i) => ({
      type: "function",
      function: { name: `tool_${i}`, description: proseOfLength(2000), parameters: { type: "object", properties: { arg: { type: "string", description: proseOfLength(400) } } } },
    })),
    max_tokens: FIT_WINDOW - 1000 - 4096,
  };
  cases.reserved = {
    large: reserved(incidentPayload("max_tokens"), "max_tokens"),
    small: reserved(small, "max_completion_tokens"),
    toolHeavy: reserved(toolHeavy, "max_tokens"),
  };

  // A thinking budget that cannot stay at Anthropic's 1024-token minimum
  // below the fitted limit: the payload is left as built (the overflow path
  // handles it) rather than sent with an invalid budget. The window leaves
  // exactly 1500 output tokens: 20000 - 4096 margin - 14404 input bound. The
  // same payload without thinking fits to 1500, so the refusal is the
  // budget's.
  const TIGHT_WINDOW = 20000;
  const tight = (extra) => {
    const base = { model: "fixture-model", messages: [{ role: "user", content: "" }], max_tokens: 5000, ...extra };
    const pad = 14404 * 3 - Buffer.byteLength(JSON.stringify(base), "utf8");
    return { ...base, messages: [{ role: "user", content: proseOfLength(pad) }] };
  };
  cases.tightNoThinking = fittedField(fit(tight(), TIGHT_WINDOW, TIGHT_WINDOW), "max_tokens");
  cases.tightThinking = fit(tight({ thinking: { type: "enabled", budget_tokens: 4000 } }), TIGHT_WINDOW, TIGHT_WINDOW) ?? null;

  // The same fit through the extension's real activation: the registered
  // donmai provider gets a before_provider_request hook, and that hook leaves
  // every other provider's payload alone.
  const env = {
    DONMAI_PI_BASE_URL: "http://127.0.0.1:9/v1",
    DONMAI_PI_MODEL: "fixture-model",
    DONMAI_PI_API: "openai-completions",
    DONMAI_PI_CONTEXT_WINDOW: String(FIT_WINDOW),
    DONMAI_PI_MAX_TOKENS: String(FIT_WINDOW),
  };
  for (const [k, v] of Object.entries(env)) process.env[k] = v;
  let lane;
  try {
    lane = await activateLane(mod, "fixture-handshake-token");
  } finally {
    for (const k of Object.keys(env)) delete process.env[k];
  }
  const hook = lane.handlers["before_provider_request"];
  let hooked = null;
  let foreign = null;
  if (typeof hook === "function") {
    const ours = await hook({ type: "before_provider_request", payload: incidentPayload("max_tokens") }, { model: { provider: "donmai", id: "fixture-model" } });
    hooked = fittedField(ours, "max_tokens");
    const theirs = await hook({ type: "before_provider_request", payload: incidentPayload("max_tokens") }, { model: { provider: "other", id: "fixture-model" } });
    foreign = theirs === undefined ? "unchanged" : "changed";
  }
  return {
    window: FIT_WINDOW,
    incidentRealInputTokens: 87109,
    piRequestedOutput: PI_REQUESTED_OUTPUT,
    cases,
    hook: { registered: typeof hook === "function", hooked, foreign },
  };
}

// --- Context-overflow wording pi does not recognize ---
//
// The gateway's overflow wording ("exceeds the model's context window") is
// outside pi's own overflow patterns, so pi neither compacted nor stopped
// retrying. The message_end hook marks such an error so pi treats it as the
// overflow it is. Each case reports the extension's classification and the
// marked error text; the Go test pins both and checks the classification
// against providerretry.ContextOverflow.

const OVERFLOW_TEXTS = {
  gateway: "400 The input (87109 tokens) plus the requested output (962800 tokens) exceeds the model's context window of 1048576 tokens.",
  openAICompatible: "400 This model's maximum context length is 131072 tokens. However, you requested 140000 tokens (9000 in the messages, 131000 in the completion).",
  alreadyMarked: "400 context_length_exceeded: request too large",
  rateLimited: "429 Rate limit reached: too many tokens per minute",
  unrelated: "400 Invalid value for parameter temperature",
};

async function overflowReport() {
  if (typeof mod.isContextOverflowText !== "function" || typeof mod.markContextOverflow !== "function") return null;
  const cases = {};
  for (const [name, text] of Object.entries(OVERFLOW_TEXTS)) {
    const marked = mod.markContextOverflow({ role: "assistant", stopReason: "error", errorMessage: text, content: [] });
    cases[name] = { text, classified: mod.isContextOverflowText(text), marked: marked?.errorMessage ?? null };
  }
  const notAnError = mod.markContextOverflow({ role: "assistant", stopReason: "stop", errorMessage: OVERFLOW_TEXTS.gateway, content: [] }) ?? null;
  const notAssistant = mod.markContextOverflow({ role: "user", stopReason: "error", errorMessage: OVERFLOW_TEXTS.gateway, content: [] }) ?? null;
  const lane = await activateLane(mod, "fixture-handshake-token");
  const hook = lane.handlers["message_end"];
  let hooked = null;
  if (typeof hook === "function") {
    const result = await hook({ type: "message_end", message: { role: "assistant", stopReason: "error", errorMessage: OVERFLOW_TEXTS.gateway, content: [] } }, {});
    hooked = result?.message?.errorMessage ?? null;
  }
  return { cases, notAnError, notAssistant, hook: { registered: typeof hook === "function", hooked } };
}

const providerFit = await fitReport();
const contextOverflow = await overflowReport();

console.log(JSON.stringify({ ok: true, timeoutCases, preludeCases, sequential, providerFit, contextOverflow }));
