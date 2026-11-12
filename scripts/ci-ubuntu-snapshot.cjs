const crypto = require('node:crypto')
const fs = require('node:fs')
const http = require('node:http')
const os = require('node:os')
const path = require('node:path')
const { Readable, Transform } = require('node:stream')
const { pipeline } = require('node:stream/promises')
const { setTimeout: delay } = require('node:timers/promises')
const { gunzipSync } = require('node:zlib')
const { verify } = require('./verify-runtime-checksum.cjs')

function snapshotID(date = new Date()) {
  return `${new Date(date.getTime() - 3 * 86400000).toISOString().slice(0, 10).replaceAll('-', '')}T000000Z`
}

async function parallel(items, action) {
  let next = 0
  let failure
  await Promise.all(Array.from({ length: Math.min(4, items.length) }, async () => {
    while (!failure && next < items.length) {
      const item = items[next++]
      try { await action(item) } catch (error) { failure ||= error }
    }
  }))
  if (failure) throw failure
}

async function downloadFile(url, destination, signal) {
  const deadline = AbortSignal.any([signal, AbortSignal.timeout(300000)])
  for (let attempt = 0; ; attempt++) {
    const connection = new AbortController()
    const timer = setTimeout(() => connection.abort(new Error('Snapshot connection timed out')), 30000)
    try {
      const response = await fetch(url, {
        signal: AbortSignal.any([deadline, connection.signal]), redirect: 'error',
      })
      clearTimeout(timer)
      if (!response.ok) {
        await response.body?.cancel()
        throw Object.assign(new Error(`Snapshot HTTP ${response.status}: ${url}`), {
          retryable: response.status === 429 || response.status >= 500,
        })
      }
      let size = 0
      await pipeline(Readable.fromWeb(response.body), new Transform({
        transform(chunk, _encoding, callback) {
          size += chunk.length
          callback(size > 512 * 1024 * 1024
            ? Object.assign(new Error('Snapshot object exceeds its bounded size limit'), { retryable: false }) : null, chunk)
        },
      }), fs.createWriteStream(destination, { flags: 'wx' }), { signal: deadline })
      return
    } catch (error) {
      fs.rmSync(destination, { force: true })
      if (deadline.aborted || error.retryable === false || attempt === 9) throw error
      console.error(`CI snapshot download retry ${attempt + 1}: ${error.message}`)
      await delay(Math.min(2 ** attempt, 30) * 1000, undefined, { signal: deadline })
    } finally {
      clearTimeout(timer)
    }
  }
}

function fields(paragraph) {
  const result = {}
  for (const line of paragraph.replace(/\n[ \t]+/g, ' ').split('\n')) {
    const match = /^([A-Za-z0-9-]+):\s*(.*)$/.exec(line)
    if (match) result[match[1]] = match[2]
  }
  return result
}

function dependencyNames(value = '') {
  return value.split(/[,|]/).map((part) => /^\s*([a-z0-9][a-z0-9+.-]*)/.exec(part)?.[1]).filter(Boolean)
}

async function prepareSnapshot(arch, {
  date = new Date(), suites = ['noble', 'resolute'].flatMap((release) =>
    [release, `${release}-updates`, `${release}-backports`, `${release}-security`]),
  components = ['main', 'universe', 'restricted', 'multiverse'],
  cacheDirectory = process.env.PORTO_CI_APT_CACHE || path.join(os.tmpdir(), 'porto-ci-apt-cache'),
  download, signal = new AbortController().signal,
} = {}) {
  if (!['amd64', 'arm64'].includes(arch)) throw new Error(`Unsupported CI APT architecture: ${arch}`)
  const snapshot = snapshotID(date)
  const origin = `https://snapshot.ubuntu.com/ubuntu/${snapshot}/`
  fs.mkdirSync(cacheDirectory, { recursive: true })
  const routes = new Map()
  const packages = new Map()
  const providers = new Map()
  const objects = new Map()
  const transfers = new Map()
  const store = async (relative, hash, size) => {
    const key = hash || crypto.createHash('sha256').update(origin + relative).digest('hex') + '.InRelease'
    const file = path.join(cacheDirectory, key)
    if (!fs.existsSync(file) && !transfers.has(key)) {
      transfers.set(key, (async () => {
        const temporary = path.join(cacheDirectory, `.download-${crypto.randomUUID()}`)
        try {
          console.error(`Preparing CI snapshot input: ${relative}`)
          if (download) await download(relative, temporary)
          else await downloadFile(origin + relative, temporary, signal)
          if (hash) verify(hash, temporary)
          if (size !== undefined && fs.statSync(temporary).size !== size) throw new Error(`Snapshot size mismatch: ${relative}`)
          fs.renameSync(temporary, file)
        } finally {
          fs.rmSync(temporary, { force: true })
        }
      })())
    }
    await transfers.get(key)
    if (hash) verify(hash, file)
    if (size !== undefined && fs.statSync(file).size !== size) throw new Error(`Snapshot cache size mismatch: ${relative}`)
    routes.set(decodeURIComponent(relative), file)
    return file
  }
  const indexes = []
  await parallel(suites, async (suite) => {
    const file = await store(`dists/${suite}/InRelease`)
    const release = fs.readFileSync(file, 'utf8')
    if (!release.startsWith('-----BEGIN PGP SIGNED MESSAGE-----\n') || !release.includes('\n-----BEGIN PGP SIGNATURE-----')) {
      throw new Error(`Snapshot ${suite} omitted its signed InRelease`)
    }
    const hashes = release.split('\nSHA256:\n')[1]?.split(/\n[^ ]/, 1)[0]
    if (!hashes) throw new Error(`Snapshot ${suite} omitted SHA-256 index identities`)
    for (const component of components) {
      const name = `${component}/binary-${arch}/Packages.gz`
      const entry = hashes.split('\n').map((line) => /^ ([a-f0-9]{64})\s+([0-9]+)\s+(\S+)$/.exec(line))
        .find((match) => match?.[3] === name)
      if (!entry) throw new Error(`Snapshot ${suite} omitted ${name}`)
      indexes.push({ relative: `dists/${suite}/${name}`, hash: entry[1], size: Number(entry[2]) })
    }
  })
  await parallel(indexes, async (index) => {
    const relative = index.relative.replace('Packages.gz', `by-hash/SHA256/${index.hash}`)
    const file = await store(relative, index.hash, index.size)
    routes.set(index.relative, file)
    const contents = gunzipSync(fs.readFileSync(file), { maxOutputLength: 256 * 1024 * 1024 }).toString()
    for (const paragraph of contents.split('\n\n')) {
      const entry = fields(paragraph)
      if (!entry.Package) continue
      if (!/^pool\/(?:main|universe|restricted|multiverse)\/[a-z0-9+.-]+\/[a-z0-9+.-]+\/[A-Za-z0-9+_.:~%-]+\.deb$/.test(entry.Filename) ||
          /%(?:2f|5c|2e)/i.test(entry.Filename) || entry.Filename.split('/').some((part) => part === '.' || part === '..') ||
          !/^[a-f0-9]{64}$/.test(entry.SHA256) || !/^[0-9]+$/.test(entry.Size)) {
        throw new Error(`Snapshot package ${entry.Package} has no safe content identity`)
      }
      const existing = objects.get(entry.Filename)
      if (existing && existing.hash !== entry.SHA256) throw new Error(`Snapshot package identity conflicts: ${entry.Filename}`)
      const object = existing || {
        relative: entry.Filename, hash: entry.SHA256, size: Number(entry.Size),
        dependencies: dependencyNames(`${entry.Depends || ''},${entry['Pre-Depends'] || ''}`),
      }
      objects.set(entry.Filename, object)
      const append = (map, name) => {
        if (!map.has(name)) map.set(name, new Set())
        map.get(name).add(object)
      }
      append(packages, entry.Package)
      for (const name of dependencyNames(entry.Provides)) append(providers, name)
    }
  })
  // Prefetch a superset, not a host-side replacement for APT's dependency solver.
  const pending = ['uidmap', 'fuse3', 'dbus-user-session', 'rsync', 'iptables', 'sshfs',
    'qemu-user-static', 'qemu-user-binfmt', 'docker.io']
  const visited = new Set()
  const selected = new Set()
  for (let next = 0; next < pending.length; next++) {
    const name = pending[next]
    if (visited.has(name)) continue
    visited.add(name)
    const candidates = new Set([...(packages.get(name) || []), ...(providers.get(name) || [])])
    for (const object of candidates) {
      selected.add(object)
      pending.push(...object.dependencies)
    }
  }
  if (!packages.has('qemu-user-static') && !packages.has('qemu-user-binfmt')) {
    throw new Error('Snapshot has no QEMU binfmt package; refusing an incomplete bootstrap')
  }
  await parallel([...selected], (object) => store(object.relative, object.hash, object.size))
  signal.throwIfAborted()
  const prefix = `/ubuntu/${snapshot}/`
  const server = http.createServer((request, response) => {
    if (!['GET', 'HEAD'].includes(request.method)) {
      response.writeHead(405).end()
      return
    }
    let relative
    try { relative = decodeURIComponent(request.url.slice(prefix.length)) } catch {
      response.writeHead(404).end()
      return
    }
    const file = request.url.startsWith(prefix) && routes.get(relative)
    if (!file) {
      response.writeHead(404).end()
      return
    }
    let size
    try { size = fs.statSync(file).size } catch (error) {
      console.error(`CI snapshot cache is unavailable: ${error.message}`)
      response.writeHead(500).end()
      return
    }
    response.writeHead(200, { 'Content-Length': size })
    if (request.method === 'HEAD') response.end()
    else fs.createReadStream(file).on('error', (error) => {
      console.error(`CI snapshot read failed: ${error.message}`)
      response.destroy(error)
    }).pipe(response)
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const port = server.address().port
  let closing
  console.error(`Prepared ${routes.size} immutable CI APT inputs for ${snapshot} (${arch}); guest signatures and hashes remain required.`)
  return {
    snapshot, preparedAt: date.toISOString(),
    source: `http://host.lima.internal:${port}${prefix}`,
    localURL: `http://127.0.0.1:${port}${prefix.slice(0, -1)}`,
    close() {
      return closing ||= new Promise((resolve, reject) => {
        server.close((error) => error ? reject(error) : resolve())
        server.closeAllConnections()
      })
    },
  }
}

if (require.main === module) {
  const controller = new AbortController()
  let repository
  const stop = async () => {
    controller.abort(new Error('CI snapshot preparation was cancelled'))
    if (repository) await repository.close()
  }
  process.once('SIGINT', stop)
  process.once('SIGTERM', stop)
  ;(async () => {
    if (process.env.GITHUB_ACTIONS !== 'true' || process.argv.length !== 4) {
      throw new Error('Usage on disposable CI hosts only: ci-ubuntu-snapshot.cjs <amd64|arm64> <new-ready-file>')
    }
    repository = await prepareSnapshot(process.argv[2], { signal: controller.signal })
    const ready = process.argv[3]
    if (fs.existsSync(ready)) throw new Error('Refusing to overwrite an existing CI repository ownership record')
    fs.writeFileSync(`${ready}.tmp`, JSON.stringify(repository), { flag: 'wx', mode: 0o600 })
    fs.renameSync(`${ready}.tmp`, ready)
  })().catch(async (error) => {
    console.error(`Unable to prepare the immutable CI Ubuntu snapshot: ${error.message}`)
    if (repository) await repository.close()
    process.exitCode = 1
  })
}

module.exports = { prepareSnapshot, snapshotID }
