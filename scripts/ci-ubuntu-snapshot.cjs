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

async function parallel(items, action, controller) {
  let next = 0
  let failure
  await Promise.all(Array.from({ length: Math.min(4, items.length) }, async () => {
    while (!failure && next < items.length) {
      const item = items[next++]
      try { await action(item) } catch (error) {
        failure ||= error
        controller?.abort(failure)
      }
    }
  }))
  if (failure) throw failure
}

// snapshot.ubuntu.com is anonymous, so 401 and 403 only mean it is shedding load
// from the many CI jobs that download in parallel; back off instead of failing.
const THROTTLED = new Set([401, 403, 429])

function retryAfter(header, now = Date.now()) {
  if (!header) return undefined
  const seconds = Number(header)
  const milliseconds = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(header) - now
  return Number.isFinite(milliseconds) && milliseconds > 0 ? Math.min(milliseconds, 120000) : undefined
}

// Jitter keeps parallel jobs from retrying the throttled mirror in lockstep.
function backoff(attempt, after, random = Math.random) {
  const base = Math.min(2 ** attempt, 60) * 1000
  return Math.max(after || 0, Math.round(base / 2 + random() * base / 2))
}

// Hosted runners can receive large snapshot objects at well under 100 KB/s, so
// only a stalled transfer is retried early; progress resumes with HTTP ranges.
async function downloadFile(url, destination, signal, { stallTimeout = 60000, objectTimeout = 15 * 60000 } = {}) {
  const deadline = AbortSignal.any([signal, AbortSignal.timeout(objectTimeout)])
  const stopped = () => signal.aborted ? signal.reason
    : new Error(`Snapshot object exceeded its ${objectTimeout / 60000}-minute transfer deadline`)
  for (let attempt = 0; ; attempt++) {
    let received = 0
    let partial = false
    try {
      received = fs.statSync(destination).size
      partial = true
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
    }
    const connection = new AbortController()
    let timer
    const progress = () => {
      clearTimeout(timer)
      timer = setTimeout(() => connection.abort(new Error(`Snapshot transfer stalled for ${stallTimeout / 1000}s`)), stallTimeout)
    }
    const active = AbortSignal.any([deadline, connection.signal])
    progress()
    try {
      const response = await fetch(url, {
        signal: active, redirect: 'error', headers: received ? { Range: `bytes=${received}-` } : {},
      })
      const resumed = received > 0 && response.status === 206
      if (resumed && !new RegExp(`^bytes ${received}-[0-9]+/[0-9]+$`).test(response.headers.get('content-range') || '')) {
        await response.body?.cancel()
        fs.rmSync(destination, { force: true })
        throw new Error(`Snapshot resume returned an unexpected range: ${url}`)
      }
      if (!resumed && response.status !== 200) {
        await response.body?.cancel()
        if (response.status === 416) fs.rmSync(destination, { force: true })
        throw Object.assign(new Error(`Snapshot HTTP ${response.status}: ${url}`), {
          retryable: THROTTLED.has(response.status) || response.status === 416 || response.status >= 500,
          retryAfter: retryAfter(response.headers.get('retry-after')),
        })
      }
      let size = resumed ? received : 0
      await pipeline(Readable.fromWeb(response.body), new Transform({
        transform(chunk, _encoding, callback) {
          progress()
          size += chunk.length
          callback(size > 512 * 1024 * 1024
            ? Object.assign(new Error('Snapshot object exceeds its bounded size limit'), { retryable: false }) : null, chunk)
        },
      }), fs.createWriteStream(destination, { flags: resumed ? 'a' : partial ? 'w' : 'wx' }), { signal: active })
      return
    } catch (error) {
      if (deadline.aborted) throw stopped()
      const reason = connection.signal.aborted ? connection.signal.reason : error
      if (reason.retryable === false || attempt === 9) throw reason
      console.error(`CI snapshot download retry ${attempt + 1}: ${reason.message}`)
      try {
        await delay(backoff(attempt, reason.retryAfter), undefined, { signal: deadline })
      } catch {
        throw stopped()
      }
    } finally {
      clearTimeout(timer)
    }
  }
}

const PRUNE_AFTER = 86400000

function pruneCache(cacheDirectory, now = Date.now()) {
  for (const name of fs.readdirSync(cacheDirectory)) {
    if (!/^[a-f0-9]{64}(?:\.InRelease)?$/.test(name)) continue
    const file = path.join(cacheDirectory, name)
    if (now - fs.statSync(file).mtimeMs > PRUNE_AFTER) fs.rmSync(file, { force: true })
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
  date = new Date(), releases = ['noble', 'resolute'], suites = releases.flatMap((release) =>
    [release, `${release}-updates`, `${release}-backports`, `${release}-security`]),
  components = ['main', 'universe', 'restricted', 'multiverse'],
  cacheDirectory = process.env.PORTO_CI_APT_CACHE || path.join(os.tmpdir(), 'porto-ci-apt-cache'),
  download, signal = new AbortController().signal,
} = {}) {
  if (!['amd64', 'arm64'].includes(arch)) throw new Error(`Unsupported CI APT architecture: ${arch}`)
  if (releases.some((release) => !['noble', 'resolute'].includes(release))) {
    throw new Error('Unsupported CI Ubuntu release')
  }
  const snapshot = snapshotID(date)
  const origin = `https://snapshot.ubuntu.com/ubuntu/${snapshot}/`
  fs.mkdirSync(cacheDirectory, { recursive: true })
  const routes = new Map()
  const packages = new Map()
  const providers = new Map()
  const objects = new Map()
  const transfers = new Map()
  const failed = new AbortController()
  const active = AbortSignal.any([signal, failed.signal])
  const store = async (relative, hash, size) => {
    const key = hash || crypto.createHash('sha256').update(origin + relative).digest('hex') + '.InRelease'
    const file = path.join(cacheDirectory, key)
    if (!fs.existsSync(file) && !transfers.has(key)) {
      transfers.set(key, (async () => {
        const temporary = path.join(cacheDirectory, `.download-${crypto.randomUUID()}`)
        try {
          console.error(`Preparing CI snapshot input: ${relative}`)
          if (download) await download(relative, temporary)
          else await downloadFile(origin + relative, temporary, active)
          if (hash) verify(hash, temporary)
          if (size !== undefined && fs.statSync(temporary).size !== size) throw new Error(`Snapshot size mismatch: ${relative}`)
          fs.renameSync(temporary, file)
        } catch (error) {
          throw new Error(`Unable to prepare snapshot input ${relative}: ${error.message}`, { cause: error })
        } finally {
          fs.rmSync(temporary, { force: true })
        }
      })())
    }
    await transfers.get(key)
    if (hash) verify(hash, file)
    if (size !== undefined && fs.statSync(file).size !== size) throw new Error(`Snapshot cache size mismatch: ${relative}`)
    const used = new Date()
    fs.utimesSync(file, used, used)
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
  }, failed)
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
  }, failed)
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
  await parallel([...selected], (object) => store(object.relative, object.hash, object.size), failed)
  signal.throwIfAborted()
  pruneCache(cacheDirectory)
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
  let stopPoll
  const stop = async () => {
    clearInterval(stopPoll)
    controller.abort(new Error('CI snapshot preparation was cancelled'))
    if (repository) await repository.close()
  }
  process.once('SIGINT', stop)
  process.once('SIGTERM', stop)
  ;(async () => {
    if (process.env.GITHUB_ACTIONS !== 'true' || process.argv.length !== 5) {
      throw new Error('Usage on disposable CI hosts only: ci-ubuntu-snapshot.cjs <amd64|arm64> <new-ready-file> <noble|resolute>')
    }
    repository = await prepareSnapshot(process.argv[2], { releases: [process.argv[4]], signal: controller.signal })
    const ready = process.argv[3]
    if (fs.existsSync(ready)) throw new Error('Refusing to overwrite an existing CI repository ownership record')
    fs.writeFileSync(`${ready}.tmp`, JSON.stringify(repository), { flag: 'wx', mode: 0o600 })
    fs.renameSync(`${ready}.tmp`, ready)
    // Windows TerminateProcess cannot deliver Node's SIGTERM handler.
    stopPoll = setInterval(() => {
      if (fs.existsSync(`${ready}.stop`)) {
        stop().catch((error) => {
          console.error(`Unable to close the owned CI repository: ${error.message}`)
          process.exitCode = 1
        })
      }
    }, 100)
    stopPoll.unref()
  })().catch(async (error) => {
    console.error(`Unable to prepare the immutable CI Ubuntu snapshot: ${error.message}`)
    if (repository) await repository.close()
    process.exitCode = 1
  })
}

module.exports = { downloadFile, prepareSnapshot, snapshotID }
