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

const GO_VERSION_RE = /^\s*go\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)/m
const GOLANGCI_LINT_PIN_RE =
  /go\s+install\s+github\.com\/golangci\/golangci-lint\/v2\/cmd\/golangci-lint@v([0-9]+\.[0-9]+\.[0-9]+)/
const GOFUMPT_PIN_RE = /go\s+install\s+mvdan\.cc\/gofumpt@v([0-9]+\.[0-9]+\.[0-9]+)/

function parseGoVersion(goModSource) {
  const match = goModSource.match(GO_VERSION_RE)
  if (!match) {
    throw new Error('could not find go directive in go.mod')
  }
  return match[1]
}

function readGoVersion(repoRoot) {
  return parseGoVersion(fs.readFileSync(path.join(repoRoot, 'go.mod'), 'utf8'))
}

function parseGolangciLintVersion(dockerfileSource) {
  const match = dockerfileSource.match(GOLANGCI_LINT_PIN_RE)
  if (!match) {
    throw new Error('could not find golangci-lint pin in the Go Dockerfile')
  }
  return match[1]
}

function parseGofumptVersion(dockerfileSource) {
  const match = dockerfileSource.match(GOFUMPT_PIN_RE)
  if (!match) {
    throw new Error('could not find gofumpt pin in the Go Dockerfile')
  }
  return match[1]
}

function readGoDockerfileSource(repoRoot) {
  const candidates = [
    path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
    path.join(repoRoot, 'e2b.go.Dockerfile'),
  ]
  for (const candidate of candidates) {
    try {
      return fs.readFileSync(candidate, 'utf8')
    } catch {
      // try the next candidate
    }
  }
  throw new Error(
    'could not find e2b.go.Dockerfile under ' + JSON.stringify(repoRoot)
  )
}

function hasGoTemplate(repoRoot) {
  if (typeof repoRoot !== 'string' || repoRoot === '') {
    return false
  }
  try {
    return fs.existsSync(path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'))
  } catch {
    return false
  }
}

function isGoTemplateRef(templateRef) {
  return typeof templateRef === 'string' && templateRef.includes('donmai-worker-go')
}

// parseAptInstalledPackages extracts the package names from executable
// `apt-get install` instructions, ignoring Dockerfile comment lines. Header
// comments may name packages (for example the race-detector note below), so
// matching the raw Dockerfile text would stay green after deleting the real
// `apt-get install` entries.
function parseAptInstalledPackages(dockerfileSource) {
  const executable = String(dockerfileSource)
    .split('\n')
    .filter((line) => !/^\s*#/.test(line))
    .join('\n')
  const normalized = executable.replace(/\\\n/g, ' ')
  const packages = []
  const installRe = /apt-get\s+install\s+([^&;]+)/g
  let match
  while ((match = installRe.exec(normalized)) !== null) {
    for (const token of match[1].split(/\s+/)) {
      const name = token.trim()
      if (!name || name.startsWith('-')) {
        continue
      }
      packages.push(name)
    }
  }
  return packages
}

function hasCToolchainForRace(dockerfileSource) {
  const installed = parseAptInstalledPackages(dockerfileSource)
  return (
    installed.includes('gcc') &&
    (installed.includes('libc6-dev') || installed.includes('build-essential'))
  )
}

// parseE2bPathRaw returns the raw ENV PATH value exactly as the pinned E2B
// SDK backend sees it via fromDockerfile: surrounding quotes are retained,
// not stripped the way an ordinary Dockerfile parser or shell would. A quoted
// `ENV PATH="..."` therefore yields a value starting with `"` whose first
// colon-separated entry cannot resolve `go`.
function parseE2bPathRaw(dockerfileSource) {
  const lines = String(dockerfileSource).split('\n')
  let raw = null
  for (const line of lines) {
    if (/^\s*#/.test(line)) {
      continue
    }
    const trimmed = line.trim()
    if (!/^ENV\b/i.test(trimmed)) {
      continue
    }
    let match = trimmed.match(/\bPATH\s*=\s*("[^"]*"|'[^']*'|[^\s]+)/)
    if (match) {
      raw = match[1]
      continue
    }
    match = trimmed.match(/^ENV\s+PATH\s+("[^"]*"|'[^']*'|[^\s]+)\s*$/i)
    if (match) {
      raw = match[1]
    }
  }
  return raw
}

function parseE2bPathEntries(dockerfileSource) {
  const raw = parseE2bPathRaw(dockerfileSource)
  if (raw === null) {
    return null
  }
  return raw.split(':')
}

// hasE2bCompatibleGoPath reports whether `go` stays resolvable after Debian
// bookworm's login-shell PATH reset. E2B runs build steps and SDK version
// checks under `/bin/bash -l -c`, and /etc/profile overwrites the incoming
// PATH (dropping /usr/local/go/bin) for both root and the sandbox user.
// ENV PATH alone therefore cannot make `go` visible there — checking only
// the ENV text returns true for a template whose first `go version` exits
// 127. This predicate requires a login-shell-persistent provision instead:
// a symlink/copy of the Go binary into a directory on the login default
// PATH (/usr/local/bin, /usr/bin), or a profile.d fragment that re-exports
// /usr/local/go/bin. Comment lines are ignored so a header note alone cannot
// satisfy it (same executable-instruction rule as parseAptInstalledPackages).
function hasE2bCompatibleGoPath(dockerfileSource) {
  const executable = String(dockerfileSource)
    .split('\n')
    .filter((line) => !/^\s*#/.test(line))
    .join('\n')
    .replace(/\\\n/g, ' ')
  const onDefaultPath =
    /\bln\s+[^&;\n]*\/usr\/local\/go\/bin\/go\s+[^&;\n]*\/usr\/local\/bin\/go/.test(
      executable
    ) ||
    /\bln\s+[^&;\n]*\/usr\/local\/go\/bin\/go\s+[^&;\n]*\/usr\/bin\/go/.test(
      executable
    ) ||
    /\bcp\s+[^&;\n]*\/usr\/local\/go\/bin\/go\s+[^&;\n]*\/usr\/(local\/)?bin\//.test(
      executable
    ) ||
    /\binstall\s+[^&;\n]*\/usr\/local\/go\/bin\/go\s+[^&;\n]*\/usr\/(local\/)?bin\//.test(
      executable
    )
  const viaProfileFragment =
    /\/etc\/profile\.d\//.test(executable) &&
    /\/usr\/local\/go\/bin/.test(executable)
  return onDefaultPath || viaProfileFragment
}

function resolveRepoRoot({ env, cwd, scriptDir } = {}) {
  const sourceEnv = env || process.env
  const sourceCwd = cwd || process.cwd()
  const sourceDir = scriptDir || __dirname
  const candidates = []
  if (sourceEnv.DONMAI_REPO_ROOT) {
    candidates.push(sourceEnv.DONMAI_REPO_ROOT)
  }
  candidates.push(sourceCwd)
  candidates.push(path.join(sourceDir, '..'))
  for (const candidate of candidates) {
    try {
      if (
        fs.existsSync(path.join(candidate, 'go.mod')) &&
        fs.existsSync(path.join(candidate, 'worker', 'e2b', 'e2b.go.Dockerfile'))
      ) {
        return candidate
      }
    } catch {
      // try the next candidate
    }
  }
  return sourceEnv.DONMAI_REPO_ROOT || sourceCwd
}

function shouldExpectGoTools({ templateRef, expectGoTools }) {
  if (expectGoTools === '1' || expectGoTools === 'true') {
    return true
  }
  return isGoTemplateRef(templateRef)
}

function extractGoToolchainVersion(output) {
  const match = String(output).match(/\bgo(\d+\.\d+\.\d+)(?![\w.+-])/)
  return match ? match[1] : null
}

function extractVersionAfterKeyword(output) {
  const match = String(output).match(/version\s+v?(\d+\.\d+\.\d+)(?![\w.+-])/i)
  return match ? match[1] : null
}

function extractFirstDottedVersion(output) {
  const match = String(output).match(/\bv?(\d+\.\d+\.\d+)(?![\w.+-])/)
  return match ? match[1] : null
}

function assertGoVersionOutput(result, expectedGoVersion) {
  if (!expectedGoVersion) {
    throw new Error('the expected Go version is required')
  }
  if (result.exitCode !== 0) {
    throw new Error(
      `go version exited ${result.exitCode}: ${String(result.stderr || '').trim()}`
    )
  }
  const raw = String(result.stdout || '').trim()
  const got = extractGoToolchainVersion(raw)
  if (got !== String(expectedGoVersion).replace(/^go/, '')) {
    throw new Error(
      `go version = ${JSON.stringify(raw)}, parsed version ${JSON.stringify(got)}, want ${JSON.stringify(String(expectedGoVersion))}`
    )
  }
}

function assertGolangciLintVersionOutput(result, expectedVersion) {
  if (!expectedVersion) {
    throw new Error('the expected golangci-lint version is required')
  }
  if (result.exitCode !== 0) {
    throw new Error(
      `golangci-lint --version exited ${result.exitCode}: ${String(result.stderr || '').trim()}`
    )
  }
  const raw = `${String(result.stdout || '')}\n${String(result.stderr || '')}`.trim()
  const want = String(expectedVersion).replace(/^v/, '')
  const got = extractVersionAfterKeyword(raw)
  if (got !== want) {
    throw new Error(
      `golangci-lint --version = ${JSON.stringify(raw.trim())}, parsed version ${JSON.stringify(got)}, want ${JSON.stringify(want)}`
    )
  }
}

function assertGofumptVersionOutput(result, expectedVersion) {
  if (!expectedVersion) {
    throw new Error('the expected gofumpt version is required')
  }
  if (result.exitCode !== 0) {
    throw new Error(
      `gofumpt --version exited ${result.exitCode}: ${String(result.stderr || '').trim()}`
    )
  }
  const raw = `${String(result.stdout || '')}\n${String(result.stderr || '')}`.trim()
  const want = String(expectedVersion).replace(/^v/, '')
  const got = extractFirstDottedVersion(raw)
  if (got !== want) {
    throw new Error(
      `gofumpt --version = ${JSON.stringify(raw.trim())}, parsed version ${JSON.stringify(got)}, want ${JSON.stringify(want)}`
    )
  }
}

async function verifyTemplateVersion({
  Sandbox,
  templateRef,
  expectedVersion,
  expectedPiVersion,
  apiKey,
  expectedGoVersion,
  expectedGolangciLintVersion,
  expectedGofumptVersion,
}) {
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
    const expectGo =
      expectedGoVersion !== undefined ||
      expectedGolangciLintVersion !== undefined ||
      expectedGofumptVersion !== undefined
    if (expectGo) {
      if (!expectedGoVersion || !expectedGolangciLintVersion || !expectedGofumptVersion) {
        throw new Error('Go toolchain expectations are incomplete')
      }
      const goResult = await sandbox.commands.run('go version')
      assertGoVersionOutput(goResult, expectedGoVersion)
      const lintResult = await sandbox.commands.run('golangci-lint --version')
      assertGolangciLintVersionOutput(lintResult, expectedGolangciLintVersion)
      const fumptResult = await sandbox.commands.run('gofumpt --version')
      assertGofumptVersionOutput(fumptResult, expectedGofumptVersion)
    }
    let verified = `Verified ${templateRef}: donmai version ${expectedVersion}, pi version ${expectedPiVersion}`
    if (expectGo) {
      verified += `, go version ${expectedGoVersion}, golangci-lint version ${expectedGolangciLintVersion}, gofumpt version ${expectedGofumptVersion}`
    }
    process.stdout.write(`${verified}\n`)
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

async function runMain({
  Sandbox,
  templateRef,
  expectedVersion,
  expectedPiVersion,
  apiKey,
  expectedGoVersion,
  expectedGolangciLintVersion,
  expectedGofumptVersion,
  writeError,
}) {
  try {
    await verifyTemplateVersion({
      Sandbox,
      templateRef,
      expectedVersion,
      expectedPiVersion,
      apiKey,
      expectedGoVersion,
      expectedGolangciLintVersion,
      expectedGofumptVersion,
    })
    return 0
  } catch (error) {
    writeError(`${formatFailure(error, apiKey)}\n`)
    return 1
  }
}

async function main(overrides = {}) {
  const env = overrides.env || process.env
  const cwd = overrides.cwd || process.cwd()
  const loadSandbox = overrides.loadSandbox || (() => require('e2b').Sandbox)
  const readGo = overrides.readGoVersion || readGoVersion
  const readDocker = overrides.readGoDockerfileSource || readGoDockerfileSource
  const readPi = overrides.readPinnedPiVersion || readPinnedPiVersion
  const runMainFn = overrides.runMain || runMain
  const writeError = overrides.writeError || ((line) => process.stderr.write(line))
  const Sandbox = loadSandbox()
  const repoRoot = env.DONMAI_REPO_ROOT || cwd
  const templateRef = env.E2B_TEMPLATE_REF
  let expectedGoVersion
  let expectedGolangciLintVersion
  let expectedGofumptVersion
  if (shouldExpectGoTools({ templateRef, expectGoTools: env.E2B_EXPECT_GO_TOOLCHAIN })) {
    expectedGoVersion = readGo(repoRoot)
    const dockerfileSource = readDocker(repoRoot)
    expectedGolangciLintVersion = parseGolangciLintVersion(dockerfileSource)
    expectedGofumptVersion = parseGofumptVersion(dockerfileSource)
  }
  return runMainFn({
    Sandbox,
    templateRef,
    expectedVersion: env.DONMAI_VERSION,
    expectedPiVersion: readPi(repoRoot),
    apiKey: env.E2B_API_KEY,
    expectedGoVersion,
    expectedGolangciLintVersion,
    expectedGofumptVersion,
    writeError,
  })
}

module.exports = {
  hasGoTemplate,
  assertGofumptVersionOutput,
  assertGolangciLintVersionOutput,
  assertGoVersionOutput,
  assertPiVersionOutput,
  assertVersionOutput,
  formatFailure,
  hasCToolchainForRace,
  hasE2bCompatibleGoPath,
  isGoTemplateRef,
  main,
  parseAptInstalledPackages,
  parseE2bPathEntries,
  parseE2bPathRaw,
  parseGofumptVersion,
  parseGolangciLintVersion,
  parseGoVersion,
  parsePinnedPiVersion,
  readGoDockerfileSource,
  readGoVersion,
  readPinnedPiVersion,
  resolveRepoRoot,
  runMain,
  shouldExpectGoTools,
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
