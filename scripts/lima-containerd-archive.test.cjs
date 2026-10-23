const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { gzipSync } = require('node:zlib')

const { selectArchive, decompressArchive, dependencyProvisionOverride } = require('./lima-containerd-archive.cjs')
const { sha256 } = require('./verify-runtime-checksum.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto containerd archive '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return { source: path.join(directory, 'runtime.tar.gz'), destination: path.join(directory, 'runtime.tar') }
}

test('CI dependencies use one immutable Ubuntu snapshot for the current UTC date', () => {
  const date = new Date('2026-10-22T14:35:00Z')
  const override = dependencyProvisionOverride(date)
  const entries = JSON.parse(override.slice('.provision += '.length))
  assert.equal(entries.length, 1)
  assert.equal(entries[0].mode, 'dependency')
  assert.match(entries[0].script, /URIs: https:\/\/snapshot\.ubuntu\.com\/ubuntu\/20261022T000000Z\//)
  assert.doesNotMatch(entries[0].script, /APT::Snapshot/)
  assert.match(entries[0].script, /APT::Update::Error-Mode "any";/)
  assert.match(entries[0].script, /Ubuntu snapshot: 20261022T000000Z/)
  assert.equal(override, dependencyProvisionOverride(date))
})

test('CI snapshot selection uses UTC boundaries and rejects an invalid date', () => {
  const override = dependencyProvisionOverride(new Date('2026-10-22T23:30:00-07:00'))
  assert.match(override, /20261023T000000Z/)
  assert.throws(() => dependencyProvisionOverride(new Date('invalid')), RangeError)
})

test('CI Lima boot avoids the systemd v255 logind deadlock without weakening rootless coverage', () => {
  const script = fs.readFileSync(path.join(__dirname, 'test-docker-clients.sh'), 'utf8')
  assert.match(script, /limactl "\$\{lima_options\[@\]\}" template:ubuntu-26\.04/)
  assert.doesNotMatch(script, /template:ubuntu-24\.04/)
  assert.match(script, /--containerd=user/)
  assert.match(script, /--timeout=20m/)
  assert.match(script, /--mount-none/)
})

test('selects the pinned Lima default without changing the runtime version or guest architecture', () => {
  const archives = ['x86_64', 'aarch64'].map((arch) => ({
    arch, location: `https://fixture.invalid/synthetic-${arch}.tar.gz`, digest: `sha256:${'1'.repeat(64)}`,
  }))
  const info = { defaultTemplate: { containerd: { archives } } }
  assert.deepEqual(selectArchive(info, 'amd64'), archives[0])
  assert.deepEqual(selectArchive(info, 'arm64'), archives[1])
  assert.throws(() => selectArchive(info, 'unknown'), /Unsupported CI guest architecture/)
})

test('rejects missing and unpinned Lima default archives', () => {
  assert.throws(() => selectArchive({}, 'arm64'), /did not provide default containerd archives/)
  for (const archives of [[], [{ arch: 'aarch64', location: 'https://fixture.invalid/runtime.gz' }]]) {
    assert.throws(() => selectArchive({ defaultTemplate: { containerd: { archives } } }, 'arm64'), /checksum-pinned/)
  }
})

test('decompresses verified bytes and pins the resulting guest archive', async (t) => {
  const { source, destination } = fixture(t)
  const content = Buffer.from('Synthetic runtime archive bytes\n')
  fs.writeFileSync(source, gzipSync(content))
  const digest = await decompressArchive(source, destination, sha256(source))
  assert.deepEqual(fs.readFileSync(destination), content)
  assert.equal(digest, `sha256:${sha256(destination)}`)
})

test('refuses a checksum mismatch before creating guest archive bytes', async (t) => {
  const { source, destination } = fixture(t)
  fs.writeFileSync(source, gzipSync('Synthetic untrusted runtime'))
  await assert.rejects(decompressArchive(source, destination, '0'.repeat(64)), /checksum mismatch/)
  assert.equal(fs.existsSync(destination), false)
})

test('removes an owned partial archive after decompression fails', async (t) => {
  const { source, destination } = fixture(t)
  fs.writeFileSync(source, gzipSync('Synthetic truncated runtime').subarray(0, 12))
  await assert.rejects(decompressArchive(source, destination, sha256(source)), /unexpected end/)
  assert.equal(fs.existsSync(destination), false)
})

test('does not overwrite or delete an existing destination', async (t) => {
  const { source, destination } = fixture(t)
  fs.writeFileSync(source, gzipSync('Synthetic runtime'))
  fs.writeFileSync(destination, 'Synthetic existing destination')
  await assert.rejects(decompressArchive(source, destination, sha256(source)), { code: 'EEXIST' })
  assert.equal(fs.readFileSync(destination, 'utf8'), 'Synthetic existing destination')
})
