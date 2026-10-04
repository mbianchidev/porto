const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')

const { installMacOSBootstrap, validateMacOSBootstrap } = require('./macos-desktop-bootstrap.cjs')

const MARKER = 'porto-development.json'

function run(command, args) {
  const result = spawnSync(command, args, { encoding: 'utf8', timeout: 60000 })
  if (result.error) throw new Error(`Unable to prepare Porto Dev with ${command}: ${result.error.message}`)
  if (result.status !== 0) throw new Error(`Unable to prepare Porto Dev with ${command}: ${result.stderr || result.signal || result.status}`)
  return result.stdout.trim()
}

function fingerprint(values) {
  const hash = crypto.createHash('sha256')
  for (const value of values) hash.update(value).update('\0')
  return hash.digest('hex')
}

function validateDevelopmentApp(bundle, key) {
  const resources = path.join(bundle, 'Contents', 'Resources')
  const metadata = JSON.parse(fs.readFileSync(path.join(resources, MARKER), 'utf8'))
  if (metadata.key !== key) throw new Error('Porto Dev cache identity does not match this source/runtime')
  if (fs.existsSync(path.join(resources, 'default_app.asar'))) {
    throw new Error('Porto Dev cache contains the generic Electron application')
  }
  fs.accessSync(path.join(resources, 'app', 'package.json'), fs.constants.R_OK)
  fs.accessSync(path.join(resources, 'app', 'main.cjs'), fs.constants.R_OK)
  validateMacOSBootstrap(bundle, { executableName: 'Electron' })
}

async function prepareMacOSDevelopmentApp({
  appDirectory,
  electronExecutable,
  cacheRoot = path.join(os.homedir(), 'Library', 'Caches', 'Porto', 'development'),
}) {
  if (process.platform !== 'darwin') throw new Error('Prepare the guarded Porto Dev application on macOS')
  const source = fs.realpathSync(appDirectory)
  const executable = fs.realpathSync(electronExecutable)
  const runtimeBundle = path.dirname(path.dirname(path.dirname(executable)))
  if (path.basename(executable) !== 'Electron' || !runtimeBundle.endsWith('.app')) {
    throw new Error('Porto Dev requires the installed macOS Electron runtime')
  }
  const manifest = JSON.parse(fs.readFileSync(path.join(source, 'package.json'), 'utf8'))
  if (!manifest.main || !manifest.version || !manifest.name) throw new Error('The Porto development application manifest is incomplete')
  const runtimePlist = path.join(runtimeBundle, 'Contents', 'Info.plist')
  const version = run('/usr/libexec/PlistBuddy', ['-c', 'Print :CFBundleShortVersionString', runtimePlist])
  const framework = path.join(runtimeBundle, 'Contents', 'Frameworks', 'Electron Framework.framework', 'Electron Framework')
  const frameworkStat = fs.statSync(framework)
  const key = fingerprint([
    source, executable, version, JSON.stringify(manifest),
    String(frameworkStat.size), String(frameworkStat.mtimeMs),
    fs.readFileSync(executable),
    fs.readFileSync(__filename),
    fs.readFileSync(path.join(__dirname, 'macos-desktop-bootstrap.cjs')),
    fs.readFileSync(path.join(__dirname, 'macos-desktop-main.cc')),
    fs.readFileSync(path.join(__dirname, '..', 'hacks', 'macos-power-notification-compat.c')),
  ])
  const scope = fingerprint([source]).slice(0, 16)
  const directory = path.join(path.resolve(cacheRoot), scope, key)
  const bundle = path.join(directory, 'Porto Dev.app')
  if (fs.existsSync(bundle)) {
    validateDevelopmentApp(bundle, key)
    return bundle
  }
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
  const temporary = fs.mkdtempSync(path.join(directory, '.build-'))
  const staged = path.join(temporary, 'Porto Dev.app')
  try {
    await fs.promises.cp(runtimeBundle, staged, { recursive: true, verbatimSymlinks: true })
    const plist = path.join(staged, 'Contents', 'Info.plist')
    for (const [name, value] of [
      ['CFBundleIdentifier', `dev.mbianchi.porto.development.${scope}`],
      ['CFBundleDisplayName', 'Porto Dev'],
      ['CFBundleName', 'Porto Dev'],
    ]) {
      run('/usr/libexec/PlistBuddy', ['-c', `Set :${name} ${value}`, plist])
    }
    run('/usr/libexec/PlistBuddy', ['-c', 'Delete :ElectronAsarIntegrity', plist])
    const resources = path.join(staged, 'Contents', 'Resources')
    fs.unlinkSync(path.join(resources, 'default_app.asar'))
    const icon = path.join(source, 'assets', 'porto.icns')
    if (fs.existsSync(icon)) {
      fs.copyFileSync(icon, path.join(resources, 'porto.icns'))
      run('/usr/libexec/PlistBuddy', ['-c', 'Set :CFBundleIconFile porto.icns', plist])
    }
    const application = path.join(resources, 'app')
    fs.mkdirSync(application)
    fs.writeFileSync(path.join(application, 'package.json'), JSON.stringify({
      name: manifest.name, productName: manifest.productName || manifest.name,
      version: manifest.version, main: 'main.cjs',
    }), { mode: 0o600 })
    // Keep Electron's executable name so app.isPackaged stays false;
    // reopening resolves this source without CLI args.
    fs.writeFileSync(path.join(application, 'main.cjs'), [
      `const source = ${JSON.stringify(source)}`,
      'process.chdir(source)',
      "require('electron').app.setAppPath(source)",
      'require(source)',
      '',
    ].join('\n'), { mode: 0o600 })
    fs.writeFileSync(path.join(resources, MARKER), JSON.stringify({ key, source, version }), { mode: 0o600 })
    installMacOSBootstrap(staged, { executableName: 'Electron' })
    // Only the isolated cached dev bundle is ad-hoc signed, after native edits.
    run('/usr/bin/codesign', ['--force', '--sign', '-', '--timestamp=none',
      path.join(staged, 'Contents', 'Frameworks', 'libporto-power-notification.dylib')])
    run('/usr/bin/codesign', ['--force', '--sign', '-', '--timestamp=none', staged])
    validateDevelopmentApp(staged, key)
    try {
      fs.renameSync(staged, bundle)
    } catch (error) {
      if (!['EEXIST', 'ENOTEMPTY'].includes(error.code)) throw error
      validateDevelopmentApp(bundle, key)
    }
    return bundle
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true })
  }
}

module.exports = { prepareMacOSDevelopmentApp }
