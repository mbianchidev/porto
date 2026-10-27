const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { spawn } = require('node:child_process')

const {
  DAEMON_URL, EXPECTED, digest, environment, failureDiagnostic, fileDigest, inspectPackage, layout, load,
  noListeners, pluginPaths, prepare, preserved, reportedVersion, run, save, target, waitFor, within, workspacePath, write,
} = require('./installed-fixture.cjs')
const { daemonProcesses } = require('../ui/electron/daemon-readiness.cjs')
const { dependencyProvisionOverride } = require('./lima-containerd-archive.cjs')

async function hostProcesses(env, { platform = process.platform, runImpl = run } = {}) {
  if (platform !== 'win32') {
    return (await runImpl('/bin/ps', ['-ax', '-o', 'pid=,command='], { env })).stdout
  }
  const result = await runImpl('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$result = @()
foreach ($target in [System.Diagnostics.Process]::GetProcesses()) {
  try {
    if ($target.ProcessName -ine 'porto') { continue }
    try {
      $executable = $target.MainModule.FileName
      if ($target.HasExited) { continue }
      if ([System.IO.Path]::GetFileName($executable) -ine 'porto.exe') { throw 'Process executable changed during inspection' }
      $result += @{ ProcessId = $target.Id; Name = 'Porto.exe'; ExecutablePath = $executable }
    } catch {
      if (-not $target.HasExited) { throw }
    }
  } finally {
    $target.Dispose()
  }
}
ConvertTo-Json -InputObject @($result) -Compress
`], { env, timeout: 10000 })
  const processes = JSON.parse(result.stdout)
  daemonCandidates(processes, platform)
  return processes
}

function existingDesktops(processes, platform = process.platform) {
  if (platform === 'win32') {
    return (Array.isArray(processes) ? processes : [processes]).some((item) => /^Porto\.exe$/i.test(item?.Name || ''))
  }
  return processes.split(/\r?\n/).some((line) => /^\s*\d+\s+.*\/Porto(?:\.app\/Contents\/MacOS\/Porto)?(?:\s|$)/.test(line))
}

async function daemons(env) {
  return daemonCandidates(await hostProcesses(env))
}

function daemonCandidates(processes, platform = process.platform) {
  if (platform !== 'win32') return daemonProcesses(processes)
  assert.ok(Array.isArray(processes), 'Windows process enumeration must return an explicit array')
  return processes.map((item) => {
    assert.ok(Number.isSafeInteger(item.ProcessId) && item.ProcessId > 0, 'Windows process enumeration omitted a specific PID')
    assert.ok(typeof item.ExecutablePath === 'string' && path.win32.basename(item.ExecutablePath).toLowerCase() === 'porto.exe',
      'Windows process enumeration omitted a verifiable Porto executable')
    assert.equal(item.Name, 'Porto.exe', 'Windows process enumeration returned an unexpected process')
    return { pid: item.ProcessId, executable: item.ExecutablePath }
  })
}

async function preflight(fixture, env) {
  if (process.platform !== 'win32') {
    assert.notEqual(process.getuid(), 0, 'Installed Lima/desktop acceptance needs an unprivileged disposable CI user')
  }
  await noListeners(fixture)
  const processes = await hostProcesses(env)
  assert.equal(existingDesktops(processes), false, 'Refusing an existing Porto desktop; never quit unrelated work')
  assert.deepEqual(daemonCandidates(processes), [],
    'Refusing existing Porto daemon processes; the desktop must never stop unrelated work')
}

function commandPath(name, env) {
  for (const directory of env.PATH.split(path.delimiter)) {
    const candidate = path.join(directory, name + (process.platform === 'win32' ? '.exe' : ''))
    try {
      fs.accessSync(candidate, process.platform === 'win32' ? fs.constants.F_OK : fs.constants.X_OK)
      return candidate
    } catch (error) {
      if (!['ENOENT', 'EACCES', 'ENOTDIR'].includes(error.code)) throw error
    }
  }
  throw new Error(`Required runtime executable ${name} is missing; installed acceptance cannot skip`)
}

async function configureLima(fixture, env) {
  if (fixture.backend === 'default') return
  assert.equal(fixture.backend, 'qemu-tcg')
  const arch = fixture.target.split('/')[1]
  const qemu = commandPath(arch === 'amd64' ? 'qemu-system-x86_64' : 'qemu-system-aarch64', env)
  await run(qemu, ['--version'], { env })
  if (fixture.target.startsWith('windows/')) {
    assert.ok(qemu.startsWith(path.join(layout(fixture.installRoot).runtime, 'qemu') + path.sep),
      'Windows acceptance must use the installed QEMU, not another installation')
  }
  const quotedQemu = `'${qemu.replaceAll('\\', '/').replaceAll("'", "'\\''")}'`
  env[arch === 'amd64' ? 'QEMU_SYSTEM_X86_64' : 'QEMU_SYSTEM_AARCH64'] = arch === 'arm64'
    ? `${quotedQemu} -machine virt,accel=tcg -cpu neoverse-n1`
    : `${quotedQemu} -machine q35,vmport=off -accel tcg,thread=multi,tb-size=512 -global ICH9-LPC.disable_s3=1 -global ICH9-LPC.disable_s4=1 -cpu max,-avx512vl,-pdpe1gb`
  if (fixture.snapshots) return
  const archive = await run(process.execPath, [
    path.join(__dirname, 'lima-containerd-archive.cjs'), arch, path.join(fixture.directory, 'containerd-bootstrap'),
  ], { env, timeout: 8 * 60 * 1000 })
  const prefix = '.containerd.archives = '
  assert.ok(archive.stdout.trim().startsWith(prefix), 'Expected the existing checksum-verified Lima archive fixture')
  const archives = JSON.parse(archive.stdout.trim().slice(prefix.length))
  const dependencies = JSON.parse(dependencyProvisionOverride().slice('.provision += '.length))
  write(path.join(fixture.limaHome, '_config', 'override.yaml'), JSON.stringify({
    vmType: 'qemu', arch: arch === 'amd64' ? 'x86_64' : 'aarch64',
    cpus: 2, memory: '3GiB', disk: '20GiB',
    ssh: { loadDotSSHPubKeys: false, forwardAgent: false, forwardX11: false },
    propagateProxyEnv: false,
    containerd: { archives },
    provision: dependencies,
    ...(arch === 'arm64' ? { vmOpts: { qemu: { cpuType: { aarch64: 'neoverse-n1' } } } } : {}),
  }))
}

async function cdp(url) {
  const socket = new WebSocket(url)
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => { socket.close(); reject(new Error('Installed desktop debugger did not open')) }, 10000)
    socket.addEventListener('open', () => { clearTimeout(timer); resolve() }, { once: true })
    socket.addEventListener('error', () => { clearTimeout(timer); reject(new Error('Installed desktop debugger failed')) }, { once: true })
  })
  let sequence = 0
  const pending = new Map()
  socket.addEventListener('message', ({ data }) => {
    const response = JSON.parse(data)
    if (!pending.has(response.id)) return
    const { resolve, reject, timer } = pending.get(response.id)
    pending.delete(response.id)
    clearTimeout(timer)
    if (response.error) reject(new Error(JSON.stringify(response.error)))
    else resolve(response.result)
  })
  const closePending = () => {
    for (const { reject, timer } of pending.values()) {
      clearTimeout(timer)
      reject(new Error('Installed desktop debugger closed'))
    }
    pending.clear()
  }
  socket.addEventListener('close', closePending)
  const send = async (method, params = {}) => await new Promise((resolve, reject) => {
    const id = ++sequence
    const timer = setTimeout(() => {
      pending.delete(id)
      reject(new Error(`Installed desktop debugger timed out: ${method}`))
    }, 30000)
    pending.set(id, { resolve, reject, timer })
    socket.send(JSON.stringify({ id, method, params }))
  })
  return {
    async evaluate(expression) {
      const result = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })
      assert.equal(result.exceptionDetails, undefined, JSON.stringify(result.exceptionDetails))
      return result.result.value
    },
    close() { closePending(); socket.close() },
  }
}

async function json(url) {
  const response = await fetch(url, { signal: AbortSignal.timeout(10000) })
  assert.ok(response.ok, `${url}: HTTP ${response.status}`)
  return await response.json()
}

async function rendererRequest(renderer, endpoint, method = 'GET') {
  assert.ok(endpoint.startsWith('/api/docker/'), 'Only synthetic Docker operations belong in installed acceptance')
  return await renderer.evaluate(`(async () => {
    const response = await fetch(${JSON.stringify(endpoint)}, { method: ${JSON.stringify(method)} })
    return { status: response.status, body: await response.text() }
  })()`)
}

async function identity(pid, env, {
  platform = process.platform, runImpl = run, logImpl = console.error,
} = {}) {
  try {
    assert.ok(Number.isSafeInteger(pid) && pid > 0, 'Process inspection requires a specific positive PID')
    const command = platform === 'win32'
      ? ['powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
try {
  $target = [System.Diagnostics.Process]::GetProcessById(${pid})
} catch [System.ArgumentException] {
  @{ pid = ${pid}; exists = $false } | ConvertTo-Json -Compress
  exit 0
}
try {
  try {
    $start = $target.StartTime.ToUniversalTime().Ticks.ToString([System.Globalization.CultureInfo]::InvariantCulture)
    $executable = $target.MainModule.FileName
    if ($target.HasExited) {
      @{ pid = ${pid}; exists = $false } | ConvertTo-Json -Compress
    } else {
      @{ pid = $target.Id; exists = $true; identity = @{ StartTimeTicks = $start; ExecutablePath = $executable } } | ConvertTo-Json -Compress
    }
  } catch {
    if ($target.HasExited) {
      @{ pid = ${pid}; exists = $false } | ConvertTo-Json -Compress
    } else {
      throw
    }
  }
} finally {
  $target.Dispose()
}`]]
      : ['/bin/ps', ['-p', String(pid), '-o', 'lstart=,command=']]
    const result = await runImpl(command[0], command[1], { env, allowFailure: true, timeout: 10000 })
    const stdout = result.stdout.trim()
    const stderr = result.stderr.trim()
    if (platform !== 'win32' && result.status === 1 && !result.signal && !stdout && !stderr) return ''
    assert.equal(result.status, 0, `Process inspection failed (${result.signal || result.status}): ${stderr || stdout}`)
    assert.ok(!result.signal && !stderr, `Process inspection was incomplete: ${result.signal || stderr}`)
    if (platform !== 'win32') {
      assert.ok(stdout, 'ps returned no process identity without its missing-PID status')
      return stdout
    }
    const document = JSON.parse(stdout)
    assert.equal(document.pid, pid, 'Windows inspection did not identify the requested PID')
    if (document.exists === false && document.identity === undefined) return ''
    assert.equal(document.exists, true, 'Windows inspection did not report whether the process exists')
    const fields = ['StartTimeTicks', 'ExecutablePath']
    for (const field of fields) {
      assert.ok(typeof document.identity?.[field] === 'string' && document.identity[field].trim(),
        `Windows process identity is unavailable: ${field}`)
    }
    assert.match(document.identity.StartTimeTicks, /^[0-9]{1,20}$/, 'Windows process start time must identify its creation')
    return JSON.stringify(Object.fromEntries(fields.map((field) => [field, document.identity[field]])))
  } catch (error) {
    const failure = new Error(`Process identity inspection unavailable for PID ${pid}: ${error.message}`, { cause: error })
    failure.code = 'PORTO_PROCESS_INSPECTION_UNAVAILABLE'
    logImpl(failureDiagnostic(failure))
    throw failure
  }
}

async function stopProcess(processRecord, env, tree = false) {
  if (!processRecord || !processRecord.identity) return
  if (await identity(processRecord.pid, env) !== processRecord.identity) return
  if (process.platform === 'win32' && tree) {
    await run(path.join(env.SystemRoot, 'System32', 'taskkill.exe'),
      ['/PID', String(processRecord.pid), '/T', '/F'], { env })
  } else {
    process.kill(processRecord.pid, 'SIGTERM')
  }
  await waitFor(async () => await identity(processRecord.pid, env) !== processRecord.identity,
    `owned process ${processRecord.pid} did not exit`, 30000)
}

async function startDesktop(fixture, installed, env) {
  await preflight(fixture, env)
  const debuggerServer = require('node:net').createServer()
  await new Promise((resolve, reject) => {
    debuggerServer.once('error', reject)
    debuggerServer.listen(0, '127.0.0.1', resolve)
  })
  const debuggerPort = debuggerServer.address().port
  await new Promise((resolve, reject) => debuggerServer.close((error) => error ? reject(error) : resolve()))
  const log = fs.openSync(path.join(fixture.directory, 'desktop-launch.log'), 'a')
  const args = [`--user-data-dir=${fixture.profile}`, '--remote-debugging-address=127.0.0.1',
    `--remote-debugging-port=${debuggerPort}`, '--disable-breakpad']
  let desktop
  try {
    desktop = spawn(installed.desktop, args, { env, cwd: fixture.home, windowsHide: true, stdio: ['ignore', log, log] })
    await new Promise((resolve, reject) => { desktop.once('spawn', resolve); desktop.once('error', reject) })
  } finally {
    fs.closeSync(log)
  }
  const record = { pid: desktop.pid, identity: await identity(desktop.pid, env) }
  let daemon
  let renderer
  try {
    assert.ok(record.identity, 'Cannot establish the identity of the installed desktop process')
    const health = await waitFor(async () => {
      assert.equal(desktop.exitCode, null, 'Installed desktop exited before becoming ready')
      const health = await json(`${DAEMON_URL}/api/health`)
      assert.equal(health.daemonIdentity, digest(installed.porto), 'Desktop used a different daemon executable')
      return health.status === 'ok' && health.dashboardReady === true && health
    }, 'installed desktop must launch its own dashboard-serving daemon', 60000)
    const owned = (await daemons(env)).filter((item) => fs.realpathSync(item.executable) === fs.realpathSync(installed.porto))
    assert.equal(owned.length, 1, 'Expected exactly one daemon launched from the installed package')
    daemon = { ...owned[0], identity: await identity(owned[0].pid, env) }
    assert.ok(daemon.identity, 'Cannot establish the identity of the installed daemon process')
    await waitFor(async () => {
      const status = await json(`${DAEMON_URL}/api/docker/status`)
      assert.equal(status.enabled, true)
      assert.equal(status.endpoint, env.DOCKER_HOST)
      if (!status.available) throw new Error(status.message || 'The installed backend is not available')
      return status
    }, 'real installed first-run Lima/containerd/BuildKit provisioning', 25 * 60 * 1000)
    const page = await waitFor(async () => {
      const pages = await json(`http://127.0.0.1:${debuggerPort}/json/list`)
      return pages.find((item) => item.type === 'page' && new URL(item.url).origin === DAEMON_URL)
    }, 'installed dashboard renderer', 60000)
    renderer = await cdp(page.webSocketDebuggerUrl)
    await waitFor(async () => await renderer.evaluate('document.getElementById("root")?.childElementCount > 0'),
      'installed dashboard must actually render')
    return { renderer, health, desktop: record, daemon }
  } catch (error) {
    renderer?.close()
    // The health check can fail before the detached, owned daemon was recorded.
    const owned = daemon ? [daemon] : (await daemons(env))
      .filter((item) => path.resolve(item.executable) === installed.porto)
    for (const item of owned) {
      await stopProcess({ ...item, identity: item.identity || await identity(item.pid, env) }, env, true)
    }
    await stopProcess(record, env, true)
    throw error
  }
}

async function toolSnapshot(installed) {
  const result = {}
  for (const file of [installed.porto, installed.desktop, installed.tools.launcher,
    installed.tools.compose, installed.tools.buildx]) {
    const stat = fs.statSync(file, { bigint: true })
    result[path.relative(installed.root, file)] = {
      sha256: await fileDigest(file), identity: `${stat.ino}/${stat.ctimeNs}`,
    }
  }
  return result
}

async function snapshot(fixture, installed) {
  const files = [path.join(fixture.portoHome, 'porto.db'), path.join(fixture.portoHome, 'docker', 'engine.json'),
    path.join(fixture.limaHome, 'porto-engine', 'diffdisk'),
    path.join(fixture.home, 'Preserved synthetic project', 'package.json')]
  const wal = path.join(fixture.portoHome, 'porto.db-wal')
  if (fs.existsSync(wal)) files.push(wal)
  assert.equal(fs.readFileSync(files[0]).subarray(0, 16).toString(), 'SQLite format 3\0',
    'Preservation must include an actual populated Porto database')
  fixture.snapshots = {}
  for (const file of files) {
    if (file !== wal) assert.ok(fs.statSync(file).size > 0, `Installed runtime did not populate ${file}`)
    const relative = path.relative(fixture.directory, file)
    fixture.snapshots[relative] = await fileDigest(stateFile(fixture, relative))
  }
  fixture.toolSnapshot = await toolSnapshot(installed)
  save(fixture)
}

function stateFile(fixture, relative) {
  const file = workspacePath(path.join(fixture.directory, relative))
  assert.ok(within(fixture.directory, file) && within(fixture.directory, fs.realpathSync(file)),
    'Retained runtime state must not escape its synthetic fixture')
  return file
}

async function restored(fixture, { replaced = false } = {}) {
  preserved(fixture)
  assert.ok(fixture.snapshots, 'Missing populated installed-runtime preservation baseline')
  for (const [relative, expected] of Object.entries(fixture.snapshots)) {
    assert.equal(await fileDigest(stateFile(fixture, relative)), expected,
      `Upgrade/uninstall changed stopped Porto state or real runtime data: ${relative}`)
  }
  if (replaced) {
    const actual = await toolSnapshot(inspectPackage(fixture.installRoot, fixture.target))
    for (const [file, previous] of Object.entries(fixture.toolSnapshot)) {
      assert.equal(actual[file].sha256, previous.sha256, `Reinstall did not restore the shipped tool: ${file}`)
      assert.notEqual(actual[file].identity, previous.identity, `Upgrade did not replace its owned executable: ${file}`)
    }
  }
}

async function acceptance(fixture, reopen = false) {
  if (reopen) await restored(fixture, { replaced: true })
  else assert.equal(fixture.snapshots, undefined, 'Initial acceptance must begin with fresh Porto state')
  const installed = inspectPackage(fixture.installRoot, fixture.target)
  const env = environment(fixture, installed)
  fs.mkdirSync(env.XDG_RUNTIME_DIR, { mode: 0o700, recursive: true })
  await preflight(fixture, env)
  await configureLima(fixture, env)
  const session = await startDesktop(fixture, installed, env)
  const cliEnv = { ...env, PATH: path.join(installed.runtime, 'bin'), BUILDKIT_HOST: '' }
  const workspace = path.join(fixture.home, 'Synthetic Compose 项目')
  fs.mkdirSync(workspace, { recursive: true, mode: 0o700 })
  const project = `porto-installed-${fixture.id}`
  const image = `${project}:acceptance`
  const cli = async (...args) => {
    const result = await run(installed.porto, ['docker', 'cli', ...args], {
      env: cliEnv, cwd: workspace, timeout: 8 * 60 * 1000,
    })
    assert.ok(!`${result.stdout}\n${result.stderr}`.includes('UNRELATED-SYNTHETIC-PLUGIN-MUST-NOT-RUN'),
      'Docker invoked an unrelated plugin instead of the installed plugin')
    return result.stdout.trim()
  }
  const compose = (...args) => cli('compose', '--project-name', project, '--file', path.join(workspace, 'compose.json'), ...args)
  const statePath = path.join(fixture.portoHome, 'docker', 'engine.json')
  let failed
  try {
    const projectPath = path.join(fixture.home, 'Preserved synthetic project')
    if (!reopen) {
      write(path.join(projectPath, 'package.json'), JSON.stringify({
        name: `porto-preserved-${fixture.id}`, scripts: { dev: 'node -e "process.exit(98)"' },
      }))
      const scan = await fetch(`${DAEMON_URL}/api/scan`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ roots: [projectPath], depth: 1 }),
        signal: AbortSignal.timeout(30000),
      })
      assert.equal(scan.status, 200, await scan.text())
    }
    const projects = await json(`${DAEMON_URL}/api/projects`)
    const storedProject = projects.find((project) => project.path === projectPath)
    assert.ok(storedProject,
      'A real registered synthetic Porto project must survive the package upgrade')
    assert.equal(storedProject.pid, 0, 'Preservation must never start a host project process')
    const engine = JSON.parse(fs.readFileSync(statePath, 'utf8'))
    assert.equal(engine.mode, 'lima', 'Never accept a host containerd/Docker fallback as the disposable owned backend')
    assert.equal(engine.instance, 'porto-engine')
    assert.ok(engine.ownerId, 'The installed binary must create the engine ownership record')
    const owner = await run(installed.limactl, ['shell', '--workdir=/', 'porto-engine', '--', 'sh', '-c',
      'cat "$HOME/.porto-engine-owner"'], { env })
    assert.equal(owner.stdout.trim(), engine.ownerId, 'Installed ownership record and real guest marker must match')
    for (const [name, args, expected] of [
      ['Docker', ['--version'], EXPECTED.docker],
      ['Compose', ['compose', 'version', '--short'], EXPECTED['docker-compose']],
      ['Buildx', ['buildx', 'version'], EXPECTED['docker-buildx']],
    ]) {
      const output = await cli(...args)
      reportedVersion(output, expected)
      process.stdout.write(`${name}: ${output}\n`)
    }
    const info = JSON.parse(await cli('info', '--format', '{{json .}}'))
    pluginPaths(info, installed.tools)
    assert.equal(info.OSType, 'linux', 'The named pipe/socket must reach the disposable Linux engine')
    const direct = await run(installed.tools.launcher, ['info', '--format', '{{json .}}'], {
      env: cliEnv,
    })
    assert.ok(!`${direct.stdout}\n${direct.stderr}`.includes('UNRELATED-SYNTHETIC-PLUGIN-MUST-NOT-RUN'))
    const directInfo = JSON.parse(direct.stdout)
    pluginPaths(directInfo, installed.tools)
    write(path.join(workspace, 'fixture.txt'), `Synthetic installed image ${fixture.id}\n`)
    write(path.join(workspace, 'Dockerfile'),
      'FROM busybox:1.37.0\nCOPY fixture.txt /porto-installed.txt\nCMD ["sh", "-c", "sleep 3600"]\n')
    if (!reopen) {
      await cli('buildx', 'build', '--builder', 'default', '--load', '--tag', image,
        '--label', `porto.test.suite=${project}`, workspace)
    }
    const built = JSON.parse(await cli('image', 'inspect', image))
    assert.match(built[0].Id, /^sha256:[a-f0-9]{64}$/)
    assert.equal(built[0].Config.Labels['porto.test.suite'], project)
    write(path.join(workspace, 'compose.json'), JSON.stringify({
      services: {
        app: {
          image, pull_policy: 'never', network_mode: 'none',
          labels: { 'porto.test.suite': project },
          volumes: ['retained:/data'],
          command: ['sh', '-c', `test -f /data/token || printf '%s' '${fixture.id}' > /data/token; exec sleep 3600`],
        },
      },
      volumes: { retained: { name: `${project}-retained`, labels: { 'porto.test.suite': project } } },
    }))
    await compose('up', '--detach', '--no-build')
    const id = await compose('ps', '--status', 'running', '--quiet')
    assert.match(id, /^[a-f0-9]{12,64}$/, 'Compose must start exactly one real synthetic container')
    const inspect = async () => JSON.parse(await cli('container', 'inspect', id))[0]
    assert.equal((await inspect()).Config.Labels['com.docker.compose.project'], project)
    await waitFor(async () => await cli('exec', id, 'cat', '/data/token') === fixture.id, 'real retained volume token')
    assert.equal(await cli('exec', id, 'cat', '/porto-installed.txt'), `Synthetic installed image ${fixture.id}`)
    const listing = await rendererRequest(session.renderer, '/api/docker/containers')
    assert.equal(listing.status, 200, listing.body)
    assert.ok(JSON.parse(listing.body).some((item) => item.id === id || id.startsWith(item.id)),
      'The real installed renderer must observe the Compose container')
    for (const [action, running] of [['stop', false], ['start', true]]) {
      const response = await rendererRequest(session.renderer, `/api/docker/containers/${id}/${action}`, 'POST')
      assert.equal(response.status, 200, response.body)
      await waitFor(async () => (await inspect()).State.Running === running, `real desktop ${action}`)
    }
    const before = await inspect()
    assert.ok(before.State.Pid > 0, 'The running synthetic workload must have a real guest PID')
    const refusal = await rendererRequest(session.renderer, '/api/docker/engine/prepare-update', 'POST')
    assert.equal(refusal.status, 409, 'Desktop update preparation must refuse a real running workload')
    assert.match(refusal.body, /running|active|stop/i, 'Unsafe update refusal must be actionable')
    const after = await inspect()
    assert.equal(after.State.Running, true, 'Update preparation killed the workload')
    assert.equal(after.State.Pid, before.State.Pid, 'Update preparation replaced the workload task')
    assert.equal(after.State.StartedAt, before.State.StartedAt, 'Update preparation restarted the workload')
    assert.equal(await cli('exec', id, 'cat', '/data/token'), fixture.id)
    await compose('down', '--remove-orphans')
    assert.equal(await cli('ps', '--all', '--quiet', '--filter', `label=porto.test.suite=${project}`), '',
      'Compose down left synthetic containers behind')
    await cli('volume', 'inspect', `${project}-retained`)
    preserved(fixture)
    fixture.evidence ||= {}
    fixture.evidence[reopen ? 'upgrade' : 'cleanInstall'] = {
      target: fixture.target, backend: fixture.backend, daemonIdentity: session.health.daemonIdentity,
      executable: installed.porto, docker: installed.tools.launcher,
      compose: installed.tools.compose, buildx: installed.tools.buildx,
      versions: EXPECTED, image: built[0].Id, ownedEngine: engine.ownerId,
      realComposeUpDown: true, realBuildxLoadInspect: !reopen || fixture.evidence.cleanInstall?.realBuildxLoadInspect === true,
      renderedDesktopControl: true, runningWorkloadUpdateRefused: true, retainedVolume: true,
      preservedRegisteredProject: projectPath, runningWorkloadPID: before.State.Pid,
    }
  } catch (error) {
    failed = error
  } finally {
    session.renderer.close()
    try {
      if (fs.existsSync(path.join(workspace, 'compose.json'))) await compose('down', '--remove-orphans')
      await run(installed.porto, ['docker', 'engine-stop'], { env, timeout: 5 * 60 * 1000 })
      const status = await run(installed.limactl, ['list', 'porto-engine', '--json'], { env })
      const instances = status.stdout.trim().split(/\r?\n/).flatMap((line) => JSON.parse(line))
      assert.equal(instances.length, 1)
      assert.equal(instances[0].status, 'Stopped', 'Never uninstall or remove a fixture with a running backend')
    } catch (error) {
      failed = new AggregateError([failed, error].filter(Boolean), 'Installed acceptance or owned runtime cleanup failed')
    }
    for (const [record, tree] of [[session.desktop, true], [session.daemon, true]]) {
      try { await stopProcess(record, env, tree) } catch (error) {
        failed = new AggregateError([failed, error].filter(Boolean), 'Unable to stop an owned acceptance process')
      }
    }
  }
  if (failed) throw failed
  await snapshot(fixture, installed)
  process.stdout.write(`Installed ${fixture.target}: real desktop, Compose, Buildx, retained volume and safe update refusal passed (${fixture.backend}).\n`)
}

async function removed(fixture) {
  assert.ok(fixture.evidence?.upgrade, 'Uninstall acceptance requires a completed real upgrade/reopen')
  await waitFor(() => !fs.existsSync(fixture.installRoot), 'uninstall must remove its owned application', 60000)
  await restored(fixture)
  await preflight(fixture, environment(fixture))
  fixture.evidence.uninstall = { retainedPortoStateAndGuestDisk: true, unrelatedDockerDataUnchanged: true }
  const report = path.resolve('dist', `installed-acceptance_${fixture.target.replace('/', '_')}.json`)
  write(report, `${JSON.stringify(fixture.evidence, null, 2)}\n`)
  fs.rmSync(fixture.directory, { recursive: true })
  process.stdout.write(`Installed upgrade/uninstall preservation passed. Evidence: ${report}\n`)
}

async function main(args) {
  assert.equal(process.env.GITHUB_ACTIONS, 'true', 'Installed acceptance provisions disposable CI hosts only, never a developer runtime')
  const [command, directory, expectedTarget, backend = 'default'] = args
  assert.ok(directory, 'Usage: installed-acceptance.cjs prepare|environment|preflight|run|upgraded|removed <fresh-checkout-directory> [goos/goarch] [default|qemu-tcg]')
  if (command === 'prepare') {
    assert.ok(['default', 'qemu-tcg'].includes(backend), 'Unknown acceptance backend')
    const fixture = prepare(directory, expectedTarget || target())
    fixture.backend = backend
    save(fixture)
    return
  }
  const fixture = load(directory)
  if (command === 'environment') process.stdout.write(`${JSON.stringify(environment(fixture))}\n`)
  else if (command === 'preflight') await preflight(fixture, environment(fixture))
  else if (command === 'run') await acceptance(fixture)
  else if (command === 'upgraded') await acceptance(fixture, true)
  else if (command === 'removed') await removed(fixture)
  else throw new Error(`Unknown installed acceptance command: ${command}`)
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(failureDiagnostic(error))
    process.exitCode = 1
  })
}

module.exports = { acceptance, configureLima, existingDesktops, hostProcesses, identity, main, preflight, removed, restored }
