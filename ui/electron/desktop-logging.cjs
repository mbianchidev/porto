const fs = require('node:fs')
const path = require('node:path')
const { format } = require('node:util')

const LEVELS = Object.freeze({ debug: 10, info: 20, warn: 30, error: 40 })

function resolveLogPath({
  platform = process.platform,
  appDataPath,
  environment = process.env,
}) {
  const paths = platform === 'win32' ? path.win32 : path.posix
  const home = environment.PORTO_HOME || (appDataPath && paths.join(appDataPath, 'porto'))
  if (!home) throw new Error('Porto application data directory is unavailable')
  return paths.join(home, 'logs', 'porto.log')
}

function installDesktopLogging({
  logPath,
  level = process.env.PORTO_LOG_LEVEL || 'debug',
  consoleImpl = console,
}) {
  const selectedLevel = level.trim().toLowerCase() || 'debug'
  if (!Object.hasOwn(LEVELS, selectedLevel)) {
    throw new Error(`PORTO_LOG_LEVEL must be debug, info, warn, or error, got ${JSON.stringify(level)}`)
  }
  const directory = path.dirname(logPath)
  let fd
  try {
    fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
    fs.chmodSync(directory, 0o700)
    fd = fs.openSync(logPath, 'a', 0o600)
    fs.fchmodSync(fd, 0o600)
  } catch (error) {
    if (fd !== undefined) fs.closeSync(fd)
    throw new Error(`Unable to open Porto log file ${logPath}: ${error.message}`, { cause: error })
  }
  const originals = Object.fromEntries(['debug', 'log', 'info', 'warn', 'error']
    .map((method) => [method, consoleImpl[method]]))
  for (const [method, original] of Object.entries(originals)) {
    const severity = method === 'log' ? 'info' : method
    consoleImpl[method] = (...args) => {
      if (LEVELS[severity] < LEVELS[selectedLevel]) return
      const record = `time=${new Date().toISOString()} level=${severity.toUpperCase()} component=desktop pid=${process.pid} msg=${JSON.stringify(format(...args))}\n`
      try {
        fs.appendFileSync(fd, record)
      } catch (error) {
        originals.error.call(consoleImpl, `Unable to write Porto log file ${logPath}`, error)
      }
      original.apply(consoleImpl, args)
    }
  }
  let closed = false
  return {
    fd,
    path: logPath,
    level: selectedLevel,
    close() {
      if (closed) return
      closed = true
      Object.assign(consoleImpl, originals)
      try {
        fs.closeSync(fd)
      } catch (error) {
        originals.error.call(consoleImpl, `Unable to close Porto log file ${logPath}`, error)
      }
    },
  }
}

function attachRendererLogging(webContents, consoleImpl = console) {
  webContents.on('console-message', ({ level, message, lineNumber }) => {
    const method = level === 'warning' ? 'warn' : level
    consoleImpl[method]('[renderer] %s (line %d)', message, lineNumber)
  })
  webContents.on('render-process-gone', (_event, details) => {
    const method = details.reason === 'clean-exit' ? 'debug' : 'error'
    consoleImpl[method]('Renderer exited: reason=%s exitCode=%d', details.reason, details.exitCode)
  })
}

module.exports = { attachRendererLogging, installDesktopLogging, resolveLogPath }
