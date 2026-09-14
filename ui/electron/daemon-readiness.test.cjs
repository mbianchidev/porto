const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const path = require('node:path')
const test = require('node:test')

const {
  bundledExecutablePaths,
  daemonBinaryIdentity,
  daemonProcessIDs,
  daemonProcesses,
  dashboardLoadAction,
  dockerBootstrapCommand,
  inspectDaemon,
  installDockerEngine,
  installDockerContext,
  isDaemonReady,
  mergeExecutablePaths,
  resolvePackagedDashboard,
  resolvePortoBinary,
  resolveLoginShellPath,
  windowsDaemonProcessIDs,
  windowsDaemonProcesses,
  waitForDockerEngine,
} = require('./daemon-readiness.cjs')

function response(body, ok = true) {
  return {
    ok,
    async json() {
      return body
    },
  }
}

test('accepts a compatible daemon with a dashboard', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 29, dashboardReady: true }),
  })

  assert.equal(ready, true)
})

test('requires the packaged daemon binary identity to match', async () => {
  const matching = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({
      status: 'ok',
      apiVersion: 29,
      dashboardReady: true,
      daemonIdentity: 'new-binary',
    }),
  })
  const stale = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({
      status: 'ok',
      apiVersion: 29,
      dashboardReady: true,
      daemonIdentity: 'old-binary',
    }),
  })
  const legacy = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({ status: 'ok', apiVersion: 29, dashboardReady: true }),
  })

  assert.equal(matching, true)
  assert.equal(stale, false)
  assert.equal(legacy, false)
})

test('hashes the bundled daemon binary for readiness matching', () => {
  const contents = Buffer.from('porto-daemon')
  const identity = daemonBinaryIdentity('/bundle/porto', {
    readFileImpl: (file) => {
      assert.equal(file, '/bundle/porto')
      return contents
    },
  })

  assert.equal(identity, crypto.createHash('sha256').update(contents).digest('hex'))
})

test('rejects an older daemon without dashboard readiness metadata', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 2 }),
  })

  assert.equal(ready, false)
})

test('rejects a daemon that cannot serve the dashboard', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 29, dashboardReady: false }),
  })

  assert.equal(ready, false)
})

test('rejects an incompatible API', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 28, dashboardReady: true }),
  })

  assert.equal(ready, false)
})

test('reloads one blank dashboard without cache before showing an error', () => {
  assert.equal(dashboardLoadAction({ rootChildren: 1, reloadAttempts: 0 }), 'ready')
  assert.equal(dashboardLoadAction({ rootChildren: 0, reloadAttempts: 0 }), 'reload')
  assert.equal(dashboardLoadAction({ rootChildren: 0, reloadAttempts: 1 }), 'error')
})

test('reports an incompatible daemon as reachable but not ready', async () => {
  const result = await inspectDaemon({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 1 }),
  })

  assert.equal(result.reachable, true)
  assert.equal(result.ready, false)
})

test('finds only exact Porto daemon processes', () => {
  const processList = `
  120 /Users/test/Applications/Porto.app/Contents/Resources/porto daemon start
  121 /Applications/Porto.app/Contents/MacOS/Porto
  122 /usr/local/bin/porto daemon status
  123 /tmp/porto daemon start --debug
  124 /Applications/My Tools/porto daemon start
`

  assert.deepEqual(daemonProcessIDs(processList), [120, 124])
  assert.deepEqual(daemonProcesses(processList), [
    { pid: 120, executable: '/Users/test/Applications/Porto.app/Contents/Resources/porto' },
    { pid: 124, executable: '/Applications/My Tools/porto' },
  ])
})

test('finds Windows Porto daemon processes', () => {
  const processes = [
    { ProcessId: 220, CommandLine: '"C:\\Program Files\\Porto\\porto.exe" daemon start' },
    { ProcessId: 221, CommandLine: '"C:\\Program Files\\Porto\\porto.exe" daemon status' },
    { ProcessId: 222, CommandLine: null },
  ]

  assert.deepEqual(windowsDaemonProcessIDs(processes), [220])
  assert.deepEqual(windowsDaemonProcesses(processes), [
    { pid: 220, executable: 'C:\\Program Files\\Porto\\porto.exe' },
  ])
})

test('bootstraps the bundled engine for packaged desktops', () => {
  const unavailable = { enabled: true, available: false }

  assert.deepEqual(
    dockerBootstrapCommand(unavailable, { isPackaged: true }),
    ['docker', 'engine-install'],
  )
  assert.equal(dockerBootstrapCommand({ enabled: true, available: true }, { isPackaged: true }), null)
  assert.equal(dockerBootstrapCommand({ enabled: false, available: false }, { isPackaged: true }), null)
  assert.equal(dockerBootstrapCommand(unavailable, { isPackaged: false }), null)
})

test('installs the engine through the active daemon', async () => {
  let request = null
  const status = await installDockerEngine({
    fetchImpl: async (url, options) => {
      request = { url, options }
      return response({ available: true })
    },
  })

  assert.equal(request.url, 'http://127.0.0.1:37623/api/docker/engine/install')
  assert.equal(request.options.method, 'POST')
  assert.equal(status.available, true)
})

test('waits for a fresh inventory beyond the old 15-second startup window', async () => {
  let elapsed = 0
  let requests = 0
  const status = await waitForDockerEngine({
    fetchImpl: async () => {
      requests += 1
      return response({
        enabled: true,
        available: requests === 40,
        message: 'Connecting to containerd',
      })
    },
    nowImpl: () => elapsed,
    delayImpl: async (milliseconds) => { elapsed += milliseconds },
  })

  assert.equal(status.available, true)
  assert.equal(requests, 40)
  assert.ok(elapsed > 15000)
})

test('bounds the engine readiness wait and retains the latest diagnostic', async () => {
  let elapsed = 0
  await assert.rejects(waitForDockerEngine({
    timeoutMs: 2000,
    fetchImpl: async () => response({
      enabled: true,
      available: false,
      message: 'containerd guest helper failed',
    }),
    nowImpl: () => elapsed,
    delayImpl: async (milliseconds) => { elapsed += milliseconds },
  }), /containerd guest helper failed/)
  assert.equal(elapsed, 2000)
})

test('finishes readiness immediately if the runtime was disabled', async () => {
  const status = await waitForDockerEngine({
    fetchImpl: async () => response({ enabled: false, available: false }),
    delayImpl: async () => assert.fail('disabled runtime must not be polled'),
  })
  assert.equal(status.enabled, false)
})

test('installs the Docker context through the active daemon', async () => {
  let request = null
  const context = await installDockerContext({
    fetchImpl: async (url, options) => {
      request = { url, options }
      return response({ context: 'porto' })
    },
  })

  assert.equal(request.url, 'http://127.0.0.1:37623/api/docker/context/install')
  assert.equal(request.options.method, 'POST')
  assert.equal(context.context, 'porto')
})

test('reads the executable path from the user login shell', async () => {
  let invocation = null
  const resolved = await resolveLoginShellPath({
    platform: 'darwin',
    environment: { HOME: '/Users/test', SHELL: '/bin/zsh', PATH: '/usr/bin:/bin' },
    execFileImpl: async (command, args, options) => {
      invocation = { command, args, options }
      return { stdout: 'shell startup output\n__PORTO_PATH__/opt/homebrew/bin:/usr/bin:/bin\n' }
    },
  })

  assert.equal(resolved, '/opt/homebrew/bin:/usr/bin:/bin')
  assert.equal(invocation.command, '/bin/zsh')
  assert.deepEqual(invocation.args, ['-ilc', 'printf "\\n__PORTO_PATH__%s\\n" "$PATH"'])
  assert.equal(invocation.options.env.HOME, '/Users/test')
})

test('falls back when the user login shell path is unavailable', async () => {
  const resolved = await resolveLoginShellPath({
    platform: 'darwin',
    environment: { SHELL: '/bin/zsh' },
    execFileImpl: async () => {
      throw new Error('shell failed')
    },
  })

  assert.equal(resolved, '')
})

test('merges bundled, login-shell, and inherited executable paths once', () => {
  assert.equal(
    mergeExecutablePaths([
      '/Applications/Porto.app/Contents/Resources/runtime/bin',
      '/opt/homebrew/bin:/usr/bin:/bin',
      '/usr/bin:/bin:/usr/sbin:/sbin',
    ], ':'),
    '/Applications/Porto.app/Contents/Resources/runtime/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin',
  )
})

test('resolves every packaged runtime executable directory that exists', () => {
  const resourcesPath = path.join(path.sep, 'Applications', 'Porto', 'resources')
  const qemuPath = path.join(resourcesPath, 'runtime', 'qemu')
  const expected = [
    path.join(resourcesPath, 'runtime', 'bin'),
    path.join(resourcesPath, 'runtime', 'lima', 'bin'),
    qemuPath,
  ]

  assert.deepEqual(bundledExecutablePaths(resourcesPath, {
    existsImpl: () => true,
  }), expected)
  assert.deepEqual(bundledExecutablePaths(resourcesPath, {
    existsImpl: (candidate) => candidate !== qemuPath,
  }), expected.slice(0, 2))
})

test('resolves the packaged dashboard beside the bundled daemon', () => {
  const resourcesPath = '/Applications/Porto.app/Contents/Resources'
  const dashboard = resolvePackagedDashboard({
    isPackaged: true,
    resourcesPath,
    existsImpl: (candidate) => candidate === `${resourcesPath}/dist/index.html`,
  })

  assert.equal(dashboard, `${resourcesPath}/dist`)
  assert.equal(resolvePackagedDashboard({ isPackaged: false, resourcesPath }), '')
})

test('packaged apps always use the bundled Porto binary', () => {
  const resolved = resolvePortoBinary({
    isPackaged: true,
    platform: 'darwin',
    resourcesPath: '/Applications/Porto.app/Contents/Resources',
    environment: { PORTO_BINARY: '/tmp/old-porto' },
    existsImpl: () => false,
  })

  assert.equal(resolved, '/Applications/Porto.app/Contents/Resources/porto')
})
