const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')

const packageJson = require('./package.json')

test('makes the Windows desktop shortcut an explicit installer choice', () => {
  assert.equal(packageJson.build.nsis.createDesktopShortcut, false)
  assert.equal(packageJson.build.nsis.include, 'assets/installer.nsh')
  assert.equal(packageJson.devDependencies['7zip-bin-full'], '^26.3.1')

  const installer = fs.readFileSync(path.join(__dirname, 'assets', 'installer.nsh'), 'utf8')
  assert.match(installer, /Create a desktop shortcut/)
  assert.match(installer, /\$\{BST_UNCHECKED\}/)
  assert.match(installer, /CreateShortCut "\$newDesktopLink"/)
  assert.match(installer, /\$\{IfNot\} \$\{isKeepShortcuts\}/)
})
