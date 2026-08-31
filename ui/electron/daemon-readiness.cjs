const DEFAULT_DAEMON_URL = 'http://127.0.0.1:37623'
const DEFAULT_TIMEOUT_MS = 800
const EXPECTED_API_VERSION = 1

async function isDaemonReady({
  daemonURL = DEFAULT_DAEMON_URL,
  fetchImpl = globalThis.fetch,
  timeoutMs = DEFAULT_TIMEOUT_MS,
} = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)

  try {
    const response = await fetchImpl(`${daemonURL}/api/health`, { signal: controller.signal })
    if (!response.ok) return false

    const health = await response.json()
    return health.status === 'ok'
      && health.apiVersion === EXPECTED_API_VERSION
      && health.dashboardReady === true
  } catch {
    return false
  } finally {
    clearTimeout(timer)
  }
}

module.exports = {
  isDaemonReady,
}
