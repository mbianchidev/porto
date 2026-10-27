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
  onProgress,
  signal,
} = {}) {
  if (onProgress !== undefined && typeof onProgress !== 'function') {
    throw new TypeError('Engine installation progress must be a function')
  }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  const abort = () => controller.abort()
  signal?.addEventListener('abort', abort, { once: true })
  if (signal?.aborted) abort()
  let receivedResponse = false
  try {
    controller.signal.throwIfAborted()
    const response = await fetchImpl(`${daemonURL}/api/docker/engine/install`, {
      method: 'POST',
      signal: controller.signal,
      ...(onProgress ? { headers: { Accept: 'application/x-ndjson' } } : {}),
    })
    receivedResponse = true
    if (onProgress) {
      if (!response.ok) throw engineInstallationError('backend')
      return await readEngineInstallProgress(response, onProgress, controller.signal)
    }
    if (!response.ok) {
      const message = typeof response.text === 'function' ? await response.text() : ''
      throw new Error(message.trim() || `Porto Docker installation returned HTTP ${response.status || 'error'}`)
    }
    return await response.json()
  } catch (error) {
    const interrupted = controller.signal.aborted
    controller.abort()
    if (!onProgress) throw error
    if (error instanceof EngineInstallationError) throw error
    throw engineInstallationError(interrupted ? 'cancelled' : receivedResponse ? 'transport' : 'backend')
  } finally {
    clearTimeout(timer)
    signal?.removeEventListener('abort', abort)
  }
}

class EngineInstallationError extends Error {
  constructor(code, message) {
    super(message)
    this.name = 'EngineInstallationError'
    this.code = code
  }
}

function runtimeFailureDiagnostic(error) {
  let message = String(error?.message ?? error)
  message = message
    .replace(/-----BEGIN (?:[A-Z0-9 ]*PRIVATE KEY|CERTIFICATE)-----[\s\S]*?-----END (?:[A-Z0-9 ]*PRIVATE KEY|CERTIFICATE)-----/g, '[REDACTED]')
    .replace(/\b[a-z][a-z0-9+.-]*:\/\/[^\s"'<>]+/gi, '[REDACTED URL]')
    .replace(/("[^"]*(?:password|passwd|token|secret|authorization|auth|key-data|certificate-data|proxy)[^"]*"\s*:\s*)"(?:\\.|[^"\\])*"/gi, '$1"[REDACTED]"')
    .replace(/(authorization\s*:\s*bearer\s+)[^\s]+/gi, '$1[REDACTED]')
    .replace(/^(\s*[A-Z0-9_.-]*(?:PASSWORD|PASSWD|TOKEN|SECRET|AUTHORIZATION|AUTH|KEY-DATA|CERTIFICATE-DATA|PROXY)[A-Z0-9_.-]*\s*[:=]\s*).+$/gim, '$1[REDACTED]')
    .replace(/\b(password|passwd|token|secret|authorization|auth)\s*[:=]\s*[^,\s;"']+/gi, '$1=[REDACTED]')
    .replace(/\b(?:gh[pousr]_[A-Za-z0-9_]{16,}|AKIA[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9-]{16,})\b/gi, '[REDACTED]')
  return message.length > 16384 ? `${message.slice(0, 16384)} [diagnostic truncated]` : message
}

function engineInstallationError(code, cleanupRequired = false) {
  const messages = {
    network: 'Required runtime downloads or network setup failed. Check your internet connection, DNS, proxy settings and HTTPS trust, then retry.',
    'local-tool': "A required runtime executable is missing or cannot run. Repair or reinstall Porto's bundled tools and required host prerequisites, then retry.",
    backend: 'The containerd / BuildKit backend or Porto daemon is unavailable. Check runtime diagnostics and required virtualization support, then retry.',
    ownership: 'Porto could not verify engine ownership. Preserve existing instances and data; resolve the ownership conflict in runtime diagnostics before retrying.',
    cancelled: 'Runtime setup was interrupted or timed out. Retry when you are ready; setup has not been confirmed.',
    transport: 'Runtime provisioning progress was interrupted or invalid. Setup has not been confirmed. Check runtime diagnostics and retry without removing existing data.',
  }
  const message = messages[code] || messages.transport
  const error = new EngineInstallationError(messages[code] ? code : 'transport', message
    + (cleanupRequired ? ' Cleanup also needs attention; inspect runtime diagnostics before retrying.' : ''))
  error.cleanupRequired = cleanupRequired
  return error
}

async function readEngineInstallProgress(response, onProgress, signal) {
  if (response.headers?.get('content-type')?.split(';', 1)[0].trim() !== 'application/x-ndjson'
    || typeof response.body?.getReader !== 'function') {
    await response.body?.cancel?.().catch(() => {})
    throw engineInstallationError('transport')
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder('utf-8', { fatal: true })
  const stages = new Set(['checking', 'starting', 'downloading', 'configuring', 'verifying', 'saving'])
  const components = new Set(['runtime', 'lima', 'guest-image', 'guest-tools', 'ownership', 'multi-platform', 'runtime-helper', 'network', 'containerd', 'buildkit', 'state'])
  const failureCodes = new Set(['network', 'local-tool', 'backend', 'ownership', 'cancelled'])
  let rejectAbort
  const aborted = new Promise((_, reject) => { rejectAbort = reject })
  const abort = () => rejectAbort(engineInstallationError('cancelled'))
  signal.addEventListener('abort', abort, { once: true })
  if (signal.aborted) abort()
  let pending = ''
  let completed = null
  const record = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
  try {
    while (true) {
      const { done, value } = await Promise.race([reader.read(), aborted])
      pending += done ? decoder.decode() : decoder.decode(value, { stream: true })
      let end
      while ((end = pending.indexOf('\n')) !== -1) {
        if (completed !== null || end > 16384) throw engineInstallationError('transport')
        const event = JSON.parse(pending.slice(0, end))
        pending = pending.slice(end + 1)
        if (!record(event)) throw engineInstallationError('transport')
        if (event.type === 'progress') {
          const progress = event.progress
          if (!record(progress) || !stages.has(progress.stage) || !components.has(progress.component)
            || typeof progress.message !== 'string' || progress.message.trim() === '' || progress.message.length > 512
            || event.status !== undefined || event.error !== undefined) {
            throw engineInstallationError('transport')
          }
          await Promise.race([Promise.resolve(onProgress(progress)), aborted])
        } else if (event.type === 'error') {
          const failure = event.error
          if (!record(failure) || !failureCodes.has(failure.code) || typeof failure.message !== 'string'
            || (failure.cleanupRequired !== undefined && typeof failure.cleanupRequired !== 'boolean')
            || event.status !== undefined || event.progress !== undefined) {
            throw engineInstallationError('transport')
          }
          throw engineInstallationError(failure.code, failure.cleanupRequired === true)
        } else if (event.type === 'complete') {
          if (!record(event.status) || event.status.available !== true
            || event.error !== undefined || event.progress !== undefined) {
            throw engineInstallationError('transport')
          }
          completed = event.status
        } else {
          throw engineInstallationError('transport')
        }
      }
      if (pending.length > 16384) throw engineInstallationError('transport')
      if (done) {
        if (pending !== '' || completed === null || signal.aborted) throw engineInstallationError('transport')
        return completed
      }
    }
  } catch (error) {
    try { await reader.cancel() } catch {}
    throw error
  } finally {
    signal.removeEventListener('abort', abort)
    reader.releaseLock()
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
  const dive = path.join(runtimeRoot, 'bin', `dive${suffix}`)
  for (const required of [
    launcher,
    dive,
    path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-compose${suffix}`),
    path.join(runtimeRoot, 'docker', 'cli-plugins', `docker-buildx${suffix}`),
  ]) {
    if (!existsImpl(required)) throw new Error(`Bundled Docker toolchain is missing ${required}`)
  }
  const commands = [
    ['docker', launcher, ['--version'], dockerVersion],
    ['compose', launcher, ['compose', 'version', '--short'], versions.get('docker-compose')],
    ['buildx', launcher, ['buildx', 'version'], versions.get('docker-buildx')],
    ['dive', dive, ['--version'], versions.get('dive')],
  ]
  const reported = {}
  for (const [name, executable, args, expected] of commands) {
    let result
    try {
      result = await execFileImpl(executable, args, {
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
  return { supported: true, launcher, dive, reported, versions: Object.fromEntries(versions) }
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
  runtimeFailureDiagnostic,
  windowsDaemonProcessIDs,
  windowsDaemonProcesses,
  waitForDockerEngine,
}
