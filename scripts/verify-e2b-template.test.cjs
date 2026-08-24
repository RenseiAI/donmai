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

test('verifyTemplateVersion destroys the probe sandbox after a successful assertion', async () => {
  const calls = []
  const Sandbox = {
    async create(templateRef, options) {
      calls.push(['create', templateRef, options])
      return {
        commands: {
          async run(command) {
            calls.push(['run', command])
            return { exitCode: 0, stdout: 'donmai version v0.68.5\n' }
          },
        },
        async kill(options) {
          calls.push(['kill', options])
        },
      }
    },
  }

  await verifyTemplateVersion({
    Sandbox,
    templateRef: 'donmai-worker:v0.68.5',
    expectedVersion: 'v0.68.5',
    apiKey: 'test-key',
  })

  assert.deepEqual(calls, [
    ['create', 'donmai-worker:v0.68.5', { apiKey: 'test-key', timeoutMs: 60_000 }],
    ['run', 'donmai --version'],
    ['kill', { apiKey: 'test-key' }],
  ])
})
