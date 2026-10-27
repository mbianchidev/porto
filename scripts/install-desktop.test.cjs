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
} = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-installer-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const bin = path.join(root, 'tools')
  const home = path.join(root, 'home')
  fs.mkdirSync(bin)
  fs.mkdirSync(home)
  for (const name of ['awk', 'cat', 'chmod', 'cp', 'dirname', 'gzip', 'head', 'ln', 'mkdir', 'mktemp', 'rm', 'sed', 'sh', 'shasum', 'sleep', 'tar']) {
    const result = spawnSync('/bin/sh', ['-c', `command -v ${name}`], { encoding: 'utf8' })
    assert.equal(result.status, 0, `required fixture tool ${name}: ${result.stderr}`)
    fs.symlinkSync(result.stdout.trim(), path.join(bin, name))
  }
  executable(path.join(bin, 'uname'), `case "$1" in -s) echo Linux;; -m) echo ${arch === 'amd64' ? 'x86_64' : 'aarch64'};; esac`)
  executable(path.join(bin, 'id'), 'echo 0')
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
