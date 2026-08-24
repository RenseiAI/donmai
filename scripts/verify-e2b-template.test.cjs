'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')

const { assertVersionOutput, verifyTemplateVersion } = require('./verify-e2b-template.cjs')

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

function makeSandbox({ launchError, commandResult, commandError, cleanupError }) {
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
            if (commandError) {
              throw commandError
            }
            return commandResult || { exitCode: 0, stdout: 'donmai version v0.68.5\n' }
          },
        },
        async kill(options) {
          calls.push(['kill', options])
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
    apiKey: 'test-key',
  })
}

function expectedCalls(includeRun = true, includeKill = true) {
  const calls = [['create', 'donmai-worker:v0.68.5', { apiKey: 'test-key', timeoutMs: 60_000 }]]
  if (includeRun) calls.push(['run', 'donmai --version'])
  if (includeKill) calls.push(['kill', { apiKey: 'test-key' }])
  return calls
}

test('verifyTemplateVersion propagates launch failure without a sandbox cleanup', async () => {
  const launchError = new Error('PRIMARY launch failure')
  const { Sandbox, calls } = makeSandbox({ launchError })

  await assert.rejects(() => runProbe(Sandbox), launchError)
  assert.deepEqual(calls, expectedCalls(false, false))
})

test('verifyTemplateVersion preserves command failure after cleanup', async () => {
  const commandError = new Error('PRIMARY command failure')
  const { Sandbox, calls } = makeSandbox({ commandError })

  await assert.rejects(() => runProbe(Sandbox), commandError)
  assert.deepEqual(calls, expectedCalls())
})

test('verifyTemplateVersion preserves exact version mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeSandbox({
    commandResult: { exitCode: 0, stdout: 'donmai version dev\n' },
  })

  await assert.rejects(() => runProbe(Sandbox), /want "donmai version v0\.68\.5"/)
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
  assert.deepEqual(calls, expectedCalls())
})

test('verifyTemplateVersion destroys the probe sandbox after a successful assertion', async () => {
  const { Sandbox, calls } = makeSandbox({})

  await runProbe(Sandbox)

  assert.deepEqual(calls, expectedCalls())
})
