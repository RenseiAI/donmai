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
// NOT ASSUMED: a Node version with native `.ts` type-stripping. That was
// this fixture's first cut, and it broke on any runner whose `node` predates
// the feature (`ERR_UNKNOWN_FILE_EXTENSION` — the loader refuses the `.ts`
// extension outright, before even looking at the content) — an environment
// assumption this always-run lane must never make. The precondition is
// self-provided instead: stripErasableTypeScript below converts the
// extension's known-narrow TypeScript surface (type-only imports, interface
// blocks, parameter/variable/return type annotations, `as Type` assertions —
// exactly the subset donmai-policy.ts's own header comment commits to using,
// "intentionally dependency-free... a type-only import") into equivalent
// plain JavaScript before the module is ever imported, so this harness runs
// identically on any Node from roughly v14 onward. It is deliberately NOT a
// general TypeScript parser — if a future edit to the extension introduces a
// shape this does not handle, the stripped source fails Node's OWN syntax
// check and this harness exits nonzero with that SyntaxError printed
// verbatim (see checkStrippedSyntax below): a loud, diagnosable failure,
// never a silent skip.
//
// Usage: node interactive-local-tool-policy-harness.mjs <extensionPath> <toolName> <inputJSON>
// Env: DONMAI_PI_ALLOWED_TOOLS / DONMAI_PI_DISALLOWED_TOOLS (JSON arrays, may
// be absent or "[]"); DONMAI_PI_HANDSHAKE must NOT be set by the caller.

import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

// --- stripErasableTypeScript: a small, verified, comment/string-aware
// TypeScript-to-JavaScript eraser (see the file header above for scope and
// the loud-failure contract). ---

function stripErasableTypeScript(src) {
  let s = src;
  s = s.replace(/^import type .*;\r?\n/gm, "");
  s = s.replace(/^interface\s+\w+\s*\{[\s\S]*?\n\}\r?\n/gm, "");
  s = stripAsAssertions(s);
  s = stripParamAndReturnTypes(s);
  s = stripVarAnnotations(s);
  return s;
}

// scanRegions marks every byte of src as "code" (1) or "not code" (0) —
// inside a `//` line comment, a `/* */` block comment, or a string/template
// literal — so every transform below only ever acts on real code, never on
// prose that happens to contain code-shaped punctuation (e.g. a doc comment
// mentioning `subject():`).
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
      // Template literal: `${...}` interpolation spans are CODE again.
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
      i = j;
      continue;
    }
    codeMask[i] = 1;
    i++;
  }
  return codeMask;
}

// stripAsAssertions removes `as Type` — the type is either a balanced
// `{ ... }` object type or a bare dotted identifier (`never`, `unknown`,
// `Foo.Bar[]`).
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

// stripParamAndReturnTypes erases `: Type` in two positions, tracked with a
// bracket STACK (not a bare depth counter) so a `{`/`[` nested inside a
// paren — an object/array literal passed as a call argument, e.g.
// `out.push({ raw: trimmed, ... })` — is never mistaken for a parameter
// list:
//
//   1. Parameter type annotations: `ident: Type`, only when the innermost
//      open bracket at that position is `(` (directly inside a paren, not
//      inside a nested `{`/`[` within it). Generic type arguments
//      (`Record<string, unknown>`) are tracked as their own nesting level so
//      their internal comma never terminates the type early.
//   2. Return-type annotations: `): Type {` / `): Type =>`, checked right
//      after a `)` pops the stack back to empty — this file's function
//      signatures are never themselves nested inside another paren. An
//      object-type return (`{ block: true; ... }`) may be followed by
//      trailing union members (`| undefined`) before the real function-body
//      brace; both are walked past.
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
            if (innerStack.length === 0) break; // top-level close of the param list
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

// stripVarAnnotations erases `let x: T;` / `const x: T = v;` variable
// declarator type annotations.
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

const [, , extensionPath, toolName, inputJSON] = process.argv;

if (!extensionPath || !toolName) {
  console.error("usage: node interactive-local-tool-policy-harness.mjs <extensionPath> <toolName> <inputJSON>");
  process.exit(2);
}

const source = readFileSync(extensionPath, "utf8");
const stripped = stripErasableTypeScript(source);

// checkStrippedSyntax fails LOUDLY (never a silent skip) when the stripper's
// output is not valid JavaScript: it runs the stripped source through this
// same node binary's own `--check` (syntax-only, no execution), and on
// failure re-throws node's own SyntaxError text verbatim so the Go test's
// failure message names the exact offending construct — the signal a future
// maintainer needs to extend the stripper, not a reason to treat this
// fixture as unavailable.
function checkStrippedSyntax(code) {
  try {
    execFileSync(process.execPath, ["--input-type=module", "--check"], { input: code, stdio: ["pipe", "pipe", "pipe"] });
  } catch (err) {
    const detail = err.stderr ? err.stderr.toString() : String(err);
    throw new Error(
      "stripErasableTypeScript produced invalid JavaScript from " +
        extensionPath +
        " — the extension gained a TypeScript shape this harness's stripper does not handle yet; update stripErasableTypeScript " +
        "(interactive-local-tool-policy-harness.mjs) to cover it. node --check said:\n" +
        detail,
    );
  }
}
checkStrippedSyntax(stripped);

// Import via a base64 data: URL — no temp file, no filesystem cleanup, and
// no dependency on the CALLER's cwd being writable. data: URL dynamic
// import() is a stable, unflagged Node ESM feature (unlike `.ts` type
// stripping), so this works on every Node version this harness targets.
const dataURL = "data:text/javascript;base64," + Buffer.from(stripped, "utf8").toString("base64");

const handlers = {};
const stubPi = {
  registerProvider() {},
  registerTool() {},
  on(event, handler) {
    handlers[event] = handler;
  },
};

const mod = await import(dataURL);
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
