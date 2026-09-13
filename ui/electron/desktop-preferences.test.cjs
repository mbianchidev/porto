const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const {
  DEFAULT_DESKTOP_PREFERENCES,
  loadDesktopPreferences,
  loginItemOptions,
  loginItemSupported,
  normalizeDesktopPreferences,
  saveDesktopPreferences,
  shouldHideWindow,
  shouldStartHidden,
  validateDesktopPreferences,
} = require('./desktop-preferences.cjs')

function preferences(overrides = {}) {
  return { ...DEFAULT_DESKTOP_PREFERENCES, ...overrides }
}

test('normalizes absent and partial desktop preferences', () => {
  assert.deepEqual(normalizeDesktopPreferences(null), DEFAULT_DESKTOP_PREFERENCES)
  assert.deepEqual(normalizeDesktopPreferences({ openAtLogin: true }), {
    openAtLogin: true,
    keepInTray: false,
    automaticallyDownloadUpdates: false,
  })
  assert.throws(() => validateDesktopPreferences({ openAtLogin: true }), /keepInTray/)
  assert.throws(
    () => validateDesktopPreferences({ openAtLogin: true, keepInTray: false }),
    /automaticallyDownloadUpdates/,
  )
})

test('round-trips desktop preferences through a protected file', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-desktop-preferences-'))
  const filePath = path.join(directory, 'desktop-preferences.json')
  try {
    saveDesktopPreferences(filePath, preferences({
      openAtLogin: true,
      keepInTray: true,
      automaticallyDownloadUpdates: true,
    }))
    assert.deepEqual(loadDesktopPreferences(filePath), {
      openAtLogin: true,
      keepInTray: true,
      automaticallyDownloadUpdates: true,
    })
    saveDesktopPreferences(filePath, preferences({ keepInTray: true }))
    assert.deepEqual(loadDesktopPreferences(filePath), {
      openAtLogin: false,
      keepInTray: true,
      automaticallyDownloadUpdates: false,
    })
    if (process.platform !== 'win32') {
      assert.equal(fs.statSync(filePath).mode & 0o777, 0o600)
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('reports corrupt preference files and restores defaults', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-desktop-preferences-'))
  const filePath = path.join(directory, 'desktop-preferences.json')
  fs.writeFileSync(filePath, '{')
  let reported = null
  try {
    assert.deepEqual(loadDesktopPreferences(filePath, (error) => {
      reported = error
    }), DEFAULT_DESKTOP_PREFERENCES)
    assert.match(reported?.message ?? '', /JSON/)
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('builds platform-specific login item settings', () => {
  assert.equal(loginItemSupported('darwin', false), false)
  assert.equal(loginItemSupported('linux', true), false)
  assert.equal(loginItemSupported('darwin', true), true)
  assert.equal(loginItemSupported('win32', true), true)
  assert.deepEqual(
    loginItemOptions(preferences({ openAtLogin: true, keepInTray: true }), 'darwin', '/Applications/Porto.app'),
    { openAtLogin: true },
  )
  assert.deepEqual(
    loginItemOptions(preferences({ openAtLogin: true, keepInTray: true }), 'win32', 'C:\\Porto\\Porto.exe'),
    { openAtLogin: true, path: 'C:\\Porto\\Porto.exe', args: ['--hidden'] },
  )
})

test('starts hidden only for a tray-enabled login launch', () => {
  assert.equal(shouldStartHidden(preferences({ openAtLogin: true, keepInTray: true }), ['Porto', '--hidden']), true)
  assert.equal(shouldStartHidden(preferences({ openAtLogin: true, keepInTray: true }), ['Porto']), false)
  assert.equal(shouldStartHidden(preferences({ openAtLogin: true }), ['Porto', '--hidden']), false)
})

test('hides a close request only while tray behavior is active', () => {
  assert.equal(shouldHideWindow(preferences({ keepInTray: true }), false), true)
  assert.equal(shouldHideWindow(preferences({ keepInTray: true }), true), false)
  assert.equal(shouldHideWindow(preferences(), false), false)
})
