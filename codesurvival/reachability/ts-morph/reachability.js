#!/usr/bin/env node
// TS/JS reachability for code-survival (RW4), invoked as a subprocess by the
// donmai Go executor (codesurvival/reachability_ts.go).
//
// SUBPROCESS CONTRACT (must stay in lockstep with reachability_ts.go):
//
//   node reachability.js --repo <repoPath> [--files a.ts,b.tsx,...] \
//        [--timeout-ms 60000] [--max-files 50000]
//
//   stdout: ONE JSON object —
//     { "status": "ok"|"partial", "language": "ts",
//       "symbols": [ { file, symbol, startLine, endLine, reachable } ] }
//     reachable ∈ "hot" | "cold" | "unknown".
//
// Approach: seed from user-facing entrypoints (Next.js app/route/page, pages/api,
// package main/bin, cron/webhook handlers), walk the ts-morph reference graph
// from each seeded export, and classify every top-level function/method/exported
// declaration in the --files set: reachable from a seed → hot, in the project but
// unreachable → cold, unresolvable (dynamic import / no symbol) → unknown.
//
// PERF (monorepo scale): project-scoped loading (a single Project with
// skipFileDependencyResolution + the repo's tsconfig when present), a hard
// --timeout-ms wall and a --max-files cap. On exceed → status:"partial" (the Go
// side then sets hotWeighted=null; survival is untouched).
//
// DEGRADATION: any thrown error prints { status:"partial", ... , error } and
// exits 0 — the Go side treats a crash/non-zero-exit/unparseable-stdout as
// partial regardless, so exiting 0 keeps the JSON contract the happy path.

'use strict'

const path = require('path')

function parseArgs(argv) {
  const args = { repo: process.cwd(), files: [], timeoutMs: 60000, maxFiles: 50000 }
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--repo') args.repo = argv[++i]
    else if (a === '--files') args.files = (argv[++i] || '').split(',').filter(Boolean)
    else if (a === '--timeout-ms') args.timeoutMs = parseInt(argv[++i], 10) || 60000
    else if (a === '--max-files') args.maxFiles = parseInt(argv[++i], 10) || 50000
  }
  return args
}

function emit(report) {
  process.stdout.write(JSON.stringify(report))
}

// isEntrypointFile: a repo-relative path that the framework invokes directly.
// These files' exports are reachability roots even with no in-repo caller.
function isEntrypointFile(rel) {
  const p = rel.replace(/\\/g, '/')
  return (
    /(^|\/)app\/.*\/route\.[cm]?[jt]sx?$/.test(p) ||
    /(^|\/)app\/.*\/page\.[cm]?[jt]sx?$/.test(p) ||
    /(^|\/)app\/.*\/(layout|loading|error|not-found|template|default)\.[cm]?[jt]sx?$/.test(p) ||
    /(^|\/)pages\/api\//.test(p) ||
    /(^|\/)pages\//.test(p) ||
    /(^|\/)(cron|webhook|webhooks|handlers)\//.test(p) ||
    /(^|\/)(bin|cmd)\//.test(p) ||
    /(^|\/)(main|index|server)\.[cm]?[jt]sx?$/.test(p)
  )
}

// isFrameworkEntryExport reports whether an export NAME of an entrypoint file is
// one the framework actually invokes — NOT an arbitrary helper that happens to
// be exported. Only these become reachability roots:
//   - Next.js Route Handlers: GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS
//   - default export (page/layout/component, pages/api handler, bin/cmd main)
//   - Next.js segment exports invoked by the framework
//   - generateMetadata / generateStaticParams / middleware / register
// An exported helper (e.g. `deadHelper`) is NOT a root: it is only hot if it is
// referenced from one of these.
const HTTP_METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'])
const FRAMEWORK_EXPORTS = new Set([
  'default',
  'middleware',
  'register',
  'generateMetadata',
  'generateStaticParams',
  'generateViewport',
  'handler',
  'config',
  'main',
])
function isFrameworkEntryExport(name, rel) {
  if (FRAMEWORK_EXPORTS.has(name)) return true
  // HTTP method exports only count in Next.js route handlers / pages/api.
  const p = rel.replace(/\\/g, '/')
  if (HTTP_METHODS.has(name) && (/route\.[cm]?[jt]sx?$/.test(p) || /(^|\/)pages\/api\//.test(p))) {
    return true
  }
  return false
}

function main() {
  const args = parseArgs(process.argv)

  let tsMorph
  try {
    tsMorph = require('ts-morph')
  } catch (e) {
    // ts-morph not baked → cannot analyse. Partial (Go side: hotWeighted=null).
    emit({ status: 'partial', language: 'ts', symbols: [], error: 'ts-morph not installed' })
    return
  }
  const { Project, Node, SyntaxKind } = tsMorph

  const started = Date.now()
  const overBudget = () => Date.now() - started > args.timeoutMs

  // Project-scoped load. Prefer the repo tsconfig; fall back to a loose in-memory
  // project that just reads the source files. skipAddingFilesFromTsConfig avoids
  // pulling the entire monorepo when we only need the changed files + their refs.
  let project
  try {
    const fs = require('fs')
    const tsconfig = path.join(args.repo, 'tsconfig.json')
    const opts = {
      skipAddingFilesFromTsConfig: true,
      skipFileDependencyResolution: false,
      compilerOptions: { allowJs: true, checkJs: false, noEmit: true },
    }
    if (fs.existsSync(tsconfig)) opts.tsConfigFilePath = tsconfig
    project = new Project(opts)
  } catch (e) {
    emit({ status: 'partial', language: 'ts', symbols: [], error: 'project init: ' + String(e && e.message) })
    return
  }

  let partial = false

  // Add the changed (--files) sources plus all entrypoint files in the repo so
  // the reference graph has roots. Cap total to --max-files.
  let added = 0
  const addFile = (abs) => {
    if (added >= args.maxFiles) {
      partial = true
      return null
    }
    try {
      const sf = project.addSourceFileAtPathIfExists(abs)
      if (sf) added++
      return sf
    } catch (e) {
      partial = true
      return null
    }
  }

  const targetSources = []
  for (const rel of args.files) {
    const sf = addFile(path.join(args.repo, rel))
    if (sf) targetSources.push({ rel, sf })
  }

  // Seed roots: every exported declaration in an entrypoint file. We add
  // entrypoint files lazily by scanning the directories of the changed files'
  // nearest app/pages roots is expensive at monorepo scale, so we instead treat
  // any CHANGED file that is itself an entrypoint as a root, and resolve
  // references outward from there via ts-morph's findReferences.
  const rootSymbols = new Set()
  for (const { rel, sf } of targetSources) {
    if (!isEntrypointFile(rel)) continue
    for (const [name, decls] of sf.getExportedDeclarations()) {
      if (!isFrameworkEntryExport(name, rel)) continue
      for (const d of decls) rootSymbols.add(d)
    }
  }

  // Reachability: BFS over the CALLEE graph. A declaration is hot if it is a
  // root or is referenced (transitively) from the body of a hot declaration —
  // i.e. we walk what each hot symbol CALLS, following identifier references in
  // its body to their declarations. Bounded by the budget.
  const reachable = new Set()
  const queue = [...rootSymbols]
  while (queue.length) {
    if (overBudget()) { partial = true; break }
    const decl = queue.shift()
    if (!decl || reachable.has(decl)) continue
    reachable.add(decl)
    try {
      for (const callee of referencedDeclarations(decl, Node)) {
        if (callee && !reachable.has(callee)) queue.push(callee)
      }
    } catch (e) {
      partial = true
    }
  }

  // Classify every top-level declaration in the changed files.
  const symbols = []
  for (const { rel, sf } of targetSources) {
    for (const decl of topLevelDeclarations(sf, SyntaxKind)) {
      const name = declName(decl)
      if (!name) continue
      const start = decl.getStartLineNumber()
      const end = decl.getEndLineNumber()
      let reach = 'cold'
      if (reachable.has(decl)) reach = 'hot'
      else if (isUnresolvable(decl)) reach = 'unknown'
      symbols.push({ file: rel, symbol: name, startLine: start, endLine: end, reachable: reach })
    }
  }

  emit({ status: partial ? 'partial' : 'ok', language: 'ts', symbols })
}

// referencedDeclarations returns the set of declarations a symbol's body refers
// to (its callees): every identifier in the body whose symbol resolves to a
// declaration node. This is the edge set of the reachability graph walked from
// entrypoint roots.
function referencedDeclarations(decl, Node) {
  const out = []
  const seen = new Set()
  let body = decl
  // For a variable declaration backing an arrow/function, walk its initializer.
  try {
    if (Node.isVariableDeclaration(decl) && decl.getInitializer) {
      body = decl.getInitializer() || decl
    }
  } catch (e) {
    /* use decl as-is */
  }
  let identifiers = []
  try {
    identifiers = body.getDescendantsOfKind
      ? body.getDescendantsOfKind(require('ts-morph').SyntaxKind.Identifier)
      : []
  } catch (e) {
    return out
  }
  for (const id of identifiers) {
    let sym
    try {
      sym = id.getSymbol()
    } catch (e) {
      continue
    }
    if (!sym) continue
    let decls = []
    try {
      decls = sym.getDeclarations() || []
    } catch (e) {
      continue
    }
    for (const d of decls) {
      const owner = nearestNamedDeclaration(d, Node)
      if (owner && !seen.has(owner)) {
        seen.add(owner)
        out.push(owner)
      }
    }
  }
  return out
}

// nearestNamedDeclaration normalises a declaration node (or a node inside one)
// to the function/method/class/variable declaration we classify.
function nearestNamedDeclaration(node, Node) {
  let cur = node
  while (cur) {
    if (
      Node.isFunctionDeclaration(cur) ||
      Node.isMethodDeclaration(cur) ||
      Node.isClassDeclaration(cur) ||
      Node.isVariableDeclaration(cur)
    ) {
      return cur
    }
    cur = cur.getParent && cur.getParent()
  }
  return null
}

// topLevelDeclarations yields the classifiable symbols of a source file:
// function declarations, exported variable declarations, class declarations and
// their methods.
function topLevelDeclarations(sf) {
  const out = []
  for (const fn of sf.getFunctions()) out.push(fn)
  for (const cls of sf.getClasses()) {
    out.push(cls)
    for (const m of cls.getMethods()) out.push(m)
  }
  for (const v of sf.getVariableDeclarations()) {
    const init = v.getInitializer()
    if (init && /Function|Arrow/.test(init.getKindName())) out.push(v)
  }
  return out
}

function declName(decl) {
  try {
    if (typeof decl.getName === 'function') {
      const n = decl.getName()
      if (n) return n
    }
  } catch (e) {
    /* fallthrough */
  }
  return '<anonymous>'
}

// isUnresolvable marks a declaration whose reachability cannot be statically
// decided (dynamic import()/require, or it is itself referenced only dynamically).
// Such symbols are reported "unknown" → the Go side weights them as hot.
function isUnresolvable(decl) {
  try {
    const text = decl.getText()
    return /\bimport\s*\(|\brequire\s*\(/.test(text)
  } catch (e) {
    return true
  }
}

try {
  main()
} catch (e) {
  emit({ status: 'partial', language: 'ts', symbols: [], error: String((e && e.stack) || e) })
}
