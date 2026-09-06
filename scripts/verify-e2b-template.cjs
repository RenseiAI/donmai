#!/usr/bin/env node
'use strict'

function assertVersionOutput(result, expectedVersion) {
  if (!expectedVersion) {
    throw new Error('DONMAI_VERSION is required')
  }
  if (result.exitCode !== 0) {
    throw new Error(
      `donmai --version exited ${result.exitCode}: ${String(result.stderr || '').trim()}`,
    )
  }
  const got = String(result.stdout || '').trim()
  const want = `donmai version ${expectedVersion}`
  if (got !== want) {
    throw new Error(`donmai --version = ${JSON.stringify(got)}, want ${JSON.stringify(want)}`)
  }
}

async function verifyTemplateVersion({ Sandbox, templateRef, expectedVersion, apiKey }) {
  if (!templateRef) {
    throw new Error('E2B_TEMPLATE_REF is required')
  }
  if (!apiKey) {
    throw new Error('E2B_API_KEY is required')
  }

  let sandbox
  let primaryError
  try {
    sandbox = await Sandbox.create(templateRef, { apiKey, timeoutMs: 60_000 })
    const result = await sandbox.commands.run('donmai --version')
    assertVersionOutput(result, expectedVersion)
    process.stdout.write(`Verified ${templateRef}: donmai version ${expectedVersion}\n`)
  } catch (error) {
    primaryError = error
  }

  if (sandbox) {
    try {
      await sandbox.kill({ apiKey })
    } catch (cleanupError) {
      if (primaryError) {
        throw new AggregateError(
          [primaryError, cleanupError],
          'E2B template version verification and sandbox cleanup both failed',
          { cause: primaryError },
        )
      }
      throw cleanupError
    }
  }

  if (primaryError) {
    throw primaryError
  }
}

function redact(message, secret) {
  if (!secret) {
    return message
  }
  return message.split(secret).join('***')
}

function formatFailure(error, apiKey) {
  if (error instanceof AggregateError) {
    const [primaryError, cleanupError] = error.errors
    return [
      'E2B template version verification failed:',
      `primary: ${redact(String(primaryError?.message || primaryError), apiKey)}`,
      `cleanup: ${redact(String(cleanupError?.message || cleanupError), apiKey)}`,
    ].join('\n')
  }
  return `E2B template version verification failed: ${redact(String(error?.message || error), apiKey)}`
}

async function runMain({ Sandbox, templateRef, expectedVersion, apiKey, writeError }) {
  try {
    await verifyTemplateVersion({
      Sandbox,
      templateRef,
      expectedVersion,
      apiKey,
    })
    return 0
  } catch (error) {
    writeError(`${formatFailure(error, apiKey)}\n`)
    return 1
  }
}

async function main() {
  const { Sandbox } = require('e2b')
  return runMain({
    Sandbox,
    templateRef: process.env.E2B_TEMPLATE_REF,
    expectedVersion: process.env.DONMAI_VERSION,
    apiKey: process.env.E2B_API_KEY,
    writeError: (line) => process.stderr.write(line),
  })
}

module.exports = { assertVersionOutput, formatFailure, runMain, verifyTemplateVersion }

if (require.main === module) {
  main().then((code) => {
    process.exitCode = code
  }).catch((error) => {
    process.stderr.write(`${formatFailure(error, process.env.E2B_API_KEY)}\n`)
    process.exitCode = 1
  })
}
