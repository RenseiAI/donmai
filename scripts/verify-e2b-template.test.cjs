'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

const {
  assertPiVersionOutput,
  assertVersionOutput,
  parsePinnedPiVersion,
  readPinnedPiVersion,
  runMain,
  verifyTemplateVersion,
} = require('./verify-e2b-template.cjs')

test('assertVersionOutput accepts the exact immutable version', () => {
  assert.doesNotThrow(() =>
    assertVersionOutput({ exitCode: 0, stdout: 'donmai version v0.68.5\n' }, 'v0.68.5'),
  )
})

test('assertVersionOutput rejects a wrong version', () => {
  assert.throws(
    () => assertVersionOutput({ exitCode: 0, stdout: 'donmai version dev\n' }, 'v0.68.5'),
    /want "donmai version v0\.68\.5"/,
  )
})

test('assertPiVersionOutput accepts the pinned version inside free-form output', () => {
  assert.doesNotThrow(() =>
    assertPiVersionOutput({ exitCode: 0, stdout: 'pi 0.80.10\n' }, '0.80.10'),
  )
})

test('assertPiVersionOutput accepts a bare version with no prefix', () => {
  assert.doesNotThrow(() => assertPiVersionOutput({ exitCode: 0, stdout: '0.80.10\n' }, '0.80.10'))
})

test('assertPiVersionOutput rejects a wrong version', () => {
  assert.throws(
    () => assertPiVersionOutput({ exitCode: 0, stdout: 'pi 0.80.9\n' }, '0.80.10'),
    /want "0\.80\.10"/,
  )
})

test('assertPiVersionOutput rejects a nonzero exit code', () => {
  assert.throws(
    () => assertPiVersionOutput({ exitCode: 127, stdout: '', stderr: 'pi: command not found' }, '0.80.10'),
    /pi --version exited 127/,
  )
})

test('assertPiVersionOutput requires an expected version', () => {
  assert.throws(
    () => assertPiVersionOutput({ exitCode: 0, stdout: 'pi 0.80.10\n' }, ''),
    /the pinned pi version is required/,
  )
})

test('parsePinnedPiVersion extracts the constant value', () => {
  const source = [
    'package pi',
    '',
    'const (',
    '\tMinVersion = "0.80.10"',
    '',
    '\tPinnedVersion = "0.80.10"',
    ')',
    '',
  ].join('\n')
  assert.equal(parsePinnedPiVersion(source), '0.80.10')
})

test('parsePinnedPiVersion rejects source with no PinnedVersion constant', () => {
  assert.throws(
    () => parsePinnedPiVersion('package pi\n'),
    /could not find PinnedVersion/,
  )
})

test('readPinnedPiVersion reads the constant from provider/harness/pi/probe.go under a repo root', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-template-'))
  const probeDir = path.join(repoRoot, 'provider', 'harness', 'pi')
  fs.mkdirSync(probeDir, { recursive: true })
  fs.writeFileSync(path.join(probeDir, 'probe.go'), 'package pi\n\nconst PinnedVersion = "0.80.10"\n')

  try {
    assert.equal(readPinnedPiVersion(repoRoot), '0.80.10')
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

function makeSandbox({ launchError, commandResult, commandError, piCommandResult, piCommandError, cleanupError }) {
  const calls = []
  const Sandbox = {
    async create(templateRef, options) {
      calls.push(['create', templateRef, options])
      if (launchError) {
        throw launchError
      }
      return {
        commands: {
          async run(command) {
            calls.push(['run', command])
            if (command === 'donmai --version') {
              if (commandError) {
                throw commandError
              }
              return commandResult || { exitCode: 0, stdout: 'donmai version v0.68.5\n' }
            }
            if (command === 'pi --version') {
              if (piCommandError) {
                throw piCommandError
              }
              return piCommandResult || { exitCode: 0, stdout: 'pi 0.80.10\n' }
            }
            throw new Error(`unexpected command: ${command}`)
          },
        },
        async kill(...args) {
          calls.push(['kill', ...args])
          if (cleanupError) {
            throw cleanupError
          }
        },
      }
    },
  }

  return { Sandbox, calls }
}

async function runProbe(Sandbox) {
  return verifyTemplateVersion({
    Sandbox,
    templateRef: 'donmai-worker:v0.68.5',
    expectedVersion: 'v0.68.5',
    expectedPiVersion: '0.80.10',
    apiKey: 'test-key',
  })
}

function expectedCalls({ runs = ['donmai --version', 'pi --version'], kill = true, apiKey = 'test-key' } = {}) {
  const calls = [['create', 'donmai-worker:v0.68.5', { apiKey, timeoutMs: 60_000 }]]
  for (const run of runs) {
    calls.push(['run', run])
  }
  if (kill) calls.push(['kill'])
  return calls
}

test('verifyTemplateVersion propagates launch failure without a sandbox cleanup', async () => {
  const launchError = new Error('PRIMARY launch failure')
  const { Sandbox, calls } = makeSandbox({ launchError })

  await assert.rejects(() => runProbe(Sandbox), launchError)
  assert.deepEqual(calls, expectedCalls({ runs: [], kill: false }))
})

test('verifyTemplateVersion preserves donmai command failure after cleanup', async () => {
  const commandError = new Error('PRIMARY command failure')
  const { Sandbox, calls } = makeSandbox({ commandError })

  await assert.rejects(() => runProbe(Sandbox), commandError)
  assert.deepEqual(calls, expectedCalls({ runs: ['donmai --version'] }))
})

test('verifyTemplateVersion preserves donmai version mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeSandbox({
    commandResult: { exitCode: 0, stdout: 'donmai version dev\n' },
  })

  await assert.rejects(() => runProbe(Sandbox), /want "donmai version v0\.68\.5"/)
  assert.deepEqual(calls, expectedCalls({ runs: ['donmai --version'] }))
})

test('verifyTemplateVersion preserves pi command failure after cleanup', async () => {
  const piCommandError = new Error('PRIMARY pi command failure')
  const { Sandbox, calls } = makeSandbox({ piCommandError })

  await assert.rejects(() => runProbe(Sandbox), piCommandError)
  assert.deepEqual(calls, expectedCalls())
})

test('verifyTemplateVersion preserves pi version mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeSandbox({
    piCommandResult: { exitCode: 0, stdout: 'pi 0.80.9\n' },
  })

  await assert.rejects(() => runProbe(Sandbox), /want "0\.80\.10"/)
  assert.deepEqual(calls, expectedCalls())
})

test('verifyTemplateVersion reports cleanup-only failure', async () => {
  const cleanupError = new Error('CLEANUP kill failure')
  const { Sandbox, calls } = makeSandbox({ cleanupError })

  await assert.rejects(() => runProbe(Sandbox), cleanupError)
  assert.deepEqual(calls, expectedCalls())
})

test('verifyTemplateVersion retains primary and cleanup evidence when both fail', async () => {
  const commandError = new Error('PRIMARY command failure')
  const cleanupError = new Error('CLEANUP kill failure')
  const { Sandbox, calls } = makeSandbox({ commandError, cleanupError })

  await assert.rejects(() => runProbe(Sandbox), (error) => {
    assert.ok(error instanceof AggregateError)
    assert.equal(error.cause, commandError)
    assert.deepEqual(error.errors, [commandError, cleanupError])
    return true
  })
  assert.deepEqual(calls, expectedCalls({ runs: ['donmai --version'] }))
})

test('verifyTemplateVersion destroys the probe sandbox after a successful assertion', async () => {
  const { Sandbox, calls } = makeSandbox({})

  await runProbe(Sandbox)

  assert.deepEqual(calls, expectedCalls())
})

test('runMain prints both primary and cleanup failure messages without the API key', async () => {
  const commandError = new Error('PRIMARY command failure for secret-key')
  const cleanupError = new Error('CLEANUP kill failure for secret-key')
  const { Sandbox, calls } = makeSandbox({ commandError, cleanupError })
  const output = []

  const code = await runMain({
    Sandbox,
    templateRef: 'donmai-worker:v0.68.5',
    expectedVersion: 'v0.68.5',
    expectedPiVersion: '0.80.10',
    apiKey: 'secret-key',
    writeError: (line) => output.push(line),
  })

  assert.equal(code, 1)
  assert.deepEqual(calls, expectedCalls({ runs: ['donmai --version'], apiKey: 'secret-key' }))
  assert.match(output.join(''), /PRIMARY command failure for \*\*\*/)
  assert.match(output.join(''), /CLEANUP kill failure for \*\*\*/)
  assert.doesNotMatch(output.join(''), /secret-key/)
})

test('runMain prints a single failure clearly without aggregate labels', async () => {
  const commandError = new Error('PRIMARY command failure')
  const { Sandbox, calls } = makeSandbox({ commandError })
  const output = []

  const code = await runMain({
    Sandbox,
    templateRef: 'donmai-worker:v0.68.5',
    expectedVersion: 'v0.68.5',
    expectedPiVersion: '0.80.10',
    apiKey: 'test-key',
    writeError: (line) => output.push(line),
  })

  assert.equal(code, 1)
  assert.deepEqual(calls, expectedCalls({ runs: ['donmai --version'] }))
  assert.equal(output.join(''), 'E2B template version verification failed: PRIMARY command failure\n')
})

test('runMain succeeds when both donmai and pi report their pinned versions', async () => {
  const { Sandbox, calls } = makeSandbox({})
  const output = []

  const code = await runMain({
    Sandbox,
    templateRef: 'donmai-worker:v0.68.5',
    expectedVersion: 'v0.68.5',
    expectedPiVersion: '0.80.10',
    apiKey: 'test-key',
    writeError: (line) => output.push(line),
  })

  assert.equal(code, 0)
  assert.deepEqual(calls, expectedCalls())
  assert.deepEqual(output, [])
})
