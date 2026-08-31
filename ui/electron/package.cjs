const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')

const { normalizeElectronRuntimeSymlinks } = require('../../scripts/desktop-runtime-symlinks.cjs')

const packagerEntry = path.join(path.dirname(require.resolve('@electron/packager')), '..', 'bin', 'electron-packager.mjs')
const forwardedArgs = process.argv.slice(2)
const args = [
  packagerEntry,
  __dirname,
  'Porto',
  '--overwrite',
  '--prune=true',
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
  ...forwardedArgs,
]
const result = spawnSync(process.execPath, args, {
  cwd: __dirname,
  stdio: 'inherit',
})

if (result.error) {
  console.error(`Unable to package Porto: ${result.error.message}`)
  process.exitCode = 1
} else if (result.status !== 0) {
  process.exitCode = result.status ?? 1
} else {
  try {
    let output = __dirname
    for (let index = 0; index < forwardedArgs.length; index += 1) {
      const argument = forwardedArgs[index]
      if (argument.startsWith('--out=')) {
        output = argument.slice('--out='.length)
      } else if (argument === '--out' && forwardedArgs[index + 1]) {
        output = forwardedArgs[index + 1]
      }
    }

    const outputDirectory = path.resolve(__dirname, output)
    const packagedApps = fs.readdirSync(outputDirectory, { withFileTypes: true })
      .filter((entry) => entry.isDirectory() && entry.name.startsWith('Porto-'))
      .map((entry) => path.join(outputDirectory, entry.name))
    if (packagedApps.length === 0) {
      throw new Error(`Electron Packager produced no Porto application in ${outputDirectory}`)
    }
    for (const packagedApp of packagedApps) {
      normalizeElectronRuntimeSymlinks(packagedApp)
    }
  } catch (error) {
    console.error(`Unable to normalize packaged runtime symlinks: ${error.message}`)
    process.exitCode = 1
  }
}
