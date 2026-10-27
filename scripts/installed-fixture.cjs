const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const net = require('node:net')
const path = require('node:path')
const { spawn } = require('node:child_process')
const { setTimeout: delay } = require('node:timers/promises')

const { validateToolchainLayout } = require('./docker-toolchain-smoke.cjs')

const SOURCE_ROOT = fs.realpathSync(path.join(__dirname, '..'))
const DAEMON_URL = 'http://127.0.0.1:37623'
const PORTS = [37623, 37680, 37681]
const EXPECTED = { docker: '29.7.2', 'docker-compose': '5.5.1', 'docker-buildx': '0.37.1' }

function within(root, candidate) {
  const relative = path.relative(root, candidate)
  return relative !== '' && !path.isAbsolute(relative) && relative !== '..' && !relative.startsWith(`..${path.sep}`)
}

function untrustedCommand() {
  return Object.assign(new Error('Acceptance commands must use trusted workspace files or approved OS tools'), {
    code: 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE',
  })
}

function workspaceFile(file) {
  const absolute = path.resolve(SOURCE_ROOT, file)
  if (!within(SOURCE_ROOT, absolute)) throw untrustedCommand()
  const actual = fs.realpathSync(absolute)
  if (!within(SOURCE_ROOT, actual) || !fs.statSync(actual).isFile()) throw untrustedCommand()
  return actual
}

function windowsSystemDirectoryFromModules(modules) {
  const directories = ['ntdll.dll', 'kernel32.dll', 'kernelbase.dll'].map((name) => {
    const matches = modules.filter((file) => path.win32.basename(file).toLowerCase() === name)
    if (matches.length !== 1) throw untrustedCommand()
    return path.win32.dirname(matches[0])
  })
  if (!path.win32.isAbsolute(directories[0]) ||
      path.win32.basename(directories[0]).toLowerCase() !== 'system32' ||
      directories.some((directory) => directory.toLowerCase() !== directories[0].toLowerCase())) {
    throw untrustedCommand()
  }
  return directories[0]
}

let systemDirectory
function windowsSystemDirectory() {
  if (systemDirectory) return systemDirectory
  const excluded = process.report.excludeEnv
  const excludedNetwork = process.report.excludeNetwork
  assert.equal(typeof excluded, 'boolean', 'The native Node report API must support excluding environment data')
  assert.equal(typeof excludedNetwork, 'boolean', 'The native Node report API must support excluding network data')
  try {
    process.report.excludeEnv = true
    process.report.excludeNetwork = true
    // Loaded KnownDLL paths come from the OS loader, never SystemRoot or PATH.
    systemDirectory = fs.realpathSync(windowsSystemDirectoryFromModules(process.report.getReport().sharedObjects))
    return systemDirectory
  } finally {
    process.report.excludeEnv = excluded
    process.report.excludeNetwork = excludedNetwork
  }
}

function windowsSupportEnvironment(fixture, inherited, directory) {
  const root = path.win32.dirname(directory)
  const homeRoot = path.win32.parse(fixture.home).root
  const result = {
    SystemRoot: root, WINDIR: root, SystemDrive: path.win32.parse(root).root.replace(/\\$/, ''),
    OS: 'Windows_NT', COMSPEC: path.win32.join(directory, 'cmd.exe'),
    HOMEDRIVE: homeRoot.replace(/\\$/, ''), HOMEPATH: '\\' + fixture.home.slice(homeRoot.length),
    PSModulePath: path.win32.join(directory, 'WindowsPowerShell', 'v1.0', 'Modules'),
    PSModuleAnalysisCachePath: path.win32.join(fixture.home, 'AppData', 'Local', 'Microsoft', 'Windows', 'PowerShell', 'ModuleAnalysisCache'),
  }
  for (const name of ['ProgramData', 'ALLUSERSPROFILE', 'ProgramFiles', 'ProgramFiles(x86)',
    'ProgramW6432', 'CommonProgramFiles', 'CommonProgramFiles(x86)', 'CommonProgramW6432']) {
    const key = Object.keys(inherited).find((key) => key.toLowerCase() === name.toLowerCase())
    if (key && path.win32.isAbsolute(inherited[key])) result[name] = inherited[key]
  }
  return result
}

function approvedTool(candidate, root) {
  const actual = fs.realpathSync(candidate)
  if (!within(fs.realpathSync(root), actual)) throw untrustedCommand()
  fs.accessSync(actual, process.platform === 'win32' ? fs.constants.F_OK : fs.constants.X_OK)
  return candidate
}

let windowsGitRoots
async function systemExecutable(name, env) {
  if (process.platform === 'win32') {
    const directory = windowsSystemDirectory()
    if (name === 'powershell.exe') return approvedTool(path.join(directory, 'WindowsPowerShell', 'v1.0', name), directory)
    if (name === 'taskkill.exe') return approvedTool(path.join(directory, name), directory)
    if (name !== 'git') throw untrustedCommand()
    if (!windowsGitRoots) {
      const result = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
foreach ($folder in @([System.Environment+SpecialFolder]::ProgramFiles, [System.Environment+SpecialFolder]::ProgramFilesX86)) {
  [Console]::WriteLine([System.Environment]::GetFolderPath($folder))
}`], { env, timeout: 10000 })
      windowsGitRoots = result.stdout.trim().split(/\r?\n/).filter(Boolean).map((folder) => path.join(folder, 'Git'))
    }
    for (const root of windowsGitRoots) {
      for (const relative of ['bin/git.exe', 'cmd/git.exe']) {
        try { return approvedTool(path.join(root, relative), root) } catch (error) {
          if (!['ENOENT', 'ENOTDIR'].includes(error.code)) throw error
        }
      }
    }
  } else {
    if (name === 'ps') return approvedTool('/bin/ps', '/bin')
    if (!['git', 'qemu-system-x86_64', 'qemu-system-aarch64'].includes(name)) throw untrustedCommand()
    const roots = process.platform === 'darwin'
      ? (process.arch === 'arm64' ? ['/opt/homebrew', '/usr/local', '/usr'] : ['/usr/local', '/opt/homebrew', '/usr'])
      : ['/usr', '/usr/local']
    for (const root of roots) {
      try { return approvedTool(path.join(root, 'bin', name), root) } catch (error) {
        if (!['ENOENT', 'ENOTDIR'].includes(error.code)) throw error
      }
    }
  }
  throw Object.assign(new Error('A required trusted OS tool is missing'), { code: 'ENOENT' })
}

async function trustedExecutable(file, env) {
  if (file === process.execPath) return process.execPath
  if (file === 'git' || file === 'powershell.exe' || file === 'taskkill.exe') return systemExecutable(file, env)
  if (process.platform !== 'win32' && file === '/bin/ps') return systemExecutable('ps', env)
  if (process.platform !== 'win32' && ['qemu-system-x86_64', 'qemu-system-aarch64'].includes(path.basename(file))) {
    const expected = await systemExecutable(path.basename(file), env)
    if (file === expected) return expected
  }
  return workspaceFile(file)
}

function workspacePath(value) {
  const resolved = path.resolve(value)
  const workspace = fs.realpathSync(process.cwd())
  assert.ok(within(workspace, resolved), 'Acceptance files must be below the current checkout')
  let ancestor = resolved
  while (!fs.existsSync(ancestor)) ancestor = path.dirname(ancestor)
  assert.ok(ancestor === workspace || within(workspace, fs.realpathSync(ancestor)),
    'Acceptance paths must not traverse symlinks outside the checkout')
  return resolved
}

function target(platform = process.platform, architecture = process.arch) {
  const goos = { darwin: 'darwin', linux: 'linux', win32: 'windows' }[platform]
  const goarch = { x64: 'amd64', arm64: 'arm64' }[architecture]
  assert.ok(goos && goarch, `Unsupported installed acceptance host: ${platform}/${architecture}`)
  if (platform === process.platform && architecture === process.arch && process.env.RUNNER_ARCH) {
    assert.equal({ X64: 'amd64', ARM64: 'arm64' }[process.env.RUNNER_ARCH.toUpperCase()], goarch,
      'The executing Node process must match the native runner architecture, not an emulation layer')
  }
  return `${goos}/${goarch}`
}

function nativeHeader(file, expectedTarget) {
  const descriptor = fs.openSync(file, 'r')
  const bytes = Buffer.alloc(64)
  let length
  let peBytes
  try {
    length = fs.readSync(descriptor, bytes, 0, bytes.length, 0)
    if (length === 64 && bytes.toString('ascii', 0, 2) === 'MZ') {
      peBytes = Buffer.alloc(6)
      assert.equal(fs.readSync(descriptor, peBytes, 0, peBytes.length, bytes.readUInt32LE(60)), 6,
        `Invalid Windows executable header: ${file}`)
    }
  } finally {
    fs.closeSync(descriptor)
  }
  const [goos, arch] = expectedTarget.split('/')
  let machine
  if (goos === 'linux' && bytes.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46])) &&
      bytes[4] === 2 && bytes[5] === 1 && length >= 20) {
    machine = { 62: 'amd64', 183: 'arm64' }[bytes.readUInt16LE(18)]
  } else if (goos === 'darwin' && length >= 8 && bytes.readUInt32LE(0) === 0xfeedfacf) {
    machine = { 0x1000007: 'amd64', 0x100000c: 'arm64' }[bytes.readUInt32LE(4)]
  } else if (goos === 'windows' && peBytes?.toString('ascii', 0, 4) === 'PE\0\0') {
    machine = { 0x8664: 'amd64', 0xaa64: 'arm64' }[peBytes.readUInt16LE(4)]
  }
  assert.equal(machine, arch, `${file} must be a native ${expectedTarget} executable, not an emulated or host substitute`)
}

function layout(root, platform = process.platform) {
  const resources = platform === 'darwin' ? path.join(root, 'Contents', 'Resources') : path.join(root, 'resources')
  const suffix = platform === 'win32' ? '.exe' : ''
  return {
    root, resources,
    desktop: platform === 'darwin' ? path.join(root, 'Contents', 'MacOS', 'Porto') : path.join(root, `Porto${suffix}`),
    porto: path.join(resources, `porto${suffix}`),
    runtime: path.join(resources, 'runtime'),
    limactl: path.join(resources, 'runtime', 'lima', 'bin', `limactl${suffix}`),
  }
}

function inspectPackage(root, expectedTarget) {
  assert.equal(target(), expectedTarget, 'Cross-built packages are not native installed acceptance')
  assert.match(root, / /, 'The actual installed executable path must contain spaces')
  assert.match(root, /[^\u0000-\u007f]/, 'The actual installed executable path must contain non-ASCII characters')
  const result = layout(root)
  const toolchain = validateToolchainLayout(result.runtime)
  assert.equal(toolchain.supported, true, 'Missing promised installed Docker tools must fail, never skip')
  for (const [name, version] of Object.entries(EXPECTED)) {
    assert.equal(toolchain.versions.get(name)?.split(/[ (]/)[0].replace(/^v/, ''), version,
      `Installed ${name} metadata must match the pinned toolchain`)
  }
  const nativeFile = (file, expected) => {
    assert.ok(within(fs.realpathSync(root), fs.realpathSync(file)), `Executable escaped its installed package: ${file}`)
    nativeHeader(file, expected)
  }
  for (const file of [result.desktop, result.porto, result.limactl,
    toolchain.layout.launcher, toolchain.layout.compose, toolchain.layout.buildx]) {
    nativeFile(file, expectedTarget)
  }
  const arch = expectedTarget.split('/')[1]
  nativeFile(path.join(result.runtime, 'bin', 'porto-runtime-helper'), `linux/${arch}`)
  if (process.platform === 'win32') {
    for (const binary of ['qemu-img.exe', arch === 'amd64' ? 'qemu-system-x86_64.exe' : 'qemu-system-aarch64.exe']) {
      nativeFile(path.join(result.runtime, 'qemu', binary), expectedTarget)
    }
  }
  return { ...result, tools: toolchain.layout }
}

function write(file, contents, mode = 0o600) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 })
  fs.writeFileSync(file, contents, { mode })
}

function digest(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')
}

async function fileDigest(file) {
  const hash = crypto.createHash('sha256')
  for await (const chunk of fs.createReadStream(file)) hash.update(chunk)
  return hash.digest('hex')
}

function save(fixture) {
  write(path.join(fixture.directory, 'fixture.json'), `${JSON.stringify(fixture, null, 2)}\n`)
}

function prepare(directory, expectedTarget = target()) {
  directory = workspacePath(directory)
  assert.equal(target(), expectedTarget, 'The package target must match this native host')
  fs.mkdirSync(directory, { mode: 0o700 })
  const fixture = {
    directory, target: expectedTarget, id: crypto.randomBytes(8).toString('hex'),
    home: path.join(directory, 'h'),
    portoHome: path.join(directory, 's'),
    limaHome: path.join(directory, 'l'),
    scratch: path.join(directory, 'scratch'),
    profile: path.join(directory, 'profile'),
    dockerConfig: path.join(directory, 'Docker 配置 with spaces'),
    installRoot: path.join(directory, 'Porto installé 路径', ...(process.platform === 'darwin' ? ['Porto.app'] : [])),
    sentinels: {},
  }
  for (const name of ['home', 'portoHome', 'limaHome', 'scratch', 'profile', 'dockerConfig']) {
    fs.mkdirSync(fixture[name], { mode: 0o700 })
  }
  for (const relative of ['Documents/WindowsPowerShell/Modules', 'AppData/Roaming',
    'AppData/Local/Microsoft/Windows/PowerShell']) {
    fs.mkdirSync(path.join(fixture.home, relative), { recursive: true, mode: 0o700 })
  }
  const suffix = process.platform === 'win32' ? '.exe' : ''
  const context = crypto.createHash('sha256').update('unrelated-synthetic-context').digest('hex')
  const config = {
    currentContext: 'unrelated-synthetic-context',
    cliPluginsExtraDirs: [path.join(fixture.home, 'unrelated plugins')],
    credHelpers: { 'synthetic.invalid': 'unrelated-synthetic-helper' },
  }
  const files = {
    [path.join(fixture.dockerConfig, 'config.json')]: `${JSON.stringify(config, null, 2)}\n`,
    [path.join(fixture.dockerConfig, 'contexts', 'meta', context, 'meta.json')]: JSON.stringify({
      Name: config.currentContext,
      Metadata: { Description: 'Unrelated synthetic context: never connect' },
      Endpoints: { docker: { Host: 'unix:///nonexistent-synthetic-docker.sock', SkipTLSVerify: false } },
    }),
    [path.join(fixture.dockerConfig, 'buildx', 'unrelated-builder-state')]: 'Unrelated synthetic Buildx data\n',
    [path.join(fixture.portoHome, 'preservation', 'project.txt')]: `Synthetic Porto project ${fixture.id}\n`,
    [path.join(fixture.portoHome, 'preservation', 'volume', 'data.txt')]: `Synthetic Porto volume data ${fixture.id}\n`,
    [path.join(fixture.home, '.local', 'bin', `docker${suffix}`)]: 'Unrelated synthetic Docker command: must be preserved\n',
    [path.join(fixture.home, '.local', 'bin', `docker-credential-unrelated-synthetic-helper${suffix}`)]: 'Unrelated helper: must never execute\n',
  }
  for (const pluginRoot of [path.join(fixture.dockerConfig, 'cli-plugins'), config.cliPluginsExtraDirs[0]]) {
    for (const name of ['compose', 'buildx']) {
      files[path.join(pluginRoot, `docker-${name}${suffix}`)] =
        '#!/bin/sh\necho UNRELATED-SYNTHETIC-PLUGIN-MUST-NOT-RUN >&2\nexit 42\n'
    }
  }
  for (const [file, contents] of Object.entries(files)) {
    write(file, contents, 0o700)
    fixture.sentinels[path.relative(directory, file)] = {
      sha256: digest(file), mode: fs.statSync(file).mode & 0o777,
    }
  }
  save(fixture)
  return fixture
}

function load(directory) {
  directory = workspacePath(directory)
  const fixture = JSON.parse(fs.readFileSync(path.join(directory, 'fixture.json'), 'utf8'))
  assert.equal(fixture.directory, directory)
  assert.match(fixture.id, /^[a-f0-9]{16}$/)
  assert.equal(fixture.target, target(), 'Fixture cannot be reused on a different native target')
  for (const key of ['home', 'portoHome', 'limaHome', 'scratch', 'profile', 'dockerConfig', 'installRoot']) {
    assert.ok(within(directory, workspacePath(fixture[key])), `Unsafe fixture path: ${key}`)
  }
  return fixture
}

function environment(fixture, packageLayout = layout(fixture.installRoot), inherited = process.env) {
  const env = {}
  // Deliberately do not inherit credentials, SSH agents, Docker contexts, loaders,
  // user configuration, trust overrides, or source-development entry points.
  for (const name of ['SystemRoot', 'WINDIR', 'COMSPEC', 'PATHEXT', 'DISPLAY', 'XAUTHORITY', 'LANG', 'LC_ALL']) {
    const key = Object.keys(inherited).find((key) => key.toLowerCase() === name.toLowerCase())
    if (key) env[name] = inherited[key]
  }
  const systemPath = inherited.PATH || inherited.Path || ''
  const nativePaths = process.platform === 'win32'
    ? [windowsSystemDirectory(), path.join(windowsSystemDirectory(), 'WindowsPowerShell', 'v1.0')]
    : []
  const locale = process.platform === 'darwin' ? 'en_US.UTF-8' : 'C.UTF-8'
  return {
    ...env,
    ...(process.platform === 'win32' ? windowsSupportEnvironment(fixture, inherited, windowsSystemDirectory()) : {}),
    ...(process.platform !== 'win32' ? { LANG: locale, LC_ALL: locale } : {}),
    HOME: fixture.home, USERPROFILE: fixture.home,
    APPDATA: path.join(fixture.home, 'AppData', 'Roaming'),
    LOCALAPPDATA: path.join(fixture.home, 'AppData', 'Local'),
    XDG_CONFIG_HOME: path.join(fixture.home, '.config'),
    XDG_CACHE_HOME: path.join(fixture.home, '.cache'),
    XDG_DATA_HOME: path.join(fixture.home, '.local', 'share'),
    XDG_RUNTIME_DIR: path.join(fixture.directory, 'xdg-run'),
    GIT_CEILING_DIRECTORIES: fixture.directory,
    GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: path.join(fixture.home, '.gitconfig'),
    TMPDIR: fixture.scratch, TEMP: fixture.scratch, TMP: fixture.scratch,
    PORTO_HOME: fixture.portoHome, LIMA_HOME: fixture.limaHome,
    DOCKER_CONFIG: fixture.dockerConfig, DOCKER_CONTEXT: '', DOCKER_AUTH_CONFIG: '',
    DOCKER_TLS_VERIFY: '', DOCKER_CERT_PATH: '',
    DOCKER_HOST: process.platform === 'win32' ? 'npipe:////./pipe/porto_docker_engine' : `unix://${fixture.portoHome}/run/docker.sock`,
    BUILDX_BUILDER: 'default', BUILDX_CONFIG: path.join(fixture.directory, 'buildx'),
    CONTAINERD_ADDRESS: path.join(fixture.directory, 'no-host-containerd.sock'),
    BUILDKIT_HOST: `unix://${fixture.directory}/no-host-buildkit.sock`,
    PATH: [path.join(packageLayout.runtime, 'bin'), path.dirname(packageLayout.limactl),
      path.join(packageLayout.runtime, 'qemu'), ...nativePaths, systemPath].join(path.delimiter),
  }
}

function preserved(fixture) {
  for (const [relative, expected] of Object.entries(fixture.sentinels)) {
    const file = workspacePath(path.join(fixture.directory, relative))
    assert.ok(within(fixture.directory, file))
    const stat = fs.lstatSync(file)
    assert.ok(stat.isFile(), `Install/upgrade/uninstall replaced an unrelated sentinel with a different file type: ${relative}`)
    assert.equal(stat.mode & 0o777, expected.mode, `Install/upgrade/uninstall changed unrelated sentinel permissions: ${relative}`)
    assert.equal(digest(file), expected.sha256, `Install/upgrade/uninstall changed unrelated or retained data: ${relative}`)
  }
}

function pluginPaths(info, tools) {
  assert.ok(Array.isArray(info?.ClientInfo?.Plugins), 'Docker info omitted selected client plugin paths')
  for (const name of ['compose', 'buildx']) {
    const candidates = info.ClientInfo.Plugins.filter((plugin) => plugin.Name === name)
    assert.equal(candidates.length, 1, `Expected one selected shipped ${name} plugin`)
    assert.equal(fs.realpathSync(candidates[0].Path), fs.realpathSync(tools[name]),
      `Docker selected a host/user ${name} plugin instead of the installed plugin`)
    assert.equal(candidates[0].Version.replace(/^v/, ''), EXPECTED[`docker-${name}`])
  }
}

function reportedVersion(output, expected) {
  assert.match(output, new RegExp(`(^|[\\s,])v?${expected.replaceAll('.', '\\.')}($|[\\s,])`),
    `Installed tool must report the exact pinned version ${expected}`)
}

function failureDiagnostic(error) {
  const fallback = 'Installed-package acceptance failed. Check the isolated runner prerequisites and package contents, then retry with a fresh disposable CI fixture. Error details are omitted to protect credentials.'
  let code
  try { code = error?.code } catch { return fallback }
  switch (code) {
    case 'PORTO_COMMAND_TIMEOUT':
      return 'An acceptance command timed out. Check guest download connectivity and backend readiness, verify owned-process cleanup, then retry the disposable CI fixture.'
    case 'PORTO_COMMAND_OUTPUT_LIMIT':
      return 'An acceptance command exceeded its output limit. Check the packaged tool for repeated output and verify owned-process cleanup before retrying.'
    case 'PORTO_COMMAND_IO':
      return 'Acceptance command output could not be read. Check the runner process and pipe capabilities; do not claim the command completed.'
    case 'PORTO_COMMAND_SHELL_FORBIDDEN':
      return 'Shell execution is forbidden in installed acceptance. Use an executable and argument array, or pass a trusted script file explicitly to /bin/sh.'
    case 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE':
      return 'An untrusted executable or script was refused. Use the verified workspace package or documented OS-installed prerequisite tools, not an inherited PATH override.'
    case 'PORTO_PROCESS_INSPECTION_UNAVAILABLE':
      return 'Process identity inspection is unavailable. Check runner permissions and native process-inspection support; never treat an unverified owned process as stopped.'
    case 'ENOENT':
      return 'A required installed file or executable is missing. Verify the complete package and documented runner prerequisites, then rerun the clean fixture.'
    case 'EACCES':
    case 'EPERM':
      return 'Runner permissions prevented access to a required tool or owned process. Verify executable permissions and process-inspection access before retrying.'
    case 'ENOSPC':
      return 'The acceptance runner has insufficient disk space. Reclaim disposable CI storage and retry with a fresh fixture.'
    case 'ERR_ASSERTION':
      return 'Installed package verification failed. Use a matching native target on a clean disposable CI host; verify shipped tools and synthetic-state preservation.'
    default:
      return fallback
  }
}

async function available(address) {
  await new Promise((resolve, reject) => {
    const socket = net.connect(address)
    socket.setTimeout(1500)
    socket.once('connect', () => {
      socket.destroy()
      reject(new Error(`Refusing existing listener ${JSON.stringify(address)}; never reuse or stop another Porto instance`))
    })
    socket.once('timeout', () => {
      socket.destroy()
      reject(new Error(`Cannot establish that ${JSON.stringify(address)} is unused`))
    })
    socket.once('error', (error) => {
      if (['ECONNREFUSED', 'ENOENT'].includes(error.code)) resolve()
      else reject(error)
    })
  })
}

async function noListeners(fixture) {
  for (const port of PORTS) {
    await available({ host: '127.0.0.1', port })
    await available({ host: '::1', port })
  }
  if (process.platform === 'win32') await available({ path: '\\\\.\\pipe\\porto_docker_engine' })
  else {
    const socket = path.join(fixture.portoHome, 'run', 'docker.sock')
    assert.ok(Buffer.byteLength(socket) < 104, 'Checkout path is too long for the native macOS Docker socket; use a shorter disposable checkout')
    await available({ path: socket })
  }
}

async function terminateCommandTree(child, env, signal) {
  assert.ok(Number.isInteger(child.pid) && child.pid > 0, 'Cannot reap a command without its owned PID')
  if (process.platform !== 'win32') {
    try { process.kill(-child.pid, 'SIGKILL') } catch (error) {
      if (error.code !== 'ESRCH') throw error
    }
    return
  }
  if (child.exitCode !== null || child.signalCode !== null) {
    throw new Error(`Owned command PID ${child.pid} exited before taskkill; refusing a possibly reused PID`)
  }
  const taskkill = await systemExecutable('taskkill.exe', env)
  await new Promise((resolve, reject) => {
    const helper = spawn(taskkill, ['/PID', String(child.pid), '/T', '/F'], {
      env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'], shell: false,
    })
    let output = ''
    let finished = false
    const dispose = () => {
      try {
        if (helper.exitCode === null && helper.signalCode === null) helper.kill('SIGKILL')
      } finally {
        helper.stdout.destroy()
        helper.stderr.destroy()
        helper.unref()
      }
    }
    const finish = (error) => {
      if (finished) return
      finished = true
      signal.removeEventListener('abort', abort)
      if (error) {
        try { dispose() } catch (cleanupError) {
          error = new AggregateError([error, cleanupError], `Unable to reap owned taskkill helper ${helper.pid}`)
        }
        reject(error)
      } else resolve()
    }
    const abort = () => {
      finish(signal.reason)
    }
    for (const stream of [helper.stdout, helper.stderr]) {
      stream.on('data', (data) => { output = (output + data).slice(-16384) })
      stream.once('error', finish)
    }
    helper.once('error', finish)
    helper.once('close', (status, exitSignal) => {
      finish(status === 0 ? undefined : new Error(
        `Owned command tree taskkill /PID ${child.pid} /T /F failed (${exitSignal || status}): ${output.trim()}`,
      ))
    })
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
}

function checkedOptions(options) {
  const {
    allowFailure = false, timeout = 120000, cleanupTimeout = 5000,
    maxBuffer = 8 * 1024 * 1024,
  } = options
  if (options.shell !== undefined && options.shell !== false) {
    throw Object.assign(new Error('Installed acceptance commands must use argv-only execution'), {
      code: 'PORTO_COMMAND_SHELL_FORBIDDEN',
    })
  }
  for (const [name, value] of Object.entries({ timeout, cleanupTimeout, maxBuffer })) {
    assert.ok(Number.isSafeInteger(value) && value > 0, `${name} must be a positive integer`)
  }
  assert.ok(timeout <= 2147483647 && cleanupTimeout <= 2147483647, 'Command timers must not overflow')
  const cwd = fs.realpathSync(options.cwd || SOURCE_ROOT)
  if (cwd !== SOURCE_ROOT && !within(SOURCE_ROOT, cwd)) throw untrustedCommand()
  return {
    allowFailure, timeout, cleanupTimeout, maxBuffer, env: options.env, cwd,
  }
}

async function run(file, args, options = {}, {
  spawnImpl = spawn, terminateImpl = terminateCommandTree,
} = {}) {
  const checked = checkedOptions(options)
  const executable = spawnImpl === spawn ? await trustedExecutable(file, checked.env) : file
  const argv = executable === process.execPath && args.length > 0 && args[0] !== '-e'
    ? [workspaceFile(path.resolve(checked.cwd, args[0])), ...args.slice(1)]
    : args
  const child = spawnImpl(executable, argv, {
    env: checked.env, cwd: checked.cwd, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
    // Keep the child referenced, but isolate its Unix process group for teardown.
    detached: process.platform !== 'win32', shell: false,
  })
  return captureCommand(child, file, argv, checked, terminateImpl)
}

async function runShellFile(file, args = [], options = {}) {
  assert.notEqual(process.platform, 'win32', 'Windows acceptance uses the native installer rather than a Unix shell')
  const script = workspaceFile(file)
  const checked = checkedOptions({ ...options, cwd: SOURCE_ROOT })
  const argv = ['--', './' + path.relative(SOURCE_ROOT, script), ...args]
  const child = spawn('/bin/sh', argv, {
    env: checked.env, cwd: checked.cwd, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
    detached: true, shell: false,
  })
  return captureCommand(child, '/bin/sh', argv, checked, terminateCommandTree)
}

async function captureCommand(child, file, args, options, terminateImpl) {
  const { allowFailure, timeout, cleanupTimeout, maxBuffer, env } = options
  const closed = new Promise((resolve, reject) => {
    child.once('error', reject)
    child.once('close', (status, signal) => resolve({ status, signal }))
  })
  let interrupt
  const interrupted = new Promise((resolve) => { interrupt = resolve })
  let failure
  const stop = (error) => {
    if (failure) return
    failure = error
    interrupt(error)
  }
  const commandError = (code, message) => Object.assign(new Error(`${file} ${args.join(' ')}: ${message}`), { code })
  const output = { stdout: { chunks: [], size: 0 }, stderr: { chunks: [], size: 0 } }
  for (const [name, stream] of [['stdout', child.stdout], ['stderr', child.stderr]]) {
    stream.on('data', (data) => {
      if (failure) return
      const state = output[name]
      const chunk = Buffer.isBuffer(data) ? data : Buffer.from(data)
      const retained = chunk.subarray(0, Math.max(0, maxBuffer - state.size))
      if (retained.length) state.chunks.push(retained)
      state.size += retained.length
      if (retained.length !== chunk.length) {
        stop(commandError('PORTO_COMMAND_OUTPUT_LIMIT', `${name} output limit exceeded (${maxBuffer} bytes)`))
      }
    })
    stream.once('error', (error) => stop(commandError('PORTO_COMMAND_IO', `${name} inspection failed: ${error.message}`)))
  }
  const timer = setTimeout(() => stop(commandError('PORTO_COMMAND_TIMEOUT', `timed out after ${timeout}ms`)), timeout)
  try {
    const outcome = await Promise.race([
      closed.then((result) => ({ result })),
      interrupted.then((error) => ({ error })),
    ])
    clearTimeout(timer)
    if (outcome.error) {
      const controller = new AbortController()
      let cleanupTimer
      const cleanupDeadline = new Promise((_, reject) => {
        cleanupTimer = setTimeout(() => {
          const error = new Error(`Owned command tree ${child.pid} cleanup exceeded ${cleanupTimeout}ms; exit could not be verified`)
          controller.abort(error)
          reject(error)
        }, cleanupTimeout)
      })
      try {
        await Promise.race([
          Promise.all([terminateImpl(child, env, controller.signal), closed]),
          cleanupDeadline,
        ])
      } catch (error) {
        controller.abort(error)
        throw Object.assign(new AggregateError([outcome.error, error],
          `${outcome.error.message}; owned command tree cleanup unavailable: ${error.message}`), {
          code: outcome.error.code,
        })
      } finally {
        clearTimeout(cleanupTimer)
      }
      throw outcome.error
    }
    const result = {
      ...outcome.result,
      stdout: Buffer.concat(output.stdout.chunks).toString('utf8'),
      stderr: Buffer.concat(output.stderr.chunks).toString('utf8'),
    }
    if (result.status !== 0 && !allowFailure) {
      throw new Error(`${file} ${args.join(' ')} failed (${result.signal || result.status}):\n${result.stdout}\n${result.stderr}`)
    }
    return result
  } finally {
    clearTimeout(timer)
    // Failed cleanup must not leave the caller waiting on inherited pipe handles.
    child.stdout.destroy()
    child.stderr.destroy()
    child.unref()
  }
}

async function waitFor(check, description, timeout = 60000) {
  const deadline = Date.now() + timeout
  let last
  while (Date.now() < deadline) {
    try {
      const value = await check()
      if (value) return value
    } catch (error) {
      last = error
    }
    await delay(500)
  }
  throw new Error(`Timed out: ${description}${last ? `: ${last.message}` : ''}`)
}

module.exports = {
  DAEMON_URL, EXPECTED, PORTS, available, digest, environment, fileDigest, inspectPackage,
  failureDiagnostic, layout, load, nativeHeader, noListeners, pluginPaths, prepare, preserved, run, save,
  reportedVersion, runShellFile, systemExecutable, target, trustedExecutable, waitFor,
  windowsSupportEnvironment, windowsSystemDirectoryFromModules, within, workspacePath, write,
}
