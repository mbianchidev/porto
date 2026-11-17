const fs = require('node:fs')
const path = require('node:path')
const { format } = require('node:util')

const LEVELS = Object.freeze({ debug: 10, info: 20, warn: 30, error: 40 })

function localDate(value = new Date()) {
  const year = String(value.getFullYear()).padStart(4, '0')
  const month = String(value.getMonth() + 1).padStart(2, '0')
  const day = String(value.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

function initializeLogDateState(logPath) {
  const statePath = path.join(path.dirname(logPath), '.porto-log-date')
  let activeDate = localDate()
  try {
    const info = fs.statSync(logPath)
    if (info.size > 0) activeDate = localDate(info.mtime)
  } catch (error) {
    if (error.code !== 'ENOENT') throw error
  }
  let fd
  try {
    fd = fs.openSync(statePath, 'wx', 0o600)
    fs.writeFileSync(fd, `${activeDate}\n`, 'utf8')
    fs.fchmodSync(fd, 0o600)
  } catch (error) {
    if (error.code !== 'EEXIST') {
      throw new Error(`Unable to initialize Porto log date state ${statePath}: ${error.message}`, { cause: error })
    }
  } finally {
    if (fd !== undefined) fs.closeSync(fd)
  }
}

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
  try {
    fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
    fs.chmodSync(directory, 0o700)
    initializeLogDateState(logPath)
    const fd = fs.openSync(logPath, 'a', 0o600)
    fs.fchmodSync(fd, 0o600)
    fs.closeSync(fd)
  } catch (error) {
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
        fs.appendFileSync(logPath, record, { encoding: 'utf8', mode: 0o600 })
      } catch (error) {
        originals.error.call(consoleImpl, `Unable to write Porto log file ${logPath}`, error)
      }
      original.apply(consoleImpl, args)
    }
  }
  let closed = false
  return {
    path: logPath,
    level: selectedLevel,
    close() {
      if (closed) return
      closed = true
      Object.assign(consoleImpl, originals)
    },
  }
}

function openDaemonBootstrapLog(logPath) {
  const bootstrapPath = path.join(path.dirname(logPath), 'daemon-startup.log')
  let fd
  try {
    fs.mkdirSync(path.dirname(bootstrapPath), { recursive: true, mode: 0o700 })
    fd = fs.openSync(bootstrapPath, 'w', 0o600)
    fs.fchmodSync(fd, 0o600)
  } catch (error) {
    if (fd !== undefined) fs.closeSync(fd)
    throw new Error(`Unable to open Porto daemon startup log ${bootstrapPath}: ${error.message}`, { cause: error })
  }
  let closed = false
  return {
    fd,
    path: bootstrapPath,
    close() {
      if (closed) return
      closed = true
      fs.closeSync(fd)
    },
  }
}

const UPDATE_VERSION_DIRECTORY = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?$/
// The installer log and the helper copy (with its Windows handshake markers)
// that a finished install leaves in its version folder.
const UPDATE_INSTALL_RECORD = /(?:\.install\.log|^apply-update-\d+\.(?:sh|ps1)(?:\.ready|\.proceed)?)$/

// Deletes updater install logs and helper copies in updates/<version>/ dated
// before the retention window, which counts calendar days including today, and
// removes version folders that become empty. Packages, in-progress downloads,
// symlinks and unrelated files are left alone.
function pruneUpdateLogs({ updatesDirectory, retentionDays, now = new Date() }) {
  if (!Number.isInteger(retentionDays) || retentionDays < 1) {
    throw new RangeError(`Log retention must be a positive whole number of days, got ${JSON.stringify(retentionDays)}`)
  }
  const cutoff = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (retentionDays - 1))
  const result = { removedFiles: 0, removedDirectories: 0, errors: [] }
  let versions
  try {
    versions = fs.readdirSync(updatesDirectory, { withFileTypes: true })
  } catch (error) {
    if (error.code === 'ENOENT') return result
    throw new Error(`Unable to list Porto updates ${updatesDirectory}: ${error.message}`, { cause: error })
  }
  for (const version of versions) {
    if (!version.isDirectory() || !UPDATE_VERSION_DIRECTORY.test(version.name)) continue
    const versionDirectory = path.join(updatesDirectory, version.name)
    try {
      // Removing a log refreshes the folder time, so judge the folder first.
      const expiredDirectory = fs.lstatSync(versionDirectory).mtime < cutoff
      for (const entry of fs.readdirSync(versionDirectory, { withFileTypes: true })) {
        if (!entry.isFile() || !UPDATE_INSTALL_RECORD.test(entry.name)) continue
        const logPath = path.join(versionDirectory, entry.name)
        try {
          const info = fs.lstatSync(logPath)
          if (!info.isFile() || info.mtime >= cutoff) continue
          fs.rmSync(logPath)
          result.removedFiles += 1
        } catch (error) {
          result.errors.push(new Error(`Unable to remove expired update install file ${logPath}: ${error.message}`, { cause: error }))
        }
      }
      if (expiredDirectory && fs.readdirSync(versionDirectory).length === 0) {
        fs.rmdirSync(versionDirectory)
        result.removedDirectories += 1
      }
    } catch (error) {
      result.errors.push(new Error(`Unable to clean update folder ${versionDirectory}: ${error.message}`, { cause: error }))
    }
  }
  return result
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

module.exports = {
  attachRendererLogging,
  initializeLogDateState,
  installDesktopLogging,
  openDaemonBootstrapLog,
  pruneUpdateLogs,
  resolveLogPath,
}
