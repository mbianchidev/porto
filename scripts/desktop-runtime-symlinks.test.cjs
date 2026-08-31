const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  normalizeElectronRuntimeSymlinks,
  validatePortableSymlinks,
} = require('./desktop-runtime-symlinks.cjs')

function temporaryDirectory(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-symlinks-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return directory
}

test('normalizes Electron Packager absolute Lima templates symlink', (t) => {
  const temporary = temporaryDirectory(t)
  const packageRoot = path.join(temporary, 'Porto.app')
  const internalTemplates = path.join(
    packageRoot,
    'Contents',
    'Resources',
    'runtime',
    'lima',
    'share',
    'lima',
    'templates',
  )
  const templatesLink = path.join(
    packageRoot,
    'Contents',
    'Resources',
    'runtime',
    'lima',
    'share',
    'doc',
    'lima',
    'templates',
  )
  const stagingTemplates = path.join(temporary, 'stage', 'runtime', 'lima', 'share', 'lima', 'templates')

  fs.mkdirSync(internalTemplates, { recursive: true })
  fs.mkdirSync(path.dirname(templatesLink), { recursive: true })
  fs.mkdirSync(stagingTemplates, { recursive: true })
  fs.writeFileSync(path.join(internalTemplates, 'default.yaml'), 'arch: default\n')
  fs.symlinkSync(stagingTemplates, templatesLink)

  normalizeElectronRuntimeSymlinks(packageRoot)

  assert.equal(fs.readlinkSync(templatesLink), path.join('..', '..', 'lima', 'templates'))
  assert.doesNotThrow(() => validatePortableSymlinks(packageRoot))
})

test('rejects broken relative symlinks', (t) => {
  const root = temporaryDirectory(t)
  fs.symlinkSync('missing', path.join(root, 'broken'))

  assert.throws(() => validatePortableSymlinks(root), /broken symlink/)
})

test('rejects unexpected absolute symlinks', (t) => {
  const root = temporaryDirectory(t)
  const external = path.join(temporaryDirectory(t), 'external')
  fs.writeFileSync(external, 'external\n')
  fs.symlinkSync(external, path.join(root, 'absolute'))

  assert.throws(() => validatePortableSymlinks(root), /absolute symlink is not portable/)
})
