const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { gzipSync } = require('node:zlib')

const { prepareSnapshot } = require('./ci-ubuntu-snapshot.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-ci-snapshot-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const files = new Map()
  const digest = (bytes) => crypto.createHash('sha256').update(bytes).digest('hex')
  const packageFile = 'pool/main/s/synthetic/synthetic_1.0_arm64.deb'
  const packageBytes = Buffer.from('Synthetic package bytes, never installed')
  files.set(packageFile, packageBytes)
  const index = gzipSync([
    'Package: qemu-user-binfmt', 'Version: 1.0', 'Architecture: arm64',
    `Filename: ${packageFile}`, `Size: ${packageBytes.length}`, `SHA256: ${digest(packageBytes)}`, '',
  ].join('\n') + '\n')
  const indexPath = `dists/noble/main/binary-arm64/by-hash/SHA256/${digest(index)}`
  files.set(indexPath, index)
  const release = Buffer.from([
    '-----BEGIN PGP SIGNED MESSAGE-----', 'Hash: SHA256', '',
    'Codename: noble', 'SHA256:',
    ` ${digest(index)} ${index.length} main/binary-arm64/Packages.gz`,
    '-----BEGIN PGP SIGNATURE-----', 'Synthetic signature; the guest APT still authenticates real archives.',
    '-----END PGP SIGNATURE-----', '',
  ].join('\n'))
  files.set('dists/noble/InRelease', release)
  const calls = []
  const download = async (relative, destination) => {
    calls.push(relative)
    if (!files.has(relative)) throw new Error(`Unexpected snapshot download: ${relative}`)
    fs.writeFileSync(destination, files.get(relative), { flag: 'wx' })
  }
  const options = {
    date: new Date('2026-11-11T12:00:00Z'), suites: ['noble'], components: ['main'],
    cacheDirectory: path.join(directory, 'cache'), download,
  }
  return { directory, files, options, calls, digest, packageFile, packageBytes, index, indexPath, release }
}

test('prepares immutable APT inputs before boot and serves their exact bytes without upstream access', async (t) => {
  const f = fixture(t)
  const repository = await prepareSnapshot('arm64', f.options)
  t.after(() => repository.close())
  assert.equal(repository.snapshot, '20261108T000000Z')
  const requests = f.calls.length
  f.files.clear()
  for (const [relative, expected] of [
    ['dists/noble/InRelease', f.release],
    [`dists/noble/main/binary-arm64/by-hash/SHA256/${f.digest(f.index)}`, f.index],
    [f.packageFile, f.packageBytes],
  ]) {
    const response = await fetch(`${repository.localURL}/${relative}`)
    assert.equal(response.status, 200)
    assert.deepEqual(Buffer.from(await response.arrayBuffer()), expected)
  }
  assert.equal(f.calls.length, requests, 'Guest downloads must not depend on snapshot-service availability')
  await repository.close()
  const cached = await prepareSnapshot('arm64', { ...f.options, download: async () => { throw new Error('Upstream is offline') } })
  t.after(() => cached.close())
  assert.equal(await (await fetch(`${cached.localURL}/${f.packageFile}`)).text(), f.packageBytes.toString())
})

test('rejects altered indexes or packages before exposing a repository and never repairs a poisoned cache silently', async (t) => {
  for (const target of ['index', 'package', 'cache']) {
    await t.test(target, async (t) => {
      const f = fixture(t)
      if (target === 'index') f.files.set(f.indexPath, Buffer.from('Altered index'))
      else if (target === 'package') f.files.set(f.packageFile, Buffer.from('Altered package'))
      else {
        const repository = await prepareSnapshot('arm64', f.options)
        await repository.close()
        fs.writeFileSync(path.join(f.options.cacheDirectory, f.digest(f.packageBytes)), 'Altered cache')
      }
      await assert.rejects(prepareSnapshot('arm64', f.options), /checksum|size/i)
    })
  }
})

test('prepares all dependency alternatives and virtual providers without choosing APT package versions', async (t) => {
  const f = fixture(t)
  let contents = [
    'Package: qemu-user-binfmt', 'Version: 1.0', 'Architecture: arm64',
    `Filename: ${f.packageFile}`, `Size: ${f.packageBytes.length}`, `SHA256: ${f.digest(f.packageBytes)}`,
    'Depends: synthetic-lib (>= 1.0), synthetic-runtime | synthetic-alternative', '', '',
  ].join('\n')
  const paths = []
  for (const [name, version, provides] of [
    ['synthetic-lib', '1.0', ''], ['synthetic-lib', '2.0', ''],
    ['synthetic-provider', '1.0', 'synthetic-runtime'], ['synthetic-alternative', '1.0', ''],
  ]) {
    const relative = `pool/main/s/synthetic/${name}_${version}_arm64.deb`
    paths.push(relative)
    const bytes = Buffer.from(`Synthetic ${name} ${version}`)
    f.files.set(relative, bytes)
    contents += [`Package: ${name}`, `Version: ${version}`, 'Architecture: arm64', `Provides: ${provides}`,
      `Filename: ${relative}`, `Size: ${bytes.length}`, `SHA256: ${f.digest(bytes)}`, '', ''].join('\n')
  }
  const index = gzipSync(contents)
  f.files.set(`dists/noble/main/binary-arm64/by-hash/SHA256/${f.digest(index)}`, index)
  f.files.set('dists/noble/InRelease', Buffer.from(f.release.toString()
    .replace(`${f.digest(f.index)} ${f.index.length}`, `${f.digest(index)} ${index.length}`)))
  const repository = await prepareSnapshot('arm64', f.options)
  t.after(() => repository.close())
  for (const relative of paths) {
    assert.ok(f.calls.includes(relative), relative)
    assert.equal((await fetch(`${repository.localURL}/${relative}`)).status, 200, relative)
  }
})

test('the repository cannot proxy arbitrary requests, expose host files, or accept writes', async (t) => {
  const f = fixture(t)
  const repository = await prepareSnapshot('arm64', f.options)
  t.after(() => repository.close())
  for (const relative of ['../../fixture.json', 'pool/unknown.deb', 'dists/noble/main/binary-arm64/Packages.xz',
    'https://unrelated.invalid/secret', 'dists/noble/InRelease?token=synthetic']) {
    const response = await fetch(`${repository.localURL}/${relative}`)
    assert.equal(response.status, 404, relative)
  }
  const response = await fetch(`${repository.localURL}/${f.packageFile}`, { method: 'PUT', body: 'Altered bytes' })
  assert.equal(response.status, 405)
  assert.deepEqual(Buffer.from(await (await fetch(`${repository.localURL}/${f.packageFile}`)).arrayBuffer()), f.packageBytes)
  const failing = fixture(t)
  await assert.rejects(prepareSnapshot('arm64', {
    ...failing.options, download: async () => { throw new Error('Synthetic snapshot HTTP 503') },
  }), /snapshot HTTP 503/)
})
