const { spawnSync } = require('node:child_process')
const path = require('node:path')

const packagerEntry = path.join(path.dirname(require.resolve('@electron/packager')), '..', 'bin', 'electron-packager.mjs')
const args = [
  packagerEntry,
  __dirname,
  'Porto',
  '--overwrite',
  '--prune=true',
  `--icon=${path.join(__dirname, 'assets', 'porto.icns')}`,
  `--extend-info=${path.join(__dirname, 'assets', 'Info.plist')}`,
  `--extra-resource=${path.join(__dirname, 'assets', 'porto.png')}`,
  '--app-bundle-id=dev.mbianchi.porto',
  '--app-category-type=public.app-category.developer-tools',
  '--win32metadata.CompanyName=Porto',
  '--win32metadata.FileDescription=Porto',
  '--win32metadata.ProductName=Porto',
  '--win32metadata.InternalName=Porto',
  '--win32metadata.OriginalFilename=Porto.exe',
  ...process.argv.slice(2),
]
const result = spawnSync(process.execPath, args, {
  cwd: __dirname,
  stdio: 'inherit',
})

if (result.error) {
  console.error(`Unable to package Porto: ${result.error.message}`)
  process.exitCode = 1
} else {
  process.exitCode = result.status ?? 1
}
