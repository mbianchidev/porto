const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  parseVersions,
  toolchainLayout,
  validateToolchainLayout,
  validateWSLToolchain,
} = require('./docker-toolchain-smoke.cjs')

function temporaryRuntime(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-docker-layout-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  fs.mkdirSync(path.join(root, 'licenses'), { recursive: true })
  return root
}

function writeLicenses(root) {
  for (const license of [
    'docker-cli.txt',
    'docker-cli-NOTICE.txt',
    'docker-cli-bundled-plugins.patch',
    'docker-compose.txt',
    'docker-compose-NOTICE.txt',
    'docker-buildx.txt',
    'docker-buildx-AUTHORS.txt',
    'dive.txt',
  ]) {
    fs.writeFileSync(path.join(root, 'licenses', license), 'license')
  }
}

function writeAnalysisTools(root, platform) {
  const suffix = platform === 'win32' ? '.exe' : ''
  for (const [tool, version] of [['syft', '1.55.0'], ['grype', '0.121.0']]) {
    fs.mkdirSync(path.join(root, 'bin'), { recursive: true })
    fs.writeFileSync(path.join(root, 'bin', tool + suffix), 'synthetic', { mode: 0o700 })
    fs.writeFileSync(path.join(root, 'licenses', `${tool}.txt`), 'license')
    fs.appendFileSync(path.join(root, 'VERSIONS'), `${tool} v${version}\n`)
  }
}

test('parses version metadata with platform limitation details', () => {
  const versions = parseVersions('docker 29.7.2\ndocker-compose not available because Docker CLI is not bundled for windows/arm64\ndive v0.13.1\n')
  assert.equal(versions.get('docker'), '29.7.2')
  assert.match(versions.get('docker-compose'), /windows\/arm64/)
  assert.equal(versions.get('dive'), 'v0.13.1')
})

test('rejects a supported bundle with a missing plugin', (t) => {
  const root = temporaryRuntime(t)
  fs.writeFileSync(path.join(root, 'VERSIONS'), 'docker 29.7.2\ndive v0.13.1\n')
  writeAnalysisTools(root, 'linux')
  const layout = toolchainLayout(root, 'linux')
  for (const file of [layout.launcher, layout.compose, layout.dive]) {
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, 'synthetic', { mode: 0o700 })
  }
  writeLicenses(root)
  assert.throws(
    () => validateToolchainLayout(root, { platform: 'linux' }),
    /missing executable buildx/,
  )
})

test('rejects Docker files on an explicitly unsupported platform', (t) => {
  const root = temporaryRuntime(t)
  fs.writeFileSync(path.join(root, 'VERSIONS'), 'docker not available for windows/arm64\ndive v0.13.1\n')
  writeAnalysisTools(root, 'win32')
  const layout = toolchainLayout(root, 'win32')
  fs.mkdirSync(path.dirname(layout.dive), { recursive: true })
  fs.writeFileSync(layout.dive, 'dive')
  fs.mkdirSync(path.dirname(layout.launcher), { recursive: true })
  fs.writeFileSync(layout.launcher, 'unexpected')
  assert.throws(
    () => validateToolchainLayout(root, { platform: 'win32' }),
    /unexpectedly contains/,
  )
})

test('requires analysis provenance and refuses an x64 Grype fallback on Windows ARM64', (t) => {
  const { localToolchain } = require('../ui/electron/local-toolchain.cjs')
  const root = temporaryRuntime(t)
  assert.throws(() => localToolchain(root, new Map()), /no version metadata/)
  writeAnalysisTools(root, 'win32')
  const versions = new Map([['syft', 'v1.55.0'], ['grype', 'not available for windows/arm64']])
  assert.throws(() => localToolchain(root, versions, { platform: 'win32' }), /unexpectedly contains/)
  fs.unlinkSync(path.join(root, 'bin', 'grype.exe'))
  assert.equal(localToolchain(root, versions, { platform: 'win32' }).length, 1)
})

test('WSL tools require native Linux ELF artifacts for each selected architecture', (t) => {
  for (const [architecture, machine] of [['amd64', 62], ['arm64', 183]]) {
    const root = temporaryRuntime(t)
    for (const name of ['bin/docker', 'bin/porto-wsl', 'docker/cli-plugins/docker-compose', 'docker/cli-plugins/docker-buildx']) {
      const file = path.join(root, 'wsl', architecture, name)
      const header = Buffer.alloc(24)
      header.set([0x7f, 0x45, 0x4c, 0x46, 2, 1])
      header.writeUInt16LE(machine, 18)
      fs.mkdirSync(path.dirname(file), { recursive: true })
      fs.writeFileSync(file, header)
    }
    assert.doesNotThrow(() => validateWSLToolchain(root, architecture))
    const helper = path.join(root, 'wsl', architecture, 'bin', 'porto-wsl')
    fs.writeFileSync(helper, Buffer.from('MZ-Windows-is-not-a-Linux-helper'))
    assert.throws(() => validateWSLToolchain(root, architecture), /native Linux ELF/)
  }
})
