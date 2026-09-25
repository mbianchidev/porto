const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const { EventEmitter, once } = require('node:events')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const { attachRendererLogging, installDesktopLogging, resolveLogPath } = require('./desktop-logging.cjs')

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
    if (file === logger.fd) throw new Error('synthetic disk full')
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

test('captures daemon stdout and stderr after the desktop closes its log handle', async (t) => {
  const options = fixture(t)
  const logger = options.open('debug')
  const child = spawn(process.execPath, ['-e', `
    process.stdin.resume()
    process.stdin.on('end', () => {
      process.stdout.write('synthetic daemon stdout\\n')
      process.stderr.write('synthetic daemon startup failure\\n')
      process.exitCode = 7
    })
  `], {
    stdio: ['pipe', logger.fd, logger.fd],
    timeout: 5000,
    windowsHide: true,
  })
  const exited = once(child, 'exit')
  await once(child, 'spawn')
  logger.close()
  child.stdin.end()
  const [code, signal] = await exited
  assert.equal(signal, null)
  assert.equal(code, 7)
  const contents = fs.readFileSync(options.logPath, 'utf8')
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
  const child = spawn(process.env.PORTO_TEST_DAEMON, ['daemon', 'start'], {
    env: { ...process.env, PORTO_HOME: home, PORTO_LOG_LEVEL: '' },
    stdio: ['ignore', logger.fd, logger.fd],
    timeout: 20000,
    windowsHide: true,
  })
  const [code, signal] = await once(child, 'exit')
  logger.close()

  assert.equal(signal, null)
  assert.equal(code, 1)
  const contents = fs.readFileSync(options.logPath, 'utf8')
  assert.match(contents, /level=DEBUG.*Starting Porto daemon/)
  assert.match(contents, /level=ERROR.*Daemon stopped/)
  assert.equal(contents.match(/Starting Porto daemon/g)?.length, 1)
  assert.match(contents, /porto:/)
})
