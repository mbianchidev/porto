const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { gzipSync } = require('node:zlib')

const { selectArchive, decompressArchive } = require('./lima-containerd-archive.cjs')
const { sha256 } = require('./verify-runtime-checksum.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto containerd archive '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return { source: path.join(directory, 'runtime.tar.gz'), destination: path.join(directory, 'runtime.tar') }
}

test('CI Lima boot avoids the systemd v255 logind deadlock without weakening rootless coverage', () => {
  const script = fs.readFileSync(path.join(__dirname, 'test-docker-clients.sh'), 'utf8')
  assert.match(script, /limactl "\$\{lima_options\[@\]\}" template:ubuntu-26\.04/)
  assert.doesNotMatch(script, /template:ubuntu-24\.04/)
  assert.match(script, /--containerd=user/)
  assert.match(script, /--timeout=20m/)
  assert.match(script, /--mount-none/)
})

test('Intel data fixtures use supported instruction-clock options without disabling APIC timer checks', () => {
  const script = fs.readFileSync(path.join(__dirname, 'test-docker-clients.sh'), 'utf8')
  assert.match(script, /if \[ "\$suite\/\$goarch" = data\/amd64 \]; then\n\s+export QEMU_SYSTEM_X86_64=/)
  assert.match(script, /-accel tcg,thread=single,tb-size=128/)
  assert.match(script, /-icount shift=3,sleep=off/)
  assert.match(script, /-global ICH9-LPC\.disable_s3=1/)
  assert.match(script, /-global ICH9-LPC\.disable_s4=1/)
  assert.doesNotMatch(script, /-icount shift=auto,sleep=off/)
  assert.doesNotMatch(script, /\b(?:noapic|no_timer_check)\b/)
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
