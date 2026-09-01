const assert = require('node:assert/strict')
const test = require('node:test')

const {
  daemonProcessIDs,
  daemonProcesses,
  dockerBootstrapCommand,
  inspectDaemon,
  installDockerEngine,
  isDaemonReady,
  mergeExecutablePaths,
  resolvePortoBinary,
  resolveLoginShellPath,
  windowsDaemonProcessIDs,
  windowsDaemonProcesses,
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
    fetchImpl: async () => response({ status: 'ok', apiVersion: 10, dashboardReady: true }),
  })

  assert.equal(ready, true)
})

test('rejects an older daemon without dashboard readiness metadata', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 2 }),
  })

  assert.equal(ready, false)
})

test('rejects a daemon that cannot serve the dashboard', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 10, dashboardReady: false }),
  })

  assert.equal(ready, false)
})

test('rejects an incompatible API', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 9, dashboardReady: true }),
  })

  assert.equal(ready, false)
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

test('bootstraps the bundled engine only for packaged supported desktops', () => {
  const unavailable = { enabled: true, available: false }

  assert.deepEqual(
    dockerBootstrapCommand(unavailable, { isPackaged: true, platform: 'darwin' }),
    ['docker', 'engine-install'],
  )
  assert.equal(dockerBootstrapCommand({ enabled: true, available: true }, { isPackaged: true, platform: 'darwin' }), null)
  assert.equal(dockerBootstrapCommand({ enabled: false, available: false }, { isPackaged: true, platform: 'darwin' }), null)
  assert.equal(dockerBootstrapCommand(unavailable, { isPackaged: false, platform: 'darwin' }), null)
  assert.equal(dockerBootstrapCommand(unavailable, { isPackaged: true, platform: 'win32' }), null)
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
