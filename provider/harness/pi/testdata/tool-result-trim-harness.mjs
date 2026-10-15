// tool-result-trim-harness.mjs — scripted (no pi binary) conformance
// fixture for the bounded error-tool-result rail: the policy extension's
// exported trimErrorResultText helper plus its real `tool_result` handler
// registration, run against the REAL production extensions/donmai-policy.ts.
//
// This harness imports the real production extension directly, never a copy,
// strips its narrow TypeScript surface to plain JavaScript with the same
// stripErasableTypeScript routine the tool-call-bounds fixture uses
// (duplicated here so this fixture stays self-contained; the two must stay
// in lock-step — if the extension gains a TypeScript shape this copy does
// not handle, the stripped source fails node's own --check loudly, never a
// silent skip), then:
//
//   - calls trimErrorResultText over boundary cases (short, at-bound,
//     over-bound, empty) and prints the outcomes;
//   - activates the extension in both lanes against a stub ExtensionAPI,
//     captures the registered `tool_result` handler, and drives it with
//     synthetic error/success results (small, large, multi-block, non-text)
//     to prove the wiring: only oversized error text is rewritten, and the
//     rewrite is the tail plus the marker.
//
// Prints one JSON line on stdout so the Go test can assert on real extension
// behavior without spawning pi.
//
// Usage: node tool-result-trim-harness.mjs <extensionPath>
// Env: none.

import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

// --- stripErasableTypeScript: duplicated from
// tool-call-bounds-harness.mjs (see its header for scope and the
// loud-failure contract). Kept as a copy so each fixture reads standalone;
// extend both together when the extension gains new syntax. ---

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
  console.error("usage: node tool-result-trim-harness.mjs <extensionPath>");
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
        "(tool-result-trim-harness.mjs, and its twins) to cover it. node --check said:\n" +
        detail,
    );
  }
}
checkStrippedSyntax(stripped);

const dataURL = "data:text/javascript;base64," + Buffer.from(stripped, "utf8").toString("base64");
const mod = await import(dataURL);

if (typeof mod.trimErrorResultText !== "function" || typeof mod.MAX_ERROR_RESULT_CHARS !== "number") {
  console.log(JSON.stringify({ ok: false, reason: "trim helpers not exported" }));
  process.exit(0);
}

const { trimErrorResultText, MAX_ERROR_RESULT_CHARS, ERROR_RESULT_TRIM_MARKER } = mod;

// Pure-helper boundary cases over the failing shape: a ~46 KB error result
// (the observed rejection) must trim to the tail plus the marker, while a
// ~33 KB result (observed to replay cleanly) and anything at/under the bound
// passes through byte-identical.
const big = "E".repeat(46600);
const medium = "M".repeat(33800);
const trimmedBig = trimErrorResultText(big);
const helperCases = {
  bound: MAX_ERROR_RESULT_CHARS,
  emptyUnchanged: trimErrorResultText("") === "",
  shortUnchanged: trimErrorResultText("short failure") === "short failure",
  atBoundUnchanged: trimErrorResultText("a".repeat(MAX_ERROR_RESULT_CHARS)) === "a".repeat(MAX_ERROR_RESULT_CHARS),
  mediumUnchanged: trimErrorResultText(medium) === medium,
  bigTrims: trimmedBig !== big,
  bigKeepsTail: trimmedBig.startsWith("E".repeat(MAX_ERROR_RESULT_CHARS)) && trimmedBig.endsWith(ERROR_RESULT_TRIM_MARKER),
  bigLength: trimmedBig.length,
};

// Wiring: activate both lanes against a stub ExtensionAPI and drive the real
// registered `tool_result` handler with the failing shape.
async function activateLane(handshakeToken) {
  if (handshakeToken) process.env.DONMAI_PI_HANDSHAKE = handshakeToken;
  else delete process.env.DONMAI_PI_HANDSHAKE;
  const handlers = {};
  const sequence = [];
  const stubPi = {
    registerProvider() {},
    registerTool(definition) {
      sequence.push("tool:" + definition.name);
    },
    // pi's real loader APPENDS every handler per event (a list per event
    // name), so the stub records the same list shape instead of keeping
    // only the last handler.
    on(event, handler) {
      sequence.push("on:" + event);
      handlers[event] = [...(handlers[event] ?? []), handler];
    },
  };
  await mod.default(stubPi);
  delete process.env.DONMAI_PI_HANDSHAKE;
  return { handlers, sequence };
}

const interactive = await activateLane("");
const rpc = await activateLane("fixture-handshake-token");

async function drive(handler, event) {
  if (typeof handler !== "function") return { registered: false, verdict: null };
  const verdict = await handler(event, {});
  return { registered: true, verdict: verdict ?? null };
}

const largeError = {
  toolName: "bash",
  toolCallId: "call-1",
  isError: true,
  content: [{ type: "text", text: "HEAD-" + "x".repeat(100) + "-MARKER-" + big + "-TAIL" }],
};
const smallError = {
  toolName: "bash",
  toolCallId: "call-2",
  isError: true,
  content: [{ type: "text", text: "exit 1: boom" }],
};
const largeSuccess = {
  toolName: "bash",
  toolCallId: "call-3",
  isError: false,
  content: [{ type: "text", text: big }],
};
const mixedBlocks = {
  toolName: "bash",
  toolCallId: "call-4",
  isError: true,
  content: [{ type: "text", text: "short head" }, { type: "text", text: big }],
};

const wiring = {};
for (const [laneName, lane] of [["interactive", interactive], ["rpc", rpc]]) {
  // The trim rail is the lane's FIRST tool_result handler (registered before
  // the lane-specific handlers); drive exactly that one.
  const trimHandlers = lane.handlers["tool_result"] ?? [];
  const handler = trimHandlers[0];
  const laneOut = { hasHandler: typeof handler === "function" };
  const large = await drive(handler, structuredClone(largeError));
  const small = await drive(handler, structuredClone(smallError));
  const success = await drive(handler, structuredClone(largeSuccess));
  const mixed = await drive(handler, structuredClone(mixedBlocks));
  laneOut.largeErrorPatched =
    large.registered && large.verdict !== null && large.verdict.content !== undefined;
  if (laneOut.largeErrorPatched) {
    const text = large.verdict.content[0].text;
    laneOut.largeErrorKeepsTail = text.endsWith("-TAIL" + ERROR_RESULT_TRIM_MARKER);
    laneOut.largeErrorDropsHead = !text.includes("HEAD-");
    laneOut.largeErrorLength = text.length;
  }
  laneOut.smallErrorUntouched = small.verdict === null;
  laneOut.largeSuccessUntouched = success.verdict === null;
  if (mixed.verdict !== null && mixed.verdict.content !== undefined) {
    laneOut.mixedFirstBlockKept = mixed.verdict.content[0].text === "short head";
    laneOut.mixedSecondBlockTrimmed =
      mixed.verdict.content[1].text !== big && mixed.verdict.content[1].text.endsWith(ERROR_RESULT_TRIM_MARKER);
  } else {
    laneOut.mixedFirstBlockKept = false;
    laneOut.mixedSecondBlockTrimmed = false;
  }
  // The trim handler must be registered before the lane's tool_call fence so
  // every result is bounded regardless of adjudication ordering.
  const trimIdx = lane.sequence.indexOf("on:tool_result");
  const fenceIdx = lane.sequence.indexOf("on:tool_call");
  laneOut.trimBeforeFence = trimIdx >= 0 && fenceIdx >= 0 && trimIdx < fenceIdx;
  wiring[laneName] = laneOut;
}

console.log(JSON.stringify({ ok: true, helperCases, wiring }));
