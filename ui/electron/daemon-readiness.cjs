const DEFAULT_DAEMON_URL = 'http://127.0.0.1:37623'
const DEFAULT_TIMEOUT_MS = 800
const EXPECTED_API_VERSION = 2

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

function isDaemonCommand(command) {
  return /(?:^|[\\/])porto(?:\.exe)?"?\s+daemon\s+start\s*$/.test(command)
}

function daemonProcessIDs(processList) {
  const result = []
  for (const line of processList.split(/\r?\n/)) {
    const match = line.match(/^\s*(\d+)\s+(.+?)\s*$/)
    if (match && isDaemonCommand(match[2])) {
      result.push(Number.parseInt(match[1], 10))
    }
  }
  return result
}

function windowsDaemonProcessIDs(processes) {
  const items = Array.isArray(processes) ? processes : [processes]
  return items
    .filter((process) => process && isDaemonCommand(process.CommandLine || ''))
    .map((process) => Number.parseInt(process.ProcessId, 10))
    .filter(Number.isInteger)
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

module.exports = {
  daemonProcessIDs,
  dockerBootstrapCommand,
  inspectDaemon,
  inspectDockerStatus,
  installDockerEngine,
  isDaemonReady,
  windowsDaemonProcessIDs,
}
