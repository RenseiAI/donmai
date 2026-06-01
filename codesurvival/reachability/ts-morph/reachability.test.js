#!/usr/bin/env node
// Lightweight test for reachability.js (no test framework; stdlib assert only).
// Builds a tiny fixture TS project in a temp dir, runs the script as a
// subprocess exactly as the Go executor does, and asserts the JSON contract:
//   - a reachable export (called from an entrypoint route) is "hot"
//   - a dead/unreferenced export in the same file is "cold"
//   - the output is the single-JSON-object contract reachability_ts.go parses
//
// Run: `node reachability.test.js` (wired as `npm test`). Exits non-zero on
// failure. This is also the golden-output fixture the Go test mirrors.

'use strict'

const assert = require('assert')
const cp = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')

function writeFixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cs-ts-fixture-'))
  fs.mkdirSync(path.join(dir, 'app', 'api', 'hello'), { recursive: true })
  // An entrypoint route that calls liveHelper but never deadHelper.
  fs.writeFileSync(
    path.join(dir, 'app', 'api', 'hello', 'route.ts'),
    [
      'export function liveHelper(): string {',
      '  return "alive"',
      '}',
      '',
      'export function deadHelper(): string {',
      '  return "never called"',
      '}',
      '',
      'export function GET(): Response {',
      '  return new Response(liveHelper())',
      '}',
      '',
    ].join('\n'),
  )
  return dir
}

function run() {
  // Skip gracefully if ts-morph isn't installed in this checkout — CI bakes it,
  // local dev may not. A skipped test still exits 0 so `npm test` is green.
  try {
    require.resolve('ts-morph')
  } catch (e) {
    console.log('SKIP: ts-morph not installed (run `npm install` to enable)')
    return
  }

  const repo = writeFixture()
  try {
    const out = cp.execFileSync(
      process.execPath,
      [path.join(__dirname, 'reachability.js'), '--repo', repo, '--files', 'app/api/hello/route.ts'],
      { encoding: 'utf8' },
    )
    const report = JSON.parse(out)
    assert.strictEqual(report.language, 'ts', 'language must be ts')
    assert.ok(report.status === 'ok' || report.status === 'partial', 'status enum')
    assert.ok(Array.isArray(report.symbols), 'symbols is an array')

    const byName = Object.fromEntries(report.symbols.map((s) => [s.symbol, s]))
    assert.ok(byName.GET, 'GET entrypoint present')
    assert.strictEqual(byName.GET.reachable, 'hot', 'GET (entrypoint) is hot')
    assert.ok(byName.liveHelper, 'liveHelper present')
    assert.strictEqual(byName.liveHelper.reachable, 'hot', 'liveHelper (called from GET) is hot')
    assert.ok(byName.deadHelper, 'deadHelper present')
    assert.strictEqual(byName.deadHelper.reachable, 'cold', 'deadHelper (unreferenced) is cold')

    for (const s of report.symbols) {
      assert.ok(s.startLine > 0, 'startLine > 0')
      assert.ok(s.endLine >= s.startLine, 'endLine >= startLine')
    }
    console.log('PASS: ts-morph reachability fixture (GET+liveHelper hot, deadHelper cold)')
  } finally {
    fs.rmSync(repo, { recursive: true, force: true })
  }
}

run()
