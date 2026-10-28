const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const net = require('node:net')
const path = require('node:path')
const test = require('node:test')
const { spawn, spawnSync } = require('node:child_process')
const { EventEmitter, once } = require('node:events')
const { PassThrough } = require('node:stream')

const {
  EXPECTED, available, digest, environment, failureDiagnostic, inspectPackage, layout, linuxProcessIdentity, load, nativeHeader,
  pluginPaths, prepare, preserved, reportedVersion, run, runShellFile, save, systemExecutable, target,
  trustedExecutable, waitFor, windowsSupportEnvironment, windowsSystemDirectoryFromModules, workspacePath, write,
} = require('./installed-fixture.cjs')
const { existingDesktops, hostProcesses, identity, restored, reportRuntimeDiagnostics, retainRuntimeDiagnostics, stopProcess, reportCommandFailure, snapshot } = require('./installed-acceptance.cjs')
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
  for (const relative of ['Documents/WindowsPowerShell/Modules', 'AppData/Roaming',
    'AppData/Local/Microsoft/Windows/PowerShell']) {
    assert.ok(fs.statSync(path.join(current.home, relative)).isDirectory())
  }
})

test('Windows startup support keeps synthetic profile paths and only native OS capabilities', () => {
  const fixture = { home: 'D:\\Synthetic workspace\\h' }
  const env = windowsSupportEnvironment(fixture, {
    SystemRoot: 'E:\\untrusted', PSModulePath: 'E:\\untrusted\\modules',
    ProgramFiles: 'R:\\Program Files', ProgramData: 'Q:\\ProgramData',
    USERNAME: 'unrelated-user', USERDOMAIN: 'unrelated-domain', GITHUB_TOKEN: 'synthetic-secret',
  }, 'Q:\\Native OS\\System32')
  assert.equal(env.SystemRoot, 'Q:\\Native OS')
  assert.equal(env.WINDIR, env.SystemRoot)
  assert.equal(env.SystemDrive, 'Q:')
  assert.equal(env.COMSPEC, 'Q:\\Native OS\\System32\\cmd.exe')
  assert.equal(env.OS, 'Windows_NT')
  assert.equal(env.HOMEDRIVE, 'D:')
  assert.equal(env.HOMEPATH, '\\Synthetic workspace\\h')
  assert.equal(env.PSModulePath, 'Q:\\Native OS\\System32\\WindowsPowerShell\\v1.0\\Modules')
  assert.ok(env.PSModuleAnalysisCachePath.startsWith(fixture.home + '\\'))
  assert.equal(env.ProgramFiles, 'R:\\Program Files')
  for (const name of ['USERNAME', 'USERDOMAIN', 'GITHUB_TOKEN']) assert.equal(env[name], undefined)
})

test('Windows OS tool roots come from loaded KnownDLLs rather than environment paths', () => {
  const modules = ['ntdll.dll', 'kernel32.dll', 'kernelbase.dll']
    .map((name) => `Q:\\Native OS\\System32\\${name}`)
  assert.equal(windowsSystemDirectoryFromModules(modules), 'Q:\\Native OS\\System32')
  for (const invalid of [
    modules.slice(0, 2),
    modules.concat('R:\\Untrusted\\System32\\kernel32.dll'),
    [modules[0], modules[1], 'R:\\Untrusted\\System32\\kernelbase.dll'],
    modules.map((file) => file.replace('System32', 'untrusted')),
  ]) assert.throws(() => windowsSystemDirectoryFromModules(invalid), { code: 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE' })
})

if (process.platform === 'win32') {
  test('native PowerShell startup works with isolated Windows capabilities and no module autoload', async (t) => {
    const current = fixture(t)
    const result = await run('powershell.exe', [
      '-NoProfile', '-NonInteractive', '-Command',
      '$PSModuleAutoLoadingPreference = "None"; [Console]::Out.Write("installed-native-startup-ready")',
    ], { env: environment(current), timeout: 10000 })
    assert.equal(result.stdout, 'installed-native-startup-ready')
    assert.equal(result.stderr, '')
  })

  test('native process payload stages work without PowerShell module autoload', async (t) => {
    const current = fixture(t)
    const env = environment(current)
    const prelude = `
$ErrorActionPreference = 'Stop'
$PSModuleAutoLoadingPreference = 'None'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
`
    const processStage = (body) => `${prelude}
$target = [System.Diagnostics.Process]::GetProcessById(${process.pid})
try {
${body}
} finally {
  $target.Dispose()
}`
    for (const [name, script, check] of [
      ['specific PID lookup', processStage('[Console]::Out.WriteLine($target.Id)'), (stdout) => {
        assert.equal(stdout.trim(), String(process.pid))
      }],
      ['creation time', processStage('[Console]::Out.WriteLine($target.StartTime.ToUniversalTime().Ticks.ToString([System.Globalization.CultureInfo]::InvariantCulture))'), (stdout) => {
        assert.match(stdout.trim(), /^[0-9]{1,20}$/)
      }],
      ['executable and liveness', processStage(`
$executable = $target.MainModule.FileName
[Console]::Out.WriteLine([System.Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($executable)))
[Console]::Out.WriteLine($target.HasExited)
`), (stdout) => {
        const lines = stdout.trim().split(/\r?\n/)
        assert.equal(lines.length, 2)
        const executable = Buffer.from(lines[0], 'base64').toString('utf16le')
        assert.equal(fs.realpathSync(executable).toLowerCase(), fs.realpathSync(process.execPath).toLowerCase())
        assert.equal(lines[1], 'False')
      }],
    ]) {
      await t.test(name, async () => {
        const result = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
          env, timeout: 10000,
        })
        assert.equal(result.stderr, '')
        check(result.stdout)
      })
    }
    await t.test('Console-only identity serialization', async () => {
      const fields = { StartTimeTicks: '639287208000000000', ExecutablePath: 'C:\\Synthetic space 路径\\Porto 🚀.exe' }
      const script = `${prelude}
$executable = '${fields.ExecutablePath}'
[Console]::Out.WriteLine('PORTO_PROCESS_IDENTITY_V1')
[Console]::Out.WriteLine('123')
[Console]::Out.WriteLine('present')
[Console]::Out.WriteLine('${fields.StartTimeTicks}')
[Console]::Out.WriteLine([System.Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($executable)))
`
      const result = await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
        env, timeout: 10000,
      })
      assert.equal(result.stderr, '')
      assert.equal(await identity(123, env, { platform: 'win32', runImpl: async () => result }), JSON.stringify(fields))
    })
  })
}

if (process.platform === 'win32') {
  test('native archive shell uses protected Git Bash rather than WSL or user profiles', async (t) => {
    const current = fixture(t)
    const env = environment(current)
    const shell = await systemExecutable('bash', env)
    const root = path.dirname(path.dirname(shell))
    assert.equal(path.basename(root).toLowerCase(), 'git')
    env.PATH = [path.join(root, 'usr', 'bin'), env.PATH].join(path.delimiter)
    const script = path.join(current.home, 'native shell probe é.sh')
    write(script, 'set -eu\n[ -n "$(dirname "$0")" ]\nprintf native-archive-shell-ready\n')
    const result = spawnSync(shell, ['--noprofile', '--norc', script], {
      env, cwd: current.home, encoding: 'utf8', windowsHide: true, shell: false, timeout: 10000,
    })
    assert.ifError(result.error)
    assert.equal(result.status, 0, 'The protected native download shell must work under the isolated environment')
    assert.equal(result.stdout, 'native-archive-shell-ready')
    assert.equal(result.stderr, '')
  })
}

test('executable selection fences workspace files and refuses symlink escapes', async (t) => {
  const current = fixture(t)
  const tool = path.join(current.home, 'approved tool é')
  write(tool, 'synthetic selection fixture', 0o700)
  assert.equal(await trustedExecutable(tool), fs.realpathSync(tool))
  const outside = path.join(current.home, 'outside')
  fs.symlinkSync(path.dirname(process.execPath), outside, process.platform === 'win32' ? 'junction' : 'dir')
  await assert.rejects(trustedExecutable(path.join(outside, path.basename(process.execPath))),
    { code: 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE' })
  await assert.rejects(run(process.execPath, [path.join(outside, path.basename(process.execPath))]),
    { code: 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE' })
})

test('fenced Node script entry points retain relative-cwd and Unicode path behavior', async (t) => {
  const current = fixture(t)
  const file = 'relative entry é.cjs'
  write(path.join(current.home, file), 'process.stdout.write("trusted workspace entry")\n')
  const result = await run(process.execPath, [file], { cwd: current.home, env: environment(current) })
  assert.equal(result.stdout, 'trusted workspace entry')
})

test('inherited PATH cannot select Git or host QEMU from a synthetic override', async (t) => {
  const current = fixture(t)
  const injected = path.join(current.home, 'untrusted PATH')
  const suffix = process.platform === 'win32' ? '.exe' : ''
  for (const name of ['git', 'qemu-system-x86_64']) write(path.join(injected, name + suffix), 'must never execute', 0o700)
  const env = environment(current, layout(current.installRoot), { ...process.env, PATH: injected })
  const git = await systemExecutable('git', env)
  assert.ok(!git.startsWith(injected + path.sep))
  assert.match((await run('git', ['--version'], { env })).stdout, /^git version /)
  if (process.platform !== 'win32') {
    try {
      const qemu = await systemExecutable('qemu-system-x86_64', env)
      assert.ok(!qemu.startsWith(injected + path.sep))
    } catch (error) {
      assert.equal(error.code, 'ENOENT', 'Missing real QEMU must fail closed instead of selecting the PATH override')
    }
  }
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

test('preservation captures the pinned Lima canonical disk and still verifies its exact data bytes', async (t) => {
  const current = fixture(t)
  const installed = packageFixture(current)
  write(path.join(current.portoHome, 'porto.db'), 'SQLite format 3\0Synthetic populated state\n')
  write(path.join(current.portoHome, 'docker', 'engine.json'), '{"mode":"lima","ownerId":"synthetic"}\n')
  write(path.join(current.home, 'Preserved synthetic project', 'package.json'), '{"name":"synthetic"}\n')
  const disk = path.join(current.limaHome, 'porto-engine', 'disk')
  write(disk, 'Synthetic retained Lima 2.2 disk bytes\n')
  await snapshot(current, installed)
  assert.equal(current.snapshots[path.relative(current.directory, disk)], digest(disk))
  assert.equal(fs.existsSync(path.join(current.limaHome, 'porto-engine', 'diffdisk')), false)
  write(disk, 'Changed retained Lima disk bytes\n')
  await assert.rejects(restored(current), /changed stopped Porto state or real runtime data/)
})

test('upgrade/uninstall checks hash populated state and guest data rather than file existence', async (t) => {
  const current = fixture(t)
  const guestData = path.join(current.limaHome, 'porto-engine', 'disk')
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

test('shell opt-in is rejected before spawning, including when failures are otherwise allowed', async () => {
  let spawned = false
  for (const shell of [true, '/bin/sh', 'cmd.exe', '']) {
    await assert.rejects(run(process.execPath, ['-e', ''], { shell, allowFailure: true }, {
      spawnImpl: () => { spawned = true; throw new Error('Shell opt-in reached spawn') },
    }), { code: 'PORTO_COMMAND_SHELL_FORBIDDEN' })
  }
  assert.equal(spawned, false)
})

test('argv-only execution preserves shell metacharacters as literal argument data', async (t) => {
  const current = fixture(t)
  const args = ['with spaces é', 'value; echo unexpected', '$(echo unexpected)', 'value & echo unexpected']
  const result = await run(process.execPath, ['-e', 'process.stdout.write(JSON.stringify(process.argv.slice(1)))', ...args], {
    shell: false, env: environment(current),
  })
  assert.deepEqual(JSON.parse(result.stdout), args)
})

if (process.platform !== 'win32') {
  test('argv-only execution retains intentional /bin/sh script-file invocation', async (t) => {
    const current = fixture(t)
    const script = path.join(current.home, "explicit ; ' $script é.sh")
    write(script, '#!/bin/sh\nprintf "%s" "$1"\n')
    const argument = 'literal ; $(echo unexpected)'
    const result = await runShellFile(script, [argument], { env: environment(current) })
    assert.equal(result.stdout, argument)
    const outside = path.join(current.home, 'outside shell scripts')
    fs.symlinkSync(path.dirname(process.execPath), outside, 'dir')
    await assert.rejects(runShellFile(path.join(outside, path.basename(process.execPath))), {
      code: 'PORTO_COMMAND_UNTRUSTED_EXECUTABLE',
    })
  })
}

test('owned command diagnostics redact output before truncation and never serialize arguments', () => {
  const secret = 'synthetic-credential-must-not-be-logged'
  const logs = []
  reportCommandFailure(Object.assign(new Error('synthetic-private-argument'), {
    code: 'PORTO_COMMAND_FAILED',
    stderr: `failed synthetic build: https://user:${secret}@synthetic.invalid/?token=${secret}\n{"auth":"${secret}"}\n`,
    stdout: `Synthetic tool error E123\n${'x'.repeat(20000)}`,
  }), (message) => logs.push(message))
  assert.equal(logs.length, 1)
  assert.match(logs[0], /Synthetic tool error E123/)
  assert.ok(!logs[0].includes(secret))
  assert.ok(!logs[0].includes('synthetic-private-argument'))
  assert.ok(logs[0].length < 17000)
})

test('post-startup diagnostics retain bounded redacted owned logs without partial lines', (t) => {
  const current = fixture(t)
  const secret = 'synthetic-credential-must-not-be-logged'
  write(path.join(current.directory, 'desktop-launch.log'),
    `discarded partial line ${'x'.repeat(20000)}\nSynthetic workflow error E456\n` +
    `https://user:${secret}@synthetic.invalid/?token=${secret}\n{"auth":"${secret}"}\n`)
  const logs = []
  reportRuntimeDiagnostics(current, (message) => logs.push(message))
  assert.equal(logs.length, 1)
  assert.match(logs[0], /Synthetic workflow error E456/)
  assert.ok(!logs[0].includes(secret))
  assert.ok(!logs[0].includes('discarded partial line'))
  assert.ok(!logs[0].includes('x'.repeat(100)))
  assert.ok(logs[0].length < 17000)
})

test('unsuccessful commands retain only a fixed public tool category and numeric exit status', async () => {
  const secret = 'synthetic-argument-must-not-be-logged'
  await assert.rejects(run(process.execPath, ['-e', 'process.exit(42)', secret]), (error) => {
    assert.equal(error.code, 'PORTO_COMMAND_FAILED')
    assert.equal(error.commandKind, 'node')
    assert.equal(error.status, 42)
    const message = failureDiagnostic(error)
    assert.match(message, /Node acceptance helper.*42/)
    assert.ok(!message.includes(secret))
    return true
  })
})

test('public failure diagnostics use fixed actionable categories without serializing errors or credentials', () => {
  const secret = 'synthetic-credential-must-not-be-logged'
  for (const code of [
    'PORTO_COMMAND_TIMEOUT', 'PORTO_COMMAND_OUTPUT_LIMIT', 'PORTO_COMMAND_IO',
    'PORTO_COMMAND_SHELL_FORBIDDEN', 'PORTO_PROCESS_INSPECTION_UNAVAILABLE',
    'PORTO_COMMAND_UNTRUSTED_EXECUTABLE', 'PORTO_COMMAND_FAILED',
    'ENOENT', 'EACCES', 'EPERM', 'ENOSPC', 'ERR_ASSERTION', secret, undefined,
  ]) {
    const error = Object.assign(new Error(`https://user:${secret}@synthetic.invalid/?token=${secret}`), {
      code, stdout: secret, stderr: secret, stack: secret,
      cause: new Error(secret),
      toString() { throw new Error(secret) },
      [Symbol.for('nodejs.util.inspect.custom')]() { throw new Error(secret) },
    })
    const diagnostic = failureDiagnostic(error)
    assert.equal(typeof diagnostic, 'string')
    assert.ok(diagnostic.length > 40)
    assert.ok(!diagnostic.includes(secret))
    assert.ok(!diagnostic.includes('https://'))
  }
  const inaccessibleCode = { get code() { throw new Error(secret) } }
  assert.equal(failureDiagnostic(inaccessibleCode), failureDiagnostic(undefined))
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
  await identity(process.pid, env)
  const script = path.join(current.home, 'owned-process-tree.cjs')
  const record = path.join(current.home, 'owned-pids.json')
  const recordedIdentities = new Map()
  let unrelated
  t.after(async () => {
    try {
      if (fs.existsSync(record)) {
        const { pids } = JSON.parse(fs.readFileSync(record, 'utf8'))
        for (const pid of pids.reverse()) {
          const value = await identity(pid, env)
          if (value && value === recordedIdentities.get(pid)) {
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
      const ready = setInterval(() => {
        if (!fs.existsSync(record + '.proceed')) return
        clearInterval(ready)
        if (mode === 'stdout' || mode === 'stderr') process[mode].write(Buffer.alloc(32768, 120))
        if (mode === 'exited') process.exit(0)
      }, 10)
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
  const command = run(process.execPath, [script, 'root', mode, record, current.id], {
    cwd: current.home, env, allowFailure: true, timeout, cleanupTimeout: 3000, maxBuffer: 4096,
  }).then((result) => ({ result }), (error) => ({ error }))
  try {
    await waitFor(() => fs.existsSync(record), 'synthetic process tree readiness', timeout)
    const { pids } = JSON.parse(fs.readFileSync(record, 'utf8'))
    for (const pid of pids) {
      const value = await identity(pid, env)
      assert.ok(value, 'The owned process generation must be recorded before interruption')
      recordedIdentities.set(pid, value)
    }
    write(record + '.proceed', 'ready')
    const outcome = await Promise.race([
      command,
      new Promise((_, reject) => {
        deadline = setTimeout(() => reject(new Error('The command regression did not settle')), timeout + 5000)
      }),
    ])
    failure = outcome.error
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
        assert.equal(options.shell, false)
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

test('native identity works under synthetic HOME and distinguishes an exited or reused PID', async (t) => {
  const current = fixture(t)
  const env = environment(current)
  const value = await identity(process.pid, env)
  assert.ok(value)
  if (process.platform === 'win32') assert.ok(Array.isArray(await hostProcesses(env)))
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], {
    env, cwd: current.home, stdio: 'ignore',
  })
  t.after(() => stopSynthetic(child))
  const original = await identity(child.pid, env)
  assert.ok(original)
  await stopSynthetic(child)
  assert.notEqual(await identity(child.pid, env), original)
})

test('Linux kernel identity ignores mutable comm and verifies executable/start generation', () => {
  const stat = (start, name = 'node', state = 'S') => {
    const fields = Array(20).fill('0')
    fields[0] = state
    fields[19] = start
    return `123 (${name}) ${fields.join(' ')}\n`
  }
  const probe = (states, images = ['/trusted/node', '/trusted/node'], inodes = [2n, 2n]) => {
    let read = 0
    let link = 0
    let metadata = 0
    return {
      readFileSync: () => states[Math.min(read++, states.length - 1)],
      readlinkSync: () => images[link++],
      statSync: () => ({ dev: 1n, ino: inodes[metadata++], nlink: 1n }),
    }
  }
  const token = linuxProcessIdentity(123, { fsImpl: probe([stat('100'), stat('100', 'Porto) renamed\n title')]) })
  assert.deepEqual(JSON.parse(token), {
    PID: 123, StartTimeTicks: '100', ExecutablePath: '/trusted/node', ExecutableDevice: '1', ExecutableInode: '2',
  })
  assert.throws(() => linuxProcessIdentity(123, { fsImpl: probe([stat('100'), stat('101')]) }), /generation changed/)
  assert.throws(() => linuxProcessIdentity(123, { fsImpl: probe([stat('100')], ['/trusted/node', '/different/node']) }), /executable changed/i)
  assert.throws(() => linuxProcessIdentity(123, { fsImpl: probe([stat('100')], undefined, [2n, 3n]) }), /executable changed/i)
  assert.equal(linuxProcessIdentity(123, { fsImpl: probe([stat('100', 'node', 'Z')]) }), '')
})

test('Unix inspection errors and unavailable procfs never become missing-process results', async () => {
  const missing = Object.assign(new Error('synthetic proc entry absent'), { code: 'ENOENT' })
  const fsImpl = { readFileSync: () => { throw missing } }
  assert.equal(linuxProcessIdentity(123, {
    fsImpl, signalImpl: () => { throw Object.assign(new Error('missing PID'), { code: 'ESRCH' }) },
  }), '')
  assert.throws(() => linuxProcessIdentity(123, { fsImpl, signalImpl: () => true }), /proc entry absent/)
  assert.throws(() => linuxProcessIdentity(123, {
    fsImpl, signalImpl: () => { throw Object.assign(new Error('permission unavailable'), { code: 'EPERM' }) },
  }), /permission unavailable/)
  for (const code of ['EACCES', 'EPERM', 'EIO']) {
    const logs = []
    await assert.rejects(identity(123, {}, {
      platform: 'linux', linuxImpl: () => { throw Object.assign(new Error('inspection failed'), { code }) },
      logImpl: (message) => logs.push(message),
    }), { code: 'PORTO_PROCESS_INSPECTION_UNAVAILABLE' })
    assert.match(logs[0], /Process identity inspection is unavailable/)
  }
})

if (process.platform !== 'win32') {
  test('mutable process.title preserves kernel identity and teardown signals only the owned generation', async (t) => {
    const current = fixture(t)
    const env = environment(current)
    await identity(process.pid, env)
    const child = spawn(process.execPath, ['-e', `
process.on('message', () => { process.title = 'Porto'; process.send('renamed') })
process.send('ready')
setInterval(() => {}, 1000)
`], { env, cwd: current.home, stdio: ['ignore', 'ignore', 'ignore', 'ipc'] })
    const unrelated = spawn(process.execPath, ['-e', 'process.send("ready"); setInterval(() => {}, 1000)'], {
      env, cwd: current.home, stdio: ['ignore', 'ignore', 'ignore', 'ipc'],
    })
    t.after(() => stopSynthetic(child))
    t.after(() => stopSynthetic(unrelated))
    await Promise.all([once(child, 'message'), once(unrelated, 'message')])
    const original = await identity(child.pid, env)
    assert.equal(fs.realpathSync(JSON.parse(original).ExecutablePath), fs.realpathSync(process.execPath))
    const signals = []
    const signalImpl = (pid, signal) => { signals.push([pid, signal]); process.kill(pid, signal) }
    await stopProcess({ pid: unrelated.pid, identity: original }, env, false, { signalImpl })
    const wrongPath = JSON.parse(original)
    wrongPath.ExecutablePath = path.join(current.home, 'unrelated-executable')
    await stopProcess({ pid: child.pid, identity: JSON.stringify(wrongPath) }, env, false, { signalImpl })
    assert.deepEqual(signals, [])
    child.send('rename')
    await once(child, 'message')
    assert.equal(await identity(child.pid, env), original, 'A display title must not change native process ownership')
    const exited = once(child, 'exit')
    await stopProcess({ pid: child.pid, identity: original }, env, false, { signalImpl })
    await waitFor(() => child.exitCode !== null || child.signalCode !== null, 'owned titled process exit', 3000)
    await exited
    assert.deepEqual(signals, [[child.pid, 'SIGTERM']])
    assert.equal(unrelated.exitCode, null)
    assert.equal(unrelated.signalCode, null)
    assert.equal(await running(unrelated.pid, env), true)
  })
}

test('unknown ownership inspection refuses teardown rather than signaling a PID', async () => {
  const signals = []
  await assert.rejects(stopProcess({ pid: 123, identity: 'owned-generation' }, {}, false, {
    identityImpl: async () => { throw new Error('specific process inspection unavailable') },
    signalImpl: (pid, signal) => signals.push([pid, signal]),
  }), /inspection unavailable/)
  assert.deepEqual(signals, [])
})

test('bounded sanitized Lima/QEMU diagnostics survive instance deletion', (t) => {
  const current = fixture(t)
  const instance = path.join(current.limaHome, 'porto-engine')
  const secret = 'synthetic-runtime-token-must-not-be-retained'
  for (const name of ['ha.stderr.log', 'qemu.stderr.log']) {
    write(path.join(instance, name), 'oversized diagnostic prefix '.repeat(1000) +
      `\nunderlying synthetic QEMU failure\nhttps://user:${secret}@synthetic.invalid/image?token=${secret}\n`)
  }
  const descriptors = new Map()
  t.after(() => {
    for (const entry of descriptors.values()) fs.closeSync(entry.descriptor)
  })
  retainRuntimeDiagnostics(current, descriptors)
  const snapshots = new Map()
  for (const name of ['ha.stderr.log', 'qemu.stderr.log']) {
    const file = path.join(current.directory, 'diagnostics', name)
    const bytes = fs.readFileSync(file)
    assert.ok(bytes.length <= 16384)
    assert.match(bytes.toString(), /underlying synthetic QEMU failure/)
    assert.ok(!bytes.toString().includes(secret))
    assert.ok(!bytes.toString().includes('https://'))
    fs.appendFileSync(path.join(instance, name), 'final underlying QEMU stderr before deletion\n')
  }
  fs.rmSync(instance, { recursive: true })
  retainRuntimeDiagnostics(current, descriptors)
  assert.equal(descriptors.size, 0)
  for (const name of ['ha.stderr.log', 'qemu.stderr.log']) {
    const bytes = fs.readFileSync(path.join(current.directory, 'diagnostics', name))
    snapshots.set(name, bytes)
    assert.match(bytes.toString(), /final underlying QEMU stderr before deletion/)
    assert.ok(bytes.length <= 16384)
    assert.ok(!bytes.toString().includes(secret))
  }
  retainRuntimeDiagnostics(current, descriptors)
  for (const [name, bytes] of snapshots) {
    assert.deepEqual(fs.readFileSync(path.join(current.directory, 'diagnostics', name)), bytes)
  }
})

test('Windows native identity requires an explicit matching missing PID and complete start/executable identity', async () => {
  const packet = (document) => [
    'PORTO_PROCESS_IDENTITY_V1', String(document.pid),
    ...(document.exists === false ? ['missing'] : [
      'present', document.identity?.StartTimeTicks || '',
      Buffer.from(document.identity?.ExecutablePath || '', 'utf16le').toString('base64'),
    ]),
  ].join('\r\n') + '\r\n'
  const result = (document) => ({ status: 0, signal: null, stdout: packet(document), stderr: '' })
  const fields = { StartTimeTicks: '639287208000000000', ExecutablePath: 'C:\\Synthetic space 路径\\Porto 🚀.exe' }
  assert.equal(await identity(123, {}, {
    platform: 'win32', runImpl: async () => result({ pid: 123, exists: false }),
  }), '')
  assert.equal(await identity(123, {}, {
    platform: 'win32', runImpl: async () => result({ pid: 123, exists: true, identity: fields }),
  }), JSON.stringify(fields))
  for (const changed of [
    { ...fields, StartTimeTicks: '639287208000000001' },
    { ...fields, ExecutablePath: 'C:\\Synthetic\\different.exe' },
  ]) {
    assert.notEqual(await identity(123, {}, {
      platform: 'win32', runImpl: async () => result({ pid: 123, exists: true, identity: changed }),
    }), JSON.stringify(fields))
  }
  for (const response of [
    { status: 1, signal: null, stdout: '', stderr: 'Native process inspection unavailable' },
    { status: 0, signal: null, stdout: '', stderr: '' },
    { status: 0, signal: null, stdout: '{}', stderr: 'Native process access denied' },
    result({ pid: 999, exists: false }),
    result({ pid: 123, exists: true, identity: { ...fields, ExecutablePath: '' } }),
    result({ pid: 123, exists: true, identity: { ...fields, StartTimeTicks: 'invalid' } }),
    { ...result({ pid: 123, exists: false }), stdout: packet({ pid: 123, exists: false }) + 'unexpected\r\n' },
    { status: 0, signal: null, stdout: 'PORTO_PROCESS_IDENTITY_V1\n123\npresent\n1\n!!!!\n', stderr: '' },
    { status: 0, signal: null, stdout: 'PORTO_PROCESS_IDENTITY_V1\n123\npresent\n1\nAA==\n', stderr: '' },
    result({ pid: 123, exists: true, identity: { ...fields, ExecutablePath: 'relative.exe' } }),
  ]) {
    const logs = []
    await assert.rejects(identity(123, {}, {
      platform: 'win32', runImpl: async (_file, args) => {
        assert.match(args.at(-1), /\$ErrorActionPreference = 'Stop'/)
        assert.match(args.at(-1), /\$PSModuleAutoLoadingPreference = 'None'/)
        assert.match(args.at(-1), /\[System\.Diagnostics\.Process\]::GetProcessById\(123\)/)
        assert.match(args.at(-1), /\$target\.StartTime\.ToUniversalTime\(\)\.Ticks/)
        assert.match(args.at(-1), /\$target\.MainModule\.FileName/)
        return response
      }, logImpl: (message) => logs.push(message),
    }), { code: 'PORTO_PROCESS_INSPECTION_UNAVAILABLE' })
    assert.match(logs[0], /Process identity inspection is unavailable/)
  }
})

test('Windows native enumeration requires explicit PID/executable records without WMI or a user profile', async () => {
  const record = { ProcessId: 123, Name: 'Porto.exe', ExecutablePath: 'C:\\Synthetic space 路径\\resources\\porto.exe' }
  const packet = (records) => 'PORTO_PROCESS_LIST_V1\r\n' + records.map((item) =>
    `${item.ProcessId}\t${Buffer.from(item.ExecutablePath, 'utf16le').toString('base64')}\r\n`).join('')
  const runImpl = async (_file, args, options) => {
    assert.equal(options.timeout, 10000)
    assert.match(args.at(-1), /\[System\.Diagnostics\.Process\]::GetProcesses\(\)/)
    assert.match(args.at(-1), /\$PSModuleAutoLoadingPreference = 'None'/)
    return { stdout: packet([record]), stderr: '', status: 0, signal: null }
  }
  assert.deepEqual(await hostProcesses({}, { platform: 'win32', runImpl }), [record])
  assert.deepEqual(await hostProcesses({}, {
    platform: 'win32', runImpl: async () => ({ status: 0, signal: null, stdout: packet([]), stderr: '' }),
  }), [])
  for (const stdout of ['', '{}', packet([{ ...record, ProcessId: 0 }]), packet([{ ...record, ExecutablePath: '' }]),
    'PORTO_PROCESS_LIST_V1\n123\t!!!!\n', packet([record]) + 'unexpected\n']) {
    await assert.rejects(hostProcesses({}, {
      platform: 'win32', runImpl: async () => ({ status: 0, signal: null, stdout, stderr: '' }),
    }))
  }
  await assert.rejects(hostProcesses({}, {
    platform: 'win32', runImpl: async () => ({ status: 0, signal: null, stdout: packet([record]), stderr: 'unexpected warning' }),
  }))
})

test('identity inspection timeout remains an unavailable-inspection error rather than process exit', async () => {
  const timedOut = Object.assign(new Error('synthetic inspection timed out: synthetic-secret-must-not-be-logged'), { code: 'PORTO_COMMAND_TIMEOUT' })
  const logs = []
  await assert.rejects(identity(123, {}, {
    platform: 'win32',
    runImpl: async () => { throw timedOut }, logImpl: (message) => logs.push(message),
  }), (error) => {
    assert.equal(error.code, 'PORTO_PROCESS_INSPECTION_UNAVAILABLE')
    assert.equal(error.cause, timedOut)
    return true
  })
  assert.match(logs[0], /Process identity inspection is unavailable/)
  assert.ok(!logs[0].includes('synthetic-secret-must-not-be-logged'))
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
  assert.match(result.stderr, /clean disposable CI host/)
  assert.equal(fs.existsSync('.must-not-be-created'), false)
})

test('unknown acceptance commands do not execute a backend even in CI', async (t) => {
  const current = fixture(t)
  const result = await run(process.execPath, [
    path.join(__dirname, 'installed-acceptance.cjs'), 'unknown-acceptance-command', current.directory,
  ], { env: { ...process.env, GITHUB_ACTIONS: 'true' }, allowFailure: true })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /Installed-package acceptance failed/)
  preserved(current)
})

test('both executable entry points omit environment-derived error details from public stderr', async (t) => {
  const current = fixture(t)
  const secret = 'synthetic-environment-credential-not-for-public-errors'
  const env = { ...process.env, GITHUB_ACTIONS: 'true', PORTO_TEST_FAILURE_SECRET: secret }
  for (const [file, args, expected] of [
    ['installed-acceptance.cjs', [env.PORTO_TEST_FAILURE_SECRET, current.directory], failureDiagnostic(undefined)],
    ['installed-package.cjs', ['unused-artifact', '1.0.0', env.PORTO_TEST_FAILURE_SECRET, 'unused-fixture'], failureDiagnostic({ code: 'ERR_ASSERTION' })],
  ]) {
    const result = await run(process.execPath, [path.join(__dirname, file), ...args], { env, allowFailure: true })
    assert.equal(result.status, 1)
    assert.equal(result.stdout, '')
    assert.equal(result.stderr.trim(), expected)
    assert.ok(!result.stderr.includes(secret))
    assert.ok(!result.stderr.includes(' at '))
  }
  preserved(current)
})
