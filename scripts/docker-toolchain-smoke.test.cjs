const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  parseVersions,
  toolchainLayout,
  validateToolchainLayout,
} = require('./docker-toolchain-smoke.cjs')

function temporaryRuntime(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-docker-layout-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  fs.mkdirSync(path.join(root, 'licenses'), { recursive: true })
  return root
}

test('parses version metadata with platform limitation details', () => {
  const versions = parseVersions('docker 29.7.2\ndocker-compose not available because Docker CLI is not bundled for windows/arm64\n')
  assert.equal(versions.get('docker'), '29.7.2')
  assert.match(versions.get('docker-compose'), /windows\/arm64/)
})

test('rejects a supported bundle with a missing plugin', (t) => {
  const root = temporaryRuntime(t)
  fs.writeFileSync(path.join(root, 'VERSIONS'), 'docker 29.7.2\n')
  const layout = toolchainLayout(root, 'linux')
  for (const file of [layout.launcher, layout.compose]) {
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, 'synthetic', { mode: 0o700 })
  }
  assert.throws(
    () => validateToolchainLayout(root, { platform: 'linux' }),
    /missing executable buildx/,
  )
})

test('rejects Docker files on an explicitly unsupported platform', (t) => {
  const root = temporaryRuntime(t)
  fs.writeFileSync(path.join(root, 'VERSIONS'), 'docker not available for windows/arm64\n')
  const layout = toolchainLayout(root, 'win32')
  fs.mkdirSync(path.dirname(layout.launcher), { recursive: true })
  fs.writeFileSync(layout.launcher, 'unexpected')
  assert.throws(
    () => validateToolchainLayout(root, { platform: 'win32' }),
    /unexpectedly contains/,
  )
})
