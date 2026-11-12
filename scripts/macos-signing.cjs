const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const { createRequire } = require('node:module')
const os = require('node:os')
const path = require('node:path')
const { pathToFileURL } = require('node:url')

const { validatePortableSymlinks } = require('./desktop-runtime-symlinks.cjs')
const { parseSemver, RELEASE_VERSION_FILE } = require('../ui/electron/desktop-updater.cjs')

const desktopRequire = createRequire(path.join(__dirname, '..', 'ui', 'electron', 'package.json'))
const entitlements = path.join(__dirname, '..', 'ui', 'electron', 'assets', 'entitlements.mac.plist')

function command(file, args, run = spawnSync, {
  allowUnsigned = false,
  wait = (milliseconds) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds),
} = {}) {
  const deadline = Date.now() + 120000
  let result
  for (let attempt = 0; ; attempt++) {
    result = run(file, args, { encoding: 'utf8', timeout: Math.max(1, deadline - Date.now()), maxBuffer: 4 * 1024 * 1024 })
    const delay = 250 * 2 ** attempt
    const busy = file === 'hdiutil' && args[0] === 'verify' && !result.error && result.status === 1 &&
      /\bResource temporarily unavailable\b/.test(result.stderr || '')
    if (!busy || attempt === 4 || Date.now() + delay >= deadline) break
    console.warn(`DiskImages verification is temporarily busy; retrying the same image in ${delay}ms (${attempt + 1}/4).`)
    wait(delay)
  }
  const unsigned = allowUnsigned && result.status === 1 && /code object is not signed at all/.test(result.stderr || '')
  if (result.error || (result.status !== 0 && !unsigned)) {
    throw new Error(`${file} ${args.join(' ')} failed: ${result.error?.message || result.stderr?.trim() || result.stdout?.trim() || `exit ${result.signal ?? result.status}`}`)
  }
  return result
}

function isNativeCode(file) {
  if (fs.lstatSync(file).isDirectory()) return /\.(app|framework)$/.test(file)
  const descriptor = fs.openSync(file, 'r')
  try {
    const header = Buffer.alloc(4)
    if (fs.readSync(descriptor, header, 0, header.length, 0) !== header.length) return false
    return [0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe,
      0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca].includes(header.readUInt32BE())
  } finally {
    fs.closeSync(descriptor)
  }
}

async function signElectron(options) {
  const { sign } = await import(pathToFileURL(desktopRequire.resolve('@electron/osx-sign')).href)
  await sign(options)
}

async function sealMacOSApplication(application, { identity = '-', keychain, sign = signElectron, run = spawnSync } = {}) {
  const app = path.resolve(application)
  const resources = path.join(app, 'Contents', 'Resources')
  validatePortableSymlinks(app)
  const existing = command('/usr/bin/codesign', ['--display', '--verbose=4', app], run, { allowUnsigned: true })
  if (identity === '-' && /^Authority=/m.test(existing.stderr || '')) {
    command('/usr/bin/codesign', ['--verify', '--deep', '--strict', app], run)
    return 'certificate'
  }
  await sign({
    app,
    platform: 'darwin',
    identity,
    keychain,
    identityValidation: identity !== '-',
    preAutoEntitlements: false,
    preEmbedProvisioningProfile: false,
    strictVerify: true,
    // Runtime binaries are checksum-pinned resources, not signing inputs.
    ignore: (file) => file.startsWith(`${resources}${path.sep}`) || !isNativeCode(file),
    optionsForFile: () => ({
      entitlements,
      hardenedRuntime: true,
      ...(identity === '-' ? { timestamp: 'none' } : {}),
    }),
  })
  command('/usr/bin/codesign', ['--verify', '--deep', '--strict', app], run)
  return identity === '-' ? 'ad-hoc' : 'certificate'
}

function verifyMacOSDmg(image, architecture, version, { run = spawnSync, wait } = {}) {
  const expectedArchitecture = { amd64: 'x86_64', arm64: 'arm64' }[architecture]
  if (!expectedArchitecture) throw new Error(`Unsupported macOS package architecture: ${architecture}`)
  const releaseVersion = parseSemver(version).version
  const desktopVersion = releaseVersion.split(/[-+]/, 1)[0]
  command('hdiutil', ['verify', path.resolve(image)], run, { wait })
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-dmg-verification-'))
  const mount = path.join(temporary, 'mount')
  fs.mkdirSync(mount)
  let failure
  let cleanupFailure
  try {
    command('hdiutil', ['attach', '-readonly', '-nobrowse', '-noautoopen', '-mountpoint', mount, path.resolve(image)], run)
    const app = path.join(mount, 'Porto.app')
    if (!fs.lstatSync(app).isDirectory()) throw new Error('DMG must contain a Porto.app directory')
    command('/usr/bin/codesign', ['--verify', '--deep', '--strict', app], run)
    validatePortableSymlinks(app)
    const info = path.join(app, 'Contents', 'Info.plist')
    for (const [key, expected] of [
      ['CFBundleExecutable', 'Porto'],
      ['CFBundleIdentifier', 'dev.mbianchi.porto'],
      ['CFBundleShortVersionString', desktopVersion],
    ]) {
      const actual = command('/usr/libexec/PlistBuddy', ['-c', `Print :${key}`, info], run).stdout.trim()
      if (actual !== expected) throw new Error(`DMG ${key} is ${actual}; expected ${expected} (release version ${releaseVersion})`)
    }
    const resources = path.join(app, 'Contents', 'Resources')
    const packagedVersion = fs.readFileSync(path.join(resources, RELEASE_VERSION_FILE), 'utf8').trim()
    if (packagedVersion !== releaseVersion) {
      throw new Error(`DMG release version is ${packagedVersion}; expected ${releaseVersion}`)
    }
    for (const executable of [path.join(app, 'Contents', 'MacOS', 'Porto'), path.join(resources, 'porto')]) {
      fs.accessSync(executable, fs.constants.X_OK)
      const actual = command('/usr/bin/lipo', ['-archs', executable], run).stdout.trim()
      if (actual !== expectedArchitecture) {
        throw new Error(`DMG executable architecture is ${actual}; expected ${expectedArchitecture}: ${executable}`)
      }
    }
    fs.accessSync(path.join(resources, 'runtime', 'bin', 'porto-runtime-helper'), fs.constants.X_OK)
    if (!fs.readFileSync(path.join(resources, 'runtime', 'VERSIONS'), 'utf8').trim()) {
      throw new Error('DMG runtime VERSIONS manifest is empty')
    }
  } catch (error) {
    failure = error
  } finally {
    try {
      if (fs.existsSync(mount) && fs.statSync(mount).dev !== fs.statSync(temporary).dev) {
        command('hdiutil', ['detach', mount], run)
      }
      fs.rmSync(temporary, { recursive: true, force: true })
    } catch (error) {
      cleanupFailure = error
    }
  }
  if (failure && cleanupFailure) {
    throw new AggregateError([failure, cleanupFailure], `${failure.message}; DMG cleanup failed: ${cleanupFailure.message}`)
  }
  if (cleanupFailure) throw cleanupFailure
  if (failure) throw failure
}

async function main(args) {
  const [mode, input, architecture, version, ...extra] = args
  if (mode === '--sign' && input && !architecture) {
    if (process.env.CSC_LINK && (!process.env.CSC_NAME || process.env.CSC_NAME === '-')) {
      throw new Error('Prepackaged signing requires an imported identity in CSC_NAME and optionally CSC_KEYCHAIN; refusing an ad-hoc downgrade of CSC_LINK credentials.')
    }
    const signature = await sealMacOSApplication(input, {
      identity: process.env.CSC_NAME || '-', keychain: process.env.CSC_KEYCHAIN,
    })
    console.log(`Verified ${signature} macOS application signature: ${input}${signature === 'ad-hoc' ? ' (not Developer ID signed or notarized)' : ''}`)
  } else if (mode === '--verify-dmg' && input && architecture && version && extra.length === 0) {
    verifyMacOSDmg(input, architecture, version)
    console.log(`Verified sealed macOS ${architecture} DMG for ${version}: ${input}`)
  } else {
    throw new Error('usage: macos-signing.cjs --sign <Porto.app> | --verify-dmg <image.dmg> <amd64|arm64> <version>')
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`Unable to seal or verify macOS package: ${error.message}`)
    process.exitCode = 1
  })
}

module.exports = { sealMacOSApplication, verifyMacOSDmg }
