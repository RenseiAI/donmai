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

async function main() {
  const templateRef = process.env.E2B_TEMPLATE_REF
  const apiKey = process.env.E2B_API_KEY
  const outputFile = process.env.GITHUB_OUTPUT
  const additionalTags = parseAdditionalTags(
    process.env.E2B_ADDITIONAL_TAGS
  )
  const dockerfileName = parseDockerfileName(process.env.E2B_DOCKERFILE)
  const cpuCount = parseCpuCount(process.env.E2B_CPU_COUNT)
  const memoryMB = parseMemoryMB(process.env.E2B_MEMORY_MB)
  const fileContextPath = process.cwd()
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

  const dockerfile = fs.readFileSync(dockerfilePath, 'utf8')
  const sdk = require('e2b')
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

  fs.appendFileSync(
    outputFile,
    [
      `template_id=${buildInfo.templateId}`,
      `build_id=${buildInfo.buildId}`,
      `template_ref=${templateRef}`,
      '',
    ].join('\n')
  )

  process.stdout.write(
    `Built ${templateRef} as template ${buildInfo.templateId}, build ${buildInfo.buildId}\n`
  )
  if (additionalTags.includes('default')) {
    process.stdout.write('Advanced the rolling default tag to the same build.\n')
  } else {
    process.stdout.write('Left the rolling default tag unchanged.\n')
  }
}

module.exports = { buildTemplate, parseAdditionalTags, parseCpuCount, parseDockerfileName, parseMemoryMB }

if (require.main === module) {
  main().catch((error) => {
    process.stderr.write(`E2B template build failed: ${error.message}\n`)
    process.exitCode = 1
  })
}
