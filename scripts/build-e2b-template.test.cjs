'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')

const {
  buildTemplate,
  parseCpuCount,
  parseDockerfileName,
  parseMemoryMB,
} = require('./build-e2b-template.cjs')

function makeSdk(calls) {
  function Template() {
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

test('env E2B_CPU_COUNT/E2B_MEMORY_MB reach Template.build, unset stays absent', async () => {
  const savedCpu = process.env.E2B_CPU_COUNT
  const savedMemory = process.env.E2B_MEMORY_MB
  try {
    process.env.E2B_CPU_COUNT = '4'
    process.env.E2B_MEMORY_MB = '8192'

    const withEnvCalls = []
    await buildTemplate({
      sdk: makeSdk(withEnvCalls),
      dockerfile: 'FROM debian:bookworm-slim\n',
      fileContextPath: '/tmp/context',
      templateRef: 'donmai-worker-go:v1.2.3',
      additionalTags: ['default'],
      apiKey: 'test-key',
      cpuCount: parseCpuCount(process.env.E2B_CPU_COUNT),
      memoryMB: parseMemoryMB(process.env.E2B_MEMORY_MB),
    })
    const withEnv = buildCallFor(withEnvCalls)
    assert.equal(withEnv.options.cpuCount, 4)
    assert.equal(withEnv.options.memoryMB, 8192)

    delete process.env.E2B_CPU_COUNT
    delete process.env.E2B_MEMORY_MB

    const withoutEnvCalls = []
    await buildTemplate({
      sdk: makeSdk(withoutEnvCalls),
      dockerfile: 'FROM debian:bookworm-slim\n',
      fileContextPath: '/tmp/context',
      templateRef: 'donmai-worker:v1.2.3',
      additionalTags: [],
      apiKey: 'test-key',
      cpuCount: parseCpuCount(process.env.E2B_CPU_COUNT),
      memoryMB: parseMemoryMB(process.env.E2B_MEMORY_MB),
    })
    const withoutEnv = buildCallFor(withoutEnvCalls)
    assert.ok(!('cpuCount' in withoutEnv.options))
    assert.ok(!('memoryMB' in withoutEnv.options))
  } finally {
    if (savedCpu === undefined) {
      delete process.env.E2B_CPU_COUNT
    } else {
      process.env.E2B_CPU_COUNT = savedCpu
    }
    if (savedMemory === undefined) {
      delete process.env.E2B_MEMORY_MB
    } else {
      process.env.E2B_MEMORY_MB = savedMemory
    }
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
