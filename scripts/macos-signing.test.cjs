const assert = require('node:assert/strict')
const { spawnSync } = require('node:child_process')
const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const nodeTest = require('node:test')

const { sealMacOSApplication, verifyMacOSDmg } = require('./macos-signing.cjs')

const nativeTest = process.platform === 'darwin' ? nodeTest : nodeTest.skip

function command(file, args, options = {}) {
  const result = spawnSync(file, args, { encoding: 'utf8', timeout: 120000, ...options })
  assert.ifError(result.error)
  assert.equal(result.status, 0, `${file} ${args.join(' ')}\n${result.stdout}\n${result.stderr}`)
  return result.stdout
}

function snapshot(root) {
  const entries = {}
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const file = path.join(root, entry.name)
    const mode = fs.lstatSync(file).mode & 0o777
    entries[entry.name] = entry.isSymbolicLink()
      ? { mode, target: fs.readlinkSync(file) }
      : entry.isDirectory()
        ? { mode, entries: snapshot(file) }
        : { mode, sha256: crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex') }
  }
  return entries
}

function fixture(t, architecture) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-signing-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const app = path.join(root, 'payload', 'Porto.app')
  const contents = path.join(app, 'Contents')
  const resources = path.join(contents, 'Resources')
  fs.mkdirSync(path.join(contents, 'MacOS'), { recursive: true })
  fs.mkdirSync(path.join(resources, 'runtime', 'bin'), { recursive: true })
  const plist = (name, identifier) => `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>${name}</string>
<key>CFBundleIdentifier</key><string>${identifier}</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>7.8.9</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSMinimumSystemVersion</key><string>13.0</string>
</dict></plist>
`
  fs.writeFileSync(path.join(contents, 'Info.plist'), plist('Porto', 'dev.mbianchi.porto'))
  const source = path.join(root, 'fixture.c')
  fs.writeFileSync(source, 'int main(void) { return 0; }\n')
  const executable = path.join(contents, 'MacOS', 'Porto')
  command('xcrun', ['clang', '-arch', architecture, '-mmacosx-version-min=13.0', source, '-o', executable])
  for (const name of ['Porto Helper', 'Porto Helper (Renderer)', 'Porto Helper (GPU)']) {
    const helperContents = path.join(contents, 'Frameworks', `${name}.app`, 'Contents')
    fs.mkdirSync(path.join(helperContents, 'MacOS'), { recursive: true })
    fs.copyFileSync(executable, path.join(helperContents, 'MacOS', name))
    fs.writeFileSync(path.join(helperContents, 'Info.plist'), plist(name, `dev.synthetic.helper.${name.length}`))
  }
  const framework = path.join(contents, 'Frameworks', 'Synthetic.framework')
  const frameworkVersion = path.join(framework, 'Versions', 'A')
  fs.mkdirSync(path.join(frameworkVersion, 'Resources'), { recursive: true })
  command('xcrun', ['clang', '-arch', architecture, '-dynamiclib', '-mmacosx-version-min=13.0', source,
    '-o', path.join(frameworkVersion, 'Synthetic')])
  fs.writeFileSync(path.join(frameworkVersion, 'Resources', 'Info.plist'),
    plist('Synthetic', 'dev.synthetic.framework').replace('APPL', 'FMWK'))
  fs.symlinkSync('A', path.join(framework, 'Versions', 'Current'))
  fs.symlinkSync('Versions/Current/Synthetic', path.join(framework, 'Synthetic'))
  fs.symlinkSync('Versions/Current/Resources', path.join(framework, 'Resources'))
  fs.copyFileSync(executable, path.join(resources, 'porto'))
  fs.copyFileSync(executable, path.join(resources, 'runtime', 'bin', 'synthetic-tool'))
  fs.writeFileSync(path.join(resources, 'runtime', 'bin', 'porto-runtime-helper'), 'synthetic Linux guest helper\n', { mode: 0o755 })
  fs.symlinkSync('synthetic-tool', path.join(resources, 'runtime', 'bin', 'tool-link'))
  fs.writeFileSync(path.join(resources, 'runtime', 'VERSIONS'), 'synthetic runtime manifest\n')
  fs.writeFileSync(path.join(resources, 'porto-release-version'), '7.8.9+fixture.1\n')
  fs.writeFileSync(path.join(resources, 'Assets.car'), 'synthetic binary icon resource\0')
  return { root, app, resources }
}

function policyFixture(t) {
  const app = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-signing-policy-'))
  t.after(() => fs.rmSync(app, { recursive: true, force: true }))
  fs.mkdirSync(path.join(app, 'Contents', 'Resources'), { recursive: true })
  return app
}

function installFixture(root, image, architecture) {
  const tools = path.join(root, 'installer-tools')
  const home = path.join(root, 'installer-home')
  const scratch = path.join(root, 'installer-tmp')
  for (const directory of [tools, home, scratch]) fs.mkdirSync(directory)
  for (const name of ['awk', 'cat', 'cp', 'ditto', 'hdiutil', 'head', 'ln', 'mkdir', 'mktemp', 'readlink', 'rm', 'sed', 'shasum']) {
    const executable = command('/bin/sh', ['-c', `command -v ${name}`]).trim()
    fs.symlinkSync(executable, path.join(tools, name))
  }
  const stub = (name, body) => fs.writeFileSync(path.join(tools, name), `#!/bin/sh\nset -eu\n${body}\n`, { mode: 0o755 })
  const controlLog = path.join(root, 'installer-controls.log')
  stub('uname', `case "$1" in -s) echo Darwin;; -m) echo ${architecture};; esac`)
  // Isolate every process-discovery/desktop-control path, not just the final launch.
  stub('ps', 'echo ps >> "$FIXTURE_CONTROL_LOG"')
  stub('osascript', 'echo osascript >> "$FIXTURE_CONTROL_LOG"')
  stub('sleep', 'exit 0')
  stub('open', 'echo forbidden-open >> "$FIXTURE_CONTROL_LOG"; exit 91')
  stub('xattr', 'echo forbidden-xattr >> "$FIXTURE_CONTROL_LOG"; exit 92')
  stub('curl', `
destination=""
while [ "$#" -gt 0 ]; do
  case "$1" in -o) destination="$2"; shift 2;; *) url="$1"; shift;; esac
done
case "$url" in */SHA256SUMS) cp "$FIXTURE_SUMS" "$destination";; *) cp "$FIXTURE_IMAGE" "$destination";; esac`)
  const goarch = architecture === 'x86_64' ? 'amd64' : 'arm64'
  const digest = crypto.createHash('sha256').update(fs.readFileSync(image)).digest('hex')
  const sums = path.join(root, 'SHA256SUMS')
  fs.writeFileSync(sums, `${digest}  porto-desktop_7.8.9+fixture.1_darwin_${goarch}.dmg\n`)
  const installDirectory = path.join(home, 'Synthetic Applications')
  command('/bin/sh', [path.join(__dirname, 'install-desktop.sh')], {
    env: {
      PATH: tools, HOME: home, TMPDIR: scratch,
      PORTO_VERSION: 'v7.8.9+fixture.1',
      PORTO_INSTALL_DIR: installDirectory,
      PORTO_BIN_DIR: path.join(home, 'bin'),
      PORTO_NO_LAUNCH: '1',
      FIXTURE_CONTROL_LOG: controlLog, FIXTURE_SUMS: sums, FIXTURE_IMAGE: image,
    },
  })
  assert.equal(fs.readFileSync(controlLog, 'utf8'), 'osascript\nps\n')
  assert.equal(fs.readdirSync(scratch).length, 0)
  return path.join(installDirectory, 'Porto.app')
}

nodeTest('preserves an existing verified certificate signature instead of replacing it with ad-hoc signing', async (t) => {
  const app = policyFixture(t)
  const calls = []
  await sealMacOSApplication(app, {
    run: (file, args) => {
      calls.push({ file, args })
      return { status: 0, stdout: '', stderr: 'Authority=Developer ID Application: Synthetic Signer\n' }
    },
    sign: () => assert.fail('must not replace a certificate signature'),
  })
  assert.equal(calls.length, 2)
  assert.deepEqual(calls[1].args, ['--verify', '--deep', '--strict', app])
})

nodeTest('fails on a damaged certificate signature without an ad-hoc downgrade', async (t) => {
  const app = policyFixture(t)
  await assert.rejects(sealMacOSApplication(app, {
    run: (file, args) => args.includes('--display')
      ? { status: 0, stderr: 'Authority=Developer ID Application: Synthetic Signer\n' }
      : { status: 1, stderr: 'a sealed resource is missing or invalid' },
    sign: () => assert.fail('must not hide a damaged certificate signature'),
  }), /sealed resource is missing or invalid/)
})

nodeTest('reports unexpected signature-inspection errors instead of treating them as unsigned input', async (t) => {
  const app = policyFixture(t)
  await assert.rejects(sealMacOSApplication(app, {
    run: () => ({ status: 1, stderr: 'synthetic signature inspection denied' }),
    sign: () => assert.fail('must not sign input that could not be inspected'),
  }), /synthetic signature inspection denied/)
})

nodeTest('passes an explicit signing identity and keychain through, without falling back after signing failure', async (t) => {
  const app = policyFixture(t)
  const identity = 'Developer ID Application: Synthetic Signer'
  const keychain = '/synthetic/signing.keychain-db'
  let attempts = 0
  await assert.rejects(sealMacOSApplication(app, {
    identity,
    keychain,
    run: () => ({ status: 1, stderr: 'code object is not signed at all' }),
    sign: async (options) => {
      attempts += 1
      assert.equal(options.identity, identity)
      assert.equal(options.keychain, keychain)
      assert.equal(options.identityValidation, true)
      assert.equal(options.optionsForFile(app).hardenedRuntime, true)
      assert.equal(options.optionsForFile(app).timestamp, undefined)
      throw new Error('synthetic unavailable signing identity')
    },
  }), /synthetic unavailable signing identity/)
  assert.equal(attempts, 1)
})

nodeTest('does not silently ignore certificate-import credentials on the prepackaged path', () => {
  const result = spawnSync(process.execPath, [path.join(__dirname, 'macos-signing.cjs'), '--sign', '/synthetic/Porto.app'], {
    encoding: 'utf8',
    timeout: 10000,
    env: { ...process.env, CSC_LINK: 'synthetic-certificate.p12', CSC_NAME: '' },
  })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /CSC_NAME.*CSC_KEYCHAIN/)
  assert.match(result.stderr, /refus.*ad-hoc/i)
})

for (const architecture of ['x86_64', 'arm64']) {
  nativeTest(`seals ${architecture} nested code without changing runtime bytes, links or modes`, async (t) => {
    const { app, resources } = fixture(t, architecture)
    assert.notEqual(spawnSync('codesign', ['--verify', '--deep', '--strict', app], { timeout: 30000 }).status, 0)
    const before = snapshot(resources)

    await sealMacOSApplication(app)

    command('codesign', ['--verify', '--deep', '--strict', app])
    assert.deepEqual(snapshot(resources), before)
    for (const bundle of [app, ...fs.readdirSync(path.join(app, 'Contents', 'Frameworks'))
      .filter((name) => name.endsWith('.app')).map((name) => path.join(app, 'Contents', 'Frameworks', name))]) {
      const entitlements = JSON.parse(command('plutil', ['-convert', 'json', '-o', '-', '--', '-'], {
        input: command('codesign', ['--display', '--entitlements', '-', '--xml', bundle]),
      }))
      for (const key of ['allow-jit', 'allow-unsigned-executable-memory', 'disable-library-validation']) {
        assert.equal(entitlements[`com.apple.security.cs.${key}`], true)
      }
    }
    fs.appendFileSync(path.join(resources, 'runtime', 'VERSIONS'), 'tampered\n')
    assert.notEqual(spawnSync('codesign', ['--verify', '--deep', '--strict', app], { timeout: 30000 }).status, 0)
  })

  nativeTest(`verifies the sealed ${architecture} app extracted from a read-only DMG and metadata-free tar`, async (t) => {
    const { root, app, resources } = fixture(t, architecture)
    await sealMacOSApplication(app)
    const goarch = architecture === 'x86_64' ? 'amd64' : 'arm64'
    const image = path.join(root, 'synthetic.dmg')
    command('hdiutil', ['create', '-quiet', '-srcfolder', path.dirname(app), '-format', 'UDZO', image])
    verifyMacOSDmg(image, goarch, '7.8.9+fixture.1')
    assert.throws(() => verifyMacOSDmg(image, goarch === 'amd64' ? 'arm64' : 'amd64', '7.8.9+fixture.1'), /architecture/)
    assert.throws(() => verifyMacOSDmg(image, goarch, '7.8.10'), /version/)

    const installed = installFixture(root, image, architecture)
    command('codesign', ['--verify', '--deep', '--strict', installed])
    assert.deepEqual(snapshot(path.join(installed, 'Contents', 'Resources')), snapshot(resources))

    const archive = path.join(root, 'synthetic.tar.gz')
    command('tar', ['--no-mac-metadata', '-czf', archive, '-C', path.dirname(app), 'Porto.app'])
    const extracted = path.join(root, 'extracted')
    fs.mkdirSync(extracted)
    command('tar', ['-xzf', archive, '-C', extracted])
    command('codesign', ['--verify', '--deep', '--strict', path.join(extracted, 'Porto.app')])
    assert.deepEqual(snapshot(path.join(extracted, 'Porto.app', 'Contents', 'Resources')), snapshot(resources))

    fs.appendFileSync(path.join(resources, 'runtime', 'VERSIONS'), 'tampered\n')
    const damaged = path.join(root, 'damaged.dmg')
    command('hdiutil', ['create', '-quiet', '-srcfolder', path.dirname(app), '-format', 'UDZO', damaged])
    assert.throws(() => verifyMacOSDmg(damaged, goarch, '7.8.9+fixture.1'), /sealed resource|invalid|modified/)
  })
}
