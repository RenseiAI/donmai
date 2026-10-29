#!/usr/bin/env node
'use strict'

const fs = require('node:fs')
const path = require('node:path')

function parseAdditionalTags(value) {
  if (!value) {
    return []
  }
  return value
    .split(',')
    .map((tag) => tag.trim())
    .filter(Boolean)
}

function parseCpuCount(value) {
  if (value === undefined || value === null || value === '') {
    return undefined
  }
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new Error(
      `E2B_CPU_COUNT must be a positive integer, got ${JSON.stringify(value)}`
    )
  }
  return parsed
}

function parseMemoryMB(value) {
  if (value === undefined || value === null || value === '') {
    return undefined
  }
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new Error(
      `E2B_MEMORY_MB must be a positive integer, got ${JSON.stringify(value)}`
    )
  }
  return parsed
}

function parseDockerfileName(value) {
  if (!value) {
    return 'e2b.Dockerfile'
  }
  if (
    value.includes('/') ||
    value.includes('\\') ||
    value.includes('..') ||
    !value.endsWith('.Dockerfile')
  ) {
    throw new Error(
      `E2B_DOCKERFILE must be a Dockerfile basename, got ${JSON.stringify(value)}`
    )
  }
  return value
}

async function buildTemplate({
  sdk,
  dockerfile,
  fileContextPath,
  templateRef,
  additionalTags,
  apiKey,
  cpuCount,
  memoryMB,
}) {
  const { Template, defaultBuildLogger } = sdk
  const template = Template({ fileContextPath })
    .fromDockerfile(dockerfile)
    .setStartCmd('sleep infinity', 'true')

  const options = {
    apiKey,
    tags: additionalTags,
    onBuildLogs: defaultBuildLogger(),
  }
  if (cpuCount !== undefined) {
    options.cpuCount = cpuCount
  }
  if (memoryMB !== undefined) {
    options.memoryMB = memoryMB
  }

  return Template.build(template, templateRef, options)
}

async function main(overrides = {}) {
  const env = overrides.env || process.env
  const cwd = overrides.cwd || process.cwd()
  const loadSdk = overrides.loadSdk || (() => require('e2b'))
  const readFile = overrides.readFile || ((p, enc) => fs.readFileSync(p, enc))
  const appendFile = overrides.appendFile || ((p, data) => fs.appendFileSync(p, data))
  const stdout = overrides.stdout || process.stdout
  const templateRef = env.E2B_TEMPLATE_REF
  const apiKey = env.E2B_API_KEY
  const outputFile = env.GITHUB_OUTPUT
  const additionalTags = parseAdditionalTags(
    env.E2B_ADDITIONAL_TAGS
  )
  const dockerfileName = parseDockerfileName(env.E2B_DOCKERFILE)
  const cpuCount = parseCpuCount(env.E2B_CPU_COUNT)
  const memoryMB = parseMemoryMB(env.E2B_MEMORY_MB)
  const fileContextPath = cwd
  const dockerfilePath = path.join(fileContextPath, dockerfileName)

  if (!templateRef) {
    throw new Error('E2B_TEMPLATE_REF is required')
  }
  if (!apiKey) {
    throw new Error('E2B_API_KEY is required')
  }
  if (!outputFile) {
    throw new Error('GITHUB_OUTPUT is required')
  }

  const dockerfile = readFile(dockerfilePath, 'utf8')
  const sdk = loadSdk()
  const buildInfo = await buildTemplate({
    sdk,
    dockerfile,
    fileContextPath,
    templateRef,
    additionalTags,
    apiKey,
    cpuCount,
    memoryMB,
  })

  appendFile(
    outputFile,
    [
      `template_id=${buildInfo.templateId}`,
      `build_id=${buildInfo.buildId}`,
      `template_ref=${templateRef}`,
      '',
    ].join('\n')
  )

  stdout.write(
    `Built ${templateRef} as template ${buildInfo.templateId}, build ${buildInfo.buildId}\n`
  )
  if (additionalTags.includes('default')) {
    stdout.write('Advanced the rolling default tag to the same build.\n')
  } else {
    stdout.write('Left the rolling default tag unchanged.\n')
  }
  return buildInfo
}

module.exports = { buildTemplate, main, parseAdditionalTags, parseCpuCount, parseDockerfileName, parseMemoryMB }

if (require.main === module) {
  main().catch((error) => {
    process.stderr.write(`E2B template build failed: ${error.message}\n`)
    process.exitCode = 1
  })
}
