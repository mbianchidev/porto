const assert = require('node:assert/strict')
const fs = require('node:fs/promises')
const { createRequire } = require('node:module')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

test('patched packaging glob libraries retain archive and file-matcher contracts', async () => {
  const fromArchive = createRequire(require.resolve('@electron/asar'))
  const fromBuilder = createRequire(require.resolve('app-builder-lib/package.json'))
  const { glob } = fromArchive('glob')
  const { Minimatch } = fromBuilder('minimatch')
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'porto-packaging-globs-'))
  try {
    await fs.writeFile(path.join(directory, 'synthetic.js'), 'synthetic')
    await fs.writeFile(path.join(directory, '.synthetic.js'), 'synthetic')
    await fs.writeFile(path.join(directory, 'excluded.txt'), 'synthetic')
    assert.deepEqual((await glob('**/*.js', { cwd: directory, dot: true, nodir: true })).sort(), ['.synthetic.js', 'synthetic.js'])
    assert.equal(new Minimatch('**/*.js', { dot: true }).match('.synthetic.js'), true)
    assert.equal(new Minimatch('**/*.js').match('excluded.txt'), false)
  } finally {
    await fs.rm(directory, { recursive: true, force: true })
  }
})

test('patched EJS build dependencies preserve rendering and FileList behavior', async () => {
  const fromBuilder = createRequire(require.resolve('app-builder-lib/package.json'))
  const ejs = fromBuilder('ejs')
  assert.equal(ejs.compile('<%= name %>')({ name: '<synthetic>' }), '&lt;synthetic&gt;')
  const fromEJS = createRequire(fromBuilder.resolve('ejs/package.json'))
  const fromJake = createRequire(fromEJS.resolve('jake/package.json'))
  const { FileList } = fromJake('filelist')
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'porto-packaging-filelist-'))
  try {
    await fs.writeFile(path.join(directory, 'synthetic.txt'), 'synthetic')
    await fs.writeFile(path.join(directory, 'excluded.js'), 'synthetic')
    const files = new FileList()
    files.include(path.join(directory, '*.txt').replaceAll('\\', '/'))
    assert.deepEqual(files.toArray().map((file) => path.basename(file)), ['synthetic.txt'])
  } finally {
    await fs.rm(directory, { recursive: true, force: true })
  }
})
