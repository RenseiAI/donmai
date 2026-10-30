'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

const {
  assertGofumptVersionOutput,
  assertGolangciLintVersionOutput,
  assertGoVersionOutput,
  assertPiVersionOutput,
  assertVersionOutput,
  hasCToolchainForRace,
  hasE2bCompatibleGoPath,
  hasGoTemplate,
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
} = require('./verify-e2b-template.cjs')

function testRepoRoot() {
  return resolveRepoRoot({ env: process.env, cwd: process.cwd(), scriptDir: __dirname })
}

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

test('parseGoVersion extracts the go directive', () => {
  assert.equal(parseGoVersion('module example.com/x\n\ngo 1.26.10\n'), '1.26.10')
})

test('parseGoVersion rejects a go.mod with no go directive', () => {
  assert.throws(() => parseGoVersion('module example.com/x\n'), /go directive/)
})

test('parseGolangciLintVersion extracts the go install pin', () => {
  const source = [
    'RUN go version \\',
    '    && GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 \\',
    '    && GOBIN=/usr/local/bin go install mvdan.cc/gofumpt@v0.12.0',
  ].join('\n')
  assert.equal(parseGolangciLintVersion(source), '2.11.4')
})

test('parseGofumptVersion extracts the go install pin', () => {
  const source = 'RUN GOBIN=/usr/local/bin go install mvdan.cc/gofumpt@v0.12.0\n'
  assert.equal(parseGofumptVersion(source), '0.12.0')
})

test('readGoVersion reads the floor from go.mod under a repo root', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-go-version-'))
  fs.writeFileSync(path.join(repoRoot, 'go.mod'), 'module example.com/x\n\ngo 1.26.10\n')
  try {
    assert.equal(readGoVersion(repoRoot), '1.26.10')
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

test('assertGoVersionOutput accepts the pinned toolchain', () => {
  assert.doesNotThrow(() =>
    assertGoVersionOutput({ exitCode: 0, stdout: 'go version go1.26.10 linux/amd64\n' }, '1.26.10')
  )
})

test('assertGoVersionOutput rejects a wrong toolchain', () => {
  assert.throws(
    () => assertGoVersionOutput({ exitCode: 0, stdout: 'go version go1.25.0 linux/amd64\n' }, '1.26.10'),
    /want "1\.26\.10"/
  )
})

test('assertGoVersionOutput rejects a longer patch sharing the expected prefix', () => {
  assert.throws(
    () => assertGoVersionOutput({ exitCode: 0, stdout: 'go version go1.26.100 linux/amd64\n' }, '1.26.10'),
    /parsed version "1\.26\.100"/
  )
})

test('assertGolangciLintVersionOutput rejects a longer patch sharing the expected prefix', () => {
  assert.throws(
    () =>
      assertGolangciLintVersionOutput(
        { exitCode: 0, stdout: 'golangci-lint has version 2.11.40 built with go1.27.3\n' },
        '2.11.4'
      ),
    /parsed version "2\.11\.40"/
  )
})

test('assertGofumptVersionOutput rejects a longer patch sharing the expected prefix', () => {
  assert.throws(
    () => assertGofumptVersionOutput({ exitCode: 0, stdout: 'v0.12.00 (go1.27.3)\n' }, '0.12.0'),
    /parsed version "0\.12\.00"/
  )
})

test('assertGolangciLintVersionOutput accepts the pinned version', () => {
  assert.doesNotThrow(() =>
    assertGolangciLintVersionOutput(
      { exitCode: 0, stdout: 'golangci-lint has version 2.11.4 built with go1.27.3\n' },
      '2.11.4'
    )
  )
})

test('assertGofumptVersionOutput accepts the pinned version', () => {
  assert.doesNotThrow(() =>
    assertGofumptVersionOutput({ exitCode: 0, stdout: 'v0.12.0 (go1.27.3)\n' }, '0.12.0')
  )
})

test('e2b.go.Dockerfile keeps a single FROM for the E2B build SDK', () => {
  const repoRoot = testRepoRoot()
  const dockerfile = fs.readFileSync(
    path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
    'utf8'
  )
  const fromLines = dockerfile.match(/^FROM\b/gm) || []
  assert.equal(fromLines.length, 1)
  assert.doesNotMatch(dockerfile, /^COPY\s+--from=/mi)
})

test('e2b.go.Dockerfile installs the C toolchain for go test -race', () => {
  const repoRoot = testRepoRoot()
  const dockerfile = fs.readFileSync(
    path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
    'utf8'
  )
  const installed = parseAptInstalledPackages(dockerfile)
  assert.ok(installed.includes('gcc'), `apt installs must include gcc, got ${JSON.stringify(installed)}`)
  assert.ok(
    installed.includes('libc6-dev') || installed.includes('build-essential'),
    `apt installs must include libc6-dev or build-essential, got ${JSON.stringify(installed)}`
  )
  assert.equal(hasCToolchainForRace(dockerfile), true)
})

test('C-toolchain assertion ignores Dockerfile comments', () => {
  const source = [
    '# gcc libc6-dev are mentioned here but install nothing',
    '# RUN apt-get install -y gcc libc6-dev',
    'FROM debian:bookworm-slim',
    'RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git curl \\',
    '    && rm -rf /var/lib/apt/lists/*',
  ].join('\n')
  assert.deepEqual(parseAptInstalledPackages(source), ['ca-certificates', 'git', 'curl'])
  assert.equal(hasCToolchainForRace(source), false)
})

test('e2b.go.Dockerfile sets an E2B-compatible Go PATH', () => {
  const repoRoot = testRepoRoot()
  const dockerfile = fs.readFileSync(
    path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
    'utf8'
  )
  assert.equal(hasE2bCompatibleGoPath(dockerfile), true)
  const entries = parseE2bPathEntries(dockerfile)
  assert.ok(entries, 'expected ENV PATH entries')
  assert.equal(entries[0], '/usr/local/go/bin')
  assert.doesNotMatch(parseE2bPathRaw(dockerfile), /^["'].*/)
})

test('E2B PATH evaluation retains quotes where an ordinary parser would strip them', () => {
  const quoted = [
    'FROM debian:bookworm-slim',
    'ENV PATH="/usr/local/go/bin:/usr/local/bin:${PATH}"',
    'RUN go version',
  ].join('\n')
  assert.equal(hasE2bCompatibleGoPath(quoted), false)
  assert.equal(parseE2bPathEntries(quoted)[0], '"/usr/local/go/bin')
  const fixed = [
    'FROM debian:bookworm-slim',
    'ENV PATH=/usr/local/go/bin:/usr/local/bin:${PATH}',
    'RUN go version',
  ].join('\n')
  assert.equal(hasE2bCompatibleGoPath(fixed), true)
  assert.equal(parseE2bPathEntries(fixed)[0], '/usr/local/go/bin')
})

test('E2B Go PATH check fails when the ENV is deleted', () => {
  const noEnv = [
    'FROM debian:bookworm-slim',
    'RUN /usr/local/go/bin/go version',
  ].join('\n')
  assert.equal(parseE2bPathRaw(noEnv), null)
  assert.equal(parseE2bPathEntries(noEnv), null)
  assert.equal(hasE2bCompatibleGoPath(noEnv), false)
})

test('e2b.go.Dockerfile Go tarball tracks the go.mod floor', () => {
  const repoRoot = testRepoRoot()
  const goVersion = readGoVersion(repoRoot)
  const dockerfile = fs.readFileSync(
    path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
    'utf8'
  )
  assert.ok(
    dockerfile.includes(`go${goVersion}.linux-amd64.tar.gz`),
    `Go Dockerfile must install go${goVersion} from the official tarball`
  )
})

test('isGoTemplateRef matches only the Go worker template', () => {
  assert.equal(isGoTemplateRef('donmai-worker-go:v1.2.3'), true)
  assert.equal(isGoTemplateRef('donmai-worker:v1.2.3'), false)
  assert.equal(isGoTemplateRef(''), false)
})

test('shouldExpectGoTools follows the template name or the explicit flag', () => {
  assert.equal(shouldExpectGoTools({ templateRef: 'donmai-worker-go:v1.2.3' }), true)
  assert.equal(shouldExpectGoTools({ templateRef: 'donmai-worker:v1.2.3' }), false)
  assert.equal(
    shouldExpectGoTools({ templateRef: 'donmai-worker:v1.2.3', expectGoTools: '1' }),
    true
  )
})

function makeGoSandbox() {
  const calls = []
  const outputs = {
    'donmai --version': { exitCode: 0, stdout: 'donmai version v0.68.5\n' },
    'pi --version': { exitCode: 0, stdout: 'pi 0.80.10\n' },
    'go version': { exitCode: 0, stdout: 'go version go1.26.10 linux/amd64\n' },
    'golangci-lint --version': {
      exitCode: 0,
      stdout: 'golangci-lint has version 2.11.4 built with go1.27.3\n',
    },
    'gofumpt --version': { exitCode: 0, stdout: 'v0.12.0 (go1.27.3)\n' },
  }
  const Sandbox = {
    async create(templateRef, options) {
      calls.push(['create', templateRef, options])
      return {
        commands: {
          async run(command) {
            calls.push(['run', command])
            if (outputs[command]) {
              return outputs[command]
            }
            throw new Error(`unexpected command: ${command}`)
          },
        },
        async kill() {
          calls.push(['kill'])
        },
      }
    },
  }
  return { Sandbox, calls }
}

test('verifyTemplateVersion checks the Go toolchain inside the Go template', async () => {
  const { Sandbox, calls } = makeGoSandbox()

  await verifyTemplateVersion({
    Sandbox,
    templateRef: 'donmai-worker-go:v0.68.5',
    expectedVersion: 'v0.68.5',
    expectedPiVersion: '0.80.10',
    apiKey: 'test-key',
    expectedGoVersion: '1.26.10',
    expectedGolangciLintVersion: '2.11.4',
    expectedGofumptVersion: '0.12.0',
  })

  assert.deepEqual(calls, [
    ['create', 'donmai-worker-go:v0.68.5', { apiKey: 'test-key', timeoutMs: 60_000 }],
    ['run', 'donmai --version'],
    ['run', 'pi --version'],
    ['run', 'go version'],
    ['run', 'golangci-lint --version'],
    ['run', 'gofumpt --version'],
    ['kill'],
  ])
})

test('verifyTemplateVersion rejects a Go toolchain mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeGoSandbox()
  // Make `go version` report the wrong floor by wrapping the sandbox.
  const originalCreate = Sandbox.create
  Sandbox.create = async (templateRef, options) => {
    const sandbox = await originalCreate(templateRef, options)
    const originalRun = sandbox.commands.run
    sandbox.commands.run = async (command) => {
      if (command === 'go version') {
        return { exitCode: 0, stdout: 'go version go1.25.0 linux/amd64\n' }
      }
      return originalRun(command)
    }
    return sandbox
  }

  await assert.rejects(
    () =>
      verifyTemplateVersion({
        Sandbox,
        templateRef: 'donmai-worker-go:v0.68.5',
        expectedVersion: 'v0.68.5',
        expectedPiVersion: '0.80.10',
        apiKey: 'test-key',
        expectedGoVersion: '1.26.10',
        expectedGolangciLintVersion: '2.11.4',
        expectedGofumptVersion: '0.12.0',
      }),
    /want "1\.26\.10"/
  )
  assert.ok(calls.includes('kill') || calls.some((entry) => entry[0] === 'kill'))
})

test('verifyTemplateVersion rejects a golangci-lint mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeGoSandbox()
  const originalCreate = Sandbox.create
  Sandbox.create = async (templateRef, options) => {
    const sandbox = await originalCreate(templateRef, options)
    const originalRun = sandbox.commands.run
    sandbox.commands.run = async (command) => {
      if (command === 'golangci-lint --version') {
        return { exitCode: 0, stdout: 'golangci-lint has version 2.11.40 built with go1.27.3\n' }
      }
      return originalRun(command)
    }
    return sandbox
  }

  await assert.rejects(
    () =>
      verifyTemplateVersion({
        Sandbox,
        templateRef: 'donmai-worker-go:v0.68.5',
        expectedVersion: 'v0.68.5',
        expectedPiVersion: '0.80.10',
        apiKey: 'test-key',
        expectedGoVersion: '1.26.10',
        expectedGolangciLintVersion: '2.11.4',
        expectedGofumptVersion: '0.12.0',
      }),
    /parsed version "2\.11\.40"/
  )
  assert.ok(calls.some((entry) => entry[0] === 'kill'))
})

test('verifyTemplateVersion rejects a gofumpt mismatch after cleanup', async () => {
  const { Sandbox, calls } = makeGoSandbox()
  const originalCreate = Sandbox.create
  Sandbox.create = async (templateRef, options) => {
    const sandbox = await originalCreate(templateRef, options)
    const originalRun = sandbox.commands.run
    sandbox.commands.run = async (command) => {
      if (command === 'gofumpt --version') {
        return { exitCode: 0, stdout: 'v0.12.00 (go1.27.3)\n' }
      }
      return originalRun(command)
    }
    return sandbox
  }

  await assert.rejects(
    () =>
      verifyTemplateVersion({
        Sandbox,
        templateRef: 'donmai-worker-go:v0.68.5',
        expectedVersion: 'v0.68.5',
        expectedPiVersion: '0.80.10',
        apiKey: 'test-key',
        expectedGoVersion: '1.26.10',
        expectedGolangciLintVersion: '2.11.4',
        expectedGofumptVersion: '0.12.0',
      }),
    /parsed version "0\.12\.00"/
  )
  assert.ok(calls.some((entry) => entry[0] === 'kill'))
})

test('verifyTemplateVersion refuses incomplete Go toolchain expectations', async () => {
  const { Sandbox } = makeGoSandbox()
  await assert.rejects(
    () =>
      verifyTemplateVersion({
        Sandbox,
        templateRef: 'donmai-worker-go:v0.68.5',
        expectedVersion: 'v0.68.5',
        expectedPiVersion: '0.80.10',
        apiKey: 'test-key',
        expectedGoVersion: '1.26.10',
      }),
    /incomplete/
  )
})

test('resolveRepoRoot finds the checkout from cwd when staged away from the checkout', () => {
  const repoRoot = testRepoRoot()
  const stagedDir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-staged-'))
  try {
    const resolved = resolveRepoRoot({
      env: process.env,
      cwd: repoRoot,
      scriptDir: stagedDir,
    })
    assert.equal(resolved, repoRoot)
    assert.ok(fs.existsSync(path.join(resolved, 'go.mod')))
    assert.ok(fs.existsSync(path.join(resolved, 'worker', 'e2b', 'e2b.go.Dockerfile')))
  } finally {
    fs.rmSync(stagedDir, { recursive: true, force: true })
  }
})

test('resolveRepoRoot honors an explicit checkout root', () => {
  const repoRoot = testRepoRoot()
  const resolved = resolveRepoRoot({
    env: { ...process.env, DONMAI_REPO_ROOT: repoRoot },
    cwd: os.tmpdir(),
    scriptDir: os.tmpdir(),
  })
  assert.equal(resolved, repoRoot)
})

test('main() runs Go toolchain checks for the Go template through production selection', async () => {
  const repoRoot = testRepoRoot()
  const runs = []
  const fakeSandbox = {
    async create(templateRef, options) {
      return {
        commands: {
          async run(command) {
            runs.push(command)
            if (command === 'donmai --version') {
              return { exitCode: 0, stdout: `donmai version v0.68.5\n` }
            }
            if (command === 'pi --version') {
              return { exitCode: 0, stdout: 'pi 0.80.10\n' }
            }
            if (command === 'go version') {
              return { exitCode: 0, stdout: `go version go${readGoVersion(repoRoot)} linux/amd64\n` }
            }
            if (command === 'golangci-lint --version') {
              const source = fs.readFileSync(
                path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
                'utf8'
              )
              const { parseGolangciLintVersion: parseLint } = require('./verify-e2b-template.cjs')
              return {
                exitCode: 0,
                stdout: `golangci-lint has version ${parseLint(source)} built with go1.27.3\n`,
              }
            }
            if (command === 'gofumpt --version') {
              const source = fs.readFileSync(
                path.join(repoRoot, 'worker', 'e2b', 'e2b.go.Dockerfile'),
                'utf8'
              )
              const { parseGofumptVersion: parseFumpt } = require('./verify-e2b-template.cjs')
              return { exitCode: 0, stdout: `v${parseFumpt(source)} (go1.27.3)\n` }
            }
            throw new Error(`unexpected command: ${command}`)
          },
        },
        async kill() {},
      }
    },
  }
  const code = await main({
    env: {
      E2B_TEMPLATE_REF: 'donmai-worker-go:v0.68.5',
      DONMAI_VERSION: 'v0.68.5',
      E2B_API_KEY: 'test-key',
      DONMAI_REPO_ROOT: repoRoot,
    },
    cwd: repoRoot,
    loadSandbox: () => fakeSandbox,
    runMain: async (args) => {
      assert.equal(args.templateRef, 'donmai-worker-go:v0.68.5')
      assert.ok(args.expectedGoVersion, 'Go version must be set for the Go template')
      assert.ok(args.expectedGolangciLintVersion, 'lint version must be set for the Go template')
      assert.ok(args.expectedGofumptVersion, 'gofumpt version must be set for the Go template')
      const { runMain: realRunMain } = require('./verify-e2b-template.cjs')
      return realRunMain({ ...args, writeError: () => {} })
    },
    writeError: () => {},
  })
  assert.equal(code, 0)
  assert.deepEqual(runs, [
    'donmai --version',
    'pi --version',
    'go version',
    'golangci-lint --version',
    'gofumpt --version',
  ])
})

test('main() skips Go toolchain checks for the base template', async () => {
  const repoRoot = testRepoRoot()
  const runs = []
  const fakeSandbox = {
    async create() {
      return {
        commands: {
          async run(command) {
            runs.push(command)
            if (command === 'donmai --version') {
              return { exitCode: 0, stdout: 'donmai version v0.68.5\n' }
            }
            if (command === 'pi --version') {
              return { exitCode: 0, stdout: 'pi 0.80.10\n' }
            }
            throw new Error(`unexpected command: ${command}`)
          },
        },
        async kill() {},
      }
    },
  }
  const code = await main({
    env: {
      E2B_TEMPLATE_REF: 'donmai-worker:v0.68.5',
      DONMAI_VERSION: 'v0.68.5',
      E2B_API_KEY: 'test-key',
      DONMAI_REPO_ROOT: repoRoot,
    },
    cwd: repoRoot,
    loadSandbox: () => fakeSandbox,
    writeError: () => {},
  })
  assert.equal(code, 0)
  assert.deepEqual(runs, ['donmai --version', 'pi --version'])
})

test('historical base-only checkout verifies the base template without Go checks', async () => {
  // Mimics a release checkout that predates worker/e2b/e2b.go.Dockerfile.
  // The production verifier must still verify the base template with only
  // donmai/pi checks, while Go expectations stay gated on the file existing.
  const historicalRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-historical-'))
  try {
    fs.writeFileSync(path.join(historicalRoot, 'go.mod'), 'module example.com/x\n\ngo 1.26.10\n')
    fs.mkdirSync(path.join(historicalRoot, 'worker', 'e2b'), { recursive: true })
    fs.writeFileSync(
      path.join(historicalRoot, 'worker', 'e2b', 'e2b.Dockerfile'),
      'FROM debian:bookworm-slim\n'
    )
    fs.mkdirSync(path.join(historicalRoot, 'provider', 'harness', 'pi'), { recursive: true })
    fs.writeFileSync(
      path.join(historicalRoot, 'provider', 'harness', 'pi', 'probe.go'),
      'package pi\n\nconst PinnedVersion = "0.80.10"\n'
    )
    assert.equal(hasGoTemplate(historicalRoot), false)
    assert.throws(() => readGoDockerfileSource(historicalRoot), /could not find e2b\.go\.Dockerfile/)

    const runs = []
    const fakeSandbox = {
      async create() {
        return {
          commands: {
            async run(command) {
              runs.push(command)
              if (command === 'donmai --version') {
                return { exitCode: 0, stdout: 'donmai version v0.53.0\n' }
              }
              if (command === 'pi --version') {
                return { exitCode: 0, stdout: 'pi 0.80.10\n' }
              }
              throw new Error(`unexpected command: ${command}`)
            },
          },
          async kill() {},
        }
      },
    }
    const code = await main({
      env: {
        E2B_TEMPLATE_REF: 'donmai-worker:v0.53.0',
        DONMAI_VERSION: 'v0.53.0',
        E2B_API_KEY: 'test-key',
        DONMAI_REPO_ROOT: historicalRoot,
      },
      cwd: historicalRoot,
      loadSandbox: () => fakeSandbox,
      writeError: () => {},
    })
    assert.equal(code, 0)
    assert.deepEqual(runs, ['donmai --version', 'pi --version'])
  } finally {
    fs.rmSync(historicalRoot, { recursive: true, force: true })
  }
})

test('hasGoTemplate reports the Go template only when the Go Dockerfile exists', () => {
  const withGo = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-hasgo-'))
  const withoutGo = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-nogo-'))
  try {
    fs.mkdirSync(path.join(withGo, 'worker', 'e2b'), { recursive: true })
    fs.writeFileSync(path.join(withGo, 'worker', 'e2b', 'e2b.go.Dockerfile'), 'FROM x\n')
    fs.mkdirSync(path.join(withoutGo, 'worker', 'e2b'), { recursive: true })
    assert.equal(hasGoTemplate(withGo), true)
    assert.equal(hasGoTemplate(withoutGo), false)
    assert.equal(hasGoTemplate(''), false)
    assert.equal(hasGoTemplate(undefined), false)
  } finally {
    fs.rmSync(withGo, { recursive: true, force: true })
    fs.rmSync(withoutGo, { recursive: true, force: true })
  }
})

function resolveWorkflowPath() {
  const candidates = []
  if (process.env.DONMAI_REPO_ROOT) {
    candidates.push(
      path.join(process.env.DONMAI_REPO_ROOT, '.github', 'workflows', 'e2b-template.yml')
    )
  }
  candidates.push(path.join(process.cwd(), '.github', 'workflows', 'e2b-template.yml'))
  candidates.push(path.join(__dirname, '..', '.github', 'workflows', 'e2b-template.yml'))
  let dir = __dirname
  for (let i = 0; i < 6; i += 1) {
    candidates.push(path.join(dir, '.github', 'workflows', 'e2b-template.yml'))
    dir = path.dirname(dir)
  }
  for (const candidate of candidates) {
    try {
      if (fs.existsSync(candidate)) {
        return candidate
      }
    } catch {
      // try the next candidate
    }
  }
  throw new Error('could not find .github/workflows/e2b-template.yml')
}

function workflowStepBlock(workflow, stepName) {
  const marker = `- name: ${stepName}`
  const start = workflow.indexOf(marker)
  assert.ok(start !== -1, `expected workflow step ${JSON.stringify(stepName)}`)
  const next = workflow.indexOf('\n      - name: ', start + marker.length)
  return next === -1 ? workflow.slice(start) : workflow.slice(start, next)
}

test('release workflow gates Go toolchain verification on the release checkout', () => {
  // Load-bearing guard for the historical base-only retry: the Go toolchain
  // verification only runs when the release contains the Go Dockerfile.
  // Deleting either Go-step condition or reverting this workflow to the
  // base-only version must fail this test.
  const workflow = fs.readFileSync(resolveWorkflowPath(), 'utf8')
  assert.ok(
    workflow.indexOf('Test E2B version assertion') < workflow.indexOf('Checkout release tag'),
    'staged version tests must run before the release checkout so assertions stay paired with their fixtures'
  )
  const detect = workflowStepBlock(workflow, 'Detect Go template in release checkout')
  assert.match(detect, /hasGoDockerfile/, 'detection must call the production hasGoDockerfile helper')
  assert.match(detect, /present=false/, 'detection must name the base-only retry it preserves')
  const goVerify = workflowStepBlock(workflow, 'Assert published E2B Go template toolchain')
  assert.match(
    goVerify,
    /steps\.go-template\.outputs\.present\s*==\s*'true'/,
    'Go toolchain verification must be gated on the release containing the Go Dockerfile'
  )
  const baseVerify = workflowStepBlock(workflow, 'Assert published E2B template binary version')
  assert.doesNotMatch(
    baseVerify,
    /steps\.go-template/,
    'base template verification must stay ungated so historical retries still verify it'
  )
  const conditions = workflow.match(/steps\.go-template\.outputs\.present\s*==\s*'true'/g) || []
  assert.equal(conditions.length, 2, `both Go steps must be gated, found ${conditions.length}`)
  const summary = workflowStepBlock(workflow, 'Summarize E2B template build')
  assert.match(summary, /base-only retry/, 'summary must name the base-only retry it preserves')
  // Execute the production presence helper against both release layouts.
  const withGo = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-workflow-go-'))
  const withoutGo = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-e2b-workflow-base-'))
  try {
    fs.mkdirSync(path.join(withGo, 'worker', 'e2b'), { recursive: true })
    fs.writeFileSync(path.join(withGo, 'worker', 'e2b', 'e2b.go.Dockerfile'), 'FROM x\n')
    fs.mkdirSync(path.join(withoutGo, 'worker', 'e2b'), { recursive: true })
    assert.equal(hasGoTemplate(withGo), true)
    assert.equal(hasGoTemplate(withoutGo), false)
  } finally {
    fs.rmSync(withGo, { recursive: true, force: true })
    fs.rmSync(withoutGo, { recursive: true, force: true })
  }
})
