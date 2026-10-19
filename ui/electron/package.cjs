const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

const { normalizeElectronRuntimeSymlinks } = require('../../scripts/desktop-runtime-symlinks.cjs')
const { macOSDeveloperDirectory } = require('../../scripts/macos-toolchain.cjs')
const { parseSemver, RELEASE_VERSION_FILE } = require('./desktop-updater.cjs')

const packagerEntry = path.join(path.dirname(require.resolve('@electron/packager')), '..', 'bin', 'electron-packager.mjs')
let releaseVersion = require('./package.json').version
const forwardedArgs = process.argv.slice(2).filter((argument) => {
  if (!argument.startsWith('--porto-release-version=')) return true
  releaseVersion = argument.slice('--porto-release-version='.length)
  return false
})
releaseVersion = parseSemver(releaseVersion).version
let output = __dirname
let platform = process.platform
for (let index = 0; index < forwardedArgs.length; index += 1) {
  const argument = forwardedArgs[index]
  if (argument.startsWith('--out=')) {
    output = argument.slice('--out='.length)
  } else if (argument === '--out' && forwardedArgs[index + 1]) {
    output = forwardedArgs[index + 1]
  } else if (argument.startsWith('--platform=')) {
    platform = argument.slice('--platform='.length)
  } else if (argument === '--platform' && forwardedArgs[index + 1]) {
    platform = forwardedArgs[index + 1]
  }
}
let environment = process.env
if (process.platform === 'darwin' && platform.split(',').some((target) => ['darwin', 'mas', 'all'].includes(target))) {
  try {
    environment = { ...process.env, DEVELOPER_DIR: macOSDeveloperDirectory() }
  } catch (error) {
    console.error(`Unable to package Porto: ${error.message}`)
    process.exit(1)
  }
}
const metadataDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-release-metadata-'))
const releaseVersionPath = path.join(metadataDirectory, RELEASE_VERSION_FILE)
fs.writeFileSync(releaseVersionPath, `${releaseVersion}\n`, { encoding: 'utf8', mode: 0o600 })
process.once('exit', () => {
  try {
    fs.rmSync(metadataDirectory, { recursive: true, force: true })
  } catch (error) {
    console.error(`Unable to remove Porto release metadata directory: ${error.message}`)
  }
})
const args = [
  packagerEntry,
  __dirname,
  'Porto',
  '--overwrite',
  '--prune=true',
  '--ignore=^/Porto-(darwin|linux|mas|win32)-[^/]+($|/)',
  `--icon=${path.join(__dirname, 'assets', 'porto')}`,
  `--extend-info=${path.join(__dirname, 'assets', 'Info.plist')}`,
  `--extra-resource=${path.join(__dirname, 'assets', 'porto.png')}`,
  '--app-bundle-id=dev.mbianchi.porto',
  '--app-category-type=public.app-category.developer-tools',
  '--app-copyright=Copyright (c) 2026 mbianchidev',
  '--win32metadata.CompanyName=Porto',
  '--win32metadata.FileDescription=Porto',
  '--win32metadata.ProductName=Porto',
  '--win32metadata.InternalName=Porto',
  '--win32metadata.OriginalFilename=Porto.exe',
  `--extra-resource=${path.join(__dirname, 'apply-update.ps1')}`,
  `--extra-resource=${path.join(__dirname, 'apply-update.sh')}`,
  `--extra-resource=${releaseVersionPath}`,
  ...forwardedArgs,
]
const result = spawnSync(process.execPath, args, {
  cwd: __dirname,
  stdio: 'inherit',
  env: { ...environment, TMPDIR: metadataDirectory, TMP: metadataDirectory, TEMP: metadataDirectory },
})

if (result.error) {
  console.error(`Unable to package Porto: ${result.error.message}`)
  process.exitCode = 1
} else if (result.status !== 0) {
  process.exitCode = result.status ?? 1
} else {
  try {
    const outputDirectory = path.resolve(__dirname, output)
    const packagedApps = fs.readdirSync(outputDirectory, { withFileTypes: true })
      .filter((entry) => entry.isDirectory() && entry.name.startsWith('Porto-'))
      .map((entry) => path.join(outputDirectory, entry.name))
    if (packagedApps.length === 0) {
      throw new Error(`Electron Packager produced no Porto application in ${outputDirectory}`)
    }
    for (const packagedApp of packagedApps) {
      normalizeElectronRuntimeSymlinks(packagedApp)
      const macOSBundles = fs.readdirSync(packagedApp, { withFileTypes: true })
        .filter((entry) => entry.isDirectory() && entry.name.endsWith('.app'))
        .map((entry) => path.join(packagedApp, entry.name))
      const resourceRoots = [
        path.join(packagedApp, 'resources'),
        ...macOSBundles.map((bundle) => path.join(bundle, 'Contents', 'Resources')),
      ]
      const resources = resourceRoots.find((candidate) => (
        fs.existsSync(path.join(candidate, 'app.asar'))
        || fs.existsSync(path.join(candidate, 'app', 'main.js'))
      ))
      if (!resources) {
        throw new Error(`Unable to locate packaged Porto application resources in ${packagedApp}`)
      }
      for (const requiredFile of ['apply-update.ps1', 'apply-update.sh']) {
        if (!fs.existsSync(path.join(resources, requiredFile))) {
          throw new Error(`Packaged Porto application is missing ${requiredFile}`)
        }
      }
      const packagedReleaseVersion = fs.readFileSync(path.join(resources, RELEASE_VERSION_FILE), 'utf8').trim()
      if (packagedReleaseVersion !== releaseVersion) {
        throw new Error(`Packaged Porto release version is ${packagedReleaseVersion}; expected ${releaseVersion}`)
      }
    }
  } catch (error) {
    console.error(`Unable to finalize packaged Porto application: ${error.message}`)
    process.exitCode = 1
  }
}
