const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')
const vm = require('node:vm')

function assertNoInlineScripts(index) {
  assert.match(index, /<script src="\/experience-preferences\.js"><\/script>/)
  assert.doesNotMatch(index, /<script(?![^>]*\bsrc=)[^>]*>/)
}

test('loads the pre-paint preference bootstrap without an inline script', () => {
  const index = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8')
  assertNoInlineScripts(index)

  const builtIndexPath = path.join(__dirname, '..', 'dist', 'index.html')
  if (fs.existsSync(builtIndexPath)) {
    assertNoInlineScripts(fs.readFileSync(builtIndexPath, 'utf8'))
  }
})

test('applies cached preferences before the dashboard bundle loads', () => {
  const bootstrap = fs.readFileSync(
    path.join(__dirname, '..', 'public', 'experience-preferences.js'),
    'utf8',
  )
  const document = { documentElement: { dataset: {} } }
  vm.runInNewContext(bootstrap, {
    console,
    document,
    localStorage: {
      getItem: () => JSON.stringify({
        interfaceDensity: 'comfortable',
        reduceMotion: true,
      }),
    },
  })
  assert.deepEqual(
    { ...document.documentElement.dataset },
    { density: 'comfortable', reduceMotion: 'true' },
  )
})
