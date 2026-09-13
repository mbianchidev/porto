const crypto = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')
const { Readable, Transform } = require('node:stream')
const { pipeline } = require('node:stream/promises')

const DEFAULT_REPOSITORY = 'mbianchidev/porto'
const DEFAULT_UPDATE_CHECK_INTERVAL = 12 * 60 * 60 * 1000
const RELEASE_VERSION_FILE = 'porto-release-version'
const SEMVER_PATTERN = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/

function parseSemver(value) {
  if (typeof value !== 'string') {
    throw new Error(`Invalid semantic version: ${String(value)}`)
  }
  const normalized = value.startsWith('v') ? value.slice(1) : value
  const match = SEMVER_PATTERN.exec(normalized)
  if (!match) {
    throw new Error(`Invalid semantic version: ${value}`)
  }
  const prerelease = match[4] ? match[4].split('.') : []
  for (const identifier of prerelease) {
    if (/^[0-9]+$/.test(identifier) && identifier.length > 1 && identifier.startsWith('0')) {
      throw new Error(`Invalid semantic version: ${value}`)
    }
  }
  return {
    version: normalized,
    major: BigInt(match[1]),
    minor: BigInt(match[2]),
    patch: BigInt(match[3]),
    prerelease,
  }
}

function compareIdentifier(left, right) {
  const leftNumeric = /^[0-9]+$/.test(left)
  const rightNumeric = /^[0-9]+$/.test(right)
  if (leftNumeric && rightNumeric) {
    const leftNumber = BigInt(left)
    const rightNumber = BigInt(right)
    return leftNumber === rightNumber ? 0 : leftNumber > rightNumber ? 1 : -1
  }
  if (leftNumeric !== rightNumeric) return leftNumeric ? -1 : 1
  return left === right ? 0 : left > right ? 1 : -1
}

function compareSemver(left, right) {
  const leftVersion = parseSemver(left)
  const rightVersion = parseSemver(right)
  for (const field of ['major', 'minor', 'patch']) {
    if (leftVersion[field] !== rightVersion[field]) {
      return leftVersion[field] > rightVersion[field] ? 1 : -1
    }
  }
  if (leftVersion.prerelease.length === 0 || rightVersion.prerelease.length === 0) {
    if (leftVersion.prerelease.length === rightVersion.prerelease.length) return 0
    return leftVersion.prerelease.length === 0 ? 1 : -1
  }
  const length = Math.max(leftVersion.prerelease.length, rightVersion.prerelease.length)
  for (let index = 0; index < length; index += 1) {
    const leftIdentifier = leftVersion.prerelease[index]
    const rightIdentifier = rightVersion.prerelease[index]
    if (leftIdentifier === undefined || rightIdentifier === undefined) {
      return leftIdentifier === rightIdentifier ? 0 : leftIdentifier === undefined ? -1 : 1
    }
    const comparison = compareIdentifier(leftIdentifier, rightIdentifier)
    if (comparison !== 0) return comparison
  }
  return 0
}

function readPackagedReleaseVersion({
  isPackaged,
  appVersion,
  resourcesPath,
  readFileImpl = fs.readFileSync,
}) {
  if (!isPackaged) return parseSemver(appVersion).version
  const releaseVersion = readFileImpl(path.join(resourcesPath, RELEASE_VERSION_FILE), 'utf8').trim()
  return parseSemver(releaseVersion).version
}

function resolveUpdateTarget(platform, arch) {
  const goarch = arch === 'x64' ? 'amd64' : arch === 'arm64' ? 'arm64' : null
  if (goarch === null) return null
  if (platform === 'darwin') return { goos: 'darwin', goarch, extension: 'dmg' }
  if (platform === 'win32') return { goos: 'windows', goarch, extension: 'exe' }
  if (platform === 'linux') return { goos: 'linux', goarch, extension: 'tar.gz' }
  return null
}

function validateRepository(repository) {
  if (typeof repository !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository)) {
    throw new Error(`Invalid GitHub repository: ${String(repository)}`)
  }
  return repository
}

function releaseAPIURL(repository = DEFAULT_REPOSITORY) {
  return `https://api.github.com/repos/${validateRepository(repository)}/releases/latest`
}

function validatedHTTPSURL(value, label) {
  let parsed
  try {
    parsed = new URL(value)
  } catch {
    throw new Error(`${label} is not a valid URL`)
  }
  if (parsed.protocol !== 'https:') {
    throw new Error(`${label} must use HTTPS`)
  }
  return parsed.toString()
}

function validatedReleaseAssetURL(value, label, repository, tag, assetName) {
  const validated = validatedHTTPSURL(value, label)
  const parsed = new URL(validated)
  let segments
  try {
    segments = parsed.pathname.split('/').filter(Boolean).map(decodeURIComponent)
  } catch {
    throw new Error(`${label} contains an invalid path`)
  }
  const [owner, repo] = validateRepository(repository).split('/')
  const expected = [owner, repo, 'releases', 'download', tag, assetName]
  if (
    parsed.hostname !== 'github.com'
    || segments.length !== expected.length
    || segments.some((segment, index) => segment !== expected[index])
  ) {
    throw new Error(`${label} is not the expected GitHub release asset`)
  }
  return validated
}

function releaseUpdateInfo(release, {
  currentVersion,
  platform,
  arch,
  repository = DEFAULT_REPOSITORY,
} = {}) {
  if (!release || typeof release !== 'object' || Array.isArray(release)) {
    throw new Error('GitHub returned an invalid release')
  }
  if (release.draft === true || release.prerelease === true) return null
  const target = resolveUpdateTarget(platform, arch)
  if (target === null) return null
  const parsedRelease = parseSemver(release.tag_name)
  if (compareSemver(parsedRelease.version, currentVersion) <= 0) return null
  if (!Array.isArray(release.assets)) {
    throw new Error(`Porto ${release.tag_name} does not contain release assets`)
  }
  const assetName = `porto-desktop_${parsedRelease.version}_${target.goos}_${target.goarch}.${target.extension}`
  const asset = release.assets.find((candidate) => candidate?.name === assetName)
  if (!asset) {
    throw new Error(`Porto ${release.tag_name} does not contain ${assetName}`)
  }
  const checksums = release.assets.find((candidate) => candidate?.name === 'SHA256SUMS')
  if (!checksums) {
    throw new Error(`Porto ${release.tag_name} does not contain SHA256SUMS`)
  }
  return {
    version: parsedRelease.version,
    tag: release.tag_name,
    releaseURL: release.html_url ? validatedHTTPSURL(release.html_url, 'Release URL') : '',
    assetName,
    assetURL: validatedReleaseAssetURL(
      asset.browser_download_url,
      `${assetName} download URL`,
      repository,
      release.tag_name,
      assetName,
    ),
    assetSize: Number.isSafeInteger(asset.size) && asset.size >= 0 ? asset.size : 0,
    checksumURL: validatedReleaseAssetURL(
      checksums.browser_download_url,
      'SHA256SUMS download URL',
      repository,
      release.tag_name,
      'SHA256SUMS',
    ),
  }
}

function parseChecksumFile(contents, assetName) {
  for (const line of String(contents).split(/\r?\n/)) {
    const match = /^([a-fA-F0-9]{64})\s+\*?(.+)$/.exec(line.trim())
    if (match && match[2] === assetName) return match[1].toLowerCase()
  }
  throw new Error(`SHA256SUMS does not contain a checksum for ${assetName}`)
}

async function fetchAndConsume(fetchImpl, url, {
  timeoutMilliseconds,
  headers,
  consume,
}) {
  const controller = new AbortController()
  let timeout = null
  const timeoutPromise = new Promise((_, reject) => {
    timeout = setTimeout(() => {
      controller.abort()
      reject(new Error(`Timed out while downloading ${url}`))
    }, timeoutMilliseconds)
  })
  try {
    return await Promise.race([
      (async () => {
        const response = await fetchImpl(url, {
          redirect: 'follow',
          headers,
          signal: controller.signal,
        })
        if (!response || typeof response.ok !== 'boolean') {
          throw new Error(`Invalid response while downloading ${url}`)
        }
        if (!response.ok) {
          throw new Error(`Download failed with HTTP ${response.status} for ${url}`)
        }
        return consume(response, controller.signal)
      })(),
      timeoutPromise,
    ])
  } finally {
    clearTimeout(timeout)
  }
}

async function sha256File(filePath) {
  const hash = crypto.createHash('sha256')
  for await (const chunk of fs.createReadStream(filePath)) hash.update(chunk)
  return hash.digest('hex')
}

async function removeStaleTemporaryDownloads(directory, assetName) {
  let entries
  try {
    entries = await fs.promises.readdir(directory, { withFileTypes: true })
  } catch (error) {
    if (error.code === 'ENOENT') return
    throw error
  }
  await Promise.all(entries
    .filter((entry) => (
      entry.isFile()
      && entry.name.startsWith(`${assetName}.`)
      && entry.name.endsWith('.tmp')
    ))
    .map((entry) => fs.promises.rm(path.join(directory, entry.name), { force: true })))
}

function statusMessage(error) {
  return error instanceof Error ? error.message : String(error)
}

function createDesktopUpdater({
  currentVersion,
  platform,
  arch,
  isPackaged,
  downloadsDirectory,
  repository = DEFAULT_REPOSITORY,
  fetchImpl = globalThis.fetch,
  onStatus = () => {},
  requestTimeouts = {},
} = {}) {
  parseSemver(currentVersion)
  validateRepository(repository)
  if (typeof downloadsDirectory !== 'string' || downloadsDirectory === '') {
    throw new Error('Desktop updater downloads directory is required')
  }
  if (typeof fetchImpl !== 'function') {
    throw new Error('Desktop updater requires fetch support')
  }
  const timeouts = {
    release: requestTimeouts.release ?? 15000,
    checksums: requestTimeouts.checksums ?? 30000,
    package: requestTimeouts.package ?? 30 * 60 * 1000,
  }
  for (const [name, timeout] of Object.entries(timeouts)) {
    if (!Number.isSafeInteger(timeout) || timeout <= 0) {
      throw new Error(`Desktop updater ${name} timeout must be a positive integer`)
    }
  }

  const target = resolveUpdateTarget(platform, arch)
  let availableUpdate = null
  let downloadedPackagePath = ''
  let downloadedChecksum = ''
  let checkPromise = null
  let downloadPromise = null
  let status = {
    phase: !isPackaged || target === null ? 'unsupported' : 'idle',
    currentVersion,
    availableVersion: '',
    progressPercent: null,
    releaseURL: '',
    message: !isPackaged
      ? 'Update checks are available in installed Porto builds.'
      : target === null
        ? `Automatic updates are not available for ${platform}/${arch}.`
        : 'Porto checks GitHub Releases for stable updates.',
  }

  function emitStatus() {
    const snapshot = { ...status }
    try {
      onStatus(snapshot)
    } catch (error) {
      console.error('Unable to publish Porto update status', error)
    }
    return snapshot
  }

  function setStatus(phase, message, overrides = {}) {
    status = {
      ...status,
      ...overrides,
      phase,
      message,
    }
    return emitStatus()
  }

  function getStatus() {
    return { ...status }
  }

  async function downloadedPackageValid() {
    if (downloadedPackagePath === '' || downloadedChecksum === '') return false
    try {
      return await sha256File(downloadedPackagePath) === downloadedChecksum
    } catch (error) {
      if (error.code === 'ENOENT') return false
      throw error
    }
  }

  async function discardDownloadedPackage() {
    const packagePath = downloadedPackagePath
    downloadedPackagePath = ''
    downloadedChecksum = ''
    if (packagePath === '') return
    try {
      await fs.promises.rm(packagePath, { force: true })
    } catch (error) {
      console.error('Unable to remove a stale Porto update', error)
    }
  }

  async function check() {
    if (!isPackaged || target === null) return getStatus()
    if (downloadPromise !== null) return downloadPromise
    if (checkPromise !== null) return checkPromise

    checkPromise = (async () => {
      const previousUpdate = availableUpdate
      const previousPackagePath = downloadedPackagePath
      const previousChecksum = downloadedChecksum
      setStatus('checking', 'Checking GitHub Releases for a newer Porto version.', {
        progressPercent: null,
      })
      try {
        const release = await fetchAndConsume(fetchImpl, releaseAPIURL(repository), {
          timeoutMilliseconds: timeouts.release,
          headers: {
            Accept: 'application/vnd.github+json',
            'User-Agent': `Porto/${currentVersion}`,
            'X-GitHub-Api-Version': '2022-11-28',
          },
          consume: (response) => response.json(),
        })
        const nextUpdate = releaseUpdateInfo(release, {
          currentVersion,
          platform,
          arch,
          repository,
        })
        if (nextUpdate === null) {
          await discardDownloadedPackage()
          availableUpdate = null
          return setStatus('up-to-date', `Porto ${currentVersion} is up to date.`, {
            availableVersion: '',
            progressPercent: null,
            releaseURL: '',
          })
        }

        availableUpdate = nextUpdate
        if (
          previousUpdate?.version === nextUpdate.version
          && previousPackagePath !== ''
          && previousChecksum !== ''
        ) {
          downloadedPackagePath = previousPackagePath
          downloadedChecksum = previousChecksum
          if (await downloadedPackageValid()) {
            return setStatus('downloaded', `Porto ${nextUpdate.version} is downloaded and ready to install.`, {
              availableVersion: nextUpdate.version,
              progressPercent: 100,
              releaseURL: nextUpdate.releaseURL,
            })
          }
        }
        await discardDownloadedPackage()
        return setStatus('available', `Porto ${nextUpdate.version} is ready to download.`, {
          availableVersion: nextUpdate.version,
          progressPercent: null,
          releaseURL: nextUpdate.releaseURL,
        })
      } catch (error) {
        if (
          previousUpdate !== null
          && previousPackagePath !== ''
          && previousChecksum !== ''
        ) {
          availableUpdate = previousUpdate
          downloadedPackagePath = previousPackagePath
          downloadedChecksum = previousChecksum
          try {
            if (await downloadedPackageValid()) {
              return setStatus(
                'downloaded',
                `Porto ${previousUpdate.version} remains ready to install. The latest update check failed: ${statusMessage(error)}`,
                {
                  availableVersion: previousUpdate.version,
                  progressPercent: 100,
                  releaseURL: previousUpdate.releaseURL,
                },
              )
            }
          } catch (validationError) {
            console.error('Unable to validate the downloaded Porto update', validationError)
          }
        }
        availableUpdate = null
        await discardDownloadedPackage()
        setStatus('error', `Unable to check for Porto updates: ${statusMessage(error)}`, {
          availableVersion: '',
          progressPercent: null,
          releaseURL: '',
        })
        throw error
      } finally {
        checkPromise = null
      }
    })()
    return checkPromise
  }

  async function downloadResponse(response, temporaryPath, update, expectedChecksum, signal) {
    if (!response.body) throw new Error(`Download response for ${update.assetName} did not contain a body`)
    const headerSize = Number(response.headers?.get?.('content-length') || 0)
    const expectedSize = update.assetSize > 0 ? update.assetSize : Number.isSafeInteger(headerSize) ? headerSize : 0
    const hash = crypto.createHash('sha256')
    let received = 0
    let lastProgress = -1
    const progress = new Transform({
      transform(chunk, _encoding, callback) {
        received += chunk.length
        hash.update(chunk)
        if (expectedSize > 0) {
          const percentage = Math.min(100, Math.floor((received / expectedSize) * 100))
          if (percentage !== lastProgress) {
            lastProgress = percentage
            setStatus('downloading', `Downloading Porto ${update.version}: ${percentage}%`, {
              progressPercent: percentage,
            })
          }
        }
        callback(null, chunk)
      },
    })
    await pipeline(
      Readable.fromWeb(response.body),
      progress,
      fs.createWriteStream(temporaryPath, { flags: 'wx', mode: 0o600 }),
      { signal },
    )
    if (expectedSize > 0 && received !== expectedSize) {
      throw new Error(`Downloaded ${received} bytes for ${update.assetName}; expected ${expectedSize}`)
    }
    const actualChecksum = hash.digest('hex')
    if (actualChecksum !== expectedChecksum) {
      throw new Error(`Checksum verification failed for ${update.assetName}`)
    }
  }

  async function download() {
    if (!isPackaged || target === null) {
      throw new Error(status.message)
    }
    if (downloadPromise !== null) return downloadPromise

    downloadPromise = (async () => {
      let temporaryPath = ''
      try {
        if (downloadedPackagePath !== '' && downloadedChecksum !== '') {
          if (await downloadedPackageValid()) return getStatus()
          await discardDownloadedPackage()
          if (availableUpdate !== null) {
            setStatus('available', `Porto ${availableUpdate.version} must be downloaded again.`, {
              progressPercent: null,
            })
          }
        }
        if (availableUpdate === null) {
          await check()
          if (availableUpdate === null) throw new Error('No Porto update is available')
        }
        const update = availableUpdate
        setStatus('downloading', `Preparing Porto ${update.version} for download.`, {
          progressPercent: 0,
        })
        const checksumContents = await fetchAndConsume(fetchImpl, update.checksumURL, {
          timeoutMilliseconds: timeouts.checksums,
          headers: { 'User-Agent': `Porto/${currentVersion}` },
          consume: (response) => response.text(),
        })
        const expectedChecksum = parseChecksumFile(checksumContents, update.assetName)
        const versionDirectory = path.join(downloadsDirectory, update.version)
        const packagePath = path.join(versionDirectory, update.assetName)
        await fs.promises.mkdir(versionDirectory, { recursive: true, mode: 0o700 })
        await removeStaleTemporaryDownloads(versionDirectory, update.assetName)

        try {
          const existingChecksum = await sha256File(packagePath)
          if (existingChecksum === expectedChecksum) {
            downloadedPackagePath = packagePath
            downloadedChecksum = expectedChecksum
            return setStatus('downloaded', `Porto ${update.version} is downloaded and ready to install.`, {
              progressPercent: 100,
            })
          }
          await fs.promises.rm(packagePath, { force: true })
        } catch (error) {
          if (error.code !== 'ENOENT') throw error
        }

        temporaryPath = `${packagePath}.${process.pid}.${crypto.randomUUID()}.tmp`
        await fetchAndConsume(fetchImpl, update.assetURL, {
          timeoutMilliseconds: timeouts.package,
          headers: { 'User-Agent': `Porto/${currentVersion}` },
          consume: (response, signal) => downloadResponse(
            response,
            temporaryPath,
            update,
            expectedChecksum,
            signal,
          ),
        })
        await fs.promises.chmod(temporaryPath, 0o600)
        await fs.promises.rename(temporaryPath, packagePath)
        temporaryPath = ''
        downloadedPackagePath = packagePath
        downloadedChecksum = expectedChecksum
        return setStatus('downloaded', `Porto ${update.version} is downloaded and ready to install.`, {
          progressPercent: 100,
        })
      } catch (error) {
        if (temporaryPath !== '') {
          try {
            await fs.promises.rm(temporaryPath, { force: true })
          } catch (cleanupError) {
            console.error('Unable to remove an incomplete Porto update', cleanupError)
          }
        }
        setStatus('error', `Unable to download the Porto update: ${statusMessage(error)}`, {
          progressPercent: null,
        })
        throw error
      } finally {
        downloadPromise = null
      }
    })()
    return downloadPromise
  }

  async function downloadedUpdate() {
    if (availableUpdate === null || downloadedPackagePath === '' || status.phase !== 'downloaded') {
      throw new Error('No downloaded Porto update is ready to install')
    }
    if (!(await downloadedPackageValid())) {
      await discardDownloadedPackage()
      setStatus('available', `Porto ${availableUpdate.version} must be downloaded again.`, {
        progressPercent: null,
      })
      throw new Error('The downloaded Porto update no longer matches its verified checksum')
    }
    return {
      ...availableUpdate,
      packagePath: downloadedPackagePath,
    }
  }

  function markInstalling() {
    if (availableUpdate === null || downloadedPackagePath === '') {
      throw new Error('No downloaded Porto update is ready to install')
    }
    return setStatus('installing', `Restarting Porto to install ${availableUpdate.version}.`, {
      progressPercent: 100,
    })
  }

  return {
    check,
    download,
    downloadedUpdate,
    getStatus,
    markInstalling,
  }
}

module.exports = {
  DEFAULT_UPDATE_CHECK_INTERVAL,
  RELEASE_VERSION_FILE,
  compareSemver,
  createDesktopUpdater,
  parseChecksumFile,
  parseSemver,
  readPackagedReleaseVersion,
  releaseAPIURL,
  releaseUpdateInfo,
  resolveUpdateTarget,
}
