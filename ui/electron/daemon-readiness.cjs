const path = require('node:path')

const DEFAULT_DAEMON_URL = 'http://127.0.0.1:37623'
const DEFAULT_TIMEOUT_MS = 800
const EXPECTED_API_VERSION = 6

async function inspectDaemon({
  daemonURL = DEFAULT_DAEMON_URL,
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
  platform = process.platform,
} = {}) {
  if (!isPackaged || platform === 'win32' || !status?.enabled || status.available) {
    return null
  }
  return ['docker', 'engine-install']
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
  daemonProcessIDs,
  daemonProcesses,
  dockerBootstrapCommand,
  inspectDaemon,
  inspectDockerStatus,
  installDockerEngine,
  isDaemonReady,
  resolvePortoBinary,
  windowsDaemonProcessIDs,
  windowsDaemonProcesses,
}
