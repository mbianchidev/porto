const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')
const vm = require('node:vm')

const {
  bundledExecutablePaths,
  daemonBinaryIdentity,
  daemonProcessIDs,
  daemonProcesses,
  dashboardLoadAction,
  dockerBootstrapCommand,
  inspectBundledDockerToolchain,
  inspectDaemon,
  installDockerEngine,
  installDockerContext,
  isDaemonReady,
  mergeExecutablePaths,
  prepareDockerEngineUpdate,
  resolvePackagedDashboard,
  resolvePortoBinary,
  resolveLoginShellPath,
  runtimeFailureDiagnostic,
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
    fetchImpl: async () => response({ status: 'ok', apiVersion: 30, dashboardReady: true }),
  })

  assert.equal(ready, true)
})

test('requires the packaged daemon binary identity to match', async () => {
  const matching = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({
      status: 'ok',
      apiVersion: 30,
      dashboardReady: true,
      daemonIdentity: 'new-binary',
    }),
  })
  const stale = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({
      status: 'ok',
      apiVersion: 30,
      dashboardReady: true,
      daemonIdentity: 'old-binary',
    }),
  })
  const legacy = await isDaemonReady({
    expectedDaemonIdentity: 'new-binary',
    fetchImpl: async () => response({ status: 'ok', apiVersion: 30, dashboardReady: true }),
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
    fetchImpl: async () => response({ status: 'ok', apiVersion: 30, dashboardReady: false }),
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

test('reconciles engine provisioning on every packaged desktop launch', () => {
  const unavailable = { enabled: true, available: false }

  assert.deepEqual(
    dockerBootstrapCommand(unavailable, { isPackaged: true }),
    ['docker', 'engine-install'],
  )
  assert.deepEqual(
    dockerBootstrapCommand({ enabled: true, available: true }, { isPackaged: true }),
    ['docker', 'engine-install'],
  )
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
  assert.equal(request.options.headers, undefined)
  assert.equal(status.available, true)
})

function engineEvent(type, value) {
  const key = type === 'complete' ? 'status' : type === 'error' ? 'error' : 'progress'
  return `${JSON.stringify({ type, [key]: value })}\n`
}

function engineProgressResponse(contents, { cancel = () => {} } = {}) {
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(typeof contents === 'string' ? new TextEncoder().encode(contents) : contents)
      controller.close()
    },
    cancel,
  }), { headers: { 'Content-Type': 'application/x-ndjson' } })
}

const guestDownloadProgress = {
  stage: 'downloading',
  component: 'guest-image',
  message: 'Downloading the required Lima guest image.',
}

test('streams actual provisioning progress before installation completes', async () => {
  let stream
  let request
  let completed = false
  let observedProgress
  const observed = new Promise((resolve) => { observedProgress = resolve })
  const result = installDockerEngine({
    fetchImpl: async (url, options) => {
      request = { url, options }
      return new Response(new ReadableStream({
        start(controller) { stream = controller },
      }), { headers: { 'Content-Type': 'application/x-ndjson; charset=utf-8' } })
    },
    onProgress: (progress) => observedProgress(progress),
  }).then((status) => {
    completed = true
    return status
  })
  await Promise.resolve()
  const progress = engineEvent('progress', guestDownloadProgress)
  stream.enqueue(new TextEncoder().encode(progress.slice(0, 27)))
  stream.enqueue(new TextEncoder().encode(progress.slice(27)))
  assert.deepEqual(await observed, guestDownloadProgress)
  assert.equal(completed, false)
  assert.equal(request.url, 'http://127.0.0.1:37623/api/docker/engine/install')
  assert.equal(request.options.headers.Accept, 'application/x-ndjson')
  stream.enqueue(new TextEncoder().encode(engineEvent('complete', { available: true, backend: 'fixture' })))
  stream.close()
  assert.deepEqual(await result, { available: true, backend: 'fixture' })
})

test('distinguishes actionable network, local-tool and backend failures without echoing raw diagnostics', async () => {
  for (const [code, guidance] of [
    ['network', /DNS, proxy settings and HTTPS trust/],
    ['local-tool', /runtime executable is missing or cannot run/],
    ['backend', /backend or Porto daemon is unavailable/],
    ['ownership', /Preserve existing instances and data/],
  ]) {
    await assert.rejects(installDockerEngine({
      fetchImpl: async () => engineProgressResponse(
        engineEvent('progress', guestDownloadProgress)
        + engineEvent('error', { code, message: 'https://user:super-secret@proxy.invalid/?token=hidden' }),
      ),
      onProgress: () => {},
    }), (error) => {
      assert.equal(error.code, code)
      assert.match(error.message, guidance)
      assert.doesNotMatch(error.message, /super-secret|proxy\.invalid|token=hidden/)
      return true
    })
  }
})

test('retains explicit cleanup guidance after a failed provisioning operation', async () => {
  await assert.rejects(installDockerEngine({
    fetchImpl: async () => engineProgressResponse(engineEvent('error', {
      code: 'network', message: 'download failed', cleanupRequired: true,
    })),
    onProgress: () => {},
  }), (error) => {
    assert.equal(error.code, 'network')
    assert.equal(error.cleanupRequired, true)
    assert.match(error.message, /Cleanup also needs attention/)
    return true
  })
})

test('rejects malformed, truncated and contradictory installation streams without falling back', async (t) => {
  const complete = engineEvent('complete', { available: true })
  for (const [name, contents] of [
    ['empty', ''],
    ['missing completion', engineEvent('progress', guestDownloadProgress)],
    ['truncated JSON', '{"type":"complete","status":'],
    ['truncated terminal line', complete.trimEnd()],
    ['invalid JSON', '{not-json}\n'],
    ['array event', '[]\n'],
    ['null event', 'null\n'],
    ['unknown event', '{"type":"unexpected"}\n'],
    ['invalid progress', engineEvent('progress', { stage: 'complete', component: 'lima', message: 'ready' })],
    ['empty progress', engineEvent('progress', { ...guestDownloadProgress, message: '' })],
    ['unknown error', engineEvent('error', { code: 'unknown', message: 'failure' })],
    ['malformed error', engineEvent('error', { code: 'network', message: 5 })],
    ['unavailable completion', engineEvent('complete', { available: false })],
    ['duplicate completion', complete + complete],
    ['progress after completion', complete + engineEvent('progress', guestDownloadProgress)],
    ['error after completion', complete + engineEvent('error', { code: 'network', message: 'failure' })],
    ['oversized event', engineEvent('progress', { ...guestDownloadProgress, message: 'x'.repeat(20000) })],
    ['invalid UTF-8', Uint8Array.from([0xff, 0x0a])],
  ]) {
    await t.test(name, async () => {
      let requests = 0
      await assert.rejects(installDockerEngine({
        fetchImpl: async () => {
          requests += 1
          return engineProgressResponse(contents)
        },
        onProgress: () => {},
      }), (error) => {
        assert.equal(error.code, 'transport')
        assert.match(error.message, /Setup has not been confirmed/)
        return true
      })
      assert.equal(requests, 1)
    })
  }
})

test('does not silently accept a legacy JSON response after requesting progress', async () => {
  let requests = 0
  await assert.rejects(installDockerEngine({
    fetchImpl: async () => {
      requests += 1
      return Response.json({ available: true })
    },
    onProgress: () => {},
  }), (error) => error.code === 'transport')
  assert.equal(requests, 1)
})

test('fails closed if the progress reader fails after a complete event', async () => {
  let reads = 0
  await assert.rejects(installDockerEngine({
    fetchImpl: async () => new Response(new ReadableStream({
      pull(controller) {
        if (reads++ === 0) {
          controller.enqueue(new TextEncoder().encode(engineEvent('complete', { available: true })))
        } else {
          controller.error(new Error('secret transport diagnostic'))
        }
      },
    }), { headers: { 'Content-Type': 'application/x-ndjson' } }),
    onProgress: () => {},
  }), (error) => {
    assert.equal(error.code, 'transport')
    assert.doesNotMatch(error.message, /secret/)
    return true
  })
})

test('cancels the request and its progress reader on timeout', async () => {
  let requestSignal
  let cancelled = false
  await assert.rejects(installDockerEngine({
    timeoutMs: 20,
    fetchImpl: async (_url, { signal }) => {
      requestSignal = signal
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(engineEvent('progress', guestDownloadProgress)))
        },
        cancel() { cancelled = true },
      }), { headers: { 'Content-Type': 'application/x-ndjson' } })
    },
    onProgress: () => {},
  }), (error) => error.code === 'cancelled')
  assert.equal(requestSignal.aborted, true)
  assert.equal(cancelled, true)
})

test('cancels provisioning when the bootstrap caller aborts', async () => {
  const controller = new AbortController()
  let requestSignal
  let cancelled = false
  await assert.rejects(installDockerEngine({
    signal: controller.signal,
    fetchImpl: async (_url, { signal }) => {
      requestSignal = signal
      return new Response(new ReadableStream({
        start(stream) {
          stream.enqueue(new TextEncoder().encode(engineEvent('progress', guestDownloadProgress)))
        },
        cancel() { cancelled = true },
      }), { headers: { 'Content-Type': 'application/x-ndjson' } })
    },
    onProgress: () => controller.abort(),
  }), (error) => error.code === 'cancelled')
  assert.equal(requestSignal.aborted, true)
  assert.equal(cancelled, true)
})

test('does not start provisioning for an already-aborted bootstrap caller', async () => {
  const controller = new AbortController()
  controller.abort()
  await assert.rejects(installDockerEngine({
    signal: controller.signal,
    fetchImpl: async () => assert.fail('an aborted caller must not start installation'),
    onProgress: () => {},
  }), (error) => error.code === 'cancelled')
})

test('aborts rather than hiding a failed progress observer', async () => {
  let requestSignal
  let cancelled = false
  await assert.rejects(installDockerEngine({
    fetchImpl: async (_url, { signal }) => {
      requestSignal = signal
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(engineEvent('progress', guestDownloadProgress)))
        },
        cancel() { cancelled = true },
      }), { headers: { 'Content-Type': 'application/x-ndjson' } })
    },
    onProgress: () => { throw new Error('secret observer failure') },
  }), (error) => {
    assert.equal(error.code, 'transport')
    assert.doesNotMatch(error.message, /secret/)
    return true
  })
  assert.equal(requestSignal.aborted, true)
  assert.equal(cancelled, true)
})

test('reports an unreachable provisioning daemon separately from required downloads', async () => {
  await assert.rejects(installDockerEngine({
    fetchImpl: async () => { throw new TypeError('fetch failed') },
    onProgress: () => {},
  }), (error) => error.code === 'backend')
})

test('preserves useful runtime diagnostics while removing secrets and entire URLs', () => {
  const error = new Error('synthetic Buildx check failed: EACCES; password=secret-private-value\n'
    + 'Details: {"token":"json-private-value"}\n'
    + 'Proxy check failed: authorization: Bearer bearer-private-value\n'
    + 'download=https://download.invalid/tool?signature=url-private-value\n'
    + 'proxy=socks5://proxy.invalid:1080\n'
    + '-----BEGIN PRIVATE KEY-----\nprivate-key-value\n-----END PRIVATE KEY-----')
  const message = runtimeFailureDiagnostic(error)
  assert.match(message, /synthetic Buildx check failed: EACCES/)
  for (const secret of ['secret-private-value', 'json-private-value', 'bearer-private-value', 'url-private-value', 'private-key-value', 'download.invalid', 'proxy.invalid', 'https://', 'socks5://']) {
    assert.equal(message.includes(secret), false, `diagnostics exposed ${secret}`)
  }
})

test('malformed escaped diagnostics cannot cause exponential backtracking', () => {
  const { spawnSync } = require('node:child_process')
  const result = spawnSync(process.execPath, ['-e', `
    const input = require('node:fs').readFileSync(0, 'utf8')
    require(process.argv[1]).runtimeFailureDiagnostic(new Error(input))
    process.stdout.write('completed')
  `, require.resolve('./daemon-readiness.cjs')], {
    input: '"auth":"' + '\\!'.repeat(20000),
    encoding: 'utf8',
    timeout: 5000,
    shell: false,
  })
  assert.ifError(result.error)
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, 'completed')
})

test('logs the sanitized bundled-toolchain root cause before returning the safe bootstrap error', async () => {
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8')
  const start = source.indexOf('async function ensureDockerEngine(')
  const end = source.indexOf('\nfunction createBootstrapWindow(', start)
  assert.ok(start !== -1 && end > start)
  const entries = []
  const ensureDockerEngine = vm.runInNewContext(`(${source.slice(start, end)})`, {
    app: { isPackaged: true },
    process: { resourcesPath: '/synthetic/runtime' },
    portoEnvironment: async () => ({}),
    inspectBundledDockerToolchain: async () => {
      throw new Error('Bundled Docker compose check failed: synthetic executable permission denied; token=tool-private-value; https://tool.invalid/download')
    },
    runtimeFailureDiagnostic,
    console: {
      debug() {},
      error: (...args) => entries.push(args),
    },
  })
  await assert.rejects(ensureDockerEngine(), (error) => {
    assert.match(error.message, /A bundled local runtime executable is missing or cannot run/)
    assert.doesNotMatch(error.message, /synthetic executable|tool-private-value|tool\.invalid/)
    assert.equal(entries.length, 1, 'root cause must be logged before the error reaches the caller')
    return true
  })
  assert.match(entries[0][1], /Bundled Docker compose check failed: synthetic executable permission denied/)
  assert.doesNotMatch(entries[0][1], /tool-private-value|tool\.invalid|https:\/\//)
  assert.equal(entries[0].some((entry) => entry instanceof Error), false)
})

test('prepares the owned engine before a Windows desktop update', async () => {
  let request = null
  const result = await prepareDockerEngineUpdate({
    fetchImpl: async (url, options) => {
      request = { url, options }
      return response({ prepared: true, engineStopped: true })
    },
  })

  assert.equal(request.url, 'http://127.0.0.1:37623/api/docker/engine/prepare-update')
  assert.equal(request.options.method, 'POST')
  assert.equal(result.prepared, true)
  assert.equal(result.engineStopped, true)
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

test('validates bundled Docker plugins from paths with spaces and non-ASCII characters', async () => {
  const resourcesPath = path.join('/Applications', 'Porto ü.app', 'Contents', 'Resources')
  const runtimeRoot = path.join(resourcesPath, 'runtime')
  const existing = new Set([
    path.join(runtimeRoot, 'VERSIONS'),
    path.join(runtimeRoot, 'bin', 'docker'),
    path.join(runtimeRoot, 'bin', 'dive'),
    path.join(runtimeRoot, 'docker', 'cli-plugins', 'docker-compose'),
    path.join(runtimeRoot, 'docker', 'cli-plugins', 'docker-buildx'),
  ])
  const calls = []
  const status = await inspectBundledDockerToolchain({
    resourcesPath,
    platform: 'darwin',
    environment: { PATH: '/clean/path' },
    existsImpl: (file) => existing.has(file),
    readFileImpl: () => [
      'docker 29.7.2',
      'docker-compose v5.5.1 (asset)',
      'docker-buildx v0.37.1 (asset)',
      'dive v0.13.1 (asset)',
    ].join('\n'),
    execFileImpl: async (file, args, options) => {
      calls.push({ file, args, options })
      const command = args.join(' ')
      if (file.endsWith('dive')) return { stdout: 'dive 0.13.1', stderr: '' }
      if (command === '--version') return { stdout: 'Docker version 29.7.2', stderr: '' }
      if (command === 'compose version --short') return { stdout: '5.5.1', stderr: '' }
      if (command === 'buildx version') return { stdout: 'github.com/docker/buildx v0.37.1', stderr: '' }
      throw new Error(`unexpected command ${command}`)
    },
  })

  assert.equal(status.supported, true)
  assert.equal(calls.length, 4)
  assert.equal(calls[0].file, path.join(runtimeRoot, 'bin', 'docker'))
  assert.equal(calls[0].options.env.PATH, '/clean/path')
})

test('rejects a packaged Docker toolchain with a missing plugin', async () => {
  const resourcesPath = '/Applications/Porto.app/Contents/Resources'
  await assert.rejects(
    inspectBundledDockerToolchain({
      resourcesPath,
      platform: 'darwin',
      existsImpl: (file) => !file.endsWith('docker-buildx'),
      readFileImpl: () => 'docker 29.7.2\ndocker-compose v5.5.1\ndocker-buildx v0.37.1\n',
    }),
    /missing .*docker-buildx/,
  )
})

test('validates the source-built Windows ARM64 Docker toolchain', async () => {
  const commands = []
  const status = await inspectBundledDockerToolchain({
    resourcesPath: 'C:\\Program Files\\Porto\\resources',
    platform: 'win32',
    existsImpl: () => true,
    readFileImpl: () => [
      'docker 29.7.2 (source)',
      'docker-compose v5.5.1 (asset)',
      'docker-buildx v0.37.1 (asset)',
      'dive v0.13.1 (asset)',
    ].join('\n'),
    execFileImpl: async (file, args) => {
      commands.push(args.join(' '))
      if (file.toLowerCase().endsWith('dive.exe')) return { stdout: 'dive 0.13.1', stderr: '' }
      if (args[0] === '--version') return { stdout: 'Docker version 29.7.2', stderr: '' }
      if (args[0] === 'compose') return { stdout: '5.5.1', stderr: '' }
      return { stdout: 'github.com/docker/buildx v0.37.1', stderr: '' }
    },
  })
  assert.equal(status.supported, true)
  assert.deepEqual(commands, ['--version', 'compose version --short', 'buildx version', '--version'])
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
