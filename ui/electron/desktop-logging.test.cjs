const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const { EventEmitter, once } = require('node:events')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  attachRendererLogging,
  initializeLogDateState,
  installDesktopLogging,
  openDaemonBootstrapLog,
  pruneUpdateLogs,
  resolveLogPath,
} = require('./desktop-logging.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-logging-'))
  const loggers = []
  t.after(() => {
    for (const logger of loggers.reverse()) logger.close()
    fs.rmSync(directory, { recursive: true, force: true })
  })
  const calls = []
  const consoleImpl = Object.fromEntries(['debug', 'log', 'info', 'warn', 'error']
    .map((method) => [method, (...args) => { calls.push({ method, args }) }]))
  const options = { logPath: path.join(directory, 'logs', 'porto.log'), consoleImpl, calls }
  return {
    ...options,
    open(level) {
      const logger = installDesktopLogging({ ...options, level })
      loggers.push(logger)
      return logger
    },
  }
}

test('places logs beside Porto state in each operating system', () => {
  const cases = [
    ['win32', 'C:\\Users\\synthetic\\AppData\\Roaming', 'C:\\Users\\synthetic\\AppData\\Roaming\\porto\\logs\\porto.log'],
    ['darwin', '/Users/synthetic/Library/Application Support', '/Users/synthetic/Library/Application Support/porto/logs/porto.log'],
    ['linux', '/home/synthetic/.config', '/home/synthetic/.config/porto/logs/porto.log'],
  ]
  for (const [platform, appDataPath, expected] of cases) {
    assert.equal(resolveLogPath({ platform, appDataPath, environment: {} }), expected)
  }
  assert.equal(resolveLogPath({
    platform: 'win32',
    appDataPath: 'C:\\Users\\synthetic\\AppData\\Roaming',
    environment: { PORTO_HOME: 'D:\\Synthetic Porto' },
  }), 'D:\\Synthetic Porto\\logs\\porto.log')
})

test('preserves the existing log date before the desktop appends', (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-log-date-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const logPath = path.join(directory, 'logs', 'porto.log')
  fs.mkdirSync(path.dirname(logPath), { recursive: true })
  fs.writeFileSync(logPath, 'previous day\n')
  const previous = new Date(2026, 8, 28, 12, 0, 0)
  fs.utimesSync(logPath, previous, previous)
  initializeLogDateState(logPath)
  assert.equal(
    fs.readFileSync(path.join(path.dirname(logPath), '.porto-log-date'), 'utf8').trim(),
    '2026-09-28',
  )
})

test('defaults to debug and persists desktop errors without replacing console output', (t) => {
  const options = fixture(t)
  const originalDebug = options.consoleImpl.debug
  const logger = options.open('')
  options.consoleImpl.debug('synthetic debug detail')
  options.consoleImpl.info('synthetic startup')
  options.consoleImpl.error(new Error('synthetic startup failure'))
  logger.close()
  logger.close()

  const contents = fs.readFileSync(options.logPath, 'utf8')
  assert.match(contents, /level=DEBUG.*synthetic debug detail/)
  assert.match(contents, /level=INFO.*synthetic startup/)
  assert.match(contents, /level=ERROR.*synthetic startup failure/)
  assert.match(contents, /component=desktop/)
  assert.equal(options.calls.length, 3)
  assert.equal(options.consoleImpl.debug, originalDebug)
  if (process.platform !== 'win32') {
    assert.equal(fs.statSync(options.logPath).mode & 0o777, 0o600)
    assert.equal(fs.statSync(path.dirname(options.logPath)).mode & 0o777, 0o700)
  }
})

test('honors the log level and appends across restarts', (t) => {
  const options = fixture(t)
  const first = options.open('debug')
  options.consoleImpl.info('previous startup')
  first.close()
  const second = options.open(' WARN ')
  options.consoleImpl.debug('hidden debug')
  options.consoleImpl.info('hidden info')
  options.consoleImpl.warn('retained warning')
  options.consoleImpl.error('retained error')
  second.close()

  const contents = fs.readFileSync(options.logPath, 'utf8')
  assert.match(contents, /previous startup/)
  assert.match(contents, /level=WARN.*retained warning/)
  assert.match(contents, /level=ERROR.*retained error/)
  assert.doesNotMatch(contents, /hidden/)
})

test('rejects invalid logging configuration instead of silently ignoring it', (t) => {
  const options = fixture(t)
  assert.throws(() => options.open('verbose'), /PORTO_LOG_LEVEL/)
  assert.equal(fs.existsSync(options.logPath), false)
  fs.writeFileSync(path.dirname(options.logPath), 'synthetic obstruction')
  assert.throws(() => options.open('debug'), /log/i)
})

test('reports file-write failures through the original console', (t) => {
  const options = fixture(t)
  const logger = options.open('debug')
  const append = fs.appendFileSync
  t.mock.method(fs, 'appendFileSync', (file, ...args) => {
    if (file === logger.path) throw new Error('synthetic disk full')
    return append(file, ...args)
  })
  options.consoleImpl.error('original diagnostic')
  assert.ok(options.calls.some(({ method, args }) => method === 'error'
    && args.some((argument) => String(argument).includes('synthetic disk full'))))
  assert.ok(options.calls.some(({ args }) => args.includes('original diagnostic')))
})

test('captures renderer messages with their severity and unexpected exits', (t) => {
  const options = fixture(t)
  options.open('debug')
  const webContents = new EventEmitter()
  attachRendererLogging(webContents, options.consoleImpl)
  for (const level of ['debug', 'info', 'warning', 'error']) {
    webContents.emit('console-message', {
      level,
      message: `synthetic renderer ${level}`,
      lineNumber: 42,
    })
  }
  webContents.emit('render-process-gone', {}, { reason: 'crashed', exitCode: 7 })
  webContents.emit('render-process-gone', {}, { reason: 'clean-exit', exitCode: 0 })
  const contents = fs.readFileSync(options.logPath, 'utf8')
  assert.match(contents, /level=DEBUG.*synthetic renderer debug/)
  assert.match(contents, /level=INFO.*synthetic renderer info/)
  assert.match(contents, /level=WARN.*synthetic renderer warning/)
  assert.match(contents, /level=ERROR.*synthetic renderer error/)
  assert.match(contents, /level=ERROR.*Renderer exited: reason=crashed exitCode=7/)
  assert.match(contents, /level=DEBUG.*Renderer exited: reason=clean-exit exitCode=0/)
})

test('captures daemon startup output after the desktop closes its bootstrap handle', async (t) => {
  const options = fixture(t)
  const bootstrap = openDaemonBootstrapLog(options.logPath)
  const child = spawn(process.execPath, ['-e', `
    process.stdin.resume()
    process.stdin.on('end', () => {
      process.stdout.write('synthetic daemon stdout\\n')
      process.stderr.write('synthetic daemon startup failure\\n')
      process.exitCode = 7
    })
  `], {
    stdio: ['pipe', bootstrap.fd, bootstrap.fd],
    timeout: 5000,
    windowsHide: true,
  })
  const exited = once(child, 'exit')
  await once(child, 'spawn')
  bootstrap.close()
  child.stdin.end()
  const [code, signal] = await exited
  assert.equal(signal, null)
  assert.equal(code, 7)
  const contents = fs.readFileSync(bootstrap.path, 'utf8')
  assert.match(contents, /synthetic daemon stdout/)
  assert.match(contents, /synthetic daemon startup failure/)
})

test('the installed daemon logs startup failures once through desktop stdio', {
  skip: !process.env.PORTO_TEST_DAEMON,
}, async (t) => {
  const options = fixture(t)
  const home = path.dirname(path.dirname(options.logPath))
  fs.mkdirSync(path.join(home, 'porto.db'))
  const logger = options.open('')
  const bootstrap = openDaemonBootstrapLog(options.logPath)
  const child = spawn(process.env.PORTO_TEST_DAEMON, ['daemon', 'start'], {
    env: {
      ...process.env,
      PORTO_HOME: home,
      PORTO_LOG_LEVEL: '',
      PORTO_LOG_MIRROR_STDERR: 'false',
    },
    stdio: ['ignore', bootstrap.fd, bootstrap.fd],
    timeout: 20000,
    windowsHide: true,
  })
  const exited = once(child, 'exit')
  await once(child, 'spawn')
  bootstrap.close()
  const [code, signal] = await exited
  logger.close()

  assert.equal(signal, null)
  assert.equal(code, 1)
  const contents = fs.readFileSync(options.logPath, 'utf8')
  assert.match(contents, /level=DEBUG.*Starting Porto daemon/)
  assert.match(contents, /level=ERROR.*Daemon stopped/)
  assert.equal(contents.match(/Starting Porto daemon/g)?.length, 1)
  assert.match(fs.readFileSync(bootstrap.path, 'utf8'), /porto:/)
})

test('update log cleanup deletes only expired install records and emptied version folders', (t) => {
  const updates = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-updates-'))
  const outside = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-outside-'))
  t.after(() => {
    fs.rmSync(updates, { recursive: true, force: true })
    fs.rmSync(outside, { recursive: true, force: true })
  })
  const now = new Date(2026, 8, 29, 12)
  const expired = new Date(2026, 8, 22, 23, 59)
  const retained = new Date(2026, 8, 23, 0, 0)
  const write = (relative, time, contents = 'synthetic') => {
    const file = path.join(updates, relative)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, contents)
    fs.utimesSync(file, time, time)
    return file
  }
  write('1.0.0/porto-desktop_1.0.0_darwin_arm64.dmg.install.log', expired)
  write('1.0.0/apply-update-4242.sh', expired)
  write('1.1.0/porto-desktop_1.1.0_darwin_arm64.dmg.install.log', retained)
  write('1.1.0/apply-update-4343.ps1.proceed', retained)
  write('1.2.0/porto-desktop_1.2.0_darwin_arm64.dmg.install.log', expired)
  write('1.2.0/porto-desktop_1.2.0_darwin_arm64.dmg', expired)
  write('not-a-version/old.install.log', expired)
  write('install-error.txt', expired)
  fs.mkdirSync(path.join(updates, '1.3.0'))
  const outsideLog = path.join(outside, 'linked.install.log')
  fs.writeFileSync(outsideLog, 'outside')
  fs.utimesSync(outsideLog, expired, expired)
  const expiredFolders = ['1.0.0', '1.2.0', '1.3.0']
  fs.mkdirSync(path.join(updates, '1.4.0'))
  try {
    fs.symlinkSync(outsideLog, path.join(updates, '1.4.0', 'linked.install.log'))
    expiredFolders.push('1.4.0')
  } catch (error) {
    // Windows needs elevated rights for file symlinks.
    if (error.code !== 'EPERM') throw error
    fs.rmdirSync(path.join(updates, '1.4.0'))
  }
  for (const folder of expiredFolders) fs.utimesSync(path.join(updates, folder), expired, expired)

  const result = pruneUpdateLogs({ updatesDirectory: updates, retentionDays: 7, now })

  assert.deepEqual(result, { removedFiles: 3, removedDirectories: 2, errors: [] })
  assert.deepEqual(fs.readdirSync(updates).sort(), [
    '1.1.0', '1.2.0', ...(expiredFolders.includes('1.4.0') ? ['1.4.0'] : []), 'install-error.txt', 'not-a-version',
  ])
  assert.deepEqual(fs.readdirSync(path.join(updates, '1.2.0')), ['porto-desktop_1.2.0_darwin_arm64.dmg'])
  assert.deepEqual(fs.readdirSync(path.join(updates, '1.1.0')).sort(), ['apply-update-4343.ps1.proceed', 'porto-desktop_1.1.0_darwin_arm64.dmg.install.log'])
  assert.equal(fs.readFileSync(outsideLog, 'utf8'), 'outside')
  assert.deepEqual(pruneUpdateLogs({ updatesDirectory: path.join(updates, 'missing'), retentionDays: 7, now }), { removedFiles: 0, removedDirectories: 0, errors: [] })
  assert.throws(() => pruneUpdateLogs({ updatesDirectory: updates, retentionDays: 0, now }), RangeError)
})
