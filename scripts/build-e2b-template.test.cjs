'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')

const {
  buildTemplate,
  main,
  parseCpuCount,
  parseDockerfileName,
  parseMemoryMB,
} = require('./build-e2b-template.cjs')

function makeSdk(calls) {
  function Template(options) {
    calls.push({ fileContextPath: options && options.fileContextPath })
    return {
      fromDockerfile(dockerfile) {
        calls.push({ dockerfile })
        return {
          setStartCmd(start, ready) {
            calls.push({ start, ready })
            return { built: true }
          },
        }
      },
    }
  }
  Template.build = async (template, target, options) => {
    calls.push({ template, target, options })
    return { templateId: 'tpl_123', buildId: 'build_456' }
  }
  return { Template, defaultBuildLogger: () => 'logger' }
}

function buildCallFor(calls) {
  const call = calls.find((entry) => entry.target)
  assert.ok(call, 'expected a Template.build call')
  return call
}

function dockerfileFor(calls) {
  const entry = calls.find((candidate) => typeof candidate.dockerfile === 'string')
  assert.ok(entry, 'expected a fromDockerfile call')
  return entry.dockerfile
}

function makeTempContext() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'build-e2b-context-'))
  fs.writeFileSync(path.join(dir, 'e2b.Dockerfile'), 'FROM debian:bookworm-slim # base\n')
  fs.writeFileSync(
    path.join(dir, 'e2b.go.Dockerfile'),
    'FROM debian:bookworm-slim # go\nRUN apt-get install gcc libc6-dev\n'
  )
  return dir
}

function nullStdout() {
  return { write: () => true }
}

test('buildTemplate forwards cpuCount and memoryMB to Template.build', async () => {
  const calls = []
  const sdk = makeSdk(calls)

  await buildTemplate({
    sdk,
    dockerfile: 'FROM debian:bookworm-slim\n',
    fileContextPath: '/tmp/context',
    templateRef: 'donmai-worker-go:v1.2.3',
    additionalTags: ['default'],
    apiKey: 'test-key',
    cpuCount: 4,
    memoryMB: 8192,
  })

  const call = buildCallFor(calls)
  assert.equal(call.target, 'donmai-worker-go:v1.2.3')
  assert.deepEqual(call.options.tags, ['default'])
  assert.equal(call.options.cpuCount, 4)
  assert.equal(call.options.memoryMB, 8192)
})

test('buildTemplate omits cpuCount and memoryMB when unset', async () => {
  const calls = []
  const sdk = makeSdk(calls)

  await buildTemplate({
    sdk,
    dockerfile: 'FROM debian:bookworm-slim\n',
    fileContextPath: '/tmp/context',
    templateRef: 'donmai-worker:v1.2.3',
    additionalTags: [],
    apiKey: 'test-key',
    cpuCount: undefined,
    memoryMB: undefined,
  })

  const call = buildCallFor(calls)
  assert.equal(call.target, 'donmai-worker:v1.2.3')
  assert.deepEqual(call.options.tags, [])
  assert.ok(!('cpuCount' in call.options), 'cpuCount must be absent for the base template')
  assert.ok(!('memoryMB' in call.options), 'memoryMB must be absent for the base template')
})

test('main() wires E2B_CPU_COUNT/E2B_MEMORY_MB and E2B_DOCKERFILE through production reads', async () => {
  const contextDir = makeTempContext()
  const outputFile = path.join(contextDir, 'github-output')
  fs.writeFileSync(outputFile, '')
  const calls = []
  try {
    await main({
      env: {
        E2B_TEMPLATE_REF: 'donmai-worker-go:v1.2.3',
        E2B_API_KEY: 'test-key',
        E2B_ADDITIONAL_TAGS: 'default',
        E2B_DOCKERFILE: 'e2b.go.Dockerfile',
        E2B_CPU_COUNT: '4',
        E2B_MEMORY_MB: '8192',
        GITHUB_OUTPUT: outputFile,
      },
      cwd: contextDir,
      loadSdk: () => makeSdk(calls),
      stdout: nullStdout(),
    })

    const call = buildCallFor(calls)
    assert.equal(call.target, 'donmai-worker-go:v1.2.3')
    assert.equal(call.options.cpuCount, 4)
    assert.equal(call.options.memoryMB, 8192)
    assert.match(dockerfileFor(calls), /# go/)
    const contextEntry = calls.find((entry) => entry.fileContextPath)
    assert.equal(contextEntry.fileContextPath, contextDir)
  } finally {
    fs.rmSync(contextDir, { recursive: true, force: true })
  }
})

test('main() omits resources and selects the base Dockerfile when env is unset', async () => {
  const contextDir = makeTempContext()
  const outputFile = path.join(contextDir, 'github-output')
  fs.writeFileSync(outputFile, '')
  const calls = []
  try {
    await main({
      env: {
        E2B_TEMPLATE_REF: 'donmai-worker:v1.2.3',
        E2B_API_KEY: 'test-key',
        E2B_ADDITIONAL_TAGS: '',
        GITHUB_OUTPUT: outputFile,
      },
      cwd: contextDir,
      loadSdk: () => makeSdk(calls),
      stdout: nullStdout(),
    })

    const call = buildCallFor(calls)
    assert.equal(call.target, 'donmai-worker:v1.2.3')
    assert.ok(!('cpuCount' in call.options), 'cpuCount must be absent without E2B_CPU_COUNT')
    assert.ok(!('memoryMB' in call.options), 'memoryMB must be absent without E2B_MEMORY_MB')
    assert.match(dockerfileFor(calls), /# base/)
  } finally {
    fs.rmSync(contextDir, { recursive: true, force: true })
  }
})

function makeFakeSdkRoot() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'build-e2b-fakesdk-'))
  const pkgDir = path.join(root, 'node_modules', 'e2b')
  fs.mkdirSync(pkgDir, { recursive: true })
  fs.writeFileSync(
    path.join(pkgDir, 'package.json'),
    JSON.stringify({ name: 'e2b', version: '0.0.0-fake', main: 'index.js' })
  )
  fs.writeFileSync(
    path.join(pkgDir, 'index.js'),
    [
      "'use strict'",
      "const fs = require('node:fs')",
      'let lastFrom = null',
      'function Template(options) {',
      '  return {',
      '    fromDockerfile(dockerfile) {',
      '      lastFrom = { fileContextPath: options && options.fileContextPath, dockerfile }',
      '      return { setStartCmd() { return { __from: lastFrom } } }',
      '    },',
      '  }',
      '}',
      'Template.build = async (template, target, options) => {',
      '  const from = (template && template.__from) || lastFrom || {}',
      '  const capture = process.env.FAKE_SDK_CAPTURE',
      '  const payload = {',
      '    target,',
      '    hasCpu: Object.prototype.hasOwnProperty.call(options, \"cpuCount\"),',
      '    hasMemory: Object.prototype.hasOwnProperty.call(options, \"memoryMB\"),',
      '    cpuCount: options.cpuCount,',
      '    memoryMB: options.memoryMB,',
      '    dockerfile: from.dockerfile || \"\",',
      '    fileContextPath: from.fileContextPath || \"\",',
      '  }',
      '  if (capture) fs.writeFileSync(capture, JSON.stringify(payload))',
      "  return { templateId: 'tpl_subprocess', buildId: 'build_subprocess' }",
      '}',
      "module.exports = { Template, defaultBuildLogger: () => 'logger' }",
      '',
    ].join('\n')
  )
  return root
}

function runBuildSubprocess({ contextDir, outputFile, captureFile, sdkRoot, extraEnv }) {
  const scriptPath = path.join(__dirname, 'build-e2b-template.cjs')
  const nodePath = [path.join(sdkRoot, 'node_modules'), process.env.NODE_PATH || '']
    .filter(Boolean)
    .join(path.delimiter)
  return spawnSync(process.execPath, [scriptPath], {
    cwd: contextDir,
    env: {
      ...process.env,
      NODE_PATH: nodePath,
      GITHUB_OUTPUT: outputFile,
      FAKE_SDK_CAPTURE: captureFile,
      ...extraEnv,
    },
    encoding: 'utf8',
  })
}

test('subprocess entry wires E2B_CPU_COUNT/E2B_MEMORY_MB and the Go Dockerfile to Template.build', () => {
  const contextDir = makeTempContext()
  const sdkRoot = makeFakeSdkRoot()
  const outputFile = path.join(contextDir, 'github-output')
  const captureFile = path.join(contextDir, 'capture.json')
  fs.writeFileSync(outputFile, '')
  try {
    const child = runBuildSubprocess({
      contextDir,
      outputFile,
      captureFile,
      sdkRoot,
      extraEnv: {
        E2B_TEMPLATE_REF: 'donmai-worker-go:v1.2.3',
        E2B_API_KEY: 'test-key',
        E2B_ADDITIONAL_TAGS: 'default',
        E2B_DOCKERFILE: 'e2b.go.Dockerfile',
        E2B_CPU_COUNT: '4',
        E2B_MEMORY_MB: '8192',
      },
    })
    assert.equal(child.status, 0, `subprocess failed: ${child.stderr}`)
    const payload = JSON.parse(fs.readFileSync(captureFile, 'utf8'))
    assert.equal(payload.target, 'donmai-worker-go:v1.2.3')
    assert.equal(payload.hasCpu, true)
    assert.equal(payload.hasMemory, true)
    assert.equal(payload.cpuCount, 4)
    assert.equal(payload.memoryMB, 8192)
    assert.match(payload.dockerfile, /# go/)
  } finally {
    fs.rmSync(contextDir, { recursive: true, force: true })
    fs.rmSync(sdkRoot, { recursive: true, force: true })
  }
})

test('subprocess entry omits resources and selects the base Dockerfile when env is unset', () => {
  const contextDir = makeTempContext()
  const sdkRoot = makeFakeSdkRoot()
  const outputFile = path.join(contextDir, 'github-output')
  const captureFile = path.join(contextDir, 'capture.json')
  fs.writeFileSync(outputFile, '')
  try {
    const child = runBuildSubprocess({
      contextDir,
      outputFile,
      captureFile,
      sdkRoot,
      extraEnv: {
        E2B_TEMPLATE_REF: 'donmai-worker:v1.2.3',
        E2B_API_KEY: 'test-key',
        E2B_ADDITIONAL_TAGS: '',
      },
    })
    assert.equal(child.status, 0, `subprocess failed: ${child.stderr}`)
    const payload = JSON.parse(fs.readFileSync(captureFile, 'utf8'))
    assert.equal(payload.target, 'donmai-worker:v1.2.3')
    assert.equal(payload.hasCpu, false)
    assert.equal(payload.hasMemory, false)
    assert.match(payload.dockerfile, /# base/)
  } finally {
    fs.rmSync(contextDir, { recursive: true, force: true })
    fs.rmSync(sdkRoot, { recursive: true, force: true })
  }
})

test('parseCpuCount and parseMemoryMB accept integers and reject garbage', () => {
  assert.equal(parseCpuCount(undefined), undefined)
  assert.equal(parseCpuCount(''), undefined)
  assert.equal(parseCpuCount('4'), 4)
  assert.equal(parseMemoryMB(undefined), undefined)
  assert.equal(parseMemoryMB(''), undefined)
  assert.equal(parseMemoryMB('8192'), 8192)
  assert.throws(() => parseCpuCount('zero'), /E2B_CPU_COUNT/)
  assert.throws(() => parseCpuCount('0'), /E2B_CPU_COUNT/)
  assert.throws(() => parseMemoryMB('1.5'), /E2B_MEMORY_MB/)
  assert.throws(() => parseMemoryMB('-1'), /E2B_MEMORY_MB/)
})

test('parseDockerfileName defaults to the base template and rejects traversal', () => {
  assert.equal(parseDockerfileName(undefined), 'e2b.Dockerfile')
  assert.equal(parseDockerfileName(''), 'e2b.Dockerfile')
  assert.equal(parseDockerfileName('e2b.go.Dockerfile'), 'e2b.go.Dockerfile')
  assert.throws(() => parseDockerfileName('../e2b.Dockerfile'), /E2B_DOCKERFILE/)
  assert.throws(() => parseDockerfileName('a/b.Dockerfile'), /E2B_DOCKERFILE/)
})
