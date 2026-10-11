const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const { developmentLaunch, launchDevelopmentApp, prepareMacOSDevelopmentApp } = require('./dev-launcher.cjs')

test('development launch retains an absolute application path on Windows and Linux', () => {
  for (const platform of ['win32', 'linux']) {
    const directory = path.resolve('synthetic source with spaces', 'ui', 'electron')
    const electron = path.resolve('synthetic runtime', platform === 'win32' ? 'electron.exe' : 'electron')
    const launch = developmentLaunch({
      platform, appDirectory: directory, electronExecutable: electron,
      args: ['--inspect=9229'], environment: { PORTO_BIN: '/synthetic/porto', ELECTRON_RUN_AS_NODE: '1' },
    })
    assert.equal(launch.executable, electron)
    assert.deepEqual(launch.args, [directory, '--inspect=9229'])
    assert.equal(launch.cwd, directory)
    assert.equal(launch.environment.PORTO_BIN, '/synthetic/porto')
    assert.equal(launch.environment.ELECTRON_RUN_AS_NODE, undefined)
  }
})

test('development launcher reports startup failures instead of retrying raw Electron', async () => {
  const { EventEmitter } = require('node:events')
  let attempts = 0
  await assert.rejects(launchDevelopmentApp({
    platform: 'linux', appDirectory: __dirname, electronExecutable: '/synthetic/missing-electron',
    spawnImpl: () => {
      attempts += 1
      const child = new EventEmitter()
      child.kill = () => true
      process.nextTick(() => child.emit('error', new Error('synthetic startup failure')))
      return child
    },
  }), /Unable to start Porto development desktop: synthetic startup failure/)
  assert.equal(attempts, 1)
})

test('macOS dev app opens source on first launch and reopens without an application argument', {
  skip: process.platform !== 'darwin',
}, async (t) => {
  const { spawnSync } = require('node:child_process')
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto dev reopen '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const source = path.join(directory, 'source with spaces-\u03b4', 'ui', 'electron')
  fs.mkdirSync(source, { recursive: true })
  fs.writeFileSync(path.join(source, 'package.json'), JSON.stringify({
    name: 'synthetic-porto-dev', productName: 'Synthetic Porto Dev', version: '1.0.0', main: 'main.cjs',
  }))
  const entry = `
    const { app, BrowserWindow } = require('electron')
    const fs = require('node:fs')
    const timeout = setTimeout(() => { console.error('Timed out before source rendering'); app.exit(1) }, 30000)
    app.whenReady().then(async () => {
      if (app.isPackaged) throw new Error('Dev app must not enable packaged daemon replacement or updates')
      if (fs.realpathSync(app.getAppPath()) !== fs.realpathSync(process.env.PORTO_TEST_DEV_SOURCE)) throw new Error('Wrong source application')
      const window = new BrowserWindow({ show: false, webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false } })
      await window.loadURL('data:text/html,<main>synthetic source renderer</main>')
      if (await window.webContents.executeJavaScript('document.querySelector("main").textContent') !== 'synthetic source renderer') throw new Error('No app rendered')
      console.log('PORTO_DEV_SOURCE_RENDERED')
      clearTimeout(timeout)
      window.destroy()
      app.quit()
    }).catch(error => { console.error(error); app.exit(1) })
  `
  fs.writeFileSync(path.join(source, 'main.cjs'), entry)
  const electron = require('electron')
  const before = fs.readFileSync(electron)
  const options = { appDirectory: source, electronExecutable: electron, cacheRoot: path.join(directory, 'cache') }
  const bundle = await prepareMacOSDevelopmentApp(options)
  const executable = path.join(bundle, 'Contents', 'MacOS', 'Electron')
  assert.deepEqual(fs.readFileSync(electron), before, 'The npm-installed Electron runtime must not be edited')
  const launch = developmentLaunch({ platform: 'darwin', developmentBundle: bundle, appDirectory: source, args: ['--disable-breakpad'] })
  assert.deepEqual(launch.args, ['--disable-breakpad'], 'The cached app must not rely on the original CLI app path')
  const environment = { ...process.env, PORTO_TEST_DEV_SOURCE: source }
  delete environment.ELECTRON_RUN_AS_NODE
  delete environment.ELECTRON_FORCE_IS_PACKAGED
  delete environment.DYLD_INSERT_LIBRARIES
  for (const attempt of ['first', 'reopened']) {
    const result = spawnSync(executable, [`--user-data-dir=${path.join(directory, attempt)}`, '--disable-breakpad'], {
      cwd: directory, env: environment, encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
    })
    assert.ifError(result.error)
    assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`)
    assert.match(result.stdout, /PORTO_DEV_SOURCE_RENDERED/)
  }
  const stdout = path.join(directory, 'reopen.stdout')
  const stderr = path.join(directory, 'reopen.stderr')
  const reopened = spawnSync('/usr/bin/open', [
    '-n', '-W', bundle, '--stdout', stdout, '--stderr', stderr,
    '--env', `PORTO_TEST_DEV_SOURCE=${source}`,
    '--args', `--user-data-dir=${path.join(directory, 'launch services profile')}`, '--disable-breakpad',
  ], { cwd: directory, env: environment, encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL' })
  assert.ifError(reopened.error)
  assert.equal(reopened.status, 0, `${reopened.stdout}\n${reopened.stderr}`)
  assert.match(fs.readFileSync(stdout, 'utf8'), /PORTO_DEV_SOURCE_RENDERED/,
    `LaunchServices reopen did not load source:\n${fs.readFileSync(stderr, 'utf8')}`)
  const modifiedAt = fs.statSync(executable).mtimeMs
  assert.equal(await prepareMacOSDevelopmentApp(options), bundle)
  assert.equal(fs.statSync(executable).mtimeMs, modifiedAt, 'Unchanged dev runs must reuse the guarded bundle')
})
