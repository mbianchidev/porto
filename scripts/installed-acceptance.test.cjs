const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const net = require('node:net')
const path = require('node:path')
const test = require('node:test')
const { spawn } = require('node:child_process')
const { EventEmitter, once } = require('node:events')
const { PassThrough } = require('node:stream')

const {
  EXPECTED, available, digest, environment, inspectPackage, layout, load, nativeHeader,
  pluginPaths, prepare, preserved, reportedVersion, run, save, target, waitFor, workspacePath, write,
} = require('./installed-fixture.cjs')
const { existingDesktops, identity, restored } = require('./installed-acceptance.cjs')
const { releaseServer, uninstall } = require('./installed-package.cjs')

function directory(t) {
  const root = path.join(process.cwd(), `.installed-unit-${crypto.randomBytes(6).toString('hex')}`)
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  return root
}

function fixture(t) {
  return prepare(directory(t))
}

function header(goos, arch) {
  const bytes = Buffer.alloc(256)
  if (goos === 'linux') {
    bytes.set([0x7f, 0x45, 0x4c, 0x46, 2, 1])
    bytes.writeUInt16LE(arch === 'amd64' ? 62 : 183, 18)
  } else if (goos === 'darwin') {
    bytes.writeUInt32LE(0xfeedfacf)
    bytes.writeUInt32LE(arch === 'amd64' ? 0x1000007 : 0x100000c, 4)
  } else {
    bytes.write('MZ')
    bytes.writeUInt32LE(128, 60)
    bytes.write('PE\0\0', 128)
    bytes.writeUInt16LE(arch === 'amd64' ? 0x8664 : 0xaa64, 132)
  }
  return bytes
}

function packageFixture(fixture) {
  const result = layout(fixture.installRoot)
  const suffix = process.platform === 'win32' ? '.exe' : ''
  result.tools = {
    launcher: path.join(result.runtime, 'bin', `docker${suffix}`),
    compose: path.join(result.runtime, 'docker', 'cli-plugins', `docker-compose${suffix}`),
    buildx: path.join(result.runtime, 'docker', 'cli-plugins', `docker-buildx${suffix}`),
  }
  for (const file of [result.porto, result.desktop, result.limactl, ...Object.values(result.tools)]) {
    write(file, header(...fixture.target.split('/')), 0o700)
  }
  write(path.join(result.runtime, 'bin', 'porto-runtime-helper'), header('linux', fixture.target.split('/')[1]), 0o700)
  write(path.join(result.runtime, 'bin', `dive${suffix}`), 'synthetic layout only', 0o700)
  write(path.join(result.runtime, 'VERSIONS'), Object.entries(EXPECTED).map(([name, version]) => `${name} v${version}`)
    .concat(`wsl linux/${fixture.target.split('/')[1]}`).join('\n'))
  for (const name of ['docker-cli.txt', 'docker-cli-NOTICE.txt', 'docker-cli-bundled-plugins.patch',
    'docker-compose.txt', 'docker-compose-NOTICE.txt', 'docker-buildx.txt', 'docker-buildx-AUTHORS.txt', 'dive.txt']) {
    write(path.join(result.runtime, 'licenses', name), 'Synthetic layout fixture, never executed\n')
  }
  if (process.platform === 'win32') {
    for (const name of ['qemu-img.exe', fixture.target.endsWith('/arm64') ? 'qemu-system-aarch64.exe' : 'qemu-system-x86_64.exe']) {
      write(path.join(result.runtime, 'qemu', name), header(...fixture.target.split('/')), 0o700)
    }
    for (const file of ['bin/docker', 'bin/porto-wsl', 'docker/cli-plugins/docker-compose', 'docker/cli-plugins/docker-buildx']) {
      write(path.join(result.runtime, 'wsl', fixture.target.split('/')[1], file),
        header('linux', fixture.target.split('/')[1]), 0o700)
    }
  }
  return result
}

test('fixture creation refuses an existing HOME or state instead of adopting it', (t) => {
  const current = fixture(t)
  const sentinel = path.join(current.home, 'keep')
  write(sentinel, 'Unrelated existing fixture')
  assert.throws(() => prepare(current.directory), /EEXIST/)
  assert.equal(fs.readFileSync(sentinel, 'utf8'), 'Unrelated existing fixture')
})

test('workspace fencing rejects the checkout itself, its parent and escaping symlinks', (t) => {
  const root = directory(t)
  fs.mkdirSync(root)
  assert.throws(() => workspacePath(process.cwd()), /below the current checkout/)
  assert.throws(() => workspacePath(path.dirname(process.cwd())), /below the current checkout/)
  fs.symlinkSync(path.dirname(process.cwd()), path.join(root, 'outside'), process.platform === 'win32' ? 'junction' : 'dir')
  assert.throws(() => workspacePath(path.join(root, 'outside', 'synthetic')), /symlinks outside/)
})

test('persisted fixture cannot redirect HOME, installation or cleanup outside its root', (t) => {
  const current = fixture(t)
  assert.deepEqual(load(current.directory), current)
  current.installRoot = process.cwd()
  save(current)
  assert.throws(() => load(current.directory), /below the current checkout|Unsafe fixture/)
})

test('isolated environment drops credentials, external transports, source entries and native loaders', (t) => {
  const current = fixture(t)
  const env = environment(current, layout(current.installRoot), {
    PATH: '/synthetic-host-tools', HOME: '/real-user-home', USERPROFILE: '/real-profile',
    DOCKER_HOST: 'tcp://unrelated.invalid:2375', DOCKER_CONTEXT: 'real-context',
    LIMA_HOME: '/real-lima', PORTO_HOME: '/real-porto', PORTO_UI_DIR: '/source-dashboard',
    SSH_AUTH_SOCK: '/real-agent', GITHUB_TOKEN: 'synthetic-forbidden-token',
    NODE_OPTIONS: '--require=untrusted', DYLD_INSERT_LIBRARIES: '/untrusted.dylib',
    SSL_CERT_FILE: '/user-trust', AWS_ACCESS_KEY_ID: 'synthetic-forbidden-key',
  })
  for (const key of ['PORTO_UI_DIR', 'SSH_AUTH_SOCK', 'GITHUB_TOKEN', 'NODE_OPTIONS', 'DYLD_INSERT_LIBRARIES',
    'SSL_CERT_FILE', 'AWS_ACCESS_KEY_ID']) assert.equal(env[key], undefined, key)
  assert.equal(env.HOME, current.home)
  assert.equal(env.USERPROFILE, current.home)
  assert.equal(env.PORTO_HOME, current.portoHome)
  assert.equal(env.LIMA_HOME, current.limaHome)
  assert.equal(env.DOCKER_CONFIG, current.dockerConfig)
  assert.equal(env.DOCKER_CONTEXT, '')
  assert.equal(env.GIT_CEILING_DIRECTORIES, current.directory)
  assert.equal(env.GIT_CONFIG_GLOBAL, path.join(current.home, '.gitconfig'))
  assert.ok(env.DOCKER_HOST.startsWith(process.platform === 'win32' ? 'npipe:' : `unix://${current.portoHome}`))
  assert.equal(env.BUILDX_BUILDER, 'default')
  assert.ok(env.CONTAINERD_ADDRESS.startsWith(current.directory))
  assert.ok(env.BUILDKIT_HOST.startsWith(`unix://${current.directory}`))
  assert.equal(env.PATH.split(path.delimiter)[0], path.join(layout(current.installRoot).runtime, 'bin'))
})

test('synthetic project discovery cannot inherit the surrounding source Git repository', async (t) => {
  const current = fixture(t)
  const project = path.join(current.home, 'Synthetic project')
  fs.mkdirSync(project)
  const result = await run('git', ['rev-parse', '--show-toplevel'], {
    cwd: project, env: environment(current), allowFailure: true,
  })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /not a git repository/i)
})

test('all six native executable headers are checked, not inferred from archive names', (t) => {
  const root = directory(t)
  fs.mkdirSync(root)
  for (const goos of ['linux', 'darwin', 'windows']) {
    for (const arch of ['amd64', 'arm64']) {
      const file = path.join(root, `${goos}-${arch}`)
      write(file, header(goos, arch))
      assert.doesNotThrow(() => nativeHeader(file, `${goos}/${arch}`))
      assert.throws(() => nativeHeader(file, `${goos}/${arch === 'amd64' ? 'arm64' : 'amd64'}`), /must be a native/)
      write(file, 'not a native executable')
      assert.throws(() => nativeHeader(file, `${goos}/${arch}`), /must be a native/)
    }
  }
})

test('installed package inspection requires real Unicode paths, pinned tools and native executables', (t) => {
  const current = fixture(t)
  const installed = packageFixture(current)
  assert.equal(inspectPackage(current.installRoot, target()).porto, installed.porto)
  const otherArch = current.target.endsWith('/amd64') ? 'arm64' : 'amd64'
  assert.throws(() => inspectPackage(current.installRoot, `${current.target.split('/')[0]}/${otherArch}`), /Cross-built/)
  fs.unlinkSync(installed.tools.compose)
  assert.throws(() => inspectPackage(current.installRoot, target()), /missing executable compose/)
})

test('plugin selection rejects user/host fallback even when its version is correct', (t) => {
  const current = fixture(t)
  const installed = packageFixture(current)
  const info = {
    ClientInfo: {
      Plugins: ['compose', 'buildx'].map((name) => ({
        Name: name, Version: `v${EXPECTED[`docker-${name}`]}`, Path: installed.tools[name],
      })),
    },
  }
  assert.doesNotThrow(() => pluginPaths(info, installed.tools))
  const external = path.join(current.home, 'host-compose')
  write(external, 'Not the installed plugin')
  info.ClientInfo.Plugins[0].Path = external
  assert.throws(() => pluginPaths(info, installed.tools), /host\/user compose plugin/)
  assert.throws(() => pluginPaths({}, installed.tools), /omitted selected client plugin paths/)
})

test('reported versions must match exact release tokens, not longer versions or different builds', () => {
  for (const [output, expected] of [
    ['Docker version 29.7.2, build synthetic', '29.7.2'],
    ['v5.5.1', '5.5.1'],
    ['github.com/docker/buildx v0.37.1 synthetic', '0.37.1'],
  ]) assert.doesNotThrow(() => reportedVersion(output, expected))
  for (const output of ['Docker version 129.7.2', 'Docker version 29.7.21', 'Docker version 29.7.2-dev']) {
    assert.throws(() => reportedVersion(output, '29.7.2'), /exact pinned version/)
  }
})

test('preservation checks cover Porto data, Docker config/context/helper/plugin and Buildx sentinels', (t) => {
  const current = fixture(t)
  preserved(current)
  const names = Object.keys(current.sentinels)
  for (const fragment of ['project.txt', 'volume', 'config.json', 'contexts', 'credential-', 'cli-plugins', 'unrelated-builder-state']) {
    assert.ok(names.some((name) => name.includes(fragment)), fragment)
  }
  const victim = path.join(current.dockerConfig, 'config.json')
  write(victim, '{}')
  assert.throws(() => preserved(current), /changed unrelated or retained data/)
})

test('preservation refuses replacement of an unrelated helper with another file type', (t) => {
  const current = fixture(t)
  const helper = Object.keys(current.sentinels).find((name) => name.includes('credential-'))
  const file = path.join(current.directory, helper)
  fs.unlinkSync(file)
  fs.mkdirSync(file)
  assert.throws(() => preserved(current), /different file type/)
})

test('upgrade/uninstall checks hash populated state and guest data rather than file existence', async (t) => {
  const current = fixture(t)
  const guestData = path.join(current.limaHome, 'porto-engine', 'diffdisk')
  write(guestData, 'Synthetic retained disk contents')
  current.snapshots = { [path.relative(current.directory, guestData)]: digest(guestData) }
  await restored(current)
  write(guestData, 'Changed retained disk contents')
  await assert.rejects(restored(current), /changed stopped Porto state or real runtime data/)
})

test('preservation snapshots cannot read data outside their synthetic fixture', async (t) => {
  const current = fixture(t)
  current.snapshots = { [path.relative(current.directory, path.join(process.cwd(), 'go.mod'))]: 'not-a-real-hash' }
  await assert.rejects(restored(current), /must not escape/)
})

test('upgrade requires actual owned-executable replacement with the shipped bytes', async (t) => {
  const current = fixture(t)
  const installed = packageFixture(current)
  const data = path.join(current.portoHome, 'preservation', 'project.txt')
  current.snapshots = { [path.relative(current.directory, data)]: digest(data) }
  const files = [installed.porto, installed.desktop, ...Object.values(installed.tools)]
  current.toolSnapshot = Object.fromEntries(files.map((file) => {
    const stat = fs.statSync(file, { bigint: true })
    return [path.relative(installed.root, file), { sha256: digest(file), identity: `${stat.ino}/${stat.ctimeNs}` }]
  }))
  await assert.rejects(restored(current, { replaced: true }), /did not replace its owned executable/)
  for (const file of files) {
    const original = fs.readFileSync(file)
    fs.renameSync(file, `${file}.old`)
    write(file, original, 0o700)
  }
  await restored(current, { replaced: true })
  fs.appendFileSync(installed.tools.buildx, 'synthetic corruption')
  await assert.rejects(restored(current, { replaced: true }), /did not restore the shipped tool/)
})

test('an existing listener is refused without stopping it or using its backend', async (t) => {
  const listener = net.createServer((socket) => socket.end())
  await new Promise((resolve) => listener.listen(0, '127.0.0.1', resolve))
  t.after(() => new Promise((resolve) => listener.close(resolve)))
  const address = { host: '127.0.0.1', port: listener.address().port }
  await assert.rejects(available(address), /Refusing existing listener.*never reuse or stop/)
  assert.equal(listener.listening, true)
})

test('preflight recognizes existing desktops before installers can quit them', () => {
  assert.equal(existingDesktops(' 123 /Applications/Porto.app/Contents/MacOS/Porto --hidden\n', 'darwin'), true)
  assert.equal(existingDesktops(' 124 /synthetic/Porto installé 路径/Porto\n', 'linux'), true)
  assert.equal(existingDesktops([{ Name: 'Porto.exe', ProcessId: 125 }], 'win32'), true)
  assert.equal(existingDesktops(' 126 node scripts/installed-acceptance.cjs run .installed-linux\n', 'linux'), false)
  assert.equal(existingDesktops([{ Name: 'node.exe', ProcessId: 127 }], 'win32'), false)
})

test('missing commands and unsuccessful processes fail closed, never become a skipped acceptance', async () => {
  await assert.rejects(run(path.resolve('does-not-exist-installed-tool'), []), /ENOENT/)
  await assert.rejects(run(process.execPath, ['-e', 'console.error("synthetic tool failure"); process.exit(7)']),
    /failed.*7.*synthetic tool failure/s)
})

function alive(pid) {
  try { process.kill(pid, 0); return true } catch (error) {
    if (error.code === 'ESRCH') return false
    throw error
  }
}

async function running(pid, env) {
  if (!alive(pid)) return false
  if (process.platform === 'win32') return true
  const result = await run('/bin/ps', ['-p', String(pid), '-o', 'stat='], {
    env, allowFailure: true, timeout: 2000,
  })
  if (result.status === 1 && !result.stdout.trim() && !result.stderr.trim()) return false
  assert.equal(result.status, 0, result.stderr)
  return !result.stdout.trim().startsWith('Z')
}

async function stopSynthetic(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return
  const exited = once(child, 'exit')
  let timer
  try {
    child.kill('SIGKILL')
    await Promise.race([exited, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`Synthetic process ${child.pid} cleanup timed out`)), 3000)
    })])
  } finally {
    clearTimeout(timer)
  }
}

async function commandTree(t, mode) {
  const root = path.join(process.cwd(), `.installed-unit-${crypto.randomBytes(6).toString('hex')}`)
  const current = prepare(root)
  const env = environment(current)
  const script = path.join(current.home, 'owned-process-tree.cjs')
  const record = path.join(current.home, 'owned-pids.json')
  let unrelated
  t.after(async () => {
    try {
      if (fs.existsSync(record)) {
        const { pids } = JSON.parse(fs.readFileSync(record, 'utf8'))
        for (const pid of pids.reverse()) {
          const value = await identity(pid, env)
          if (value.includes(current.id)) {
            try { process.kill(pid, 'SIGKILL') } catch (error) {
              if (error.code !== 'ESRCH') throw error
            }
          }
        }
      }
    } finally {
      await stopSynthetic(unrelated)
      fs.rmSync(root, { recursive: true, force: true })
    }
  })
  write(script, `
const fs = require('node:fs')
const { spawn } = require('node:child_process')
const [level, mode, record, marker] = process.argv.slice(2)
setInterval(() => {}, 1000)
if (level === 'leaf') {
  process.send([process.pid])
} else {
  const child = spawn(process.execPath, [__filename, level === 'root' ? 'child' : 'leaf', mode, record, marker], {
    stdio: ['ignore', 'inherit', 'inherit', 'ipc'], windowsHide: true,
  })
  child.once('error', (error) => { throw error })
  child.once('message', (pids) => {
    pids.unshift(process.pid)
    if (level !== 'root') process.send(pids)
    else {
      fs.writeFileSync(record + '.next', JSON.stringify({ pids }))
      fs.renameSync(record + '.next', record)
      if (mode === 'stdout' || mode === 'stderr') process[mode].write(Buffer.alloc(32768, 120))
      if (mode === 'exited') process.exit(0)
    }
  })
}
`)
  unrelated = spawn(process.execPath, ['-e', 'process.send("ready"); setInterval(() => {}, 1000)'], {
    cwd: current.home, env, windowsHide: true, detached: process.platform !== 'win32',
    stdio: ['ignore', 'ignore', 'ignore', 'ipc'],
  })
  await once(unrelated, 'message')
  const timeout = process.platform === 'win32' ? 10000 : 2500
  const started = Date.now()
  let failure
  let deadline
  try {
    await Promise.race([
      run(process.execPath, [script, 'root', mode, record, current.id], {
        cwd: current.home, env, allowFailure: true, timeout, cleanupTimeout: 3000, maxBuffer: 4096,
      }),
      new Promise((_, reject) => {
        deadline = setTimeout(() => reject(new Error('The command regression did not settle')), timeout + 5000)
      }),
    ])
  } catch (error) {
    failure = error
  } finally {
    clearTimeout(deadline)
  }
  assert.ok(failure, 'A deadline or output limit must reject even with allowFailure')
  assert.equal(failure.code, mode === 'stdout' || mode === 'stderr' ? 'PORTO_COMMAND_OUTPUT_LIMIT' : 'PORTO_COMMAND_TIMEOUT')
  assert.match(failure.message, mode === 'stdout' || mode === 'stderr' ? /output limit exceeded/ : /timed out/)
  assert.ok(Date.now() - started < timeout + 5000, 'Command teardown exceeded its bounded cleanup budget')
  assert.ok(fs.existsSync(record), 'The complete synthetic child/grandchild tree must start before interruption')
  const { pids } = JSON.parse(fs.readFileSync(record, 'utf8'))
  assert.equal(pids.length, 3)
  await waitFor(async () => {
    const active = await Promise.all(pids.map((pid) => running(pid, env)))
    return active.every((value) => !value)
  }, 'owned command child and grandchild must be reaped', 5000)
  assert.equal(unrelated.exitCode, null, 'Cleanup terminated the unrelated synthetic process')
  assert.equal(unrelated.signalCode, null, 'Cleanup signaled the unrelated synthetic process')
  assert.equal(await running(unrelated.pid, env), true, 'Cleanup affected an unrelated process group')
}

test('command timeout reaps its owned child/grandchild tree without touching an unrelated process', async (t) => {
  await commandTree(t, 'timeout')
})

test('stdout and stderr caps reap the owned tree and cannot be masked by allowFailure', async (t) => {
  for (const stream of ['stdout', 'stderr']) await t.test(stream, async (t) => await commandTree(t, stream))
})

test('a successful wrapper exit cannot mask its output-limit failure', async () => {
  await assert.rejects(run(process.execPath, ['-e', 'process.stdout.write(Buffer.alloc(8192, 120))'], {
    maxBuffer: 4096, allowFailure: true,
  }), { code: 'PORTO_COMMAND_OUTPUT_LIMIT' })
})

if (process.platform !== 'win32') {
  test('Unix teardown reaps descendants retaining pipes after their wrapper has already exited', async (t) => {
    await commandTree(t, 'exited')
  })
}

test('cleanup itself is bounded when pipe close or the reaper never completes', async () => {
  for (const stalled of ['pipe-close', 'reaper']) {
    const child = Object.assign(new EventEmitter(), {
      pid: 123, stdout: new PassThrough(), stderr: new PassThrough(), unref() {},
    })
    const started = Date.now()
    let reaped
    await assert.rejects(run('synthetic-only', [], {
      timeout: 20, cleanupTimeout: 40, allowFailure: true, detached: process.platform === 'win32',
    }, {
      spawnImpl: (_file, _args, options) => {
        assert.equal(options.detached, process.platform !== 'win32')
        return child
      },
      terminateImpl: (owned) => {
        reaped = owned
        if (stalled === 'pipe-close') return Promise.resolve()
        child.emit('close', null, 'SIGKILL')
        return new Promise(() => {})
      },
    }), (error) => {
      assert.equal(error.code, 'PORTO_COMMAND_TIMEOUT')
      assert.match(error.message, /cleanup exceeded 40ms; exit could not be verified/)
      return true
    })
    assert.equal(reaped, child)
    assert.equal(child.stdout.destroyed, true)
    assert.equal(child.stderr.destroyed, true)
    assert.ok(Date.now() - started < 2000, 'The cleanup deadline did not settle the command')
  }
})

test('native identity distinguishes a live process from an exited or reused PID', async (t) => {
  const value = await identity(process.pid, process.env)
  assert.ok(value)
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' })
  t.after(() => stopSynthetic(child))
  const original = await identity(child.pid, process.env)
  assert.ok(original)
  await stopSynthetic(child)
  assert.notEqual(await identity(child.pid, process.env), original)
})

test('Unix identity accepts only the actual missing-PID status, not inspection failures', async () => {
  const missing = { status: 1, signal: null, stdout: '', stderr: '' }
  assert.equal(await identity(123, {}, { platform: 'linux', runImpl: async () => missing }), '')
  for (const result of [
    { ...missing, status: 2, stderr: 'ps inspection failed' },
    { ...missing, stderr: 'ps permission denied' },
    { ...missing, stdout: 'partial identity' },
    { ...missing, status: 0 },
    { ...missing, status: null, signal: 'SIGKILL' },
  ]) {
    const logs = []
    await assert.rejects(identity(123, {}, {
      platform: 'linux', runImpl: async () => result, logImpl: (message) => logs.push(message),
    }), { code: 'PORTO_PROCESS_INSPECTION_UNAVAILABLE' })
    assert.match(logs[0], /inspection unavailable for PID 123/)
  }
})

test('CIM identity requires an explicit matching missing-PID result and complete live identity', async () => {
  const result = (document) => ({ status: 0, signal: null, stdout: JSON.stringify(document), stderr: '' })
  const fields = { CreationDate: '2026-10-27T18:00:00Z', ExecutablePath: 'C:\\Synthetic\\porto.exe', CommandLine: 'porto.exe daemon start' }
  assert.equal(await identity(123, {}, {
    platform: 'win32', runImpl: async () => result({ pid: 123, exists: false }),
  }), '')
  assert.equal(await identity(123, {}, {
    platform: 'win32', runImpl: async () => result({ pid: 123, exists: true, identity: fields }),
  }), JSON.stringify(fields))
  for (const response of [
    { status: 1, signal: null, stdout: '', stderr: 'CIM service unavailable' },
    { status: 0, signal: null, stdout: '', stderr: '' },
    { status: 0, signal: null, stdout: '{}', stderr: 'Non-terminating CIM error' },
    result({ pid: 999, exists: false }),
    result({ pid: 123, exists: true, identity: { ...fields, ExecutablePath: '' } }),
  ]) {
    const logs = []
    await assert.rejects(identity(123, {}, {
      platform: 'win32', runImpl: async (_file, args) => {
        assert.match(args.at(-1), /\$ErrorActionPreference = 'Stop'/)
        assert.match(args.at(-1), /Get-CimInstance.*-ErrorAction Stop/)
        return response
      }, logImpl: (message) => logs.push(message),
    }), { code: 'PORTO_PROCESS_INSPECTION_UNAVAILABLE' })
    assert.match(logs[0], /inspection unavailable for PID 123/)
  }
})

test('identity inspection timeout remains an unavailable-inspection error rather than process exit', async () => {
  const timedOut = Object.assign(new Error('synthetic inspection timed out'), { code: 'PORTO_COMMAND_TIMEOUT' })
  const logs = []
  await assert.rejects(identity(123, {}, {
    runImpl: async () => { throw timedOut }, logImpl: (message) => logs.push(message),
  }), (error) => {
    assert.equal(error.code, 'PORTO_PROCESS_INSPECTION_UNAVAILABLE')
    assert.equal(error.cause, timedOut)
    return true
  })
  assert.match(logs[0], /inspection unavailable.*timed out/)
})

test('local release fixture serves exactly the built package and its real checksum', async (t) => {
  const root = directory(t)
  fs.mkdirSync(root)
  const artifact = path.join(root, 'Porto built package é.tar.gz')
  write(artifact, 'Synthetic HTTP fixture only, not executable acceptance')
  const release = await releaseServer(artifact)
  t.after(release.close)
  assert.equal(await (await fetch(`${release.url}/${encodeURI(path.basename(artifact))}`)).text(),
    fs.readFileSync(artifact, 'utf8'))
  assert.equal(await (await fetch(`${release.url}/SHA256SUMS`)).text(), `${digest(artifact)}  ${path.basename(artifact)}\n`)
  assert.equal((await fetch(`${release.url}/fixture.json`)).status, 404)
})

test('manual Unix app removal leaves populated synthetic state and unrelated command entries intact', (t) => {
  const current = fixture(t)
  packageFixture(current)
  const entry = path.join(current.home, '.local', 'share', 'applications', 'porto.desktop')
  write(entry, `[Desktop Entry]\nExec="${current.installRoot}/Porto"\n`)
  uninstall(current)
  assert.equal(fs.existsSync(current.installRoot), false)
  assert.equal(fs.existsSync(entry), false)
  preserved(current)
})

test('manual removal refuses a desktop entry pointing at an unrelated application', (t) => {
  const current = fixture(t)
  packageFixture(current)
  const entry = path.join(current.home, '.local', 'share', 'applications', 'porto.desktop')
  write(entry, '[Desktop Entry]\nExec="/unrelated/synthetic/application"\n')
  assert.throws(() => uninstall(current), /desktop entry not owned/)
  assert.equal(fs.existsSync(entry), true)
  assert.equal(fs.existsSync(current.installRoot), true)
})

test('the executable acceptance command refuses a non-CI developer environment', async () => {
  const result = await run(process.execPath, [path.join(__dirname, 'installed-acceptance.cjs'), 'prepare', '.must-not-be-created'], {
    env: { ...process.env, GITHUB_ACTIONS: '' }, allowFailure: true,
  })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /disposable CI hosts only, never a developer runtime/)
  assert.equal(fs.existsSync('.must-not-be-created'), false)
})

test('unknown acceptance commands do not execute a backend even in CI', async (t) => {
  const current = fixture(t)
  const result = await run(process.execPath, [
    path.join(__dirname, 'installed-acceptance.cjs'), 'unknown-acceptance-command', current.directory,
  ], { env: { ...process.env, GITHUB_ACTIONS: 'true' }, allowFailure: true })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /Unknown installed acceptance command/)
  preserved(current)
})
