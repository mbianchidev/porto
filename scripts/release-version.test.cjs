const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  updateGoVersion,
  verifyReleaseVersions,
} = require('./release-version.cjs')

function writeJson(filePath, value) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true })
  fs.writeFileSync(filePath, `${JSON.stringify(value, null, 2)}\n`)
}

function releaseFixture(t, version = '1.2.12') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-release-version-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))

  fs.mkdirSync(path.join(root, 'internal/config'), { recursive: true })
  fs.writeFileSync(
    path.join(root, 'internal/config/config.go'),
    `package config\n\nvar Version = "${version}"\n`,
  )
  writeJson(path.join(root, 'ui/package.json'), { version })
  writeJson(path.join(root, 'ui/package-lock.json'), {
    version,
    packages: { '': { version } },
  })
  writeJson(path.join(root, 'ui/electron/package.json'), { version })
  writeJson(path.join(root, 'ui/electron/package-lock.json'), {
    version,
    packages: { '': { version } },
  })

  return root
}

test('updates the Go release version and verifies aligned metadata', (t) => {
  const root = releaseFixture(t)

  updateGoVersion(root, '1.2.13')
  for (const relativePath of [
    'ui/package.json',
    'ui/package-lock.json',
    'ui/electron/package.json',
    'ui/electron/package-lock.json',
  ]) {
    const filePath = path.join(root, relativePath)
    const contents = fs.readFileSync(filePath, 'utf8').replaceAll('1.2.12', '1.2.13')
    fs.writeFileSync(filePath, contents)
  }

  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.13'))
  assert.match(
    fs.readFileSync(path.join(root, 'internal/config/config.go'), 'utf8'),
    /^var Version = "1\.2\.13"$/m,
  )
})

test('reports the release file whose version does not match', (t) => {
  const root = releaseFixture(t)
  const packagePath = path.join(root, 'ui/electron/package.json')
  writeJson(packagePath, { version: '1.2.11' })

  assert.throws(
    () => verifyReleaseVersions(root, '1.2.12'),
    /ui\/electron\/package\.json has version "1\.2\.11"; expected 1\.2\.12/,
  )
})

test('rejects an unexpected Go version declaration', (t) => {
  const root = releaseFixture(t)
  fs.writeFileSync(path.join(root, 'internal/config/config.go'), 'package config\n')

  assert.throws(
    () => updateGoVersion(root, '1.2.13'),
    /does not contain the expected Version declaration/,
  )
})

test('release script avoids Bash heredoc pipe deadlocks', () => {
  const releaseScript = fs.readFileSync(path.join(__dirname, '..', 'release.sh'), 'utf8')

  assert.doesNotMatch(releaseScript, /<</)
})
