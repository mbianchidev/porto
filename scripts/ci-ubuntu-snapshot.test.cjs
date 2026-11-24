const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const http = require('node:http')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { gzipSync } = require('node:zlib')

const { downloadFile, prepareSnapshot } = require('./ci-ubuntu-snapshot.cjs')

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

test('reuses unchanged inputs across snapshot days and prunes cache entries unused for a day', async (t) => {
  const f = fixture(t)
  const first = await prepareSnapshot('arm64', f.options)
  await first.close()
  const stale = path.join(f.options.cacheDirectory, 'f'.repeat(64))
  const reused = path.join(f.options.cacheDirectory, f.digest(f.packageBytes))
  const unrelated = path.join(f.options.cacheDirectory, '.download-synthetic')
  const old = new Date(Date.now() - 2 * 86400000)
  fs.writeFileSync(stale, 'Synthetic input from an older snapshot')
  fs.writeFileSync(unrelated, 'Synthetic partial transfer')
  for (const file of [stale, reused, unrelated]) fs.utimesSync(file, old, old)
  const requests = f.calls.length
  const next = await prepareSnapshot('arm64', { ...f.options, date: new Date('2026-11-12T12:00:00Z') })
  t.after(() => next.close())
  assert.deepEqual(f.calls.slice(requests), ['dists/noble/InRelease'], 'Only the new signed release is fetched')
  assert.equal(fs.existsSync(stale), false)
  assert.equal(fs.existsSync(reused), true)
  assert.equal(fs.existsSync(unrelated), true)
})

test('slow snapshot transfers resume stalled bytes instead of failing or restarting the object', async (t) => {
  for (const [name, sent, honorsRange] of [
    ['range resume', 0.5, true], ['range ignored', 0.5, false], ['headers only', 0, true],
  ]) {
    await t.test(name, async (t) => {
      const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-ci-snapshot-download-'))
      const content = crypto.randomBytes(65536)
      const ranges = []
      const server = http.createServer((request, response) => {
        ranges.push(request.headers.range)
        const start = Number(/^bytes=([0-9]+)-$/.exec(request.headers.range || '')?.[1])
        if (ranges.length === 1) {
          response.writeHead(200, { 'Content-Length': content.length })
          if (sent) response.write(content.subarray(0, content.length * sent))
          else response.flushHeaders()
        } else if (honorsRange && start) {
          response.writeHead(206, {
            'Content-Length': content.length - start,
            'Content-Range': `bytes ${start}-${content.length - 1}/${content.length}`,
          })
          response.end(content.subarray(start))
        } else {
          response.writeHead(200, { 'Content-Length': content.length })
          response.end(content)
        }
      })
      await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
      t.after(() => {
        server.closeAllConnections()
        server.close()
        fs.rmSync(directory, { recursive: true, force: true })
      })
      const destination = path.join(directory, 'object')
      await downloadFile(`http://127.0.0.1:${server.address().port}/object`, destination,
        new AbortController().signal, { stallTimeout: 200 })
      assert.deepEqual(fs.readFileSync(destination), content)
      assert.equal(ranges.length, 2)
      assert.equal(ranges[0], undefined)
      if (sent) assert.match(ranges[1], /^bytes=[1-9][0-9]*-$/)
      else assert.equal(ranges[1], undefined)
    })
  }
})

test('throttled snapshot responses back off and retry while missing objects fail at once', async (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-ci-snapshot-throttle-'))
  const content = Buffer.from('Synthetic snapshot object')
  const requests = []
  const server = http.createServer((request, response) => {
    requests.push(request.url)
    if (request.url === '/missing') response.writeHead(404).end()
    else if (requests.filter((url) => url === '/object').length <= 2) {
      response.writeHead(requests.length === 1 ? 401 : 403, { 'Retry-After': '1' }).end()
    } else response.writeHead(200, { 'Content-Length': content.length }).end(content)
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  t.after(() => {
    server.closeAllConnections()
    server.close()
    fs.rmSync(directory, { recursive: true, force: true })
  })
  const origin = `http://127.0.0.1:${server.address().port}`
  const destination = path.join(directory, 'object')
  await downloadFile(`${origin}/object`, destination, new AbortController().signal)
  assert.deepEqual(fs.readFileSync(destination), content)
  assert.equal(requests.length, 3)
  await assert.rejects(downloadFile(`${origin}/missing`, path.join(directory, 'missing'),
    new AbortController().signal), /Snapshot HTTP 404/)
  assert.equal(requests.length, 4)
})
