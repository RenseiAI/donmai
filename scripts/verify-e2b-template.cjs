#!/usr/bin/env node
'use strict'

const fs = require('node:fs')
const path = require('node:path')

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

// PI_VERSION_RE extracts a dotted X.Y.Z... version from free-form pi --version
// output (e.g. "pi 0.80.10" or a bare "0.80.10"), mirroring versionRe in
// provider/harness/pi/probe.go — pi is a third-party binary, so its
// `--version` output is not a string this repo controls the way it controls
// donmai's.
const PI_VERSION_RE = /\d+(?:\.\d+)+/

function assertPiVersionOutput(result, expectedVersion) {
  if (!expectedVersion) {
    throw new Error('the pinned pi version is required')
  }
  if (result.exitCode !== 0) {
    throw new Error(
      `pi --version exited ${result.exitCode}: ${String(result.stderr || '').trim()}`,
    )
  }
  const raw = String(result.stdout || '').trim()
  const match = raw.match(PI_VERSION_RE)
  const got = match ? match[0] : null
  if (got !== expectedVersion) {
    throw new Error(
      `pi --version = ${JSON.stringify(raw)}, parsed version ${JSON.stringify(got)}, want ${JSON.stringify(expectedVersion)}`,
    )
  }
}

// PINNED_PI_VERSION_RE pulls the value out of provider/harness/pi/probe.go's
// `PinnedVersion = "0.80.10"` constant so this script and the harness's
// probe-time enforcement can never independently drift — the same
// single-source-of-truth pattern matrix/cells.go uses for its generated
// binaryPins section.
const PINNED_PI_VERSION_RE = /\bPinnedVersion\s*=\s*"([^"]+)"/

function parsePinnedPiVersion(probeSource) {
  const match = probeSource.match(PINNED_PI_VERSION_RE)
  if (!match) {
    throw new Error('could not find PinnedVersion in provider/harness/pi/probe.go')
  }
  return match[1]
}

function readPinnedPiVersion(repoRoot) {
  const probePath = path.join(repoRoot, 'provider', 'harness', 'pi', 'probe.go')
  return parsePinnedPiVersion(fs.readFileSync(probePath, 'utf8'))
}

async function verifyTemplateVersion({ Sandbox, templateRef, expectedVersion, expectedPiVersion, apiKey }) {
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
    const piResult = await sandbox.commands.run('pi --version')
    assertPiVersionOutput(piResult, expectedPiVersion)
    process.stdout.write(
      `Verified ${templateRef}: donmai version ${expectedVersion}, pi version ${expectedPiVersion}\n`,
    )
  } catch (error) {
    primaryError = error
  }

  if (sandbox) {
    try {
      await sandbox.kill()
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

async function runMain({ Sandbox, templateRef, expectedVersion, expectedPiVersion, apiKey, writeError }) {
  try {
    await verifyTemplateVersion({
      Sandbox,
      templateRef,
      expectedVersion,
      expectedPiVersion,
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
    expectedPiVersion: readPinnedPiVersion(process.cwd()),
    apiKey: process.env.E2B_API_KEY,
    writeError: (line) => process.stderr.write(line),
  })
}

module.exports = {
  assertPiVersionOutput,
  assertVersionOutput,
  formatFailure,
  parsePinnedPiVersion,
  readPinnedPiVersion,
  runMain,
  verifyTemplateVersion,
}

if (require.main === module) {
  main().then((code) => {
    process.exitCode = code
  }).catch((error) => {
    process.stderr.write(`${formatFailure(error, process.env.E2B_API_KEY)}\n`)
    process.exitCode = 1
  })
}
