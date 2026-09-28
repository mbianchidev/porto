const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

function parseVersions(contents) {
  const versions = new Map()
  for (const line of contents.split(/\r?\n/)) {
    const separator = line.indexOf(' ')
    if (separator <= 0) continue
    versions.set(line.slice(0, separator), line.slice(separator + 1).trim())
  }
  return versions
}

function toolchainLayout(runtimeRoot, platform = process.platform) {
  const suffix = platform === 'win32' ? '.exe' : ''
  return {
    launcher: path.join(runtimeRoot, 'bin', `docker${suffix}`),
    compose: path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-compose${suffix}`),
    buildx: path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-buildx${suffix}`),
  }
}

function validateToolchainLayout(runtimeRoot, {
  platform = process.platform,
  accessImpl = fs.accessSync,
  existsImpl = fs.existsSync,
  readFileImpl = fs.readFileSync,
} = {}) {
  const versionsPath = path.join(runtimeRoot, 'VERSIONS')
  const versions = parseVersions(readFileImpl(versionsPath, 'utf8'))
  const dockerVersion = versions.get('docker') || ''
  const supported = !dockerVersion.startsWith('not available')
  const layout = toolchainLayout(runtimeRoot, platform)
  if (!supported) {
    for (const file of Object.values(layout)) {
      if (existsImpl(file)) {
        throw new Error(`Unsupported Docker toolchain unexpectedly contains ${file}`)
      }
    }
    return { supported: false, versions, layout, reason: dockerVersion }
  }
  const mode = platform === 'win32' ? fs.constants.F_OK : fs.constants.F_OK | fs.constants.X_OK
  for (const [name, file] of Object.entries(layout)) {
    try {
      accessImpl(file, mode)
    } catch (error) {
      throw new Error(`Bundled Docker toolchain is missing executable ${name} at ${file}: ${error.message}`)
    }
  }
  for (const license of [
    'docker-cli.txt',
    'docker-cli-NOTICE.txt',
    'docker-cli-bundled-plugins.patch',
    'docker-compose.txt',
    'docker-compose-NOTICE.txt',
    'docker-buildx.txt',
    'docker-buildx-AUTHORS.txt',
  ]) {
    const file = path.join(runtimeRoot, 'licenses', license)
    if (!existsImpl(file)) throw new Error(`Bundled Docker toolchain is missing ${file}`)
  }
  return { supported: true, versions, layout }
}

function versionToken(value) {
  return value.split(/[ (]/, 1)[0].replace(/^v/, '')
}

function createStalePlugin(file, platform) {
  fs.mkdirSync(path.dirname(file), { recursive: true })
  if (platform === 'win32') {
    fs.writeFileSync(file, 'stale plugin must not execute')
    return
  }
  fs.writeFileSync(file, '#!/bin/sh\necho STALE-PLUGIN-MUST-NOT-RUN >&2\nexit 42\n', { mode: 0o700 })
}

function execute(file, args, options) {
  const result = spawnSync(file, args, {
    ...options,
    encoding: 'utf8',
    timeout: 30000,
    windowsHide: true,
  })
  if (result.error) throw result.error
  if (result.status !== 0) {
    throw new Error(`${file} ${args.join(' ')} exited with ${result.status}: ${(result.stderr || result.stdout).trim()}`)
  }
  const output = `${result.stdout || ''}\n${result.stderr || ''}`.trim()
  if (output.includes('STALE-PLUGIN-MUST-NOT-RUN')) {
    throw new Error('Docker selected a stale user plugin before the bundled plugin')
  }
  return output
}

function smoke(runtimeRoot, options = {}) {
  const platform = options.platform || process.platform
  const inspection = validateToolchainLayout(runtimeRoot, { ...options, platform })
  if (!inspection.supported) {
    process.stdout.write(`Docker toolchain unsupported: ${inspection.reason}\n`)
    return inspection
  }
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-docker-toolchain-'))
  try {
    const configDirectory = path.join(temporary, 'Docker 配置 with spaces')
    const staleExtra = path.join(temporary, 'stale extra plugins')
    const staleUser = path.join(configDirectory, 'cli-plugins')
    const suffix = platform === 'win32' ? '.exe' : ''
    createStalePlugin(path.join(staleExtra, `docker-compose${suffix}`), platform)
    createStalePlugin(path.join(staleExtra, `docker-buildx${suffix}`), platform)
    createStalePlugin(path.join(staleUser, `docker-compose${suffix}`), platform)
    createStalePlugin(path.join(staleUser, `docker-buildx${suffix}`), platform)
    const config = `${JSON.stringify({
      currentContext: 'porto',
      cliPluginsExtraDirs: [staleExtra],
    }, null, 2)}\n`
    fs.mkdirSync(configDirectory, { recursive: true })
    const configPath = path.join(configDirectory, 'config.json')
    fs.writeFileSync(configPath, config, { mode: 0o600 })
    const environment = {
      ...process.env,
      DOCKER_CONFIG: configDirectory,
      HOME: temporary,
      USERPROFILE: temporary,
      PATH: path.dirname(inspection.layout.launcher),
    }
    if (process.env.SystemRoot) environment.SystemRoot = process.env.SystemRoot
    const dockerOutput = execute(inspection.layout.launcher, ['--version'], { env: environment })
    const composeOutput = execute(inspection.layout.launcher, ['compose', 'version', '--short'], { env: environment })
    const buildxOutput = execute(inspection.layout.launcher, ['buildx', 'version'], { env: environment })
    for (const [name, output, expected] of [
      ['docker', dockerOutput, versionToken(inspection.versions.get('docker'))],
      ['compose', composeOutput, versionToken(inspection.versions.get('docker-compose'))],
      ['buildx', buildxOutput, versionToken(inspection.versions.get('docker-buildx'))],
    ]) {
      if (!output.includes(expected)) {
        throw new Error(`${name} reported ${output}; expected version ${expected}`)
      }
    }
    if (fs.readFileSync(configPath, 'utf8') !== config) {
      throw new Error('Bundled Docker launcher modified the user Docker config')
    }
    process.stdout.write(`Docker ${dockerOutput}; Compose ${composeOutput}; Buildx ${buildxOutput}\n`)
    return inspection
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true })
  }
}

if (require.main === module) {
  const [runtimeRoot] = process.argv.slice(2)
  if (!runtimeRoot) {
    console.error('usage: docker-toolchain-smoke.cjs <runtime-directory>')
    process.exitCode = 2
  } else {
    try {
      smoke(path.resolve(runtimeRoot))
    } catch (error) {
      console.error(error.message)
      process.exitCode = 1
    }
  }
}

module.exports = {
  parseVersions,
  smoke,
  toolchainLayout,
  validateToolchainLayout,
}
