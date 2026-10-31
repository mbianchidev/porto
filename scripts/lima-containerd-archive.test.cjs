const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { gzipSync } = require('node:zlib')
const crypto = require('node:crypto')

const { selectArchive, decompressArchive, dependencyProvisionOverride, intelTCGImages, tcgGuestCPUs, prepareContainerdArchive } = require('./lima-containerd-archive.cjs')
const { sha256 } = require('./verify-runtime-checksum.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto containerd archive '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return { source: path.join(directory, 'runtime.tar.gz'), destination: path.join(directory, 'runtime.tar') }
}

test('CI dependencies use one immutable Ubuntu snapshot from the previous completed UTC day', () => {
  const date = new Date('2026-10-22T14:35:00Z')
  const override = dependencyProvisionOverride(date)
  const entries = JSON.parse(override.slice('.provision += '.length))
  assert.equal(entries.length, 1)
  assert.equal(entries[0].mode, 'dependency')
  assert.match(entries[0].script, /URIs: https:\/\/snapshot\.ubuntu\.com\/ubuntu\/20261021T000000Z\//)
  assert.doesNotMatch(entries[0].script, /APT::Snapshot/)
  assert.match(entries[0].script, /APT::Update::Error-Mode "any";/)
  assert.match(entries[0].script, /Ubuntu snapshot: 20261021T000000Z/)
  assert.equal(override, dependencyProvisionOverride(date))
})

test('CI snapshot selection uses UTC boundaries and rejects an invalid date', () => {
  const override = dependencyProvisionOverride(new Date('2026-10-22T23:30:00-07:00'))
  assert.match(override, /20261022T000000Z/)
  assert.match(dependencyProvisionOverride(new Date('2026-01-01T00:00:00Z')), /20251231T000000Z/)
  assert.match(dependencyProvisionOverride(new Date('2024-03-01T23:59:59Z')), /20240229T000000Z/)
  assert.throws(() => dependencyProvisionOverride(new Date('invalid')), RangeError)
})

test('CI APT downloads retry transient failures with bounded backoff and transport timeouts', () => {
  const override = dependencyProvisionOverride(new Date('2026-10-22T14:35:00Z'))
  const [{ script }] = JSON.parse(override.slice('.provision += '.length))
  assert.match(script, /Acquire::Retries "10";/)
  assert.match(script, /Acquire::Retries::Delay "true";/)
  assert.match(script, /Acquire::Retries::Delay::Maximum "30";/)
  assert.match(script, /Acquire::http::Timeout "30";/)
  assert.match(script, /Acquire::https::Timeout "30";/)
  assert.match(script, /Debug::Acquire::Retries "true";/)
  assert.match(script, /APT::Update::Error-Mode "any";/)
  assert.doesNotMatch(script, /AllowUnauthenticated|AllowInsecureRepositories|Verify-Peer|Check-Valid-Until/)
})

test('Intel TCG uses matching pinned guest, kernel and initrd with only the upstream timer-check workaround', () => {
  const images = intelTCGImages()
  assert.equal(images.length, 1)
  const [image] = images
  const disk = new URL(image.location)
  assert.equal(disk.origin, 'https://cloud-images.ubuntu.com')
  assert.match(disk.pathname, /\/release-\d{8}\/ubuntu-26\.04-server-cloudimg-amd64\.img$/)
  for (const artifact of [image, image.kernel, image.initrd]) {
    assert.equal(artifact.arch, 'x86_64')
    assert.match(artifact.digest, /^sha256:[a-f0-9]{64}$/)
  }
  const base = image.location.slice(0, -4)
  const directory = base.slice(0, base.lastIndexOf('/') + 1)
  const name = base.slice(base.lastIndexOf('/') + 1)
  assert.equal(image.kernel.location, `${directory}unpacked/${name}-vmlinuz-generic`)
  assert.equal(image.initrd.location, `${directory}unpacked/${name}-initrd-generic`)
  assert.equal(image.kernel.cmdline, 'root=LABEL=cloudimg-rootfs ro console=tty1 console=ttyS0 no_timer_check')
  image.kernel.cmdline = 'synthetic mutation'
  assert.notEqual(intelTCGImages()[0].kernel.cmdline, image.kernel.cmdline)
})

test('installed ARM TCG uses measured host parallelism without changing the Intel or minimum CPU allocation', () => {
  for (const [available, expected] of [[2, 2], [3, 2], [4, 3], [8, 4], [64, 4]]) {
    assert.equal(tcgGuestCPUs('arm64', available), expected)
    assert.equal(tcgGuestCPUs('amd64', available), 2)
    assert.ok(expected <= available)
  }
  for (const available of [0, 1, NaN, 2.5]) {
    assert.throws(() => tcgGuestCPUs('arm64', available), /at least two available host CPUs/)
  }
  assert.throws(() => tcgGuestCPUs('unknown', 4), /Unsupported TCG guest architecture/)
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

test('Windows archive preparation selects verified Git Bash, never the PATH WSL shim or a user profile', async (t) => {
  const { source } = fixture(t)
  const content = Buffer.from('Synthetic native CI guest tools\n')
  const compressed = gzipSync(content)
  const expected = crypto.createHash('sha256').update(compressed).digest('hex')
  const shell = 'C:\\Program Files\\Git\\bin\\bash.exe'
  const directory = path.join(path.dirname(source), 'prepared')
  let downloaded = false
  const expression = await prepareContainerdArchive('arm64', directory, {
    platform: 'win32',
    resolveShell: async () => shell,
    runImpl: (command, args, capture, env) => {
      if (command === 'limactl') {
        assert.deepEqual(args, ['info'])
        assert.equal(capture, true)
        return JSON.stringify({ defaultTemplate: { containerd: { archives: [{
          arch: 'aarch64', location: 'https://synthetic.invalid/runtime.gz', digest: `sha256:${expected}`,
        }] } } })
      }
      assert.equal(command, shell)
      assert.deepEqual(args.slice(0, 2), ['--noprofile', '--norc'])
      assert.equal(path.basename(args[2]), 'download-file.sh')
      assert.equal(args[5], expected)
      assert.equal(env.PATH.split(';')[0], 'C:\\Program Files\\Git\\usr\\bin')
      fs.writeFileSync(args[4], compressed)
      downloaded = true
    },
  })
  assert.ok(downloaded)
  const [archive] = JSON.parse(expression.slice('.containerd.archives = '.length))
  assert.equal(archive.arch, 'aarch64')
  assert.deepEqual(fs.readFileSync(archive.location), content)
  assert.equal(archive.digest, `sha256:${sha256(archive.location)}`)
})
