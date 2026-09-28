const path = require('node:path')
const crypto = require('node:crypto')
const { execFile } = require('node:child_process')
const fs = require('node:fs')
const { setTimeout: delay } = require('node:timers/promises')
const { promisify } = require('node:util')

const DEFAULT_DAEMON_URL = 'http://127.0.0.1:37623'
const DEFAULT_TIMEOUT_MS = 800
const EXPECTED_API_VERSION = 30
const PATH_MARKER = '__PORTO_PATH__'
const execFileAsync = promisify(execFile)

async function inspectDaemon({
  daemonURL = DEFAULT_DAEMON_URL,
  expectedDaemonIdentity = '',
  fetchImpl = globalThis.fetch,
  timeoutMs = DEFAULT_TIMEOUT_MS,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)

  try {
    const response = await fetchImpl(`${daemonURL}/api/health`, { signal: controller.signal })
    if (!response.ok) return { reachable: true, ready: false, health: null }

    const health = await response.json()
    const ready = health.status === 'ok'
      && health.apiVersion === EXPECTED_API_VERSION
      && health.dashboardReady === true
      && (expectedDaemonIdentity === '' || health.daemonIdentity === expectedDaemonIdentity)
    return { reachable: true, ready, health }
  } catch {
    return { reachable: false, ready: false, health: null }
  } finally {
    clearTimeout(timer)
  }
}

async function isDaemonReady(options) {
  return (await inspectDaemon(options)).ready
}

function daemonBinaryIdentity(binary, {
  readFileImpl = fs.readFileSync,
} = {}) {
  return crypto.createHash('sha256').update(readFileImpl(binary)).digest('hex')
}

async function inspectDockerStatus({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = 30000,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetchImpl(`${daemonURL}/api/docker/status`, { signal: controller.signal })
    if (!response.ok) {
      throw new Error(`Porto Docker status returned HTTP ${response.status || 'error'}`)
    }
    return await response.json()
  } finally {
    clearTimeout(timer)
  }
}

async function installDockerEngine({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = 20 * 60 * 1000,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetchImpl(`${daemonURL}/api/docker/engine/install`, {
      method: 'POST',
      signal: controller.signal,
    })
    if (!response.ok) {
      const message = typeof response.text === 'function' ? await response.text() : ''
      throw new Error(message.trim() || `Porto Docker installation returned HTTP ${response.status || 'error'}`)
    }
    return await response.json()
  } finally {
    clearTimeout(timer)
  }
}

async function prepareDockerEngineUpdate({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = 5 * 60 * 1000,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetchImpl(`${daemonURL}/api/docker/engine/prepare-update`, {
      method: 'POST',
      signal: controller.signal,
    })
    if (!response.ok) {
      const message = typeof response.text === 'function' ? await response.text() : ''
      throw new Error(message.trim() || `Porto update preparation returned HTTP ${response.status || 'error'}`)
    }
    return await response.json()
  } finally {
    clearTimeout(timer)
  }
}

async function waitForDockerEngine({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = 120000,
  nowImpl = () => performance.now(),
  delayImpl = delay,
} = {}) {
  const deadline = nowImpl() + timeoutMs
  let message = 'Container inventory is still connecting'
  while (nowImpl() < deadline) {
    const status = await inspectDockerStatus({
      daemonURL,
      fetchImpl,
      timeoutMs: Math.min(30000, deadline - nowImpl()),
    })
    if (status.available || status.enabled === false) return status
    if (status.message) message = status.message
    const remaining = deadline - nowImpl()
    if (remaining > 0) await delayImpl(Math.min(500, remaining))
  }
  throw new Error(`Porto container runtime did not become available within ${timeoutMs / 1000}s: ${message}`)
}

async function installDockerContext({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = 30000,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetchImpl(`${daemonURL}/api/docker/context/install`, {
      method: 'POST',
      signal: controller.signal,
    })
    if (!response.ok) {
      const message = typeof response.text === 'function' ? await response.text() : ''
      throw new Error(message.trim() || `Porto Docker context setup returned HTTP ${response.status || 'error'}`)
    }
    return await response.json()
  } finally {
    clearTimeout(timer)
  }
}

async function resolveLoginShellPath({
  platform = process.platform,
  environment = process.env,
  execFileImpl = execFileAsync,
} = {}) {
  if (platform === 'win32') return ''
  const shell = environment.SHELL || '/bin/sh'
  try {
    const { stdout } = await execFileImpl(
      shell,
      ['-ilc', `printf "\\n${PATH_MARKER}%s\\n" "$PATH"`],
      {
        env: environment,
        timeout: 5000,
        maxBuffer: 1024 * 1024,
      },
    )
    const line = stdout
      .split(/\r?\n/)
      .findLast((candidate) => candidate.startsWith(PATH_MARKER))
    return line ? line.slice(PATH_MARKER.length).trim() : ''
  } catch {
    return ''
  }
}

function mergeExecutablePaths(paths, delimiter = path.delimiter) {
  const seen = new Set()
  const entries = []
  for (const value of paths) {
    for (const entry of (value || '').split(delimiter)) {
      const normalized = entry.trim()
      if (normalized === '' || seen.has(normalized)) continue
      seen.add(normalized)
      entries.push(normalized)
    }
  }
  return entries.join(delimiter)
}

function bundledExecutablePaths(resourcesPath, {
  existsImpl = fs.existsSync,
} = {}) {
  return [
    path.join(resourcesPath, 'runtime', 'bin'),
    path.join(resourcesPath, 'runtime', 'lima', 'bin'),
    path.join(resourcesPath, 'runtime', 'qemu'),
  ].filter((candidate) => existsImpl(candidate))
}

function parseRuntimeVersions(contents) {
  const versions = new Map()
  for (const line of contents.split(/\r?\n/)) {
    const separator = line.indexOf(' ')
    if (separator <= 0) continue
    versions.set(line.slice(0, separator), line.slice(separator + 1).trim())
  }
  return versions
}

function runtimeVersionToken(value) {
  return String(value || '').split(/[ (]/, 1)[0].replace(/^v/, '')
}

async function inspectBundledDockerToolchain({
  resourcesPath,
  platform = process.platform,
  environment = process.env,
  existsImpl = fs.existsSync,
  readFileImpl = fs.readFileSync,
  execFileImpl = execFileAsync,
} = {}) {
  const runtimeRoot = path.join(resourcesPath, 'runtime')
  const versionsPath = path.join(runtimeRoot, 'VERSIONS')
  if (!existsImpl(versionsPath)) {
    throw new Error(`Bundled runtime version manifest is missing: ${versionsPath}`)
  }
  const versions = parseRuntimeVersions(readFileImpl(versionsPath, 'utf8'))
  const dockerVersion = versions.get('docker') || ''
  if (dockerVersion.startsWith('not available')) {
    return { supported: false, message: dockerVersion, versions: Object.fromEntries(versions) }
  }
  const suffix = platform === 'win32' ? '.exe' : ''
  const launcher = path.join(runtimeRoot, 'bin', `docker${suffix}`)
  for (const required of [
    launcher,
    path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-compose${suffix}`),
    path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-buildx${suffix}`),
  ]) {
    if (!existsImpl(required)) throw new Error(`Bundled Docker toolchain is missing ${required}`)
  }
  const commands = [
    ['docker', ['--version'], dockerVersion],
    ['compose', ['compose', 'version', '--short'], versions.get('docker-compose')],
    ['buildx', ['buildx', 'version'], versions.get('docker-buildx')],
  ]
  const reported = {}
  for (const [name, args, expected] of commands) {
    let result
    try {
      result = await execFileImpl(launcher, args, {
        env: environment,
        timeout: 30000,
        maxBuffer: 1024 * 1024,
        windowsHide: true,
      })
    } catch (error) {
      throw new Error(`Bundled Docker ${name} check failed: ${error.message}`)
    }
    const output = `${result.stdout || ''}\n${result.stderr || ''}`.trim()
    const version = runtimeVersionToken(expected)
    if (version === '' || !output.includes(version)) {
      throw new Error(`Bundled Docker ${name} reported ${JSON.stringify(output)}; expected ${version || 'version metadata'}`)
    }
    reported[name] = output
  }
  return { supported: true, launcher, reported, versions: Object.fromEntries(versions) }
}

function daemonExecutable(command) {
  const match = command.match(/\s+daemon\s+start\s*$/)
  if (!match) return null
  const executable = command.slice(0, match.index).trim().replace(/^"(.*)"$/, '$1')
  return /(?:^|[\\/])porto(?:\.exe)?$/i.test(executable) ? executable : null
}

function daemonProcesses(processList) {
  const result = []
  for (const line of processList.split(/\r?\n/)) {
    const match = line.match(/^\s*(\d+)\s+(.+?)\s*$/)
    const executable = match ? daemonExecutable(match[2]) : null
    if (match && executable !== null) {
      result.push({ pid: Number.parseInt(match[1], 10), executable })
    }
  }
  return result
}

function daemonProcessIDs(processList) {
  return daemonProcesses(processList).map((process) => process.pid)
}

function windowsDaemonProcesses(processes) {
  const items = Array.isArray(processes) ? processes : [processes]
  return items
    .map((process) => {
      const executable = process ? daemonExecutable(process.CommandLine || '') : null
      const pid = Number.parseInt(process?.ProcessId, 10)
      return executable !== null && Number.isInteger(pid) ? { pid, executable } : null
    })
    .filter(Boolean)
}

function windowsDaemonProcessIDs(processes) {
  return windowsDaemonProcesses(processes).map((process) => process.pid)
}

function dockerBootstrapCommand(status, {
  isPackaged = true,
} = {}) {
  if (!isPackaged || !status?.enabled) {
    return null
  }
  return ['docker', 'engine-install']
}

function dashboardLoadAction({ rootChildren = 0, reloadAttempts = 0 } = {}) {
  if (rootChildren > 0) return 'ready'
  return reloadAttempts < 1 ? 'reload' : 'error'
}

function resolvePackagedDashboard({
  isPackaged = false,
  resourcesPath = '',
  existsImpl = () => false,
} = {}) {
  if (!isPackaged) return ''
  const dashboard = path.join(resourcesPath, 'dist')
  return existsImpl(path.join(dashboard, 'index.html')) ? dashboard : ''
}

function resolvePortoBinary({
  isPackaged = false,
  platform = process.platform,
  resourcesPath = '',
  environment = process.env,
  existsImpl = () => false,
} = {}) {
  const binary = platform === 'win32' ? 'porto.exe' : 'porto'
  const bundled = path.join(resourcesPath, binary)
  if (isPackaged) return bundled
  const candidates = [environment.PORTO_BINARY, bundled, binary].filter(Boolean)
  return candidates.find((candidate) => candidate === binary || existsImpl(candidate)) || binary
}

module.exports = {
  bundledExecutablePaths,
  daemonBinaryIdentity,
  daemonProcessIDs,
  daemonProcesses,
  dashboardLoadAction,
  dockerBootstrapCommand,
  inspectDaemon,
  inspectDockerStatus,
  installDockerContext,
  installDockerEngine,
  inspectBundledDockerToolchain,
  isDaemonReady,
  mergeExecutablePaths,
  prepareDockerEngineUpdate,
  resolvePackagedDashboard,
  resolvePortoBinary,
  resolveLoginShellPath,
  windowsDaemonProcessIDs,
  windowsDaemonProcesses,
  waitForDockerEngine,
}
