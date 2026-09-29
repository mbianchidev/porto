const assert = require('node:assert/strict')
const { EventEmitter } = require('node:events')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  UPDATE_HELPER_READY_TIMEOUT,
  currentInstallation,
  installerCommand,
  launchDownloadedUpdate,
  waitForUpdateHelperReady,
} = require('./desktop-update-install.cjs')

test('resolves the replaceable desktop installation from the running executable', () => {
  assert.deepEqual(
    currentInstallation('darwin', '/Users/test/Applications/Porto.app/Contents/MacOS/Porto'),
    {
      destination: '/Users/test/Applications/Porto.app',
      executable: '/Users/test/Applications/Porto.app/Contents/MacOS/Porto',
    },
  )
  assert.deepEqual(
    currentInstallation('linux', '/home/test/.local/opt/porto/Porto'),
    {
      destination: '/home/test/.local/opt/porto',
      executable: '/home/test/.local/opt/porto/Porto',
    },
  )
  assert.deepEqual(
    currentInstallation('win32', 'C:\\Users\\test\\AppData\\Local\\Programs\\Porto\\Porto.exe'),
    {
      destination: 'C:\\Users\\test\\AppData\\Local\\Programs\\Porto',
      executable: 'C:\\Users\\test\\AppData\\Local\\Programs\\Porto\\Porto.exe',
    },
  )
  assert.throws(
    () => currentInstallation('darwin', '/tmp/Porto'),
    /application bundle/i,
  )
  assert.throws(
    () => currentInstallation('freebsd', '/opt/Porto'),
    /not supported/i,
  )
})

test('builds detached helper commands without interpolating paths into shell code', () => {
  const mac = installerCommand({
    platform: 'darwin',
    helperPath: '/Users/test/Library/Application Support/Porto/apply-update.sh',
    parentPid: 42,
    packagePath: '/Users/test/Library/Application Support/Porto/update.dmg',
    destination: '/Users/test/Applications/Porto.app',
    executable: '/Users/test/Applications/Porto.app/Contents/MacOS/Porto',
    errorFile: '/Users/test/Library/Application Support/Porto/update-error.txt',
    archiveRootName: '',
  })
  assert.equal(mac.command, '/bin/bash')
  assert.deepEqual(mac.args, [
    mac.helperPath,
    'darwin',
    '42',
    mac.packagePath,
    mac.destination,
    mac.executable,
    mac.errorFile,
    '',
  ])

  const windows = installerCommand({
    platform: 'win32',
    helperPath: 'C:\\Users\\test\\AppData\\Local\\Porto\\apply-update.ps1',
    parentPid: 42,
    packagePath: 'C:\\Users\\test\\AppData\\Local\\Porto\\update.exe',
    destination: 'C:\\Users\\test\\AppData\\Local\\Programs\\Porto',
    executable: 'C:\\Users\\test\\AppData\\Local\\Programs\\Porto\\Porto.exe',
    errorFile: 'C:\\Users\\test\\AppData\\Local\\Porto\\update-error.txt',
    archiveRootName: '',
    readyFile: 'C:\\Users\\test\\AppData\\Local\\Porto\\apply-update.ready',
    proceedFile: 'C:\\Users\\test\\AppData\\Local\\Porto\\apply-update.proceed',
  })
  assert.equal(windows.command, 'powershell.exe')
  assert.deepEqual(windows.args.slice(0, 5), [
    '-NoProfile',
    '-NonInteractive',
    '-ExecutionPolicy',
    'Bypass',
    '-File',
  ])
  assert.ok(windows.args.includes(windows.packagePath))
  assert.ok(windows.args.includes(windows.destination))
  assert.ok(windows.args.includes(windows.readyFile))
  assert.ok(windows.args.includes(windows.proceedFile))
})

test('waits for the Windows update helper to become ready', async () => {
  const child = { exitCode: null }
  let checks = 0
  await waitForUpdateHelperReady(child, 'ready', {
    timeoutMs: 100,
    nowImpl: () => checks * 10,
    existsImpl: () => {
      checks += 1
      return checks === 3
    },
    delayImpl: async () => {},
  })
  assert.equal(checks, 3)
})

test('rejects a Windows update helper that exits or times out before readiness', async () => {
  await assert.rejects(
    waitForUpdateHelperReady({ exitCode: 7 }, 'ready', {
      existsImpl: () => false,
    }),
    /exited before becoming ready with code 7/i,
  )
  let now = 0
  await assert.rejects(
    waitForUpdateHelperReady({ exitCode: null }, 'ready', {
      timeoutMs: 100,
      nowImpl: () => now,
      existsImpl: () => false,
      delayImpl: async () => {
        now += 50
      },
    }),
    /did not become ready within 0.1s/i,
  )
  assert.equal(UPDATE_HELPER_READY_TIMEOUT, 15 * 1000)
})

test('surfaces a Windows update helper error before Porto exits', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-update-error-'))
  const errorFile = path.join(directory, 'install-error.txt')
  fs.writeFileSync(
    errorFile,
    'Stop running Porto VMs and Kubernetes clusters before updating.\n',
  )

  try {
    await assert.rejects(
      waitForUpdateHelperReady({ exitCode: null, signalCode: null }, 'ready', {
        errorFile,
        existsImpl: () => false,
      }),
      /Stop running Porto VMs and Kubernetes clusters before updating/,
    )
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('stages a detached installer helper beside a verified update', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-update-install-'))
  const installDirectory = path.join(directory, 'installed')
  const downloadsDirectory = path.join(directory, 'downloads')
  const executablePath = path.join(installDirectory, 'Porto')
  const packagePath = path.join(downloadsDirectory, 'porto-desktop_1.3.0_linux_amd64.tar.gz')
  const errorFile = path.join(directory, 'install-error.txt')
  fs.mkdirSync(installDirectory)
  fs.mkdirSync(downloadsDirectory)
  fs.writeFileSync(executablePath, '')
  fs.writeFileSync(packagePath, 'verified update')
  let invocation = null
  let unrefCalled = false

  try {
    const installation = await launchDownloadedUpdate({
      platform: 'linux',
      executablePath,
      packagePath,
      downloadsDirectory,
      errorFile,
      parentPid: 42,
      spawnImpl: (command, args, options) => {
        invocation = { command, args, options }
        const child = new EventEmitter()
        child.pid = 314
        child.unref = () => {
          unrefCalled = true
        }
        process.nextTick(() => child.emit('spawn'))
        return child
      },
    })

    assert.equal(installation.destination, installDirectory)
    assert.equal(installation.executable, executablePath)
    assert.equal(installation.helperPid, 314)
    assert.equal(invocation.command, '/bin/bash')
    assert.equal(invocation.options.detached, true)
    assert.equal(invocation.options.stdio, 'ignore')
    assert.equal(invocation.options.cwd, downloadsDirectory)
    assert.equal(unrefCalled, true)
    assert.equal(typeof installation.proceed, 'function')
    const helperPath = invocation.args[0]
    assert.equal(fs.existsSync(helperPath), true)
    if (process.platform !== 'win32') {
      assert.equal(fs.statSync(helperPath).mode & 0o700, 0o700)
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})
