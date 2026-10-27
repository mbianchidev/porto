const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const nodeTest = require('node:test')

const test = process.platform === 'win32' ? nodeTest.skip : nodeTest

const installer = path.join(__dirname, 'install-desktop.sh')

function executable(file, contents) {
  fs.writeFileSync(file, `#!/bin/sh\nset -eu\n${contents}\n`, { mode: 0o700 })
}

function fixture(t, {
  arch = 'amd64',
  packageResult = 'installed',
  packageManager = 'apt-get',
  skip = false,
  existing = '',
  imageTool = true,
  sandbox = 'unrestricted',
} = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-installer-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const bin = path.join(root, 'tools')
  const home = path.join(root, 'home')
  fs.mkdirSync(bin)
  fs.mkdirSync(home)
  for (const name of ['awk', 'basename', 'cat', 'chmod', 'cp', 'dirname', 'gzip', 'head', 'ln', 'mkdir', 'mktemp', 'rm', 'sed', 'sh', 'shasum', 'sleep', 'tar']) {
    const result = spawnSync('/bin/sh', ['-c', `command -v ${name}`], { encoding: 'utf8' })
    assert.equal(result.status, 0, `required fixture tool ${name}: ${result.stderr}`)
    fs.symlinkSync(result.stdout.trim(), path.join(bin, name))
  }
  executable(path.join(bin, 'uname'), `case "$1" in -s) echo Linux;; -m) echo ${arch === 'amd64' ? 'x86_64' : 'aarch64'};; esac`)
  executable(path.join(bin, 'id'), 'echo 1001')
  executable(path.join(bin, 'sysctl'), `
[ "$*" = "-n kernel.apparmor_restrict_unprivileged_userns" ]
if [ "$FIXTURE_SANDBOX" = unrestricted ]; then echo 0; else echo 1; fi`)
  executable(path.join(bin, 'apparmor_parser'), 'exit 0')
  executable(path.join(bin, 'sudo'), `
if [ "$1" = /bin/sh ]; then
  printf '%s\\n' "$@" > "$FIXTURE_ROOT/sandbox-command.log"
  cat > "$FIXTURE_ROOT/sandbox-setup.sh"
  if [ "$FIXTURE_SANDBOX" = failed ]; then
    echo 'synthetic AppArmor setup failure' >&2
    exit 42
  fi
else
  exec "$@"
fi`)
  // Never discover or stop real host daemons when exercising the Linux installer.
  executable(path.join(bin, 'tr'), 'exit 0')
  executable(path.join(bin, 'readlink'), 'exit 1')
  executable(path.join(bin, 'curl'), `
destination=""
while [ "$#" -gt 0 ]; do
  case "$1" in -o) destination="$2"; shift 2;; *) url="$1"; shift;; esac
done
case "$url" in */SHA256SUMS) cp "$FIXTURE_ROOT/SHA256SUMS" "$destination";; *) cp "$FIXTURE_ROOT/desktop.tar.gz" "$destination";; esac`)
  if (packageManager) executable(path.join(bin, packageManager), `
printf '%s\\n' "$*" >> "$FIXTURE_ROOT/packages.log"
case "$1" in
  update) exit 0;;
  install|-S|--non-interactive)
    case "$FIXTURE_PACKAGE_RESULT" in
      failed) echo 'synthetic package installation failure' >&2; exit 42;;
      missing) exit 0;;
      installed)
        printf '#!/bin/sh\\necho QEMU synthetic\\n' > "$FIXTURE_QEMU"
        chmod 700 "$FIXTURE_QEMU"
        if [ "$FIXTURE_IMAGE_TOOL" = "1" ]; then
          cp "$FIXTURE_QEMU" "$FIXTURE_QEMU_IMG"
        fi;;
    esac;;
esac`)
  const qemu = path.join(bin, arch === 'amd64' ? 'qemu-system-x86_64' : 'qemu-system-aarch64')
  const qemuImg = path.join(bin, 'qemu-img')
  if (existing === 'broken') executable(qemu, "echo 'synthetic executable failure' >&2; exit 7")
  if (existing === 'working') {
    executable(qemu, 'echo QEMU synthetic')
    if (imageTool) executable(qemuImg, 'echo QEMU synthetic')
  }
  if (existing === 'wrong-architecture') {
    executable(path.join(bin, arch === 'amd64' ? 'qemu-system-aarch64' : 'qemu-system-x86_64'), 'echo QEMU synthetic')
  }
  const source = path.join(root, `porto-desktop_fixture_linux_${arch}`)
  fs.mkdirSync(path.join(source, 'resources', 'runtime', 'bin'), { recursive: true })
  executable(path.join(source, 'Porto'), 'exit 0')
  executable(path.join(source, 'resources', 'porto'), 'exit 0')
  const archive = spawnSync('tar', ['-czf', path.join(root, 'desktop.tar.gz'), '-C', root, path.basename(source)], { encoding: 'utf8' })
  assert.equal(archive.status, 0, archive.stderr)
  const checksum = crypto.createHash('sha256').update(fs.readFileSync(path.join(root, 'desktop.tar.gz'))).digest('hex')
  fs.writeFileSync(path.join(root, 'SHA256SUMS'), `${checksum}  porto-desktop_fixture_linux_${arch}.tar.gz\n`)
  const installRoot = path.join(root, 'Installed Porto \u00e9')
  fs.mkdirSync(installRoot)
  fs.writeFileSync(path.join(installRoot, 'previous-installation'), 'synthetic previous installation')
  const result = spawnSync('/bin/sh', [installer], {
    encoding: 'utf8',
    timeout: 30000,
    env: {
      PATH: bin,
      HOME: home,
      TMPDIR: root,
      PORTO_VERSION: 'vfixture',
      PORTO_INSTALL_DIR: installRoot,
      PORTO_BIN_DIR: path.join(home, '.local', 'bin'),
      PORTO_NO_LAUNCH: '1',
      PORTO_SKIP_PREREQS: skip ? '1' : '0',
      FIXTURE_ROOT: root,
      FIXTURE_PACKAGE_RESULT: packageResult,
      FIXTURE_QEMU: qemu,
      FIXTURE_QEMU_IMG: qemuImg,
      FIXTURE_IMAGE_TOOL: imageTool ? '1' : '0',
      FIXTURE_SANDBOX: sandbox,
    },
  })
  assert.ifError(result.error)
  return { ...result, root, qemu, installRoot }
}

test('required Linux QEMU package failure reports incomplete setup instead of success', (t) => {
  const result = fixture(t, { packageResult: 'failed' })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(result.stderr, /incomplete.*QEMU|QEMU.*incomplete/is)
  assert.match(result.stderr, /managed container|container engine/i)
  assert.doesNotMatch(result.stdout, /Installed Porto at/)
  assert.equal(fs.readFileSync(path.join(result.installRoot, 'previous-installation'), 'utf8'), 'synthetic previous installation')
})

test('successful package installation must provide an executable host-architecture QEMU', (t) => {
  const result = fixture(t, { packageResult: 'missing' })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(result.stderr, /incomplete.*qemu-system-x86_64|qemu-system-x86_64.*incomplete/is)
  assert.doesNotMatch(result.stdout, /Installed Porto at/)
})

test('required Linux QEMU image utility must also execute', (t) => {
  const result = fixture(t, { imageTool: false })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(result.stderr, /incomplete.*qemu-img|qemu-img.*incomplete/is)
})

for (const arch of ['amd64', 'arm64']) {
  for (const packageManager of ['apt-get', 'dnf', 'pacman', 'zypper']) {
    test(`installs and verifies ${arch} QEMU through ${packageManager}`, (t) => {
      const result = fixture(t, { arch, packageManager })
      assert.equal(result.status, 0, result.stderr)
      assert.match(result.stdout, /Installed Porto at/)
      assert.equal(fs.existsSync(result.qemu), true)
      assert.equal(fs.existsSync(path.join(result.installRoot, 'resources', 'porto')), true)
    })
  }
}

test('does not accept only the other architecture emulator', (t) => {
  const result = fixture(t, { existing: 'wrong-architecture', packageResult: 'failed' })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(fs.readFileSync(path.join(result.root, 'packages.log'), 'utf8'), /install/)
})

test('a broken existing emulator fails before replacing Porto', (t) => {
  const result = fixture(t, { existing: 'broken' })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(result.stderr, /synthetic executable failure/)
  assert.match(result.stderr, /incomplete/i)
  assert.equal(fs.existsSync(path.join(result.installRoot, 'previous-installation')), true)
})

test('an explicit prerequisite opt-out is reported separately from verified installation', (t) => {
  const result = fixture(t, { skip: true, packageResult: 'failed' })
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stderr, /explicitly skipped.*PORTO_SKIP_PREREQS=1/)
  assert.match(result.stdout, /verification was explicitly skipped/)
  assert.doesNotMatch(result.stdout, /Installed Porto at/)
  assert.equal(fs.existsSync(path.join(result.root, 'packages.log')), false)
})

test('an already working QEMU installation does not invoke a package manager', (t) => {
  const result = fixture(t, { existing: 'working', packageResult: 'failed' })
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.existsSync(path.join(result.root, 'packages.log')), false)
})

test('missing Linux package manager reports incomplete setup', (t) => {
  const result = fixture(t, { packageManager: '' })
  assert.notEqual(result.status, 0, result.stdout)
  assert.match(result.stderr, /incomplete.*QEMU/is)
  assert.equal(fs.existsSync(path.join(result.installRoot, 'previous-installation')), true)
})

test('the Linux desktop launcher quotes installed paths with spaces and non-ASCII names', (t) => {
  const result = fixture(t)
  assert.equal(result.status, 0, result.stderr)
  const desktop = fs.readFileSync(path.join(result.root, 'home', '.local', 'share', 'applications', 'porto.desktop'), 'utf8')
  assert.equal(desktop.split('\n').find((line) => line.startsWith('Exec=')), `Exec="${result.installRoot}/Porto"`)
})

test('restricted Linux namespaces request scoped AppArmor setup for the actual installed executable', (t) => {
  const result = fixture(t, { existing: 'working', sandbox: 'restricted' })
  assert.equal(result.status, 0, result.stderr)
  const command = fs.readFileSync(path.join(result.root, 'sandbox-command.log'), 'utf8').trim().split('\n')
  assert.deepEqual(command, ['/bin/sh', '-s', '--', `${fs.realpathSync(result.installRoot)}/Porto`, '1001'])
  const setup = fs.readFileSync(path.join(result.root, 'sandbox-setup.sh'), 'utf8')
  assert.match(setup, /install_linux_sandbox_profile \/etc\/apparmor\.d "\$1" "\$2"/)
  assert.doesNotMatch(setup, /--no-sandbox|sysctl\s+-w|chown/)
})

test('sandbox setup failure preserves the previous installation and does not report success', (t) => {
  const result = fixture(t, { existing: 'working', sandbox: 'failed' })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /synthetic AppArmor setup failure/)
  assert.match(result.stderr, /setup incomplete.*sandbox/is)
  assert.doesNotMatch(result.stdout, /Installed Porto at/)
  assert.equal(fs.readFileSync(path.join(result.installRoot, 'previous-installation'), 'utf8'), 'synthetic previous installation')
})

test('unrestricted hosts and explicit prerequisite opt-outs do not change AppArmor policy', (t) => {
  for (const options of [{ existing: 'working' }, { skip: true, sandbox: 'restricted' }]) {
    const result = fixture(t, options)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(fs.existsSync(path.join(result.root, 'sandbox-command.log')), false)
  }
})

function sandboxPolicy(t, { executablePath = '/synthetic/Porto installed 路径/Porto', uid = '1001',
  existing = '', parserFailure = '', repeat = false } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-sandbox-policy-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const bin = path.join(root, 'bin')
  const profiles = path.join(root, 'profiles')
  fs.mkdirSync(bin)
  fs.mkdirSync(profiles)
  for (const name of ['awk', 'chmod', 'cmp', 'ln', 'mktemp', 'rm', 'tr']) {
    const result = spawnSync('/bin/sh', ['-c', `command -v ${name}`], { encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
    fs.symlinkSync(result.stdout.trim(), path.join(bin, name))
  }
  executable(path.join(bin, 'sha256sum'), 'exec /usr/bin/shasum -a 256 "$@"')
  executable(path.join(bin, 'apparmor_parser'), `
printf '%s\\n' "$1" >> "$POLICY_ROOT/parser.log"
if [ "$POLICY_FAILURE" = "$1" ]; then echo 'synthetic AppArmor parser failure' >&2; exit 42; fi`)
  const source = fs.readFileSync(installer, 'utf8')
  const start = source.indexOf('install_linux_sandbox_profile() {')
  assert.notEqual(start, -1, 'The same privileged policy function must be exercised without changing this host')
  const end = source.indexOf('\n}\n', start)
  assert.notEqual(end, -1)
  const body = source.slice(start, end + 2)
  const profile = path.join(profiles, `porto-sandbox-${uid}-${crypto.createHash('sha256').update(executablePath).digest('hex')}`)
  const unrelated = path.join(root, 'unrelated-admin-policy')
  fs.writeFileSync(unrelated, 'Synthetic unrelated administrator policy\n')
  if (existing === 'symlink') fs.symlinkSync(unrelated, profile)
  if (existing === 'custom') fs.writeFileSync(profile, 'Synthetic custom administrator profile\n')
  const execute = () => spawnSync('/bin/sh', ['-c', `set -eu\n${body}\ninstall_linux_sandbox_profile "$1" "$2" "$3"`,
    'sandbox-policy-test', profiles, executablePath, uid], {
    encoding: 'utf8', timeout: 5000,
    env: { PATH: bin, POLICY_ROOT: root, POLICY_FAILURE: parserFailure },
  })
  const result = execute()
  assert.ifError(result.error)
  const inode = fs.existsSync(profile) ? fs.statSync(profile).ino : null
  const repeated = repeat ? execute() : null
  if (repeated) assert.ifError(repeated.error)
  return { ...result, root, profile, unrelated, inode, repeated, profiles }
}

test('the elevated policy creates only a literal user-namespace permission and reloads it without replacing the file', (t) => {
  const result = sandboxPolicy(t, { repeat: true })
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.repeated.status, 0, result.repeated.stderr)
  assert.equal(fs.statSync(result.profile).ino, result.inode)
  assert.equal(fs.statSync(result.profile).mode & 0o777, 0o644)
  const policy = fs.readFileSync(result.profile, 'utf8')
  assert.match(policy, /"\/synthetic\/Porto installed 路径\/Porto" flags=\(unconfined\)/)
  assert.match(policy, /\{\n  userns,\n\}\n$/)
  assert.doesNotMatch(policy, /capability|\/\*\*|change_profile/)
  assert.equal(fs.readFileSync(path.join(result.root, 'parser.log'), 'utf8'), '-Q\n-a\n-Q\n-r\n')
  assert.deepEqual(fs.readdirSync(result.profiles), [path.basename(result.profile)])
})

test('the elevated policy refuses symlinks, custom policy contents, unsafe paths and nonnumeric account IDs', (t) => {
  for (const options of [
    { existing: 'symlink' }, { existing: 'custom' }, { uid: '1001;unexpected' },
    ...['relative/Porto', '/synthetic/*/Porto', '/synthetic/[user]/Porto',
      '/synthetic/" injected/Porto', '/synthetic/line\nbreak/Porto',
      '/synthetic/line\rbreak/Porto', '/synthetic/back\\slash/Porto'].map((executablePath) => ({ executablePath })),
  ]) {
    const result = sandboxPolicy(t, options)
    assert.notEqual(result.status, 0, JSON.stringify(options))
    assert.equal(fs.readFileSync(result.unrelated, 'utf8'), 'Synthetic unrelated administrator policy\n')
    if (options.existing === 'symlink') assert.ok(fs.lstatSync(result.profile).isSymbolicLink())
    else if (options.existing === 'custom') {
      assert.equal(fs.readFileSync(result.profile, 'utf8'), 'Synthetic custom administrator profile\n')
    } else assert.deepEqual(fs.readdirSync(result.profiles), [])
  }
})

test('an AppArmor parser failure removes only a newly created policy and retains an existing policy', (t) => {
  for (const parserFailure of ['-Q', '-a', '-r']) {
    const result = sandboxPolicy(t, { parserFailure, repeat: parserFailure === '-r' })
    assert.equal((result.repeated || result).status, parserFailure === '-Q' ? 42 : 1)
    assert.match((result.repeated || result).stderr, /synthetic AppArmor parser failure/)
    if (parserFailure === '-r') {
      assert.equal(fs.statSync(result.profile).ino, result.inode)
    } else assert.deepEqual(fs.readdirSync(result.profiles), [])
    assert.equal(fs.readFileSync(result.unrelated, 'utf8'), 'Synthetic unrelated administrator policy\n')
  }
})
