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

if (typeof mod.resolveBashTimeoutSeconds !== "function" || typeof mod.withPipefailPrelude !== "function") {
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
console.log(JSON.stringify({ ok: true, timeoutCases, preludeCases }));
