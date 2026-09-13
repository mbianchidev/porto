const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  DEFAULT_UPDATE_CHECK_INTERVAL,
  RELEASE_VERSION_FILE,
  compareSemver,
  createDesktopUpdater,
  parseChecksumFile,
  readPackagedReleaseVersion,
  releaseUpdateInfo,
  resolveUpdateTarget,
} = require('./desktop-updater.cjs')

function releaseFixture(version, platform = 'darwin', arch = 'arm64', contents = Buffer.from('porto update')) {
  const target = resolveUpdateTarget(platform, arch)
  const tag = `v${version}`
  const assetName = `porto-desktop_${version}_${target.goos}_${target.goarch}.${target.extension}`
  const checksum = crypto.createHash('sha256').update(contents).digest('hex')
  const baseURL = `https://github.com/mbianchidev/porto/releases/download/${tag}`
  return {
    contents,
    checksum,
    assetName,
    checksumURL: `${baseURL}/SHA256SUMS`,
    assetURL: `${baseURL}/${assetName}`,
    release: {
      tag_name: tag,
      draft: false,
      prerelease: false,
      html_url: `https://github.com/mbianchidev/porto/releases/tag/${tag}`,
      assets: [
        {
          name: 'SHA256SUMS',
          browser_download_url: `${baseURL}/SHA256SUMS`,
          size: 128,
        },
        {
          name: assetName,
          browser_download_url: `${baseURL}/${assetName}`,
          size: contents.length,
        },
      ],
    },
  }
}

test('compares stable and pre-release semantic versions', () => {
  assert.equal(compareSemver('1.2.3', '1.2.3'), 0)
  assert.equal(compareSemver('1.2.4', '1.2.3'), 1)
  assert.equal(compareSemver('2.0.0', '10.0.0'), -1)
  assert.equal(compareSemver('1.2.3', '1.2.3-rc.1'), 1)
  assert.equal(compareSemver('1.2.3-rc.2', '1.2.3-rc.10'), -1)
  assert.equal(compareSemver('1.2.3+build.2', '1.2.3+build.1'), 0)
  assert.throws(() => compareSemver('1.2', '1.2.3'), /semantic version/i)
})

test('checks for updates every 12 hours by default', () => {
  assert.equal(DEFAULT_UPDATE_CHECK_INTERVAL, 12 * 60 * 60 * 1000)
})

test('reads the full packaged release version instead of stripped OS metadata', () => {
  assert.equal(readPackagedReleaseVersion({
    isPackaged: false,
    appVersion: '1.3.0-rc.1',
    resourcesPath: '/unused',
  }), '1.3.0-rc.1')
  assert.equal(readPackagedReleaseVersion({
    isPackaged: true,
    appVersion: '1.3.0',
    resourcesPath: '/resources',
    readFileImpl: (filePath) => {
      assert.equal(filePath, path.join('/resources', RELEASE_VERSION_FILE))
      return '1.3.0-rc.1\n'
    },
  }), '1.3.0-rc.1')
})

test('maps Electron platforms and architectures to release assets', () => {
  assert.deepEqual(resolveUpdateTarget('darwin', 'arm64'), {
    goos: 'darwin',
    goarch: 'arm64',
    extension: 'dmg',
  })
  assert.deepEqual(resolveUpdateTarget('win32', 'x64'), {
    goos: 'windows',
    goarch: 'amd64',
    extension: 'exe',
  })
  assert.deepEqual(resolveUpdateTarget('linux', 'x64'), {
    goos: 'linux',
    goarch: 'amd64',
    extension: 'tar.gz',
  })
  assert.equal(resolveUpdateTarget('freebsd', 'x64'), null)
  assert.equal(resolveUpdateTarget('linux', 'ia32'), null)
})

test('selects a newer stable release and its checksum-protected installer', () => {
  const fixture = releaseFixture('1.3.0')
  const update = releaseUpdateInfo(fixture.release, {
    currentVersion: '1.2.3',
    platform: 'darwin',
    arch: 'arm64',
  })

  assert.equal(update.version, '1.3.0')
  assert.equal(update.assetName, fixture.assetName)
  assert.equal(update.assetURL, fixture.assetURL)
  assert.equal(update.checksumURL, fixture.checksumURL)
  assert.equal(releaseUpdateInfo(fixture.release, {
    currentVersion: '1.3.0',
    platform: 'darwin',
    arch: 'arm64',
  }), null)

  fixture.release.prerelease = true
  assert.equal(releaseUpdateInfo(fixture.release, {
    currentVersion: '1.2.3',
    platform: 'darwin',
    arch: 'arm64',
  }), null)
})

test('rejects releases that omit the platform installer or checksum file', () => {
  const fixture = releaseFixture('1.3.0')
  assert.throws(
    () => releaseUpdateInfo({ ...fixture.release, assets: fixture.release.assets.slice(0, 1) }, {
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
    }),
    /does not contain.*darwin_arm64\.dmg/i,
  )
  assert.throws(
    () => releaseUpdateInfo({ ...fixture.release, assets: fixture.release.assets.slice(1) }, {
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
    }),
    /SHA256SUMS/,
  )
  const externalAsset = {
    ...fixture.release,
    assets: fixture.release.assets.map((asset) => (
      asset.name === fixture.assetName
        ? { ...asset, browser_download_url: `https://example.com/${fixture.assetName}` }
        : asset
    )),
  }
  assert.throws(
    () => releaseUpdateInfo(externalAsset, {
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
    }),
    /expected GitHub release asset/i,
  )
})

test('parses GNU checksum files without accepting partial names', () => {
  const fixture = releaseFixture('1.3.0')
  const checksums = `${'0'.repeat(64)}  other-file.dmg\n${fixture.checksum} *${fixture.assetName}\n`
  assert.equal(parseChecksumFile(checksums, fixture.assetName), fixture.checksum)
  assert.throws(() => parseChecksumFile(checksums, `${fixture.assetName}.extra`), /checksum/i)
})

test('checks GitHub Releases and downloads a verified update', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-updater-'))
  const fixture = releaseFixture('1.3.0')
  const statuses = []
  const fetchImpl = async (url) => {
    if (url === 'https://api.github.com/repos/mbianchidev/porto/releases/latest') {
      return new Response(JSON.stringify(fixture.release), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    }
    if (url === fixture.checksumURL) {
      return new Response(`${fixture.checksum}  ${fixture.assetName}\n`, { status: 200 })
    }
    if (url === fixture.assetURL) {
      return new Response(fixture.contents, {
        status: 200,
        headers: { 'content-length': String(fixture.contents.length) },
      })
    }
    throw new Error(`Unexpected URL: ${url}`)
  }

  try {
    const updater = createDesktopUpdater({
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
      isPackaged: true,
      downloadsDirectory: directory,
      fetchImpl,
      onStatus: (status) => statuses.push(status),
    })

    assert.equal((await updater.check()).phase, 'available')
    const versionDirectory = path.join(directory, '1.3.0')
    fs.mkdirSync(versionDirectory, { recursive: true })
    const staleTemporaryPath = path.join(versionDirectory, `${fixture.assetName}.stale.tmp`)
    fs.writeFileSync(staleTemporaryPath, 'partial')
    const downloaded = await updater.download()
    assert.equal(downloaded.phase, 'downloaded')
    assert.equal(downloaded.availableVersion, '1.3.0')
    assert.deepEqual(fs.readFileSync((await updater.downloadedUpdate()).packagePath), fixture.contents)
    assert.equal(fs.existsSync(staleTemporaryPath), false)
    assert.ok(statuses.some((status) => status.phase === 'checking'))
    assert.ok(statuses.some((status) => status.phase === 'downloading'))
    assert.equal(statuses.at(-1).phase, 'downloaded')
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('removes an untrusted download when checksum verification fails', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-updater-'))
  const fixture = releaseFixture('1.3.0')
  const fetchImpl = async (url) => {
    if (url.endsWith('/releases/latest')) {
      return new Response(JSON.stringify(fixture.release), { status: 200 })
    }
    if (url === fixture.checksumURL) {
      return new Response(`${'0'.repeat(64)}  ${fixture.assetName}\n`, { status: 200 })
    }
    return new Response(fixture.contents, { status: 200 })
  }

  try {
    const updater = createDesktopUpdater({
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
      isPackaged: true,
      downloadsDirectory: directory,
      fetchImpl,
    })
    await updater.check()
    await assert.rejects(updater.download(), /checksum verification failed/i)
    assert.equal(updater.getStatus().phase, 'error')
    assert.equal(fs.existsSync(path.join(directory, '1.3.0', fixture.assetName)), false)
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('keeps polling after download and invalidates missing cached packages', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-updater-'))
  let fixture = releaseFixture('1.3.0')
  const fetchImpl = async (url) => {
    if (url.endsWith('/releases/latest')) {
      return new Response(JSON.stringify(fixture.release), { status: 200 })
    }
    if (url === fixture.checksumURL) {
      return new Response(`${fixture.checksum}  ${fixture.assetName}\n`, { status: 200 })
    }
    if (url === fixture.assetURL) {
      return new Response(fixture.contents, { status: 200 })
    }
    throw new Error(`Unexpected URL: ${url}`)
  }

  try {
    const updater = createDesktopUpdater({
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
      isPackaged: true,
      downloadsDirectory: directory,
      fetchImpl,
    })
    await updater.check()
    await updater.download()
    const oldPackagePath = (await updater.downloadedUpdate()).packagePath

    fixture = releaseFixture('1.4.0', 'darwin', 'arm64', Buffer.from('newer Porto update'))
    const newer = await updater.check()
    assert.equal(newer.phase, 'available')
    assert.equal(newer.availableVersion, '1.4.0')
    assert.equal(fs.existsSync(oldPackagePath), false)

    await updater.download()
    const missingPackagePath = (await updater.downloadedUpdate()).packagePath
    fs.rmSync(missingPackagePath)
    const missing = await updater.check()
    assert.equal(missing.phase, 'available')
    assert.equal(missing.availableVersion, '1.4.0')
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('reverifies the downloaded package immediately before installation', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-updater-'))
  const fixture = releaseFixture('1.3.0')
  const fetchImpl = async (url) => {
    if (url.endsWith('/releases/latest')) {
      return new Response(JSON.stringify(fixture.release), { status: 200 })
    }
    if (url === fixture.checksumURL) {
      return new Response(`${fixture.checksum}  ${fixture.assetName}\n`, { status: 200 })
    }
    return new Response(fixture.contents, { status: 200 })
  }

  try {
    const updater = createDesktopUpdater({
      currentVersion: '1.2.3',
      platform: 'darwin',
      arch: 'arm64',
      isPackaged: true,
      downloadsDirectory: directory,
      fetchImpl,
    })
    await updater.check()
    await updater.download()
    const packagePath = (await updater.downloadedUpdate()).packagePath
    fs.writeFileSync(packagePath, 'tampered')
    await assert.rejects(updater.downloadedUpdate(), /no longer matches/i)
    assert.equal(updater.getStatus().phase, 'available')
    assert.equal(fs.existsSync(packagePath), false)
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('times out when a release response stalls after headers', async () => {
  const updater = createDesktopUpdater({
    currentVersion: '1.2.3',
    platform: 'darwin',
    arch: 'arm64',
    isPackaged: true,
    downloadsDirectory: '/tmp/unused',
    requestTimeouts: { release: 20 },
    fetchImpl: async (_url, { signal }) => new Response(new ReadableStream({
      start(controller) {
        signal.addEventListener('abort', () => controller.error(new Error('aborted')), { once: true })
      },
    }), { status: 200 }),
  })

  await assert.rejects(updater.check(), /timed out/i)
  assert.equal(updater.getStatus().phase, 'error')
})

test('disables update checks for unpackaged or unsupported applications', async () => {
  const unpackaged = createDesktopUpdater({
    currentVersion: '1.2.3',
    platform: 'darwin',
    arch: 'arm64',
    isPackaged: false,
    downloadsDirectory: '/tmp/unused',
  })
  assert.equal((await unpackaged.check()).phase, 'unsupported')

  const unsupported = createDesktopUpdater({
    currentVersion: '1.2.3',
    platform: 'freebsd',
    arch: 'x64',
    isPackaged: true,
    downloadsDirectory: '/tmp/unused',
  })
  assert.equal((await unsupported.check()).phase, 'unsupported')
})
