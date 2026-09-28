const fs = require('node:fs')
const path = require('node:path')

function readJson(root, relativePath) {
  return JSON.parse(fs.readFileSync(path.join(root, relativePath), 'utf8'))
}

function updateGoVersion(root, expected) {
  const relativePath = 'internal/config/config.go'
  const configPath = path.join(root, relativePath)
  const source = fs.readFileSync(configPath, 'utf8')
  const pattern = /^(var Version = ")[^"]+(")$/m

  if (!pattern.test(source)) {
    throw new Error(`${relativePath} does not contain the expected Version declaration`)
  }

  fs.writeFileSync(
    configPath,
    source.replace(pattern, (_, prefix, suffix) => `${prefix}${expected}${suffix}`),
  )
}

function verifyReleaseVersions(root, expected) {
  const configSource = fs.readFileSync(path.join(root, 'internal/config/config.go'), 'utf8')
  const configVersion = configSource.match(/^var Version = "([^"]+)"$/m)?.[1]
  const packageJson = readJson(root, 'ui/package.json')
  const packageLock = readJson(root, 'ui/package-lock.json')
  const electronPackageJson = readJson(root, 'ui/electron/package.json')
  const electronPackageLock = readJson(root, 'ui/electron/package-lock.json')
  const versions = [
    ['internal/config/config.go', configVersion],
    ['ui/package.json', packageJson.version],
    ['ui/package-lock.json', packageLock.version],
    ['ui/package-lock.json packages[""]', packageLock.packages?.['']?.version],
    ['ui/electron/package.json', electronPackageJson.version],
    ['ui/electron/package-lock.json', electronPackageLock.version],
    ['ui/electron/package-lock.json packages[""]', electronPackageLock.packages?.['']?.version],
  ]

  for (const [source, actual] of versions) {
    if (actual !== expected) {
      throw new Error(`${source} has version ${JSON.stringify(actual)}; expected ${expected}`)
    }
  }
}

if (require.main === module) {
  const [mode, expected] = process.argv.slice(2)
  const root = path.resolve(__dirname, '..')

  if (!expected || (mode !== 'update-go' && mode !== 'verify')) {
    console.error('usage: release-version.cjs <update-go|verify> <version>')
    process.exitCode = 2
  } else {
    try {
      if (mode === 'update-go') {
        updateGoVersion(root, expected)
      } else {
        verifyReleaseVersions(root, expected)
      }
    } catch (error) {
      console.error(error instanceof Error ? error.message : String(error))
      process.exitCode = 1
    }
  }
}

module.exports = {
  updateGoVersion,
  verifyReleaseVersions,
}
